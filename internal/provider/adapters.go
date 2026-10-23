package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
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

// StripHeaders: X-Zen-Model is opencode's own internal model selector and must
// never come from a client. The SESSION headers are deliberately NOT stripped
// any more (see PrepareRequest): opencode asks clients for a stable session id
// per conversation, and the OpenCode-shaped clients we front already send one.
func (a *openCodeGo) StripHeaders() []string {
	return []string{"X-Zen-Model"}
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
	// Session identity is PRESERVED from the client when it sends one.
	//
	// This reverses an earlier deliberate choice here, which regenerated a random
	// session per request because "a shared session grows server-side until it
	// blew context". That rationale was wrong: the session id is not the
	// conversation state — the client's own message history is — it is the key
	// opencode uses to PIN a conversation to one backend. opencode's Go docs ask
	// clients to "send a stable session ID in x-opencode-session for each
	// conversation so we can optimize routing and prompt caching", and their
	// validated-clients table says Hermes, Claude Code, Codex and Kilo all send a
	// native session header they recognize. Regenerating it per request makes
	// every turn look like a brand-new conversation: no backend pinning, no
	// prompt-cache affinity, and (measured 2026-10-11) routing that degrades into
	// "Endpoint is unavailable" under load. A router must forward that header,
	// not own it.
	//
	// It is still SYNTHESIZED when absent, so a bare curl gets a valid header
	// instead of opencode's 400 MissingSessionID.
	if req.Header.Get("x-opencode-session") == "" {
		req.Header.Set("x-opencode-session", newSessionID())
	}
	if req.Header.Get("x-opencode-request") == "" {
		req.Header.Set("x-opencode-request", "req-"+newRequestID())
	}
	// ezllm identifies ITSELF. opencode asks clients to identify "with its own
	// user agent ... rather than a generic SDK or HTTP-library name", and the
	// client's UA is unsuited to that: it is whatever library the caller used
	// (Python-urllib, Go-http-client), which Cloudflare — in front of opencode —
	// answers with 403 "error code: 1010" (browser-signature ban, measured live
	// 2026-10-11). Setting ours here also means a banned library UA can never
	// reach their edge, whatever client is behind us.
	req.Header.Set("User-Agent", agentUserAgent)
	applyCustomHeaders(req, acct.CustomHeaders)
	return nil
}

// agentUserAgent is how ezllm identifies itself to upstreams that police client
// signatures. Deliberately NOT a browser string and not a library default.
const agentUserAgent = "ezllm/1.0 (llm-router)"

// setAgentUAIfAbsent stamps ezllm's own UA on an OUTBOUND request only when no
// UA is already set — operator custom_headers (applied after) always win.
// Inference keeps the CLIENT's UA (copied upstream before PrepareRequest runs);
// this fills the gap for probes/quota/catalog where there is no client, so a
// library default never reaches a WAF-fronted edge. Measured 2026-10-14: the
// 403/1010 on Groq + Command Code was python-urllib, while Go's own default
// answered 200 on both (consistent with probe N9, "Go was never affected") —
// so this is an operator rule + one less WAF variable, NOT a live fix.
func setAgentUAIfAbsent(req *http.Request) {
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", agentUserAgent)
	}
}

func (a *openCodeGo) ListModels(ctx context.Context, acct Account, key string) ([]ModelInfo, error) {
	return listModelsGeneric(ctx, a.client, acct, key, nil)
}

// AuthOracle: GET /usage proved 401 on a bad key and 200 on a good one, needs
// no session header, costs ~0.75s and zero tokens. It is the gate.
// (N1: GET /models on this provider is PUBLIC — 200 for any string — so it can
// never prove auth.)
func (a *openCodeGo) AuthOracle() AuthProbe {
	return AuthProbe{Method: http.MethodGet, Path: "/usage"}
}

// CatalogIsPublic: verified live with empty/garbage/sk- keys → all 200.
func (a *openCodeGo) CatalogIsPublic() bool { return true }

