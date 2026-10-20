package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/registration"
	"github.com/c4lyp5o/ezllm/internal/router"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// TestVSCodeAliasRoutes verifies the tokenized VS Code alias routes reach the
// handler (proven by our JSON error envelope or valid empty model list).
func TestVSCodeAliasRoutes(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(context.Background(), store.Options{
		Path:      filepath.Join(dir, "t.sqlite"),
		BatchSize: 4, BatchWait: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.UpsertClientToken(context.Background(), "hermes", "tok-infer", "infer"); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	registry := provider.NewRegistry(client)
	dispatcher := proxy.NewDispatcher(client, registry)
	s := New(Options{
		Auth: db, Resolver: router.New(db),
		Dispatcher: dispatcher,
		DB:         db, MaxBodyMiB: 1,
		Registry: registry, Tester: registration.NewTester(client, registry, db),
	})

	// No account registered, so:
	// - chat/completions & responses: model_not_found (404, our envelope)
	// - /models & /: empty model list (200, valid)
	routes := map[string]struct {
		method          string
		expectStatus    int
		expectErrorType string // empty = don't check
	}{
		"/api/v1/vscode/tok-infer/":                 {http.MethodGet, http.StatusOK, ""},
		"/api/v1/vscode/tok-infer/models":           {http.MethodGet, http.StatusOK, ""},
		"/api/v1/vscode/tok-infer/chat/completions": {http.MethodPost, http.StatusNotFound, "model_not_found"},
		"/api/v1/vscode/tok-infer/responses":        {http.MethodPost, http.StatusNotFound, "model_not_found"},
	}
	for route, spec := range routes {
		body := strings.NewReader(`{"model":"test/gpt-6-luna","messages":[{"role":"user","content":"hi"}]}`)
		req := httptest.NewRequest(spec.method, route, body)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)

		if rec.Code != spec.expectStatus {
			t.Errorf("%s: expected %d, got %d — %q", route, spec.expectStatus, rec.Code, rec.Body.String())
			continue
		}

		if spec.expectErrorType != "" {
			var env struct {
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Errorf("%s: body is not our JSON error envelope: %q", route, rec.Body.String())
				continue
			}
			if env.Error.Type != spec.expectErrorType {
				t.Errorf("%s: expected error type %q, got %q", route, spec.expectErrorType, env.Error.Type)
				continue
			}
		}
		t.Logf("%s (%s): %d OK", route, spec.method, rec.Code)
	}
}

// TestVSCodeAliasEnforcesTokenBudget (H2): the path-token alias must run the
// same per-key budget gate as bearer auth. It used to skip it, so a capped
// key kept working past its cap by switching to the VS Code URL.
func TestVSCodeAliasEnforcesTokenBudget(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("an exhausted key must never reach the upstream")
	})
	ctx := context.Background()
	bucket := store.CountBucket(time.Now(), "monthly", time.Local)
	if _, err := h.db.Writer().ExecContext(ctx,
		`UPDATE client_tokens SET cap_tokens=100, cap_window='monthly' WHERE name='hermes'`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Writer().ExecContext(ctx,
		`INSERT INTO client_usage_counters (client, bucket, tokens, calls, updated_at) VALUES ('hermes', ?, 100, 1, '2026-01-01T00:00:00Z')`, bucket); err != nil {
		t.Fatal(err)
	}

	// Bearer path: refused (the existing behaviour, as the control).
	if code, _, body := h.do("GET", "/v1/models", "tok-infer", ""); code != http.StatusTooManyRequests {
		t.Fatalf("bearer with spent budget = %d %s, want 429", code, body)
	}
	for _, path := range []string{"/api/v1/vscode/tok-infer/models", "/api/v1/vscode/tok-infer/chat/completions"} {
		method, body := http.MethodGet, ""
		if strings.HasSuffix(path, "completions") {
			method, body = http.MethodPost, `{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"hi"}]}`
		}
		code, _, resp := h.do(method, path, "", body)
		if code != http.StatusTooManyRequests || !strings.Contains(resp, "token budget reached") {
			t.Fatalf("%s %s with spent budget = %d %s, want 429 token budget reached", method, path, code, resp)
		}
	}
}

// TestVSCodeAliasTokenNotInAccessLog (H1): the access log printed
// r.URL.Path verbatim, which for this alias is a working client token.
func TestVSCodeAliasTokenNotInAccessLog(t *testing.T) {
	var logBuf syncBuffer
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {}, func(o *Options) {
		o.Log = slog.New(slog.NewTextHandler(&logBuf, nil))
	})
	for _, path := range []string{"/api/v1/vscode/tok-infer/models", "/api/v1/vscode/tok-infer/", "/api/v1/vscode/wrong-secret-token/models"} {
		h.do(http.MethodGet, path, "", "")
	}
	out := logBuf.String()
	if strings.Contains(out, "tok-infer") || strings.Contains(out, "wrong-secret-token") {
		t.Fatalf("access log leaked a path token:\n%s", out)
	}
	if !strings.Contains(out, "path=/api/v1/vscode/***/models") {
		t.Fatalf("redacted path missing from the access log:\n%s", out)
	}
}

func TestLogPath(t *testing.T) {
	for in, want := range map[string]string{
		"/api/v1/vscode/abc/chat/completions": "/api/v1/vscode/***/chat/completions",
		"/api/v1/vscode/abc/":                 "/api/v1/vscode/***/",
		"/api/v1/vscode/abc":                  "/api/v1/vscode/***",
		"/api/v1/vscode/":                     "/api/v1/vscode/",
		"/v1/chat/completions":                "/v1/chat/completions",
	} {
		if got := logPath(in); got != want {
			t.Errorf("logPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// syncBuffer is a bytes.Buffer safe for the logger's concurrent writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestVSCodeAliasInvalidToken verifies invalid token in URL is rejected.
func TestVSCodeAliasInvalidToken(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(context.Background(), store.Options{
		Path:      filepath.Join(dir, "t.sqlite"),
		BatchSize: 4, BatchWait: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.UpsertClientToken(context.Background(), "hermes", "tok-infer", "infer"); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	registry := provider.NewRegistry(client)
	dispatcher := proxy.NewDispatcher(client, registry)
	s := New(Options{
		Auth: db, Resolver: router.New(db),
		Dispatcher: dispatcher,
		DB:         db, MaxBodyMiB: 1,
		Registry: registry, Tester: registration.NewTester(client, registry, db),
	})

	// Invalid token should return 401
	route := "/api/v1/vscode/invalid-token/chat/completions"
	body := strings.NewReader(`{"model":"test/gpt-6-luna","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, route, body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401 for invalid token, got %d: %q", rec.Code, rec.Body.String())
	}
	t.Logf("Invalid token: %d", rec.Code)
}
