# Server Architecture

> **Last updated:** 2026-05-18
>
> This document describes the Go HTTP service under `server/`.
> It is intended for LLMs and contributors who need to understand
> the codebase structure before making changes.
>
> **Maintenance:** Update this file whenever files are added/removed,
> functions are moved between files, routes change, or the provider
> interface evolves. A CI check or periodic review is recommended.

---

## 1. High-Level Overview

The server is a single-package (`package main`) HTTP service that
bridges Neovim (via a Lua plugin) to one or more LLM providers.
It manages provider sessions, streams events over SSE, and handles
user interactions (permissions, input prompts) asynchronously.

```
┌────────────┐  HTTP/SSE   ┌──────────────┐  SDK / API  ┌──────────────┐
│  Neovim    │ ◄──────────► │  Go Service  │ ◄──────────► │  Copilot SDK │
│  Plugin    │   curl       │  (this code) │              │  / Anthropic │
└────────────┘              └──────────────┘              └──────────────┘
```

**Key traits:**

- All Go source lives in `server/` under `package main` (no sub-packages
  except `config/` and `serviceutil/`).
- Requires Go 1.24+; HTTP routes use the 1.22+ `"METHOD /path"` syntax.
- Multi-provider: supports Copilot (via SDK) and Claude (via Anthropic API).
- Provider abstraction via the `providerSession` interface eliminates
  most provider-specific branching in handlers.

---

## 2. File Map

| File | Lines | Responsibility |
|------|------:|----------------|
| `main.go` | ~1030 | Constants, request/response types, `service` struct, `copilotClient` interface, provider loading, `main()`, route registration, startup helpers |
| `api_handlers.go` | ~916 | All session HTTP handlers (create, delete, send message, model, mode, abort, compact, fleet, events SSE, permissions, user-input, tools) |
| `session_state.go` | ~970 | `managedSession` struct and methods: SSE broadcast, event sequencing, replay/history shaping, permission/input request handling |
| `session_service.go` | ~456 | Session map operations (get/store/remove), model alias resolution, working directory resolution, context window computation |
| `provider_session.go` | ~227 | `providerSession` interface definition + `copilotSession` and `claudeSession` implementations |
| `provider_copilot.go` | ~320 | Copilot SDK client lifecycle (start/stop/restart/retry), health/providers/models/sessions handlers, port-range listener |
| `provider_claude.go` | ~503 | Claude-specific: attachment processing, turn management, streaming, event/message history |
| `helpers.go` | ~367 | HTTP middleware (CORS, logging, recovery), JSON/SSE response helpers, query param helpers, config discovery, session ID generation, service lifecycle handlers |
| `lsp.go` | ~538 | Embedded LSP server for editor integration (code actions → send prompt to active session) |
| `config/config.go` | ~185 | Provider YAML config parsing, provider type normalization |
| `serviceutil/serviceutil.go` | ~221 | Control-socket/port-range/lease discovery for finding running service instances |

### Test Files

| File | Lines | Coverage |
|------|------:|----------|
| `main_test.go` | ~1214 | Client lifecycle, session creation, health handlers, event sequencing, replay |
| `session_state_test.go` | ~882 | Event metadata, replay window, truncation, subscriber management, broadcast |
| `session_service_test.go` | ~464 | Session map CRUD, model alias resolution, live summaries, disconnect |
| `provider_session_test.go` | ~181 | Interface compliance, nil-safety, Claude/Copilot implementation behavior |
| `helpers_test.go` | ~585 | All helper functions: middleware, JSON/SSE, query params, config discovery, session IDs |
| `lsp_test.go` | ~65 | URI-to-filename decoding, selection text extraction |

---

## 3. Core Types

### `service` (main.go)

The top-level singleton. Holds the Copilot SDK client, provider
registry, session map, client lease registry, and shutdown hooks.

```go
type service struct {
    client          copilotClient              // Active Copilot SDK client
    clientFactory   func() copilotClient       // Creates new clients on restart
    providers       map[string]providerRuntime // Loaded provider configs
    providerOrder   []string                   // Display-ordered provider names
    sessions        map[string]*managedSession // Active sessions (key: "provider::sessionID")
    activeClients   map[string]registeredClient// Registered Neovim clients (for idle shutdown)
    shutdownHTTP    func(context.Context) error
    stop            func()                     // Cancels service context
    // ... mutexes, timers, defaults
}
```

