package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	copilot "github.com/github/copilot-sdk/go"
)

func newClaudeMessageID() string {
	return fmt.Sprintf("claude-msg-%d", time.Now().UnixNano())
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

	apiKey := strings.TrimSpace(managed.claudeAPIKey)
	if apiKey == "" {
		apiKeyEnv := strings.TrimSpace(managed.claudeAPIKeyEnv)
		if apiKeyEnv == "" {
			apiKeyEnv = "ANTHROPIC_API_KEY"
		}
		apiKey = strings.TrimSpace(os.Getenv(apiKeyEnv))
	}
	authToken := strings.TrimSpace(managed.claudeAuthToken)
	if authToken == "" {
		authTokenEnv := strings.TrimSpace(managed.claudeAuthTokenEnv)
		if authTokenEnv == "" {
			authTokenEnv = "ANTHROPIC_AUTH_TOKEN"
		}
		authToken = firstNonEmpty(
			strings.TrimSpace(os.Getenv(authTokenEnv)),
			strings.TrimSpace(os.Getenv("ANTHROPIC_BEARER_TOKEN")),
		)
	}
	if apiKey == "" && authToken == "" {
		return nil, fmt.Errorf("missing Anthropic credentials: set ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN")
	}

	options := make([]option.RequestOption, 0, 3)
	if apiKey != "" {
		options = append(options, option.WithAPIKey(apiKey))
	} else if authToken != "" {
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
