package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

// ---------------------------------------------------------------------------
// Stub providerSession
// ---------------------------------------------------------------------------

type stubProviderSession struct{ id string }

func (s *stubProviderSession) SessionID() string { return s.id }
func (s *stubProviderSession) SendMessage(_ context.Context, _ sendMessageOpts) (string, error) {
	return "", ErrNotSupported
}
func (s *stubProviderSession) Abort(_ context.Context) (bool, error) { return false, ErrNotSupported }
func (s *stubProviderSession) SetModel(_ context.Context, _, _ string) error {
	return ErrNotSupported
}
func (s *stubProviderSession) GetMessages(_ context.Context) (any, error) {
	return nil, ErrNotSupported
}
func (s *stubProviderSession) GetTasks(_ context.Context) ([]backgroundTaskView, error) {
	return nil, ErrNotSupported
}
func (s *stubProviderSession) CompactHistory(_ context.Context) (*rpc.HistoryCompactResult, error) {
	return nil, ErrNotSupported
}
func (s *stubProviderSession) FleetStart(_ context.Context, _ string) (*rpc.FleetStartResult, error) {
	return nil, ErrNotSupported
}
func (s *stubProviderSession) SetMode(_ context.Context, _ rpc.SessionMode) error {
	return ErrNotSupported
}
func (s *stubProviderSession) WorkspacePath() string { return "" }
func (s *stubProviderSession) Capabilities() copilot.SessionCapabilities {
	return copilot.SessionCapabilities{}
}
func (s *stubProviderSession) Disconnect() error { return nil }

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newTestService() *service {
	return &service{
		sessions:  make(map[string]*managedSession),
		providers: map[string]providerRuntime{},
	}
}

func newTestManaged(id, provider string) *managedSession {
	return &managedSession{
		ps:                 &stubProviderSession{id: id},
		sessionID:          id,
		provider:           provider,
		subscribers:        make(map[chan sseMessage]struct{}),
		pendingInputs:      make(map[string]*pendingUserInput),
		pendingPermissions: make(map[string]*pendingPermission),
	}
}

func addSubscriber(m *managedSession) chan sseMessage {
	ch := make(chan sseMessage, 64)
	m.subscribersMu.Lock()
	m.subscribers[ch] = struct{}{}
	m.subscribersMu.Unlock()
	return ch
}

// ---------------------------------------------------------------------------
// TestSessionService_sessionMapKey
// ---------------------------------------------------------------------------