### `managedSession` (session_state.go)

Wraps a provider session with SSE fan-out, event sequencing,
and pending interaction tracking.

```go
type managedSession struct {
    ps               providerSession           // Provider-agnostic session interface
    session          *copilot.Session           // Deprecated: direct SDK ref (transition period)
    sessionID        string                     // Backend-assigned or generated ID
    provider         string                     // Provider name (e.g. "copilot", "claude")
    model            string                     // Current model ID
    subscribers      map[chan sseMessage]struct{}// SSE fan-out channels
    pendingInputs    map[string]*pendingUserInput
    pendingPermissions map[string]*pendingPermission
    nextEventSequence  uint64                   // Monotonic event counter
    messageChunkIndexes map[string]uint64       // Per-message chunk counter
    // ... Claude-specific fields (event history, messages, turn cancel)
}
```

### `providerSession` (provider_session.go)

The abstraction that lets handlers work without provider if/else:

```go
type providerSession interface {
    SessionID() string
    SendMessage(ctx, opts) (messageID string, err error)
    Abort(ctx) (wasActive bool, err error)
    SetModel(ctx, model, reasoningEffort string) error
    GetMessages(ctx) (any, error)
    GetTasks(ctx) ([]backgroundTaskView, error)
    CompactHistory(ctx) (*rpc.HistoryCompactResult, error)
    FleetStart(ctx, prompt) (*rpc.FleetStartResult, error)
    SetMode(ctx, mode) error
    WorkspacePath() string
    Capabilities() copilot.SessionCapabilities
    Disconnect() error
}
```

| Method | Copilot | Claude |
|--------|---------|--------|
| `SendMessage` | SDK `session.Send()` | `beginClaudeTurn()` + goroutine `runClaudeQuery()` |
| `Abort` | SDK `session.Abort()` | Context cancellation via `abortClaudeTurn()` |
| `SetModel` | SDK `session.SetModel()` | Local field update (no backend call) |
| `GetMessages` | SDK `session.GetMessages()` | JSON-marshaled local event history |
| `GetTasks` | Extracted from SDK event history | Empty slice (unsupported) |
| `CompactHistory` | SDK RPC call | `ErrNotSupported` |
| `FleetStart` | SDK RPC call | `ErrNotSupported` |
| `SetMode` | SDK RPC call | No-op (stored locally) |

### `copilotClient` (main.go)

Interface wrapping the Copilot SDK client for testability:

```go
type copilotClient interface {
    Start(context.Context) error
    Stop() error
    ForceStop()
    State() copilot.ConnectionState
    ListModels(context.Context) ([]copilot.ModelInfo, error)
    ListSessions(context.Context, *copilot.SessionListFilter) ([]copilot.SessionMetadata, error)
    GetSessionMetadata(context.Context, string) (*copilot.SessionMetadata, error)
    CreateSession(context.Context, *copilot.SessionConfig) (*copilot.Session, error)
    ResumeSession(context.Context, string, *copilot.ResumeSessionConfig) (*copilot.Session, error)
    DeleteSession(context.Context, string) error
    OnEventType(copilot.SessionLifecycleEventType, copilot.SessionLifecycleHandler) func()
}
```

---

## 4. HTTP Route Table

### Main Service Routes

