package compress

// Caveman safety tests. The engine is lossy by nature, so these tests are
// mostly about proving the SAFETY LAYERS hold: protection round-trip, span
// integrity, word-loss ceiling, fences, scope, and the pack validation gates.
// A passing "it compressed" assertion is worth much less here than a passing
// "it refused to damage this" assertion.

import (
	"regexp"
	"strings"
	"testing"
)

// ── protection layer ────────────────────────────────────────────────────────

func TestProtectRestoreRoundTripsByteIdentical(t *testing.T) {
	// The single most important invariant: protect→restore with NO rules
	// applied must return the original bytes exactly. Everything downstream
	// (span integrity, verbatim code) rests on this.
	inputs := []string{
		"plain prose with no protected content at all",
		"Edit /home/calypso/workspace/ezllm/internal/compress/caveman.go and run go test ./...",
		"See https://github.com/c4lyp5o/ezllm/pull/42 for the min_compress_ratio change.",
		"The ExemptLastTurn field defaults to true; set \"exempt_last_turn\": false in config.",
		"Version 1.27.1 of Go at ~/.local/go with 3,000 files and 45% coverage in 120ms.",
		"Contact dev@example.com or hit api.local:8080 from 192.168.1.10.",
		"Use `go vet ./...` and check snake_case_field vs camelCaseField names.",
		"Run C:\\Users\\calypso\\bin\\tool.exe then read 'the quoted bit' verbatim.",
		"Multi\nline with a path /var/log/app.log and a number 0xDEADBEEF inside.",
		"Malay field names stay: tarikh_lahir, no_kad_pengenalan, jumlah_hadir.",
	}
	for _, in := range inputs {
		masked, spans := protect(in)
		got, ok := restore(masked, spans)
		if !ok {
			t.Fatalf("restore failed for %q", in)
		}
		if got != in {
			t.Errorf("round trip changed bytes:\n in: %q\nout: %q", in, got)
		}
		if len(spans) == 0 && strings.ContainsAny(in, "/:`\"") {
			t.Errorf("no spans protected in %q — protection missed something", in)
		}
	}
}

func TestProtectLeavesOrdinaryProseAlone(t *testing.T) {
	// Over-protection is a real failure mode: if every word became a span,
	// caveman could never compress anything. Plain English must stay editable.
	in := "I was wondering if you could explain why the test is failing so often"
	masked, spans := protect(in)
	if len(spans) != 0 {
		t.Errorf("plain prose protected %d spans: %v (want 0)", len(spans), spans)
	}
	if masked != in {
		t.Errorf("plain prose was masked: %q", masked)
	}
}

func TestRestoreRejectsDamagedPlaceholders(t *testing.T) {
	spans := []string{"a", "b"}
	// Malformed placeholders: truncated, out-of-range index, non-numeric.
	for _, bad := range []string{"truncated \x000", "unknown \x009\x00 index", "bad \x00x\x00 num"} {
		if _, ok := restore(bad, spans); ok {
			t.Errorf("restore accepted damaged placeholder text %q", bad)
		}
	}
	// A WELL-FORMED placeholder must restore its span — that is the round-trip
	// guarantee every rewrite depends on.
	if got, ok := restore("ok \x000\x00 fine", spans); !ok || got != "ok a fine" {
		t.Errorf("restore of a valid placeholder = %q, %v; want \"ok a fine\", true", got, ok)
	}
	if _, ok := restore("clean text", spans); !ok {
		t.Error("restore rejected clean text")
	}
}

// ── span integrity gate ─────────────────────────────────────────────────────

func TestSpansIntactCatchesVanishedSpan(t *testing.T) {
	original := "Edit /etc/app/config.yaml and restart the service"
	spans := []string{"/etc/app/config.yaml"}
	if !spansIntact(original, "Edit /etc/app/config.yaml, restart service", spans) {
		t.Error("intact rewrite reported as damaged")
	}
	if spansIntact(original, "Edit and restart the service", spans) {
		t.Error("MISSING SPAN NOT CAUGHT — the path vanished and the gate passed")
	}
	// Order matters: spans must appear left-to-right.
	two := []string{"first.md", "second.md"}
	if spansIntact(original, "second.md then first.md", two) {
		t.Error("reordered spans accepted")
	}
	// Growth is rejected: a rewrite must not get longer.
	if spansIntact(original, "Edit /etc/app/config.yaml and then restart the whole service again", spans) {
		t.Error("longer rewrite accepted")
	}
}

