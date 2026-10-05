package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// opencode-go — https://opencode.ai/zen/go/v1
//
// Verified live 2026-10-05:
//   • requires x-opencode-session on /chat/completions and /messages
//     (missing -> 400 MissingSessionID); /models and /usage do NOT require it
//   • serves all three surfaces, but per-model: /messages 400s
//     ModelProtocolUnsupported on glm-5.2 / omen-alpha / kimi-k2.6 while
//     qwen3.8-flash / deepseek-v4.1-flash / minimax-m3 return 200
//   • GET /v1/usage -> {rolling,weekly,monthly}{status,percent,resetsAt}  (percent shape)
//   • responses headers x-opencode-endpoint-id / x-opencode-upstream-model-id
// ─────────────────────────────────────────────────────────────────────────────

type openCodeGo struct{ client *http.Client }

// NewOpenCodeGo builds the opencode-go adapter.
func NewOpenCodeGo(c *http.Client) Adapter { return &openCodeGo{client: c} }

func (a *openCodeGo) Kind() Kind { return KindOpenCodeGo }

// StripHeaders: a client must never pin OUR session or collide with it, so any
// client-supplied opencode session/request headers are dropped and regenerated.
func (a *openCodeGo) StripHeaders() []string {
	return []string{"X-Opencode-Session", "X-Opencode-Request", "X-Zen-Model"}
}

