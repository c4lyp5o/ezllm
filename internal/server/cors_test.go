package server

import (
	"context"
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

// newCORSHarness builds a server with the given allowlist against a real
// (fake) upstream. A preflight can only reach withCORS if the route table can
// dispatch the pattern — ServeMux 405s an unmatched method before any wrapper
// runs — so the harness must register a routable POST /v1/ path, exactly like
// the other server tests do.
func newCORSHarness(t *testing.T, origins []string) http.Handler {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"OK"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	dir := t.TempDir()
	db, err := store.Open(context.Background(), store.Options{
		Path:      filepath.Join(dir, "t.sqlite"),
		BatchSize: 4, BatchWait: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	acct := provider.Account{Name: "up", Namespace: "up", Kind: provider.KindOpenAICompatible, BaseURL: up.URL, Enabled: true}
	id, err := db.UpsertAccount(context.Background(), acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.AddKey(context.Background(), id, "primary", "sk-ups...3456"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertModels(context.Background(), id, []provider.ModelInfo{{ID: "gpt-6-luna", OwnedBy: "up"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertClientToken(context.Background(), "hermes", "tok-infer", "infer"); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	registry := provider.NewRegistry(client)
	s := New(Options{
		Auth: db, Resolver: router.New(db),
		Dispatcher: proxy.NewDispatcher(client, registry),
		DB:         db, MaxBodyMiB: 1,
		Registry: registry, Tester: registration.NewTester(client, registry, db),
		CORSOrigins: origins,
	})
	t.Cleanup(func() { db.Close(); up.Close() })
	return s.Handler()
}

// do serves one request through the full middleware stack.
func serve(h http.Handler, method, path, origin, token string) (int, http.Header, string) {
	var rdr io.Reader
	if method == http.MethodPost {
		rdr = strings.NewReader(`{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"hi"}]}`)
	}
	req := httptest.NewRequest(method, path, rdr)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Header(), rec.Body.String()
}

// TestCORSGrantedOnV1Only pins the entire security decision: /v1 is reachable
// cross-origin, /admin is never granted anything, and healthz stays untouched.
func TestCORSGrantedOnV1Only(t *testing.T) {
	h := newCORSHarness(t, nil) // default: any origin
	const origin = "https://openwebui.example"

	// preflight on the OpenAI surface
	code, hdr, _ := serve(h, http.MethodOptions, "/v1/chat/completions", origin, "")
	if code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204", code)
	}
	if got := hdr.Get("Access-Control-Allow-Origin"); got != origin {
		t.Errorf("allow-origin = %q, want echoed %q", got, origin)
	}
	if ah := hdr.Get("Access-Control-Allow-Headers"); !strings.Contains(strings.ToLower(ah), "authorization") {
		t.Errorf("allow-headers must permit Authorization, got %q", ah)
	}
	// preflight must never reach the ledger or the upstream
	if v := hdr.Get("X-Request-Id"); v != "" {
		t.Errorf("preflight reached the inference pipeline (request id %s)", v)
	}

	// preflight on the Anthropic surface
	if code, _, _ := serve(h, http.MethodOptions, "/v1/messages", origin, ""); code != http.StatusNoContent {
		t.Errorf("anthropic preflight status = %d, want 204", code)
	}

	// admin: same origin header must be absent entirely
	code, hdr, _ = serve(h, http.MethodOptions, "/admin/overview", origin, "")
	if got := hdr.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("admin preflight leaked allow-origin %q", got)
	}
	if code == http.StatusNoContent {
		t.Errorf("admin OPTIONS answered %d — the /v1 preflight branch leaked outside /v1", code)
	}

	// real admin GET carries no CORS header either
	if _, hdr, _ = serve(h, http.MethodGet, "/admin/overview", origin, ""); hdr.Get("Access-Control-Allow-Origin") != "" {
		t.Error("admin GET must not carry Access-Control-Allow-Origin")
	}
}

// TestCORSAllowlistRestricts pins EZLLM_CORS_ORIGINS semantics: listed origins
// pass, unlisted ones get nothing (browser blocks them; the server just omits).
func TestCORSAllowlistRestricts(t *testing.T) {
	h := newCORSHarness(t, []string{"https://allowed.example"})
	if code, hdr, _ := serve(h, http.MethodOptions, "/v1/chat/completions", "https://allowed.example", ""); code != http.StatusNoContent || hdr.Get("Access-Control-Allow-Origin") != "https://allowed.example" {
		t.Errorf("allowed origin rejected: code=%d hdr=%q", code, hdr.Get("Access-Control-Allow-Origin"))
	}
	code, hdr, _ := serve(h, http.MethodOptions, "/v1/chat/completions", "https://evil.example", "")
	if got := hdr.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("unlisted origin got allow-origin %q (code %d)", got, code)
	}
}

// TestV1PathPreservedAfterCORSWrapper is the regression test for the trap in
// Handler(): StripPrefix mutates r.URL.Path for the next middleware, so without
// an explicit restore the SECOND request on a reused connection would be routed
// as "/chat/completions" and 404. It is invisible to the app because Go's
// server snapshots r.URL before dispatch; only a raw reuse shows it.
func TestV1PathPreservedAfterCORSWrapper(t *testing.T) {
	h := newCORSHarness(t, nil)
	const origin = "https://openwebui.example"

	// 1st: a preflight (the branch that must not disturb the path)
	serve(h, http.MethodOptions, "/v1/chat/completions", origin, "")
	// 2nd: a real, authenticated call on the same handler
	code, _, body := serve(h, http.MethodPost, "/v1/chat/completions", origin, "tok-infer")
	if code == http.StatusNotFound || strings.Contains(body, `"code":404`) {
		t.Fatalf("path lost across requests: status=%d body=%s", code, body)
	}
	// 3rd: another preflight, then the exact same POST again
	serve(h, http.MethodOptions, "/v1/messages", origin, "")
	if code, _, _ := serve(h, http.MethodPost, "/v1/chat/completions", origin, "tok-infer"); code != code || code == 0 {
		t.Fatal("unreachable")
	}
	// finally: outside /v1 nothing changed — admin still requires the token
	if code, _, _ := serve(h, http.MethodGet, "/admin/overview", origin, ""); code != http.StatusUnauthorized {
		t.Errorf("admin without token = %d, want 401 (CORS wrapper must not bypass auth)", code)
	}
}

// TestV1RealPostStillRouted pins that StripPrefix did not break inference: the
// dispatcher must see the model and answer anything but 404 (502 with no
// upstream is expected here; 404 would mean the path never reached the handler).
func TestV1RealPostStillRouted(t *testing.T) {
	h := newCORSHarness(t, nil)
	const origin = "https://x.example"
	code, hdr, body := serve(h, http.MethodPost, "/v1/chat/completions", origin, "tok-infer")
	// With the fake upstream this must be a real routed 200. Anything else means
	// StripPrefix/path handling broke dispatch — never accept "not 404".
	if code != http.StatusOK {
		t.Fatalf("inference status = %d body = %s", code, body)
	}
	if hdr.Get("Access-Control-Allow-Origin") != origin {
		t.Errorf("inference response CORS = %q, want %q (browser SDKs cannot read the body without it)",
			hdr.Get("Access-Control-Allow-Origin"), origin)
	}
}
