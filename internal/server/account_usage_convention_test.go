package server

import (
	"context"
	"strings"
	"testing"
)

// The admin PATCH exposes the per-account Messages usage fact. Absent means
// keep, a valid value lands, and an invalid value is refused by name so an
// operator can see which field was wrong.

func TestAccountPatchUsageConvention(t *testing.T) {
	fake := &fakeUpstream{}
	h := newHarness(t, fake.handler())
	ctx := context.Background()

	acct, err := h.db.AccountByNamespace(ctx, "up")
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if got := acct.UsageConvention; string(got) != "auto" {
		t.Fatalf("seed usage_convention = %q, want auto", got)
	}

	// Absent: an unrelated PATCH leaves the fact alone.
	code, resp := jsonDo(t, h, "PATCH", "/admin/accounts/1", "tok-admin", map[string]any{"notes": "touched"})
	if code != 200 {
		t.Fatalf("unrelated PATCH = %d %s", code, resp)
	}
	if got := mustJSON(t, resp)["usage_convention"]; got != "auto" {
		t.Fatalf("usage_convention after unrelated PATCH = %v, want auto", got)
	}

	// Valid: the value lands and is echoed by GET.
	code, resp = jsonDo(t, h, "PATCH", "/admin/accounts/1", "tok-admin", map[string]any{"usage_convention": "total"})
	if code != 200 {
		t.Fatalf("valid PATCH = %d %s", code, resp)
	}
	if got := mustJSON(t, resp)["usage_convention"]; got != "total" {
		t.Fatalf("usage_convention = %v, want total", got)
	}
	code, resp = jsonDo(t, h, "GET", "/admin/accounts/1", "tok-admin", nil)
	if code != 200 {
		t.Fatalf("GET account = %d %s", code, resp)
	}
	if got := mustJSON(t, resp)["usage_convention"]; got != "total" {
		t.Fatalf("GET usage_convention = %v, want total", got)
	}

	// Invalid: refused by name, and the stored value is unchanged.
	code, resp = jsonDo(t, h, "PATCH", "/admin/accounts/1", "tok-admin", map[string]any{"usage_convention": "exclusive"})
	if code != 422 {
		t.Fatalf("invalid PATCH = %d %s, want 422", code, resp)
	}
	if !strings.Contains(resp, "usage_convention") {
		t.Fatalf("refusal does not name the field: %s", resp)
	}
	code, resp = jsonDo(t, h, "GET", "/admin/accounts/1", "tok-admin", nil)
	if code != 200 || mustJSON(t, resp)["usage_convention"] != "total" {
		t.Fatalf("stored value changed by a refused PATCH: %d %s", code, resp)
	}

	// Reset back to the documented unknown value.
	code, resp = jsonDo(t, h, "PATCH", "/admin/accounts/1", "tok-admin", map[string]any{"usage_convention": "auto"})
	if code != 200 || mustJSON(t, resp)["usage_convention"] != "auto" {
		t.Fatalf("reset PATCH = %d %s", code, resp)
	}
}
