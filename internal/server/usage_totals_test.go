package server

// The totals row's P50 was never computed: handleUsage summed every count
// column but medians don't add up, so the dashboard's totals card read
// "0ms · not measured yet" over a window with thousands of measured TTFTs.
// The totals row also never summed context_pre/context_saved, so its
// context_saved read stayed 0 no matter what the groups said. Both are
// window-level facts (one whole-window median, plain sums) — asserted here
// through the HTTP boundary, which is where the card reads them.

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

func TestUsageTotalsCarryWindowMedianAndContext(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	now := time.Now()
	// Three measured TTFTs in one window (median = 300) plus one unmeasured
	// row that must not shift it, and a context pair the totals previously
	// dropped on the floor.
	for _, c := range []store.Call{
		{TS: now, Client: "c", Account: "a", AccountID: 1, Model: "m", Status: 200,
			TTFTms: i64ptr(100), ContextTokensPre: 1000, ContextTokensSaved: 120},
		{TS: now, Client: "c", Account: "a", AccountID: 1, Model: "m", Status: 200,
			TTFTms: i64ptr(300), ContextTokensPre: 500, ContextTokensSaved: 80},
		{TS: now, Client: "c", Account: "b", AccountID: 2, Model: "m", Status: 200,
			TTFTms: i64ptr(500), ContextTokensPre: 300, ContextTokensSaved: 50},
		{TS: now, Client: "c", Account: "b", AccountID: 2, Model: "m", Status: 200},
	} {
		h.db.RecordCall(c)
	}
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}

	code, _, body := h.do("GET", "/admin/usage?group_by=account", "tok-admin", "")
	if code != http.StatusOK {
		t.Fatalf("usage = %d: %s", code, body)
	}
	var resp struct {
		Groups []store.UsageRow `json:"groups"`
		Totals store.UsageRow   `json:"totals"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("body is not JSON: %s", body)
	}
	if resp.Totals.P50TTFTms != 300 {
		t.Errorf("totals.p50_ttft_ms = %d, want 300 (median of 100/300/500 over the window — not 0, not a group sum)", resp.Totals.P50TTFTms)
	}
	if resp.Totals.ContextPre != 1800 || resp.Totals.ContextSaved != 250 {
		t.Errorf("totals context = pre %d / saved %d, want 1800/250 (the groups sum; totals used to ship 0)", resp.Totals.ContextPre, resp.Totals.ContextSaved)
	}
	// The window median must equal the median of ALL measured rows together,
	// not the sum of the per-group medians (100-group and 500-group → 600).
	var groupSum int64
	for _, g := range resp.Groups {
		groupSum += g.P50TTFTms
	}
	if groupSum == resp.Totals.P50TTFTms {
		t.Errorf("totals p50 %d equals the group-median SUM — medians are not additive", resp.Totals.P50TTFTms)
	}
}

func i64ptr(v int64) *int64 { return &v }
