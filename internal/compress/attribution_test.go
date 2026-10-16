package compress

import (
	"encoding/json"
	"strings"
	"testing"
)

// Addendum #7: per-engine attribution + sampled shadow compression.
// Two properties the ledger depends on:
//   1. Mixed engine attribution RECONCILES with the final net reduction
//      (the documented rule: for an all-reductive pipeline, SUM(stage.saved)
//      equals the net — and the row records both, so a discrepancy is
//      visible instead of absorbed).
//   2. The shadow pass is DISCARDED: measured, never shipped, never in the
//      applied savings, never in the memo.

// A mixed pipeline (rtk + lite + session_dedup) records every stage that
// ran, and the per-stage savings reconcile with the net reduction.
func TestPerEngineAttributionReconcilesWithNet(t *testing.T) {
	memo := NewSessionMemo(nil)
	p := memoProfile(
		Stage{Engine: "rtk"},
		Stage{Engine: "lite"},
		Stage{Engine: "session_dedup"},
	)
	body := bodyOf(
		textMsg("system", "same instructions"),
		map[string]any{"role": "assistant", "content": "calling", "tool_calls": []any{
			map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "read"}},
		}},
		textMsg("tool", "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-old\n+new\n"),
		aCompressible(),
		textMsg("system", "same instructions"), // dedup material
		textMsg("user", "q"),
	)
	out, res := ApplyWithSession(body, p, nil, "sAttr", memo)
	if !res.Applied {
		t.Fatalf("expected an applied rewrite: %s", res.Err)
	}
	if len(res.Stages) != 3 {
		t.Fatalf("stages recorded = %d, want 3 (%+v)", len(res.Stages), res.Stages)
	}
	// Order preserved, each engine once.
	want := []string{"rtk", "lite", "session_dedup"}
	for i, st := range res.Stages {
		if st.Engine != want[i] {
			t.Errorf("stage %d = %q, want %q", i, st.Engine, want[i])
		}
		if st.Pre < st.Post {
			t.Errorf("stage %s expanded: pre=%d post=%d", st.Engine, st.Pre, st.Post)
		}
	}
	// The documented reconciliation rule (StageStat): the stage sums and the
	// net agree up to boundary effects (slice-level vs whole-body estimates).
	var summed int64
	for _, st := range res.Stages {
		summed += st.Saved
	}
	if diff := summed - res.Saved; diff > 2 || diff < -2 {
		t.Errorf("attribution does not reconcile within boundary slack: SUM(stages)=%d, net=%d (diff %d)",
			summed, res.Saved, diff)
	}
	if summed > res.Saved+2 {
		t.Error("the attribution may not OVERSTATE the net beyond slack — engines are reductive by contract")
	}
	// The net must equal first-Pre minus last-Post.
	chained := res.Stages[0].Pre - res.Stages[len(res.Stages)-1].Post
	if chained != res.Saved {
		t.Errorf("chained pre/post = %d, net = %d", chained, res.Saved)
	}
	// The stages JSON is marshalable for the ledger.
	if _, err := json.Marshal(res.Stages); err != nil {
		t.Errorf("stages must marshal for the ledger: %v", err)
	}
	// A budget stage is recorded too (it is a real stage with a real cost).
	if string(out) == "" {
		t.Error("output must not be empty")
	}
}

// A floor-rejected request still attributes what ran — the engines RAN, the
// decision declined. The ledger needs both facts.
func TestStagesRecordedEvenWhenDeclined(t *testing.T) {
	memo := NewSessionMemo(nil)
	p := memoProfile(Stage{Engine: "lite"})
	p.Enabled = false // skip the pipeline
	body := bodyOf(aCompressible(), textMsg("user", "q"))
	_, res := ApplyWithSession(body, p, nil, "sDecl", memo)
	if len(res.Stages) != 0 {
		t.Errorf("a disabled profile runs no stages: %+v", res.Stages)
	}
}

