package server

// docx item 13: the flag must reach the browser on the FAILURE path — the
// login page decides whether the first-run hint is relevant before any
// successful login exists. One endpoint, no new public surface.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/store"
)

func loginFlag(t *testing.T, body string) any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("login body is not JSON: %v (%s)", err, body)
	}
	e, _ := m["error"].(map[string]any)
	if e == nil {
		t.Fatalf("no error object in %s", body)
	}
	return e["default_password"]
}

func TestDashboardLoginCarriesDefaultPasswordFlag(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()

	// Fresh harness DB bootstraps the factory password; a wrong attempt
	// answers 401 WITH the flag so the page can show the hint.
	code, _, body := h.do("POST", "/admin/login", "", `{"password":"definitely-wrong"}`)
	if code != 401 {
		t.Fatalf("wrong password = %d, want 401: %s", code, body)
	}
	if loginFlag(t, body) != true {
		t.Fatalf("fresh install must report default_password=true on the 401: %s", body)
	}

	// Change it: the flag must flip (the hash compare is what changed).
	if err := h.db.ChangeDashboardPassword(ctx, store.DefaultDashboardPassword, "a-much-longer-password"); err != nil {
		t.Fatalf("change password: %v", err)
	}
	code, _, body = h.do("POST", "/admin/login", "", `{"password":"definitely-wrong"}`)
	if code != 401 || loginFlag(t, body) != false {
		t.Fatalf("after a change the flag must read false: %d %s", code, body)
	}

	// The success path carries the flag too, and still returns the token
	// (the flag is additive — nothing about the contract moved).
	code, _, body = h.do("POST", "/admin/login", "", `{"password":"a-much-longer-password"}`)
	if code != 200 {
		t.Fatalf("correct password = %d, want 200: %s", code, body)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("success body: %v (%s)", err, body)
	}
	if tok, _ := m["token"].(string); tok == "" {
		t.Fatalf("success body lost the token: %s", body)
	}
	if m["default_password"] != false {
		t.Fatalf("success body flag = %v, want false after the change", m["default_password"])
	}
	if !strings.Contains(body, "token") {
		t.Fatalf("success body shape changed: %s", body)
	}
}

func TestDashboardLoginSetsHttpOnlyCookieAndCookieAuthenticates(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, hdr, body := h.do("POST", "/admin/login", "", `{"password":"STRONGPASSWORD"}`)
	if code != 200 {
		t.Fatalf("login = %d: %s", code, body)
	}
	raw := hdr.Get("Set-Cookie")
	if raw == "" {
		t.Fatal("login did not set a session cookie")
	}
	cookie, err := http.ParseSetCookie(raw)
	if err != nil {
		t.Fatalf("parse cookie: %v", err)
	}
	if cookie.Name != "ezllm_session" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie = %+v, want ezllm_session + HttpOnly + SameSite=Strict", cookie)
	}

	// The cookie alone (no Authorization header) must open an admin route.
	req := httptest.NewRequest("GET", "/admin/overview", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Fatalf("cookie auth rejected: %d %s", rec.Code, rec.Body.String())
	}
}

func TestDashboardLoginRateLimitsPerIP(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	clearLoginAttempts("192.0.2.1")
	defer clearLoginAttempts("192.0.2.1")
	for i := 0; i < 5; i++ {
		code, _, _ := h.do("POST", "/admin/login", "", `{"password":"wrong"}`)
		if code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i+1, code)
		}
	}
	code, _, body := h.do("POST", "/admin/login", "", `{"password":"wrong"}`)
	if code != http.StatusTooManyRequests {
		t.Fatalf("sixth attempt = %d, want 429: %s", code, body)
	}
}
