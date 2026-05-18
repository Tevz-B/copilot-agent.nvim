// Copyright 2026 ray-x. All rights reserved.
// Use of this source code is governed by an Apache 2.0
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"errors"
	"testing"

	copilot "github.com/github/copilot-sdk/go"
)

// Compile-time interface compliance checks.
var _ providerSession = (*copilotSession)(nil)
var _ providerSession = (*claudeSession)(nil)

// ---------------------------------------------------------------------------
// ErrNotSupported
// ---------------------------------------------------------------------------

func TestProviderErrNotSupported(t *testing.T) {
	t.Parallel()
	if ErrNotSupported == nil {
		t.Fatal("ErrNotSupported must not be nil")
	}
	if ErrNotSupported.Error() == "" {
		t.Fatal("ErrNotSupported must have a non-empty message")
	}
}

// ---------------------------------------------------------------------------
// copilotSession — nil-safety tests
// ---------------------------------------------------------------------------

func TestProviderCopilotSessionID_NilSession(t *testing.T) {
	t.Parallel()
	cs := &copilotSession{session: nil}
	if got := cs.SessionID(); got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestProviderCopilotWorkspacePath_NilSession(t *testing.T) {
	t.Parallel()
	cs := &copilotSession{session: nil}
	if got := cs.WorkspacePath(); got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestProviderCopilotCapabilities_NilSession(t *testing.T) {
	t.Parallel()
	cs := &copilotSession{session: nil}
	got := cs.Capabilities()
	if got != (copilot.SessionCapabilities{}) {
		t.Fatalf("expected zero-value SessionCapabilities, got %+v", got)
	}
}

func TestProviderCopilotDisconnect_NilSession(t *testing.T) {
	t.Parallel()
	cs := &copilotSession{session: nil}
	if err := cs.Disconnect(); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// claudeSession
// ---------------------------------------------------------------------------

func newTestClaudeSession() *claudeSession {
	managed := &managedSession{
		sessionID: "test-123",
		model:     "claude-sonnet-4-5",
	}
	return &claudeSession{managed: managed}
}

func TestProviderClaudeSessionID(t *testing.T) {
	t.Parallel()
	cs := newTestClaudeSession()
	if got := cs.SessionID(); got != "test-123" {
		t.Fatalf("expected %q, got %q", "test-123", got)
	}
}

func TestProviderClaudeSetModel(t *testing.T) {
	t.Parallel()
	cs := newTestClaudeSession()
	if err := cs.SetModel(context.Background(), "claude-opus-4", ""); err != nil {
		t.Fatalf("SetModel returned error: %v", err)
	}
	if cs.managed.model != "claude-opus-4" {
		t.Fatalf("expected model %q, got %q", "claude-opus-4", cs.managed.model)
	}
}

func TestProviderClaudeGetTasks(t *testing.T) {
	t.Parallel()
	cs := newTestClaudeSession()
	tasks, err := cs.GetTasks(context.Background())
	if err != nil {
		t.Fatalf("GetTasks returned error: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("expected empty slice, got %d tasks", len(tasks))
	}
}

func TestProviderClaudeCompactHistory(t *testing.T) {
	t.Parallel()
	cs := newTestClaudeSession()
	result, err := cs.CompactHistory(context.Background())
	if !errors.Is(err, ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
	if result != nil {
		t.Fatalf("expected nil result, got %+v", result)
	}
}

func TestProviderClaudeFleetStart(t *testing.T) {
	t.Parallel()
	cs := newTestClaudeSession()
	result, err := cs.FleetStart(context.Background(), "test prompt")
	if !errors.Is(err, ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
	if result != nil {
		t.Fatalf("expected nil result, got %+v", result)
	}
}

func TestProviderClaudeSetMode(t *testing.T) {
	t.Parallel()
	cs := newTestClaudeSession()
	if err := cs.SetMode(context.Background(), "autopilot"); err != nil {
		t.Fatalf("SetMode returned error: %v", err)
	}
}

func TestProviderClaudeWorkspacePath(t *testing.T) {
	t.Parallel()
	cs := newTestClaudeSession()
	if got := cs.WorkspacePath(); got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestProviderClaudeCapabilities(t *testing.T) {
	t.Parallel()
	cs := newTestClaudeSession()
	got := cs.Capabilities()
	if got != (copilot.SessionCapabilities{}) {
		t.Fatalf("expected zero-value SessionCapabilities, got %+v", got)
	}
}

func TestProviderClaudeDisconnect(t *testing.T) {
	t.Parallel()
	cs := newTestClaudeSession()
	// Must not panic; abortClaudeTurn on a zero-value mutex with nil cancel is safe.
	if err := cs.Disconnect(); err != nil {
		t.Fatalf("Disconnect returned error: %v", err)
	}
}

func TestProviderClaudeAbort_NoActiveTurn(t *testing.T) {
	t.Parallel()
	cs := newTestClaudeSession()
	aborted, err := cs.Abort(context.Background())
	if err != nil {
		t.Fatalf("Abort returned error: %v", err)
	}
	if aborted {
		t.Fatal("expected aborted=false when no turn is active")
	}
}