func TestSessionService_sessionMapKey(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		session  string
		want     string
	}{
		{"basic", "Copilot", "abc", "copilot::abc"},
		{"trimmed", "  Claude  ", "  xyz  ", "claude::xyz"},
		{"empty_both", "", "", "::"},
		{"empty_provider", "", "s1", "::s1"},
		{"empty_session", "copilot", "", "copilot::"},
		{"mixed_case", "CoPiLoT", "SID", "copilot::SID"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sessionMapKey(tc.provider, tc.session)
			if got != tc.want {
				t.Errorf("sessionMapKey(%q, %q) = %q; want %q", tc.provider, tc.session, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestSessionService_clampInt64
// ---------------------------------------------------------------------------

func TestSessionService_clampInt64(t *testing.T) {
	tests := []struct {
		name  string
		value int64
		want  int64
	}{
		{"negative", -42, 0},
		{"zero", 0, 0},
		{"positive", 100, 100},
		{"large_negative", -9223372036854775808, 0},
		{"one", 1, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := clampInt64(tc.value)
			if got != tc.want {
				t.Errorf("clampInt64(%d) = %d; want %d", tc.value, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestSessionService_resolveModelAlias
// ---------------------------------------------------------------------------

func TestSessionService_resolveModelAlias(t *testing.T) {
	models := []copilot.ModelInfo{
		{ID: "gpt-4o"},
		{ID: "gpt-4.1"},
		{ID: "gpt-4.1-mini"},
		{ID: "claude-sonnet-4"},
		{ID: "claude-sonnet-4.5"},
		{ID: "o3-pro"},
		{ID: "mymodel.1"},
		{ID: "mymodel.3"},
		{ID: "mymodel.2"},
	}

	tests := []struct {
		name      string
		requested string
		wantID    string
		wantOK    bool
	}{
		{"exact_match", "gpt-4o", "gpt-4o", true},
		{"exact_case_insensitive", "GPT-4o", "gpt-4o", true},
		{"prefix_version_match", "gpt-4", "gpt-4.1", true},
		{"exact_match_over_prefix", "claude-sonnet-4", "claude-sonnet-4", true},
		{"prefix_picks_highest_version", "mymodel", "mymodel.3", true},
		{"prefix_prefers_plain_over_suffix", "gpt-4", "gpt-4.1", true},
		{"no_match", "nonexistent", "", false},
		{"empty_string", "", "", false},
		{"whitespace_only", "   ", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotID, gotOK := resolveModelAlias(tc.requested, models)
			if gotOK != tc.wantOK || gotID != tc.wantID {
				t.Errorf("resolveModelAlias(%q) = (%q, %v); want (%q, %v)", tc.requested, gotID, gotOK, tc.wantID, tc.wantOK)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestSessionService_resolveWorkingDirectory
// ---------------------------------------------------------------------------

func TestSessionService_resolveWorkingDirectory(t *testing.T) {
	t.Run("empty_returns_cwd", func(t *testing.T) {
		got, err := resolveWorkingDirectory("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		cwd, _ := os.Getwd()
		want, _ := filepath.Abs(cwd)
		if got != want {
			t.Errorf("resolveWorkingDirectory(\"\") = %q; want %q", got, want)
		}
	})

	t.Run("valid_directory", func(t *testing.T) {
		dir := t.TempDir()
		got, err := resolveWorkingDirectory(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want, _ := filepath.Abs(dir)
		if got != want {
			t.Errorf("resolveWorkingDirectory(%q) = %q; want %q", dir, got, want)
		}
	})

	t.Run("whitespace_only_returns_cwd", func(t *testing.T) {
		got, err := resolveWorkingDirectory("   ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		cwd, _ := os.Getwd()
		want, _ := filepath.Abs(cwd)
		if got != want {
			t.Errorf("resolveWorkingDirectory(\"   \") = %q; want %q", got, want)
		}
	})
}

// ---------------------------------------------------------------------------
// TestSessionService_getManagedSession
// ---------------------------------------------------------------------------

func TestSessionService_getManagedSession(t *testing.T) {
	t.Run("empty_id_returns_not_found", func(t *testing.T) {
		svc := newTestService()
		_, found, ambiguous := svc.getManagedSession("", "copilot")
		if found || ambiguous {
			t.Error("expected not found for empty id")
		}
	})

	t.Run("with_provider_direct_lookup", func(t *testing.T) {
		svc := newTestService()
		m := newTestManaged("s1", "copilot")
		svc.sessions[sessionMapKey("copilot", "s1")] = m

		got, found, ambiguous := svc.getManagedSession("s1", "copilot")
		if !found || ambiguous || got != m {
			t.Error("expected direct lookup to succeed")
		}
	})

	t.Run("with_provider_not_found", func(t *testing.T) {
		svc := newTestService()
		svc.sessions[sessionMapKey("copilot", "s1")] = newTestManaged("s1", "copilot")

		_, found, _ := svc.getManagedSession("s1", "claude")
		if found {
			t.Error("expected not found for wrong provider")
		}
	})

	t.Run("without_provider_unique_match", func(t *testing.T) {
		svc := newTestService()
		m := newTestManaged("s1", "copilot")
		svc.sessions[sessionMapKey("copilot", "s1")] = m

		got, found, ambiguous := svc.getManagedSession("s1", "")
		if !found || ambiguous || got != m {
			t.Error("expected unique match without provider")
		}
	})

	t.Run("without_provider_ambiguous", func(t *testing.T) {
		svc := newTestService()
		svc.sessions[sessionMapKey("copilot", "s1")] = newTestManaged("s1", "copilot")
		svc.sessions[sessionMapKey("claude", "s1")] = newTestManaged("s1", "claude")

		_, found, ambiguous := svc.getManagedSession("s1", "")
		if found || !ambiguous {
			t.Error("expected ambiguous for duplicate session id across providers")
		}
	})

	t.Run("without_provider_no_match", func(t *testing.T) {
		svc := newTestService()
		svc.sessions[sessionMapKey("copilot", "s1")] = newTestManaged("s1", "copilot")

		_, found, ambiguous := svc.getManagedSession("s2", "")
		if found || ambiguous {
			t.Error("expected not found for unknown id")
		}
	})
}

// ---------------------------------------------------------------------------
// TestSessionService_storeManagedSession
// ---------------------------------------------------------------------------

func TestSessionService_storeManagedSession(t *testing.T) {
	t.Run("stores_by_key", func(t *testing.T) {
		svc := newTestService()
		m := newTestManaged("s1", "copilot")
		svc.storeManagedSession(m)

		key := sessionMapKey("copilot", "s1")
		if svc.sessions[key] != m {
			t.Error("session not stored under expected key")
		}
	})

	t.Run("nil_is_noop", func(t *testing.T) {
		svc := newTestService()
		svc.storeManagedSession(nil)
		if len(svc.sessions) != 0 {
			t.Error("nil should not be stored")
		}
	})

	t.Run("empty_id_is_noop", func(t *testing.T) {
		svc := newTestService()
		m := newTestManaged("", "copilot")
		svc.storeManagedSession(m)
		if len(svc.sessions) != 0 {
			t.Error("empty id should not be stored")
		}
	})
}

// ---------------------------------------------------------------------------
// TestSessionService_removeManagedSession
// ---------------------------------------------------------------------------

func TestSessionService_removeManagedSession(t *testing.T) {
	t.Run("with_provider_removes", func(t *testing.T) {
		svc := newTestService()
		m := newTestManaged("s1", "copilot")
		svc.storeManagedSession(m)

		got, found, ambiguous := svc.removeManagedSession("s1", "copilot")
		if !found || ambiguous || got != m {
			t.Error("expected remove to return the session")
		}
		if len(svc.sessions) != 0 {
			t.Error("expected session map to be empty after removal")
		}
	})

	t.Run("with_provider_not_found", func(t *testing.T) {
		svc := newTestService()
		svc.storeManagedSession(newTestManaged("s1", "copilot"))

		_, found, _ := svc.removeManagedSession("s1", "claude")
		if found {
			t.Error("expected not found for wrong provider")
		}
	})

	t.Run("without_provider_unique", func(t *testing.T) {
		svc := newTestService()
		m := newTestManaged("s1", "copilot")
		svc.storeManagedSession(m)

		got, found, ambiguous := svc.removeManagedSession("s1", "")
		if !found || ambiguous || got != m {
			t.Error("expected unique remove without provider")
		}
		if len(svc.sessions) != 0 {
			t.Error("expected session map to be empty")
		}
	})

	t.Run("without_provider_ambiguous", func(t *testing.T) {
		svc := newTestService()
		svc.storeManagedSession(newTestManaged("s1", "copilot"))
		svc.storeManagedSession(newTestManaged("s1", "claude"))

		_, found, ambiguous := svc.removeManagedSession("s1", "")
		if found || !ambiguous {
			t.Error("expected ambiguous when multiple providers match")
		}
		if len(svc.sessions) != 2 {
			t.Error("ambiguous remove should not delete anything")
		}
	})

	t.Run("empty_id_noop", func(t *testing.T) {
		svc := newTestService()
		svc.storeManagedSession(newTestManaged("s1", "copilot"))

		_, found, _ := svc.removeManagedSession("", "copilot")
		if found {
			t.Error("expected not found for empty id")
		}
	})
}

// ---------------------------------------------------------------------------
// TestSessionService_liveSessionSummaries
// ---------------------------------------------------------------------------

func TestSessionService_liveSessionSummaries(t *testing.T) {
	t.Run("empty_service", func(t *testing.T) {
		svc := newTestService()
		items := svc.liveSessionSummaries("")
		if len(items) != 0 {
			t.Errorf("expected 0 summaries; got %d", len(items))
		}
	})

	t.Run("filters_no_subscribers", func(t *testing.T) {
		svc := newTestService()
		svc.storeManagedSession(newTestManaged("s1", "copilot"))
		items := svc.liveSessionSummaries("")
		if len(items) != 0 {
			t.Error("sessions without subscribers should be excluded")
		}
	})

	t.Run("includes_with_subscribers", func(t *testing.T) {
		svc := newTestService()
		m := newTestManaged("s1", "copilot")
		addSubscriber(m)
		svc.storeManagedSession(m)

		items := svc.liveSessionSummaries("")
		if len(items) != 1 {
			t.Errorf("expected 1 summary; got %d", len(items))
		}
	})

	t.Run("filters_by_provider", func(t *testing.T) {
		svc := newTestService()

		m1 := newTestManaged("s1", "copilot")
		addSubscriber(m1)
		svc.storeManagedSession(m1)

		m2 := newTestManaged("s2", "claude")
		addSubscriber(m2)
		svc.storeManagedSession(m2)

		items := svc.liveSessionSummaries("copilot")
		if len(items) != 1 || items[0].SessionID != "s1" {
			t.Errorf("expected only copilot session; got %v", items)
		}
	})
}

// ---------------------------------------------------------------------------
// TestSessionService_disconnectAll
// ---------------------------------------------------------------------------

func TestSessionService_disconnectAll(t *testing.T) {
	svc := newTestService()
	svc.storeManagedSession(newTestManaged("s1", "copilot"))
	svc.storeManagedSession(newTestManaged("s2", "copilot"))

	svc.disconnectAll()

	if len(svc.sessions) != 0 {
		t.Errorf("expected all sessions removed; got %d", len(svc.sessions))
	}
}

func TestSessionService_closeManagedSessions(t *testing.T) {
	svc := newTestService()
	svc.storeManagedSession(newTestManaged("s1", "copilot"))
	svc.storeManagedSession(newTestManaged("s2", "claude"))

	svc.closeManagedSessions(nil)

	if len(svc.sessions) != 0 {
		t.Errorf("expected all sessions removed; got %d", len(svc.sessions))
	}
}