| Method | Path | Handler | File | Description |
|--------|------|---------|------|-------------|
| `GET` | `/healthz` | `handleHealth` | `provider_copilot.go` | Health check; restarts dead Copilot client |
| `GET` | `/providers` | `handleListProviders` | `provider_copilot.go` | List configured providers with capabilities |
| `GET` | `/models` | `handleListModels` | `provider_copilot.go` | List available models for a provider |
| `GET` | `/sessions` | `handleListSessions` | `provider_copilot.go` | List persisted + live sessions |
| `POST` | `/sessions` | `handleCreateSession` | `api_handlers.go` | Create or resume a session |
| `GET` | `/sessions/{id}` | `handleGetSession` | `api_handlers.go` | Get session summary |
| `GET` | `/sessions/{id}/context` | `handleGetContext` | `api_handlers.go` | Get context window usage |
| `DELETE` | `/sessions/{id}` | `handleDeleteSession` | `api_handlers.go` | Delete/disconnect session |
| `POST` | `/sessions/{id}/model` | `handleSetModel` | `api_handlers.go` | Change model (with alias resolution) |
| `POST` | `/sessions/{id}/mode` | `handleSetAgentMode` | `api_handlers.go` | Change agent mode (ask/plan/autopilot) |
| `GET` | `/sessions/{id}/messages` | `handleGetMessages` | `api_handlers.go` | Get conversation history |
| `GET` | `/sessions/{id}/tasks` | `handleGetTasks` | `api_handlers.go` | Get background tasks |
| `POST` | `/sessions/{id}/messages` | `handleSendMessage` | `api_handlers.go` | Send a user message (start turn) |
| `POST` | `/sessions/{id}/compact` | `handleCompactHistory` | `api_handlers.go` | Compact/summarize history |
| `POST` | `/sessions/{id}/fleet` | `handleStartFleet` | `api_handlers.go` | Launch sub-agents |
| `GET` | `/sessions/{id}/events` | `handleEvents` | `api_handlers.go` | SSE event stream (long-lived) |
| `POST` | `/sessions/{id}/user-input/{requestID}` | `handleAnswerUserInput` | `api_handlers.go` | Answer a user-input prompt |
| `POST` | `/sessions/{id}/permission/{requestID}` | `handleAnswerPermission` | `api_handlers.go` | Answer a permission request |
| `POST` | `/sessions/{id}/permission-mode` | `handleSetPermissionMode` | `api_handlers.go` | Change permission mode |
| `POST` | `/sessions/{id}/abort` | `handleAbortSession` | `api_handlers.go` | Abort current turn |
| `POST` | `/sessions/{id}/tools` | `handleSetTools` | `api_handlers.go` | Update excluded tools list |
| `POST` | `/clients/{id}` | `handleRegisterClient` | `helpers.go` | Register Neovim client (lease) |
| `DELETE` | `/clients/{id}` | `handleUnregisterClient` | `helpers.go` | Unregister client (may trigger idle shutdown) |
| `POST` | `/shutdown` | `handleShutdown` | `helpers.go` | Graceful shutdown |

### Control Server Routes (separate listener for service discovery)

| Method | Path | Handler | File |
|--------|------|---------|------|
| `GET` | `/service-addr` | inline | `main.go` |
| `GET` | `/healthz` | inline | `main.go` |
| `POST` | `/shutdown` | inline | `main.go` |

---

## 5. Data Flow

### Sending a Message

```
Plugin: POST /sessions/{id}/messages  {"prompt": "Fix the bug"}
         │
         ▼
   handleSendMessage()                          [api_handlers.go]
     ├─ requireManagedSession(id)               [session_service.go]
     ├─ Register client lease
     ├─ managed.ps.SendMessage(ctx, opts)       [provider_session.go]
     │   ├─ [Copilot] session.Send(prompt, attachments)
     │   │     → SDK runs turn asynchronously
     │   │     → Events arrive via handleSessionEvent callback
     │   └─ [Claude] beginClaudeTurn() + go runClaudeQuery()
     │         → Streams from Anthropic API
     │         → Events generated by broadcastClaudeEvent()
     └─ Return HTTP 202 {messageId: "..."}

         ║ (async, via callbacks/goroutines)
         ▼
   managed.broadcast(sseMessage)                [session_state.go]
     └─ Fan-out to all subscriber channels
         │
         ▼
   handleEvents() SSE loop                      [api_handlers.go]
     └─ writeSSE(w, event, payload)             [helpers.go]
         │
         ▼
   Plugin receives SSE events via curl -N
```

### Permission / User-Input Flow

```
Provider requests permission
         │
         ▼
   handlePermissionRequest()                    [session_state.go]
     ├─ Auto-resolve if mode is approve-all / reject-all / approve-reads
     │   └─ Broadcast host.permission_decision → return immediately
     └─ [interactive mode]
         ├─ Create pendingPermission with resultCh
         ├─ Broadcast host.permission_requested via SSE
         └─ Block on resultCh (15-min timeout)

Plugin: POST /sessions/{id}/permission/{requestID}
         │
         ▼
   handleAnswerPermission()                     [api_handlers.go]
     └─ managed.answerPermission(requestID, approved)  [session_state.go]
         ├─ Send result to pendingPermission.resultCh
         ├─ Broadcast host.permission_decision
         └─ Provider callback unblocks, continues turn
```

