package server

// F9 regression: the dashboard's group_by=day bucketed UTC-stamped rows with
// substr(ts,1,10) — midnight UTC — while a usage rule's daily cap evaluated
// its bucket in the RULE's own *time.Location. With any non-UTC rule the two
// disagreed about which day a call belonged to, by the zone offset: the
// dashboard counted a call the cap had already refused, or vice versa.
//
// The fix buckets days in one place — store.CountBucket, the same function the
// meter writes with — in a zone chosen by EZLLM_USAGE_TZ (default) or ?tz=
// (override). These tests prove the dashboard's day boundary matches the
// meter's for a fixed-offset zone AND a DST zone, which is where a naive
// offset-in-SQL implementation would be silently wrong.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

// dayBucketOf reads the day-grouped report and returns a key -> calls map.
func dayBuckets(t *testing.T, h *harness, path string) map[string]int64 {
	t.Helper()
	code, _, body := h.do("GET", path, "tok-admin", "")
	if code != 200 {
		t.Fatalf("%s = %d: %s", path, code, body)
	}
	var resp struct {
		Groups []struct {
			Key   string `json:"k"`
			Calls int64  `json:"calls"`
		} `json:"groups"`
		TZ string `json:"tz"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	out := map[string]int64{}
	for _, g := range resp.Groups {
		out[g.Key] = g.Calls
	}
	return out
}

// The core case: rows stamped either side of the Kuala Lumpur day boundary
// (UTC+8, so the boundary is 16:00 UTC) must bucket on the KL day, not UTC.
func TestDayBucketingUsesRequestedZone(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	// 2026-10-11 15:59 UTC is KL 23:59 on Oct 11; 16:01 UTC is KL 00:01 on
	// Oct 12. A UTC bucketing puts both on Oct 11; KL splits them.
	for _, ts := range []time.Time{
		time.Date(2026, 10, 11, 15, 59, 0, 0, time.UTC),
		time.Date(2026, 10, 11, 16, 1, 0, 0, time.UTC),
	} {
		h.db.RecordCall(store.Call{TS: ts, Client: "c", Account: "up", AccountID: 1,
			Model: "gpt-6-luna", Status: 200})
	}
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}

	from := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	to := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)

	utc := dayBuckets(t, h, "/admin/usage?group_by=day&from="+from+"&to="+to)
	if utc["2026-10-11"] != 2 {
		t.Errorf("UTC default put %v, want both rows on 2026-10-11 (historical behaviour)", utc)
	}

	kl := dayBuckets(t, h, "/admin/usage?group_by=day&from="+from+"&to="+to+"&tz=Asia/Kuala_Lumpur")
	if kl["2026-10-11"] != 1 || kl["2026-10-12"] != 1 {
		t.Fatalf("KL bucketing = %v, want one row on each of 2026-10-11 and 2026-10-12 — the day boundary must be the zone's, not UTC's", kl)
	}
}

// The DST case, which is why the bucketing is NOT a fixed offset in SQL. On
// 2026-03-08 America/New_York springs forward at 07:00 UTC (02:00 EST →
// 03:00 EDT). A bucketing that applied one offset all day would put the
// 06:30 UTC row on the wrong side of the boundary.
func TestDayBucketingIsDSTCorrect(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("zoneinfo unavailable on this host")
	}
	// 06:30 UTC is 01:30 EST on Mar 8; 08:30 UTC is 04:30 EDT on Mar 8.
	// Both are Mar 8 in New York, but Mar 8 in UTC too — so use a pair that
	// straddles the NEW YORK midnight instead: 04:30 UTC Mar 8 is 23:30 Mar 7
	// EST, and 07:30 UTC Mar 8 is 03:30 EDT Mar 8. A frozen-offset
	// implementation would place the second on Mar 7.
	for _, ts := range []time.Time{
		time.Date(2026, 3, 8, 4, 30, 0, 0, time.UTC), // 23:30 EST Mar 7
		time.Date(2026, 3, 8, 7, 30, 0, 0, time.UTC), // 03:30 EDT Mar 8
	} {
		h.db.RecordCall(store.Call{TS: ts, Client: "c", Account: "up", AccountID: 1,
			Model: "gpt-6-luna", Status: 200})
	}
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}

	from := time.Date(2026, 3, 7, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	to := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	got := dayBuckets(t, h, "/admin/usage?group_by=day&from="+from+"&to="+to+"&tz=America/New_York")

	// Independent check: the meter's own bucketing, computed here in the test
	// rather than reusing the code under test.
	want := map[string]int64{}
	for _, ts := range []time.Time{
		time.Date(2026, 3, 8, 4, 30, 0, 0, time.UTC),
		time.Date(2026, 3, 8, 7, 30, 0, 0, time.UTC),
	} {
		want[store.CountBucket(ts, "daily", ny)]++
	}
	if want["2026-03-07"] != 1 || want["2026-03-08"] != 1 {
		t.Fatalf("test fixture assumption broken: want=%v", want)
	}
	if got["2026-03-07"] != 1 || got["2026-03-08"] != 1 {
		t.Fatalf("DST bucketing = %v, want %v — the offset must be evaluated per row, not frozen at query time", got, want)
	}
}

// The response must name the zone it used: a day-grouped number is meaningless
// without it.
func TestDayBucketingEchoesZone(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	h.db.RecordCall(store.Call{TS: time.Now().UTC(), Client: "c", Account: "up", AccountID: 1,
		Model: "gpt-6-luna", Status: 200})
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}
	code, _, body := h.do("GET", "/admin/usage?group_by=day&tz=Asia/Kuala_Lumpur", "tok-admin", "")
	if code != 200 {
		t.Fatalf("usage = %d: %s", code, body)
	}
	var resp struct {
		TZ string `json:"tz"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.TZ != "Asia/Kuala_Lumpur" {
		t.Fatalf("response tz = %q, want the zone the buckets were computed in", resp.TZ)
	}
}

// A bogus tz is a 400, never a silent UTC fallback — the C5 rule, applied here.
func TestDayBucketingRejectsUnresolvableZone(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, _, body := h.do("GET", "/admin/usage?group_by=day&tz=Asia/Kuala_Lumpyr", "tok-admin", "")
	if code != 400 {
		t.Fatalf("bogus tz answered %d, want 400 (never a silent UTC shift): %s", code, body)
	}
	if !strings.Contains(body, "Kuala_Lumpyr") {
		t.Errorf("400 should name the offending tz: %s", body)
	}
}

// tz with a non-day grouping is rejected, not ignored: a silently dropped
// parameter is how a caller believes a number means something it does not.
func TestTZRejectedForNonDayGrouping(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, _, body := h.do("GET", "/admin/usage?group_by=account&tz=Asia/Kuala_Lumpur", "tok-admin", "")
	if code != 400 {
		t.Fatalf("tz with group_by=account answered %d, want 400: %s", code, body)
	}
}

// Other groupings are untouched by the tz work.
func TestOtherGroupingsUnaffected(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	h.db.RecordCall(store.Call{TS: time.Now().UTC(), Client: "c", Account: "up", AccountID: 1,
		Model: "gpt-6-luna", Status: 200})
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}
	for _, g := range []string{"account", "model", "surface", "client", "key", "alias"} {
		code, _, body := h.do("GET", "/admin/usage?group_by="+g, "tok-admin", "")
		if code != 200 {
			t.Errorf("group_by=%s = %d: %s", g, code, body)
		}
	}
}


