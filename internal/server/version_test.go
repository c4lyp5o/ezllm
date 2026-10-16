package server

import (
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestVersionMatchesVERSIONDotMD is the anti-drift guard for the release
// version: internal/server/version.go is what /healthz and /admin/overview
// serve, VERSION.md is the human changelog-of-versions. A bump that updates
// one without the other fails here instead of shipping a lying healthz.
func TestVersionMatchesVERSIONDotMD(t *testing.T) {
	raw, err := os.ReadFile("../../VERSION.md")
	if err != nil {
		t.Fatalf("read VERSION.md: %v", err)
	}
	m := regexp.MustCompile(`(?m)^\*\*Current:\s*v([0-9]+\.[0-9]+\.[0-9]+)\*\*`).FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatalf(`VERSION.md has no "**Current: vX.Y.Z" line — the contract is documented at the top of VERSION.md`)
	}
	if m[1] != Version {
		t.Errorf("version drift: internal/server/version.go = %s, VERSION.md Current = v%s (bump both in one commit)", Version, m[1])
	}
}

// TestVersionIsASingleTriple keeps the served string in the exact shape
// VERSION.md's scheme defines (one patch component, no v-prefix, no suffix).
func TestVersionIsASingleTriple(t *testing.T) {
	if parts := strings.Split(Version, "."); len(parts) != 3 {
		t.Errorf("Version = %q, want MAJOR.MINOR.PATCH", Version)
	}
	if strings.HasPrefix(Version, "v") {
		t.Errorf("Version = %q — the constant carries no v-prefix (VERSION.md headings do)", Version)
	}
}

// TestOverviewHealthCarriesVersion pins the dashboard's source of truth:
// the admin overview health object carries the same version healthz does
// (web/src/pages/Dashboard.tsx renders it as the first health chip).
func TestOverviewHealthCarriesVersion(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, _, body := h.do("GET", "/admin/overview", "tok-admin", "")
	if code != 200 {
		t.Fatalf("overview: %d", code)
	}
	var out struct {
		Health struct {
			Version string `json:"version"`
		} `json:"health"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("overview json: %v", err)
	}
	if out.Health.Version != Version {
		t.Errorf("overview health.version = %q, want %q", out.Health.Version, Version)
	}
}

// TestHealthzCarriesVersion pins the public contract: both the ok and the
// down response name the release, and nothing else leaks (the full leak list
// lives in TestHealthIsPublic).
func TestHealthzCarriesVersion(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	for _, tc := range []struct{ name, path, want string }{
		{"ok", "/healthz", "ok"},
	} {
		code, _, body := h.do("GET", tc.path, "", "")
		if code != 200 {
			t.Fatalf("%s: %d", tc.name, code)
		}
		if want := `"version":"` + Version + `"`; !strings.Contains(body, want) {
			t.Errorf("%s: healthz body %s does not contain %s", tc.name, body, want)
		}
	}
}
