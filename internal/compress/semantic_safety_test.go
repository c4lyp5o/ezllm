package compress

import (
	"bytes"
	"strings"
	"testing"
)

// The two §11 correctness fixes (docs: feasibility audit 2026-10-16):
// (a) a nil profile must return unchanged content safely, never panic;
// (b) the deferred MS stamp must actually reach the caller — with unnamed
// returns the write landed on a local copy and every ledger row read 0.

func TestNilProfileSafelyReturnsUnchanged(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	out, res := ApplyWithDoc(body, nil, nil)
	if !bytes.Equal(out, body) {
		t.Error("nil profile must return the body byte-identical")
	}
	if res.Profile != "" {
		t.Errorf("nil profile has no name to record, got %q", res.Profile)
	}
	if res.Applied || res.Saved != 0 {
		t.Errorf("nil profile must record nothing applied: %+v", res)
	}
}

func TestDisabledProfileRecordsNameAndNoRewrite(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	p := &Profile{Name: "off-profile", Enabled: false, Stages: []Stage{{Engine: "lite"}}}
	out, res := ApplyWithDoc(body, p, nil)
	if !bytes.Equal(out, body) {
		t.Error("disabled profile must return the body byte-identical")
	}
	if res.Profile != "off-profile" {
		t.Errorf("a RESOLVED profile must stamp its name even when disabled, got %q", res.Profile)
	}
	if res.Applied {
		t.Error("disabled profile must not mark applied")
	}
}

// TestMSReachesCaller pins the named-return fix: the deferred stamp must
// reach the caller. The workload is sized so the marshals alone exceed a
// millisecond on any real machine (no sleeps — the work IS the clock).
func TestMSReachesCaller(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 4000; i++ {
		sb.WriteString(`{"role":"tool","content":"` + strings.Repeat("x", 1024) + `"},`)
	}
	body := []byte(`{"model":"m","messages":[` + strings.TrimSuffix(sb.String(), ",") + `]}`)
	p := &Profile{Name: "slow", Enabled: true, Stages: []Stage{{Engine: "lite"}}}
	_, res := ApplyWithDoc(body, p, nil)
	if res.MS <= 0 {
		t.Fatalf("compression_ms = %d on a multi-millisecond workload; the deferred stamp is not reaching the caller (unnamed-return regression)", res.MS)
	}
}

// ── semantic safety of the caveman rule packs ────────────────────────────
// The audit found two meaning-altering rules: please_could_you_tell_me
// consumed the question word ("tell me what time" -> "Tell me time"), and
// drop_there_seems_to_be hardened hedges into assertions. Both pass every
// structural gate and record as successful savings — so they need tests,
// not trust.

func TestCavemanQuestionWordSurvives(t *testing.T) {
	rules := rulesByName(t, "context")
	r, ok := rules["please_could_you_tell_me"]
	if !ok {
		return // rule removed entirely — fine
	}
	for _, c := range []string{
		"Please could you tell me what time it is?",
		"please could you tell me why the build fails?",
		"Could you tell me how to build this?",
	} {
		got := r.Re.ReplaceAllString(c, r.Replacement)
		low := strings.ToLower(got)
		for _, w := range []string{"what", "why", "how", "whether", "if"} {
			if strings.Contains(strings.ToLower(c), w) && !strings.Contains(low, w) {
				t.Errorf("rule %q consumes the question word %q:\n  in:  %s\n  out: %s", r.Name, w, c, got)
				break
			}
		}
		if got == c {
			t.Errorf("rule %q must still condense the politeness prefix (no-op): %s", r.Name, c)
		}
	}
}

func TestCavemanNoHedgeHardening(t *testing.T) {
	rules := rulesByName(t, "context")
	if r, ok := rules["drop_there_seems_to_be"]; ok {
		got := r.Re.ReplaceAllString("There seems to be a bug in the parser.", r.Replacement)
		if !strings.Contains(strings.ToLower(got), "seems") {
			t.Errorf("rule %q hardens uncertainty into assertion:\n  out: %s", r.Name, got)
		}
	}
	// Any other rule that maps a hedged existential to a plain one fails too.
	for name, r := range rules {
		if name == "drop_there_seems_to_be" {
			continue
		}
		got := r.Re.ReplaceAllString("There seems to be a bug in the parser.", r.Replacement)
		if strings.Contains(got, "There is a bug") {
			t.Errorf("rule %q hardens a hedge into an assertion: %s", name, got)
		}
	}
}

func rulesByName(t *testing.T, category string) map[string]cavemanRule {
	t.Helper()
	packs, diags := loadCavemanPacks()
	for _, d := range diags {
		t.Logf("pack diag: %s", d)
	}
	out := map[string]cavemanRule{}
	for _, p := range packs {
		for _, r := range p.Rules {
			if category != "" && r.Category != category && p.Category != category {
				continue
			}
			out[r.Name] = r
		}
	}
	if len(out) == 0 {
		t.Fatalf("no %q rules loaded — the test would pass vacuously", category)
	}
	return out
}