### SSE History Replay (on connect)

```
Plugin: GET /sessions/{id}/events?history=true&history_turn_limit=5
         │
         ▼
   handleEvents()                               [api_handlers.go]
     ├─ Subscribe to managed session
     ├─ Send host.session_attached event
     ├─ [Copilot replay]
     │   ├─ session.GetMessages() → full event list
     │   ├─ selectReplayWindow() → last N turns
     │   ├─ marshalReplaySessionEvents() → sequenced, trimmed
     │   └─ writeSSE for each → host.history_done
     ├─ [Claude replay]
     │   ├─ claudeEventHistorySnapshot() → raw JSON events
     │   └─ writeSSE for each → host.history_done
     └─ Enter live event loop (select on subscriber ch / keepalive / ctx.Done)
```

---

## 6. SSE Event Contract

### Host Events (service → client)

| Event Name | Trigger | Key Data Fields |
|------------|---------|----------------|
| `host.session_attached` | SSE connect | Full `sessionSummary` |
| `host.history_done` | After replay | `sessionId`, `count` |
| `host.session_disconnected` | Delete/shutdown | `sessionId`, `reason`, `serviceRestarted` |
| `host.model_changed` | Model update | `sessionId`, `model`, `reasoningEffort` |
| `host.agent_mode_changed` | Mode update | `sessionId`, `mode` |
| `host.permission_requested` | Interactive perm | `sessionId`, `request` (id, kind, path) |
| `host.permission_decision` | Perm resolved | `sessionId`, `requestId`, `decision` |
| `host.permission_mode_changed` | Perm mode update | `sessionId`, `mode` |
| `host.user_input_requested` | User prompt | `sessionId`, `request` (id, question, choices) |
| `host.user_input_resolved` | Input answered | `sessionId`, `requestId`, `answer`, `auto` |
| `host.turn_aborted` | Abort | `sessionId` |
| `host.fleet_started` | Fleet launch | `sessionId`, `prompt`, `started` |
| `host.tools_changed` | Tools update | `sessionId`, `excludedTools` |
| `host.session_name_updated` | SDK auto-name | `sessionId`, `name` |

### Session Events (provider → client)

Wrapped as `event: session.event` with a JSON payload containing:
- `type` — provider event type (e.g. `assistant.turn_start`)
- `sequenceId` — monotonically increasing per-session counter
- `messageChunkIndex` — per-message chunk counter (for streaming)
- `data` — event-specific payload

**Common Copilot event types:**
`assistant.turn_start`, `assistant.message_delta`, `assistant.message`,
`assistant.turn_end`, `tool.execution_started`, `tool.execution_complete`,
`tool.execution_failed`, `permission.requested`, `permission.completed`,
`user_input.requested`, `user_input.completed`, `session.usage_info`,
`subagent.started`, `subagent.completed`

**Claude event types** (generated by service):
`assistant.turn_start`, `assistant.message_delta`, `assistant.error`,
`assistant.turn_end`, `assistant.usage`

---

## 7. Session Lifecycle

```
                    ┌─────────────┐
                    │  POST       │
                    │  /sessions  │
                    └──────┬──────┘
                           │
              ┌────────────┴────────────┐
              │ resume=true?            │
              ├─── yes ─► ResumeSession │
              └─── no ──► CreateSession │
                           │
                    ┌──────▼──────┐
                    │    LIVE     │◄──── SSE clients connect/disconnect
                    │             │      Send messages, change model/mode
                    │             │      Abort turns, manage permissions
                    └──────┬──────┘
                           │
              ┌────────────┴────────────┐
              │                         │
       DELETE /sessions/{id}    Service shutdown
              │                         │
              ▼                         ▼
     ┌────────────────┐      closeManagedSessions()
     │ Broadcast       │      ├─ Broadcast disconnect
     │  disconnect     │      ├─ Drain pending inputs
     │ Close session   │      └─ Disconnect all
     │ Delete state?   │
     └────────────────┘
```

**Idle shutdown:** When the last registered client unregisters, a
10-minute idle timer starts. If no client registers before it fires,
the service shuts down automatically.

---

## 8. Function Index by File

### main.go

