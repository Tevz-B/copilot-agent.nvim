// Copyright 2026 ray-x. All rights reserved.
// Use of this source code is governed by an Apache 2.0
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"encoding/json"
	"errors"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

// providerSession abstracts the provider-specific session operations so that
// HTTP handlers can operate uniformly without if/else provider checks.
type providerSession interface {
	// SessionID returns the backend-assigned session identifier.
	SessionID() string

	// SendMessage initiates a turn. Returns a messageID for tracking.
	SendMessage(ctx context.Context, opts sendMessageOpts) (string, error)

	// Abort cancels the current in-flight turn. Returns true if a turn was active.
	Abort(ctx context.Context) (bool, error)

	// SetModel applies a model change to the backend session.
	SetModel(ctx context.Context, model string, reasoningEffort string) error

	// GetMessages returns the conversation history in a provider-appropriate format.
	GetMessages(ctx context.Context) (any, error)

	// GetTasks extracts background tasks from session history.
	GetTasks(ctx context.Context) ([]backgroundTaskView, error)

	// CompactHistory asks the backend to compact/summarize conversation history.
	// Returns nil,ErrNotSupported if the provider doesn't support compaction.
	CompactHistory(ctx context.Context) (*rpc.HistoryCompactResult, error)

	// FleetStart launches sub-agents. Returns nil,ErrNotSupported if unsupported.
	FleetStart(ctx context.Context, prompt string) (*rpc.FleetStartResult, error)

	// SetMode changes the agent mode (interactive/plan/autopilot).
	SetMode(ctx context.Context, mode rpc.SessionMode) error

	// WorkspacePath returns the effective workspace path known to the backend.
	WorkspacePath() string

	// Capabilities returns session capability flags.
	Capabilities() copilot.SessionCapabilities

	// Disconnect cleanly shuts down the backend session.
	Disconnect() error
}

// sendMessageOpts groups the parameters for SendMessage.
type sendMessageOpts struct {
	Prompt         string
	Attachments    []copilot.Attachment
	RequestHeaders map[string]string
}

// ErrNotSupported is returned by providerSession methods that are not implemented
// for a given provider.
var ErrNotSupported = errors.New("operation not supported by this provider")

// ---------------------------------------------------------------------------
// Copilot SDK implementation
// ---------------------------------------------------------------------------

type copilotSession struct {
	session *copilot.Session
}

func (c *copilotSession) SessionID() string {
	if c.session == nil {
		return ""
	}
	return c.session.SessionID
}

func (c *copilotSession) SendMessage(ctx context.Context, opts sendMessageOpts) (string, error) {
	return c.session.Send(ctx, copilot.MessageOptions{
		Prompt:         opts.Prompt,
		Attachments:    opts.Attachments,
		RequestHeaders: opts.RequestHeaders,
	})
}

func (c *copilotSession) Abort(ctx context.Context) (bool, error) {
	err := c.session.Abort(ctx)
	return err == nil, err
}

func (c *copilotSession) SetModel(ctx context.Context, model string, reasoningEffort string) error {
	var setOpts *copilot.SetModelOptions
	if reasoningEffort != "" {
		setOpts = &copilot.SetModelOptions{ReasoningEffort: &reasoningEffort}
	}
	return c.session.SetModel(ctx, model, setOpts)
}

func (c *copilotSession) GetMessages(ctx context.Context) (any, error) {
	return c.session.GetMessages(ctx)
}

func (c *copilotSession) GetTasks(ctx context.Context) ([]backgroundTaskView, error) {
	events, err := c.session.GetMessages(ctx)
	if err != nil {
		return nil, err
	}
	return extractBackgroundTasks(events), nil
}

func (c *copilotSession) CompactHistory(ctx context.Context) (*rpc.HistoryCompactResult, error) {
	return c.session.RPC.History.Compact(ctx)
}

func (c *copilotSession) FleetStart(ctx context.Context, prompt string) (*rpc.FleetStartResult, error) {
	var params *rpc.FleetStartRequest
	if prompt != "" {
		params = &rpc.FleetStartRequest{Prompt: &prompt}
	}
	return c.session.RPC.Fleet.Start(ctx, params)
}

func (c *copilotSession) SetMode(ctx context.Context, mode rpc.SessionMode) error {
	_, err := c.session.RPC.Mode.Set(ctx, &rpc.ModeSetRequest{Mode: mode})
	return err
}

func (c *copilotSession) WorkspacePath() string {
	if c.session == nil {
		return ""
	}
	return c.session.WorkspacePath()
}

func (c *copilotSession) Capabilities() copilot.SessionCapabilities {
	if c.session == nil {
		return copilot.SessionCapabilities{}
	}
	return c.session.Capabilities()
}

func (c *copilotSession) Disconnect() error {
	if c.session == nil {
		return nil
	}
	return c.session.Disconnect()
}

// ---------------------------------------------------------------------------
// Claude (Anthropic) implementation
// ---------------------------------------------------------------------------

type claudeSession struct {
	managed *managedSession
	svc     *service
}

func (c *claudeSession) SessionID() string {
	return c.managed.sessionID
}

func (c *claudeSession) SendMessage(_ context.Context, opts sendMessageOpts) (string, error) {
	turnCtx, cancel, err := c.managed.beginClaudeTurn()
	if err != nil {
		return "", err
	}
	messageID := newClaudeMessageID()
	c.managed.broadcastClaudeEvent("assistant.turn_start", map[string]any{"messageId": messageID}, messageID)
	go c.svc.runClaudeQuery(c.managed, turnCtx, cancel, opts.Prompt, opts.Attachments, opts.RequestHeaders, messageID)
	return messageID, nil
}

func (c *claudeSession) Abort(_ context.Context) (bool, error) {
	return c.managed.abortClaudeTurn(), nil
}

func (c *claudeSession) SetModel(_ context.Context, model string, _ string) error {
	c.managed.model = model
	return nil
}

func (c *claudeSession) GetMessages(_ context.Context) (any, error) {
	rawEvents := c.managed.claudeEventHistorySnapshot()
	events := make([]map[string]any, 0, len(rawEvents))
	for _, raw := range rawEvents {
		var parsed map[string]any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			continue
		}
		events = append(events, parsed)
	}
	return events, nil
}

func (c *claudeSession) GetTasks(_ context.Context) ([]backgroundTaskView, error) {
	return []backgroundTaskView{}, nil
}

func (c *claudeSession) CompactHistory(_ context.Context) (*rpc.HistoryCompactResult, error) {
	return nil, ErrNotSupported
}

func (c *claudeSession) FleetStart(_ context.Context, _ string) (*rpc.FleetStartResult, error) {
	return nil, ErrNotSupported
}

func (c *claudeSession) SetMode(_ context.Context, _ rpc.SessionMode) error {
	return nil // Claude stores mode locally; no backend call needed.
}

func (c *claudeSession) WorkspacePath() string {
	return ""
}

func (c *claudeSession) Capabilities() copilot.SessionCapabilities {
	return copilot.SessionCapabilities{}
}

func (c *claudeSession) Disconnect() error {
	c.managed.abortClaudeTurn()
	return nil
}
