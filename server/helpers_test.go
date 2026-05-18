// Copyright 2026 ray-x. All rights reserved.
// Use of this source code is governed by an Apache 2.0
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// countMCPServerEntries
// ---------------------------------------------------------------------------

func TestHelpers_countMCPServerEntries(t *testing.T) {
	tests := []struct {
		name  string
		input any
		want  int
	}{
		{"nil", nil, 0},
		{"string", "hello", 0},
		{"int", 42, 0},
		{"empty map", map[string]any{}, 0},
		{"map with entries", map[string]any{"a": 1, "b": 2}, 2},
		{"empty slice", []any{}, 0},
		{"slice with entries", []any{1, 2, 3}, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countMCPServerEntries(tt.input); got != tt.want {
				t.Errorf("countMCPServerEntries() = %d, want %d", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// countMCPServersInFile
// ---------------------------------------------------------------------------

func TestHelpers_countMCPServersInFile(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		if got := countMCPServersInFile("/no/such/file.json"); got != 0 {
			t.Fatalf("expected 0, got %d", got)
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "bad.json")
		os.WriteFile(path, []byte("{not json}"), 0644)
		if got := countMCPServersInFile(path); got != 0 {
			t.Fatalf("expected 0, got %d", got)
		}
	})

	t.Run("mcpServers key", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "mcp.json")
		os.WriteFile(path, []byte(`{"mcpServers":{"s1":{},"s2":{}}}`), 0644)
		if got := countMCPServersInFile(path); got != 2 {
			t.Fatalf("expected 2, got %d", got)
		}
	})

	t.Run("servers key fallback", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "mcp.json")
		os.WriteFile(path, []byte(`{"servers":{"x":{}}}`), 0644)
		if got := countMCPServersInFile(path); got != 1 {
			t.Fatalf("expected 1, got %d", got)
		}
	})

	t.Run("mcpServers takes precedence", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "mcp.json")
		os.WriteFile(path, []byte(`{"mcpServers":{"a":{},"b":{},"c":{}},"servers":{"x":{}}}`), 0644)
		if got := countMCPServersInFile(path); got != 3 {
			t.Fatalf("expected 3, got %d", got)
		}
	})

	t.Run("no relevant keys", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "mcp.json")
		os.WriteFile(path, []byte(`{"other":"value"}`), 0644)
		if got := countMCPServersInFile(path); got != 0 {
			t.Fatalf("expected 0, got %d", got)
		}
	})
}

// ---------------------------------------------------------------------------
// countDiscoverableConfig
// ---------------------------------------------------------------------------

func TestHelpers_countDiscoverableConfig(t *testing.T) {
	t.Run("empty working directory", func(t *testing.T) {
		i, a, s, m := countDiscoverableConfig("")
		if i+a+s+m != 0 {
			t.Fatalf("expected all zeros for empty dir, got %d %d %d %d", i, a, s, m)
		}
	})

	t.Run("full structure", func(t *testing.T) {
		dir := t.TempDir()
		ghDir := filepath.Join(dir, ".github")

		// copilot-instructions.md
		os.MkdirAll(ghDir, 0755)
		os.WriteFile(filepath.Join(ghDir, "copilot-instructions.md"), []byte("hi"), 0644)

		// .github/instructions/*.instructions.md
		instDir := filepath.Join(ghDir, "instructions")
		os.MkdirAll(instDir, 0755)
		os.WriteFile(filepath.Join(instDir, "one.instructions.md"), []byte(""), 0644)
		os.WriteFile(filepath.Join(instDir, "two.instructions.md"), []byte(""), 0644)

		// .github/agents/*.agent.md
		agentsDir := filepath.Join(ghDir, "agents")
		os.MkdirAll(agentsDir, 0755)
		os.WriteFile(filepath.Join(agentsDir, "helper.agent.md"), []byte(""), 0644)

		// .github/skills/SKILL.md
		skillsDir := filepath.Join(ghDir, "skills")
		os.MkdirAll(skillsDir, 0755)
		os.WriteFile(filepath.Join(skillsDir, "SKILL.md"), []byte(""), 0644)

		// .mcp.json
		os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(`{"mcpServers":{"s1":{}}}`), 0644)

		instructions, agents, skills, mcp := countDiscoverableConfig(dir)
		// 1 copilot-instructions.md + 2 *.instructions.md = 3
		if instructions != 3 {
			t.Errorf("instructions: got %d, want 3", instructions)
		}
		if agents != 1 {
			t.Errorf("agents: got %d, want 1", agents)
		}
		if skills != 1 {
			t.Errorf("skills: got %d, want 1", skills)
		}
		// mcp from .mcp.json (home dir config may also contribute)
		if mcp < 1 {
			t.Errorf("mcp: got %d, want >= 1", mcp)
		}
	})

	t.Run("no .github dir", func(t *testing.T) {
		dir := t.TempDir()
		i, a, s, _ := countDiscoverableConfig(dir)
		if i+a+s != 0 {
			t.Fatalf("expected 0 instructions/agents/skills without .github, got %d %d %d", i, a, s)
		}
	})
}

