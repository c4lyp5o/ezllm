package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every inference route needs the infer role (v2.1.0). Before, any valid token
// could call models, so a key minted for admin or metrics was also an
// inference key. Covers the Anthropic surface through x-api-key, the header
// Claude Code sends, and the VS Code path alias.
func TestInferenceNeedsTheInferRole(t *testing.T) {
	h := newHarness(t, okUpstream)
	ctx := context.Background()
	for name, roles := range map[string]string{"ops-only": "admin", "scrape-only": "metrics"} {
		if err := h.db.UpsertClientToken(ctx, name, "tok-"+name, roles); err != nil {
			t.Fatal(err)
		}
	}
	routes := []struct{ m, path string }{
		{"GET", "/v1/models"},
		{"GET", "/v1/prices"},
		{"POST", "/v1/chat/completions"},
		{"POST", "/v1/messages"},
		{"POST", "/v1/responses"},
		{"POST", "/v1/embeddings"},
		{"POST", "/v1/images/generations"},
		{"POST", "/v1/audio/speech"},
		{"POST", "/v1/audio/transcriptions"},
		{"POST", "/v1/images/edits"},
		{"POST", "/v1/moderations"},
		{"POST", "/v1/rerank"},
		{"POST", "/v1/videos/generations"},
		{"POST", "/v1/music/generations"},
	}
	for _, tok := range []string{"tok-ops-only", "tok-scrape-only"} {
		for _, rt := range routes {
			body := ""
			if rt.m == "POST" {
				body = `{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`
			}
			code, _, resp := h.do(rt.m, rt.path, tok, body)
			if code != http.StatusForbidden || !strings.Contains(resp, "infer role required") {
				t.Errorf("%s on %s %s: %d %s, want 403 infer role required", tok, rt.m, rt.path, code, resp)
			}
		}
		// Anthropic clients authenticate with x-api-key, not Authorization.
		req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"up/gpt-6-luna","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("x-api-key", tok)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s via x-api-key on /v1/messages: %d, want 403", tok, rec.Code)
		}
		for _, p := range []string{"/api/v1/vscode/" + tok + "/models"} {
			if code, _, _ := h.do("GET", p, "", ""); code != http.StatusForbidden {
				t.Errorf("%s on the VS Code alias: %d, want 403", tok, code)
			}
		}
	}

	// The infer key still works on both surfaces and both headers.
	if code, _, body := h.do("POST", "/v1/chat/completions", "tok-infer", `{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"hi"}]}`); code != http.StatusOK {
		t.Errorf("infer key on chat: %d %s", code, body)
	}
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("x-api-key", "tok-infer")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("infer key via x-api-key: %d", rec.Code)
	}
	if code, _, _ := h.do("GET", "/api/v1/vscode/tok-infer/models", "", ""); code != http.StatusOK {
		t.Errorf("infer key on the VS Code alias: %d", code)
	}
}

// After upgrading, an operator whose key lost inference needs the reason
// named at debug, without the token.
func TestInferRoleRefusalIsLogged(t *testing.T) {
	h, sink := harnessWithLog(t, slog.LevelDebug, okUpstream)
	if err := h.db.UpsertClientToken(context.Background(), "scrape-only", "tok-scrape-only", "metrics"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := h.do("GET", "/v1/models", "tok-scrape-only", ""); code != http.StatusForbidden {
		t.Fatalf("metrics key on /v1/models: %d", code)
	}
	out := sink.String()
	if !strings.Contains(out, "role refused") || !strings.Contains(out, "lacks the infer role") || !strings.Contains(out, "path=/v1/models") {
		t.Fatalf("infer refusal not logged:\n%s", out)
	}
	if strings.Contains(out, "tok-scrape-only") {
		t.Fatalf("the token was written to the log:\n%s", out)
	}

	// Filtered out at info.
	h2, sink2 := harnessWithLog(t, slog.LevelInfo, okUpstream)
	if err := h2.db.UpsertClientToken(context.Background(), "scrape-only", "tok-scrape-only", "metrics"); err != nil {
		t.Fatal(err)
	}
	h2.do("GET", "/v1/models", "tok-scrape-only", "")
	if strings.Contains(sink2.String(), "role refused") {
		t.Fatal("role refusal logged at info")
	}
}
