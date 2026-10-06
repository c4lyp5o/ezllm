package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/registration"
	"github.com/c4lyp5o/ezllm/internal/router"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// harness wires a real store + real dispatcher against a fake upstream.
type harness struct {
	srv      *http.Server
	handler  http.Handler
	db       *store.DB
	base     string // ezllm base URL
	upstream *httptest.Server
	t        *testing.T
}

func newHarness(t *testing.T, upstream http.HandlerFunc) *harness {
	t.Helper()
	up := httptest.NewServer(upstream)
	dir := t.TempDir()
	db, err := store.Open(context.Background(), store.Options{
		Path: filepath.Join(dir, "t.sqlite"), MasterKey: "test-master",
		BatchSize: 4, BatchWait: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	// one account, one key, one model
	acct := provider.Account{
		Name: "up", Namespace: "up", Kind: provider.KindOpenAICompatible,
		BaseURL: up.URL, Enabled: true,
	}
	id, err := db.UpsertAccount(context.Background(), acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.AddKey(context.Background(), id, "primary", "sk-upstream-key-123456"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertModels(context.Background(), id, []provider.ModelInfo{{ID: "gpt-6-luna", OwnedBy: "up"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertClientToken(context.Background(), "hermes", "tok-infer", "infer"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertClientToken(context.Background(), "admin", "tok-admin", "infer,admin"); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	registry := provider.NewRegistry(client)
	s := New(Options{
		Auth: db, Resolver: router.New(db),
		Dispatcher: proxy.NewDispatcher(client, registry),
		DB:         db, MaxBodyMiB: 1,
		Registry: registry, Tester: registration.NewTester(client, registry, db),
	})
	h := &harness{handler: s.Handler(), db: db, upstream: up, t: t}
	t.Cleanup(func() { db.Close(); up.Close() })
	return h
}

func (h *harness) do(method, path, token, body string) (int, http.Header, string) {
	h.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec.Code, rec.Header(), rec.Body.String()
}

// ── auth ────────────────────────────────────────────────────────────────────

func TestHealthIsPublic(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, _, body := h.do("GET", "/healthz", "", "")
	if code != 200 {
		t.Fatalf("healthz = %d", code)
	}
	var m map[string]any
	json.Unmarshal([]byte(body), &m)
	if m["status"] != "ok" || m["version"] != "m2" {
		t.Errorf("healthz payload = %v", m)
	}
	if m["accounts"].(float64) != 1 {
		t.Errorf("accounts = %v, want 1", m["accounts"])
	}
	// must never leak secrets
	for _, bad := range []string{"sk-upstream-key", "tok-infer", "test-master"} {
		if strings.Contains(body, bad) {
			t.Errorf("healthz leaked %q", bad)
		}
	}
}

func TestAuthRequiredOnInferenceAndModels(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	for _, p := range []struct{ m, path string }{
		{"GET", "/v1/models"},
		{"POST", "/v1/chat/completions"},
		{"POST", "/v1/messages"},
		{"POST", "/v1/responses"},
	} {
		code, _, body := h.do(p.m, p.path, "", `{"model":"up/gpt-6-luna"}`)
		if code != 401 {
			t.Errorf("%s %s without token = %d, want 401", p.m, p.path, code)
		}
		if !strings.Contains(body, "error") {
			t.Errorf("%s %s: error body missing: %s", p.m, p.path, body)
		}
		// a wrong token is also 401
		code, _, _ = h.do(p.m, p.path, "wrong-token", `{"model":"up/gpt-6-luna"}`)
		if code != 401 {
			t.Errorf("%s %s with bad token = %d, want 401", p.m, p.path, code)
		}
	}
}

// Anthropic clients authenticate with x-api-key, not Bearer (Claude Code).
func TestAuthAcceptsXApiKey(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"type":"message","usage":{"input_tokens":5,"output_tokens":2}}`))
	})
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"up/gpt-6-luna","messages":[]}`))
	req.Header.Set("x-api-key", "tok-infer")
	req.Header.Set("anthropic-version", "2023-06-01")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("x-api-key auth failed: %d %s", rec.Code, rec.Body.String())
	}
}

// ── the three surfaces ──────────────────────────────────────────────────────

func TestChatCompletionsRoutesAndLedgers(t *testing.T) {
	var gotPath, gotModel, gotAuth string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(b, &m)
		gotModel, _ = m["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-opencode-endpoint-id", "alibaba-us")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"OK"}}],"usage":{"prompt_tokens":2044,"completion_tokens":72,"total_tokens":2116,"prompt_tokens_details":{"cached_tokens":1792},"completion_tokens_details":{"reasoning_tokens":28}}}`))
	})

	code, _, body := h.do("POST", "/v1/chat/completions", "tok-infer",
		`{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"hi"}],"extra_body":{"keep":"me"}}`)
	if code != 200 {
		t.Fatalf("status %d body %s", code, body)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("upstream path = %s", gotPath)
	}
	if gotModel != "gpt-6-luna" {
		t.Errorf("upstream model = %q, want the RESOLVED model (namespace stripped)", gotModel)
	}
	if gotAuth != "Bearer sk-upstream-key-123456" {
		t.Errorf("upstream auth = %q, want the PROVIDER key (not the client token)", gotAuth)
	}
	if !strings.Contains(body, `"content":"OK"`) {
		t.Errorf("client did not get upstream body: %s", body)
	}

	// ledger: normalized accounting (cached INCLUDED in prompt_tokens upstream)
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}
	rows, err := h.db.RecentCalls(context.Background(), 5)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	c := rows[0]
	if c.Client != "hermes" {
		t.Errorf("client attribution = %q, want hermes", c.Client)
	}
	if c.Account != "up" || c.Model != "gpt-6-luna" || c.Alias != "up/gpt-6-luna" {
		t.Errorf("attribution wrong: acct=%q model=%q alias=%q", c.Account, c.Model, c.Alias)
	}
	if c.Surface != store.SurfaceOpenAI {
		t.Errorf("surface = %q", c.Surface)
	}
	if c.TokensIn != 252 || c.TokensOut != 72 || c.TokensCachedRead != 1792 || c.ReasoningTokens != 28 {
		t.Errorf("NORMALIZED usage wrong: in=%d out=%d cr=%d rt=%d (want 252/72/1792/28)",
			c.TokensIn, c.TokensOut, c.TokensCachedRead, c.ReasoningTokens)
	}
	if c.EndpointID != "alibaba-us" {
		t.Errorf("provenance not captured: %q", c.EndpointID)
	}
	if !strings.Contains(c.RawUsage, "1792") {
		t.Error("raw usage not retained for audit")
	}
	if strings.Contains(c.RawUsage, "sk-upstream") {
		t.Error("raw usage leaked a key")
	}
}

func TestMessagesRoutesToAnthropicPath(t *testing.T) {
	var gotPath, gotAPIKey, gotVersion string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"type":"message","usage":{"input_tokens":315,"output_tokens":73,"cache_read_input_tokens":0,"cache_creation_input_tokens":2048}}`))
	})
	code, _, _ := h.do("POST", "/v1/messages", "tok-infer",
		`{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"hi"}],"tools":[]}`)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if gotPath != "/messages" {
		t.Errorf("upstream path = %s, want /messages", gotPath)
	}
	if gotAPIKey == "" || gotVersion == "" {
		t.Errorf("anthropic auth headers missing: x-api-key=%q version=%q", gotAPIKey, gotVersion)
	}
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}
	rows, _ := h.db.RecentCalls(context.Background(), 1)
	if len(rows) != 1 {
		t.Fatal("no ledger row")
	}
	// Anthropic EXCLUDES cache_read from input_tokens, and reports cache_write
	if rows[0].Surface != store.SurfaceAnthropic {
		t.Errorf("surface = %q", rows[0].Surface)
	}
	if rows[0].TokensIn != 315 || rows[0].TokensCachedWrite != 2048 {
		t.Errorf("anthropic normalization wrong: in=%d cw=%d (want 315/2048)",
			rows[0].TokensIn, rows[0].TokensCachedWrite)
	}
}

