package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A signed-in operator's browser visiting another origin of the same site
// (another port on the host, a sibling subdomain) carries the SameSite=Strict
// session cookie. That page must not be able to mutate admin state with it;
// same-origin dashboard calls, bearer clients and non-browser cookie clients
// keep working.
func TestAdminCookieRefusesCrossOriginWrites(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	setupDashboard(t, h, "csrf-test-dummy-password")
	code, hdr, body := h.do("POST", "/admin/login", "", `{"password":"csrf-test-dummy-password"}`)
	if code != 200 {
		t.Fatalf("login = %d %s", code, body)
	}
	cookie, err := http.ParseSetCookie(hdr.Get("Set-Cookie"))
	if err != nil {
		t.Fatal(err)
	}
	mint := func(name string, hdrs map[string]string, withCookie bool, bearer string) int {
		req := httptest.NewRequest("POST", "/admin/tokens",
			strings.NewReader(`{"name":"`+name+`","roles":["admin"],"token":"minted-`+name+`"}`))
		req.Host = "gw.example:8443"
		for k, v := range hdrs {
			req.Header.Set(k, v)
		}
		if withCookie {
			req.AddCookie(cookie)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		return rec.Code
	}
	usable := func(name string) bool {
		code, _, _ := h.do("GET", "/admin/accounts", "minted-"+name, "")
		return code == 200
	}

	for _, c := range []struct {
		name string
		hdrs map[string]string
	}{
		{"same-site-port", map[string]string{"Origin": "http://gw.example:9000", "Sec-Fetch-Site": "same-site", "Content-Type": "text/plain"}},
		{"cross-site", map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}},
		{"origin-only", map[string]string{"Origin": "http://gw.example:9000", "Content-Type": "text/plain"}},
	} {
		if code := mint(c.name, c.hdrs, true, ""); code != http.StatusForbidden {
			t.Errorf("%s: cookie write from another origin = %d, want 403", c.name, code)
		}
		if usable(c.name) {
			t.Errorf("%s: the cross-origin page minted a working admin token", c.name)
		}
	}

	for _, c := range []struct {
		name   string
		hdrs   map[string]string
		cookie bool
		bearer string
	}{
		{"dashboard", map[string]string{"Origin": "http://gw.example:8443", "Sec-Fetch-Site": "same-origin", "Content-Type": "application/json"}, true, ""},
		{"cli-cookie", map[string]string{"Content-Type": "application/json"}, true, ""},
		{"bearer", map[string]string{"Origin": "https://tool.example", "Sec-Fetch-Site": "cross-site", "Content-Type": "application/json"}, false, "tok-admin"},
	} {
		if code := mint(c.name, c.hdrs, c.cookie, c.bearer); code != http.StatusCreated {
			t.Errorf("%s: write = %d, want 201", c.name, code)
		}
	}

	// Sign-out reads the same cookie: another origin cannot end the session.
	req := httptest.NewRequest("POST", "/admin/logout", nil)
	req.Header.Set("Sec-Fetch-Site", "same-site")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin logout = %d, want 403", rec.Code)
	}
	if code := mint("after-logout", map[string]string{"Sec-Fetch-Site": "same-origin"}, true, ""); code != http.StatusCreated {
		t.Errorf("session ended by a cross-origin logout: write = %d", code)
	}
}