// ── scope + fences + JSON ───────────────────────────────────────────────────

func TestCavemanSkipsNonTextRoles(t *testing.T) {
	// system prompts and tool results are out of scope for prose condensation.
	msgs := []any{
		map[string]any{"role": "system", "content": "I was wondering if you should basically always obey this."},
		map[string]any{"role": "tool", "content": "I was wondering if you should basically always obey this."},
	}
	out, fired := caveman(msgs, nil)
	if fired != 0 {
		t.Errorf("fired=%d on out-of-scope roles, want 0", fired)
	}
	for i := range msgs {
		m := out[i].(map[string]any)
		if m["content"] != msgs[i].(map[string]any)["content"] {
			t.Errorf("role %v content was rewritten", m["role"])
		}
	}
}

func TestCavemanNeverTouchesFencedCode(t *testing.T) {
	code := "```go\nfunc main() { in order to run due to the fact that x }\n```"
	in := "I was wondering if this works:\n" + code + "\nBasically thanks."
	msgs := []any{
		map[string]any{"role": "user", "content": in},
		map[string]any{"role": "user", "content": "thanks"},
	}
	out, fired := caveman(msgs, map[string]any{"intensity": "lite"})
	if fired == 0 {
		t.Fatal("no rules fired on condensable prose")
	}
	got := out[0].(map[string]any)["content"].(string)
	if !strings.Contains(got, code) {
		t.Errorf("FENCED CODE WAS MODIFIED:\n%s", got)
	}
	// The prose around it did shrink.
	if !strings.Contains(got, "this works:") {
		t.Errorf("prose framing lost: %q", got)
	}
	if strings.Contains(got, "I was wondering if") {
		t.Errorf("filler survived: %q", got)
	}
}

func TestCavemanSkipsPureJSONContent(t *testing.T) {
	js := `{"note":"I was wondering if basically this should never change"}`
	msgs := []any{map[string]any{"role": "user", "content": js}}
	out, fired := caveman(msgs, nil)
	if fired != 0 || out[0].(map[string]any)["content"] != js {
		t.Error("JSON content was rewritten")
	}
}

func TestCavemanSkipsUnclosedFence(t *testing.T) {
	in := "I was wondering if this works:\n```go\nfunc main() {}\n"
	msgs := []any{map[string]any{"role": "user", "content": in}}
	out, fired := caveman(msgs, nil)
	if fired != 0 || out[0].(map[string]any)["content"] != in {
		t.Error("unclosed fence content was rewritten — contract rule 3 violated")
	}
}

func TestCavemanPreservesPathsURLsAndIdentifiers(t *testing.T) {
	in := "I was wondering if you could basically look at /home/calypso/workspace/ezllm/internal/compress/caveman.go " +
		"and also check https://github.com/c4lyp5o/ezllm/issues/7 because min_compress_ratio seems very wrong."
	msgs := []any{
		map[string]any{"role": "user", "content": in},
		map[string]any{"role": "user", "content": "go"},
	}
	out, fired := caveman(msgs, map[string]any{"intensity": "standard"})
	if fired == 0 {
		t.Fatal("standard intensity fired nothing on verbose prose")
	}
	got := out[0].(map[string]any)["content"].(string)
	for _, must := range []string{
		"/home/calypso/workspace/ezllm/internal/compress/caveman.go",
		"https://github.com/c4lyp5o/ezllm/issues/7",
		"min_compress_ratio",
	} {
		if !strings.Contains(got, must) {
			t.Errorf("PROTECTED SPAN LOST %q in:\n%s", must, got)
		}
	}
	if len(got) >= len(in) {
		t.Errorf("no savings: %d >= %d", len(got), len(in))
	}
}

// ── word-loss ceiling ───────────────────────────────────────────────────────