func TestResponsesRoutesAndCapturesCacheWrite(t *testing.T) {
	var gotPath string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp_x","object":"response","usage":{"input_tokens":3000,"output_tokens":40,"input_tokens_details":{"cached_tokens":2048,"cache_write_tokens":512},"output_tokens_details":{"reasoning_tokens":7}}}`))
	})
	code, _, _ := h.do("POST", "/v1/responses", "tok-infer", `{"model":"up/gpt-6-luna","input":"hi"}`)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if gotPath != "/responses" {
		t.Errorf("upstream path = %s, want /responses", gotPath)
	}
	h.db.Flush()
	rows, _ := h.db.RecentCalls(context.Background(), 1)
	if len(rows) != 1 {
		t.Fatal("no ledger row")
	}
	c := rows[0]
	if c.Surface != store.SurfaceResponses {
		t.Errorf("surface = %q", c.Surface)
	}
	// Responses is the ONLY surface reporting cache_write — the dashboard's
	// "cached write" column depends on this.
	if c.TokensCachedWrite != 512 || c.TokensCachedRead != 2048 || c.TokensIn != 952 || c.ReasoningTokens != 7 {
		t.Errorf("responses normalization wrong: in=%d cr=%d cw=%d rt=%d (want 952/2048/512/7)",
			c.TokensIn, c.TokensCachedRead, c.TokensCachedWrite, c.ReasoningTokens)
	}
}

