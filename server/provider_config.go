package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	copilot "github.com/github/copilot-sdk/go"
)

func inferredProviderName(provider, model string) string {
	if normalized := providerKey(provider); normalized != "" {
		return normalized
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "claude") {
		return providerClaude
	}
	return providerCopilot
}

func providerConfigForSession(provider string, overrides sessionProviderOverrides) (*copilot.ProviderConfig, string, error) {
	normalized := inferredProviderName(provider, "")
	if normalized != providerClaude {
		return nil, normalized, nil
	}

	baseURL := strings.TrimSpace(overrides.BaseURL)
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL"))
	}
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	apiKey := strings.TrimSpace(overrides.APIKey)
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	}
	bearerToken := strings.TrimSpace(overrides.BearerToken)
	if bearerToken == "" {
		bearerToken = firstNonEmpty(
			strings.TrimSpace(os.Getenv("ANTHROPIC_AUTH_TOKEN")),
			strings.TrimSpace(os.Getenv("ANTHROPIC_BEARER_TOKEN")),
		)
	}

	if apiKey == "" && bearerToken == "" {
		return nil, normalized, errors.New("missing Anthropic credentials: set ANTHROPIC_API_KEY/ANTHROPIC_AUTH_TOKEN (or ANTHROPIC_BEARER_TOKEN) or pass providerApiKey/providerBearerToken")
	}

	return &copilot.ProviderConfig{
		Type:        "anthropic",
		BaseURL:     baseURL,
		APIKey:      apiKey,
		BearerToken: bearerToken,
	}, normalized, nil
}

func decodeListModelsRequest(r *http.Request) (listModelsRequest, error) {
	var req listModelsRequest
	switch r.Method {
	case http.MethodGet:
		query := r.URL.Query()
		req.Provider = query.Get("provider")
		req.ProviderBaseURL = query.Get("providerBaseUrl")
		req.ProviderAPIKey = query.Get("providerApiKey")
		req.ProviderBearerToken = query.Get("providerBearerToken")
	case http.MethodPost:
		if err := decodeJSON(r, &req); err != nil {
			return listModelsRequest{}, err
		}
	default:
		return listModelsRequest{}, fmt.Errorf("unsupported method %s", r.Method)
	}
	return req, nil
}

func (s *service) listModelsForProvider(ctx context.Context, provider string, overrides sessionProviderOverrides) ([]copilot.ModelInfo, error) {
	switch inferredProviderName(provider, "") {
	case providerClaude:
		providerConfig, _, err := providerConfigForSession(providerClaude, overrides)
		if err != nil {
			return nil, err
		}
		return listAnthropicModels(ctx, providerConfig)
	default:
		return withCopilotClientRetry(s, "list models", func(client copilotClient) ([]copilot.ModelInfo, error) {
			return client.ListModels(ctx)
		})
	}
}

type anthropicModelsEnvelope struct {
	Data   []anthropicModelEntry `json:"data"`
	Models []anthropicModelEntry `json:"models"`
}

type anthropicModelEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Type        string `json:"type,omitempty"`
}

func listAnthropicModels(ctx context.Context, providerConfig *copilot.ProviderConfig) ([]copilot.ModelInfo, error) {
	if providerConfig == nil {
		return nil, errors.New("anthropic provider config is required")
	}
	endpoint, err := anthropicModelsURL(providerConfig.BaseURL)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if providerConfig.APIKey != "" {
		req.Header.Set("x-api-key", providerConfig.APIKey)
	} else if providerConfig.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+providerConfig.BearerToken)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = resp.Status
		}
		return nil, fmt.Errorf("provider returned %s: %s", resp.Status, message)
	}

	models, err := decodeAnthropicModels(body)
	if err != nil {
		return nil, err
	}
	return models, nil
}

func anthropicModelsURL(baseURL string) (string, error) {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return "", errors.New("provider base URL is required")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("parse provider base URL: %w", err)
	}
	path := strings.TrimRight(parsed.Path, "/")
	lowerPath := strings.ToLower(path)
	if strings.HasSuffix(lowerPath, "/v1/models") {
		parsed.Path = path
	} else if strings.HasSuffix(lowerPath, "/v1") {
		parsed.Path = path + "/models"
	} else {
		parsed.Path = path + "/v1/models"
	}
	if parsed.Path == "" {
		parsed.Path = "/v1/models"
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func decodeAnthropicModels(body []byte) ([]copilot.ModelInfo, error) {
	var envelope anthropicModelsEnvelope
	if err := json.Unmarshal(body, &envelope); err == nil {
		entries := envelope.Data
		if len(entries) == 0 {
			entries = envelope.Models
		}
		if len(entries) > 0 {
			return anthropicModelInfos(entries), nil
		}
	}

	var entries []anthropicModelEntry
	if err := json.Unmarshal(body, &entries); err == nil && len(entries) > 0 {
		return anthropicModelInfos(entries), nil
	}

	return nil, fmt.Errorf("decode provider models response: unsupported payload %s", strings.TrimSpace(string(body)))
}

func anthropicModelInfos(entries []anthropicModelEntry) []copilot.ModelInfo {
	models := make([]copilot.ModelInfo, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		models = append(models, copilot.ModelInfo{
			ID:   id,
			Name: firstNonEmpty(strings.TrimSpace(entry.DisplayName), strings.TrimSpace(entry.Name), id),
		})
	}
	return models
}
