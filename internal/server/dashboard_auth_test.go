package server

// First-boot setup (C1): a fresh install has no password. The dashboard shows
// only its set-password page, and setting it needs the one-time code from the
// boot log, so the first visitor cannot claim the instance.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testSetupCode = "ABCD-EFGH"

func withSetupCode(o *Options) { o.SetupCode = testSetupCode }

// setupDashboard completes first-boot setup directly in the store, for tests
// about what happens after it.
func setupDashboard(t *testing.T, h *harness, pw string) {
	t.Helper()
	if err := h.db.CompleteDashboardSetup(context.Background(), pw); err != nil {
		t.Fatalf("setup: %v", err)
	}
}

func firstBoot(t *testing.T, h *harness) bool {
	t.Helper()
	code, _, body := h.do("GET", "/admin/setup", "", "")
	if code != 200 {
		t.Fatalf("GET /admin/setup = %d: %s", code, body)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("setup status body: %v (%s)", err, body)
	}
	v, ok := m["first_boot"].(bool)
	if !ok {
		t.Fatalf("first_boot missing: %s", body)
	}
	return v
}

func TestFirstBootSetupFlow(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {}, withSetupCode)
	clearLoginAttempts("192.0.2.1")
	defer clearLoginAttempts("192.0.2.1")

	if !firstBoot(t, h) {
		t.Fatal("fresh install must report first_boot=true")
	}
	// No password exists, so login is refused as setup-required — the old
	// factory password included.
	for _, pw := range []string{"STRONGPASSWORD", ""} {
		code, _, body := h.do("POST", "/admin/login", "", `{"password":"`+pw+`"}`)
		if code != http.StatusConflict || !strings.Contains(body, "setup_required") {
			t.Fatalf("login(%q) before setup = %d %s, want 409 setup_required", pw, code, body)
		}
	}

	code, _, body := h.do("POST", "/admin/setup", "", `{"setup_code":"WXYZ-2345","password":"a-good-long-password"}`)
	if code != http.StatusUnauthorized {
		t.Fatalf("wrong setup code = %d %s, want 401", code, body)
	}
	// Correct code (typed loosely), password too short: refused, flag kept.
	code, _, body = h.do("POST", "/admin/setup", "", `{"setup_code":" abcdefgh ","password":"short"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("short password = %d %s, want 400", code, body)
	}
	if !firstBoot(t, h) {
		t.Fatal("a refused setup must not flip first_boot")
	}

	code, hdr, body := h.do("POST", "/admin/setup", "", `{"setup_code":"abcd-efgh","password":"a-good-long-password"}`)
	if code != 200 {
		t.Fatalf("setup = %d %s, want 200", code, body)
	}
	if firstBoot(t, h) {
		t.Fatal("setup must flip first_boot")
	}
	cookie, err := http.ParseSetCookie(hdr.Get("Set-Cookie"))
	if err != nil || cookie.Name != "ezllm_session" || !cookie.HttpOnly {
		t.Fatalf("setup must sign the caller in with the session cookie: %v %+v", err, cookie)
	}
	req := httptest.NewRequest("GET", "/admin/overview", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Fatalf("setup session rejected: %d %s", rec.Code, rec.Body.String())
	}

	// One-shot: the code cannot reset the password afterwards.
	code, _, body = h.do("POST", "/admin/setup", "", `{"setup_code":"ABCD-EFGH","password":"attacker-password-1"}`)
	if code != http.StatusConflict || !strings.Contains(body, "setup_complete") {
		t.Fatalf("second setup = %d %s, want 409 setup_complete", code, body)
	}
	if code, _, body := h.do("POST", "/admin/login", "", `{"password":"a-good-long-password"}`); code != 200 {
		t.Fatalf("login with the setup password = %d %s", code, body)
	}
}

func TestSetupRefusedWithoutIssuedCode(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	clearLoginAttempts("192.0.2.1")
	defer clearLoginAttempts("192.0.2.1")
	// An empty server-side code must never match an empty submitted one.
	code, _, body := h.do("POST", "/admin/setup", "", `{"setup_code":"","password":"a-good-long-password"}`)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("setup without an issued code = %d %s, want 503", code, body)
	}
	if !firstBoot(t, h) {
		t.Fatal("refused setup must leave first_boot set")
	}
}

func TestSetupCodeGuessesAreRateLimited(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {}, withSetupCode)
	clearLoginAttempts("192.0.2.1")
	defer clearLoginAttempts("192.0.2.1")
	for i := 0; i < 5; i++ {
		if code, _, _ := h.do("POST", "/admin/setup", "", `{"setup_code":"ZZZZ-ZZZZ","password":"a-good-long-password"}`); code != http.StatusUnauthorized {
			t.Fatalf("guess %d = %d, want 401", i+1, code)
		}
	}
	// Even the right code is refused once the window is spent.
	code, _, body := h.do("POST", "/admin/setup", "", `{"setup_code":"ABCD-EFGH","password":"a-good-long-password"}`)
	if code != http.StatusTooManyRequests {
		t.Fatalf("sixth attempt = %d %s, want 429", code, body)
	}
}

func TestNewSetupCodeShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		c, err := NewSetupCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(c) != 9 || c[4] != '-' {
			t.Fatalf("code %q, want XXXX-XXXX", c)
		}
		for _, r := range strings.ReplaceAll(c, "-", "") {
			if !strings.ContainsRune(setupCodeAlphabet, r) {
				t.Fatalf("code %q has %q outside the alphabet", c, r)
			}
		}
		seen[c] = true
	}
	if len(seen) < 45 {
		t.Fatalf("only %d distinct codes in 50 draws", len(seen))
	}
}

func TestDashboardLoginCookieSecureBehindProxy(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	setupDashboard(t, h, "proxy-test-password")
	req := httptest.NewRequest("POST", "/admin/login", strings.NewReader(`{"password":"proxy-test-password"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("login = %d: %s", rec.Code, rec.Body.String())
	}
	cookie, err := http.ParseSetCookie(rec.Header().Get("Set-Cookie"))
	if err != nil {
		t.Fatalf("parse cookie: %v", err)
	}
	if !cookie.Secure {
		t.Fatal("cookie must be Secure when the proxy reports https")
	}
}

