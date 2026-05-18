// Copyright 2026 ray-x. All rights reserved.
// Use of this source code is governed by an Apache 2.0
// license that can be found in the LICENSE file.

package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	copilot "github.com/github/copilot-sdk/go"
)

// ---------------------------------------------------------------------------
// Copilot CLI client lifecycle
// ---------------------------------------------------------------------------

func (s *service) currentClient() copilotClient {
	s.clientMu.RLock()
	defer s.clientMu.RUnlock()
	return s.client
}

func (s *service) attachClientLifecycleHandlers(client copilotClient) {
	// Track session name updates from the SDK. The SDK auto-generates a summary
	// after each turn and delivers it via a SessionLifecycleUpdated event.
	client.OnEventType(copilot.SessionLifecycleUpdated, func(event copilot.SessionLifecycleEvent) {
		if event.Metadata == nil || event.Metadata.Summary == nil {
			return
		}
		s.sessionsMu.RLock()
		matched := make([]*managedSession, 0, 1)
		for _, candidate := range s.sessions {
			if candidate == nil || candidate.id() != event.SessionID {
				continue
			}
			if !s.isCopilotProviderName(candidate.provider) {
				continue
			}
			matched = append(matched, candidate)
		}
		s.sessionsMu.RUnlock()
		if len(matched) == 0 {
			return
		}
		name := *event.Metadata.Summary
		for _, managed := range matched {
			managed.sessionName = name
			managed.broadcastHostEvent("host.session_name_updated", map[string]string{
				"sessionId": event.SessionID,
				"name":      name,
			})
		}
	})
}

func (s *service) startCopilotClient() error {
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	return s.startCopilotClientLocked()
}

func (s *service) startCopilotClientLocked() error {
	if s.clientFactory == nil {
		return errors.New("copilot client factory is not configured")
	}
	if s.clientCtx == nil {
		return errors.New("copilot client context is not configured")
	}
	client := s.clientFactory()
	if client == nil {
		return errors.New("copilot client factory returned nil")
	}
	if err := client.Start(s.clientCtx); err != nil {
		return fmt.Errorf("start copilot client: %w", err)
	}
	s.attachClientLifecycleHandlers(client)
	s.client = client
	return nil
}

func (s *service) stopCopilotClient() error {
	s.clientMu.Lock()
	client := s.client
	s.client = nil
	s.clientMu.Unlock()
	if client == nil {
		return nil
	}
	return client.Stop()
}

func (s *service) ensureClientConnected() error {
	if s == nil || !s.hasProviderType(providerCopilot) {
		return nil
	}
	client := s.currentClient()
	if client != nil && client.State() == copilot.StateConnected {
		return nil
	}
	if client == nil {
		logWarnf("health check restarting Copilot CLI client: no connected client")
	} else {
		logWarnf("health check restarting Copilot CLI client: state=%v", client.State())
	}
	return s.restartCopilotClient(errors.New("copilot client is disconnected"), false)
}

func (s *service) restartCopilotClient(reason error, force bool) error {
	s.clientMu.Lock()
	defer s.clientMu.Unlock()

	if !force && s.client != nil && s.client.State() == copilot.StateConnected {
		return nil
	}

	oldClient := s.client
	s.client = nil
	if oldClient != nil {
		logWarnf("restarting Copilot CLI client after failure: %v", reason)
		oldClient.ForceStop()
		s.closeManagedSessions(fmt.Errorf("copilot client restarted: %w", reason))
	}

	return s.startCopilotClientLocked()
}

func isRecoverableCopilotClientError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "cli process exited"),
		strings.Contains(message, "process exited unexpectedly"),
		strings.Contains(message, "client not connected"),
		strings.Contains(message, "client stopped"):
		return true
	default:
		return false
	}
}

func withCopilotClientRetry[T any](s *service, operation string, fn func(copilotClient) (T, error)) (T, error) {
	var zero T
	if s == nil || !s.hasProviderType(providerCopilot) {
		return zero, fmt.Errorf("%s is only supported with provider type=%s", operation, providerCopilot)
	}

	if err := s.ensureClientConnected(); err != nil {
		return zero, err
	}
	client := s.currentClient()
	if client == nil {
		return zero, errors.New("copilot client is unavailable")
	}

	result, err := fn(client)
	if err == nil || !isRecoverableCopilotClientError(err) {
		return result, err
	}

	if restartErr := s.restartCopilotClient(fmt.Errorf("%s: %w", operation, err), true); restartErr != nil {
		return zero, errors.Join(err, fmt.Errorf("restart copilot client: %w", restartErr))
	}
	client = s.currentClient()
	if client == nil {
		return zero, err
	}
	return fn(client)
}