| Function | Description |
|----------|-------------|
| `main()` | Entry point: parse flags, load providers, start client, register routes, listen |
| `registerRoutes(mux, svc)` | Wire all HTTP handlers to the mux |
| `resolveListener(addr, portRange)` | Resolve address or port-range listener |
| `startControlServerFromFlags(...)` | Start the control-socket/port-range discovery server |
| `logStartupInfo(svc, addr)` | Print startup banner with provider/model info |
| `normalizeProvider(value)` | Delegate to config package |
| `providerKey(value)` | Lowercase+trim provider name for map keys |
| `isCopilotProviderType(value)` | Check if provider type is "copilot" |
| `isClaudeProviderType(value)` | Check if provider type is "claude" |
| `loadProviders(...)` | Load provider config from YAML |
| `(s *service) providerNames()` | Ordered list of provider names |
| `(s *service) providerRuntime(name)` | Lookup a provider by name |
| `(s *service) isCopilotProviderName(name)` | Check if named provider is Copilot-type |
| `(s *service) isClaudeProviderName(name)` | Check if named provider is Claude-type |
| `(s *service) requestedProvider(r, fallback)` | Resolve provider from query param or default |
| `(s *service) registerClient(id, name)` | Add client to lease registry |
| `(s *service) unregisterClient(id)` | Remove client; schedule idle shutdown if empty |

### api_handlers.go

| Function | Description |
|----------|-------------|
| `handleCreateSession` | Create or resume a provider session |
| `handleCreateClaudeSession` | Claude-specific session creation subroutine |
| `handleGetSession` | Return session summary |
| `handleGetContext` | Return context window token usage |
| `handleDeleteSession` | Disconnect and optionally delete backend state |
| `handleSetModel` | Change model (alias resolution + provider call) |
| `handleGetMessages` | Return conversation history |
| `handleGetTasks` | Return background task list |
| `handleSendMessage` | Send user prompt to start a turn |
| `handleCompactHistory` | Compact conversation history |
| `handleStartFleet` | Launch sub-agents |
| `handleSetAgentMode` | Change agent mode (ask/plan/autopilot) |
| `handleSetPermissionMode` | Change permission mode |
| `handleSetTools` | Update excluded tools list |
| `handleAbortSession` | Cancel in-flight turn |
| `handleAnswerUserInput` | Resolve pending user-input prompt |
| `handleAnswerPermission` | Resolve pending permission request |
| `handleEvents` | SSE event stream (long-lived connection) |

### session_state.go

| Function | Description |
|----------|-------------|
| `(m) id()` | Session ID (prefers `ps.SessionID()`, falls back to field) |
| `(m) summary()` | Build `sessionSummary` snapshot |
| `(m) close(reason)` | Tear down session: unsubscribe events, drain pending, disconnect |
| `(m) broadcast(msg)` | Fan-out SSE message to all subscriber channels |
| `(m) subscribe()` | Create SSE subscriber channel |
| `(m) unsubscribe(ch)` | Remove subscriber channel |
| `(m) hasSubscribers()` | Check if any SSE clients connected |
| `(m) broadcastHostEvent(name, data)` | Emit a `host.*` event to subscribers |
| `(m) broadcastClaudeEvent(type, data, msgID)` | Emit a Claude `session.event` |
| `(m) handleSessionEvent(event)` | Copilot SDK event callback → sequence + broadcast |
| `(m) handlePermissionRequest(ctx, req)` | Process permission request (auto or interactive) |
| `(m) handleUserInputRequest(ctx, req)` | Process user-input request (auto or interactive) |
| `(m) answerPermission(id, approved)` | Deliver permission answer to blocked callback |
| `(m) answerUserInput(id, answer, freeform)` | Deliver user-input answer to blocked callback |
| `(m) marshalSequencedSessionEvent(event)` | Add sequenceId + chunkIndex to event payload |
| `(m) nextSessionEventMetadata(msgID)` | Generate next sequence ID and chunk index |
| `marshalSessionEventPayload(event)` | Serialize SDK event to JSON |
| `enrichSessionEventPayload(payload, seq, chunk)` | Inject metadata into JSON payload |
| `sessionEventMessageID(payload)` | Extract messageId from event JSON |
| `shouldReplayHistoryEvent(event, flag)` | Filter events for history replay |
| `selectReplayWindow(events, turnLimit)` | Select last N turns from event list |
| `(m) marshalReplaySessionEvents(...)` | Full replay pipeline: window → filter → trim → sequence |
| `truncateRunes(text, max)` | Truncate string with "..." ellipsis |
| `previewString(value, max)` | Extract preview text from various types |
| `trimReplayPayload(payload, type, summary, chars)` | Trim streaming deltas and tool results for replay |

