package server

// docx item 13: the flag must reach the browser on the FAILURE path — the
// login page decides whether the first-run hint is relevant before any
// successful login exists. One endpoint, no new public surface.

import (
	"context"
	"encoding/json"
	"net/http"
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
