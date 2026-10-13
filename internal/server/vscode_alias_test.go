package server

import (
	"context"
	"encoding/json"
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
