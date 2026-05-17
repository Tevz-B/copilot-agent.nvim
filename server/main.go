// Copyright 2026 ray-x. All rights reserved.
// Use of this source code is governed by an Apache 2.0
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
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

type pendingUserInputView struct {
	ID            string    `json:"id"`
	Question      string    `json:"question"`
	Choices       []string  `json:"choices,omitempty"`
	AllowFreeform bool      `json:"allowFreeform"`
	CreatedAt     time.Time `json:"createdAt"`
}

type userInputResult struct {
	Response copilot.UserInputResponse
	Err      error
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

type managedSession struct {
	session              *copilot.Session
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
	APIKeyEnv   string `yaml:"api_key_env"`
	AuthTokenEnv string `yaml:"auth_token_env"`
	BaseURL     string `yaml:"base_url"`
}

type providerRuntime struct {
	Name           string
	Type           string
	Model          string
	ClaudeAPIKeyEnv string
	ClaudeAuthTokenEnv string
	ClaudeBaseURL  string
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
			Name:                name,
			Type:                ptype,
			Model:               strings.TrimSpace(defaultModel),
			ClaudeAPIKeyEnv:     defaultClaudeAPIKeyEnv(nil),
			ClaudeAuthTokenEnv:  defaultClaudeAuthTokenEnv(nil),
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

func (s *service) currentClient() copilotClient {
	s.clientMu.RLock()
	defer s.clientMu.RUnlock()
	return s.client
}

func (s *service) attachClientLifecycleHandlers(client copilotClient) {
	// Track session name updates from the SDK. The SDK auto-generates a summary
	// after each turn and delivers it via a SessionLifecycleUpdated event.
	client.OnEventType(copilot.SessionLifecycleUpdated, func(event copilot.SessionLifecycleEvent) {
		if event.Metadata == nil || event.Metadata.Summary == nil {
			return
		}
		s.sessionsMu.RLock()
		matched := make([]*managedSession, 0, 1)
		for _, candidate := range s.sessions {
			if candidate == nil || candidate.id() != event.SessionID {
				continue
			}
			if !s.isCopilotProviderName(candidate.provider) {
				continue
			}
			matched = append(matched, candidate)
		}
		s.sessionsMu.RUnlock()
		if len(matched) == 0 {
			return
		}
		name := *event.Metadata.Summary
		for _, managed := range matched {
			managed.sessionName = name
			managed.broadcastHostEvent("host.session_name_updated", map[string]string{
				"sessionId": event.SessionID,
				"name":      name,
			})
		}
	})
}

func (s *service) startCopilotClient() error {
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	return s.startCopilotClientLocked()
}

func (s *service) startCopilotClientLocked() error {
	if s.clientFactory == nil {
		return errors.New("copilot client factory is not configured")
	}
	if s.clientCtx == nil {
		return errors.New("copilot client context is not configured")
	}
	client := s.clientFactory()
	if client == nil {
		return errors.New("copilot client factory returned nil")
	}
	if err := client.Start(s.clientCtx); err != nil {
		return fmt.Errorf("start copilot client: %w", err)
	}
	s.attachClientLifecycleHandlers(client)
	s.client = client
	return nil
}

func (s *service) stopCopilotClient() error {
	s.clientMu.Lock()
	client := s.client
	s.client = nil
	s.clientMu.Unlock()
	if client == nil {
		return nil
	}
	return client.Stop()
}

func (s *service) ensureClientConnected() error {
	if s == nil || !s.hasProviderType(providerCopilot) {
		return nil
	}
	client := s.currentClient()
	if client != nil && client.State() == copilot.StateConnected {
		return nil
	}
	if client == nil {
		logWarnf("health check restarting Copilot CLI client: no connected client")
	} else {
		logWarnf("health check restarting Copilot CLI client: state=%v", client.State())
	}
	return s.restartCopilotClient(errors.New("copilot client is disconnected"), false)
}

func (s *service) restartCopilotClient(reason error, force bool) error {
	s.clientMu.Lock()
	defer s.clientMu.Unlock()

	if !force && s.client != nil && s.client.State() == copilot.StateConnected {
		return nil
	}

	oldClient := s.client
	s.client = nil
	if oldClient != nil {
		logWarnf("restarting Copilot CLI client after failure: %v", reason)
		oldClient.ForceStop()
		s.closeManagedSessions(fmt.Errorf("copilot client restarted: %w", reason))
	}

	return s.startCopilotClientLocked()
}

func isRecoverableCopilotClientError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "cli process exited"),
		strings.Contains(message, "process exited unexpectedly"),
		strings.Contains(message, "client not connected"),
		strings.Contains(message, "client stopped"):
		return true
	default:
		return false
	}
}

