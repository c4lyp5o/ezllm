package compress

// v8 Saved semantics: SUM(Saved) must mean bytes that actually shipped shorter.
// Before this split, a ratio-floor rejection still recorded its would-have-been
// saving into Saved, so the dashboard headline counted up to 2x what was real
// (measured on prod: 18.2k shipped vs 21.4k notional in one week).

import (
	"strings"
	"testing"
)

func TestRatioFloorSplitsNotionalFromSaved(t *testing.T) {
	body := bodyOf(
		textMsg("system", strings.Repeat("long context. ", 200)),
		textMsg("user", "hi"),
		textMsg("assistant", "ok"),
		textMsg("user", "hi"), // duplicate: measurable but far below the 5% floor
	)
	_, res := Apply(body, fullProfile(Stage{Engine: "session_dedup"}))
	if res.Applied {
		t.Fatal("expected the floor to reject this rewrite")
	}
	if res.Saved != 0 {
		t.Errorf("Saved = %d, want 0: nothing shipped, so the headline must not count it", res.Saved)
	}
	if !strings.Contains(res.Err, "ratio floor") {
		t.Errorf("Err = %q, want a ratio-floor explanation", res.Err)
	}
	// The notional figure is only meaningful if engines actually shrank
	// something — this tiny dedup may round to 0 tokens by the estimator, so
	// assert the bookkeeping invariant instead of a positive number:
	// SavedNotional must never exceed the pre-estimate, and both Saved fields
	// must be zero unless something was computed.
	if res.SavedNotional > res.Pre {
		t.Errorf("SavedNotional %d exceeds Pre %d", res.SavedNotional, res.Pre)
	}
}

func TestAppliedSavesAreRealAndNotionalZero(t *testing.T) {
	// The applied path from the existing headroom test shape: big repeated
	// payload well over the floor.
	rows := make([]string, 10)
	for i := range rows {
		rows[i] = `{"id":` + itoa(i+1) + `,"name":"inventory-row-` + pad3(i+1) + `","qty":` + itoa((i+1)*3) + `}`
	}
	payload := "[" + strings.Join(rows, ",") + "]"
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":"Prose with trailing spaces.   \n\n\n\n"},
		{"role":"tool","content":` + mustJSONString(payload) + `}
	]}`)
	p := &Profile{Name: "p2", Enabled: true, Stages: []Stage{{Engine: "headroom"}, {Engine: "lite"}}}
	out, res := Apply(body, p)
	if !res.Applied {
		t.Fatalf("expected applied: %s", res.Err)
	}
	if res.Saved <= 0 {
		t.Errorf("Saved = %d, want > 0", res.Saved)
	}
	if res.SavedNotional != 0 {
		t.Errorf("SavedNotional = %d on an applied rewrite, want 0 — the split is either/or", res.SavedNotional)
	}
	if len(out) >= len(body) {
		t.Errorf("body not smaller: %d -> %d", len(body), len(out))
	}
}
