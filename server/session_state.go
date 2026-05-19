package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

type userInputResult struct {
	Response copilot.UserInputResponse
	Err      error
}

type pendingUserInputView struct {
	ID            string    `json:"id"`
	Question      string    `json:"question"`
	Choices       []string  `json:"choices,omitempty"`
	AllowFreeform bool      `json:"allowFreeform"`
	CreatedAt     time.Time `json:"createdAt"`
}

type pendingUserInput struct {
	view     pendingUserInputView
	resultCh chan userInputResult
}

type permissionResult struct {
	Approved bool
	Err      error
}

type pendingPermissionView struct {
	ID        string                    `json:"id"`
	Request   copilot.PermissionRequest `json:"request"`
	CreatedAt time.Time                 `json:"createdAt"`
}

type pendingPermission struct {
	view     pendingPermissionView
	resultCh chan permissionResult
}

type sseMessage struct {
	Event string
	Data  []byte
}

type sessionSummary struct {
	SessionID         string                      `json:"sessionId"`
	Provider          string                      `json:"provider"`
	Model             string                      `json:"model,omitempty"`
	AgentMode         string                      `json:"agentMode,omitempty"`
	WorkingDirectory  string                      `json:"workingDirectory,omitempty"`
	WorkspacePath     string                      `json:"workspacePath,omitempty"`
	PermissionMode    string                      `json:"permissionMode"`
	ExcludedTools     []string                    `json:"excludedTools,omitempty"`
	Capabilities      copilot.SessionCapabilities `json:"capabilities"`
	PendingUserInputs []pendingUserInputView      `json:"pendingUserInputs,omitempty"`
	Summary           string                      `json:"summary,omitempty"`
	Live              bool                        `json:"live"`
	CreatedAt         time.Time                   `json:"createdAt"`
	Resumed           bool                        `json:"resumed"`
	Streaming         bool                        `json:"streaming"`
	Agent             string                      `json:"agent,omitempty"`
	ConfigDiscovery   bool                        `json:"configDiscovery"`
	ClientName        string                      `json:"clientName,omitempty"`
	InstructionCount  int                         `json:"instructionCount,omitempty"`
	AgentCount        int                         `json:"agentCount,omitempty"`
	SkillCount        int                         `json:"skillCount,omitempty"`
	MCPCount          int                         `json:"mcpCount,omitempty"`
}

type managedSession struct {
	session              *copilot.Session // deprecated: use ps field; retained for transition
	ps                   providerSession
	sessionID            string
	provider             string
	model                string
	agentMode            string // "interactive", "plan", or "autopilot"
	workingDirectory     string
	permissionMode       string
	excludedTools        []string
	sessionName          string // auto-generated session name provided by the SDK after each turn
	createdAt            time.Time
	resumed              bool
	streaming            bool
	agent                string
	configDiscovery      bool
	clientName           string
	instructionCount     int
	agentCount           int
	skillCount           int
	mcpCount             int
	subscribers          map[chan sseMessage]struct{}
	subscribersMu        sync.RWMutex
	pendingInputs        map[string]*pendingUserInput
	pendingInputsMu      sync.Mutex
	pendingPermissions   map[string]*pendingPermission
	pendingPermissionsMu sync.Mutex
	eventSequenceMu      sync.Mutex
	nextEventSequence    uint64
	messageChunkIndexes  map[string]uint64
	contextUsage         *contextWindowUsage
	contextUsageMu       sync.RWMutex
	eventUnsubscribe     func()
	inputResponseGrace   time.Duration
	claudeEventHistory   []json.RawMessage
	claudeEventHistoryMu sync.RWMutex
	claudeMessages       []anthropic.MessageParam
	claudeMessagesMu     sync.RWMutex
	claudeSystemPrompt   string
	claudeAPIKey         string
	claudeAuthToken      string
	claudeAPIKeyEnv      string
	claudeAuthTokenEnv   string
	claudeBaseURL        string
	claudeTurnCancel     context.CancelFunc
	claudeTurnMu         sync.Mutex
}