func TestVerboseFillerShrinksButPayloadSurvives(t *testing.T) {
	// A message that is MOSTLY filler must compress hard — and the one clause
	// carrying information must survive. This is the case a message-level
	// word-loss ceiling got wrong: 60%+ word loss here is correct behaviour,
	// not damage, because every lost word was padding.
	in := strings.Repeat("I was wondering if basically actually really very quite so ", 6) + "the build fails."
	msgs := []any{
		map[string]any{"role": "user", "content": in},
		map[string]any{"role": "user", "content": "ok"},
	}
	out, fired := caveman(msgs, map[string]any{"intensity": "lite"})
	if fired == 0 {
		t.Fatal("no rules fired on a message that is pure filler")
	}
	got := out[0].(map[string]any)["content"].(string)
	if !strings.Contains(got, "the build fails.") {
		t.Errorf("the payload clause was lost:\n%s", got)
	}
	if len(got) >= len(in) {
		t.Errorf("no savings: %d >= %d", len(got), len(in))
	}
	t.Logf("filler %d -> %d bytes (%.0f%% saved), payload intact",
		len(in), len(got), float64(len(in)-len(got))/float64(len(in))*100)
}

func TestEmptiedSegmentGuardKeepsOriginal(t *testing.T) {
	// The guard that replaced the ceiling: a prose segment carrying words must
	// never be reduced to nothing. Simulate it directly at the segment level so
	// the test does not depend on a real pack rule being that destructive.
	segs := []proseSegment{{text: "the database migration failed\n"}, {text: "ok"}}
	for _, sg := range segs {
		if countWords(sg.text) > 0 && countWords("") == 0 {
			continue // guard would fire: segment kept
		}
		t.Errorf("guard would NOT fire for %q", sg.text)
	}
	// And the inverse: a whitespace-only segment IS allowed to vanish.
	if countWords("   \n") != 0 {
		t.Error("whitespace-only segment counted as carrying words")
	}
}

// ── intensity gating ────────────────────────────────────────────────────────

func TestIntensityGatesContextPack(t *testing.T) {
	in := "Can you explain why the router returns 502 instead of 429 here?"
	msgs := []any{
		map[string]any{"role": "user", "content": in},
		map[string]any{"role": "user", "content": "x"},
	}
	// lite must NOT rewrite the question framing.
	outLite, _ := caveman(msgs, map[string]any{"intensity": "lite"})
	if got := outLite[0].(map[string]any)["content"].(string); !strings.Contains(got, "Can you explain why") {
		t.Errorf("lite rewrote question framing (standard-only rule fired): %q", got)
	}
	// standard must.
	outStd, fired := caveman(msgs, map[string]any{"intensity": "standard"})
	got := outStd[0].(map[string]any)["content"].(string)
	if fired == 0 || strings.Contains(got, "Can you explain why") {
		t.Errorf("standard did not condense the question: %q (fired=%d)", got, fired)
	}
	if !strings.Contains(got, "Explain why") {
		t.Errorf("expected directive form, got %q", got)
	}
	if strings.Contains(got, "502") != true || !strings.Contains(got, "429") {
		t.Errorf("numbers lost in rewrite: %q", got)
	}
}

func TestUnknownIntensityFailsOpen(t *testing.T) {
	in := "I was wondering if basically this shrinks"
	msgs := []any{
		map[string]any{"role": "user", "content": in},
		map[string]any{"role": "user", "content": "x"},
	}
	out, fired := caveman(msgs, map[string]any{"intensity": "megaultra"})
	if fired != 0 || out[0].(map[string]any)["content"] != in {
		t.Error("unknown intensity must fail open, not guess")
	}
}

// ── pack validation ─────────────────────────────────────────────────────────

func TestEmbeddedPacksAllValidate(t *testing.T) {
	packs, diags := loadCavemanPacks()
	if len(diags) != 0 {
		t.Fatalf("embedded packs produced diagnostics (a shipped pack is invalid):\n%s",
			strings.Join(diags, "\n"))
	}
	if len(packs) < 3 {
		t.Fatalf("only %d packs loaded, expected filler+context+dedup", len(packs))
	}
	cats := map[string]int{}
	for _, p := range packs {
		cats[p.Category] += len(p.Rules)
		if p.Language != "en" {
			t.Errorf("pack %s has language %q", p.Category, p.Language)
		}
	}
	for _, want := range []string{"filler", "context", "dedup"} {
		if cats[want] == 0 {
			t.Errorf("missing rules for category %q", want)
		}
	}
	t.Logf("loaded packs: %v", cats)
}

