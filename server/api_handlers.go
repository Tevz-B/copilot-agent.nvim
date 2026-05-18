package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

func (s *service) handleCreateClaudeSession(w http.ResponseWriter, req createSessionRequest, managed *managedSession, provider providerRuntime) {
	if managed == nil {
		writeError(w, http.StatusInternalServerError, "failed to initialize claude session")
		return
	}
	if strings.TrimSpace(managed.model) == "" {
		managed.model = firstNonEmpty(strings.TrimSpace(req.Model), strings.TrimSpace(provider.Model), strings.TrimSpace(s.defaultModel), claudeDefaultModel)
	}
	if req.SystemMessage != nil {
		managed.claudeSystemPrompt = strings.TrimSpace(req.SystemMessage.Content)
	}
	managed.claudeAPIKeyEnv = provider.ClaudeAPIKeyEnv
	managed.claudeAuthTokenEnv = provider.ClaudeAuthTokenEnv
	managed.claudeBaseURL = provider.ClaudeBaseURL
	managed.ps = &claudeSession{managed: managed, svc: s}
	if managed.sessionName == "" {
		managed.sessionName = managed.id()
	}

	s.storeManagedSession(managed)
	action := "created"
	if req.Resume {
		action = "resumed"
	}
	logInfof("claude session %s %s provider=%s wd=%s model=%s mode=%s streaming=%t", managed.id(), action, provider.Name, managed.workingDirectory, managed.model, managed.permissionMode, managed.streaming)
	writeJSON(w, http.StatusCreated, managed.summary())
}

