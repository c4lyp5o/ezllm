package server

// S7 + S8 regressions.
//
// S7: /admin/health discarded its COUNT query errors with `_ =`, so a failed
// query reported accounts: 0 as if that were the truth — an untruth in the one
// endpoint whose job is telling the operator whether the box is healthy. It now
// answers 503 with the reason.
//
// S8: from/to were unbounded, so an admin could force a full-table GROUP BY
// over an arbitrary range — cost that scales with the requested span is cost the
// caller controls, and `limit` was already capped for exactly that reason. The
// span is now capped in one shared helper (parseWindow).

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// A wide window is refused rather than aggregated. This is the case that
// previously scanned the entire ledger from its first row on every request.
func TestUsageRejectsOverCapWindow(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	// 2020 -> now is ~6 years, far past the 400-day cap.
	code, _, body := h.do("GET", "/admin/usage?from=2020-01-01T00:00:00Z", "tok-admin", "")
	if code != 400 {
		t.Fatalf("over-cap window answered %d, want 400: %s", code, body)
	}
	if !strings.Contains(body, "cap") {
		t.Fatalf("400 body should name the cap, got: %s", body)
	}
}

// The default window (no params) still works: the cap must not break the
// dashboard's ordinary load.
func TestUsageDefaultWindowStillServes(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, _, body := h.do("GET", "/admin/usage", "tok-admin", "")
	if code != 200 {
		t.Fatalf("default usage window answered %d, want 200: %s", code, body)
	}
	if !strings.Contains(body, "\"rows\"") && !strings.Contains(body, "\"k\"") {
		t.Logf("usage body: %s", body)
	}
}

// A window inside the cap is served — the bound must be generous enough for
// real admin work (a year of history is a normal ask).
func TestUsageAcceptsWindowInsideCap(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	from := time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	code, _, body := h.do("GET", "/admin/usage?from="+from, "tok-admin", "")
	if code != 200 {
		t.Fatalf("30-day window answered %d, want 200: %s", code, body)
	}
}

// An inverted window is rejected rather than silently returning nothing.
func TestUsageRejectsInvertedWindow(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	now := time.Now().UTC().Format(time.RFC3339)
	future := time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339)
	code, _, body := h.do("GET", "/admin/usage?from="+future+"&to="+now, "tok-admin", "")
	if code != 400 {
		t.Fatalf("inverted window answered %d, want 400: %s", code, body)
	}
}

// The same cap applies to /admin/requests, which shares the helper.
func TestRequestsRejectsOverCapWindow(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, _, body := h.do("GET", "/admin/requests?from=2019-01-01T00:00:00Z", "tok-admin", "")
	if code != 400 {
		t.Fatalf("over-cap requests window answered %d, want 400: %s", code, body)
	}
}

// /admin/health must answer 200 with real counts on a healthy store — the S7
// rewrite changed the helper's shape, so this pins that the happy path is
// unchanged.
func TestAdminHealthServesRealCounts(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, _, body := h.do("GET", "/admin/health", "tok-admin", "")
	if code != 200 {
		t.Fatalf("health answered %d, want 200: %s", code, body)
	}
	// The one account the harness seeds must be counted, not zeroed by a
	// swallowed error.
	if !strings.Contains(body, "\"accounts\":1") {
		t.Fatalf("health body does not report the seeded account: %s", body)
	}
	// lost_rows must be present next to dropped_rows (F6 surface).
	if !strings.Contains(body, "\"lost_rows\"") {
		t.Errorf("health body lacks lost_rows: %s", body)
	}
}