// ---------------------------------------------------------------------------
// loggingMiddleware
// ---------------------------------------------------------------------------

func TestHelpers_loggingMiddleware(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := loggingMiddleware(inner)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Fatal("inner handler was not called")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// recoveryMiddleware
// ---------------------------------------------------------------------------

func TestHelpers_recoveryMiddleware(t *testing.T) {
	t.Run("normal", func(t *testing.T) {
		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		handler := recoveryMiddleware(inner)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ok", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
	})

	t.Run("panic", func(t *testing.T) {
		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			panic("boom")
		})
		handler := recoveryMiddleware(inner)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panic", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500, got %d", rec.Code)
		}
	})
}

// ---------------------------------------------------------------------------
// withCORS
// ---------------------------------------------------------------------------

func TestHelpers_withCORS(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := withCORS(inner)

	t.Run("normal request", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Fatalf("expected CORS origin *, got %q", got)
		}
		if got := rec.Header().Get("Access-Control-Allow-Methods"); got == "" {
			t.Fatal("expected CORS methods header")
		}
	})

	t.Run("OPTIONS preflight", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodOptions, "/", nil))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d", rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Fatalf("expected CORS origin *, got %q", got)
		}
	})
}

// ---------------------------------------------------------------------------
// writeJSON
// ---------------------------------------------------------------------------

func TestHelpers_writeJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusCreated, map[string]string{"key": "value"})

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected application/json, got %q", ct)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if body["key"] != "value" {
		t.Fatalf("expected key=value, got %v", body)
	}
}

// ---------------------------------------------------------------------------
// writeError
// ---------------------------------------------------------------------------

func TestHelpers_writeError(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, http.StatusBadRequest, "something went wrong")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "something went wrong" {
		t.Fatalf("expected error message, got %v", body)
	}
}

// ---------------------------------------------------------------------------
// decodeJSON
// ---------------------------------------------------------------------------

func TestHelpers_decodeJSON(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}

	t.Run("valid", func(t *testing.T) {
		body := bytes.NewBufferString(`{"name":"alice","age":30}`)
		req := httptest.NewRequest(http.MethodPost, "/", body)
		var p payload
		if err := decodeJSON(req, &p); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p.Name != "alice" || p.Age != 30 {
			t.Fatalf("unexpected decoded value: %+v", p)
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		body := bytes.NewBufferString(`{bad}`)
		req := httptest.NewRequest(http.MethodPost, "/", body)
		var p payload
		if err := decodeJSON(req, &p); err == nil {
			t.Fatal("expected error for invalid JSON")
		}
	})

	t.Run("unknown fields rejected", func(t *testing.T) {
		body := bytes.NewBufferString(`{"name":"alice","unknown":"field"}`)
		req := httptest.NewRequest(http.MethodPost, "/", body)
		var p payload
		if err := decodeJSON(req, &p); err == nil {
			t.Fatal("expected error for unknown fields")
		}
	})
}

// ---------------------------------------------------------------------------
// writeSSE
// ---------------------------------------------------------------------------