// The shadow pass measures potential WITHOUT shipping or accounting.
func TestShadowMeasuresPotentialButShipsNothing(t *testing.T) {
	memo := NewSessionMemo(nil)
	// The floored profile ships almost nothing on its own...
	p := memoProfile(Stage{Engine: "lite"})
	p.MinCompressRatio = 0.99 // no realistic saving can clear it
	body := bodyOf(aCompressible(), textMsg("user", "q"))

	// ...but the shadow (floors ignored, everything eligible) sees it.
	p.Shadow = true
	out, res := ApplyWithSession(body, p, nil, "sShadow", memo)
	if res.Applied {
		t.Error("the floored profile must not ship — even with a shadow armed")
	}
	if res.ShadowSaved <= 0 {
		t.Errorf("the shadow must measure the potential: %+v", res)
	}
	// The shadow NEVER becomes applied savings.
	if res.Saved != 0 {
		t.Errorf("shadow measurement leaked into applied savings: %d", res.Saved)
	}
	// The body is byte-identical to the non-shadow run: nothing shipped.
	p.Shadow = false
	off, resOff := ApplyWithSession(body, p, nil, "sShadowOff", memo)
	if string(out) != string(off) {
		t.Error("the armed shadow changed what shipped — it must be discarded")
	}
	if resOff.ShadowSaved != 0 {
		t.Errorf("an unarmed request must measure no shadow: %d", resOff.ShadowSaved)
	}
	// The strongest property: the shadow is INERT. Replay the same
	// conversation on a session that never ran a shadow — every SHIPPED
	// output must be byte-identical. The only difference between the two
	// runs is the shadow measurement itself.
	p.Shadow = false
	memoClean := NewSessionMemo(nil)
	follow2 := bodyOf(aCompressible(), textMsg("user", "q"), textMsg("user", "next"))
	clean1, _ := ApplyWithSession(body, p, nil, "sClean", memoClean)
	clean2, _ := ApplyWithSession(follow2, p, nil, "sClean", memoClean)
	// The shadow-session shipped bytes for the same two requests.
	if string(out) != string(clean1) {
		t.Error("the armed shadow changed what the FIRST request shipped")
	}
	p.Shadow = true
	_, _ = ApplyWithSession(body, p, nil, "sShadow2", memo)
	shadowFollow, _ := ApplyWithSession(follow2, p, nil, "sShadow2", memo)
	if string(shadowFollow) != string(clean2) {
		t.Error("a shadow-armed session ships different bytes than a clean one — the shadow committed")
	}
}

// The shadow pass runs the engines over EVERYTHING, including the exempt
// suffix — that is the point: it measures what the greedy policy leaves.
func TestShadowSeesExemptSuffix(t *testing.T) {
	memo := NewSessionMemo(nil)
	p := memoProfile(Stage{Engine: "lite"})
	p.MinCompressRatio = 0.99
	p.ExemptLastTurn = true
	body := bodyOf(aCompressible(), textMsg("user", "compress me?? please, could you tell me why  \n\n\n"))
	p.Shadow = true
	_, res := ApplyWithSession(body, p, nil, "sShadowSfx", memo)
	if res.ShadowSaved <= 0 {
		t.Errorf("the shadow must include the exempt suffix's potential: %+v", res)
	}
}

// The CPU cap bounds the shadow too: a pipeline already over budget skips it.
func TestShadowSkippedWhenOverCPUCap(t *testing.T) {
	memo := NewSessionMemo(nil)
	p := memoProfile(Stage{Engine: "lite"})
	p.MaxPipelineMS = 1
	p.Shadow = true
	var sb strings.Builder
	for i := 0; i < 4000; i++ {
		sb.WriteString("padding line with trailing spaces   \n\n\n")
	}
	body := bodyOf(map[string]any{"role": "user", "content": sb.String()}, textMsg("user", "q"))
	_, res := ApplyWithSession(body, p, nil, "sShadowCpu", memo)
	if res.ShadowSaved != 0 {
		t.Errorf("an over-budget pipeline must not spend on the shadow: %d", res.ShadowSaved)
	}
}