func (s *service) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	providerRuntime, err := s.requestedProvider(r, req.Provider)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var workingDirectory string
	if req.SessionID == "" {
		resolvedWorkingDirectory, resolveErr := s.resolveSessionWorkingDirectory(req.WorkingDirectory)
		if resolveErr != nil {
			writeError(w, http.StatusBadRequest, resolveErr.Error())
			return
		}
		workingDirectory = resolvedWorkingDirectory
		req.SessionID = newSessionID(workingDirectory)
	}

	if req.PermissionMode == "" {
		req.PermissionMode = permissionModeApproveAll
	}
	if !isValidPermissionMode(req.PermissionMode) {
		writeError(w, http.StatusBadRequest, "permissionMode must be one of: interactive, approve-all, approve-reads, autopilot, reject-all")
		return
	}

	clientName := strings.TrimSpace(req.ClientName)
	if clientName == "" {
		clientName = defaultClientName
	}
	if clientID := strings.TrimSpace(req.ClientID); clientID != "" {
		s.registerClient(clientID, clientName)
	}

	if existing, ok, ambiguous := s.getManagedSession(req.SessionID, providerRuntime.Name); ok {
		if req.PermissionMode != "" && req.PermissionMode != existing.permissionMode {
			existing.permissionMode = req.PermissionMode
			existing.broadcastHostEvent("host.permission_mode_changed", map[string]any{
				"sessionId": req.SessionID,
				"provider":  existing.provider,
				"mode":      req.PermissionMode,
			})
		}
		if requestedModel := strings.TrimSpace(req.Model); requestedModel != "" && requestedModel != existing.model {
			resolvedModel, resolveErr := s.resolveRequestedModel(r.Context(), providerRuntime, requestedModel)
			if resolveErr == nil && resolvedModel != existing.model {
				if existing.ps != nil {
					if setErr := existing.ps.SetModel(r.Context(), resolvedModel, ""); setErr == nil {
						existing.model = resolvedModel
						existing.broadcastHostEvent("host.model_changed", map[string]any{
							"sessionId": req.SessionID,
							"provider":  existing.provider,
							"model":     resolvedModel,
						})
					} else {
						logErrorf("session %s: failed to apply requested model %q on re-attach: %v", req.SessionID, resolvedModel, setErr)
					}
				}
			}
		}
		writeJSON(w, http.StatusOK, existing.summary())
		return
	} else if ambiguous {
		writeError(w, http.StatusConflict, fmt.Sprintf("session %q exists for multiple providers; specify provider", req.SessionID))
		return
	}

	if workingDirectory == "" {
		var resolveErr error
		workingDirectory, resolveErr = s.resolveSessionWorkingDirectory(req.WorkingDirectory)
		if resolveErr != nil {
			writeError(w, http.StatusBadRequest, resolveErr.Error())
			return
		}
	}

	streaming := boolOrDefault(req.Streaming, true)
	configDiscovery := boolOrDefault(req.EnableConfigDiscovery, true)

	model, err := s.resolveRequestedModel(r.Context(), providerRuntime, firstNonEmpty(req.Model, providerRuntime.Model, s.defaultModel))
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("resolve model: %v", err))
		return
	}

	managed := &managedSession{
		sessionID:           req.SessionID,
		provider:            providerRuntime.Name,
		model:               model,
		workingDirectory:    workingDirectory,
		permissionMode:      req.PermissionMode,
		excludedTools:       req.ExcludedTools,
		createdAt:           time.Now().UTC(),
		resumed:             req.Resume,
		streaming:           streaming,
		agent:               req.Agent,
		configDiscovery:     configDiscovery,
		clientName:          clientName,
		subscribers:         make(map[chan sseMessage]struct{}),
		pendingInputs:       make(map[string]*pendingUserInput),
		pendingPermissions:  make(map[string]*pendingPermission),
		messageChunkIndexes: make(map[string]uint64),
		inputResponseGrace:  defaultInputTimeout,
	}
	if configDiscovery {
		managed.instructionCount, managed.agentCount, managed.skillCount, managed.mcpCount = countDiscoverableConfig(workingDirectory)
	}

	if req.Resume && isCopilotProviderType(providerRuntime.Type) {
		if meta, metaErr := withCopilotClientRetry(s, "get session metadata", func(client copilotClient) (*copilot.SessionMetadata, error) {
			return client.GetSessionMetadata(r.Context(), req.SessionID)
		}); metaErr == nil && meta != nil && meta.Summary != nil {
			managed.sessionName = *meta.Summary
		}
	}

	if isClaudeProviderType(providerRuntime.Type) {
		s.handleCreateClaudeSession(w, req, managed, providerRuntime)
		return
	}
	if !isCopilotProviderType(providerRuntime.Type) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("provider %q type %q is not supported", providerRuntime.Name, providerRuntime.Type))
		return
	}

	var session *copilot.Session
	if req.Resume {
		session, err = withCopilotClientRetry(s, "resume session", func(client copilotClient) (*copilot.Session, error) {
			return client.ResumeSession(r.Context(), req.SessionID, &copilot.ResumeSessionConfig{
				ClientName:                     clientName,
				Model:                          managed.model,
				ReasoningEffort:                req.ReasoningEffort,
				SystemMessage:                  req.SystemMessage,
				AvailableTools:                 req.AvailableTools,
				ExcludedTools:                  req.ExcludedTools,
				OnPermissionRequest:            managed.handlePermissionRequest,
				OnUserInputRequest:             managed.handleUserInputRequest,
				WorkingDirectory:               workingDirectory,
				EnableConfigDiscovery:          configDiscovery,
				Streaming:                      streaming,
				IncludeSubAgentStreamingEvents: req.IncludeSubAgentStreamingEvents,
				CustomAgents:                   req.CustomAgents,
				Agent:                          req.Agent,
				SkillDirectories:               req.SkillDirectories,
				DisabledSkills:                 req.DisabledSkills,
			})
		})
		if err != nil {
			logErrorf("resume session session_id=%s provider=%s wd=%s: %v", req.SessionID, providerRuntime.Name, workingDirectory, err)
			writeError(w, http.StatusBadGateway, fmt.Sprintf("resume session: %v", err))
			return
		}
		managed.session = session
		managed.ps = &copilotSession{session: session}
		managed.eventUnsubscribe = session.On(managed.handleSessionEvent)
	} else {
		session, err = withCopilotClientRetry(s, "create session", func(client copilotClient) (*copilot.Session, error) {
			return client.CreateSession(r.Context(), &copilot.SessionConfig{
				SessionID:                      req.SessionID,
				ClientName:                     clientName,
				Model:                          managed.model,
				ReasoningEffort:                req.ReasoningEffort,
				SystemMessage:                  req.SystemMessage,
				AvailableTools:                 req.AvailableTools,
				ExcludedTools:                  req.ExcludedTools,
				OnPermissionRequest:            managed.handlePermissionRequest,
				OnUserInputRequest:             managed.handleUserInputRequest,
				WorkingDirectory:               workingDirectory,
				Streaming:                      streaming,
				IncludeSubAgentStreamingEvents: req.IncludeSubAgentStreamingEvents,
				EnableConfigDiscovery:          configDiscovery,
				CustomAgents:                   req.CustomAgents,
				Agent:                          req.Agent,
				SkillDirectories:               req.SkillDirectories,
				DisabledSkills:                 req.DisabledSkills,
				OnEvent:                        managed.handleSessionEvent,
			})
		})
		if err != nil {
			logErrorf("create session session_id=%s provider=%s wd=%s: %v", req.SessionID, providerRuntime.Name, workingDirectory, err)
			writeError(w, http.StatusBadGateway, fmt.Sprintf("create session: %v", err))
			return
		}
		managed.session = session
		managed.ps = &copilotSession{session: session}
	}

	s.storeManagedSession(managed)
	action := "created"
	if req.Resume {
		action = "resumed"
	}
	logInfof("session %s %s provider=%s wd=%s model=%s mode=%s streaming=%t", req.SessionID, action, providerRuntime.Name, workingDirectory, managed.model, req.PermissionMode, streaming)
	writeJSON(w, http.StatusCreated, managed.summary())
}