type contextWindowUsage struct {
	CurrentTokens         int64
	TokenLimit            int64
	MessagesLength        int64
	SystemTokens          int64
	ToolDefinitionsTokens int64
	ConversationTokens    int64
}

func (m *managedSession) handleSessionEvent(event copilot.SessionEvent) {
	if event.Type == copilot.SessionEventTypeSessionUsageInfo {
		if data, ok := event.Data.(*copilot.SessionUsageInfoData); ok {
			m.setContextUsage(contextWindowUsageFromEvent(data))
		}
	}

	// Drop assistant.message events with empty or whitespace-only content
	// so the client never sees bare "Assistant:" entries.
	if event.Type == copilot.SessionEventTypeAssistantMessage {
		if data, ok := event.Data.(*copilot.AssistantMessageData); ok {
			if strings.TrimSpace(data.Content) == "" && len(data.ToolRequests) == 0 {
				return
			}
		}
	}

	payload, err := m.marshalSequencedSessionEvent(event)
	if err != nil {
		return
	}
	m.broadcast(sseMessage{Event: "session.event", Data: payload})
}

func (m *managedSession) setContextUsage(usage *contextWindowUsage) {
	m.contextUsageMu.Lock()
	defer m.contextUsageMu.Unlock()
	if usage == nil {
		m.contextUsage = nil
		return
	}
	copy := *usage
	m.contextUsage = &copy
}

func (m *managedSession) getContextUsage() *contextWindowUsage {
	m.contextUsageMu.RLock()
	defer m.contextUsageMu.RUnlock()
	if m.contextUsage == nil {
		return nil
	}
	copy := *m.contextUsage
	return &copy
}

func int64FromFloat64(value *float64) int64 {
	if value == nil {
		return 0
	}
	return int64(math.Round(*value))
}

func contextWindowUsageFromEvent(data *copilot.SessionUsageInfoData) *contextWindowUsage {
	if data == nil {
		return nil
	}
	return &contextWindowUsage{
		CurrentTokens:         int64(math.Round(data.CurrentTokens)),
		TokenLimit:            int64(math.Round(data.TokenLimit)),
		MessagesLength:        int64(math.Round(data.MessagesLength)),
		SystemTokens:          int64FromFloat64(data.SystemTokens),
		ToolDefinitionsTokens: int64FromFloat64(data.ToolDefinitionsTokens),
		ConversationTokens:    int64FromFloat64(data.ConversationTokens),
	}
}

func contextWindowUsageFromCompact(data *rpc.HistoryCompactContextWindow) *contextWindowUsage {
	if data == nil {
		return nil
	}
	return &contextWindowUsage{
		CurrentTokens:         data.CurrentTokens,
		TokenLimit:            data.TokenLimit,
		MessagesLength:        data.MessagesLength,
		SystemTokens:          int64Value(data.SystemTokens),
		ToolDefinitionsTokens: int64Value(data.ToolDefinitionsTokens),
		ConversationTokens:    int64Value(data.ConversationTokens),
	}
}

func int64Value(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func (m *managedSession) nextSessionEventMetadata(messageID string) (uint64, *uint64) {
	m.eventSequenceMu.Lock()
	defer m.eventSequenceMu.Unlock()
	m.nextEventSequence++
	sequenceID := m.nextEventSequence
	if strings.TrimSpace(messageID) == "" {
		return sequenceID, nil
	}
	m.messageChunkIndexes[messageID]++
	chunkIndex := m.messageChunkIndexes[messageID]
	return sequenceID, &chunkIndex
}

func (m *managedSession) marshalSequencedSessionEvent(event copilot.SessionEvent) ([]byte, error) {
	payload, messageID, err := marshalSessionEventPayload(event)
	if err != nil {
		return nil, err
	}
	sequenceID, messageChunkIndex := m.nextSessionEventMetadata(messageID)
	return enrichSessionEventPayload(payload, sequenceID, messageChunkIndex)
}

func marshalSessionEventPayload(event copilot.SessionEvent) ([]byte, string, error) {
	payload, err := (&event).Marshal()
	if err != nil {
		return nil, "", err
	}
	return payload, sessionEventMessageID(payload), nil
}

func enrichSessionEventPayload(payload []byte, sequenceID uint64, messageChunkIndex *uint64) ([]byte, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, err
	}

	encodedSequenceID, err := json.Marshal(sequenceID)
	if err != nil {
		return nil, err
	}
	envelope["sequenceId"] = encodedSequenceID

	if messageChunkIndex != nil {
		encodedChunkIndex, err := json.Marshal(*messageChunkIndex)
		if err != nil {
			return nil, err
		}
		envelope["messageChunkIndex"] = encodedChunkIndex
	}

	return json.Marshal(envelope)
}