func TestHelpers_writeSSE(t *testing.T) {
	t.Run("single line", func(t *testing.T) {
		rec := httptest.NewRecorder()
		err := writeSSE(rec, "test.event", []byte(`{"ok":true}`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got := rec.Body.String()
		if !strings.HasPrefix(got, "event: test.event\n") {
			t.Fatalf("expected event line, got %q", got)
		}
		if !strings.Contains(got, "data: {\"ok\":true}\n") {
			t.Fatalf("expected data line, got %q", got)
		}
		if !strings.HasSuffix(got, "\n\n") {
			t.Fatalf("expected trailing blank line, got %q", got)
		}
	})

	t.Run("multi-line payload", func(t *testing.T) {
		rec := httptest.NewRecorder()
		err := writeSSE(rec, "multi", []byte("line1\nline2\nline3"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got := rec.Body.String()
		if strings.Count(got, "data: ") != 3 {
			t.Fatalf("expected 3 data lines, got %q", got)
		}
	})
}

// ---------------------------------------------------------------------------
// mustJSON
// ---------------------------------------------------------------------------

func TestHelpers_mustJSON(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		data := mustJSON(map[string]int{"x": 1})
		var m map[string]int
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("invalid JSON output: %v", err)
		}
		if m["x"] != 1 {
			t.Fatalf("expected x=1, got %v", m)
		}
	})

	t.Run("unmarshalable", func(t *testing.T) {
		data := mustJSON(make(chan int))
		if !strings.Contains(string(data), "error") {
			t.Fatalf("expected error JSON, got %q", data)
		}
	})
}

// ---------------------------------------------------------------------------
// queryBool
// ---------------------------------------------------------------------------

func TestHelpers_queryBool(t *testing.T) {
	tests := []struct {
		query string
		want  bool
	}{
		{"flag=1", true},
		{"flag=true", true},
		{"flag=yes", true},
		{"flag=TRUE", true},
		{"flag=Yes", true},
		{"flag=0", false},
		{"flag=false", false},
		{"flag=no", false},
		{"flag=", false},
		{"other=1", false},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/?"+tt.query, nil)
			if got := queryBool(req, "flag"); got != tt.want {
				t.Errorf("queryBool() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// queryInt
// ---------------------------------------------------------------------------

func TestHelpers_queryInt(t *testing.T) {
	tests := []struct {
		query string
		want  int
	}{
		{"n=42", 42},
		{"n=0", 0},
		{"n=-1", 0},
		{"n=abc", 0},
		{"n=", 0},
		{"other=5", 0},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/?"+tt.query, nil)
			if got := queryInt(req, "n"); got != tt.want {
				t.Errorf("queryInt() = %d, want %d", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// boolOrDefault
// ---------------------------------------------------------------------------

func TestHelpers_boolOrDefault(t *testing.T) {
	tr, fa := true, false
	tests := []struct {
		name     string
		value    *bool
		fallback bool
		want     bool
	}{
		{"nil true fallback", nil, true, true},
		{"nil false fallback", nil, false, false},
		{"true ptr", &tr, false, true},
		{"false ptr", &fa, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := boolOrDefault(tt.value, tt.fallback); got != tt.want {
				t.Errorf("boolOrDefault() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// stringOrEmpty
// ---------------------------------------------------------------------------

func TestHelpers_stringOrEmpty(t *testing.T) {
	s := "hello"
	if got := stringOrEmpty(nil); got != "" {
		t.Errorf("expected empty string for nil, got %q", got)
	}
	if got := stringOrEmpty(&s); got != "hello" {
		t.Errorf("expected hello, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// firstNonEmpty
// ---------------------------------------------------------------------------

func TestHelpers_firstNonEmpty(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   string
	}{
		{"all empty", []string{"", " ", "\t"}, ""},
		{"first non-empty", []string{"", "hello", "world"}, "hello"},
		{"trims whitespace", []string{"  ", " hi  ", "there"}, "hi"},
		{"no args", nil, ""},
		{"single", []string{"only"}, "only"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstNonEmpty(tt.values...); got != tt.want {
				t.Errorf("firstNonEmpty() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// isValidPermissionMode
// ---------------------------------------------------------------------------

func TestHelpers_isValidPermissionMode(t *testing.T) {
	valid := []string{"approve-all", "reject-all", "interactive", "autopilot", "approve-reads"}
	for _, mode := range valid {
		if !isValidPermissionMode(mode) {
			t.Errorf("isValidPermissionMode(%q) = false, want true", mode)
		}
	}

	invalid := []string{"", "unknown", "APPROVE-ALL", "approve_all"}
	for _, mode := range invalid {
		if isValidPermissionMode(mode) {
			t.Errorf("isValidPermissionMode(%q) = true, want false", mode)
		}
	}
}

// ---------------------------------------------------------------------------
// sessionIDPrefixForWorkingDirectory
// ---------------------------------------------------------------------------

func TestHelpers_sessionIDPrefixForWorkingDirectory(t *testing.T) {
	tests := []struct {
		name string
		dir  string
		want string
	}{
		{"simple repo", "/home/user/myrepo", "myrepo"},
		{"dotted name", "/tmp/go.nvim", "go-nvim"},
		{"long name truncated", "/tmp/copilot-agent.nvim", "copilot"},
		{"special chars", "/tmp/My Repo!", "my-repo"},
		{"all special", "/tmp/!!!", sessionIDPrefix},
		{"empty string", "", sessionIDPrefix},
		{"just slash", "/", sessionIDPrefix},
		{"trailing slash", "/home/user/project/", "project"},
		{"mixed case", "/tmp/MyProject", "myprojec"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sessionIDPrefixForWorkingDirectory(tt.dir)
			if got != tt.want {
				t.Errorf("sessionIDPrefixForWorkingDirectory(%q) = %q, want %q", tt.dir, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// newSessionID
// ---------------------------------------------------------------------------

func TestHelpers_newSessionID(t *testing.T) {
	id := newSessionID("/home/user/myrepo")
	if !strings.HasPrefix(id, "myrepo-") {
		t.Fatalf("expected prefix myrepo-, got %q", id)
	}
	// Should contain a timestamp portion
	parts := strings.SplitN(id, "-", 2)
	if len(parts) != 2 || parts[1] == "" {
		t.Fatalf("expected prefix-timestamp format, got %q", id)
	}
}
