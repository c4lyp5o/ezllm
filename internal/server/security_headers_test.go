package server

import (
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"net/http"
	"strings"
	"testing"
)

// M6: the admin shell was served with no framing or script policy. It now
// refuses to be framed and allows scripts only from this origin plus the
// inline theme bootstrap, admitted by its exact hash.
func TestDashboardShellSecurityHeaders(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	srv := New(Options{}) // same embedded dist the harness serves
	index, err := fs.ReadFile(srv.web, "index.html")
	if err != nil {
		t.Skip("dashboard not built")
	}
	m := inlineScriptRe.FindSubmatch(index)
	if m == nil {
		t.Fatal("index.html has no inline bootstrap script to hash")
	}
	sum := sha256.Sum256(m[1])
	wantHash := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"

	for _, path := range []string{"/", "/providers", "/settings"} {
		code, hdr, _ := h.do("GET", path, "", "")
		if code != 200 {
			t.Fatalf("GET %s = %d", path, code)
		}
		csp := hdr.Get("Content-Security-Policy")
		for _, want := range []string{"frame-ancestors 'none'", "default-src 'self'", "object-src 'none'", wantHash} {
			if !strings.Contains(csp, want) {
				t.Fatalf("GET %s CSP %q missing %q", path, csp, want)
			}
		}
		if strings.Contains(strings.SplitN(strings.SplitN(csp, "script-src", 2)[1], ";", 2)[0], "unsafe-inline") {
			t.Fatalf("script-src must not allow unsafe-inline: %q", csp)
		}
		if hdr.Get("X-Frame-Options") != "DENY" {
			t.Fatalf("GET %s X-Frame-Options = %q, want DENY", path, hdr.Get("X-Frame-Options"))
		}
	}

	// Every response — API errors included — refuses MIME sniffing.
	code, hdr, _ := h.do("GET", "/v1/models", "", "")
	if code != http.StatusUnauthorized || hdr.Get("X-Content-Type-Options") != "nosniff" || hdr.Get("Referrer-Policy") != "same-origin" {
		t.Fatalf("API response %d: nosniff=%q referrer=%q", code, hdr.Get("X-Content-Type-Options"), hdr.Get("Referrer-Policy"))
	}
}

// Audit finding 7: the Services panel previews generated media from data:
// URIs, blob: object URLs (a raw-bytes response) and the provider's own https
// URLs. img-src admitted only self and data:, and with no media-src audio and
// video fell back to default-src 'self', so successful generations showed
// broken previews. Scripts stay locked down (the test above).
func TestDashboardCSPAdmitsMediaPreviews(t *testing.T) {
	csp := New(Options{}).dashboardCSP()
	directive := func(name string) []string {
		for _, d := range strings.Split(csp, ";") {
			f := strings.Fields(d)
			if len(f) > 0 && f[0] == name {
				return f[1:]
			}
		}
		return nil
	}
	for _, name := range []string{"img-src", "media-src"} {
		got := directive(name)
		for _, want := range []string{"'self'", "data:", "blob:", "https:"} {
			found := false
			for _, g := range got {
				found = found || g == want
			}
			if !found {
				t.Errorf("%s %v missing %s (CSP %q)", name, got, want, csp)
			}
		}
	}
}