func withCopilotClientRetry[T any](s *service, operation string, fn func(copilotClient) (T, error)) (T, error) {
	var zero T
	if s == nil || !s.hasProviderType(providerCopilot) {
		return zero, fmt.Errorf("%s is only supported with provider type=%s", operation, providerCopilot)
	}

	if err := s.ensureClientConnected(); err != nil {
		return zero, err
	}
	client := s.currentClient()
	if client == nil {
		return zero, errors.New("copilot client is unavailable")
	}

	result, err := fn(client)
	if err == nil || !isRecoverableCopilotClientError(err) {
		return result, err
	}

	if restartErr := s.restartCopilotClient(fmt.Errorf("%s: %w", operation, err), true); restartErr != nil {
		return zero, errors.Join(err, fmt.Errorf("restart copilot client: %w", restartErr))
	}
	client = s.currentClient()
	if client == nil {
		return zero, err
	}
	return fn(client)
}

func (s *service) handleHealth(w http.ResponseWriter, r *http.Request) {
	providerRuntime, err := s.requestedProvider(r, "")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if isCopilotProviderType(providerRuntime.Type) {
		if err := s.ensureClientConnected(); err != nil {
			writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("copilot client unavailable: %v", err))
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"provider":        providerRuntime.Name,
		"providerType":    providerRuntime.Type,
		"defaultProvider": s.defaultProviderName(),
	})
}

func (s *service) handleListProviders(w http.ResponseWriter, _ *http.Request) {
	names := s.providerNames()
	providers := make([]map[string]any, 0, len(names))
	for _, name := range names {
		rt, ok := s.providerRuntime(name)
		if !ok {
			continue
		}
		item := map[string]any{
			"name":         rt.Name,
			"type":         rt.Type,
			"defaultModel": rt.Model,
		}
		if isClaudeProviderType(rt.Type) {
			item["claude"] = map[string]any{
				"apiKeyEnv":    rt.ClaudeAPIKeyEnv,
				"authTokenEnv": rt.ClaudeAuthTokenEnv,
				"baseURL":      rt.ClaudeBaseURL,
			}
		}
		providers = append(providers, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"default":   s.defaultProviderName(),
		"providers": providers,
	})
}

// listenInRange tries each port in "lo-hi" (inclusive) on the given host and
// returns the first listener that succeeds. Returns an error if no port in the
// range is available.
func listenInRange(host, portRange string) (net.Listener, error) {
	parts := strings.SplitN(portRange, "-", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid port range %q: expected \"lo-hi\"", portRange)
	}
	lo, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	hi, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || lo < 1 || hi > 65535 || lo > hi {
		return nil, fmt.Errorf("invalid port range %q", portRange)
	}
	for port := lo; port <= hi; port++ {
		addr := fmt.Sprintf("%s:%d", host, port)
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln, nil
		}
	}
	return nil, fmt.Errorf("no available port in range %s", portRange)
}

func (s *service) handleListModels(w http.ResponseWriter, r *http.Request) {
	providerRuntime, err := s.requestedProvider(r, "")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if isClaudeProviderType(providerRuntime.Type) {
		models := []map[string]any{
			{"id": "claude-opus-4-6", "name": "Claude Opus 4.6"},
			{"id": "claude-sonnet-4-5", "name": "Claude Sonnet 4.5"},
			{"id": "claude-haiku-4-5", "name": "Claude Haiku 4.5"},
		}
		if strings.TrimSpace(providerRuntime.Model) != "" {
			models = append([]map[string]any{{"id": providerRuntime.Model, "name": providerRuntime.Model}}, models...)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"provider": providerRuntime.Name,
			"models":   models,
		})
		return
	}

	models, err := withCopilotClientRetry(s, "list models", func(client copilotClient) ([]copilot.ModelInfo, error) {
		return client.ListModels(r.Context())
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("list models: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"provider": providerRuntime.Name,
		"models":   models,
	})
}

