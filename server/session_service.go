package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	copilot "github.com/github/copilot-sdk/go"
)

func (s *service) liveSessionSummaries(providerName string) []sessionSummary {
	s.sessionsMu.RLock()
	defer s.sessionsMu.RUnlock()

	items := make([]sessionSummary, 0, len(s.sessions))
	for _, managed := range s.sessions {
		if providerKey(providerName) != "" && providerKey(managed.provider) != providerKey(providerName) {
			continue
		}
		if !managed.hasSubscribers() {
			continue
		}
		items = append(items, managed.summary())
	}
	return items
}

func (s *service) resolveSessionWorkingDirectory(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return s.defaultWorkingDirectory, nil
	}
	return resolveWorkingDirectory(value)
}

func (s *service) resolveRequestedModel(ctx context.Context, provider providerRuntime, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", nil
	}
	if !isCopilotProviderType(provider.Type) {
		return requested, nil
	}

	models, err := withCopilotClientRetry(s, "list models", func(client copilotClient) ([]copilot.ModelInfo, error) {
		return client.ListModels(ctx)
	})
	if err != nil {
		return "", err
	}

	if resolved, ok := resolveModelAlias(requested, models); ok {
		return resolved, nil
	}
	return requested, nil
}

func sessionMapKey(providerName, sessionID string) string {
	return providerKey(providerName) + "::" + strings.TrimSpace(sessionID)
}

func (s *service) getManagedSession(id, providerName string) (*managedSession, bool, bool) {
	sessionID := strings.TrimSpace(id)
	if sessionID == "" {
		return nil, false, false
	}

	s.sessionsMu.RLock()
	defer s.sessionsMu.RUnlock()

	if providerKey(providerName) != "" {
		managed, ok := s.sessions[sessionMapKey(providerName, sessionID)]
		return managed, ok, false
	}

	var matched *managedSession
	for _, managed := range s.sessions {
		if managed == nil || managed.id() != sessionID {
			continue
		}
		if matched != nil {
			return nil, false, true
		}
		matched = managed
	}
	if matched == nil {
		return nil, false, false
	}
	return matched, true, false
}

func (s *service) requireManagedSession(w http.ResponseWriter, r *http.Request) (*managedSession, bool) {
	if r == nil {
		writeError(w, http.StatusBadRequest, "missing request")
		return nil, false
	}
	sessionID := r.PathValue("id")
	providerName := strings.TrimSpace(r.URL.Query().Get("provider"))
	if providerName != "" {
		if _, exists := s.providerRuntime(providerName); !exists {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown provider %q", providerName))
			return nil, false
		}
	}
	managed, ok, ambiguous := s.getManagedSession(sessionID, providerName)
	if ambiguous {
		writeError(w, http.StatusConflict, fmt.Sprintf("session %q exists for multiple providers; specify ?provider=", sessionID))
		return nil, false
	}
	if !ok {
		writeError(w, http.StatusNotFound, "session is not attached to this service")
		return nil, false
	}
	return managed, true
}

func (s *service) storeManagedSession(managed *managedSession) {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if managed == nil {
		return
	}
	id := managed.id()
	if id == "" {
		return
	}
	s.sessions[sessionMapKey(managed.provider, id)] = managed
}

func (s *service) removeManagedSession(id, providerName string) (*managedSession, bool, bool) {
	sessionID := strings.TrimSpace(id)
	if sessionID == "" {
		return nil, false, false
	}

	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if providerKey(providerName) != "" {
		key := sessionMapKey(providerName, sessionID)
		managed, ok := s.sessions[key]
		if ok {
			delete(s.sessions, key)
		}
		return managed, ok, false
	}

	var matchedKey string
	var matched *managedSession
	for key, managed := range s.sessions {
		if managed == nil || managed.id() != sessionID {
			continue
		}
		if matched != nil {
			return nil, false, true
		}
		matched = managed
		matchedKey = key
	}
	if matched != nil {
		delete(s.sessions, matchedKey)
		return matched, true, false
	}
	return nil, false, false
}

func (s *service) disconnectAll() {
	s.closeManagedSessions(errors.New("service shutdown"))
}

func (s *service) closeManagedSessions(reason error) {
	s.sessionsMu.Lock()
	sessions := make([]*managedSession, 0, len(s.sessions))
	for id, managed := range s.sessions {
		sessions = append(sessions, managed)
		delete(s.sessions, id)
	}
	s.sessionsMu.Unlock()

	for _, managed := range sessions {
		reasonText := ""
		if reason != nil {
			reasonText = reason.Error()
		}
		managed.broadcastHostEvent("host.session_disconnected", map[string]any{
			"sessionId":        managed.id(),
			"provider":         managed.provider,
			"serviceRestarted": reasonText != "" && reasonText != "service shutdown",
			"reason":           reasonText,
		})
		managed.close(reason)
	}
}