func (s *service) runClaudeQuery(managed *managedSession, ctx context.Context, cancel context.CancelFunc, prompt string, attachments []copilot.Attachment, requestHeaders map[string]string, messageID string) {
	defer cancel()
	defer managed.clearClaudeTurnCancel()

	userMessage, err := buildClaudeUserMessage(prompt, attachments, managed.workingDirectory)
	if err != nil {
		managed.broadcastClaudeEvent("assistant.error", map[string]any{
			"messageId": messageID,
			"error":     err.Error(),
		}, messageID)
		managed.broadcastClaudeEvent("assistant.turn_end", map[string]any{"messageId": messageID, "error": err.Error()}, messageID)
		return
	}

	messages := managed.claudeMessageHistorySnapshot()
	messages = append(messages, userMessage)
	model := firstNonEmpty(strings.TrimSpace(managed.model), claudeDefaultModel)
	params := anthropic.MessageNewParams{
		MaxTokens: claudeDefaultMaxTokens,
		Messages:  messages,
		Model:     anthropic.Model(model),
	}
	if systemPrompt := strings.TrimSpace(managed.claudeSystemPrompt); systemPrompt != "" {
		params.System = []anthropic.TextBlockParam{{Text: systemPrompt}}
	}

	requestOptions := anthropicRequestOptionsFromHeaders(requestHeaders)
	clientOptions, optionsErr := anthropicClientOptionsFromManaged(managed)
	if optionsErr != nil {
		managed.broadcastClaudeEvent("assistant.error", map[string]any{
			"messageId": messageID,
			"error":     optionsErr.Error(),
		}, messageID)
		managed.broadcastClaudeEvent("assistant.turn_end", map[string]any{"messageId": messageID, "error": optionsErr.Error()}, messageID)
		return
	}
	client := anthropic.NewClient(clientOptions...)
	stream := client.Messages.NewStreaming(ctx, params, requestOptions...)
	if stream == nil {
		err := errors.New("failed to initialize anthropic stream")
		managed.broadcastClaudeEvent("assistant.error", map[string]any{
			"messageId": messageID,
			"error":     err.Error(),
		}, messageID)
		managed.broadcastClaudeEvent("assistant.turn_end", map[string]any{"messageId": messageID, "error": err.Error()}, messageID)
		return
	}
	defer stream.Close()

	var response anthropic.Message
	emittedDelta := false
	for stream.Next() {
		event := stream.Current()
		if err := response.Accumulate(event); err != nil {
			logWarnf("accumulate anthropic stream event failed session=%s: %v", managed.id(), err)
		}
		if event.Type != "content_block_delta" {
			continue
		}
		delta := event.AsContentBlockDelta().Delta.AsAny()
		textDelta, ok := delta.(anthropic.TextDelta)
		if !ok || textDelta.Text == "" {
			continue
		}
		emittedDelta = true
		managed.broadcastClaudeEvent("assistant.message_delta", map[string]any{
			"messageId": messageID,
			"delta":     textDelta.Text,
		}, messageID)
	}

	if err := stream.Err(); err != nil {
		if errors.Is(err, context.Canceled) {
			managed.broadcastClaudeEvent("assistant.turn_end", map[string]any{"messageId": messageID, "aborted": true}, messageID)
			return
		}
		managed.broadcastClaudeEvent("assistant.error", map[string]any{
			"messageId": messageID,
			"error":     err.Error(),
		}, messageID)
		managed.broadcastClaudeEvent("assistant.turn_end", map[string]any{"messageId": messageID, "error": err.Error()}, messageID)
		return
	}

	finalText := extractAnthropicTextBlocks(response.Content)
	if !emittedDelta && strings.TrimSpace(finalText) != "" {
		managed.broadcastClaudeEvent("assistant.message_delta", map[string]any{
			"messageId": messageID,
			"delta":     finalText,
		}, messageID)
	}

	managed.appendClaudeMessages(userMessage)
	if len(response.Content) > 0 {
		managed.appendClaudeMessages(response.ToParam())
	}

	payload := map[string]any{"messageId": messageID}
	if response.StopReason != "" {
		payload["stopReason"] = string(response.StopReason)
	}
	if stopSequence := strings.TrimSpace(response.StopSequence); stopSequence != "" {
		payload["stopSequence"] = stopSequence
	}
	managed.broadcastClaudeEvent("assistant.turn_end", payload, messageID)
}

