// Copyright 2026 ray-x. All rights reserved.
// Use of this source code is governed by an Apache 2.0
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"gopkg.in/yaml.v3"
)

const (
	permissionModeApproveAll   = "approve-all"
	permissionModeRejectAll    = "reject-all"
	permissionModeInteractive  = "interactive"   // routes each request to the Neovim UI
	permissionModeAutopilot    = "autopilot"     // approve-all + auto-answer user inputs
	permissionModeApproveReads = "approve-reads" // auto-approve workspace reads; prompt for writes/shell
	defaultModel               = ""
	defaultClientName          = "neovim-copilot-service"
	defaultInputTimeout        = 15 * time.Minute
	httpReadHeaderTimeout      = 10 * time.Second // Long enough for local clients while still rejecting stalled connections promptly.
	httpShutdownTimeout        = 5 * time.Second  // Allow in-flight HTTP requests a brief grace period during service shutdown.
	sseKeepAliveInterval       = 15 * time.Second // Keep reverse proxies and clients from treating idle event streams as dead.
	clientLeasePollInterval    = 500 * time.Millisecond
	clientLeaseEmptyGrace      = 10 * time.Minute
	sseSubscriberBufferSize    = 256 // Absorb bursts of session events (tool calls can produce 100+ events rapidly).
	asyncResultChannelSize     = 1   // Each pending prompt/permission only needs to hold a single terminal response.
	permissionRequestIDPrefix  = "perm"
	userInputRequestIDPrefix   = "input"
	sessionIDPrefix            = "nvim"
	sessionIDPrefixMaxLen      = 8
	providerCopilot            = "copilot"
	providerClaude             = "claude"
	claudeDefaultModel         = "claude-sonnet-4-5"
	claudeDefaultMaxTokens     = 4096
	claudeAttachmentMaxBytes   = 5 * 1024 * 1024
	claudeTextAttachmentMaxLen = 128 * 1024
	claudeDirectoryEntryLimit  = 200
)

func logInfof(format string, args ...any) {
	log.Printf("[INFO] "+format, args...)
}

func logWarnf(format string, args ...any) {
	log.Printf("[WARN] "+format, args...)
}

func logErrorf(format string, args ...any) {
	log.Printf("[ERROR] "+format, args...)
}

func fatalErrorf(format string, args ...any) {
	log.Fatalf("[ERROR] "+format, args...)
}

type createSessionRequest struct {
	SessionID                      string                       `json:"sessionId,omitempty"`
	Resume                         bool                         `json:"resume,omitempty"`
	Provider                       string                       `json:"provider,omitempty"`
	ClientID                       string                       `json:"clientId,omitempty"`
	ClientName                     string                       `json:"clientName,omitempty"`
	Model                          string                       `json:"model,omitempty"`
	ReasoningEffort                string                       `json:"reasoningEffort,omitempty"`
	WorkingDirectory               string                       `json:"workingDirectory,omitempty"`
	Streaming                      *bool                        `json:"streaming,omitempty"`
	IncludeSubAgentStreamingEvents *bool                        `json:"includeSubAgentStreamingEvents,omitempty"`
	PermissionMode                 string                       `json:"permissionMode,omitempty"`
	AvailableTools                 []string                     `json:"availableTools,omitempty"`
	ExcludedTools                  []string                     `json:"excludedTools,omitempty"`
	SystemMessage                  *copilot.SystemMessageConfig `json:"systemMessage,omitempty"`
	EnableConfigDiscovery          *bool                        `json:"enableConfigDiscovery,omitempty"`
	Agent                          string                       `json:"agent,omitempty"`
	CustomAgents                   []copilot.CustomAgentConfig  `json:"customAgents,omitempty"`
	SkillDirectories               []string                     `json:"skillDirectories,omitempty"`
	DisabledSkills                 []string                     `json:"disabledSkills,omitempty"`
}

type registerClientRequest struct {
	ClientID   string `json:"clientId,omitempty"`
	ClientName string `json:"clientName,omitempty"`
}

type sendMessageRequest struct {
	Prompt         string               `json:"prompt"`
	ClientID       string               `json:"clientId,omitempty"`
	Attachments    []copilot.Attachment `json:"attachments,omitempty"`
	RequestHeaders map[string]string    `json:"requestHeaders,omitempty"`
}

type fleetStartRequest struct {
	Prompt string `json:"prompt,omitempty"`
}