// Streaming must be incremental and the usage tap must still fire.
func TestStreamingEndToEnd(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f, _ := w.(http.Flusher)
		for _, l := range []string{
			`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
			`data: {"choices":[{"delta":{"content":"1"}}]}`,
			`data: {"choices":[{"delta":{"content":"2"}}],"usage":{"prompt_tokens":100,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":0}}}`,
			`data: [DONE]`,
		} {
			fmt.Fprintf(w, "%s\n\n", l)
			if f != nil {
				f.Flush()
			}
			time.Sleep(15 * time.Millisecond)
		}
	})
	code, hdr, body := h.do("POST", "/v1/chat/completions", "tok-infer",
		`{"model":"up/gpt-6-luna","stream":true,"messages":[]}`)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if !strings.Contains(hdr.Get("Content-Type"), "text/event-stream") {
		t.Errorf("content-type = %q, want text/event-stream", hdr.Get("Content-Type"))
	}
	for _, want := range []string{"delta", "[DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream body missing %q: %s", want, body)
		}
	}
	h.db.Flush()
	rows, _ := h.db.RecentCalls(context.Background(), 1)
	if len(rows) != 1 {
		t.Fatal("no ledger row for streamed call")
	}
	if !rows[0].Stream {
		t.Error("stream flag not recorded")
	}
	if rows[0].TokensOut != 2 {
		t.Errorf("streamed usage not tapped: out=%d", rows[0].TokensOut)
	}
}

// ── error paths ─────────────────────────────────────────────────────────────

func TestUnknownModelIs404WithHints(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, _, body := h.do("POST", "/v1/chat/completions", "tok-infer",
		`{"model":"up/does-not-exist","messages":[]}`)
	if code != 404 {
		t.Fatalf("status = %d, want 404", code)
	}
	var e map[string]any
	json.Unmarshal([]byte(body), &e)
	errObj, _ := e["error"].(map[string]any)
	if errObj == nil || errObj["type"] != "model_not_found" {
		t.Errorf("error shape wrong: %s", body)
	}
	// a bare model id (no namespace) is also 404 — there is no default account
	code, _, body = h.do("POST", "/v1/chat/completions", "tok-infer", `{"model":"gpt-6-luna","messages":[]}`)
	if code != 404 {
		t.Fatalf("bare model = %d, want 404 (no default account)", code)
	}
	if !strings.Contains(body, "up/gpt-6-luna") {
		t.Errorf("404 should hint the real namespace: %s", body)
	}
}

func TestMissingModelFieldIs400(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	for _, body := range []string{`{}`, `{"messages":[]}`, `not json`, ``} {
		code, _, _ := h.do("POST", "/v1/chat/completions", "tok-infer", body)
		if code != 400 {
			t.Errorf("body %q -> %d, want 400", body, code)
		}
	}
}