func (s *service) handleGetSession(w http.ResponseWriter, r *http.Request) {
	managed, ok := s.requireManagedSession(w, r)
	if !ok {
		return
	}
	s.refreshManagedSessionSummary(r.Context(), managed)
	writeJSON(w, http.StatusOK, managed.summary())
}

func (s *service) handleGetContext(w http.ResponseWriter, r *http.Request) {
	managed, ok := s.requireManagedSession(w, r)
	if !ok {
		return
	}

	usage := managed.getContextUsage()
	if usage == nil {
		writeJSON(w, http.StatusOK, contextWindowResponse{
			SessionID: managed.id(),
			Provider:  managed.provider,
			Available: false,
		})
		return
	}

	breakdown := s.buildContextWindowSnapshot(r.Context(), managed, usage)
	writeJSON(w, http.StatusOK, contextWindowResponse{
		SessionID:     managed.id(),
		Provider:      managed.provider,
		Available:     true,
		ContextWindow: breakdown,
	})
}

func (s *service) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	deleteState := queryBool(r, "delete")
	providerName := strings.TrimSpace(r.URL.Query().Get("provider"))
	if providerName != "" {
		if _, exists := s.providerRuntime(providerName); !exists {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown provider %q", providerName))
			return
		}
	}

	managed, ok, ambiguous := s.removeManagedSession(id, providerName)
	if ambiguous {
		writeError(w, http.StatusConflict, fmt.Sprintf("session %q exists for multiple providers; specify ?provider=", id))
		return
	}
	if !ok {
		if deleteState {
			providerRuntime, providerErr := s.requestedProvider(r, "")
			if providerErr != nil {
				writeError(w, http.StatusBadRequest, providerErr.Error())
				return
			}
			if !isCopilotProviderType(providerRuntime.Type) {
				writeError(w, http.StatusNotFound, "session is not attached to this service")
				return
			}
			if err := s.ensureClientConnected(); err != nil {
				writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("copilot client unavailable: %v", err))
				return
			}
			if err := s.currentClient().DeleteSession(r.Context(), id); err != nil {
				writeError(w, http.StatusNotFound, fmt.Sprintf("delete session: %v", err))
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"sessionId": id, "provider": providerRuntime.Name, "deleted": true})
			return
		}
		writeError(w, http.StatusNotFound, "session is not attached to this service")
		return
	}

	managed.broadcastHostEvent("host.session_disconnected", map[string]any{"sessionId": id, "provider": managed.provider, "deleteState": deleteState})
	managed.close(errors.New("session closed by host"))

	if deleteState && s.isCopilotProviderName(managed.provider) {
		if err := s.ensureClientConnected(); err != nil {
			writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("copilot client unavailable: %v", err))
			return
		}
		if err := s.currentClient().DeleteSession(r.Context(), id); err != nil {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("delete session state: %v", err))
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"sessionId": id, "provider": managed.provider, "deleted": deleteState})
}