func sessionEventMessageID(payload []byte) string {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil || len(envelope.Data) == 0 {
		return ""
	}

	var message struct {
		MessageID string `json:"messageId"`
	}
	if err := json.Unmarshal(envelope.Data, &message); err != nil {
		return ""
	}
	return strings.TrimSpace(message.MessageID)
}

func shouldReplayHistoryEvent(event copilot.SessionEvent, replayPermissionHistory bool) bool {
	if event.Type == "permission.requested" || event.Type == "permission.completed" {
		return replayPermissionHistory
	}
	if event.Type != copilot.SessionEventTypeAssistantMessage {
		return true
	}
	data, ok := event.Data.(*copilot.AssistantMessageData)
	if !ok {
		return true
	}
	return strings.TrimSpace(data.Content) != "" || len(data.ToolRequests) > 0
}

func historyReplayTurnLimit(r *http.Request) int {
	return queryInt(r, "history_turn_limit")
}

func historyReplayActivityTurnLimit(r *http.Request) int {
	return queryInt(r, "history_activity_turn_limit")
}

func historyReplayPreviewChars(r *http.Request) int {
	value := queryInt(r, "history_preview_chars")
	if value > 0 {
		return value
	}
	return 120
}

func selectReplayWindow(events []copilot.SessionEvent, turnLimit int) []copilot.SessionEvent {
	if turnLimit <= 0 {
		return events
	}

	turnCount := 0
	for _, event := range events {
		if event.Type == "assistant.turn_start" {
			turnCount++
		}
	}
	if turnCount <= turnLimit {
		return events
	}

	keepTurns := turnLimit
	cutoffTurns := turnCount - keepTurns
	seenTurns := 0
	startIndex := 0
	for idx, event := range events {
		if event.Type != "assistant.turn_start" {
			continue
		}
		seenTurns++
		if seenTurns > cutoffTurns {
			startIndex = idx
			break
		}
	}
	if startIndex <= 0 {
		return events
	}
	return events[startIndex:]
}

func truncateRunes(text string, maxChars int) string {
	if maxChars <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxChars {
		return text
	}
	if maxChars <= 3 {
		return string(runes[:maxChars])
	}
	return string(runes[:maxChars-3]) + "..."
}

func previewString(value any, maxChars int) string {
	switch v := value.(type) {
	case string:
		return truncateRunes(strings.TrimSpace(v), maxChars)
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if preview := previewString(item, maxChars); preview != "" {
				parts = append(parts, preview)
			}
		}
		return truncateRunes(strings.Join(parts, "\n\n"), maxChars)
	case map[string]any:
		for _, key := range []string{"detailedContent", "content", "text", "output", "message", "value"} {
			if s, ok := v[key].(string); ok && strings.TrimSpace(s) != "" {
				return truncateRunes(strings.TrimSpace(s), maxChars)
			}
		}
		if nested, ok := v["contents"]; ok {
			return previewString(nested, maxChars)
		}
	}
	return ""
}