// Oversized bodies must be rejected, not read into memory unbounded.
func TestBodySizeCap(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	big := `{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"` +
		strings.Repeat("x", 2<<20) + `"}]}`
	code, _, _ := h.do("POST", "/v1/chat/completions", "tok-infer", big)
	// 400 (bad JSON after truncation) or 413 are both acceptable; a 200 is not.
	if code == 200 {
		t.Errorf("oversized body was accepted (%d)", code)
	}
}

// Upstream 429 must be relayed verbatim (M4 adds cooldown; M2 must not swallow it).
func TestUpstream429Relayed(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"type":"rate_limit","message":"Maximum 2 requests within 1 minutes"}}`))
	})
	code, _, body := h.do("POST", "/v1/chat/completions", "tok-infer", `{"model":"up/gpt-6-luna","messages":[]}`)
	if code != 429 {
		t.Fatalf("status = %d, want 429", code)
	}
	if !strings.Contains(body, "Maximum 2 requests") {
		t.Errorf("upstream error not relayed verbatim: %s", body)
	}
	h.db.Flush()
	rows, _ := h.db.RecentCalls(context.Background(), 1)
	if len(rows) != 1 || rows[0].Status != 429 {
		t.Errorf("429 not ledgered: %+v", rows)
	}
}

func TestUpstream5xxRelayed(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		w.Write([]byte(`{"error":{"message":"cache-only admission rejected"}}`))
	})
	code, _, body := h.do("POST", "/v1/chat/completions", "tok-infer", `{"model":"up/gpt-6-luna","messages":[]}`)
	if code != 503 {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(body, "cache-only") {
		t.Errorf("body not relayed: %s", body)
	}
}

// A panic in OUR handler must become a 500, not kill the server. (A panic in
// the fake *upstream* would be recovered by httptest itself and surface as a
// truncated response, so we panic in our own code path instead.)
func TestRecovererHandlesPanic(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	// Build a server whose handler panics after auth, exercising the recoverer.
	panicMux := http.NewServeMux()
	panicMux.HandleFunc("POST /boom", func(w http.ResponseWriter, r *http.Request) {
		panic("internal explosion")
	})
	s := New(Options{Auth: h.db, DB: h.db, MaxBodyMiB: 1})
	wrapped := s.WrapForTest(panicMux)

	req := httptest.NewRequest("POST", "/boom", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer tok-infer")
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != 500 {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "internal explosion") {
		t.Error("panic detail leaked to the client")
	}
	if !strings.Contains(rec.Body.String(), "error") {
		t.Errorf("expected an error body, got %s", rec.Body.String())
	}
}

// A non-streamed upstream that closes without a valid response must be
// ledgered as a failure, not silently recorded as success.
func TestUpstreamFailureIsLedgered(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		// Hijack and close WITHOUT writing a response: the client sees a
		// connection reset / empty reply, which must not look like a 200.
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
				return
			}
		}
		w.WriteHeader(http.StatusInternalServerError)
	})

	// Serve over a real listener so a hijacked close surfaces as a client error
	// rather than being papered over by httptest.ResponseRecorder.
	ln := httptest.NewServer(h.handler)
	// Close it explicitly (not via t.Cleanup) so no goroutine or handle outlives
	// this test — a leaked listener was perturbing the NEXT test's ledger counts.
	defer ln.Close()
	defer ln.CloseClientConnections()

	req, err := http.NewRequest("POST", ln.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"up/gpt-6-luna","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok-infer") // required, else 401 pre-dispatch

	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Errorf("hijacked/closed upstream should not yield 200, got %d", resp.StatusCode)
		}
	} else {
		t.Logf("client saw transport error (expected): %v", err)
	}

	h.db.Flush()
	rows, _ := h.db.RecentCalls(context.Background(), 3)
	if len(rows) == 0 {
		t.Fatal("failed call was not ledgered")
	}
	c := rows[0]
	if c.Status == 200 && c.Err == "" {
		t.Errorf("truncated upstream ledgered as clean success: %+v", c)
	}
}

// ── /v1/models ──────────────────────────────────────────────────────────────

func TestModelsListsNamespacedIDs(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, _, body := h.do("GET", "/v1/models", "tok-infer", "")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	var resp struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	json.Unmarshal([]byte(body), &resp)
	if resp.Object != "list" {
		t.Errorf("object = %q", resp.Object)
	}
	if len(resp.Data) != 1 || resp.Data[0].ID != "up/gpt-6-luna" {
		t.Errorf("models = %+v, want one namespaced id", resp.Data)
	}
}

func TestModelsAnthropicShape(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, _, body := h.do("GET", "/v1/models?protocol=anthropic", "tok-infer", "")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	var resp struct {
		Data []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"data"`
		HasMore bool `json:"has_more"`
	}
	json.Unmarshal([]byte(body), &resp)
	if len(resp.Data) != 1 || resp.Data[0].Type != "model" {
		t.Errorf("anthropic model shape wrong: %s", body)
	}
}

