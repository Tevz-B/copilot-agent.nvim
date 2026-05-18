# Copilot Instructions

## Build & Run

```bash
# Run the service directly
cd server && go run .

# Build a binary (preferred path: bin/copilot-agent)
make build

# Run with common flags
cd server && go run . \
  -addr 127.0.0.1:8088 \
  -cwd /path/to/workspace \
  -cli-path /path/to/@github/copilot/index.js

# Run Go tests
cd server && go test -count=1 ./...

# Run Lua linting and tests
make lint-lua
make test-lua
```

## Architecture

> **For a comprehensive reference** — file map, function index, route table,
> data flows, SSE event contract, and session lifecycle — see
> **[`server/ARCHITECTURE.md`](../server/ARCHITECTURE.md)**.

This project has two layers:

**Go host service (`server/`)** — A multi-file HTTP service (`package main`) that wraps `github.com/github/copilot-sdk/go` and the Anthropic API. It supports multiple LLM providers via a `providerSession` interface. The service manages sessions keyed by `provider::sessionID`, streams events over SSE, and handles user interactions (permissions, input prompts) asynchronously.

Key source files:
- `main.go` — types, `service` struct, `main()`, route registration
- `api_handlers.go` — all session HTTP handlers
- `session_state.go` — `managedSession` struct, SSE broadcast, event sequencing
- `session_service.go` — session map operations, model resolution
- `provider_session.go` — `providerSession` interface + implementations
- `provider_copilot.go` — Copilot SDK client lifecycle
- `provider_claude.go` — Claude/Anthropic streaming and turn management
- `helpers.go` — HTTP middleware, JSON/SSE helpers, config discovery
- `lsp.go` — embedded LSP server for editor code actions

The service broadcasts two event namespaces over SSE:
- `session.event` — provider events (Copilot SDK events or Claude streaming events)
- `host.*` — synthetic control events (e.g., `host.user_input_requested`, `host.permission_decision`, `host.model_changed`)

**Neovim Lua plugin (`lua/copilot_agent/init.lua`, `plugin/copilot_agent.lua`)** — A thin Lua module that communicates with the Go service exclusively via `curl` shell-outs (`jobstart()` for async, `vim.fn.system()` for sync). It maintains a single `state` table (session_id, SSE job, chat buffer, model cache, pending service callbacks). The plugin entry point `plugin/copilot_agent.lua` registers all `CopilotAgent*` user commands.

**Key data flow:**
1. Lua plugin starts the Go service (if `auto_start = true`) by running `service.command` from the plugin root directory.
2. Plugin POSTs to `/sessions` to create/resume a session.
3. Plugin opens a persistent `curl -N` SSE stream to `/sessions/{id}/events`.
4. User prompts are POSTed to `/sessions/{id}/messages`.
5. `host.user_input_requested` events are routed to `vim.ui.select()` / `vim.ui.input()`, and the answer is POSTed to `/sessions/{id}/user-input/{requestID}`.

## Key Conventions

**Go side:**
- The service is split across multiple files in `server/` under `package main` — see [`server/ARCHITECTURE.md`](../server/ARCHITECTURE.md) for the full file map and function index.
- Requires Go 1.24 (see `go.mod`). HTTP routes use the 1.22+ method+path syntax: `"GET /sessions/{id}"`.
- Provider abstraction: handlers use the `providerSession` interface (`provider_session.go`) instead of if/else provider checks. Only session creation, deletion state cleanup, model alias resolution, and SSE replay retain explicit provider checks.
- All JSON responses go through `writeJSON(w, status, v)` and errors through `writeError(w, status, msg)`.
- SSE uses `writeSSE(w, eventName, data)` with `mustJSON()` for payload serialization.
- `decodeJSON` uses `DisallowUnknownFields()` — unknown fields in request bodies are rejected with 400.
- Use `boolOrDefault(ptr, fallback)` and `firstNonEmpty(values...)` helpers for optional/defaulted fields; avoid inline nil/empty checks.
- Use `queryBool(r, name)` for boolean query parameters (accepts `1`, `true`, `yes`).
- Session mutations are protected by `service.sessionsMu` (RWMutex). Subscriber fan-out uses `managedSession.subscribersMu`.
- SSE subscriber channels have a buffer of 256. `broadcast` uses a non-blocking `select`/`default` — slow or full subscriber channels silently drop messages.
- `CreateSession` registers `OnEvent` in the config struct; `ResumeSession` requires calling `session.On(managed.handleSessionEvent)` after the call returns. Keep these two paths in sync when touching event handling.
- The HTTP handler stack is `withCORS(recoveryMiddleware(loggingMiddleware(mux)))`. CORS is open (`Access-Control-Allow-Origin: *`) — do not add auth headers.
- Pending user-input requests time out after `defaultInputTimeout` (15 minutes). Timed-out or closed sessions drain `resultCh` with an error.
- Permission modes: `approve-all`, `reject-all`, `interactive`, `autopilot`, `approve-reads`. `handlePermissionRequest` resolves automatically or blocks for interactive input.

**Lua side:**
- All config is merged into `state.config` from `defaults` at `setup()` time; access config through `state.config.*`.
- HTTP calls use `raw_request` (async, `jobstart`) or `request` (sync); both shell out to `curl`.
- `working_directory` and `service.command` config values can be functions — always resolve via `working_directory()` and `service_command()`, never access the raw config value directly.
- The SSE parser accumulates partial lines in `state.sse_partial` and builds `state.sse_event` incrementally before dispatching.
- Model entries from the SDK may use either `id`/`name` or `ID`/`Name` keys; always normalize through `normalize_model_entry()` when reading model lists.
- Model IDs are cached in `state.model_cache` (normalized table with `id`, `name`, `label` fields). Tab-completion for `:CopilotAgentModel` uses `model_completion_items()`.
- Connection errors from curl are detected by string-matching against a fixed set of patterns in `is_connection_error()`.
- Notifications use the module-level `notify(message, level)` helper, which respects `state.config.notify`.
- `plugin_root()` resolves the repo directory from `debug.getinfo(1,"S").source` — used as the default `service.cwd`.
- Guard `vim.g.loaded_copilot_agent_plugin` prevents double-loading.

**SSE event contract (Go → Lua):**
- `host.*` events: `{ "timestamp": "...", "data": { ... } }`
- `session.event` events: raw marshaled `copilot.SessionEvent` payloads
- Keepalive comments (`: keepalive`) are sent every 15 seconds and should be ignored by the parser.

## Git

- Do not automatically commit changes
- Do not add Co-authored-by trailers to commits