func trimReplayPayload(payload []byte, eventType string, summaryMode bool, previewChars int) ([]byte, bool, error) {
	var envelope map[string]any
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return payload, false, err
	}

	data, _ := envelope["data"].(map[string]any)
	if data == nil {
		return payload, false, nil
	}

	switch eventType {
	case "assistant.message_delta", "assistant.streaming_delta", "assistant.reasoning_delta", "tool.execution_partial_result", "tool.execution_progress":
		if summaryMode {
			return nil, true, nil
		}
	case "tool.execution_complete":
		if result, ok := data["result"].(map[string]any); ok {
			preview := previewString(result, previewChars)
			if preview != "" {
				result = map[string]any{"content": truncateRunes(preview, previewChars)}
				data["result"] = result
			}
		}
	}

	envelope["data"] = data
	trimmed, err := json.Marshal(envelope)
	if err != nil {
		return nil, false, err
	}
	return trimmed, false, nil
}

func (m *managedSession) marshalReplaySessionEvents(events []copilot.SessionEvent, replayPermissionHistory bool, turnLimit, activityLimit, previewChars int) ([][]byte, int) {
	events = selectReplayWindow(events, turnLimit)
	if activityLimit > 0 && activityLimit > turnLimit && turnLimit > 0 {
		activityLimit = turnLimit
	}

	payloads := make([][]byte, 0, len(events))
	nextSequence := uint64(0)
	messageChunkIndexes := make(map[string]uint64)
	totalTurns := 0
	for _, event := range events {
		if event.Type == "assistant.turn_start" {
			totalTurns++
		}
	}
	summaryCutoff := totalTurns - activityLimit

	currentTurn := 0
	for _, event := range events {
		if !shouldReplayHistoryEvent(event, replayPermissionHistory) {
			continue
		}
		if event.Type == "assistant.turn_start" {
			currentTurn++
		}
		summaryMode := activityLimit > 0 && currentTurn > 0 && currentTurn <= summaryCutoff
		rawPayload, messageID, err := marshalSessionEventPayload(event)
		if err != nil {
			continue
		}
		trimmedPayload, skipped, err := trimReplayPayload(rawPayload, string(event.Type), summaryMode, previewChars)
		if err != nil {
			continue
		}
		if skipped {
			continue
		}
		nextSequence++
		sequenceID := nextSequence
		var messageChunkIndex *uint64
		if messageID != "" {
			messageChunkIndexes[messageID]++
			chunkIndex := messageChunkIndexes[messageID]
			messageChunkIndex = &chunkIndex
		}
		enriched, err := enrichSessionEventPayload(trimmedPayload, sequenceID, func() *uint64 {
			return messageChunkIndex
		}())
		if err != nil {
			continue
		}
		payloads = append(payloads, enriched)
	}

	m.eventSequenceMu.Lock()
	if m.nextEventSequence < nextSequence {
		m.nextEventSequence = nextSequence
	}
	if len(messageChunkIndexes) > 0 {
		if m.messageChunkIndexes == nil {
			m.messageChunkIndexes = make(map[string]uint64)
		}
		for messageID, chunkIndex := range messageChunkIndexes {
			if existing := m.messageChunkIndexes[messageID]; existing < chunkIndex {
				m.messageChunkIndexes[messageID] = chunkIndex
			}
		}
	}
	m.eventSequenceMu.Unlock()

	return payloads, len(events)
}

func (m *managedSession) id() string {
	if m == nil {
		return ""
	}
	if m.ps != nil {
		if sid := m.ps.SessionID(); sid != "" {
			return sid
		}
	}
	if m.session != nil && strings.TrimSpace(m.session.SessionID) != "" {
		return m.session.SessionID
	}
	return strings.TrimSpace(m.sessionID)
}

func (m *managedSession) pendingUserInputsSnapshot() []pendingUserInputView {
	m.pendingInputsMu.Lock()
	defer m.pendingInputsMu.Unlock()

	items := make([]pendingUserInputView, 0, len(m.pendingInputs))
	for _, pending := range m.pendingInputs {
		items = append(items, pending.view)
	}
	return items
}

func (m *managedSession) subscribe() chan sseMessage {
	ch := make(chan sseMessage, sseSubscriberBufferSize)
	m.subscribersMu.Lock()
	m.subscribers[ch] = struct{}{}
	m.subscribersMu.Unlock()
	return ch
}