type contextWindowSnapshot struct {
	CurrentTokens         int64 `json:"currentTokens"`
	TokenLimit            int64 `json:"tokenLimit"`
	PromptTokenLimit      int64 `json:"promptTokenLimit"`
	MessagesLength        int64 `json:"messagesLength"`
	SystemTokens          int64 `json:"systemTokens"`
	ToolDefinitionsTokens int64 `json:"toolDefinitionsTokens"`
	SystemToolsTokens     int64 `json:"systemToolsTokens"`
	ConversationTokens    int64 `json:"conversationTokens"`
	FreeTokens            int64 `json:"freeTokens"`
	BufferTokens          int64 `json:"bufferTokens"`
}

type contextWindowResponse struct {
	SessionID     string                 `json:"sessionId"`
	Provider      string                 `json:"provider,omitempty"`
	Available     bool                   `json:"available"`
	ContextWindow *contextWindowSnapshot `json:"contextWindow,omitempty"`
}

type compactHistoryRequest struct{}

type setModelRequest struct {
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
}

type answerUserInputRequest struct {
	Answer      string `json:"answer"`
	WasFreeform bool   `json:"wasFreeform,omitempty"`
}

type listSessionsResponse struct {
	Provider  string                    `json:"provider,omitempty"`
	Persisted []copilot.SessionMetadata `json:"persisted"`
	Live      []sessionSummary          `json:"live"`
}