// ── admin ───────────────────────────────────────────────────────────────────

func TestAdminRequiresAdminRole(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	// infer-only token is forbidden
	code, _, body := h.do("GET", "/admin/health", "tok-infer", "")
	if code != 403 {
		t.Fatalf("infer token got %d on /admin/health, want 403 (%s)", code, body)
	}
	code, _, _ = h.do("GET", "/admin/usage", "tok-infer", "")
	if code != 403 {
		t.Errorf("infer token got %d on /admin/usage, want 403", code)
	}
	// admin token is allowed
	code, _, body = h.do("GET", "/admin/health", "tok-admin", "")
	if code != 200 {
		t.Fatalf("admin token got %d: %s", code, body)
	}
	for _, want := range []string{"schema_version", "ledger_rows", "dropped_rows"} {
		if !strings.Contains(body, want) {
			t.Errorf("admin health missing %q: %s", want, body)
		}
	}
}

func TestAdminUsageAggregates(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"usage":{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":0}}}`))
	})
	for i := 0; i < 3; i++ {
		if code, _, b := h.do("POST", "/v1/chat/completions", "tok-infer", `{"model":"up/gpt-6-luna","messages":[]}`); code != 200 {
			t.Fatalf("call %d: %d %s", i, code, b)
		}
	}
	h.db.Flush()
	code, _, body := h.do("GET", "/admin/usage?group_by=account", "tok-admin", "")
	if code != 200 {
		t.Fatalf("usage status %d", code)
	}
	var resp struct {
		Groups []store.UsageRow `json:"groups"`
		Totals store.UsageRow   `json:"totals"`
	}
	json.Unmarshal([]byte(body), &resp)
	if len(resp.Groups) != 1 || resp.Groups[0].Key != "up" {
		t.Fatalf("groups = %+v", resp.Groups)
	}
	if resp.Groups[0].Calls != 3 || resp.Groups[0].TokensIn != 300 || resp.Groups[0].TokensOut != 30 {
		t.Errorf("aggregate wrong: %+v", resp.Groups[0])
	}
	if resp.Totals.Calls != 3 {
		t.Errorf("totals = %+v", resp.Totals)
	}
	// bad group_by is a 400, not a silent default
	if code, _, _ := h.do("GET", "/admin/usage?group_by=nonsense", "tok-admin", ""); code != 400 {
		t.Errorf("bad group_by = %d, want 400", code)
	}
}

// ── security: no secret may reach a response or log ─────────────────────────

func TestNoSecretsInResponses(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	paths := []struct{ m, p, tok string }{
		{"GET", "/healthz", ""},
		{"GET", "/v1/models", "tok-infer"},
		{"GET", "/admin/health", "tok-admin"},
		{"GET", "/admin/usage", "tok-admin"},
		{"POST", "/v1/chat/completions", "tok-infer"},
	}
	secrets := []string{"sk-upstream-key-123456", "sk-upstream", "tok-infer", "tok-admin", "test-master"}
	for _, p := range paths {
		body := `{"model":"up/gpt-6-luna","messages":[]}`
		if p.m == "GET" {
			body = ""
		}
		_, _, respBody := h.do(p.m, p.p, p.tok, body)
		for _, s := range secrets {
			if strings.Contains(respBody, s) {
				t.Errorf("%s %s leaked secret %q", p.m, p.p, s)
			}
		}
	}
}
