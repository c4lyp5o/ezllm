package githubcopilot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Endpoint routing (omniroute buildUrl): claude* → the Anthropic-native
// /v1/messages shim; openai-responses target or *codex* → /responses;
// everything else → /chat/completions. Gemini/Claude models must NEVER go
// to /responses (they 400 with unsupported_api_for_model).
func endpointForModel(model, targetFormat string) string {
	m := strings.ToLower(model)
	isClaude := strings.Contains(m, "claude")
	if isClaude {
		return copilotAPIBase + "/v1/messages"
	}
	if (targetFormat == "openai-responses" || strings.Contains(m, "codex")) && !strings.Contains(m, "gemini") && !isClaude {
		return copilotAPIBase + "/responses"
	}
	return copilotAPIBase + "/chat/completions"
}

// ChatHeaders builds the request header set (omniroute
// getGitHubCopilotChatHeaders + buildHeaders).
func ChatHeaders(cfg Config, copilotToken string, stream bool) http.Header {
	cfg = cfg.withDefaults()
	h := http.Header{}
	h.Set("Authorization", "Bearer "+copilotToken)
	h.Set("copilot-integration-id", cfg.Integration)
	h.Set("editor-version", "copilot/"+cfg.CLIVersion)
	h.Set("user-agent", "copilot/"+cfg.CLIVersion)
	h.Set("openai-intent", "conversation-agent")
	h.Set("x-interaction-type", "conversation-user")
	h.Set("copilot-harness-id", "copilot-sdk")
	h.Set("x-github-api-version", apiVersion)
	h.Set("X-Initiator", "user")
	if stream {
		h.Set("Accept", "text/event-stream")
		h.Set("x-stainless-helper-method", "stream")
	} else {
		h.Set("Accept", "application/json")
	}
	h.Set("Content-Type", "application/json")
	return h
}

// ModelInfo is one discovered model (GET /models on Copilot).
type ModelInfo struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name,omitempty"`
	Vendor             string   `json:"vendor,omitempty"`
	SupportedEndpoints []string `json:"supported_endpoints,omitempty"`
	ModelPickerEnabled bool     `json:"model_picker_enabled"`
	Policy             struct {
		State string `json:"state"`
	} `json:"policy"`
	Capabilities struct {
		Type string `json:"type"`
	} `json:"capabilities"`
}

// ListModels fetches the account Copilot catalog through the minted token.
// Filters: capabilities.type == "chat" (when present), policy.state ==
// "enabled" (when present), model_picker_enabled != false.
func ListModels(ctx context.Context, cfg Config, copilotToken string) ([]ModelInfo, error) {
	cfg = cfg.withDefaults()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, copilotAPIBase+"/models", nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range ChatHeaders(cfg, copilotToken, false) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := cfg.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("githubcopilot: /models: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Data   []map[string]any `json:"data"`
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("githubcopilot: /models: %w", err)
	}
	entries := out.Data
	if len(entries) == 0 {
		entries = out.Models
	}
	var models []ModelInfo
	for _, e := range entries {
		if picker, ok := e["model_picker_enabled"].(bool); ok && !picker {
			continue
		}
		if pol, ok := e["policy"].(map[string]any); ok {
			if state, ok := pol["state"].(string); ok && state != "" && state != "enabled" {
				continue
			}
		}
		if caps, ok := e["capabilities"].(map[string]any); ok {
			if t, ok := caps["type"].(string); ok && t != "" && t != "chat" {
				continue
			}
		}
		id, _ := e["id"].(string)
		if id == "" {
			id, _ = e["model"].(string)
		}
		if id == "" {
			continue
		}
		m := ModelInfo{ID: id, ModelPickerEnabled: true}
		if name, ok := e["name"].(string); ok {
			m.Name = name
		}
		if eps, ok := e["supported_endpoints"].([]any); ok {
			for _, ep := range eps {
				if s, ok := ep.(string); ok {
					m.SupportedEndpoints = append(m.SupportedEndpoints, s)
				}
			}
		}
		models = append(models, m)
	}
	if len(models) == 0 {
		// Bootstrap fallback (omniroute's curated set) — documented as
		// fallback, never truth; live /models wins.
		for _, id := range fallbackModelIDs {
			models = append(models, ModelInfo{ID: id})
		}
	}
	return models, nil
}

// fallbackModelIDs mirrors omniroute's registry fallback so a /models outage
// degrades to a known set instead of an empty catalog.
var fallbackModelIDs = []string{
	"claude-fable-5", "claude-opus-5", "claude-opus-4.8-fast", "claude-opus-4.8",
	"claude-opus-4.7", "claude-opus-4.6", "claude-sonnet-4.6", "claude-opus-4.5",
	"claude-sonnet-5", "claude-sonnet-4.5", "claude-haiku-4.5",
	"gemini-3.1-pro-preview", "gemini-3.7-flash", "gemini-3.6-flash", "gemini-3.5-flash",
	"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "gpt-5.4",
	"gpt-5.4-mini", "gpt-5.4-nano", "gpt-5.3-codex", "gpt-5-mini",
	"gpt-4o-2024-11-20", "gpt-4o-mini", "gpt-4-0125-preview",
	"kimi-k2.7-code", "mai-code-1-flash", "mai-code-1.1-flash", "mai-code-1-flash-picker",
	"grok-4.6", "grok-4.5", "oswe-vscode-prime",
}