type backgroundTaskView struct {
	ID             string     `json:"id"`
	Kind           string     `json:"kind"`
	Status         string     `json:"status"`
	Title          string     `json:"title,omitempty"`
	Description    string     `json:"description,omitempty"`
	Summary        string     `json:"summary,omitempty"`
	Prompt         string     `json:"prompt,omitempty"`
	AgentID        string     `json:"agentId,omitempty"`
	AgentType      string     `json:"agentType,omitempty"`
	AgentName      string     `json:"agentName,omitempty"`
	ToolCallID     string     `json:"toolCallId,omitempty"`
	EntryID        string     `json:"entryId,omitempty"`
	Error          string     `json:"error,omitempty"`
	Model          string     `json:"model,omitempty"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	StartedAt      *time.Time `json:"startedAt,omitempty"`
	CompletedAt    *time.Time `json:"completedAt,omitempty"`
	DurationMs     *float64   `json:"durationMs,omitempty"`
	TotalTokens    *float64   `json:"totalTokens,omitempty"`
	TotalToolCalls *float64   `json:"totalToolCalls,omitempty"`
}

type hostEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Data      any       `json:"data"`
}

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

type providerFileConfig struct {
	Default         string              `yaml:"default"`
	DefaultProvider string              `yaml:"default_provider"`
	Providers       []providerFileEntry `yaml:"providers"`
}

type providerFileEntry struct {
	Name   string                 `yaml:"name"`
	Type   string                 `yaml:"type"`
	Model  string                 `yaml:"model"`
	Claude *providerClaudeOptions `yaml:"claude"`
}

type providerClaudeOptions struct {
	APIKeyEnv    string `yaml:"api_key_env"`
	AuthTokenEnv string `yaml:"auth_token_env"`
	BaseURL      string `yaml:"base_url"`
}

type providerRuntime struct {
	Name               string
	Type               string
	Model              string
	ClaudeAPIKeyEnv    string
	ClaudeAuthTokenEnv string
	ClaudeBaseURL      string
}

type service struct {
	clientMu                sync.RWMutex
	client                  copilotClient
	clientFactory           func() copilotClient
	clientCtx               context.Context
	provider                string
	providers               map[string]providerRuntime
	providerOrder           []string
	defaultModel            string
	defaultWorkingDirectory string
	shutdownHTTP            func(context.Context) error
	stop                    func()
	clientRegistryMu        sync.Mutex
	activeClients           map[string]registeredClient
	idleShutdownTimer       *time.Timer
	idleShutdownGrace       time.Duration
	sessions                map[string]*managedSession
	sessionsMu              sync.RWMutex
}

type registeredClient struct {
	ClientName   string
	RegisteredAt time.Time
	LastSeen     time.Time
}

func normalizeProvider(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", providerCopilot:
		return providerCopilot, nil
	case providerClaude:
		return providerClaude, nil
	default:
		return "", fmt.Errorf("provider must be one of: %s, %s", providerCopilot, providerClaude)
	}
}

func providerKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func isCopilotProviderType(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), providerCopilot)
}

func isClaudeProviderType(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), providerClaude)
}

func defaultClaudeAPIKeyEnv(options *providerClaudeOptions) string {
	if options != nil && strings.TrimSpace(options.APIKeyEnv) != "" {
		return strings.TrimSpace(options.APIKeyEnv)
	}
	return "ANTHROPIC_API_KEY"
}

func defaultClaudeAuthTokenEnv(options *providerClaudeOptions) string {
	if options != nil && strings.TrimSpace(options.AuthTokenEnv) != "" {
		return strings.TrimSpace(options.AuthTokenEnv)
	}
	return "ANTHROPIC_AUTH_TOKEN"
}

func providerFromFlags(defaultProviderType, defaultModel string) (map[string]providerRuntime, []string, string, error) {
	ptype, err := normalizeProvider(defaultProviderType)
	if err != nil {
		return nil, nil, "", err
	}
	name := providerKey(ptype)
	entries := map[string]providerRuntime{
		name: {
			Name:               name,
			Type:               ptype,
			Model:              strings.TrimSpace(defaultModel),
			ClaudeAPIKeyEnv:    defaultClaudeAPIKeyEnv(nil),
			ClaudeAuthTokenEnv: defaultClaudeAuthTokenEnv(nil),
		},
	}
	return entries, []string{name}, name, nil
}

func providerFromYAML(configPath, defaultProviderType, defaultModel string) (map[string]providerRuntime, []string, string, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("read providers config: %w", err)
	}

	var parsed providerFileConfig
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		return nil, nil, "", fmt.Errorf("parse providers config: %w", err)
	}
	if len(parsed.Providers) == 0 {
		return nil, nil, "", errors.New("providers config must define at least one provider")
	}

	providers := make(map[string]providerRuntime, len(parsed.Providers))
	order := make([]string, 0, len(parsed.Providers))
	for idx, entry := range parsed.Providers {
		name := providerKey(entry.Name)
		kind := strings.TrimSpace(entry.Type)
		if kind == "" {
			kind = entry.Name
		}
		ptype, kindErr := normalizeProvider(kind)
		if kindErr != nil {
			return nil, nil, "", fmt.Errorf("providers[%d] type: %w", idx, kindErr)
		}
		if name == "" {
			name = providerKey(ptype)
		}
		if name == "" {
			return nil, nil, "", fmt.Errorf("providers[%d] has an empty name", idx)
		}
		if _, exists := providers[name]; exists {
			return nil, nil, "", fmt.Errorf("providers config duplicates provider name %q", name)
		}
		claudeBaseURL := ""
		if entry.Claude != nil {
			claudeBaseURL = strings.TrimSpace(entry.Claude.BaseURL)
		}
		providers[name] = providerRuntime{
			Name:               name,
			Type:               ptype,
			Model:              strings.TrimSpace(entry.Model),
			ClaudeAPIKeyEnv:    defaultClaudeAPIKeyEnv(entry.Claude),
			ClaudeAuthTokenEnv: defaultClaudeAuthTokenEnv(entry.Claude),
			ClaudeBaseURL:      claudeBaseURL,
		}
		order = append(order, name)
	}

	if len(order) == 1 && strings.TrimSpace(defaultModel) != "" {
		rt := providers[order[0]]
		if strings.TrimSpace(rt.Model) == "" {
			rt.Model = strings.TrimSpace(defaultModel)
			providers[order[0]] = rt
		}
	}

	defaultName := providerKey(parsed.DefaultProvider)
	if defaultName == "" {
		defaultName = providerKey(parsed.Default)
	}
	flagName := providerKey(defaultProviderType)
	if flagName != "" {
		if _, ok := providers[flagName]; ok {
			defaultName = flagName
		}
	}
	if defaultName == "" && len(order) == 1 {
		defaultName = order[0]
	}
	if defaultName == "" {
		return nil, nil, "", errors.New("providers config must define default/default_provider when multiple providers exist")
	}
	if _, ok := providers[defaultName]; !ok {
		return nil, nil, "", fmt.Errorf("default provider %q is not defined in providers config", defaultName)
	}
	return providers, order, defaultName, nil
}

func loadProviders(configPath, defaultProviderType, defaultModel string) (map[string]providerRuntime, []string, string, error) {
	if strings.TrimSpace(configPath) == "" {
		return providerFromFlags(defaultProviderType, defaultModel)
	}
	return providerFromYAML(configPath, defaultProviderType, defaultModel)
}

func (s *service) providerNames() []string {
	if s == nil {
		return nil
	}
	if len(s.providers) == 0 {
		return []string{s.defaultProviderName()}
	}
	if len(s.providerOrder) > 0 {
		return append([]string(nil), s.providerOrder...)
	}
	names := make([]string, 0, len(s.providers))
	for _, rt := range s.providers {
		names = append(names, rt.Name)
	}
	sort.Strings(names)
	return names
}

func (s *service) defaultProviderName() string {
	if s == nil {
		return providerCopilot
	}
	name := providerKey(s.provider)
	if name == "" {
		name = providerCopilot
	}
	if len(s.providers) == 0 {
		return name
	}
	if _, ok := s.providers[name]; ok {
		return name
	}
	for _, candidate := range s.providerNames() {
		if _, ok := s.providers[candidate]; ok {
			return candidate
		}
	}
	return name
}

func (s *service) providerRuntime(name string) (providerRuntime, bool) {
	if s == nil {
		return providerRuntime{}, false
	}
	if len(s.providers) == 0 {
		ptype, err := normalizeProvider(s.provider)
		if err != nil {
			ptype = providerCopilot
		}
		defaultName := providerKey(name)
		if defaultName == "" {
			defaultName = providerKey(ptype)
		}
		return providerRuntime{
			Name:               defaultName,
			Type:               ptype,
			Model:              strings.TrimSpace(s.defaultModel),
			ClaudeAPIKeyEnv:    defaultClaudeAPIKeyEnv(nil),
			ClaudeAuthTokenEnv: defaultClaudeAuthTokenEnv(nil),
		}, true
	}
	key := providerKey(name)
	if key == "" {
		key = s.defaultProviderName()
	}
	rt, ok := s.providers[key]
	return rt, ok
}

func (s *service) requestedProviderName(r *http.Request, bodyProvider string) (string, error) {
	requested := providerKey(bodyProvider)
	if requested == "" && r != nil {
		requested = providerKey(r.URL.Query().Get("provider"))
	}
	if requested == "" {
		requested = s.defaultProviderName()
	}
	if _, ok := s.providerRuntime(requested); ok {
		return requested, nil
	}
	return "", fmt.Errorf("unknown provider %q (available: %s)", requested, strings.Join(s.providerNames(), ", "))
}

func (s *service) requestedProvider(r *http.Request, bodyProvider string) (providerRuntime, error) {
	name, err := s.requestedProviderName(r, bodyProvider)
	if err != nil {
		return providerRuntime{}, err
	}
	rt, ok := s.providerRuntime(name)
	if !ok {
		return providerRuntime{}, fmt.Errorf("unknown provider %q", name)
	}
	return rt, nil
}

func (s *service) isCopilotProviderName(name string) bool {
	rt, ok := s.providerRuntime(name)
	return ok && isCopilotProviderType(rt.Type)
}

func (s *service) isClaudeProviderName(name string) bool {
	rt, ok := s.providerRuntime(name)
	return ok && isClaudeProviderType(rt.Type)
}

func (s *service) hasProviderType(ptype string) bool {
	if len(s.providers) == 0 {
		rt, ok := s.providerRuntime("")
		return ok && strings.EqualFold(rt.Type, ptype)
	}
	for _, rt := range s.providers {
		if strings.EqualFold(rt.Type, ptype) {
			return true
		}
	}
	return false
}

func (s *service) detachedIdleGrace() time.Duration {
	if s != nil && s.idleShutdownGrace > 0 {
		return s.idleShutdownGrace
	}
	return clientLeaseEmptyGrace
}

func (s *service) cancelIdleShutdownLocked() {
	if s.idleShutdownTimer == nil {
		return
	}
	if s.idleShutdownTimer.Stop() {
		logInfof("client registration resumed; canceled detached-service idle timer")
	}
	s.idleShutdownTimer = nil
}

func (s *service) scheduleIdleShutdownLocked(reason string) {
	if s == nil || s.stop == nil {
		return
	}
	grace := s.detachedIdleGrace()
	if grace <= 0 {
		return
	}
	s.cancelIdleShutdownLocked()
	logInfof("no active registered clients; will shut down detached service in %s (%s)", grace, reason)
	s.idleShutdownTimer = time.AfterFunc(grace, func() {
		s.clientRegistryMu.Lock()
		if len(s.activeClients) > 0 {
			s.clientRegistryMu.Unlock()
			return
		}
		s.idleShutdownTimer = nil
		s.clientRegistryMu.Unlock()
		logWarnf("no active registered clients for %s (%s); shutting down detached service", grace, reason)
		s.stop()
	})
}

func (s *service) startIdleShutdownIfNoClients(reason string) {
	s.clientRegistryMu.Lock()
	defer s.clientRegistryMu.Unlock()
	if len(s.activeClients) == 0 {
		s.scheduleIdleShutdownLocked(reason)
	}
}

func (s *service) registerClient(clientID, clientName string) int {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return 0
	}
	now := time.Now()
	s.clientRegistryMu.Lock()
	defer s.clientRegistryMu.Unlock()
	if s.activeClients == nil {
		s.activeClients = make(map[string]registeredClient)
	}
	reg, exists := s.activeClients[clientID]
	reg.ClientName = clientName
	if reg.RegisteredAt.IsZero() {
		reg.RegisteredAt = now
	}
	reg.LastSeen = now
	s.activeClients[clientID] = reg
	if !exists {
		logInfof("client registered clientID=%s name=%s activeClients=%d", clientID, clientName, len(s.activeClients))
	} else {
		logInfof("client refreshed clientID=%s name=%s activeClients=%d", clientID, clientName, len(s.activeClients))
	}
	s.cancelIdleShutdownLocked()
	return len(s.activeClients)
}

func (s *service) unregisterClient(clientID string) int {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return 0
	}
	s.clientRegistryMu.Lock()
	defer s.clientRegistryMu.Unlock()
	if s.activeClients == nil {
		s.activeClients = make(map[string]registeredClient)
	}
	delete(s.activeClients, clientID)
	logInfof("client unregistered clientID=%s activeClients=%d", clientID, len(s.activeClients))
	if len(s.activeClients) == 0 {
		s.scheduleIdleShutdownLocked("last client unregistered")
	}
	return len(s.activeClients)
}

func main() {
	addr := flag.String("addr", "", "HTTP listen address (host:port). Leave empty or use port 0 to let the OS assign a free port (default).")
	portRange := flag.String("port-range", "", "Port range to try when -addr is not set, e.g. 18000-19000. The first available port in the range is used.")
	provider := flag.String("provider", providerCopilot, "default provider name (or provider type when --providers-config is not set)")
	providersConfig := flag.String("providers-config", "", "path to providers YAML (supports multiple named providers)")
	controlSocket := flag.String("control-socket", "", "Unix socket path for local control API (GET /service-addr, GET /healthz, POST /shutdown)")
	controlAddr := flag.String("control-addr", "", "TCP listen address for local control API (use on Windows, e.g. 127.0.0.1:0)")
	cliPath := flag.String("cli-path", defaultCLIPath(), "path to Copilot CLI binary or JS entrypoint")
	cliURL := flag.String("cli-url", "", "URL for an already-running Copilot CLI server")
	claudeCLIPath := flag.String("claude-cli-path", "", "deprecated: ignored when -provider=claude (Anthropic API is used directly)")
	model := flag.String("model", defaultModel, "default model for new sessions; empty uses the Copilot CLI account default")
	logLevel := flag.String("log-level", "error", "Copilot CLI log level")
	logFile := flag.String("log-file", "", "path to write service logs (default: stderr only)")
	cwdFlag := flag.String("cwd", "", "default working directory for new sessions")
	lspMode := flag.Bool("lsp", true, "run LSP server over stdio alongside the HTTP service (default: true)")
	lspOnly := flag.Bool("lsp-only", false, "run only the LSP server over stdio and connect to an existing HTTP service")
	serviceURL := flag.String("service-url", "", "HTTP service URL for -lsp-only mode, e.g. http://127.0.0.1:8088")
	flag.Parse()

	if path := strings.TrimSpace(*logFile); path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fatalErrorf("create log directory: %v", err)
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fatalErrorf("open log file %s: %v", path, err)
		}
		defer f.Close()
		// Write to both stderr (captured by Neovim) and the persistent file.
		log.SetOutput(io.MultiWriter(os.Stderr, f))
		logInfof("logging to %s (pid %d)", path, os.Getpid())
	}

	if *lspOnly {
		if strings.TrimSpace(*serviceURL) == "" {
			fatalErrorf("service-url is required when -lsp-only is set")
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := runLSPServer(ctx, strings.TrimSpace(*serviceURL)); err != nil && err != context.Canceled {
			fatalErrorf("%v", err)
		}
		return
	}

	workingDirectory, err := resolveWorkingDirectory(*cwdFlag)
	if err != nil {
		fatalErrorf("%v", err)
	}
	providers, providerOrder, selectedProvider, err := loadProviders(strings.TrimSpace(*providersConfig), strings.TrimSpace(*provider), strings.TrimSpace(*model))
	if err != nil {
		fatalErrorf("%v", err)
	}

	clientOptions := copilot.ClientOptions{
		CLIPath:  strings.TrimSpace(*cliPath),
		CLIUrl:   strings.TrimSpace(*cliURL),
		Cwd:      workingDirectory,
		LogLevel: *logLevel,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	svc := &service{
		clientCtx: ctx,
		clientFactory: func() copilotClient {
			return copilot.NewClient(&clientOptions)
		},
		provider:                selectedProvider,
		providers:               providers,
		providerOrder:           providerOrder,
		defaultModel:            strings.TrimSpace(*model),
		defaultWorkingDirectory: workingDirectory,
		stop:                    stop,
		activeClients:           make(map[string]registeredClient),
		idleShutdownGrace:       clientLeaseEmptyGrace,
		sessions:                make(map[string]*managedSession),
	}
	if svc.hasProviderType(providerClaude) && strings.TrimSpace(*claudeCLIPath) != "" {
		logWarnf("-claude-cli-path is ignored when using provider type=claude; configure Anthropic credentials via environment or providers YAML")
	}

	if svc.hasProviderType(providerCopilot) {
		if err = svc.startCopilotClient(); err != nil {
			fatalErrorf("%v", err)
		}
		defer func() {
			if err := svc.stopCopilotClient(); err != nil {
				logWarnf("stop copilot client: %v", err)
			}
		}()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", svc.handleHealth)
	mux.HandleFunc("GET /providers", svc.handleListProviders)
	mux.HandleFunc("GET /models", svc.handleListModels)
	mux.HandleFunc("GET /sessions", svc.handleListSessions)
	mux.HandleFunc("POST /sessions", svc.handleCreateSession)
	mux.HandleFunc("GET /sessions/{id}", svc.handleGetSession)
	mux.HandleFunc("GET /sessions/{id}/context", svc.handleGetContext)
	mux.HandleFunc("DELETE /sessions/{id}", svc.handleDeleteSession)
	mux.HandleFunc("POST /sessions/{id}/model", svc.handleSetModel)
	mux.HandleFunc("POST /sessions/{id}/mode", svc.handleSetAgentMode)
	mux.HandleFunc("GET /sessions/{id}/messages", svc.handleGetMessages)
	mux.HandleFunc("GET /sessions/{id}/tasks", svc.handleGetTasks)
	mux.HandleFunc("POST /sessions/{id}/messages", svc.handleSendMessage)
	mux.HandleFunc("POST /sessions/{id}/compact", svc.handleCompactHistory)
	mux.HandleFunc("POST /sessions/{id}/fleet", svc.handleStartFleet)
	mux.HandleFunc("GET /sessions/{id}/events", svc.handleEvents)
	mux.HandleFunc("POST /sessions/{id}/user-input/{requestID}", svc.handleAnswerUserInput)
	mux.HandleFunc("POST /sessions/{id}/permission/{requestID}", svc.handleAnswerPermission)
	mux.HandleFunc("POST /sessions/{id}/permission-mode", svc.handleSetPermissionMode)
	mux.HandleFunc("POST /sessions/{id}/abort", svc.handleAbortSession)
	mux.HandleFunc("POST /sessions/{id}/tools", svc.handleSetTools)
	mux.HandleFunc("POST /clients/{id}", svc.handleRegisterClient)
	mux.HandleFunc("DELETE /clients/{id}", svc.handleUnregisterClient)
	mux.HandleFunc("POST /shutdown", svc.handleShutdown)

	// Resolve listen address. When -addr is not set, honour -port-range if
	// provided, otherwise let the OS assign a free port (127.0.0.1:0).
	listenAddr := strings.TrimSpace(*addr)
	var listener net.Listener
	if listenAddr == "" || listenAddr == ":0" {
		if pr := strings.TrimSpace(*portRange); pr != "" {
			listener, err = listenInRange("127.0.0.1", pr)
		} else {
			listener, err = net.Listen("tcp", "127.0.0.1:0")
		}
	} else {
		listener, err = net.Listen("tcp", listenAddr)
	}
	if err != nil {
		fatalErrorf("listen %s: %v", listenAddr, err)
	}
	boundAddr := listener.Addr().String()
	// Ensure LSP proxy always has a full host:port (handles bare ":PORT" case).
	if strings.HasPrefix(boundAddr, ":") {
		boundAddr = "127.0.0.1" + boundAddr
	}

	server := &http.Server{
		Handler:           withCORS(recoveryMiddleware(loggingMiddleware(mux))),
		ReadHeaderTimeout: httpReadHeaderTimeout,
	}
	svc.shutdownHTTP = server.Shutdown
	var controlCleanup func()
	socketPath := strings.TrimSpace(*controlSocket)
	controlListenAddr := strings.TrimSpace(*controlAddr)
	switch {
	case socketPath != "":
		cleanup, controlErr := startControlServer(ctx, svc, "unix", socketPath, "", boundAddr)
		if controlErr != nil {
			fatalErrorf("start control socket %s: %v", socketPath, controlErr)
		}
		controlCleanup = cleanup
		defer controlCleanup()
	case controlListenAddr != "":
		cleanup, controlErr := startControlServer(ctx, svc, "tcp", "", controlListenAddr, boundAddr)
		if controlErr != nil {
			fatalErrorf("start control listener %s: %v", controlListenAddr, controlErr)
		}
		controlCleanup = cleanup
		defer controlCleanup()
	case runtime.GOOS == "windows":
		cleanup, controlErr := startControlServer(ctx, svc, "tcp", "", "127.0.0.1:0", boundAddr)
		if controlErr != nil {
			fatalErrorf("start control listener: %v", controlErr)
		}
		controlCleanup = cleanup
		defer controlCleanup()
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logWarnf("shutdown HTTP server: %v", err)
		}
		svc.disconnectAll()
	}()

	// Start the LSP server on stdio concurrently with the HTTP service.
	if *lspMode {
		serviceURL := "http://" + boundAddr
		go func() {
			if err := runLSPServer(ctx, serviceURL); err != nil && err != context.Canceled {
				logWarnf("lsp server: %v", err)
			}
		}()
	}

	logInfof("Neovim provider service listening on %s", boundAddr)
	logInfof("Providers: %s (default=%s)", strings.Join(svc.providerNames(), ", "), svc.defaultProviderName())
	logInfof("Default workspace: %s", workingDirectory)
	if svc.hasProviderType(providerCopilot) {
		if strings.TrimSpace(*cliURL) != "" {
			logInfof("Using external Copilot CLI server at %s", strings.TrimSpace(*cliURL))
		} else if strings.TrimSpace(*cliPath) != "" {
			logInfof("Using Copilot CLI at %s", strings.TrimSpace(*cliPath))
		}
	}
	if svc.hasProviderType(providerClaude) {
		claudeCredentialPresent := false
		for _, name := range svc.providerNames() {
			rt, ok := svc.providerRuntime(name)
			if !ok || !isClaudeProviderType(rt.Type) {
				continue
			}
			if firstNonEmpty(os.Getenv(rt.ClaudeAPIKeyEnv), os.Getenv(rt.ClaudeAuthTokenEnv)) != "" {
				claudeCredentialPresent = true
				break
			}
		}
		if !claudeCredentialPresent {
			logWarnf("provider type=claude configured but no configured Anthropic credential env vars were found")
		} else {
			logInfof("Using Anthropic API credentials from environment")
		}
	}
	svc.startIdleShutdownIfNoClients("startup")

	// Print the machine-readable address to stderr so it doesn't pollute the
	// LSP stdio stream. The Neovim plugin reads it from on_stderr.
	fmt.Fprintf(os.Stderr, "COPILOT_AGENT_ADDR=%s\n", boundAddr)

	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatalErrorf("%v", err)
	}
}

func startControlServer(ctx context.Context, svc *service, network string, socketPath string, listenAddr string, serviceAddr string) (func(), error) {
	if network == "unix" {
		if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
			return nil, err
		}
		if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}

	ln, err := net.Listen(network, firstNonEmpty(listenAddr, socketPath))
	if err != nil {
		return nil, err
	}
	if network == "unix" {
		if chmodErr := os.Chmod(socketPath, 0o600); chmodErr != nil {
			ln.Close()
			_ = os.Remove(socketPath)
			return nil, chmodErr
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /service-addr", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"serviceAddr": serviceAddr,
			"serviceURL":  "http://" + serviceAddr,
		})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":          true,
			"serviceAddr": serviceAddr,
		})
	})
	mux.HandleFunc("POST /shutdown", func(w http.ResponseWriter, _ *http.Request) {
		logInfof("shutdown requested via control socket")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		if svc.shutdownHTTP == nil {
			return
		}
		go func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
			defer cancel()
			if err := svc.shutdownHTTP(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logWarnf("shutdown via control socket: %v", err)
			}
		}()
	})

	controlServer := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: httpReadHeaderTimeout,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
		defer cancel()
		_ = controlServer.Shutdown(shutdownCtx)
	}()

	go func() {
		if serveErr := controlServer.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			logWarnf("control socket server: %v", serveErr)
		}
	}()

	if network != "unix" {
		fmt.Fprintf(os.Stderr, "COPILOT_AGENT_CONTROL_ADDR=%s\n", ln.Addr().String())
	}

	return func() {
		_ = controlServer.Close()
		_ = ln.Close()
		if network == "unix" {
			_ = os.Remove(socketPath)
		}
	}, nil
}

func startClientLeaseWatcher(ctx context.Context, leaseDir string, stop func()) {
	leaseDir = strings.TrimSpace(leaseDir)
	if leaseDir == "" || stop == nil {
		return
	}
	logInfof("client lease watcher active dir=%s poll=%s grace=%s", leaseDir, clientLeasePollInterval, clientLeaseEmptyGrace)

	// Require several consecutive empty reads before shutting down.
	// This prevents transient filesystem glitches (momentary empty
	// directory during lease renewal, NFS caching, etc.) from killing
	// the service while a client is still connected.
	emptyThreshold := int(clientLeaseEmptyGrace / clientLeasePollInterval)
	if emptyThreshold < 1 {
		emptyThreshold = 1
	}

	go func() {
		ticker := time.NewTicker(clientLeasePollInterval)
		defer ticker.Stop()

		seenLease := false
		consecutiveEmpty := 0
		lastLeaseCount := -1
		shutdown := func(reason string) {
			logWarnf("client lease dir empty (%s, %d consecutive checks); shutting down detached service", reason, consecutiveEmpty)
			stop()
		}

		check := func() (bool, string) {
			entries, err := os.ReadDir(leaseDir)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					if seenLease {
						consecutiveEmpty++
						if consecutiveEmpty >= emptyThreshold {
							return true, "missing"
						}
						return false, "missing"
					}
					return false, "missing"
				}
				logWarnf("watch client leases %s: %v", leaseDir, err)
				consecutiveEmpty = 0
				return false, "error"
			}
			if count := len(entries); count != lastLeaseCount {
				logInfof("client lease watcher state dir=%s leases=%d seenLease=%t", leaseDir, count, seenLease)
				lastLeaseCount = count
			}
			if len(entries) > 0 {
				seenLease = true
				consecutiveEmpty = 0
				return false, "live"
			}
			if seenLease {
				consecutiveEmpty++
				if consecutiveEmpty >= emptyThreshold {
					return true, "empty"
				}
				return false, "empty"
			}
			return false, "empty"
		}

		if shouldStop, reason := check(); shouldStop {
			shutdown(reason)
			return
		}

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if shouldStop, reason := check(); shouldStop {
					shutdown(reason)
					return
				}
			}
		}
	}()
}

// listenInRange tries each port in "lo-hi" (inclusive) on the given host and
// returns the first listener that succeeds. Returns an error if no port in the
// range is available.

// handleSetAgentMode changes the agent mode (interactive / plan / autopilot) for a session
// by calling the SDK's session.mode.set RPC.

// toSDKAgentMode maps user-facing mode names to SDK SessionMode constants.

// handleSetTools updates the locally-tracked excluded-tools list for the session.
// Note: this does not affect the active session in the SDK (tools are configured at
// session-creation time); the updated list is reflected in subsequent GET /sessions/{id}
// responses and will be used when the session is next resumed.

func (m *managedSession) hydrateSessionEventSequenceState(sequenceID uint64, messageChunkIndexes map[string]uint64) {
	m.eventSequenceMu.Lock()
	defer m.eventSequenceMu.Unlock()

	if m.nextEventSequence < sequenceID {
		m.nextEventSequence = sequenceID
	}
	if m.messageChunkIndexes == nil {
		m.messageChunkIndexes = make(map[string]uint64)
	}
	for messageID, chunkIndex := range messageChunkIndexes {
		if existing := m.messageChunkIndexes[messageID]; existing < chunkIndex {
			m.messageChunkIndexes[messageID] = chunkIndex
		}
	}
}

// recoveryMiddleware catches panics in HTTP handlers so a single bad request
// cannot crash the entire service process.  The panic and stack trace are
// logged before returning 500 to the client.
