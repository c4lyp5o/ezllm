package compress

// ── caveman rule packs ──────────────────────────────────────────────────────
// Packs are DATA: curated regex substitutions that turn verbose request prose
// into directives. They are embedded in the binary (go:embed) so a deployment
// cannot be silently weakened by a missing file, and every pack is validated at
// load — a rule whose pattern fails to compile, or whose replacement would ADD
// length, disables its pack with a diagnostic instead of half-applying.
//
// Category semantics drive intensity gating:
//   filler    → cleanup class, fires at lite AND standard
//   context   → rewrites framing/lead-in prose, standard only
//   dedup     → collapses repeated framing within one message, standard only
//
// These packs are deliberately CONSERVATIVE. Every rule must be one a reviewer
// can read and agree cannot change meaning: hedging, polite padding, verbose
// request framing. Anything that could drop a fact, a number or a name does not
// belong here — that is what the protection spans and the word-loss ceiling
// exist to catch if someone ever adds it by mistake.

import (
	"embed"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// embeddedPackFS holds the built-in English packs. Embedding them means a
// deployment cannot be silently weakened by a missing file on disk, and the
// shipped ruleset is part of the binary you can audit.
//
//go:embed caveman_packs/en_*.json
var embeddedPackFS embed.FS

// embeddedPacks reads the pack files in a STABLE (sorted) order so rule
// application — and therefore output — does not depend on filesystem order.
func embeddedPacks() []string {
	ents, err := embeddedPackFS.ReadDir("caveman_packs")
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "en_") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, "caveman_packs/"+e.Name())
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, n := range names {
		if b, err := embeddedPackFS.ReadFile(n); err == nil {
			out = append(out, string(b))
		}
	}
	return out
}

// cavemanRule is one validated substitution.
type cavemanRule struct {
	Name         string
	Re           *regexp.Regexp
	Replacement  string            // used when ReplacementMap misses
	ReplaceMap   map[string]string // keyed by lowercased matched text
	MinIntensity int               // intensityLite or intensityStandard
	Category     string
}

// cavemanPack is a validated set of rules from one category file.
type cavemanPack struct {
	Language string
	Category string
	Rules    []cavemanRule
}

const (
	intensityLite     = 1
	intensityStandard = 2
)

// packRules is the on-disk/embedded pack shape (mirrors the reference format so
// packs stay portable between systems).
type packRules struct {
	Language string `json:"language"`
	Category string `json:"category"`
	Rules    []rawRule
}

// rawRule is one rule as written in a pack file.
type rawRule struct {
	Name           string            `json:"name"`
	Pattern        string            `json:"pattern"`
	Flags          string            `json:"flags"`
	Replacement    *string           `json:"replacement"`
	ReplacementMap map[string]string `json:"replacementMap"`
	Context        string            `json:"context"`
	Category       string            `json:"category"`
	MinIntensity   string            `json:"minIntensity"`
	Description    string            `json:"description"`
}

// firstNonEmpty returns the first non-blank string, or "" if all are blank.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// minIntensityFor maps a rule's declared floor to our level ints. Empty means
// the pack's own category default (filler -> lite, everything else -> standard),
// so a pack author cannot accidentally make an aggressive rule always-on.
func minIntensityFor(declared, category string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(declared)) {
	case "":
		if strings.EqualFold(category, "filler") {
			return intensityLite, true
		}
		return intensityStandard, true
	case "lite":
		return intensityLite, true
	case "standard", "aggressive", "ultra":
		// aggressive/ultra are pipeline compositions upstream, not pack
		// levels; a rule declaring them simply requires standard here.
		return intensityStandard, true
	}
	return 0, false
}

// parsePacks validates raw pack JSON. Returns the packs that passed and the
// diagnostics for those that did not — a rejected pack never silently drops a
// rule, and never blocks the other packs.
func parsePacks(raw []string) ([]cavemanPack, []string) {
	var packs []cavemanPack
	var diags []string
	for i, doc := range raw {
		var pr packRules
		if err := json.Unmarshal([]byte(doc), &pr); err != nil {
			diags = append(diags, fmt.Sprintf("pack[%d]: invalid JSON: %v", i, err))
			continue
		}
		if len(pr.Rules) == 0 {
			diags = append(diags, fmt.Sprintf("pack[%d] %s/%s: no rules", i, pr.Language, pr.Category))
			continue
		}
		pack := cavemanPack{Language: pr.Language, Category: pr.Category}
		// Per-rule validation: a bad rule is SKIPPED with a diagnostic, it does
		// not take the whole pack down. One typo in one rule must never silently
		// disable prose condensation for every other rule in the file.
		for _, r := range pr.Rules {
			rule, diag, ok := validateRule(pr.Language, pr.Category, r)
			if !ok {
				diags = append(diags, diag)
				continue
			}
			pack.Rules = append(pack.Rules, rule)
		}
		if len(pack.Rules) > 0 {
			packs = append(packs, pack)
		}
	}
	return packs, diags
}