func (s *service) handleSetModel(w http.ResponseWriter, r *http.Request) {
	managed, ok := s.requireManagedSession(w, r)
	if !ok {
		return
	}

	var req setModelRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}

	model := strings.TrimSpace(req.Model)
	providerRuntime, ok := s.providerRuntime(managed.provider)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown provider %q", managed.provider))
		return
	}
	if isCopilotProviderType(providerRuntime.Type) {
		resolvedModel, resolveErr := s.resolveRequestedModel(r.Context(), providerRuntime, model)
		if resolveErr != nil {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("resolve model: %v", resolveErr))
			return
		}
		model = resolvedModel
	}
	if err := managed.ps.SetModel(r.Context(), model, strings.TrimSpace(req.ReasoningEffort)); err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("set model: %v", err))
		return
	}

	managed.model = model
	evt := map[string]any{
		"sessionId": managed.id(),
		"provider":  managed.provider,
		"model":     model,
	}
	if req.ReasoningEffort != "" {
		evt["reasoningEffort"] = req.ReasoningEffort
	}
	managed.broadcastHostEvent("host.model_changed", evt)
	writeJSON(w, http.StatusOK, managed.summary())
}

func (s *service) handleGetMessages(w http.ResponseWriter, r *http.Request) {
	managed, ok := s.requireManagedSession(w, r)
	if !ok {
		return
	}

	events, err := managed.ps.GetMessages(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("get messages: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": managed.provider, "events": events})
}

func (s *service) handleGetTasks(w http.ResponseWriter, r *http.Request) {
	managed, ok := s.requireManagedSession(w, r)
	if !ok {
		return
	}

	tasks, err := managed.ps.GetTasks(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("get tasks: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": managed.provider, "tasks": tasks})
}

func (s *service) handleAbortSession(w http.ResponseWriter, r *http.Request) {
	managed, ok := s.requireManagedSession(w, r)
	if !ok {
		return
	}
	aborted, err := managed.ps.Abort(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("abort: %v", err))
		return
	}
	if aborted {
		managed.broadcastHostEvent("host.turn_aborted", map[string]any{"sessionId": managed.id(), "provider": managed.provider})
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "aborted": aborted})
}

func (s *service) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	managed, ok := s.requireManagedSession(w, r)
	if !ok {
		return
	}

	var req sendMessageRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	if clientID := strings.TrimSpace(req.ClientID); clientID != "" {
		s.registerClient(clientID, managed.clientName)
	}

	messageID, err := managed.ps.SendMessage(r.Context(), sendMessageOpts{
		Prompt:         req.Prompt,
		Attachments:    req.Attachments,
		RequestHeaders: req.RequestHeaders,
	})
	if err != nil {
		logErrorf("send message session=%s: %v", managed.id(), err)
		writeError(w, http.StatusBadGateway, fmt.Sprintf("send message: %v", err))
		return
	}

	logInfof("send message session=%s provider=%s prompt_chars=%d attachments=%d message_id=%s", managed.id(), managed.provider, len(req.Prompt), len(req.Attachments), messageID)
	writeJSON(w, http.StatusAccepted, map[string]any{"sessionId": managed.id(), "provider": managed.provider, "messageId": messageID})
}

func (s *service) handleCompactHistory(w http.ResponseWriter, r *http.Request) {
	managed, ok := s.requireManagedSession(w, r)
	if !ok {
		return
	}

	var req compactHistoryRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := managed.ps.CompactHistory(r.Context())
	if errors.Is(err, ErrNotSupported) {
		writeError(w, http.StatusNotImplemented, fmt.Sprintf("compact history is not yet supported with provider=%s", managed.provider))
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("compact history: %v", err))
		return
	}
	if result != nil && result.ContextWindow != nil {
		managed.setContextUsage(contextWindowUsageFromCompact(result.ContextWindow))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"sessionId": managed.id(),
		"provider":  managed.provider,
		"result":    result,
	})
}

