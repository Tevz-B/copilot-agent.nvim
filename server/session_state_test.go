// Copyright 2026 ray-x. All rights reserved.
// Use of this source code is governed by an Apache 2.0
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"encoding/json"
	"math"
	"sync"
	"testing"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

// ---------------------------------------------------------------------------
// Mock providerSession
// ---------------------------------------------------------------------------

type mockProviderSession struct {
	sessionID     string
	workspacePath string
	capabilities  copilot.SessionCapabilities
}

func (m *mockProviderSession) SessionID() string { return m.sessionID }
func (m *mockProviderSession) SendMessage(_ context.Context, _ sendMessageOpts) (string, error) {
	return "", ErrNotSupported
}
func (m *mockProviderSession) Abort(_ context.Context) (bool, error) { return false, ErrNotSupported }
func (m *mockProviderSession) SetModel(_ context.Context, _, _ string) error {
	return ErrNotSupported
}
func (m *mockProviderSession) GetMessages(_ context.Context) (any, error) {
	return nil, ErrNotSupported
}
func (m *mockProviderSession) GetTasks(_ context.Context) ([]backgroundTaskView, error) {
	return nil, ErrNotSupported
}
func (m *mockProviderSession) CompactHistory(_ context.Context) (*rpc.HistoryCompactResult, error) {
	return nil, ErrNotSupported
}
func (m *mockProviderSession) FleetStart(_ context.Context, _ string) (*rpc.FleetStartResult, error) {
	return nil, ErrNotSupported
}
func (m *mockProviderSession) SetMode(_ context.Context, _ rpc.SessionMode) error {
	return ErrNotSupported
}
func (m *mockProviderSession) WorkspacePath() string                     { return m.workspacePath }
func (m *mockProviderSession) Capabilities() copilot.SessionCapabilities { return m.capabilities }
func (m *mockProviderSession) Disconnect() error                         { return nil }

// ---------------------------------------------------------------------------
// Helper to create a new managedSession for tests.
// ---------------------------------------------------------------------------

func newTestManagedSession() *managedSession {
	return &managedSession{
		subscribers:         make(map[chan sseMessage]struct{}),
		pendingInputs:       make(map[string]*pendingUserInput),
		pendingPermissions:  make(map[string]*pendingPermission),
		messageChunkIndexes: make(map[string]uint64),
	}
}

// ---------------------------------------------------------------------------
// int64FromFloat64
// ---------------------------------------------------------------------------

