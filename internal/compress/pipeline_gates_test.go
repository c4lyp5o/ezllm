package compress

import (
	"strings"
	"testing"
	"time"
)

// Addendum #5: the remaining request-time gates.
//   - absolute reduction: a ratio that passes on a tiny saving is still not
//     worth the bytes
//   - CPU limit: the stage loop stops at a boundary and ships what ran
//   - persisted decisions: the Result names the immutable ruleset version

// A rewrite that passes the ratio but removes too few tokens must NOT ship.
func TestAbsoluteFloorRejectsTinySaving(t *testing.T) {
	memo := NewSessionMemo(nil)
	p := memoProfile(Stage{Engine: "lite"})
	p.MinAbsoluteTokens = 10_000 // nothing realistic can clear it
	// Content with a REAL but small lite-able saving.
	small := map[string]any{"role": "user", "content": "question  \nmore text   \n"}
	body := bodyOf(small, textMsg("user", "q"))
	out, res := ApplyWithSession(body, p, nil, "sAbs", memo)
	if res.Applied {
		t.Errorf("a saving below the absolute floor must not ship: %+v", res)
	}
	if string(out) != string(body) {
		t.Error("a floor-rejected request must ship the body byte-identical")
	}
	if !strings.Contains(res.Err, "absolute floor") {
		t.Errorf("the note must name the absolute floor, got %q", res.Err)
	}
	if res.SavedNotional <= 0 {
		t.Error("the rejected-but-real saving belongs in SavedNotional")
	}
}

// The same content with a sane floor ships.
func TestAbsoluteFloorAllowsRealSaving(t *testing.T) {
	memo := NewSessionMemo(nil)
	p := memoProfile(Stage{Engine: "lite"})
	p.MinAbsoluteTokens = 1 // any real saving clears it
	body := bodyOf(aCompressible(), textMsg("user", "q"))
	out, res := ApplyWithSession(body, p, nil, "sAbs2", memo)
	if !res.Applied {
		t.Fatalf("a real saving must ship: %s", res.Err)
	}
	if res.Saved <= 0 {
		t.Error("the shipped saving must be recorded")
	}
	if msgContent(t, out, 0) == msgContent(t, body, 0) {
		t.Error("the content must actually be transformed")
	}
}

// The CPU cap stops the pipeline at a stage boundary and ships what ran.
// The observable: stage 2 (session_dedup) never runs, so a duplicate system
// message survives even though the profile would have dropped it.
func TestCPUCapStopsAtStageBoundary(t *testing.T) {
	memo := NewSessionMemo(nil)
	p := memoProfile(Stage{Engine: "lite"}, Stage{Engine: "session_dedup"})
	p.MaxPipelineMS = 1 // 1ms: a big body's marshal alone exceeds it after stage 1
	var sb strings.Builder
	for i := 0; i < 4000; i++ {
		sb.WriteString("padding line with trailing spaces   \n\n\n")
	}
	big := map[string]any{"role": "user", "content": sb.String()}
	body := bodyOf(
		textMsg("system", "same instructions"),
		big,
		textMsg("system", "same instructions"), // a duplicate dedup would drop
		textMsg("user", "q"),
	)
	out, res := ApplyWithSession(body, p, nil, "sCpu", memo)
	if !res.Applied {
		t.Fatalf("stage 1's saving must still ship: %s", res.Err)
	}
	if !strings.Contains(res.Err, "cpu cap") {
		t.Errorf("the note must record the cap, got %q", res.Err)
	}
	// session_dedup never ran: the duplicate system message is still there.
	msgs := decodeMsgs(t, out)
	dupes := 0
	for _, m := range msgs {
		if mm, ok := m.(map[string]any); ok {
			if s, _ := mm["content"].(string); s == "same instructions" {
				dupes++
			}
		}
	}
	if dupes != 2 {
		t.Errorf("the cap must stop session_dedup: %d copies of the duplicate survived (want 2)", dupes)
	}
	// The version is stamped even on a partial pipeline.
	if res.ProfileVersion == "" {
		t.Error("a memo-mode decision must carry its profile version")
	}
}

// A CPU cap of 0 = no limit: every stage runs.
func TestCPUCapZeroIsUnlimited(t *testing.T) {
	memo := NewSessionMemo(nil)
	p := memoProfile(Stage{Engine: "lite"}, Stage{Engine: "session_dedup"})
	body := bodyOf(
		textMsg("system", "same"),
		aCompressible(),
		textMsg("system", "same"),
		textMsg("user", "q"),
	)
	_, res := ApplyWithSession(body, p, nil, "sCpu0", memo)
	if strings.Contains(res.Err, "cpu cap") {
		t.Errorf("no cap configured, but the pipeline reports one: %q", res.Err)
	}
	// session_dedup ran: one of the two identical system messages is gone.
	out, _ := ApplyWithSession(body, p, nil, "sCpu0b", memo)
	msgs := decodeMsgs(t, out)
	dupes := 0
	for _, m := range msgs {
		if mm, ok := m.(map[string]any); ok {
			if s, _ := mm["content"].(string); s == "same" {
				dupes++
			}
		}
	}
	if dupes != 1 {
		t.Errorf("session_dedup should have collapsed the duplicate: %d copies", dupes)
	}
}

// The Result names the immutable ruleset that made the decision — in memo
// mode that is the BOUND version (an edit never restamps an active session).
func TestResultNamesDecisionVersion(t *testing.T) {
	memo := NewSessionMemo(nil)
	p := memoProfile(Stage{Engine: "lite"})
	v := ProfileFingerprint(p)
	body := bodyOf(aCompressible(), textMsg("user", "q"))
	_, res := ApplyWithSession(body, p, nil, "sVer", memo)
	if res.ProfileVersion != v {
		t.Errorf("ProfileVersion = %q, want the bound fingerprint %q", res.ProfileVersion, v)
	}
	// After an operator edit, the ACTIVE session still reports the old version.
	p.Stages = append(p.Stages, Stage{Engine: "session_dedup"})
	body2 := bodyOf(aCompressible(), textMsg("user", "q"), textMsg("user", "final"))
	_, res2 := ApplyWithSession(body2, p, nil, "sVer", memo)
	if res2.ProfileVersion != v {
		t.Errorf("an active session's decision must stay on the version it bound: %q != %q", res2.ProfileVersion, v)
	}
	// A legacy (no session) decision names the CURRENT fingerprint — the
	// ruleset that actually ran — with no binding involved.
	p2 := memoProfile(Stage{Engine: "lite"})
	_, res3 := Apply(body, p2)
	if res3.ProfileVersion != ProfileFingerprint(p2) {
		t.Errorf("legacy decisions must name the ruleset that ran: %q != %q",
			res3.ProfileVersion, ProfileFingerprint(p2))
	}
	// A nil profile invents nothing.
	_, res4 := Apply(body, nil)
	if res4.ProfileVersion != "" {
		t.Errorf("a nil profile has no version, got %q", res4.ProfileVersion)
	}
}

// The stamp clock used by the CPU cap is injectable in tests via MaxPipelineMS.
var _ = time.Now
