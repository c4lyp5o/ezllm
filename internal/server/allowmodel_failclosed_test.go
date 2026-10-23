package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The per-key model allowlist is an authorization control. When the store
// cannot answer, the request must be refused rather than allowed: the budget
// check later in the same handler already answers 503 for an unavailable
// store, and failing open here suspended the allowlist for every request in
// the window.
func TestAllowModelRefusesWhenTheAllowlistCannotBeRead(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	// A closed store makes the lookup fail with a real driver error, which is
	// the class this test is about. It is NOT ErrNotFound.
	if err := h.db.Close(); err != nil {
		t.Fatalf("closing the store: %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)

	if h.server.allowModel(w, req, "some-client", "some/model") {
		t.Fatalf("an unreadable allowlist must refuse the request, got allow")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("refusal must use the API error envelope, got Content-Type %q", ct)
	}
}

// The unchanged half: a token with no row carries no policy, so the lookup
// answers ErrNotFound and the request proceeds. This is what keeps the
// dashboard's own chat working.
func TestAllowModelAllowsWhenTheTokenHasNoPolicy(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)

	if !h.server.allowModel(w, req, "client-with-no-token-row", "any/model") {
		t.Fatalf("a token with no policy row must be allowed, got refusal %d", w.Code)
	}
}