// The whole point of the env knob: a deployer sets EZLLM_USAGE_TZ once and
// every dashboard day view agrees with the rule's day, with no per-request
// parameter. Options.UsageTZ is exactly what main.go derives from that env var.
func TestEnvUsageTZIsTheDefault(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {},
		func(o *Options) { o.UsageTZ = "Asia/Kuala_Lumpur" })

	for _, ts := range []time.Time{
		time.Date(2026, 10, 11, 15, 59, 0, 0, time.UTC), // KL Oct 11
		time.Date(2026, 10, 11, 16, 1, 0, 0, time.UTC),  // KL Oct 12
	} {
		h.db.RecordCall(store.Call{TS: ts, Client: "c", Account: "up", AccountID: 1,
			Model: "gpt-6-luna", Status: 200})
	}
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}

	from := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	to := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)

	// No tz param at all: the env default must drive the bucketing.
	got := dayBuckets(t, h, "/admin/usage?group_by=day&from="+from+"&to="+to)
	if got["2026-10-11"] != 1 || got["2026-10-12"] != 1 {
		t.Fatalf("env default ignored: %v, want one row on each KL day", got)
	}

	// And an explicit ?tz= still overrides it.
	over := dayBuckets(t, h, "/admin/usage?group_by=day&from="+from+"&to="+to+"&tz=UTC")
	if over["2026-10-11"] != 2 {
		t.Fatalf("?tz= did not override the env default: %v", over)
	}
}

// An unresolvable EZLLM_USAGE_TZ must never break the server: main.go rejects
// it at boot, so this fallback is a last resort for hand-built Options, and the
// per-request path still refuses a bad tz outright.
func TestUnresolvableEnvTZStillServes(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {},
		func(o *Options) { o.UsageTZ = "Not/AZone" })
	code, _, body := h.do("GET", "/admin/usage?group_by=day", "tok-admin", "")
	if code != 200 {
		t.Fatalf("server with a bad env tz answered %d, want it to keep serving: %s", code, body)
	}
}