func clampInt64(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func (s *service) buildContextWindowSnapshot(ctx context.Context, managed *managedSession, usage *contextWindowUsage) *contextWindowSnapshot {
	if managed == nil || usage == nil {
		return nil
	}

	promptTokenLimit := usage.TokenLimit
	modelID := strings.TrimSpace(managed.model)
	if modelID != "" {
		models, err := withCopilotClientRetry(s, "list models", func(client copilotClient) ([]copilot.ModelInfo, error) {
			return client.ListModels(ctx)
		})
		if err == nil {
			for _, model := range models {
				if model.ID == modelID || strings.EqualFold(model.Name, modelID) {
					if model.Capabilities.Limits.MaxPromptTokens != nil && *model.Capabilities.Limits.MaxPromptTokens > 0 {
						promptTokenLimit = int64(*model.Capabilities.Limits.MaxPromptTokens)
					}
					break
				}
			}
		}
	}

	if promptTokenLimit <= 0 || promptTokenLimit > usage.TokenLimit {
		promptTokenLimit = usage.TokenLimit
	}

	systemToolsTokens := clampInt64(usage.SystemTokens + usage.ToolDefinitionsTokens)
	freeTokens := clampInt64(promptTokenLimit - usage.CurrentTokens)
	bufferTokens := clampInt64(usage.TokenLimit - maxInt64(usage.CurrentTokens, promptTokenLimit))

	return &contextWindowSnapshot{
		CurrentTokens:         usage.CurrentTokens,
		TokenLimit:            usage.TokenLimit,
		PromptTokenLimit:      promptTokenLimit,
		MessagesLength:        usage.MessagesLength,
		SystemTokens:          usage.SystemTokens,
		ToolDefinitionsTokens: usage.ToolDefinitionsTokens,
		SystemToolsTokens:     systemToolsTokens,
		ConversationTokens:    usage.ConversationTokens,
		FreeTokens:            freeTokens,
		BufferTokens:          bufferTokens,
	}
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (s *service) refreshManagedSessionSummary(ctx context.Context, managed *managedSession) {
	if managed == nil || managed.session == nil || !s.isCopilotProviderName(managed.provider) {
		return
	}

	meta, err := withCopilotClientRetry(s, "get session metadata", func(client copilotClient) (*copilot.SessionMetadata, error) {
		return client.GetSessionMetadata(ctx, managed.session.SessionID)
	})
	if err != nil || meta == nil || meta.Summary == nil {
		return
	}

	managed.sessionName = strings.TrimSpace(*meta.Summary)
}

func resolveWorkingDirectory(value string) (string, error) {
	workingDirectory := strings.TrimSpace(value)
	if workingDirectory == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("get working directory: %w", err)
		}
		workingDirectory = cwd
	}

	resolved, err := filepath.Abs(workingDirectory)
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	return resolved, nil
}

func defaultSessionStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".copilot", "session-state")
}

func enrichPersistedSessionsFromWorkspace(sessions []copilot.SessionMetadata, stateDir string) []copilot.SessionMetadata {
	if len(sessions) == 0 || strings.TrimSpace(stateDir) == "" {
		return sessions
	}

	enriched := make([]copilot.SessionMetadata, len(sessions))
	copy(enriched, sessions)
	for i := range enriched {
		if enriched[i].Context != nil && strings.TrimSpace(enriched[i].Context.Cwd) != "" {
			continue
		}

		context, ok := readWorkspaceContext(stateDir, enriched[i].SessionID)
		if !ok {
			continue
		}
		enriched[i].Context = context
	}
	return enriched
}

func readWorkspaceContext(stateDir, sessionID string) (*copilot.SessionContext, bool) {
	if strings.TrimSpace(stateDir) == "" || strings.TrimSpace(sessionID) == "" {
		return nil, false
	}

	data, err := os.ReadFile(filepath.Join(stateDir, sessionID, "workspace.yaml"))
	if err != nil {
		return nil, false
	}

	context := &copilot.SessionContext{}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(strings.Trim(value, `"'`))
		if value == "" {
			continue
		}

		switch strings.TrimSpace(key) {
		case "cwd":
			context.Cwd = value
		case "git_root":
			context.GitRoot = value
		case "repository":
			context.Repository = value
		case "branch":
			context.Branch = value
		}
	}

	if strings.TrimSpace(context.Cwd) == "" {
		return nil, false
	}
	return context, true
}

func resolveModelAlias(requested string, models []copilot.ModelInfo) (string, bool) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", false
	}

	var bestID string
	var bestVersion []int
	bestIsPlain := false
	foundFamilyAlias := false

	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		if strings.EqualFold(id, requested) {
			return id, true
		}

		version, isPlain, ok := parseVersionedModelAlias(requested, id)
		if !ok {
			continue
		}
		if !foundFamilyAlias || compareVersionParts(version, bestVersion) > 0 || (compareVersionParts(version, bestVersion) == 0 && isPlain && !bestIsPlain) {
			bestID = id
			bestVersion = version
			bestIsPlain = isPlain
			foundFamilyAlias = true
		}
	}

	if foundFamilyAlias {
		return bestID, true
	}
	return "", false
}

func parseVersionedModelAlias(requested, id string) ([]int, bool, bool) {
	prefix := requested + "."
	if !strings.HasPrefix(id, prefix) {
		return nil, false, false
	}

	remainder := strings.TrimPrefix(id, prefix)
	versionPart := remainder
	isPlain := true
	if idx := strings.Index(versionPart, "-"); idx >= 0 {
		versionPart = versionPart[:idx]
		isPlain = false
	}

	version, ok := parseNumericVersion(versionPart)
	if !ok {
		return nil, false, false
	}
	return version, isPlain, true
}

func parseNumericVersion(value string) ([]int, bool) {
	if value == "" {
		return nil, false
	}

	parts := strings.Split(value, ".")
	version := make([]int, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			return nil, false
		}
		number, err := strconv.Atoi(part)
		if err != nil {
			return nil, false
		}
		version = append(version, number)
	}
	return version, true
}

func compareVersionParts(left, right []int) int {
	limit := max(len(right), len(left))

	for i := range limit {
		leftPart := 0
		if i < len(left) {
			leftPart = left[i]
		}
		rightPart := 0
		if i < len(right) {
			rightPart = right[i]
		}
		switch {
		case leftPart > rightPart:
			return 1
		case leftPart < rightPart:
			return -1
		}
	}
	return 0
}