### session_service.go

| Function | Description |
|----------|-------------|
| `sessionMapKey(provider, id)` | Build map key `"provider::sessionID"` |
| `(s) getManagedSession(id, provider)` | Lookup by ID, optional provider filter, detect ambiguity |
| `(s) requireManagedSession(w, r)` | Lookup + write HTTP error on failure |
| `(s) storeManagedSession(managed)` | Insert into session map |
| `(s) removeManagedSession(id, provider)` | Remove and return session |
| `(s) liveSessionSummaries(provider)` | List summaries of sessions with active subscribers |
| `(s) disconnectAll()` | Close all sessions (service shutdown) |
| `(s) closeManagedSessions(reason)` | Broadcast disconnect + close all sessions |
| `(s) resolveSessionWorkingDirectory(value)` | Resolve to absolute path or use default |
| `(s) resolveRequestedModel(ctx, provider, model)` | Expand model alias via Copilot model list |
| `resolveModelAlias(requested, models)` | Match model name against model list (exact → prefix → substring) |
| `resolveWorkingDirectory(value)` | Validate and absolutize a path |
| `clampInt64(value)` | Clamp negative to 0 |
| `buildContextWindowSnapshot(usage, models, model)` | Compute full context window with prompt/free/buffer tokens |

### provider_session.go

| Type | Description |
|------|-------------|
| `providerSession` | Interface: 11 methods abstracting provider operations |
| `sendMessageOpts` | Parameters struct for `SendMessage` |
| `ErrNotSupported` | Sentinel error for unsupported operations |
| `copilotSession` | Wraps `*copilot.Session` — full SDK delegation |
| `claudeSession` | Wraps `*managedSession` + `*service` — local state + Anthropic API |

### provider_copilot.go

| Function | Description |
|----------|-------------|
| `(s) currentClient()` | Get active Copilot SDK client |
| `(s) attachClientLifecycleHandlers(client)` | Subscribe to SDK lifecycle events |
| `(s) startCopilotClient()` | Start SDK client (acquire lock) |
| `(s) stopCopilotClient()` | Stop SDK client |
| `(s) ensureClientConnected()` | Start client if disconnected |
| `(s) restartCopilotClient(reason, force)` | Stop + start with optional force |
| `isRecoverableCopilotClientError(err)` | Check if error warrants auto-restart |
| `withCopilotClientRetry(s, op, fn)` | Execute with one auto-restart retry |
| `(s) handleHealth` | Health check endpoint |
| `(s) handleListProviders` | List providers with their capabilities |
| `(s) handleListModels` | List models for a provider |
| `(s) handleListSessions` | List persisted + live sessions |
| `listenInRange(host, portRange)` | Bind to first available port in range |

### provider_claude.go

| Function | Description |
|----------|-------------|
| `(m) beginClaudeTurn()` | Acquire turn lock, create cancellable context |
| `(m) abortClaudeTurn()` | Cancel active turn context |
| `(s) runClaudeQuery(managed, ctx, cancel, prompt, ...)` | Execute Anthropic API call with streaming |
| `(m) appendClaudeUserMessage(prompt, attachments)` | Add user message to conversation |
| `(m) claudeEventHistorySnapshot()` | Thread-safe copy of event history |
| `(m) appendClaudeEventHistory(payload)` | Append raw event to history |
| `processClaudeAttachments(attachments)` | Convert Copilot attachments to Anthropic content blocks |
| `readAttachmentContent(att)` | Read file/directory/selection attachment content |
| `newClaudeMessageID()` | Generate unique Claude message ID |
| `newAnthropicClient(apiKeyEnv, authTokenEnv, baseURL)` | Create Anthropic API client |

### helpers.go