// PrepareBody applies the per-model quirks that keep upstream from 400ing
// (omniroute transformRequest, each ported with its reference):
//   - tools capped at 128
//   - gpt-5.4*: strip temperature ("Unsupported parameter")
//   - assistant messages: strip reasoning_text/reasoning_content
//   - claude models: none of the /chat/completions-only quirks apply
func PrepareBody(model string, body map[string]any) {
	m := strings.ToLower(model)
	if tools, ok := body["tools"].([]any); ok && len(tools) > 128 {
		body["tools"] = tools[:128]
	}
	if strings.Contains(m, "gpt-5.4") {
		delete(body, "temperature")
	}
	if msgs, ok := body["messages"].([]any); ok {
		for i, mm := range msgs {
			msg, ok := mm.(map[string]any)
			if !ok || msg["role"] != "assistant" {
				continue
			}
			if _, has := msg["reasoning_text"]; has {
				delete(msg, "reasoning_text")
			}
			if _, has := msg["reasoning_content"]; has {
				delete(msg, "reasoning_content")
			}
			msgs[i] = msg
		}
	}
	if !strings.Contains(m, "claude") {
		if msgs, ok := body["messages"].([]any); ok {
			body["messages"] = sanitizeChatMessages(msgs)
		}
		// Trailing assistant prefill: Copilot /chat/completions rejects a
		// conversation ending with an assistant message.
		if msgs, ok := body["messages"].([]any); ok && len(msgs) > 1 {
			end := len(msgs)
			for end > 1 {
				last, ok := msgs[end-1].(map[string]any)
				if !ok || last["role"] != "assistant" {
					break
				}
				end--
			}
			if end < len(msgs) {
				body["messages"] = msgs[:end]
			}
		}
	}
}

// sanitizeChatMessages: /chat/completions accepts only text/image_url parts;
// serialize unknown Anthropic-ish parts to text rather than dropping them.
func sanitizeChatMessages(msgs []any) []any {
	out := make([]any, len(msgs))
	for i, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			out[i] = mm
			continue
		}
		content, ok := msg["content"].([]any)
		if !ok {
			out[i] = msg
			continue
		}
		var clean []any
		for _, part := range content {
			p, ok := part.(map[string]any)
			if !ok {
				continue
			}
			switch p["type"] {
			case "text", "image_url":
				clean = append(clean, p)
			default:
				text := jsonText(p["text"])
				if text == "" {
					text = jsonText(p["thinking"])
				}
				if text == "" {
					text = jsonText(p["content"])
				}
				if text == "" {
					if b, err := json.Marshal(p); err == nil {
						text = string(b)
					}
				}
				if text != "" {
					clean = append(clean, map[string]any{"type": "text", "text": text})
				}
			}
		}
		msg["content"] = clean
		out[i] = msg
	}
	return out
}

func jsonText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// NeedsRefresh reports whether the Copilot token must be re-minted (absent,
// or within 5 minutes of expiry).
func NeedsRefresh(s Session, now time.Time) bool {
	if s.CopilotToken == "" {
		return true
	}
	if s.CopilotExpiresAt.IsZero() {
		return true
	}
	return s.CopilotExpiresAt.Sub(now) < 5*time.Minute
}

// EnsureSession returns a session with a fresh Copilot token, minting from
// the GitHub token first and falling back to the refresh-token grant
// (omniroute's refreshCredentials chain).
func EnsureSession(ctx context.Context, cfg Config, s Session, now time.Time) (Session, error) {
	cfg = cfg.withDefaults()
	if !NeedsRefresh(s, now) {
		return s, nil
	}
	token, exp, err := MintCopilotToken(ctx, cfg, s.AccessToken)
	if err == nil {
		s.CopilotToken, s.CopilotExpiresAt = token, exp
		return s, nil
	}
	if s.RefreshToken == "" {
		return s, fmt.Errorf("githubcopilot: copilot token mint failed and no refresh token: %w", err)
	}
	refreshed, rerr := RefreshGitHubToken(ctx, cfg, s.RefreshToken)
	if rerr != nil {
		return s, fmt.Errorf("githubcopilot: refresh failed after mint failure: %v (%w)", rerr, err)
	}
	token, exp, merr := MintCopilotToken(ctx, cfg, refreshed.AccessToken)
	if merr != nil {
		return refreshed, fmt.Errorf("githubcopilot: mint after refresh: %w", merr)
	}
	refreshed.CopilotToken, refreshed.CopilotExpiresAt = token, exp
	return refreshed, nil
}

// DoChat performs one POST against the routed endpoint with the CLI header
// set. The caller owns the body; quirks are applied here.
func DoChat(ctx context.Context, cfg Config, s Session, model, targetFormat string, body map[string]any, stream bool) (*http.Response, error) {
	cfg = cfg.withDefaults()
	// Stealth: drop system messages (SOUL.md/MEMORY.md/USER.md) so the
	// client's persona and personal context never reach a web provider.
	if msgs, ok := body["messages"].([]any); ok {
		filtered := make([]any, 0, len(msgs))
		for _, m := range msgs {
			msg, ok := m.(map[string]any)
			if ok && msg["role"] == "system" {
				continue
			}
			filtered = append(filtered, m)
		}
		body["messages"] = filtered
	}
	PrepareBody(model, body)
	endpoint := endpointForModel(model, targetFormat)
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	for k, vs := range ChatHeaders(cfg, s.CopilotToken, stream) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if strings.Contains(strings.ToLower(model), "claude") {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	return cfg.HTTP.Do(req)
}