// ---------------------------------------------------------------------------
// HTTP handlers that depend on the Copilot client or multi-provider routing
// ---------------------------------------------------------------------------

func (s *service) handleHealth(w http.ResponseWriter, r *http.Request) {
	providerRuntime, err := s.requestedProvider(r, "")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if isCopilotProviderType(providerRuntime.Type) {
		if err := s.ensureClientConnected(); err != nil {
			writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("copilot client unavailable: %v", err))
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"provider":        providerRuntime.Name,
		"providerType":    providerRuntime.Type,
		"defaultProvider": s.defaultProviderName(),
	})
}

func (s *service) handleListProviders(w http.ResponseWriter, _ *http.Request) {
	names := s.providerNames()
	providers := make([]map[string]any, 0, len(names))
	for _, name := range names {
		rt, ok := s.providerRuntime(name)
		if !ok {
			continue
		}
		item := map[string]any{
			"name":         rt.Name,
			"type":         rt.Type,
			"defaultModel": rt.Model,
		}
		if isClaudeProviderType(rt.Type) {
			item["claude"] = map[string]any{
				"apiKeyEnv":    rt.ClaudeAPIKeyEnv,
				"authTokenEnv": rt.ClaudeAuthTokenEnv,
				"baseURL":      rt.ClaudeBaseURL,
			}
		}
		providers = append(providers, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"default":   s.defaultProviderName(),
		"providers": providers,
	})
}

// listenInRange tries each port in "lo-hi" (inclusive) on the given host and
// returns the first listener that succeeds. Returns an error if no port in the
// range is available.
func listenInRange(host, portRange string) (net.Listener, error) {
	parts := strings.SplitN(portRange, "-", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid port range %q: expected \"lo-hi\"", portRange)
	}
	lo, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	hi, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || lo < 1 || hi > 65535 || lo > hi {
		return nil, fmt.Errorf("invalid port range %q", portRange)
	}
	for port := lo; port <= hi; port++ {
		addr := fmt.Sprintf("%s:%d", host, port)
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln, nil
		}
	}
	return nil, fmt.Errorf("no available port in range %s", portRange)
}

func (s *service) handleListModels(w http.ResponseWriter, r *http.Request) {
	providerRuntime, err := s.requestedProvider(r, "")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if isClaudeProviderType(providerRuntime.Type) {
		models := []map[string]any{
			{"id": "claude-opus-4-6", "name": "Claude Opus 4.6"},
			{"id": "claude-sonnet-4-5", "name": "Claude Sonnet 4.5"},
			{"id": "claude-haiku-4-5", "name": "Claude Haiku 4.5"},
		}
		if strings.TrimSpace(providerRuntime.Model) != "" {
			models = append([]map[string]any{{"id": providerRuntime.Model, "name": providerRuntime.Model}}, models...)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"provider": providerRuntime.Name,
			"models":   models,
		})
		return
	}

	models, err := withCopilotClientRetry(s, "list models", func(client copilotClient) ([]copilot.ModelInfo, error) {
		return client.ListModels(r.Context())
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("list models: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"provider": providerRuntime.Name,
		"models":   models,
	})
}

func (s *service) handleListSessions(w http.ResponseWriter, r *http.Request) {
	providerRuntime, err := s.requestedProvider(r, "")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if isClaudeProviderType(providerRuntime.Type) {
		writeJSON(w, http.StatusOK, listSessionsResponse{
			Provider:  providerRuntime.Name,
			Persisted: []copilot.SessionMetadata{},
			Live:      s.liveSessionSummaries(providerRuntime.Name),
		})
		return
	}

	persisted, err := withCopilotClientRetry(s, "list sessions", func(client copilotClient) ([]copilot.SessionMetadata, error) {
		return client.ListSessions(r.Context(), nil)
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("list sessions: %v", err))
		return
	}

	persisted = enrichPersistedSessionsFromWorkspace(persisted, defaultSessionStateDir())
	live := s.liveSessionSummaries(providerRuntime.Name)
	writeJSON(w, http.StatusOK, listSessionsResponse{
		Provider:  providerRuntime.Name,
		Persisted: persisted,
		Live:      live,
	})
}