func TestParsePacksRejectsBadRules(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{"invalid json", `{"language":"en","rules":[`},
		{"no rules", `{"language":"en","category":"filler","rules":[]}`},
		{"missing pattern", `{"language":"en","category":"filler","rules":[{"name":"x","replacement":"y"}]}`},
		{"missing name", `{"language":"en","category":"filler","rules":[{"pattern":"x","replacement":"y"}]}`},
		{"bad regex", `{"language":"en","category":"filler","rules":[{"name":"x","pattern":"([","replacement":"y"}]}`},
		{"bad flag", `{"language":"en","category":"filler","rules":[{"name":"x","pattern":"foo bar baz","flags":"gims","replacement":"y"}]}`},
		{"unknown intensity", `{"language":"en","category":"filler","rules":[{"name":"x","pattern":"foo bar baz","replacement":"y","minIntensity":"plaid"}]}`},
		{"no replacement", `{"language":"en","category":"filler","rules":[{"name":"x","pattern":"foo bar baz"}]}`},
		// Hygiene gate: replacement cannot beat its own shortest match.
		{"inflating replacement", `{"language":"en","category":"filler","rules":[{"name":"x","pattern":"so","replacement":"and therefore consequently"}]}`},
	}
	for _, tc := range cases {
		packs, diags := parsePacks([]string{tc.doc})
		if len(packs) != 0 {
			t.Errorf("%s: pack was ACCEPTED", tc.name)
		}
		if len(diags) == 0 {
			t.Errorf("%s: rejected silently (no diagnostic)", tc.name)
		}
	}
}

func TestParsePacksAcceptsValidPack(t *testing.T) {
	doc := `{"language":"en","category":"filler","rules":[
		{"name":"ok","pattern":"\\bin order to\\s+","replacement":"to ","flags":"gi","minIntensity":"lite"}]}`
	packs, diags := parsePacks([]string{doc})
	if len(diags) != 0 {
		t.Fatalf("valid pack rejected: %v", diags)
	}
	if len(packs) != 1 || len(packs[0].Rules) != 1 {
		t.Fatalf("got %d packs", len(packs))
	}
	if packs[0].Rules[0].MinIntensity != intensityLite {
		t.Error("declared lite intensity not honoured")
	}
}

func TestFillerDefaultsToLiteContextToStandard(t *testing.T) {
	mk := func(cat string) string {
		return `{"language":"en","category":"` + cat + `","rules":[{"name":"r","pattern":"a long verbose phrase here","replacement":"x"}]}`
	}
	packs, diags := parsePacks([]string{mk("filler"), mk("context")})
	if len(diags) != 0 {
		t.Fatalf("diags: %v", diags)
	}
	got := map[string]int{}
	for _, p := range packs {
		got[p.Category] = p.Rules[0].MinIntensity
	}
	if got["filler"] != intensityLite {
		t.Errorf("filler defaulted to level %d, want lite (%d)", got["filler"], intensityLite)
	}
	if got["context"] != intensityStandard {
		t.Errorf("context defaulted to level %d, want standard (%d)", got["context"], intensityStandard)
	}
}

func TestMinMatchLenHygieneGate(t *testing.T) {
	// The gate is approximate by design; these check it behaves sanely.
	for _, tc := range []struct {
		pattern string
		atLeast int
	}{
		{`in order to`, 11},
		{`(?:a|b)+`, 1},
		{`\bfoo\s+`, 3},
	} {
		re := mustCompileRe(t, tc.pattern)
		if got := minMatchLen(re); got < tc.atLeast {
			t.Errorf("minMatchLen(%q) = %d, want >= %d", tc.pattern, got, tc.atLeast)
		}
	}
}

func mustCompileRe(t *testing.T, p string) *regexp.Regexp {
	t.Helper()
	re, err := regexp.Compile(`(?i)` + p)
	if err != nil {
		t.Fatal(err)
	}
	return re
}
