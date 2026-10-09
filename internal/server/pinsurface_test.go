package server

// A pinned model called on the wrong surface must fail as a CLIENT error
// (400 pinned_surface), not as an upstream failure (502). The message alone
// was already right, so a default-branch regression would pass any test that
// only greps the text — this asserts status, type, and the structured fields.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestPinnedSurfaceIs400Not502(t *testing.T) {
	called := false
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"OK"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})
	acct := seededAccountID(t, h)
	if err := h.db.SetModelPin(context.Background(), acct, "gpt-6-luna", "openai"); err != nil {
		t.Fatalf("set pin: %v", err)
	}

	code, _, body := h.do("POST", "/v1/messages", "tok-infer",
		`{"model":"up/gpt-6-luna","max_tokens":8,"messages":[{"role":"user","content":"ping"}]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("wrong-surface call: got %d want 400 (%s)", code, body)
	}
	if called {
		t.Error("refused request still reached the upstream")
	}

	var e struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Pinned  string `json:"pinned"`
			Asked   string `json:"requested_surface"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("body not json: %v (%s)", err, body)
	}
	if e.Error.Type != "pinned_surface" {
		t.Errorf("type = %q, want pinned_surface", e.Error.Type)
	}
	if e.Error.Pinned != "openai" || e.Error.Asked != "anthropic" {
		t.Errorf("fields = pinned %q / requested %q, want openai / anthropic",
			e.Error.Pinned, e.Error.Asked)
	}
	if !strings.Contains(e.Error.Message, "pinned to the openai surface") {
		t.Errorf("message lost its guidance: %q", e.Error.Message)
	}

	// The pinned surface itself must still route normally.
	code, _, body = h.do("POST", "/v1/chat/completions", "tok-infer",
		`{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"ping"}]}`)
	if code != http.StatusOK {
		t.Fatalf("pinned surface on matching endpoint: got %d want 200 (%s)", code, body)
	}
}
