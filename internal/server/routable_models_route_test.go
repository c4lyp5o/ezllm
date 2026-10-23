package server

import (
	"net/http"
	"strings"
	"testing"
)

// The dashboard holds a session cookie, never a bearer key, so it could not
// call /v1/models — which is why the Connect page's live probe never ran and
// its "supported" chips were labels. This route serves the same routable
// catalog behind the admin session.
func TestAdminRoutableModelsServesTheCatalog(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	code, _, body := h.do("GET", "/admin/routable-models", adminTok(), "")
	if code != 200 {
		t.Fatalf("admin routable models: %d %s", code, body)
	}
	if !strings.Contains(body, `"data"`) {
		t.Fatalf("payload is not the OpenAI-shaped catalog: %s", body)
	}
}

// Same gate as every other /admin route: a cookie or an admin token, never
// anonymous.
func TestAdminRoutableModelsRejectsAnonymous(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	code, _, _ := h.do("GET", "/admin/routable-models", "", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("anonymous read must be refused, got %d", code)
	}
}