| Function | Description |
|----------|-------------|
| `countDiscoverableConfig(wd)` | Count instructions/agents/skills/MCP servers in workspace |
| `countMCPServersInFile(path)` | Count MCP server entries in a JSON config file |
| `countMCPServerEntries(value)` | Count entries in map or slice |
| `defaultCLIPath()` | Find Copilot CLI path (env var → relative probing) |
| `loggingMiddleware(next)` | Log method, path, duration |
| `recoveryMiddleware(next)` | Catch panics, return 500 |
| `withCORS(next)` | Set CORS headers, handle OPTIONS |
| `writeJSON(w, status, payload)` | Write JSON response |
| `writeError(w, status, message)` | Write `{"error": "..."}` response |
| `decodeJSON(r, target)` | Decode body with `DisallowUnknownFields()` |
| `writeSSE(w, event, payload)` | Write SSE-formatted event |
| `mustJSON(payload)` | Marshal to JSON; return error JSON on failure |
| `queryBool(r, name)` | Parse boolean query param (1/true/yes) |
| `queryInt(r, name)` | Parse non-negative integer query param |
| `boolOrDefault(ptr, fallback)` | Dereference `*bool` with fallback |
| `stringOrEmpty(ptr)` | Dereference `*string` with empty fallback |
| `firstNonEmpty(values...)` | Return first non-empty trimmed string |
| `isValidPermissionMode(value)` | Validate permission mode string |
| `sessionIDPrefixForWorkingDirectory(wd)` | Generate safe prefix from directory name |
| `newSessionID(wd)` | Generate `prefix-timestamp` session ID |
| `(s) handleShutdown` | Graceful HTTP shutdown handler |
| `(s) handleRegisterClient` | Client lease registration handler |
| `(s) handleUnregisterClient` | Client lease removal handler |

### lsp.go

| Function | Description |
|----------|-------------|
| `runLSPServer(ctx, serviceURL)` | Start LSP server on stdin/stdout |
| `(s) serve(ctx)` | Main LSP message loop |
| `(s) readMessage()` | Read LSP JSON-RPC message from stdin |
| `(s) sendResponse(id, result, err)` | Write LSP response to stdout |
| `(s) handleMessage(ctx, msg)` | Route LSP methods (initialize, codeAction, executeCommand) |
| `(s) handleCodeAction(ctx, msg)` | Generate "Send to Copilot Agent" code actions |
| `(s) handleExecuteCommand(ctx, msg)` | Execute send-prompt command |
| `(s) resolveActiveSession(ctx, url)` | Find first live session via HTTP |
| `(s) sendPrompt(ctx, url, session, prompt)` | POST prompt to running session |
| `uriToFilename(uri)` | Convert `file://` URI to filesystem path |
| `readSelectionText(filename, range)` | Read selected text from file by line/char range |

---

## 9. Configuration

### Provider Config File (`~/.config/copilot-agent/providers.yml`)

```yaml
providers:
  - name: copilot
    type: copilot
    default_model: auto
  - name: claude
    type: claude
    default_model: claude-sonnet-4-5
    options:
      api_key_env: ANTHROPIC_API_KEY
      base_url: https://api.anthropic.com
```

Parsed by `config/config.go` into `providerRuntime` structs.
If no config file exists, a single "copilot" provider is used.

### CLI Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-addr` | `127.0.0.1:0` | Listen address (port 0 = random) |
| `-port-range` | | Port range fallback (e.g. `8080-8090`) |
| `-cwd` | current dir | Default working directory |
| `-cli-path` | auto-detect | Path to Copilot CLI `index.js` |
| `-provider` | `copilot` | Default provider type |
| `-model` | | Default model |
| `-control-socket` | | Unix socket for service discovery |
| `-control-port-range` | | TCP port range for service discovery |
| `-lsp` | `false` | Run as LSP server instead of HTTP |

---

## 10. Remaining Provider-Specific Code

These locations still have explicit provider type checks (by design):

| File | Location | Reason |
|------|----------|--------|
| `api_handlers.go` | `handleCreateSession` | Session creation routing — fundamentally different per provider |
| `api_handlers.go` | `handleDeleteSession` | Copilot client-level `DeleteSession()` call |
| `api_handlers.go` | `handleSetModel` | Copilot model alias resolution pre-step |
| `api_handlers.go` | `handleEvents` | SSE replay format differs (Copilot typed events vs Claude raw JSON) |

All other handlers use the `providerSession` interface uniformly.