func (m *managedSession) unsubscribe(ch chan sseMessage) {
	m.subscribersMu.Lock()
	if _, ok := m.subscribers[ch]; ok {
		delete(m.subscribers, ch)
		close(ch)
	}
	m.subscribersMu.Unlock()
}

func (m *managedSession) broadcastHostEvent(eventName string, payload any) {
	m.broadcast(sseMessage{Event: eventName, Data: mustJSON(hostEvent{Timestamp: time.Now().UTC(), Data: payload})})
}

func (m *managedSession) broadcast(msg sseMessage) {
	m.subscribersMu.RLock()
	defer m.subscribersMu.RUnlock()
	for ch := range m.subscribers {
		select {
		case ch <- msg:
		default:
			logWarnf("SSE subscriber channel full, dropped event: %s (session %s)", msg.Event, m.id())
		}
	}
}

func (m *managedSession) summary() sessionSummary {
	sessionID := m.id()
	workspacePath := ""
	capabilities := copilot.SessionCapabilities{}
	if m.ps != nil {
		workspacePath = m.ps.WorkspacePath()
		capabilities = m.ps.Capabilities()
	} else if m.session != nil {
		workspacePath = m.session.WorkspacePath()
		capabilities = m.session.Capabilities()
	}

	return sessionSummary{
		SessionID:         sessionID,
		Provider:          m.provider,
		Model:             m.model,
		AgentMode:         m.agentMode,
		WorkingDirectory:  m.workingDirectory,
		WorkspacePath:     workspacePath,
		PermissionMode:    m.permissionMode,
		ExcludedTools:     m.excludedTools,
		Capabilities:      capabilities,
		PendingUserInputs: m.pendingUserInputsSnapshot(),
		Summary:           m.sessionName,
		Live:              m.hasSubscribers(),
		CreatedAt:         m.createdAt,
		Resumed:           m.resumed,
		Streaming:         m.streaming,
		Agent:             m.agent,
		ConfigDiscovery:   m.configDiscovery,
		ClientName:        m.clientName,
		InstructionCount:  m.instructionCount,
		AgentCount:        m.agentCount,
		SkillCount:        m.skillCount,
		MCPCount:          m.mcpCount,
	}
}

func (m *managedSession) hasSubscribers() bool {
	m.subscribersMu.RLock()
	defer m.subscribersMu.RUnlock()
	return len(m.subscribers) > 0
}

