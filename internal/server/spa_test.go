package server

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newSPA wires a Server with no dependencies: the SPA handler touches only
// s.web, and the API routes registered around it answer 401/404 on their own.
func newSPA(t *testing.T) *Server {
	t.Helper()
	if _, err := fs.Stat(webFS(), "index.html"); err != nil {
		t.Skipf("web/dist not built (run `npm run build` in web/): %v", err)
	}
	return New(Options{})
}

func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// The dist/ prefix inside the embed FS (//go:embed dist keeps the directory in
// the path) once made every lookup miss while the API kept answering fine —
// i.e. exactly the failure this asserts against.
func TestSPAServesIndexAndDeepLinks(t *testing.T) {
	s := newSPA(t)

	for _, path := range []string{"/", "/index.html", "/providers", "/combos"} {
		rec := get(t, s, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200 (%s)", path, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Errorf("GET %s Content-Type = %q, want text/html", path, ct)
		}
		if !strings.Contains(rec.Body.String(), "<div id=\"root\"") {
			t.Errorf("GET %s did not return the SPA shell", path)
		}
	}

	// the shell must revalidate, or a rebuilt dashboard never reaches the phone
	if cc := get(t, s, "/").Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("index Cache-Control = %q, want no-cache", cc)
	}
}

// A missing asset must 404 — masking it with index.html turns a broken deploy
// into a blank page instead of a visible failure.
func TestSPAMissingAssetIs404(t *testing.T) {
	s := newSPA(t)
	rec := get(t, s, "/assets/nope-123456.js")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing asset = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "text/html") {
		t.Errorf("missing asset returned HTML (%s) — asset miss must not fall back to index.html", ct)
	}
}

// API-shaped paths must stay JSON even though they fall through to the SPA
// catch-all (defence in depth: a removed admin route must not start
// answering the dashboard shell to an unauthenticated caller).
func TestSPANeverSwallowsAPIPaths(t *testing.T) {
	s := newSPA(t)
	for _, path := range []string{"/admin/nope", "/v1/nope"} {
		rec := get(t, s, path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("GET %s Content-Type = %q, want JSON", path, ct)
		}
	}
	// a REGISTERED admin route keeps its auth: it must win over the catch-all
	if rec := get(t, s, "/admin/health"); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /admin/health = %d, want 401 (registered route must beat the SPA catch-all)", rec.Code)
	}
}

// EZLLM_WEB_DIR lets the UI be iterated against a running binary without a Go
// rebuild, and must win over the embed.
func TestSPAWebDirOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"),
		[]byte("<div id=\"root\">from-disk</div>"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EZLLM_WEB_DIR", dir)

	s := New(Options{})
	rec := get(t, s, "/")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "from-disk") {
		t.Fatalf("EZLLM_WEB_DIR override not served: %d %s", rec.Code, rec.Body.String())
	}
}