// minMatchLen is the shortest string the pattern can match, used only for the
// hygiene gate. Go's regexp has no literal-length analysis, so we approximate
// from the source: strip quantifier/alternation metacharacters and count the
// literal remainder, flooring at 1. An approximation is fine here — the gate
// exists to reject obviously-inflating rules, not to prove a bound.
func minMatchLen(re *regexp.Regexp) int {
	src := re.String()
	src = strings.TrimPrefix(src, `(?i)`)
	var lit strings.Builder
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch c {
		case '\\':
			i++ // escaped char counts as one literal
			lit.WriteByte('x')
		case '(', ')', '[', ']', '{', '}', '*', '+', '?', '|', '^', '$', '.':
			// structural: contributes nothing guaranteed
		default:
			lit.WriteByte(c)
		}
	}
	n := lit.Len()
	if n < 1 {
		return 1
	}
	return n
}

// applyPacks runs every rule at or below the given intensity over PROTECTED
// text (placeholders in place). Returns the rewritten text, the number of rules
// that actually fired, and any diagnostic. Rules apply left-to-right in pack
// order; each rule sees the previous rule's output, which is what makes
// "verbose question → directive → drop trailing politeness" compose.
func applyPacks(packs []cavemanPack, text string, intensity int) (string, int, string) {
	out := text
	fired := 0
	for _, p := range packs {
		for _, r := range p.Rules {
			if r.MinIntensity > intensity {
				continue
			}
			replaced, n := replaceRule(r, out)
			if n > 0 {
				fired++
				out = replaced
			}
		}
	}
	if out == text {
		return text, 0, ""
	}
	return out, fired, ""
}

// replaceRule applies one rule. replacementMap wins on an exact (lowercased)
// match of the matched text, so "Can you explain why" and "can you explain why"
// can map to the same directive while an unmatched variant falls back to the
// generic replacement. Capture-group references ($1) are NOT supported on
// purpose: a rule that re-emits captured text could smuggle protected content
// around in a different form, and we would rather have simple, auditable rules.
func replaceRule(r cavemanRule, s string) (string, int) {
	n := 0
	out := r.Re.ReplaceAllStringFunc(s, func(m string) string {
		// replacementMap wins on an exact (lowercased) match so variants can map
		// to the same directive; otherwise the generic replacement applies.
		// An EMPTY replacement is a deletion rule ("I was wondering if " -> "")
		// and must actually delete — returning the original match here would
		// silently disable every pure-filler rule in the pack.
		if len(r.ReplaceMap) > 0 {
			if v, ok := r.ReplaceMap[strings.ToLower(m)]; ok {
				n++
				return v
			}
		}
		n++
		return r.Replacement
	})
	return out, n
}

// validateRule turns one raw pack rule into a compiled cavemanRule, or returns
// the reason it was rejected. Every rejection path is a diagnostic the caller
// surfaces — a rule is never dropped silently.
//
// Gates, in order of cheapness:
//   - name + pattern required
//   - flags limited to g/i (multiline/dotall could cross sentence boundaries
//     and eat meaning)
//   - pattern must compile
//   - minIntensity must be a known level
//   - hygiene: the replacement (and every replacementMap value) must be shorter
//     than the pattern's shortest possible match, so a rule can never inflate
//     the prompt. An EMPTY replacement is legal: it means "delete this filler".
func validateRule(lang, category string, r rawRule) (cavemanRule, string, bool) {
	reject := func(format string, args ...any) (cavemanRule, string, bool) {
		msg := fmt.Sprintf("pack %s/%s rule %q: "+format,
			append([]any{lang, category, r.Name}, args...)...)
		return cavemanRule{}, msg, false
	}
	if r.Name == "" || r.Pattern == "" {
		return reject("name and pattern required")
	}
	for _, f := range r.Flags {
		if f != 'g' && f != 'i' {
			return reject("unsupported flag %q (only g,i)", string(f))
		}
	}
	re, err := regexp.Compile(`(?i)` + r.Pattern)
	if err != nil {
		return reject("pattern does not compile: %v", err)
	}
	// Intensity floor: the rule may declare one; otherwise the PACK category
	// decides (filler -> lite, everything else -> standard). Passing the pack
	// category as the fallback is what stops an unannotated filler rule from
	// silently requiring standard.
	level, ok := minIntensityFor(r.MinIntensity, firstNonEmpty(r.Category, category))
	if !ok {
		return reject("unknown minIntensity %q", r.MinIntensity)
	}
	// Intent gate: a rule must DECLARE what it does. `replacement` absent (nil)
	// with no replacementMap is a pack bug — an undeclared deletion is exactly
	// the shape of an accidental content eraser, so refuse it. An EXPLICIT empty
	// string ("") is a legal deletion rule and is how every filler rule works.
	if r.Replacement == nil && len(r.ReplacementMap) == 0 {
		return reject("needs an explicit replacement (empty string deletes) or a replacementMap")
	}
	repl := ""
	if r.Replacement != nil {
		repl = *r.Replacement
	}
	shortest := minMatchLen(re)
	if len(r.ReplacementMap) == 0 && len(repl) >= shortest {
		return reject("replacement %d bytes cannot beat its shortest match (%d)",
			len(repl), shortest)
	}
	for k, v := range r.ReplacementMap {
		if len(v) >= shortest {
			return reject("replacementMap[%q] %d bytes cannot beat its shortest match (%d)",
				k, len(v), shortest)
		}
	}
	cat := r.Category
	if cat == "" {
		cat = category
	}
	norm := make(map[string]string, len(r.ReplacementMap))
	for k, v := range r.ReplacementMap {
		norm[strings.ToLower(k)] = v
	}
	return cavemanRule{
		Name: r.Name, Re: re, Replacement: repl, ReplaceMap: norm,
		MinIntensity: level, Category: cat,
	}, "", true
}