func (m *managedSession) close(reason error) {
	if m.eventUnsubscribe != nil {
		m.eventUnsubscribe()
		m.eventUnsubscribe = nil
	}

	m.pendingInputsMu.Lock()
	for id, pending := range m.pendingInputs {
		delete(m.pendingInputs, id)
		pending.resultCh <- userInputResult{Err: reason}
	}
	m.pendingInputsMu.Unlock()

	m.pendingPermissionsMu.Lock()
	for id, pending := range m.pendingPermissions {
		delete(m.pendingPermissions, id)
		pending.resultCh <- permissionResult{Err: reason}
	}
	m.pendingPermissionsMu.Unlock()

	m.abortClaudeTurn()

	if m.ps != nil {
		if err := m.ps.Disconnect(); err != nil {
			logWarnf("disconnect session %s: %v", m.id(), err)
		}
	} else if m.session != nil {
		if err := m.session.Disconnect(); err != nil {
			logWarnf("disconnect session %s: %v", m.id(), err)
		}
	}

	m.subscribersMu.Lock()
	for ch := range m.subscribers {
		close(ch)
		delete(m.subscribers, ch)
	}
	m.subscribersMu.Unlock()
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func backgroundTaskStatusRank(status string) int {
	switch status {
	case "running":
		return 0
	case "inbox":
		return 1
	case "idle":
		return 2
	case "failed":
		return 3
	case "completed":
		return 4
	default:
		return 5
	}
}

func extractBackgroundTasks(events []copilot.SessionEvent) []backgroundTaskView {
	tasks := make(map[string]*backgroundTaskView)

	ensureTask := func(id, kind string) *backgroundTaskView {
		task, ok := tasks[id]
		if !ok {
			task = &backgroundTaskView{ID: id, Kind: kind}
			tasks[id] = task
		}
		if task.Kind == "" {
			task.Kind = kind
		}
		return task
	}

	for idx, event := range events {
		switch data := event.Data.(type) {
		case *copilot.SubagentStartedData:
			task := ensureTask("subagent:"+data.ToolCallID, "subagent")
			task.Status = "running"
			task.Title = firstNonEmpty(data.AgentDisplayName, data.AgentName, task.Title, "Subagent")
			task.Description = firstNonEmpty(data.AgentDescription, task.Description)
			task.AgentName = firstNonEmpty(data.AgentName, task.AgentName)
			task.ToolCallID = firstNonEmpty(data.ToolCallID, task.ToolCallID)
			task.UpdatedAt = event.Timestamp
			if task.StartedAt == nil {
				startedAt := event.Timestamp
				task.StartedAt = &startedAt
			}
		case *copilot.SubagentCompletedData:
			task := ensureTask("subagent:"+data.ToolCallID, "subagent")
			task.Status = "completed"
			task.Title = firstNonEmpty(data.AgentDisplayName, data.AgentName, task.Title, "Subagent")
			task.AgentName = firstNonEmpty(data.AgentName, task.AgentName)
			task.ToolCallID = firstNonEmpty(data.ToolCallID, task.ToolCallID)
			task.Model = firstNonEmpty(stringValue(data.Model), task.Model)
			task.DurationMs = data.DurationMs
			task.TotalTokens = data.TotalTokens
			task.TotalToolCalls = data.TotalToolCalls
			task.UpdatedAt = event.Timestamp
			completedAt := event.Timestamp
			task.CompletedAt = &completedAt
			if task.StartedAt == nil {
				startedAt := event.Timestamp
				task.StartedAt = &startedAt
			}
		case *copilot.SubagentFailedData:
			task := ensureTask("subagent:"+data.ToolCallID, "subagent")
			task.Status = "failed"
			task.Title = firstNonEmpty(data.AgentDisplayName, data.AgentName, task.Title, "Subagent")
			task.AgentName = firstNonEmpty(data.AgentName, task.AgentName)
			task.ToolCallID = firstNonEmpty(data.ToolCallID, task.ToolCallID)
			task.Model = firstNonEmpty(stringValue(data.Model), task.Model)
			task.DurationMs = data.DurationMs
			task.TotalTokens = data.TotalTokens
			task.TotalToolCalls = data.TotalToolCalls
			task.Error = firstNonEmpty(data.Error, task.Error)
			task.UpdatedAt = event.Timestamp
			completedAt := event.Timestamp
			task.CompletedAt = &completedAt
			if task.StartedAt == nil {
				startedAt := event.Timestamp
				task.StartedAt = &startedAt
			}
		case *copilot.SystemNotificationData:
			kind := data.Kind
			switch kind.Type {
			case copilot.SystemNotificationTypeAgentCompleted, copilot.SystemNotificationTypeAgentIdle, copilot.SystemNotificationTypeNewInboxMessage:
				key := firstNonEmpty(stringValue(kind.AgentID), stringValue(kind.EntryID), fmt.Sprintf("notification-%d", idx))
				task := ensureTask("background:"+key, "background")
				task.Title = firstNonEmpty(stringValue(kind.Description), task.Title, stringValue(kind.Summary), stringValue(kind.AgentType), "Background agent")
				task.Description = firstNonEmpty(stringValue(kind.Description), task.Description)
				task.Summary = firstNonEmpty(stringValue(kind.Summary), task.Summary)
				task.Prompt = firstNonEmpty(stringValue(kind.Prompt), task.Prompt)
				task.AgentID = firstNonEmpty(stringValue(kind.AgentID), task.AgentID)
				task.AgentType = firstNonEmpty(stringValue(kind.AgentType), task.AgentType)
				task.EntryID = firstNonEmpty(stringValue(kind.EntryID), task.EntryID)
				task.UpdatedAt = event.Timestamp
				if task.StartedAt == nil {
					startedAt := event.Timestamp
					task.StartedAt = &startedAt
				}

				switch kind.Type {
				case copilot.SystemNotificationTypeAgentCompleted:
					if kind.Status != nil && *kind.Status == copilot.SystemNotificationAgentCompletedStatusFailed {
						task.Status = "failed"
					} else {
						task.Status = "completed"
					}
					completedAt := event.Timestamp
					task.CompletedAt = &completedAt
				case copilot.SystemNotificationTypeAgentIdle:
					task.Status = "idle"
				case copilot.SystemNotificationTypeNewInboxMessage:
					task.Status = "inbox"
				}
			}
		}
	}

	out := make([]backgroundTaskView, 0, len(tasks))
	for _, task := range tasks {
		out = append(out, *task)
	}
	sort.Slice(out, func(i, j int) bool {
		leftRank := backgroundTaskStatusRank(out[i].Status)
		rightRank := backgroundTaskStatusRank(out[j].Status)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}
func (m *managedSession) handlePermissionRequest(req copilot.PermissionRequest, inv copilot.PermissionInvocation) (copilot.PermissionRequestResult, error) {
	switch m.permissionMode {
	case permissionModeRejectAll:
		m.broadcastHostEvent("host.permission_decision", map[string]any{
			"sessionId": inv.SessionID, "request": req, "decision": "rejected", "mode": m.permissionMode,
		})
		return copilot.PermissionRequestResult{Kind: copilot.PermissionRequestResultKindRejected}, nil

	case permissionModeApproveReads:
		// Auto-approve read requests whose path is within the working directory.
		// Everything else (write, shell, url, mcp, hook, …) falls through to the
		// interactive handler so the user is still prompted.
		if req.Kind == copilot.PermissionRequestKindRead {
			path := firstNonEmpty(stringOrEmpty(req.Path), stringOrEmpty(req.FileName))
			if path != "" && m.workingDirectory != "" {
				wd := m.workingDirectory
				if !strings.HasSuffix(wd, "/") {
					wd += "/"
				}
				absPath := path
				if !strings.HasPrefix(absPath, "/") {
					absPath = wd + absPath
				}
				if strings.HasPrefix(absPath, wd) {
					m.broadcastHostEvent("host.permission_decision", map[string]any{
						"sessionId": inv.SessionID, "request": req, "decision": "approved", "mode": m.permissionMode,
					})
					return copilot.PermissionRequestResult{Kind: copilot.PermissionRequestResultKindApproved}, nil
				}
			}
		}
		// Fall through to interactive for non-read or out-of-workspace paths.
		fallthrough

	case permissionModeInteractive:
		pending := &pendingPermission{
			view: pendingPermissionView{
				ID:        fmt.Sprintf("%s-%d", permissionRequestIDPrefix, time.Now().UnixNano()),
				Request:   req,
				CreatedAt: time.Now().UTC(),
			},
			resultCh: make(chan permissionResult, asyncResultChannelSize),
		}
		m.pendingPermissionsMu.Lock()
		m.pendingPermissions[pending.view.ID] = pending
		m.pendingPermissionsMu.Unlock()

		m.broadcastHostEvent("host.permission_requested", map[string]any{
			"sessionId": inv.SessionID, "request": pending.view, "mode": m.permissionMode,
		})

		defer func() {
			m.pendingPermissionsMu.Lock()
			delete(m.pendingPermissions, pending.view.ID)
			m.pendingPermissionsMu.Unlock()
		}()

		select {
		case result := <-pending.resultCh:
			if result.Err != nil {
				logWarnf("permission response error session=%s request_id=%s: %v", inv.SessionID, pending.view.ID, result.Err)
				return copilot.PermissionRequestResult{Kind: copilot.PermissionRequestResultKindRejected}, result.Err
			}
			kind := copilot.PermissionRequestResultKindApproved
			decision := "approved"
			if !result.Approved {
				kind = copilot.PermissionRequestResultKindRejected
				decision = "rejected"
			}
			m.broadcastHostEvent("host.permission_decision", map[string]any{
				"sessionId": inv.SessionID, "requestId": pending.view.ID, "decision": decision, "mode": m.permissionMode,
			})
			return copilot.PermissionRequestResult{Kind: kind}, nil
		case <-time.After(m.inputResponseGrace):
			logWarnf("timed out waiting for permission response session=%s request_id=%s", inv.SessionID, pending.view.ID)
			return copilot.PermissionRequestResult{Kind: copilot.PermissionRequestResultKindRejected},
				fmt.Errorf("timed out waiting for permission response %s", pending.view.ID)
		}

	default: // approve-all and autopilot
		m.broadcastHostEvent("host.permission_decision", map[string]any{
			"sessionId": inv.SessionID, "request": req, "decision": "approved", "mode": m.permissionMode,
		})
		return copilot.PermissionRequestResult{Kind: copilot.PermissionRequestResultKindApproved}, nil
	}
}

func (m *managedSession) handleUserInputRequest(req copilot.UserInputRequest, inv copilot.UserInputInvocation) (copilot.UserInputResponse, error) {
	// In autopilot mode, auto-answer with the first choice or empty string.
	if m.permissionMode == permissionModeAutopilot {
		answer := ""
		if len(req.Choices) > 0 {
			answer = req.Choices[0]
		}
		m.broadcastHostEvent("host.user_input_resolved", map[string]any{
			"sessionId": inv.SessionID, "auto": true, "answer": answer,
		})
		return copilot.UserInputResponse{Answer: answer, WasFreeform: false}, nil
	}

	pending := &pendingUserInput{
		view: pendingUserInputView{
			ID:            fmt.Sprintf("%s-%d", userInputRequestIDPrefix, time.Now().UnixNano()),
			Question:      req.Question,
			Choices:       req.Choices,
			AllowFreeform: boolOrDefault(req.AllowFreeform, true),
			CreatedAt:     time.Now().UTC(),
		},
		resultCh: make(chan userInputResult, asyncResultChannelSize),
	}

	m.pendingInputsMu.Lock()
	m.pendingInputs[pending.view.ID] = pending
	m.pendingInputsMu.Unlock()

	m.broadcastHostEvent("host.user_input_requested", map[string]any{
		"sessionId": inv.SessionID,
		"request":   pending.view,
	})

	defer func() {
		m.pendingInputsMu.Lock()
		delete(m.pendingInputs, pending.view.ID)
		m.pendingInputsMu.Unlock()
	}()

	select {
	case result := <-pending.resultCh:
		if result.Err != nil {
			logWarnf("user input response error session=%s request_id=%s: %v", inv.SessionID, pending.view.ID, result.Err)
			return copilot.UserInputResponse{}, result.Err
		}
		m.broadcastHostEvent("host.user_input_resolved", map[string]any{
			"sessionId": inv.SessionID,
			"requestId": pending.view.ID,
		})
		return result.Response, nil
	case <-time.After(m.inputResponseGrace):
		logWarnf("timed out waiting for user input session=%s request_id=%s", inv.SessionID, pending.view.ID)
		return copilot.UserInputResponse{}, fmt.Errorf("timed out waiting for user input %s", pending.view.ID)
	}
}

func (m *managedSession) answerUserInput(requestID string, req answerUserInputRequest) error {
	m.pendingInputsMu.Lock()
	pending, ok := m.pendingInputs[requestID]
	if ok {
		delete(m.pendingInputs, requestID)
	}
	m.pendingInputsMu.Unlock()

	if !ok {
		return fmt.Errorf("pending user input %s was not found", requestID)
	}

	pending.resultCh <- userInputResult{Response: copilot.UserInputResponse{Answer: req.Answer, WasFreeform: req.WasFreeform}}
	return nil
}

func (m *managedSession) answerPermission(requestID string, approved bool) error {
	m.pendingPermissionsMu.Lock()
	pending, ok := m.pendingPermissions[requestID]
	if ok {
		delete(m.pendingPermissions, requestID)
	}
	m.pendingPermissionsMu.Unlock()

	if !ok {
		return fmt.Errorf("pending permission %s was not found", requestID)
	}

	pending.resultCh <- permissionResult{Approved: approved}
	return nil
}