func (s *service) handleStartFleet(w http.ResponseWriter, r *http.Request) {
	managed, ok := s.requireManagedSession(w, r)
	if !ok {
		return
	}

	var req fleetStartRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := managed.ps.FleetStart(r.Context(), strings.TrimSpace(req.Prompt))
	if errors.Is(err, ErrNotSupported) {
		writeError(w, http.StatusNotImplemented, fmt.Sprintf("fleet mode is not yet supported with provider=%s", managed.provider))
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("start fleet: %v", err))
		return
	}

	started := result != nil && result.Started
	if started {
		logInfof("fleet started session=%s prompt_chars=%d", managed.id(), len(strings.TrimSpace(req.Prompt)))
		managed.broadcastHostEvent("host.fleet_started", map[string]any{
			"sessionId": managed.id(),
			"provider":  managed.provider,
			"prompt":    strings.TrimSpace(req.Prompt),
			"started":   true,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"sessionId": managed.id(),
		"provider":  managed.provider,
		"started":   started,
	})
}

func (s *service) handleEvents(w http.ResponseWriter, r *http.Request) {
	managed, ok := s.requireManagedSession(w, r)
	if !ok {
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported by this server")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sub := managed.subscribe()
	defer managed.unsubscribe(sub)
	s.refreshManagedSessionSummary(r.Context(), managed)
	logInfof("SSE attached session=%s history=%t", managed.id(), queryBool(r, "history"))

	if err := writeSSE(w, "host.session_attached", mustJSON(hostEvent{Timestamp: time.Now().UTC(), Data: managed.summary()})); err != nil {
		logWarnf("SSE attach write failed session=%s: %v", managed.id(), err)
		return
	}
	flusher.Flush()

	if queryBool(r, "history") {
		if s.isClaudeProviderName(managed.provider) {
			history := managed.claudeEventHistorySnapshot()
			for _, payload := range history {
				if err := writeSSE(w, "session.event", payload); err != nil {
					logWarnf("SSE replay write failed session=%s: %v", managed.id(), err)
					return
				}
			}
			if err := writeSSE(w, "host.history_done", mustJSON(hostEvent{
				Timestamp: time.Now().UTC(),
				Data:      map[string]any{"sessionId": managed.id(), "provider": managed.provider, "count": len(history)},
			})); err != nil {
				logWarnf("SSE history done write failed session=%s: %v", managed.id(), err)
				return
			}
			flusher.Flush()
		} else {
			replayPermissionHistory := queryBool(r, "replay_permission_history")
			replayTurnLimit := historyReplayTurnLimit(r)
			replayActivityLimit := historyReplayActivityTurnLimit(r)
			replayPreviewChars := historyReplayPreviewChars(r)
			events, err := managed.session.GetMessages(r.Context())
			if err != nil {
				logErrorf("get history session=%s: %v", managed.id(), err)
				writeError(w, http.StatusBadGateway, fmt.Sprintf("get history: %v", err))
				return
			}
			payloads, replayedCount := managed.marshalReplaySessionEvents(events, replayPermissionHistory, replayTurnLimit, replayActivityLimit, replayPreviewChars)
			logInfof("SSE replay start session=%s events=%d replay_permission_history=%t turn_limit=%d activity_limit=%d preview_chars=%d", managed.id(), replayedCount, replayPermissionHistory, replayTurnLimit, replayActivityLimit, replayPreviewChars)
			for _, payload := range payloads {
				if err := writeSSE(w, "session.event", payload); err != nil {
					logWarnf("SSE replay write failed session=%s: %v", managed.id(), err)
					return
				}
			}
			// Signal that history replay is complete so clients can batch-render once.
			if err := writeSSE(w, "host.history_done", mustJSON(hostEvent{
				Timestamp: time.Now().UTC(),
				Data:      map[string]any{"sessionId": managed.id(), "provider": managed.provider, "count": replayedCount},
			})); err != nil {
				logWarnf("SSE history done write failed session=%s: %v", managed.id(), err)
				return
			}
			flusher.Flush()
			logInfof("SSE replay done session=%s events=%d", managed.id(), replayedCount)
		}
	}

	keepAlive := time.NewTicker(sseKeepAliveInterval)
	defer keepAlive.Stop()

	for {
		select {
		case <-r.Context().Done():
			logInfof("SSE client disconnected session=%s", managed.id())
			return
		case <-keepAlive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				logWarnf("SSE keepalive write failed session=%s: %v", managed.id(), err)
				return
			}
			flusher.Flush()
		case msg, ok := <-sub:
			if !ok {
				logInfof("SSE subscriber channel closed session=%s", managed.id())
				return
			}
			if err := writeSSE(w, msg.Event, msg.Data); err != nil {
				logWarnf("SSE write failed session=%s event=%s: %v", managed.id(), msg.Event, err)
				return
			}
			// Drain any additional buffered messages before flushing to
			// reduce channel back-pressure during event bursts.
		drain:
			for {
				select {
				case msg, ok = <-sub:
					if !ok {
						flusher.Flush()
						return
					}
					if err := writeSSE(w, msg.Event, msg.Data); err != nil {
						logWarnf("SSE write failed session=%s event=%s: %v", managed.id(), msg.Event, err)
						return
					}
				default:
					break drain
				}
			}
			flusher.Flush()
		}
	}
}