func TestDashboardLoginSetsHttpOnlyCookieAndCookieAuthenticates(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	setupDashboard(t, h, "cookie-test-password")
	code, hdr, body := h.do("POST", "/admin/login", "", `{"password":"cookie-test-password"}`)
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
	setupDashboard(t, h, "rate-limit-password")
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

// H3: the per-IP limiter cannot stop many source addresses hashing at once,
// so bcrypt work is capped globally. With every slot busy, a login is
// refused with 429 + Retry-After instead of starting another bcrypt.
func TestDashboardLoginRefusedWhenBcryptSlotsBusy(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	setupDashboard(t, h, "slot-test-password")
	clearLoginAttempts("192.0.2.1")
	defer clearLoginAttempts("192.0.2.1")

	old := bcryptWait
	bcryptWait = 20 * time.Millisecond
	defer func() { bcryptWait = old }()
	for i := 0; i < cap(bcryptSlots); i++ {
		bcryptSlots <- struct{}{}
	}
	code, hdr, body := h.do("POST", "/admin/login", "", `{"password":"slot-test-password"}`)
	for i := 0; i < cap(bcryptSlots); i++ {
		<-bcryptSlots
	}
	if code != http.StatusTooManyRequests || hdr.Get("Retry-After") == "" {
		t.Fatalf("login with every slot busy = %d (Retry-After %q) %s, want 429 + Retry-After", code, hdr.Get("Retry-After"), body)
	}
	// A refusal for load is not a failed guess: it must not burn the IP's budget.
	if !loginAllowed("192.0.2.1") {
		t.Fatal("a busy refusal must not count as a failed attempt")
	}
	if code, _, body := h.do("POST", "/admin/login", "", `{"password":"slot-test-password"}`); code != 200 {
		t.Fatalf("login once slots free = %d %s", code, body)
	}
	if len(bcryptSlots) != 0 {
		t.Fatalf("slots leaked: %d still held", len(bcryptSlots))
	}
}

// H3: empty passwords used to be exempt from the limiter (the old
// default-password probe sent them), leaving an unthrottled bcrypt loop.
func TestDashboardLoginEmptyPasswordsAreRateLimited(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	setupDashboard(t, h, "empty-probe-password")
	clearLoginAttempts("192.0.2.1")
	defer clearLoginAttempts("192.0.2.1")
	for i := 0; i < 5; i++ {
		if code, _, _ := h.do("POST", "/admin/login", "", `{"password":""}`); code != http.StatusUnauthorized {
			t.Fatalf("empty attempt %d = %d, want 401", i+1, code)
		}
	}
	if code, _, body := h.do("POST", "/admin/login", "", `{"password":""}`); code != http.StatusTooManyRequests {
		t.Fatalf("sixth empty attempt = %d %s, want 429", code, body)
	}
}