// ReadQuota parses opencode-go's percent-shaped /usage.
func (a *openCodeGo) ReadQuota(ctx context.Context, acct Account, key string) (Quota, error) {
	body, err := getJSON(ctx, a.client, UpstreamURL(acct.BaseURL, acct.Kind, "/usage"), withCustomHeaders(map[string]string{
		"Authorization": "Bearer " + key,
	}, acct))
	if err != nil {
		return Quota{}, mapUsageErr(err)
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
	setAgentUAIfAbsent(req)
	applyCustomHeaders(req, acct.CustomHeaders)
	return nil
}

func (a *openAICompatible) ListModels(ctx context.Context, acct Account, key string) ([]ModelInfo, error) {
	if isCloudflareAI(acct) {
		return listModelsCloudflare(ctx, a.client, acct, key)
	}
	return listModelsGeneric(ctx, a.client, acct, key, map[string]string{
		"Authorization": "Bearer " + key,
	})
}

// isTokenRouter recognizes TokenRouter by its exact public hostname (the
// isCloudflareAI discipline): the management key must never be sent to a
// host whose URL merely contains the name.
func isTokenRouter(acct Account) bool {
	if acct.Kind != KindOpenAICompatible {
		return false
	}
	u, err := url.Parse(strings.TrimSpace(acct.BaseURL))
	return err == nil && strings.EqualFold(u.Scheme, "https") && strings.EqualFold(u.Hostname(), "api.tokenrouter.com")
}

// isLoopbackBase reports a base URL on this host (127.0.0.0/8, ::1,
// localhost): the in-process subscription bridges, never a remote gateway.
func isLoopbackBase(base string) bool {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isAnthropicAPI(acct Account) bool {
	if acct.Kind != KindAnthropicCompat {
		return false
	}
	u, err := url.Parse(strings.TrimSpace(acct.BaseURL))
	return err == nil && strings.EqualFold(u.Scheme, "https") && strings.EqualFold(u.Hostname(), "api.anthropic.com")
}

func isOpenAI(acct Account) bool {
	if acct.Kind != KindOpenAICompatible {
		return false
	}
	u, err := url.Parse(strings.TrimSpace(acct.BaseURL))
	return err == nil && strings.EqualFold(u.Scheme, "https") && strings.EqualFold(u.Hostname(), "api.openai.com")
}

func isCommandCode(acct Account) bool {
	if acct.Kind != KindOpenAICompatible {
		return false
	}
	u, err := url.Parse(strings.TrimSpace(acct.BaseURL))
	return err == nil && strings.EqualFold(u.Scheme, "https") && strings.EqualFold(u.Hostname(), "api.commandcode.ai")
}

// AuthOracle: GET /usage — verified live on ssn-gpt: 401 INVALID_API_KEY on a
// bad key, 401 API_KEY_REQUIRED with no header, 200 on a good one. Costs zero
// tokens. Some OpenAI-shaped gateways lack /usage entirely; registration treats
// a 404 here as "no oracle" and falls back to the inference step as the auth
// proof (see registration.go), so a provider without /usage is still testable.
func (a *openAICompatible) AuthOracle() AuthProbe {
	return AuthProbe{Method: http.MethodGet, Path: "/usage"}
}

// AuthOracleFor: the account-aware oracle (HostedAuthOracle). Cloudflare's
// cheapest credential check lives OUTSIDE the account-scoped base URL — GET
// https://api.cloudflare.com/client/v4/user/tokens/verify, verified live
// 2026-10-13: 200 + {"status":"active"} on a real token, 401 "Invalid API
// Token" on a well-formed cfut_ fake. A non-cfut_ fake gets a 400 format
// error instead, which calibration would read as inconclusive — hence
// CalibrationKey (a format-valid, definitely-invalid token). Every other
// openai-compatible account keeps the generic GET /usage.
func (a *openAICompatible) AuthOracleFor(acct Account) AuthProbe {
	if isCloudflareAI(acct) {
		return AuthProbe{
			Method:         http.MethodGet,
			Path:           "https://api.cloudflare.com/client/v4/user/tokens/verify",
			CalibrationKey: cfFakeKey(),
		}
	}
	return a.AuthOracle()
}

// CatalogIsPublic: false on ssn-gpt (bad key → 401 INVALID_API_KEY), which is
// the common case for OpenAI-shaped gateways. Registration still records the
// catalog as informational and gates on the oracle, so a provider that flips
// to a public catalog can't cause a false pass.
func (a *openAICompatible) CatalogIsPublic() bool { return false }

// mapOracleErr classifies a failed oracle read. A retired/renamed balance
// endpoint is "provider has no quota endpoint" (404/405 →
// ErrQuotaUnsupported — the documented contract). Every other status
// (401/429/5xx) propagates unchanged so a transient failure or a dead
// credential never overwrites a good snapshot with kind=none.
func mapOracleErr(err error) error {
	var he *HTTPError
	if errors.As(err, &he) && (he.Status == http.StatusNotFound || he.Status == http.StatusMethodNotAllowed) {
		return ErrQuotaUnsupported
	}
	return err
}

// mapUsageErr classifies a failed generic {base}/usage read. 400/404/405 mean
// the gateway has no quota endpoint there (OpenAI answers GET /v1/usage with
// 400 "missing date" even for a valid key): ErrQuotaUnsupported, recorded as
// "none". 401/429/5xx propagate, as for the oracles.
func mapUsageErr(err error) error {
	var he *HTTPError
	if errors.As(err, &he) && he.Status == http.StatusBadRequest {
		return ErrQuotaUnsupported
	}
	return mapOracleErr(err)
}

// ReadQuota classifies whatever shape the gateway exposes. Verified against
// ssn-gpt (space.stationine.com): balance/remaining in USD plus usage.today and
// usage.total cost fields. Host-scoped oracles (DeepSeek/OpenRouter/Vercel/
// Ollama) run first — providers whose balance lives outside {base}/usage but
// on the SAME key (docs/quota-oracles-design.md); their endpoints were
// live-probed 2026-10-14. Providers with no quota endpoint yield
// ErrQuotaUnsupported, which the UI renders as "measured" (our own numbers).
func (a *openAICompatible) ReadQuota(ctx context.Context, acct Account, key string) (Quota, error) {
	authKey := key
	if isTokenRouter(acct) {
		// The wallet takes the MANAGEMENT key only; the inference key draws
		// a 401 there, which a key test would read as a dead key.
		if strings.TrimSpace(acct.ManagementKey) == "" {
			return Quota{}, ErrQuotaUnsupported
		}
		authKey = acct.ManagementKey
	}
	if isOpenAI(acct) {
		// The credit read takes a platform SESSION token only; the sk- key
		// draws a 401. Without one the account stays "measured".
		authKey = openAISessionToken(acct.ManagementKey)
		if authKey == "" {
			return Quota{}, ErrQuotaUnsupported
		}
	}
	hdrs := withCustomHeaders(map[string]string{"Authorization": "Bearer " + authKey}, acct)
	if isCommandCode(acct) {
		// The billing read is cookie-gated (web session), not key-gated: the
		// inference key is never sent there.
		cookie := commandCodeCookieHeader(acct.ManagementKey)
		if cookie == "" {
			return Quota{}, ErrQuotaUnsupported
		}
		hdrs = withCustomHeaders(map[string]string{"Cookie": cookie}, acct)
	}
	if oracleURL, parse, ok := quotaOracleFor(acct); ok {
		body, err := getJSON(ctx, a.client, oracleURL, hdrs)
		if err != nil {
			return Quota{}, mapOracleErr(err)
		}
		if q := parse(string(body)); q.Kind != "" {
			return q, nil
		}
		return Quota{}, ErrQuotaUnsupported
	}
	body, err := getJSON(ctx, a.client, UpstreamURL(acct.BaseURL, acct.Kind, "/usage"), hdrs)
	if err != nil {
		return Quota{}, mapUsageErr(err)
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
	setAgentUAIfAbsent(req)
	applyCustomHeaders(req, acct.CustomHeaders)
	return nil
}

// ListModels: Anthropic's own catalog endpoint. Not all compatible gateways
// implement it, so a 404/405 degrades to an empty catalog rather than an error —
// registration can still proceed via an explicit model list. The endpoint is
// paginated (20 per page by default), so the listing follows has_more/last_id.
func (a *anthropicCompatible) ListModels(ctx context.Context, acct Account, key string) ([]ModelInfo, error) {
	hdrs := withCustomHeaders(map[string]string{
		"x-api-key":         key,
		"anthropic-version": "2023-06-01",
	}, acct)
	base := UpstreamURL(acct.BaseURL, acct.Kind, "/models") + "?limit=1000"
	var out []ModelInfo
	after := ""
	for page := 0; page < maxCatalogPages; page++ {
		u := base
		if after != "" {
			u += "&after_id=" + url.QueryEscape(after)
		}
		body, status, err := getJSONStatus(ctx, a.client, u, hdrs)
		if page == 0 && (status == http.StatusNotFound || status == http.StatusMethodNotAllowed) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		var raw map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			break
		}
		out = append(out, parseModelList(raw)...)
		more, _ := raw["has_more"].(bool)
		last, _ := raw["last_id"].(string)
		if !more || last == "" || last == after {
			break
		}
		after = last
	}
	return out, nil
}

// maxCatalogPages bounds a paginated catalog walk.
const maxCatalogPages = 20

func (a *anthropicCompatible) ReadQuota(ctx context.Context, acct Account, key string) (Quota, error) {
	// No standard Anthropic quota endpoint; usage arrives in rate-limit headers
	// on real calls instead. The one exception is the Console's prepaid-credit
	// balance, readable with the operator's web session (opt-in, stored in
	// management_key) — else report unsupported so the UI shows "measured".
	// A loopback base is ezllm's own claude-bridge, which serves the plan's
	// 5h/weekly windows on {base}/usage in the percent shape.
	if isLoopbackBase(acct.BaseURL) {
		body, err := getJSON(ctx, a.client, UpstreamURL(acct.BaseURL, acct.Kind, "/usage"),
			withCustomHeaders(map[string]string{"x-api-key": key}, acct))
		if err != nil {
			return Quota{}, mapUsageErr(err)
		}
		if q := ClassifyQuota(string(body)); q.Kind == QuotaPercent {
			return q, nil
		}
		return Quota{}, ErrQuotaUnsupported
	}
	if !isAnthropicAPI(acct) {
		return Quota{}, ErrQuotaUnsupported
	}
	cookie, org, ok := claudePlatformCreds(acct.ManagementKey)
	if !ok {
		return Quota{}, ErrQuotaUnsupported
	}
	hdrs := withCustomHeaders(map[string]string{"Cookie": cookie}, acct)
	body, err := getJSON(ctx, a.client, "https://platform.claude.com/api/organizations/"+org+"/prepaid/credits", hdrs)
	if err != nil {
		return Quota{}, mapOracleErr(err)
	}
	if q := parseClaudePrepaidCredits(string(body)); q.Kind != "" {
		return q, nil
	}
	return Quota{}, ErrQuotaUnsupported
}

// AuthOracle: GET /models with x-api-key — on the real Anthropic API that
// endpoint 401s on an invalid key and costs nothing (no tokens, no generation).
// The M3 design assumed Anthropic had no cheap GET oracle and defaulted to
// POST /messages; GET /models is strictly cheaper and registration's
// calibration probe (a deliberate bad-key call) proves at runtime whether this
// endpoint actually discriminates. If a gateway serves /models publicly,
// calibration detects it and the flow falls back to the inference step as the
// credential gate — so a wrong guess here cannot produce a false pass.
func (a *anthropicCompatible) AuthOracle() AuthProbe {
	return AuthProbe{Method: http.MethodGet, Path: "/models"}
}

// CatalogIsPublic: unknown for Anthropic-shaped gateways (nobody has claimed
// one either way). Registration never trusts this flag as proof anyway — the
// calibration probe decides at runtime.
func (a *anthropicCompatible) CatalogIsPublic() bool { return false }

// ─────────────────────────────────────────────────────────────────────────────
// shared helpers
// ─────────────────────────────────────────────────────────────────────────────

// isCloudflareAI recognizes the Cloudflare Workers AI preset's base URL — an
// account-scoped OpenAI-compatible prefix ending in /ai/v1 on api.cloudflare.com
// (https://api.cloudflare.com/client/v4/accounts/{id}/ai/v1). Host-scoped like
// isOllamaCloud: neither the account NAME nor a user-supplied path can trigger
// the Cloudflare-specific oracle or catalog below.
func isCloudflareAI(acct Account) bool {
	if acct.Kind != KindOpenAICompatible {
		return false
	}
	b := strings.TrimSuffix(strings.TrimSpace(acct.BaseURL), "/")
	u, err := url.Parse(b)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || !strings.EqualFold(u.Hostname(), "api.cloudflare.com") {
		return false
	}
	return strings.HasSuffix(b, "/ai/v1")
}

// listModelsCloudflare: Cloudflare's OpenAI-compatible prefix has NO /models
// (verified live 2026-10-13: 405 "GET not supported for requested URI"), so
// the catalog comes from the native Workers AI endpoint
// GET {origin}/client/v4/accounts/{id}/ai/models/search. That answers the
// Cloudflare envelope {"success":true,"result":[...]} — NOT OpenAI's {"data":[...]}
// — so the generic parser reads zero models and the key test reports "no model
// to probe with". Entries without an id are skipped: the published schema
// documents the envelope, not the item shape.
func listModelsCloudflare(ctx context.Context, c *http.Client, acct Account, key string) ([]ModelInfo, error) {
	prefix := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(acct.BaseURL), "/"), "/ai/v1")
	body, err := getJSON(ctx, c, prefix+"/ai/models/search", withCustomHeaders(map[string]string{
		"Authorization": "Bearer " + key,
	}, acct))
	if err != nil {
		return nil, err
	}
	var env struct {
		Success bool `json:"success"`
		Result  []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
			Label       string `json:"label"`
		} `json:"result"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, err
	}
	if !env.Success {
		msg := "cloudflare catalog rejected the request"
		if len(env.Errors) > 0 && env.Errors[0].Message != "" {
			msg = "cloudflare catalog: " + env.Errors[0].Message
		}
		return nil, fmt.Errorf("%s", msg)
	}
	out := make([]ModelInfo, 0, len(env.Result))
	seen := map[string]bool{}
	for _, m := range env.Result {
		// The callable model id is m.Name (@cf/...). m.ID is an internal UUID.
		// Display name: prefer description/label, fall back to name.
		display := m.Description
		if display == "" {
			display = m.Label
		}
		if display == "" {
			display = m.Name
		}
		if m.Name == "" || seen[m.Name] {
			continue
		}
		seen[m.Name] = true
		out = append(out, ModelInfo{ID: m.Name, Display: display})
	}
	return out, nil
}

// cfFakeKey is a format-valid but definitely-invalid Cloudflare API token.
// Cloudflare validates the Authorization header's SHAPE before the credential:
// a non-cfut_ string gets 400 "Invalid format for Authorization header", which
// the registration calibration would read as "inconclusive" and defer — so the
// deliberate bad key must be cfut_ + 40 hex to reach the 401 the calibration
// needs (verified live 2026-10-13).
func cfFakeKey() string {
	buf := make([]byte, 20)
	_, _ = rand.Read(buf)
	return "cfut_" + hex.EncodeToString(buf)
}

// withCustomHeaders adds the account's operator headers to ezllm's own
// catalog/quota requests, with the same precedence as PrepareRequest gives
// inference: the operator's value wins.
func withCustomHeaders(hdrs map[string]string, acct Account) map[string]string {
	for k, v := range acct.CustomHeaders {
		if k != "" && v != "" {
			hdrs[k] = v
		}
	}
	return hdrs
}

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

// HTTPError is a non-200 provider response. It carries the status so callers
// can classify it (401/403 = bad key, 429 = retryable) instead of parsing the
// message — registration.isAuthErr relies on this to quote the provider's own
// verdict. The message keeps the exact "provider: URL -> HTTP n: body" form
// that step details and error bodies preserve verbatim.
type HTTPError struct {
	URL    string
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("provider: %s -> HTTP %d: %s", e.URL, e.Status, truncate(e.Body, 200))
}

// StatusCode lets callers unwrap the status through an interface, so nothing
// has to depend on this concrete type.
func (e *HTTPError) StatusCode() int { return e.Status }

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
	// These are ezllm's OWN requests (quota, catalog, key probes) — there is no
	// client UA to preserve, so ours is stamped unless the caller set one.
	setAgentUAIfAbsent(req)
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
		return body, resp.StatusCode, &HTTPError{
			URL: url, Status: resp.StatusCode, Body: string(body),
		}
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
	body, err := getJSON(ctx, c, UpstreamURL(acct.BaseURL, acct.Kind, "/models"), withCustomHeaders(hdrs, acct))
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

// truncate cuts s to at most n bytes on a rune boundary: the text reaches the
// ledger and the dashboard, where half a rune is invalid UTF-8.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
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

// newSessionID mints a session id in the shape opencode's own clients use
// ("ses_" + opaque suffix). Only reached when the client sent no session header
// of its own; a client that has one keeps it (see PrepareRequest).
func newSessionID() string { return "ses_" + newRequestID() }

// geminiOpenAI is the OpenAI-compatible surface of Gemini
// (generativelanguage.googleapis.com/v1beta/openai). Live-probed 2026-10-09:
// a bad key answers 400 INVALID_ARGUMENT "Please pass a valid API key", not 401.
type geminiOpenAI struct{ openAICompatible }

func NewGeminiOpenAI(c *http.Client) Adapter {
	return &geminiOpenAI{openAICompatible{client: c}}
}

func (a *geminiOpenAI) Kind() Kind { return KindGeminiOpenAI }

// AuthRejectStatuses: 400 is the bad-key signal here, alongside 401/403.
func (a *geminiOpenAI) AuthRejectStatuses() []int { return []int{400, 401, 403} }