func TestSessionState_int64FromFloat64(t *testing.T) {
	tests := []struct {
		name  string
		input *float64
		want  int64
	}{
		{"nil", nil, 0},
		{"zero", ptrFloat64(0), 0},
		{"positive integer", ptrFloat64(42), 42},
		{"negative integer", ptrFloat64(-5), -5},
		{"rounds up", ptrFloat64(2.7), 3},
		{"rounds down", ptrFloat64(2.3), 2},
		{"rounds half up", ptrFloat64(2.5), 3},
		{"negative rounds", ptrFloat64(-1.5), -2},
		{"large value", ptrFloat64(1e9), 1000000000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := int64FromFloat64(tt.input)
			if got != tt.want {
				t.Errorf("int64FromFloat64(%v) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func ptrFloat64(v float64) *float64 { return &v }
func ptrInt64(v int64) *int64       { return &v }

// ---------------------------------------------------------------------------
// int64Value
// ---------------------------------------------------------------------------

func TestSessionState_int64Value(t *testing.T) {
	tests := []struct {
		name  string
		input *int64
		want  int64
	}{
		{"nil", nil, 0},
		{"zero", ptrInt64(0), 0},
		{"positive", ptrInt64(100), 100},
		{"negative", ptrInt64(-42), -42},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := int64Value(tt.input)
			if got != tt.want {
				t.Errorf("int64Value(%v) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// contextWindowUsageFromEvent
// ---------------------------------------------------------------------------

func TestSessionState_contextWindowUsageFromEvent(t *testing.T) {
	t.Run("nil data", func(t *testing.T) {
		if got := contextWindowUsageFromEvent(nil); got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})

	t.Run("all fields set", func(t *testing.T) {
		data := &copilot.SessionUsageInfoData{
			CurrentTokens:         1024.7,
			TokenLimit:            4096.3,
			MessagesLength:        10.5,
			SystemTokens:          ptrFloat64(200.9),
			ToolDefinitionsTokens: ptrFloat64(300.4),
			ConversationTokens:    ptrFloat64(500.5),
		}
		got := contextWindowUsageFromEvent(data)
		if got == nil {
			t.Fatal("expected non-nil")
		}
		assertInt64(t, "CurrentTokens", got.CurrentTokens, 1025)
		assertInt64(t, "TokenLimit", got.TokenLimit, 4096)
		assertInt64(t, "MessagesLength", got.MessagesLength, int64(math.Round(10.5)))
		assertInt64(t, "SystemTokens", got.SystemTokens, 201)
		assertInt64(t, "ToolDefinitionsTokens", got.ToolDefinitionsTokens, 300)
		assertInt64(t, "ConversationTokens", got.ConversationTokens, int64(math.Round(500.5)))
	})

	t.Run("optional fields nil", func(t *testing.T) {
		data := &copilot.SessionUsageInfoData{
			CurrentTokens:  100,
			TokenLimit:     200,
			MessagesLength: 5,
		}
		got := contextWindowUsageFromEvent(data)
		if got == nil {
			t.Fatal("expected non-nil")
		}
		assertInt64(t, "SystemTokens", got.SystemTokens, 0)
		assertInt64(t, "ToolDefinitionsTokens", got.ToolDefinitionsTokens, 0)
		assertInt64(t, "ConversationTokens", got.ConversationTokens, 0)
	})
}

func assertInt64(t *testing.T, name string, got, want int64) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %d, want %d", name, got, want)
	}
}

// ---------------------------------------------------------------------------
// sessionEventMessageID
// ---------------------------------------------------------------------------

func TestSessionState_sessionEventMessageID(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{"valid messageId", `{"data":{"messageId":"abc-123"}}`, "abc-123"},
		{"empty messageId", `{"data":{"messageId":""}}`, ""},
		{"whitespace messageId", `{"data":{"messageId":"  "}}`, ""},
		{"no data field", `{"type":"test"}`, ""},
		{"data is not object", `{"data":"string"}`, ""},
		{"invalid json", `{invalid`, ""},
		{"empty json", `{}`, ""},
		{"no messageId in data", `{"data":{"other":"value"}}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sessionEventMessageID([]byte(tt.payload))
			if got != tt.want {
				t.Errorf("sessionEventMessageID(%s) = %q, want %q", tt.payload, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// enrichSessionEventPayload
// ---------------------------------------------------------------------------

func TestSessionState_enrichSessionEventPayload(t *testing.T) {
	t.Run("adds sequenceId without chunk index", func(t *testing.T) {
		payload := []byte(`{"type":"test","data":{}}`)
		got, err := enrichSessionEventPayload(payload, 42, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]json.RawMessage
		if err := json.Unmarshal(got, &result); err != nil {
			t.Fatalf("unmarshal result: %v", err)
		}
		var seqID uint64
		if err := json.Unmarshal(result["sequenceId"], &seqID); err != nil {
			t.Fatalf("unmarshal sequenceId: %v", err)
		}
		if seqID != 42 {
			t.Errorf("sequenceId = %d, want 42", seqID)
		}
		if _, ok := result["messageChunkIndex"]; ok {
			t.Error("messageChunkIndex should not be present when nil")
		}
	})

	t.Run("adds both sequenceId and messageChunkIndex", func(t *testing.T) {
		payload := []byte(`{"type":"test"}`)
		chunkIdx := uint64(7)
		got, err := enrichSessionEventPayload(payload, 1, &chunkIdx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]json.RawMessage
		if err := json.Unmarshal(got, &result); err != nil {
			t.Fatalf("unmarshal result: %v", err)
		}
		var gotChunk uint64
		if err := json.Unmarshal(result["messageChunkIndex"], &gotChunk); err != nil {
			t.Fatalf("unmarshal messageChunkIndex: %v", err)
		}
		if gotChunk != 7 {
			t.Errorf("messageChunkIndex = %d, want 7", gotChunk)
		}
	})

	t.Run("invalid json returns error", func(t *testing.T) {
		_, err := enrichSessionEventPayload([]byte(`{invalid`), 1, nil)
		if err == nil {
			t.Error("expected error for invalid json")
		}
	})
}

// ---------------------------------------------------------------------------
// shouldReplayHistoryEvent
// ---------------------------------------------------------------------------

func TestSessionState_shouldReplayHistoryEvent(t *testing.T) {
	tests := []struct {
		name                    string
		event                   copilot.SessionEvent
		replayPermissionHistory bool
		want                    bool
	}{
		{
			name:                    "permission.requested with replay enabled",
			event:                   copilot.SessionEvent{Type: "permission.requested"},
			replayPermissionHistory: true,
			want:                    true,
		},
		{
			name:                    "permission.requested with replay disabled",
			event:                   copilot.SessionEvent{Type: "permission.requested"},
			replayPermissionHistory: false,
			want:                    false,
		},
		{
			name:                    "permission.completed with replay disabled",
			event:                   copilot.SessionEvent{Type: "permission.completed"},
			replayPermissionHistory: false,
			want:                    false,
		},
		{
			name:  "non-assistant event always replayed",
			event: copilot.SessionEvent{Type: "tool.execution_start"},
			want:  true,
		},
		{
			name: "assistant message with content",
			event: copilot.SessionEvent{
				Type: copilot.SessionEventTypeAssistantMessage,
				Data: &copilot.AssistantMessageData{Content: "hello"},
			},
			want: true,
		},
		{
			name: "assistant message empty content no tools",
			event: copilot.SessionEvent{
				Type: copilot.SessionEventTypeAssistantMessage,
				Data: &copilot.AssistantMessageData{Content: ""},
			},
			want: false,
		},
		{
			name: "assistant message whitespace content no tools",
			event: copilot.SessionEvent{
				Type: copilot.SessionEventTypeAssistantMessage,
				Data: &copilot.AssistantMessageData{Content: "   \n\t  "},
			},
			want: false,
		},
		{
			name: "assistant message empty content but has tool requests",
			event: copilot.SessionEvent{
				Type: copilot.SessionEventTypeAssistantMessage,
				Data: &copilot.AssistantMessageData{
					Content:      "",
					ToolRequests: []copilot.AssistantMessageToolRequest{{}},
				},
			},
			want: true,
		},
		{
			name: "assistant message with non-AssistantMessageData",
			event: copilot.SessionEvent{
				Type: copilot.SessionEventTypeAssistantMessage,
				Data: nil,
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldReplayHistoryEvent(tt.event, tt.replayPermissionHistory)
			if got != tt.want {
				t.Errorf("shouldReplayHistoryEvent() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// selectReplayWindow
// ---------------------------------------------------------------------------

func TestSessionState_selectReplayWindow(t *testing.T) {
	makeEvents := func(types ...string) []copilot.SessionEvent {
		events := make([]copilot.SessionEvent, len(types))
		for i, tp := range types {
			events[i] = copilot.SessionEvent{Type: copilot.SessionEventType(tp)}
		}
		return events
	}

	tests := []struct {
		name      string
		events    []copilot.SessionEvent
		turnLimit int
		wantLen   int
		wantFirst string
	}{
		{
			name:      "zero limit returns all",
			events:    makeEvents("assistant.turn_start", "assistant.message", "assistant.turn_start", "assistant.message"),
			turnLimit: 0,
			wantLen:   4,
		},
		{
			name:      "negative limit returns all",
			events:    makeEvents("assistant.turn_start", "assistant.message"),
			turnLimit: -1,
			wantLen:   2,
		},
		{
			name:      "limit >= turn count returns all",
			events:    makeEvents("assistant.turn_start", "tool.execution_start", "assistant.turn_start"),
			turnLimit: 5,
			wantLen:   3,
		},
		{
			name: "limit 1 of 3 turns returns last turn",
			events: makeEvents(
				"assistant.turn_start", "assistant.message",
				"assistant.turn_start", "tool.execution_start",
				"assistant.turn_start", "assistant.message",
			),
			turnLimit: 1,
			wantLen:   2,
			wantFirst: "assistant.turn_start",
		},
		{
			name: "limit 2 of 3 turns",
			events: makeEvents(
				"assistant.turn_start", "A",
				"assistant.turn_start", "B",
				"assistant.turn_start", "C",
			),
			turnLimit: 2,
			wantLen:   4,
			wantFirst: "assistant.turn_start",
		},
		{
			name:      "empty events",
			events:    nil,
			turnLimit: 5,
			wantLen:   0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := selectReplayWindow(tt.events, tt.turnLimit)
			if len(got) != tt.wantLen {
				t.Errorf("len = %d, want %d", len(got), tt.wantLen)
			}
			if tt.wantFirst != "" && len(got) > 0 && string(got[0].Type) != tt.wantFirst {
				t.Errorf("first event type = %q, want %q", got[0].Type, tt.wantFirst)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// truncateRunes
// ---------------------------------------------------------------------------

func TestSessionState_truncateRunes(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		maxChars int
		want     string
	}{
		{"empty string", "", 10, ""},
		{"within limit", "hello", 10, "hello"},
		{"exact limit", "hello", 5, "hello"},
		{"truncate with ellipsis", "hello world", 8, "hello..."},
		{"maxChars 0", "hello", 0, ""},
		{"maxChars 1", "abcdef", 1, "a"},
		{"maxChars 2", "abcdef", 2, "ab"},
		{"maxChars 3", "abcdef", 3, "abc"},
		{"maxChars 4 truncates with ellipsis", "abcdefgh", 4, "a..."},
		{"unicode chars", "日本語テスト", 4, "日..."},
		{"unicode within limit", "日本", 5, "日本"},
		{"negative maxChars", "hello", -1, ""},
		{"exactly 3 over", "abcd", 3, "abc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateRunes(tt.text, tt.maxChars)
			if got != tt.want {
				t.Errorf("truncateRunes(%q, %d) = %q, want %q", tt.text, tt.maxChars, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// previewString
// ---------------------------------------------------------------------------

func TestSessionState_previewString(t *testing.T) {
	tests := []struct {
		name     string
		value    any
		maxChars int
		want     string
	}{
		{"string value", "hello world", 20, "hello world"},
		{"string trimmed", "  hello  ", 20, "hello"},
		{"string truncated", "abcdefghij", 5, "ab..."},
		{"nil value", nil, 10, ""},
		{"int value", 42, 10, ""},
		{"map with content key", map[string]any{"content": "found it"}, 20, "found it"},
		{"map with text key", map[string]any{"text": "found text"}, 20, "found text"},
		{"map with output key", map[string]any{"output": "output val"}, 20, "output val"},
		{"map with message key", map[string]any{"message": "msg val"}, 20, "msg val"},
		{"map with value key", map[string]any{"value": "val"}, 20, "val"},
		{"map with detailedContent key", map[string]any{"detailedContent": "detail"}, 20, "detail"},
		{"map prefers detailedContent over content", map[string]any{"detailedContent": "detail", "content": "content"}, 20, "detail"},
		{"map with empty content falls through", map[string]any{"content": "  ", "text": "fallback"}, 20, "fallback"},
		{"map with contents key (nested)", map[string]any{"contents": "nested string"}, 20, "nested string"},
		{"map with no known keys", map[string]any{"unknown": "value"}, 20, ""},
		{"slice of strings", []any{"hello", "world"}, 30, "hello\n\nworld"},
		{"slice with empty items", []any{"", "hello"}, 20, "hello"},
		{"empty slice", []any{}, 10, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := previewString(tt.value, tt.maxChars)
			if got != tt.want {
				t.Errorf("previewString(%v, %d) = %q, want %q", tt.value, tt.maxChars, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// trimReplayPayload
// ---------------------------------------------------------------------------

func TestSessionState_trimReplayPayload(t *testing.T) {
	t.Run("streaming delta skipped in summary mode", func(t *testing.T) {
		payload := []byte(`{"type":"assistant.streaming_delta","data":{"content":"text"}}`)
		got, skipped, err := trimReplayPayload(payload, "assistant.streaming_delta", true, 120)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !skipped {
			t.Error("expected skipped=true for streaming delta in summary mode")
		}
		if got != nil {
			t.Errorf("expected nil payload when skipped, got %s", got)
		}
	})

	t.Run("streaming delta not skipped outside summary mode", func(t *testing.T) {
		payload := []byte(`{"type":"assistant.streaming_delta","data":{"content":"text"}}`)
		got, skipped, err := trimReplayPayload(payload, "assistant.streaming_delta", false, 120)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if skipped {
			t.Error("expected skipped=false outside summary mode")
		}
		if got == nil {
			t.Error("expected non-nil payload")
		}
	})

	t.Run("message_delta skipped in summary", func(t *testing.T) {
		payload := []byte(`{"type":"x","data":{"content":"text"}}`)
		_, skipped, err := trimReplayPayload(payload, "assistant.message_delta", true, 120)
		if err != nil {
			t.Fatal(err)
		}
		if !skipped {
			t.Error("expected skipped")
		}
	})

	t.Run("tool execution complete trims result", func(t *testing.T) {
		payload := []byte(`{"type":"tool.execution_complete","data":{"result":{"content":"a very long result string that should be previewed"}}}`)
		got, skipped, err := trimReplayPayload(payload, "tool.execution_complete", false, 20)
		if err != nil {
			t.Fatal(err)
		}
		if skipped {
			t.Error("should not skip tool.execution_complete")
		}
		var envelope map[string]any
		if err := json.Unmarshal(got, &envelope); err != nil {
			t.Fatal(err)
		}
		data, _ := envelope["data"].(map[string]any)
		result, _ := data["result"].(map[string]any)
		content, _ := result["content"].(string)
		if len([]rune(content)) > 20 {
			t.Errorf("content not truncated: %q (len=%d)", content, len([]rune(content)))
		}
	})

	t.Run("passthrough for unknown event type", func(t *testing.T) {
		payload := []byte(`{"type":"session.start","data":{"sessionId":"s1"}}`)
		got, skipped, err := trimReplayPayload(payload, "session.start", true, 120)
		if err != nil {
			t.Fatal(err)
		}
		if skipped {
			t.Error("should not skip session.start")
		}
		if got == nil {
			t.Error("expected payload")
		}
	})

	t.Run("invalid json returns error", func(t *testing.T) {
		_, _, err := trimReplayPayload([]byte(`{invalid`), "test", false, 120)
		if err == nil {
			t.Error("expected error")
		}
	})

	t.Run("no data field passthrough", func(t *testing.T) {
		payload := []byte(`{"type":"test"}`)
		got, skipped, err := trimReplayPayload(payload, "test", false, 120)
		if err != nil {
			t.Fatal(err)
		}
		if skipped {
			t.Error("should not skip")
		}
		if got == nil {
			t.Error("expected payload")
		}
	})
}

// ---------------------------------------------------------------------------
// managedSession.id()
// ---------------------------------------------------------------------------

func TestSessionState_managedSession_id(t *testing.T) {
	t.Run("nil receiver", func(t *testing.T) {
		var m *managedSession
		if got := m.id(); got != "" {
			t.Errorf("nil receiver should return empty, got %q", got)
		}
	})

	t.Run("ps set returns ps session id", func(t *testing.T) {
		m := newTestManagedSession()
		m.ps = &mockProviderSession{sessionID: "ps-id"}
		m.sessionID = "fallback"
		if got := m.id(); got != "ps-id" {
			t.Errorf("got %q, want %q", got, "ps-id")
		}
	})

	t.Run("ps returns empty falls through to sessionID", func(t *testing.T) {
		m := newTestManagedSession()
		m.ps = &mockProviderSession{sessionID: ""}
		m.sessionID = "field-id"
		if got := m.id(); got != "field-id" {
			t.Errorf("got %q, want %q", got, "field-id")
		}
	})

	t.Run("no ps uses sessionID field", func(t *testing.T) {
		m := newTestManagedSession()
		m.sessionID = "direct-id"
		if got := m.id(); got != "direct-id" {
			t.Errorf("got %q, want %q", got, "direct-id")
		}
	})

	t.Run("sessionID with whitespace is trimmed", func(t *testing.T) {
		m := newTestManagedSession()
		m.sessionID = "  trimmed  "
		if got := m.id(); got != "trimmed" {
			t.Errorf("got %q, want %q", got, "trimmed")
		}
	})
}

// ---------------------------------------------------------------------------
// managedSession.summary()
// ---------------------------------------------------------------------------

func TestSessionState_managedSession_summary(t *testing.T) {
	now := time.Now()
	m := newTestManagedSession()
	m.ps = &mockProviderSession{sessionID: "s1", workspacePath: "/workspace"}
	m.provider = "copilot"
	m.model = "gpt-4"
	m.agentMode = "interactive"
	m.workingDirectory = "/work"
	m.permissionMode = "approve-all"
	m.excludedTools = []string{"tool1"}
	m.sessionName = "my session"
	m.createdAt = now
	m.resumed = true
	m.streaming = true
	m.agent = "agent1"
	m.configDiscovery = true
	m.clientName = "nvim"
	m.instructionCount = 3
	m.agentCount = 2
	m.skillCount = 1
	m.mcpCount = 4

	s := m.summary()

	if s.SessionID != "s1" {
		t.Errorf("SessionID = %q, want %q", s.SessionID, "s1")
	}
	if s.Provider != "copilot" {
		t.Errorf("Provider = %q", s.Provider)
	}
	if s.Model != "gpt-4" {
		t.Errorf("Model = %q", s.Model)
	}
	if s.AgentMode != "interactive" {
		t.Errorf("AgentMode = %q", s.AgentMode)
	}
	if s.WorkingDirectory != "/work" {
		t.Errorf("WorkingDirectory = %q", s.WorkingDirectory)
	}
	if s.WorkspacePath != "/workspace" {
		t.Errorf("WorkspacePath = %q", s.WorkspacePath)
	}
	if s.PermissionMode != "approve-all" {
		t.Errorf("PermissionMode = %q", s.PermissionMode)
	}
	if s.Summary != "my session" {
		t.Errorf("Summary = %q", s.Summary)
	}
	if s.Live {
		t.Error("Live should be false with no subscribers")
	}
	if !s.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt mismatch")
	}
	if !s.Resumed {
		t.Error("Resumed should be true")
	}
	if !s.Streaming {
		t.Error("Streaming should be true")
	}
	if s.Agent != "agent1" {
		t.Errorf("Agent = %q", s.Agent)
	}
	if !s.ConfigDiscovery {
		t.Error("ConfigDiscovery should be true")
	}
	if s.ClientName != "nvim" {
		t.Errorf("ClientName = %q", s.ClientName)
	}
	if s.InstructionCount != 3 {
		t.Errorf("InstructionCount = %d", s.InstructionCount)
	}
	if s.MCPCount != 4 {
		t.Errorf("MCPCount = %d", s.MCPCount)
	}
}

// ---------------------------------------------------------------------------
// managedSession.broadcast
// ---------------------------------------------------------------------------

func TestSessionState_managedSession_broadcast(t *testing.T) {
	t.Run("fans out to all subscribers", func(t *testing.T) {
		m := newTestManagedSession()
		ch1 := m.subscribe()
		ch2 := m.subscribe()

		msg := sseMessage{Event: "test", Data: []byte("payload")}
		m.broadcast(msg)

		got1 := <-ch1
		got2 := <-ch2
		if got1.Event != "test" || string(got1.Data) != "payload" {
			t.Errorf("ch1 got %+v", got1)
		}
		if got2.Event != "test" || string(got2.Data) != "payload" {
			t.Errorf("ch2 got %+v", got2)
		}

		m.unsubscribe(ch1)
		m.unsubscribe(ch2)
	})

	t.Run("no subscribers no panic", func(t *testing.T) {
		m := newTestManagedSession()
		m.broadcast(sseMessage{Event: "test", Data: []byte("x")})
	})
}

// ---------------------------------------------------------------------------
// managedSession.subscribe / unsubscribe
// ---------------------------------------------------------------------------

func TestSessionState_managedSession_subscribe_unsubscribe(t *testing.T) {
	m := newTestManagedSession()

	ch := m.subscribe()
	if !m.hasSubscribers() {
		t.Error("expected hasSubscribers() true after subscribe")
	}

	m.unsubscribe(ch)
	if m.hasSubscribers() {
		t.Error("expected hasSubscribers() false after unsubscribe")
	}

	// Double unsubscribe should not panic.
	m.unsubscribe(ch)
}

// ---------------------------------------------------------------------------
// managedSession.hasSubscribers
// ---------------------------------------------------------------------------

func TestSessionState_managedSession_hasSubscribers(t *testing.T) {
	m := newTestManagedSession()
	if m.hasSubscribers() {
		t.Error("should be false initially")
	}
	ch := m.subscribe()
	if !m.hasSubscribers() {
		t.Error("should be true after subscribe")
	}
	m.unsubscribe(ch)
	if m.hasSubscribers() {
		t.Error("should be false after unsubscribe")
	}
}

// ---------------------------------------------------------------------------
// managedSession.nextSessionEventMetadata
// ---------------------------------------------------------------------------

func TestSessionState_managedSession_nextSessionEventMetadata(t *testing.T) {
	m := newTestManagedSession()

	t.Run("empty messageID returns nil chunk index", func(t *testing.T) {
		seqID, chunkIdx := m.nextSessionEventMetadata("")
		if seqID != 1 {
			t.Errorf("seqID = %d, want 1", seqID)
		}
		if chunkIdx != nil {
			t.Error("expected nil chunk index for empty messageID")
		}
	})

	t.Run("whitespace messageID returns nil chunk index", func(t *testing.T) {
		seqID, chunkIdx := m.nextSessionEventMetadata("   ")
		if seqID != 2 {
			t.Errorf("seqID = %d, want 2", seqID)
		}
		if chunkIdx != nil {
			t.Error("expected nil chunk index for whitespace messageID")
		}
	})

	t.Run("monotonic sequence IDs", func(t *testing.T) {
		seq3, _ := m.nextSessionEventMetadata("msg-a")
		seq4, _ := m.nextSessionEventMetadata("msg-a")
		if seq3 != 3 || seq4 != 4 {
			t.Errorf("sequences = %d, %d, want 3, 4", seq3, seq4)
		}
	})

	t.Run("per-messageID chunk indexing", func(t *testing.T) {
		m2 := newTestManagedSession()
		_, chunkA1 := m2.nextSessionEventMetadata("msgA")
		_, chunkA2 := m2.nextSessionEventMetadata("msgA")
		_, chunkB1 := m2.nextSessionEventMetadata("msgB")
		_, chunkA3 := m2.nextSessionEventMetadata("msgA")

		if chunkA1 == nil || *chunkA1 != 1 {
			t.Errorf("chunkA1 = %v, want 1", chunkA1)
		}
		if chunkA2 == nil || *chunkA2 != 2 {
			t.Errorf("chunkA2 = %v, want 2", chunkA2)
		}
		if chunkB1 == nil || *chunkB1 != 1 {
			t.Errorf("chunkB1 = %v, want 1", chunkB1)
		}
		if chunkA3 == nil || *chunkA3 != 3 {
			t.Errorf("chunkA3 = %v, want 3", chunkA3)
		}
	})

	t.Run("concurrent safety", func(t *testing.T) {
		m3 := newTestManagedSession()
		var wg sync.WaitGroup
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				m3.nextSessionEventMetadata("concurrent")
			}()
		}
		wg.Wait()
		// After 50 calls, nextEventSequence should be 50.
		if m3.nextEventSequence != 50 {
			t.Errorf("nextEventSequence = %d, want 50", m3.nextEventSequence)
		}
	})
}