func (s *service) handleListSessions(w http.ResponseWriter, r *http.Request) {
	providerRuntime, err := s.requestedProvider(r, "")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if isClaudeProviderType(providerRuntime.Type) {
		writeJSON(w, http.StatusOK, listSessionsResponse{
			Provider:  providerRuntime.Name,
			Persisted: []copilot.SessionMetadata{},
			Live:      s.liveSessionSummaries(providerRuntime.Name),
		})
		return
	}

	persisted, err := withCopilotClientRetry(s, "list sessions", func(client copilotClient) ([]copilot.SessionMetadata, error) {
		return client.ListSessions(r.Context(), nil)
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("list sessions: %v", err))
		return
	}

	persisted = enrichPersistedSessionsFromWorkspace(persisted, defaultSessionStateDir())
	live := s.liveSessionSummaries(providerRuntime.Name)
	writeJSON(w, http.StatusOK, listSessionsResponse{
		Provider:  providerRuntime.Name,
		Persisted: persisted,
		Live:      live,
	})
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

	// Pre-populate the session name from persisted metadata so it's available
	// immediately in the statusline without waiting for the next turn.
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
	}

	s.storeManagedSession(managed)
	action := "created"
	if req.Resume {
		action = "resumed"
	}
	logInfof("session %s %s provider=%s wd=%s model=%s mode=%s streaming=%t", req.SessionID, action, providerRuntime.Name, workingDirectory, managed.model, req.PermissionMode, streaming)
	writeJSON(w, http.StatusCreated, managed.summary())
}

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
	if isClaudeProviderType(providerRuntime.Type) {
		managed.model = model
		evt := map[string]any{"sessionId": managed.id(), "provider": managed.provider, "model": model}
		if req.ReasoningEffort != "" {
			evt["reasoningEffort"] = req.ReasoningEffort
		}
		managed.broadcastHostEvent("host.model_changed", evt)
		writeJSON(w, http.StatusOK, managed.summary())
		return
	}

	var setOpts *copilot.SetModelOptions
	if re := strings.TrimSpace(req.ReasoningEffort); re != "" {
		setOpts = &copilot.SetModelOptions{ReasoningEffort: &re}
	}
	if err := managed.session.SetModel(r.Context(), model, setOpts); err != nil {
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

	if s.isClaudeProviderName(managed.provider) {
		rawEvents := managed.claudeEventHistorySnapshot()
		events := make([]map[string]any, 0, len(rawEvents))
		for _, raw := range rawEvents {
			var parsed map[string]any
			if err := json.Unmarshal(raw, &parsed); err != nil {
				continue
			}
			events = append(events, parsed)
		}
		writeJSON(w, http.StatusOK, map[string]any{"provider": managed.provider, "events": events})
		return
	}

	events, err := managed.session.GetMessages(r.Context())
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

	if s.isClaudeProviderName(managed.provider) {
		writeJSON(w, http.StatusOK, map[string]any{"provider": managed.provider, "tasks": []backgroundTaskView{}})
		return
	}

	events, err := managed.session.GetMessages(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("get tasks: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"provider": managed.provider, "tasks": extractBackgroundTasks(events)})
}

func (s *service) handleAbortSession(w http.ResponseWriter, r *http.Request) {
	managed, ok := s.requireManagedSession(w, r)
	if !ok {
		return
	}
	if s.isClaudeProviderName(managed.provider) {
		aborted := managed.abortClaudeTurn()
		if aborted {
			managed.broadcastHostEvent("host.turn_aborted", map[string]any{"sessionId": managed.id(), "provider": managed.provider})
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "aborted": aborted})
		return
	}
	if err := managed.session.Abort(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("abort: %v", err))
		return
	}
	managed.broadcastHostEvent("host.turn_aborted", map[string]any{"sessionId": r.PathValue("id"), "provider": managed.provider})
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
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

	if s.isClaudeProviderName(managed.provider) {
		turnCtx, cancel, beginErr := managed.beginClaudeTurn()
		if beginErr != nil {
			writeError(w, http.StatusConflict, beginErr.Error())
			return
		}
		messageID := fmt.Sprintf("claude-msg-%d", time.Now().UnixNano())
		managed.broadcastClaudeEvent("assistant.turn_start", map[string]any{"messageId": messageID}, messageID)
		go s.runClaudeQuery(managed, turnCtx, cancel, req.Prompt, req.Attachments, req.RequestHeaders, messageID)
		logInfof("send message session=%s provider=%s prompt_chars=%d attachments=%d message_id=%s", managed.id(), managed.provider, len(req.Prompt), len(req.Attachments), messageID)
		writeJSON(w, http.StatusAccepted, map[string]any{"sessionId": managed.id(), "provider": managed.provider, "messageId": messageID})
		return
	}

	messageID, err := managed.session.Send(r.Context(), copilot.MessageOptions{
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
	if s.isClaudeProviderName(managed.provider) {
		writeError(w, http.StatusNotImplemented, "compact history is not yet supported with provider=claude")
		return
	}

	var req compactHistoryRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := managed.session.RPC.History.Compact(r.Context())
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
	if s.isClaudeProviderName(managed.provider) {
		writeError(w, http.StatusNotImplemented, "fleet mode is not yet supported with provider=claude")
		return
	}

	var req fleetStartRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var params *rpc.FleetStartRequest
	if prompt := strings.TrimSpace(req.Prompt); prompt != "" {
		params = &rpc.FleetStartRequest{Prompt: &prompt}
	}

	result, err := managed.session.RPC.Fleet.Start(r.Context(), params)
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

	if s.isClaudeProviderName(managed.provider) {
		managed.agentMode = req.Mode
		managed.broadcastHostEvent("host.agent_mode_changed", map[string]any{
			"sessionId": managed.id(),
			"provider":  managed.provider,
			"mode":      req.Mode,
		})
		writeJSON(w, http.StatusOK, map[string]any{"sessionId": managed.id(), "provider": managed.provider, "mode": req.Mode})
		return
	}

	if _, err := managed.session.RPC.Mode.Set(r.Context(), &rpc.ModeSetRequest{Mode: sdkMode}); err != nil {
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
		rawPayload, skip, err := trimReplayPayload(rawPayload, string(event.Type), summaryMode, previewChars)
		if err != nil || skip {
			continue
		}

		nextSequence++
		var messageChunkIndex *uint64
		if messageID != "" {
			messageChunkIndexes[messageID]++
			index := messageChunkIndexes[messageID]
			messageChunkIndex = &index
		}

		payload, err := enrichSessionEventPayload(rawPayload, nextSequence, messageChunkIndex)
		if err != nil {
			continue
		}
		payloads = append(payloads, payload)
	}

	m.hydrateSessionEventSequenceState(nextSequence, messageChunkIndexes)
	return payloads, len(payloads)
}

func (m *managedSession) marshalSequencedSessionEvent(event copilot.SessionEvent) ([]byte, error) {
	rawPayload, messageID, err := marshalSessionEventPayload(event)
	if err != nil {
		return nil, err
	}

	sequenceID, messageChunkIndex := m.nextSessionEventMetadata(messageID)
	return enrichSessionEventPayload(rawPayload, sequenceID, messageChunkIndex)
}

func (m *managedSession) nextSessionEventMetadata(messageID string) (uint64, *uint64) {
	m.eventSequenceMu.Lock()
	defer m.eventSequenceMu.Unlock()

	m.nextEventSequence++
	sequenceID := m.nextEventSequence

	if messageID == "" {
		return sequenceID, nil
	}
	if m.messageChunkIndexes == nil {
		m.messageChunkIndexes = make(map[string]uint64)
	}

	m.messageChunkIndexes[messageID]++
	index := m.messageChunkIndexes[messageID]
	return sequenceID, &index
}

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

func (m *managedSession) id() string {
	if m == nil {
		return ""
	}
	if m.session != nil && strings.TrimSpace(m.session.SessionID) != "" {
		return m.session.SessionID
	}
	return strings.TrimSpace(m.sessionID)
}

func (m *managedSession) appendClaudeEvent(payload []byte) {
	if len(payload) == 0 {
		return
	}
	m.claudeEventHistoryMu.Lock()
	m.claudeEventHistory = append(m.claudeEventHistory, json.RawMessage(payload))
	m.claudeEventHistoryMu.Unlock()
}

func (m *managedSession) claudeEventHistorySnapshot() []json.RawMessage {
	m.claudeEventHistoryMu.RLock()
	defer m.claudeEventHistoryMu.RUnlock()
	out := make([]json.RawMessage, len(m.claudeEventHistory))
	copy(out, m.claudeEventHistory)
	return out
}

func (m *managedSession) claudeMessageHistorySnapshot() []anthropic.MessageParam {
	m.claudeMessagesMu.RLock()
	defer m.claudeMessagesMu.RUnlock()
	out := make([]anthropic.MessageParam, len(m.claudeMessages))
	copy(out, m.claudeMessages)
	return out
}

func (m *managedSession) appendClaudeMessages(messages ...anthropic.MessageParam) {
	if len(messages) == 0 {
		return
	}
	m.claudeMessagesMu.Lock()
	m.claudeMessages = append(m.claudeMessages, messages...)
	m.claudeMessagesMu.Unlock()
}

func (m *managedSession) broadcastClaudeEvent(eventType string, data map[string]any, messageID string) {
	if data == nil {
		data = map[string]any{}
	}

	payload := map[string]any{
		"type":      eventType,
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"data":      data,
	}
	sequenceID, messageChunkIndex := m.nextSessionEventMetadata(messageID)
	payload["sequenceId"] = sequenceID
	if messageChunkIndex != nil {
		payload["messageChunkIndex"] = *messageChunkIndex
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		logWarnf("marshal claude session event failed session=%s event=%s: %v", m.id(), eventType, err)
		return
	}

	m.appendClaudeEvent(encoded)
	m.broadcast(sseMessage{Event: "session.event", Data: encoded})
}

func (m *managedSession) setClaudeTurnCancel(cancel context.CancelFunc) {
	m.claudeTurnMu.Lock()
	m.claudeTurnCancel = cancel
	m.claudeTurnMu.Unlock()
}

func (m *managedSession) clearClaudeTurnCancel() {
	m.claudeTurnMu.Lock()
	m.claudeTurnCancel = nil
	m.claudeTurnMu.Unlock()
}

func (m *managedSession) abortClaudeTurn() bool {
	m.claudeTurnMu.Lock()
	cancel := m.claudeTurnCancel
	m.claudeTurnCancel = nil
	m.claudeTurnMu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

func (m *managedSession) beginClaudeTurn() (context.Context, context.CancelFunc, error) {
	m.claudeTurnMu.Lock()
	defer m.claudeTurnMu.Unlock()
	if m.claudeTurnCancel != nil {
		return nil, nil, errors.New("claude turn already in progress")
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.claudeTurnCancel = cancel
	return ctx, cancel, nil
}

func anthropicClientOptionsFromManaged(managed *managedSession) ([]option.RequestOption, error) {
	if managed == nil {
		return nil, errors.New("claude session is not initialized")
	}

	apiKeyEnv := strings.TrimSpace(managed.claudeAPIKeyEnv)
	if apiKeyEnv == "" {
		apiKeyEnv = "ANTHROPIC_API_KEY"
	}
	authTokenEnv := strings.TrimSpace(managed.claudeAuthTokenEnv)
	if authTokenEnv == "" {
		authTokenEnv = "ANTHROPIC_AUTH_TOKEN"
	}

	apiKey := strings.TrimSpace(os.Getenv(apiKeyEnv))
	authToken := strings.TrimSpace(os.Getenv(authTokenEnv))
	if apiKey == "" && authToken == "" {
		return nil, fmt.Errorf("missing Anthropic credentials: set %s or %s", apiKeyEnv, authTokenEnv)
	}

	options := make([]option.RequestOption, 0, 3)
	if apiKey != "" {
		options = append(options, option.WithAPIKey(apiKey))
	}
	if authToken != "" {
		options = append(options, option.WithAuthToken(authToken))
	}
	if baseURL := strings.TrimSpace(managed.claudeBaseURL); baseURL != "" {
		options = append(options, option.WithBaseURL(baseURL))
	}
	return options, nil
}

func anthropicRequestOptionsFromHeaders(headers map[string]string) []option.RequestOption {
	if len(headers) == 0 {
		return nil
	}

	options := make([]option.RequestOption, 0, len(headers))
	for key, value := range headers {
		headerName := strings.TrimSpace(key)
		if headerName == "" {
			continue
		}
		lowerHeader := strings.ToLower(headerName)
		if lowerHeader == "content-length" || lowerHeader == "host" {
			continue
		}
		options = append(options, option.WithHeader(headerName, strings.TrimSpace(value)))
	}
	return options
}

func buildClaudeUserMessage(prompt string, attachments []copilot.Attachment, workingDirectory string) (anthropic.MessageParam, error) {
	blocks := make([]anthropic.ContentBlockParamUnion, 0, 1+len(attachments))
	if strings.TrimSpace(prompt) != "" {
		blocks = append(blocks, anthropic.NewTextBlock(prompt))
	}

	for idx, attachment := range attachments {
		attachmentBlocks, err := claudeAttachmentBlocks(attachment, workingDirectory)
		if err != nil {
			return anthropic.MessageParam{}, fmt.Errorf("attachment %d: %w", idx+1, err)
		}
		blocks = append(blocks, attachmentBlocks...)
	}

	if len(blocks) == 0 {
		return anthropic.MessageParam{}, errors.New("prompt or attachments are required")
	}
	return anthropic.NewUserMessage(blocks...), nil
}

func claudeAttachmentBlocks(attachment copilot.Attachment, workingDirectory string) ([]anthropic.ContentBlockParamUnion, error) {
	switch attachment.Type {
	case copilot.UserMessageAttachmentTypeSelection:
		block, err := claudeSelectionAttachmentBlock(attachment)
		if err != nil {
			return nil, err
		}
		return []anthropic.ContentBlockParamUnion{block}, nil
	case copilot.UserMessageAttachmentTypeDirectory:
		path := firstNonEmpty(stringOrEmpty(attachment.Path), stringOrEmpty(attachment.FilePath))
		resolvedPath, err := resolveClaudeAttachmentPath(path, workingDirectory)
		if err != nil {
			return nil, err
		}
		block, err := claudeDirectoryAttachmentBlock(resolvedPath, attachment)
		if err != nil {
			return nil, err
		}
		return []anthropic.ContentBlockParamUnion{block}, nil
	case copilot.UserMessageAttachmentTypeGithubReference:
		return nil, errors.New("github reference attachments are not supported with provider=claude")
	case copilot.UserMessageAttachmentTypeBlob:
		return claudeBlobAttachmentBlocks(attachment)
	default:
		path := firstNonEmpty(stringOrEmpty(attachment.Path), stringOrEmpty(attachment.FilePath))
		if path == "" && strings.TrimSpace(stringOrEmpty(attachment.Data)) != "" {
			return claudeBlobAttachmentBlocks(attachment)
		}
		return claudePathAttachmentBlocks(path, attachment, workingDirectory)
	}
}

func resolveClaudeAttachmentPath(rawPath, workingDirectory string) (string, error) {
	path := strings.TrimSpace(rawPath)
	if path == "" {
		return "", errors.New("attachment path is empty")
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}

	base := strings.TrimSpace(workingDirectory)
	if base == "" {
		return "", fmt.Errorf("relative attachment path %q requires a working directory", path)
	}
	return filepath.Clean(filepath.Join(base, path)), nil
}

func claudePathAttachmentBlocks(path string, attachment copilot.Attachment, workingDirectory string) ([]anthropic.ContentBlockParamUnion, error) {
	resolvedPath, err := resolveClaudeAttachmentPath(path, workingDirectory)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(resolvedPath)
	if err != nil {
		return nil, fmt.Errorf("read attachment %q: %w", resolvedPath, err)
	}
	if info.IsDir() {
		block, dirErr := claudeDirectoryAttachmentBlock(resolvedPath, attachment)
		if dirErr != nil {
			return nil, dirErr
		}
		return []anthropic.ContentBlockParamUnion{block}, nil
	}
	if info.Size() > claudeAttachmentMaxBytes {
		return nil, fmt.Errorf("attachment %q exceeds %d bytes", resolvedPath, claudeAttachmentMaxBytes)
	}

	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		return nil, fmt.Errorf("read attachment %q: %w", resolvedPath, err)
	}

	name := firstNonEmpty(stringOrEmpty(attachment.DisplayName), filepath.Base(resolvedPath))
	mimeType := detectAttachmentMIME(data, stringOrEmpty(attachment.MIMEType))
	return claudeBytesAttachmentBlocks(data, mimeType, name, resolvedPath)
}

func claudeBlobAttachmentBlocks(attachment copilot.Attachment) ([]anthropic.ContentBlockParamUnion, error) {
	if text := strings.TrimSpace(stringOrEmpty(attachment.Text)); text != "" {
		content := clampTextAttachment(text, claudeTextAttachmentMaxLen)
		label := attachmentLabel(stringOrEmpty(attachment.DisplayName), "")
		if label != "" {
			content = fmt.Sprintf("Attachment: %s\n\n%s", label, content)
		}
		return []anthropic.ContentBlockParamUnion{
			anthropic.NewDocumentBlock(anthropic.PlainTextSourceParam{Data: content}),
		}, nil
	}

	encoded := strings.TrimSpace(stringOrEmpty(attachment.Data))
	if encoded == "" {
		return nil, errors.New("blob attachment is missing data")
	}
	data, err := decodeBase64Data(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode blob attachment data: %w", err)
	}
	mimeType := detectAttachmentMIME(data, stringOrEmpty(attachment.MIMEType))
	return claudeBytesAttachmentBlocks(data, mimeType, stringOrEmpty(attachment.DisplayName), "")
}

func decodeBase64Data(encoded string) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err == nil {
		return data, nil
	}
	data, rawErr := base64.RawStdEncoding.DecodeString(encoded)
	if rawErr != nil {
		return nil, err
	}
	return data, nil
}

func claudeBytesAttachmentBlocks(data []byte, mimeType, name, sourcePath string) ([]anthropic.ContentBlockParamUnion, error) {
	if len(data) > claudeAttachmentMaxBytes {
		return nil, fmt.Errorf("attachment exceeds %d bytes", claudeAttachmentMaxBytes)
	}

	mimeType = detectAttachmentMIME(data, mimeType)
	label := attachmentLabel(name, sourcePath)
	if label == "" {
		label = "attachment"
	}
	if isAnthropicImageMIMEType(mimeType) {
		blocks := make([]anthropic.ContentBlockParamUnion, 0, 2)
		blocks = append(blocks, anthropic.NewTextBlock("Attached image: "+label))
		blocks = append(blocks, anthropic.NewImageBlockBase64(mimeType, base64.StdEncoding.EncodeToString(data)))
		return blocks, nil
	}

	if mimeType == "application/pdf" {
		blocks := make([]anthropic.ContentBlockParamUnion, 0, 2)
		blocks = append(blocks, anthropic.NewTextBlock("Attached PDF: "+label))
		blocks = append(blocks, anthropic.NewDocumentBlock(anthropic.Base64PDFSourceParam{
			Data: base64.StdEncoding.EncodeToString(data),
		}))
		return blocks, nil
	}

	text, err := textFromAttachmentBytes(data)
	if err != nil {
		return nil, fmt.Errorf("unsupported attachment %q (%s): %w", label, mimeType, err)
	}
	text = clampTextAttachment(text, claudeTextAttachmentMaxLen)
	text = fmt.Sprintf("Attachment: %s\n\n%s", label, text)

	return []anthropic.ContentBlockParamUnion{
		anthropic.NewDocumentBlock(anthropic.PlainTextSourceParam{Data: text}),
	}, nil
}

func detectAttachmentMIME(data []byte, fallback string) string {
	if normalized := normalizeMIMEType(fallback); normalized != "" {
		return normalized
	}
	if len(data) == 0 {
		return "text/plain"
	}
	sample := data
	if len(sample) > 512 {
		sample = sample[:512]
	}
	return normalizeMIMEType(http.DetectContentType(sample))
}

func normalizeMIMEType(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		return ""
	}
	if idx := strings.Index(normalized, ";"); idx >= 0 {
		normalized = strings.TrimSpace(normalized[:idx])
	}
	return normalized
}

func isAnthropicImageMIMEType(value string) bool {
	switch normalizeMIMEType(value) {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

func attachmentLabel(name, path string) string {
	return firstNonEmpty(strings.TrimSpace(name), strings.TrimSpace(path))
}

func textFromAttachmentBytes(data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	if bytes.IndexByte(data, 0x00) >= 0 {
		return "", errors.New("binary data contains NUL bytes")
	}
	if !utf8.Valid(data) {
		return "", errors.New("attachment is not valid UTF-8")
	}
	return string(data), nil
}

func clampTextAttachment(text string, maxRunes int) string {
	if maxRunes <= 0 {
		return text
	}
	if utf8.RuneCountInString(text) <= maxRunes {
		return text
	}
	return truncateRunes(text, maxRunes) + "\n\n[Attachment truncated due to size.]"
}

func claudeDirectoryAttachmentBlock(path string, attachment copilot.Attachment) (anthropic.ContentBlockParamUnion, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return anthropic.ContentBlockParamUnion{}, fmt.Errorf("read directory %q: %w", path, err)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	limit := len(entries)
	if limit > claudeDirectoryEntryLimit {
		limit = claudeDirectoryEntryLimit
	}

	lines := make([]string, 0, limit+1)
	for i := 0; i < limit; i++ {
		name := entries[i].Name()
		if entries[i].IsDir() {
			name += "/"
		}
		lines = append(lines, "- "+name)
	}
	if len(entries) > limit {
		lines = append(lines, fmt.Sprintf("- ... (%d more entries)", len(entries)-limit))
	}
	if len(lines) == 0 {
		lines = []string{"(empty directory)"}
	}

	label := attachmentLabel(stringOrEmpty(attachment.DisplayName), path)
	return anthropic.NewTextBlock(fmt.Sprintf("Directory attachment: %s\nPath: %s\nContents:\n%s", label, path, strings.Join(lines, "\n"))), nil
}

func formatAttachmentLineRange(lineRange *copilot.UserMessageAttachmentFileLineRange) string {
	if lineRange == nil {
		return ""
	}

	start := int(math.Round(lineRange.Start))
	end := int(math.Round(lineRange.End))
	if start <= 0 || end <= 0 {
		return ""
	}
	if end < start {
		start, end = end, start
	}
	if start == end {
		return fmt.Sprintf("line %d", start)
	}
	return fmt.Sprintf("lines %d-%d", start, end)
}

func claudeSelectionAttachmentBlock(attachment copilot.Attachment) (anthropic.ContentBlockParamUnion, error) {
	text := strings.TrimSpace(stringOrEmpty(attachment.Text))
	if text == "" {
		return anthropic.ContentBlockParamUnion{}, errors.New("selection attachment text is empty")
	}

	label := attachmentLabel(
		stringOrEmpty(attachment.DisplayName),
		firstNonEmpty(stringOrEmpty(attachment.FilePath), stringOrEmpty(attachment.Path)),
	)
	if lineRange := formatAttachmentLineRange(attachment.LineRange); lineRange != "" {
		if label == "" {
			label = lineRange
		} else {
			label = label + " (" + lineRange + ")"
		}
	}

	content := clampTextAttachment(text, claudeTextAttachmentMaxLen)
	if label != "" {
		content = fmt.Sprintf("Attached code selection: %s\n\n%s", label, content)
	}
	return anthropic.NewTextBlock(content), nil
}

func extractAnthropicTextBlocks(content []anthropic.ContentBlockUnion) string {
	if len(content) == 0 {
		return ""
	}

	parts := make([]string, 0, len(content))
	for _, block := range content {
		if block.Type != "text" {
			continue
		}
		if strings.TrimSpace(block.Text) == "" {
			continue
		}
		parts = append(parts, block.Text)
	}
	return strings.Join(parts, "\n")
}

func (m *managedSession) summary() sessionSummary {
	sessionID := m.id()
	workspacePath := ""
	capabilities := copilot.SessionCapabilities{}
	if m.session != nil {
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

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
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

	if m.session != nil {
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

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		logInfof("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

// recoveryMiddleware catches panics in HTTP handlers so a single bad request
// cannot crash the entire service process.  The panic and stack trace are
// logged before returning 500 to the client.
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
