package compress

import (
	"math/rand"
	"strings"
	"testing"
)

func builtinRules(t *testing.T) []cavemanRule {
	t.Helper()
	packs, diags := parsePacks(embeddedPacks())
	if len(diags) > 0 {
		t.Fatalf("pack diagnostics: %v", diags)
	}
	var rules []cavemanRule
	for _, p := range packs {
		rules = append(rules, p.Rules...)
	}
	return rules
}

// The prefilter never skips a rule that matches: whenever a rule matches, its
// text holds one of its literals. Rules built on character classes (the
// whitespace ones) have no literal and always run.
func TestRuleNeedsNeverSkipsAMatch(t *testing.T) {
	rules := builtinRules(t)
	var corpus []string
	filtered := 0
	for _, r := range rules {
		if r.Need == nil {
			continue
		}
		filtered++
		for _, n := range r.Need {
			// Each literal in other cases and inside other text.
			corpus = append(corpus, n, strings.ToLower(n), "x "+strings.ToLower(n)+" y", strings.Title(strings.ToLower(n))+" more")
		}
	}
	if filtered < len(rules)-2 {
		t.Fatalf("%d of %d rules have a prefilter, want all but the two whitespace rules", filtered, len(rules))
	}
	// Unicode case-fold relatives (ſ folds to s, the Kelvin sign to k).
	for _, s := range corpus[:len(corpus)/2] {
		corpus = append(corpus, strings.ReplaceAll(strings.ToLower(s), "s", "ſ"), strings.ReplaceAll(strings.ToLower(s), "k", "K"))
	}
	words := []string{"basically", "actually", "I", "think", "that", "you", "can", "explain", "why", "just", "really", "please", "would", "like", "to", "mind", "so", "very", "ſo", "Kind", " ", "  ", "\n", ",", "."}
	rng := rand.New(rand.NewSource(2))
	for i := 0; i < 20000; i++ {
		var b strings.Builder
		for j := 0; j < 1+rng.Intn(12); j++ {
			w := words[rng.Intn(len(words))]
			if rng.Intn(3) == 0 {
				w = strings.ToUpper(w)
			}
			b.WriteString(w)
			b.WriteByte(' ')
		}
		corpus = append(corpus, b.String())
	}
	for _, s := range corpus {
		f := foldText(s)
		for _, r := range rules {
			if r.Need != nil && !containsAny(f, r.Need) && r.Re.MatchString(s) {
				t.Fatalf("rule %s matches %q but its prefilter %q skipped it", r.Name, s, r.Need)
			}
		}
	}
}

// applyPacks gives the same output with and without the prefilter.
func TestApplyPacksSameWithoutPrefilter(t *testing.T) {
	packs, _ := parsePacks(embeddedPacks())
	bare := make([]cavemanPack, len(packs))
	for i, p := range packs {
		bare[i] = p
		bare[i].Rules = append([]cavemanRule(nil), p.Rules...)
		for j := range bare[i].Rules {
			bare[i].Rules[j].Need = nil
		}
	}
	texts := []string{
		"Basically, I think that we should really just go ahead and update it. Could you show me how to fix this? Would you mind please checking?",
		"I would like you to explain. Can you explain why it fails?",
		"plain words with nothing to condense",
	}
	for _, s := range texts {
		for _, level := range []int{intensityLite, intensityStandard} {
			a, fa, _ := applyPacks(packs, s, level)
			b, fb, _ := applyPacks(bare, s, level)
			if a != b || fa != fb {
				t.Fatalf("level %d %q: with prefilter %q (%d), without %q (%d)", level, s, a, fa, b, fb)
			}
		}
	}
}

// ruleNeeds derives only literals every match contains, for the shapes a
// custom pack might use.
func TestRuleNeedsShapes(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		want    []string // nil = always run
	}{
		{`(?i)\bjust\s+`, []string{"JUST"}},
		{`(?i)(?:foo){0,3}bar`, []string{"BAR"}},
		{`(?i)(?:foo){2,3}`, []string{"FOOFOO"}},
		{`(?i)\b(?:basically|actually)\b`, []string{"BASICALLY", "ACTUALLY"}},
		{`(?i)(?:foo|\s+)`, nil},
		{`(?i)(?:please )?`, nil},
		{`\s{2,}`, nil},
	} {
		got := ruleNeeds(tc.pattern)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") || (got == nil) != (tc.want == nil) {
			t.Errorf("%s: needs %q, want %q", tc.pattern, got, tc.want)
		}
	}
}
