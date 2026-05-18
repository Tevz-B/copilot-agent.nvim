// Copyright 2026 ray-x. All rights reserved.
// Use of this source code is governed by an Apache 2.0
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Config discovery helpers
// ---------------------------------------------------------------------------

func countDiscoverableConfig(workingDirectory string) (instructionCount, agentCount, skillCount, mcpCount int) {
	if strings.TrimSpace(workingDirectory) == "" {
		return 0, 0, 0, 0
	}

	githubDir := filepath.Join(workingDirectory, ".github")
	if _, err := os.Stat(githubDir); err == nil {
		if fileExists(filepath.Join(githubDir, "copilot-instructions.md")) {
			instructionCount++
		}

		instructionsDir := filepath.Join(githubDir, "instructions")
		_ = filepath.WalkDir(instructionsDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d == nil || d.IsDir() {
				return nil
			}
			if strings.HasSuffix(d.Name(), ".instructions.md") {
				instructionCount++
			}
			return nil
		})

		agentsDir := filepath.Join(githubDir, "agents")
		_ = filepath.WalkDir(agentsDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d == nil || d.IsDir() {
				return nil
			}
			if strings.HasSuffix(d.Name(), ".agent.md") {
				agentCount++
			}
			return nil
		})

		skillsDir := filepath.Join(githubDir, "skills")
		_ = filepath.WalkDir(skillsDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d == nil || d.IsDir() {
				return nil
			}
			if d.Name() == "SKILL.md" || d.Name() == "skill.md" {
				skillCount++
			}
			return nil
		})
	}

	mcpCount += countMCPServersInFile(filepath.Join(workingDirectory, ".mcp.json"))
	mcpCount += countMCPServersInFile(filepath.Join(workingDirectory, ".vscode", "mcp.json"))
	if home, err := os.UserHomeDir(); err == nil {
		mcpCount += countMCPServersInFile(filepath.Join(home, ".copilot", "mcp-config.json"))
	}

	return instructionCount, agentCount, skillCount, mcpCount
}

func countMCPServersInFile(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return 0
	}

	if count := countMCPServerEntries(payload["mcpServers"]); count > 0 {
		return count
	}
	return countMCPServerEntries(payload["servers"])
}

func countMCPServerEntries(value any) int {
	switch v := value.(type) {
	case map[string]any:
		return len(v)
	case []any:
		return len(v)
	default:
		return 0
	}
}

// ---------------------------------------------------------------------------
// CLI path resolution
// ---------------------------------------------------------------------------

func defaultCLIPath() string {
	if env := strings.TrimSpace(os.Getenv("COPILOT_CLI_PATH")); env != "" {
		return env
	}

	candidates := []string{
		filepath.Join("..", "..", "nodejs", "node_modules", "@github", "copilot", "index.js"),
		filepath.Join("..", "nodejs", "node_modules", "@github", "copilot", "index.js"),
		filepath.Join("nodejs", "node_modules", "@github", "copilot", "index.js"),
	}

	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// HTTP middleware
// ---------------------------------------------------------------------------

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		logInfof("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				stack := debug.Stack()
				logErrorf("PANIC %s %s: %v\n%s", r.Method, r.URL.Path, rec, stack)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,DELETE,OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// JSON / SSE response helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

func decodeJSON(r *http.Request, target any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode JSON body: %w", err)
	}
	return nil
}

func writeSSE(w http.ResponseWriter, eventName string, payload []byte) error {
	if _, err := fmt.Fprintf(w, "event: %s\n", eventName); err != nil {
		return err
	}
	for line := range strings.SplitSeq(string(payload), "\n") {
		if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
			return err
		}
	}
	_, err := fmt.Fprint(w, "\n")
	return err
}

func mustJSON(payload any) []byte {
	data, err := json.Marshal(payload)
	if err != nil {
		return []byte(`{"error":"failed to marshal host event"}`)
	}
	return data
}

// ---------------------------------------------------------------------------
// Query / parameter helpers
// ---------------------------------------------------------------------------

func queryBool(r *http.Request, name string) bool {
	value := strings.TrimSpace(strings.ToLower(r.URL.Query().Get(name)))
	return value == "1" || value == "true" || value == "yes"
}

func queryInt(r *http.Request, name string) int {
	value := strings.TrimSpace(r.URL.Query().Get(name))
	if value == "" {
		return 0
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func boolOrDefault(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func stringOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func isValidPermissionMode(value string) bool {
	switch value {
	case permissionModeApproveAll, permissionModeRejectAll, permissionModeInteractive, permissionModeAutopilot, permissionModeApproveReads:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// Session ID generation
// ---------------------------------------------------------------------------

func sessionIDPrefixForWorkingDirectory(workingDirectory string) string {
	repo := strings.TrimSpace(filepath.Base(filepath.Clean(workingDirectory)))
	if repo == "" || repo == "." || repo == string(filepath.Separator) {
		return sessionIDPrefix
	}

	var builder strings.Builder
	lastSeparator := false
	for _, r := range strings.ToLower(repo) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
			lastSeparator = false
		case r == '-' || r == '_':
			if builder.Len() > 0 && !lastSeparator {
				builder.WriteRune(r)
				lastSeparator = true
			}
		default:
			if builder.Len() > 0 && !lastSeparator {
				builder.WriteByte('-')
				lastSeparator = true
			}
		}
	}

	prefix := strings.Trim(builder.String(), "-_")
	if len(prefix) > sessionIDPrefixMaxLen {
		prefix = strings.TrimRight(prefix[:sessionIDPrefixMaxLen], "-_")
	}
	if prefix == "" {
		return sessionIDPrefix
	}
	return prefix
}

func newSessionID(workingDirectory string) string {
	return fmt.Sprintf("%s-%d", sessionIDPrefixForWorkingDirectory(workingDirectory), time.Now().UnixNano())
}

// ---------------------------------------------------------------------------
// Service lifecycle HTTP handlers
// ---------------------------------------------------------------------------

func (s *service) handleShutdown(w http.ResponseWriter, r *http.Request) {
	logInfof("shutdown requested via HTTP path=%s remote=%s", r.URL.Path, r.RemoteAddr)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	if s.shutdownHTTP == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
		defer cancel()
		if err := s.shutdownHTTP(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logWarnf("shutdown via endpoint: %v", err)
		}
	}()
}

func (s *service) handleRegisterClient(w http.ResponseWriter, r *http.Request) {
	clientID := strings.TrimSpace(r.PathValue("id"))
	if clientID == "" {
		writeError(w, http.StatusBadRequest, "client id is required")
		return
	}

	var req registerClientRequest
	if err := decodeJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		logWarnf("client register decode failed clientID=%s err=%v", clientID, err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	count := s.registerClient(clientID, req.ClientName)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"clientId":      clientID,
		"clientName":    req.ClientName,
		"activeClients": count,
	})
}

func (s *service) handleUnregisterClient(w http.ResponseWriter, r *http.Request) {
	clientID := strings.TrimSpace(r.PathValue("id"))
	if clientID == "" {
		writeError(w, http.StatusBadRequest, "client id is required")
		return
	}

	count := s.unregisterClient(clientID)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"clientId":      clientID,
		"activeClients": count,
	})
}
