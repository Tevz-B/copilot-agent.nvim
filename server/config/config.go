package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	ProviderCopilot = "copilot"
	ProviderClaude  = "claude"
)

type ProviderFileConfig struct {
	Default         string              `yaml:"default"`
	DefaultProvider string              `yaml:"default_provider"`
	Providers       []ProviderFileEntry `yaml:"providers"`
}

type ProviderFileEntry struct {
	Name   string                 `yaml:"name"`
	Type   string                 `yaml:"type"`
	Model  string                 `yaml:"model"`
	Claude *ProviderClaudeOptions `yaml:"claude"`
}

type ProviderClaudeOptions struct {
	APIKeyEnv    string `yaml:"api_key_env"`
	AuthTokenEnv string `yaml:"auth_token_env"`
	BaseURL      string `yaml:"base_url"`
}

type ProviderRuntime struct {
	Name               string
	Type               string
	Model              string
	ClaudeAPIKeyEnv    string
	ClaudeAuthTokenEnv string
	ClaudeBaseURL      string
}

func NormalizeProvider(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", ProviderCopilot:
		return ProviderCopilot, nil
	case ProviderClaude:
		return ProviderClaude, nil
	default:
		return "", fmt.Errorf("provider must be one of: %s, %s", ProviderCopilot, ProviderClaude)
	}
}

func ProviderKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func IsCopilotProviderType(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), ProviderCopilot)
}

func IsClaudeProviderType(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), ProviderClaude)
}

func DefaultClaudeAPIKeyEnv(options *ProviderClaudeOptions) string {
	if options != nil && strings.TrimSpace(options.APIKeyEnv) != "" {
		return strings.TrimSpace(options.APIKeyEnv)
	}
	return "ANTHROPIC_API_KEY"
}

func DefaultClaudeAuthTokenEnv(options *ProviderClaudeOptions) string {
	if options != nil && strings.TrimSpace(options.AuthTokenEnv) != "" {
		return strings.TrimSpace(options.AuthTokenEnv)
	}
	return "ANTHROPIC_AUTH_TOKEN"
}

func ProviderFromFlags(defaultProviderType, defaultModel string) (map[string]ProviderRuntime, []string, string, error) {
	ptype, err := NormalizeProvider(defaultProviderType)
	if err != nil {
		return nil, nil, "", err
	}
	name := ProviderKey(ptype)
	entries := map[string]ProviderRuntime{
		name: {
			Name:               name,
			Type:               ptype,
			Model:              strings.TrimSpace(defaultModel),
			ClaudeAPIKeyEnv:    DefaultClaudeAPIKeyEnv(nil),
			ClaudeAuthTokenEnv: DefaultClaudeAuthTokenEnv(nil),
		},
	}
	return entries, []string{name}, name, nil
}

func ProviderFromYAML(configPath, defaultProviderType, defaultModel string) (map[string]ProviderRuntime, []string, string, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("read providers config: %w", err)
	}

	var parsed ProviderFileConfig
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		return nil, nil, "", fmt.Errorf("parse providers config: %w", err)
	}
	if len(parsed.Providers) == 0 {
		return nil, nil, "", errors.New("providers config must define at least one provider")
	}

	providers := make(map[string]ProviderRuntime, len(parsed.Providers))
	order := make([]string, 0, len(parsed.Providers))
	for idx, entry := range parsed.Providers {
		name := ProviderKey(entry.Name)
		kind := strings.TrimSpace(entry.Type)
		if kind == "" {
			kind = entry.Name
		}
		ptype, kindErr := NormalizeProvider(kind)
		if kindErr != nil {
			return nil, nil, "", fmt.Errorf("providers[%d] type: %w", idx, kindErr)
		}
		if name == "" {
			name = ProviderKey(ptype)
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
		providers[name] = ProviderRuntime{
			Name:               name,
			Type:               ptype,
			Model:              strings.TrimSpace(entry.Model),
			ClaudeAPIKeyEnv:    DefaultClaudeAPIKeyEnv(entry.Claude),
			ClaudeAuthTokenEnv: DefaultClaudeAuthTokenEnv(entry.Claude),
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

	defaultName := ProviderKey(parsed.DefaultProvider)
	if defaultName == "" {
		defaultName = ProviderKey(parsed.Default)
	}
	flagName := ProviderKey(defaultProviderType)
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

func LoadProviders(configPath, defaultProviderType, defaultModel string) (map[string]ProviderRuntime, []string, string, error) {
	if strings.TrimSpace(configPath) == "" {
		return ProviderFromFlags(defaultProviderType, defaultModel)
	}
	return ProviderFromYAML(configPath, defaultProviderType, defaultModel)
}