func (a *openCodeGo) PrepareRequest(ctx context.Context, req *http.Request, acct Account, key string, surface Surface) error {
	if key == "" {
		return fmt.Errorf("opencode-go: empty api key")
	}
	// /messages authenticates with x-api-key on this provider (verified), while
	// the OpenAI-shaped surfaces use Bearer. Send both: harmless, and covers
	// either upstream expectation.
	req.Header.Set("Authorization", "Bearer "+key)
	if surface == SurfaceAnthropic {
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	// Session identity is PER REQUEST. A shared long-lived session would grow
	// server-side until it blew context — a real hazard for a router fronting
	// many clients.
	sid := newRequestID()
	if acct.RequiresSessionHeader || surface != SurfaceOpenAI {
		req.Header.Set("x-opencode-session", "ezllm-"+sid)
	}
	req.Header.Set("x-opencode-request", "req-"+sid)
	applyCustomHeaders(req, acct.CustomHeaders)
	return nil
}

func (a *openCodeGo) ListModels(ctx context.Context, acct Account, key string) ([]ModelInfo, error) {
	return listModelsGeneric(ctx, a.client, acct, key, nil)
}

// ReadQuota parses opencode-go's percent-shaped /usage.
func (a *openCodeGo) ReadQuota(ctx context.Context, acct Account, key string) (Quota, error) {
	body, err := getJSON(ctx, a.client, acct.BaseURL+"/usage", map[string]string{
		"Authorization": "Bearer " + key,
	})
	if err != nil {
		return Quota{}, err
	}
	if q := parsePercentQuota(string(body)); q.Kind != "" {
		return q, nil
	}
	// Fall back to generic classification in case an opencode-shaped gateway
	// starts reporting money instead of percentages.
	if q := ClassifyQuota(string(body)); q.Kind != "" && q.Kind != QuotaNone {
		return q, nil
	}
	return Quota{}, ErrQuotaUnsupported
}

// ─────────────────────────────────────────────────────────────────────────────
// openai-compatible — any /v1 server speaking OpenAI chat completions
// (covers ssn-gpt = space.stationine.com, ollama-cloud, etc.)
// ─────────────────────────────────────────────────────────────────────────────

type openAICompatible struct{ client *http.Client }

func NewOpenAICompatible(c *http.Client) Adapter { return &openAICompatible{client: c} }

func (a *openAICompatible) Kind() Kind { return KindOpenAICompatible }

func (a *openAICompatible) StripHeaders() []string { return nil }

func (a *openAICompatible) PrepareRequest(_ context.Context, req *http.Request, acct Account, key string, surface Surface) error {
	if key == "" {
		return fmt.Errorf("openai-compatible: empty api key")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if surface == SurfaceAnthropic {
		// Some gateways expose /v1/messages with x-api-key even when the rest is
		// OpenAI-shaped. Sending both is harmless and widens compatibility.
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	applyCustomHeaders(req, acct.CustomHeaders)
	return nil
}

func (a *openAICompatible) ListModels(ctx context.Context, acct Account, key string) ([]ModelInfo, error) {
	return listModelsGeneric(ctx, a.client, acct, key, map[string]string{
		"Authorization": "Bearer " + key,
	})
}

// ReadQuota classifies whatever shape the gateway exposes. Verified against
// ssn-gpt (space.stationine.com): balance/remaining in USD plus usage.today and
// usage.total cost fields. Providers with no quota endpoint yield
// ErrQuotaUnsupported, which the UI renders as "measured" (our own numbers).
func (a *openAICompatible) ReadQuota(ctx context.Context, acct Account, key string) (Quota, error) {
	body, err := getJSON(ctx, a.client, acct.BaseURL+"/usage", map[string]string{
		"Authorization": "Bearer " + key,
	})
	if err != nil {
		return Quota{}, err
	}
	if q := ClassifyQuota(string(body)); q.Kind != "" && q.Kind != QuotaNone {
		return q, nil
	}
	return Quota{}, ErrQuotaUnsupported
}

// ─────────────────────────────────────────────────────────────────────────────
// anthropic-compatible — /v1/messages with x-api-key + anthropic-version
// (its own kind because the AUTH HEADER SHAPE differs, not just the path)
// ─────────────────────────────────────────────────────────────────────────────

type anthropicCompatible struct{ client *http.Client }

func NewAnthropicCompatible(c *http.Client) Adapter { return &anthropicCompatible{client: c} }

func (a *anthropicCompatible) Kind() Kind { return KindAnthropicCompat }

func (a *anthropicCompatible) StripHeaders() []string { return []string{"Authorization"} }

func (a *anthropicCompatible) PrepareRequest(_ context.Context, req *http.Request, acct Account, key string, _ Surface) error {
	if key == "" {
		return fmt.Errorf("anthropic-compatible: empty api key")
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	// Deliberately no Authorization header: Anthropic-shaped endpoints reject
	// requests carrying a Bearer they don't recognise, and StripHeaders removes
	// any client-supplied one.
	applyCustomHeaders(req, acct.CustomHeaders)
	return nil
}

// ListModels: Anthropic's own catalog endpoint. Not all compatible gateways
// implement it, so a 404/405 degrades to an empty catalog rather than an error —
// registration can still proceed via an explicit model list.
func (a *anthropicCompatible) ListModels(ctx context.Context, acct Account, key string) ([]ModelInfo, error) {
	body, status, err := getJSONStatus(ctx, a.client, acct.BaseURL+"/models", map[string]string{
		"x-api-key":         key,
		"anthropic-version": "2023-06-01",
	})
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		return nil, nil
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, nil
	}
	return parseModelList(raw), nil
}

func (a *anthropicCompatible) ReadQuota(ctx context.Context, acct Account, key string) (Quota, error) {
	// No standard Anthropic quota endpoint; usage arrives in rate-limit headers
	// on real calls instead. Report unsupported so the UI shows "measured".
	return Quota{}, ErrQuotaUnsupported
}

// ─────────────────────────────────────────────────────────────────────────────
// shared helpers
// ─────────────────────────────────────────────────────────────────────────────

func applyCustomHeaders(req *http.Request, hdrs map[string]string) {
	for k, v := range hdrs {
		if k == "" || v == "" {
			continue
		}
		req.Header.Set(k, v)
	}
}

func getJSON(ctx context.Context, c *http.Client, url string, hdrs map[string]string) ([]byte, error) {
	b, _, err := getJSONStatus(ctx, c, url, hdrs)
	return b, err
}

func getJSONStatus(ctx context.Context, c *http.Client, url string, hdrs map[string]string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range hdrs {
		if k != "" && v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode != http.StatusOK {
		return body, resp.StatusCode, fmt.Errorf("provider: %s -> HTTP %d: %s",
			url, resp.StatusCode, truncate(string(body), 200))
	}
	return body, resp.StatusCode, nil
}

func listModelsGeneric(ctx context.Context, c *http.Client, acct Account, key string, hdrs map[string]string) ([]ModelInfo, error) {
	if hdrs == nil {
		hdrs = map[string]string{}
		if key != "" {
			hdrs["Authorization"] = "Bearer " + key
		}
	}
	body, err := getJSON(ctx, c, strings.TrimSuffix(acct.BaseURL, "/")+"/models", hdrs)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("provider: parse models: %w", err)
	}
	return parseModelList(raw), nil
}

func parseModelList(raw map[string]any) []ModelInfo {
	arr, ok := raw["data"].([]any)
	if !ok {
		return nil
	}
	out := make([]ModelInfo, 0, len(arr))
	for _, it := range arr {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["id"].(string)
		if id == "" {
			continue
		}
		mi := ModelInfo{ID: id}
		mi.OwnedBy, _ = m["owned_by"].(string)
		if c, ok := m["created"].(float64); ok {
			mi.Created = int64(c)
		}
		if d, ok := m["display_name"].(string); ok {
			mi.Display = d
		}
		out = append(out, mi)
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// newRequestID returns a short random id for session/request headers.
func newRequestID() string {
	var b [8]byte
	if _, err := io.ReadFull(randReader, b[:]); err != nil {
		// Extremely unlikely; fall back to a timestamp so we never send an
		// empty session header (which the upstream rejects with a 400).
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b)
}