func (s *service) handleAnswerUserInput(w http.ResponseWriter, r *http.Request) {
	managed, ok := s.requireManagedSession(w, r)
	if !ok {
		return
	}

	var req answerUserInputRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := managed.answerUserInput(r.PathValue("requestID"), req); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func (s *service) handleAnswerPermission(w http.ResponseWriter, r *http.Request) {
	managed, ok := s.requireManagedSession(w, r)
	if !ok {
		return
	}

	var req struct {
		Approved bool `json:"approved"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := managed.answerPermission(r.PathValue("requestID"), req.Approved); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func (s *service) handleSetPermissionMode(w http.ResponseWriter, r *http.Request) {
	managed, ok := s.requireManagedSession(w, r)
	if !ok {
		return
	}

	var req struct {
		Mode string `json:"mode"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !isValidPermissionMode(req.Mode) {
		writeError(w, http.StatusBadRequest, "mode must be one of: interactive, approve-all, approve-reads, autopilot, reject-all")
		return
	}

	managed.permissionMode = req.Mode
	managed.broadcastHostEvent("host.permission_mode_changed", map[string]any{
		"sessionId": managed.id(),
		"provider":  managed.provider,
		"mode":      req.Mode,
	})

	writeJSON(w, http.StatusOK, map[string]any{"sessionId": managed.id(), "provider": managed.provider, "mode": req.Mode})
}

// handleSetAgentMode changes the agent mode (interactive / plan / autopilot) for a session
// by calling the SDK's session.mode.set RPC.
func (s *service) handleSetAgentMode(w http.ResponseWriter, r *http.Request) {
	managed, ok := s.requireManagedSession(w, r)
	if !ok {
		return
	}

	var req struct {
		Mode string `json:"mode"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	sdkMode, ok := toSDKAgentMode(req.Mode)
	if !ok {
		writeError(w, http.StatusBadRequest, "mode must be one of: ask, plan, agent (or: interactive, plan, autopilot)")
		return
	}

	if err := managed.ps.SetMode(r.Context(), sdkMode); err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("set agent mode: %v", err))
		return
	}

	managed.agentMode = req.Mode
	managed.broadcastHostEvent("host.agent_mode_changed", map[string]any{
		"sessionId": managed.id(),
		"provider":  managed.provider,
		"mode":      req.Mode,
	})

	writeJSON(w, http.StatusOK, map[string]any{"sessionId": managed.id(), "provider": managed.provider, "mode": req.Mode})
}

// toSDKAgentMode maps user-facing mode names to SDK SessionMode constants.
func toSDKAgentMode(mode string) (rpc.SessionMode, bool) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "ask", "interactive":
		return rpc.SessionModeInteractive, true
	case "plan":
		return rpc.SessionModePlan, true
	case "agent", "autopilot":
		return rpc.SessionModeAutopilot, true
	default:
		return "", false
	}
}

// handleSetTools updates the locally-tracked excluded-tools list for the session.
// Note: this does not affect the active session in the SDK (tools are configured at
// session-creation time); the updated list is reflected in subsequent GET /sessions/{id}
// responses and will be used when the session is next resumed.
func (s *service) handleSetTools(w http.ResponseWriter, r *http.Request) {
	managed, ok := s.requireManagedSession(w, r)
	if !ok {
		return
	}

	var req struct {
		ExcludedTools []string `json:"excludedTools"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	managed.excludedTools = req.ExcludedTools
	managed.broadcastHostEvent("host.tools_changed", map[string]any{
		"sessionId":     managed.id(),
		"provider":      managed.provider,
		"excludedTools": req.ExcludedTools,
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"sessionId":     managed.id(),
		"provider":      managed.provider,
		"excludedTools": req.ExcludedTools,
	})
}
