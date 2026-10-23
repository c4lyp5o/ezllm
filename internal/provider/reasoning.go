package provider

import (
	"regexp"
	"strings"
)

// ReasoningRule is what a model accepts for `reasoning_effort`: the values its
// own refusal enumerates, and whether reasoning can be turned off at all.
//
// The vocabulary is not a gateway invention. Every provider that rejected the
// effort in Calypso's ledger listed its own options in the 400:
//
//	opencode-go   Supported values: [minimal, low, medium, high, xhigh, max]
//	command-code  Invalid option: expected one of "off"|"low"|"medium"|"high"
//	minimax       Reasoning is mandatory for this endpoint and cannot be disabled
//
// so the parse below is a READ of what the provider already said, and an
// unknown rule (Known=false, Required=false) always means "leave the request
// exactly as the client sent it".
type ReasoningRule struct {
	// Allowed is the provider's own enumeration, in ITS order. Empty means the
	// provider named no list (it may still be Required).
	Allowed []string
	// Required means the model cannot run without reasoning: "off"/"none" must
	// never be sent, and if no vocabulary is known the field is dropped so the
	// provider's default applies.
	Required bool
	// Known is true when the provider told us something actionable.
	Known bool
}

// Empty reports whether the rule carries no information (the default).
func (r ReasoningRule) Empty() bool { return !r.Known }

// effortRank orders the values the providers use, cheapest first. A value
// outside this table is "unranked": the clamp treats it as the LOWEST effort,
// because upgrading a client's request costs money and lowering it does not.
var effortRank = map[string]int{
	"off": 0, "none": 0, "disabled": 0,
	"minimal": 1, "minimum": 1,
	"low":    2,
	"medium": 3, "mid": 3, "adaptive": 3,
	"high":  4,
	"xhigh": 5, "xh": 5,
	"max": 6, "maximum": 6,
}

func rankOf(v string) (int, bool) {
	r, ok := effortRank[strings.ToLower(strings.TrimSpace(v))]
	return r, ok
}

// Clamp maps a requested effort onto what the model accepts.
//
// value is what to send; drop means omit the field entirely (the provider
// requires reasoning and named no vocabulary, so its default is the only safe
// answer); changed is false when the request may go out untouched — which is
// every case where the rule is unknown or the value is already allowed, so the
// common path never rewrites a body.
//
// It only ever moves DOWN, or up to the cheapest allowed value when nothing
// below is allowed. A clamp must never buy more reasoning than the client asked
// for.
func (r ReasoningRule) Clamp(want string) (value string, drop bool, changed bool) {
	want = strings.TrimSpace(want)
	if want == "" || !r.Known {
		return want, false, false
	}
	for _, a := range r.Allowed {
		if strings.EqualFold(a, want) {
			return want, false, false // already acceptable: byte-identical request
		}
	}
	if len(r.Allowed) == 0 {
		// Required with no enumeration: never send a "disabled" effort, and do
		// not invent a value the provider did not name.
		if r.Required {
			if rank, ok := rankOf(want); ok && rank == 0 {
				return "", true, true
			}
		}
		return want, false, false
	}

	// Choose the highest allowed value at or below what was asked; if none is
	// that low, the cheapest allowed one.
	wantRank, ranked := rankOf(want)
	if !ranked {
		wantRank = 0
	}
	best, bestRank := "", -1
	lowest, lowestRank := "", 1<<30
	for _, a := range r.Allowed {
		ar, ok := rankOf(a)
		if !ok {
			continue
		}
		if ar < lowestRank {
			lowest, lowestRank = a, ar
		}
		if ar <= wantRank && ar > bestRank {
			best, bestRank = a, ar
		}
	}
	if best != "" {
		return best, false, true
	}
	if lowest != "" {
		return lowest, false, true
	}
	return want, false, false // the enumeration held only words we do not rank
}

var (
	// Supported values: [minimal, low, medium, high, xhigh, max]
	reSupportedValues = regexp.MustCompile(`(?i)supported values\s*:\s*\[([^\]]*)\]`)
	// Invalid option: expected one of "off"|"low"|"medium"|"high"
	// The capture stops at the first token that is not part of the "a"|"b"
	// run: the refusal continues with its own JSON (,"type":…,"param":…) and
	// those words are not vocabulary members.
	reExpectedOneOf = regexp.MustCompile(`(?i)expected one of\s*((?:\\?"[a-zA-Z][a-zA-Z_-]{1,12}\\?"\s*(?:\|\s*)?)+)`)
	// any quoted word list, used on the expected-one-of tail. The quotes may
	// arrive escaped (\"off\") when the refusal is read back out of a JSON
	// body or a ledger row, so the backslash is optional on both sides.
	reQuoted = regexp.MustCompile(`\\?"([a-zA-Z][a-zA-Z_-]{1,12})\\?"`)
)

// maxVocabulary bounds a parsed list. A provider naming more than this is not
// enumerating an effort vocabulary, and the clamp has no business carrying it.
const maxVocabulary = 12

// ParseReasoningRule reads a refusal for what it says about the model's effort
// vocabulary. It returns the zero rule when the body says nothing about
// reasoning, so callers can apply it unconditionally.
func ParseReasoningRule(status int, body []byte) ReasoningRule {
	if status < 400 || status >= 500 {
		return ReasoningRule{}
	}
	text := string(body)
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "reasoning") && !strings.Contains(lower, "thinking") {
		return ReasoningRule{}
	}

	var r ReasoningRule

	// An explicit enumeration wins.
	if m := reSupportedValues.FindStringSubmatch(text); m != nil {
		r.Allowed = splitVocab(m[1])
	} else if m := reExpectedOneOf.FindStringSubmatch(text); m != nil {
		for _, q := range reQuoted.FindAllStringSubmatch(m[1], -1) {
			r.Allowed = append(r.Allowed, strings.ToLower(q[1]))
		}
		if len(r.Allowed) > maxVocabulary {
			r.Allowed = nil
		}
	}

	// "Reasoning is mandatory for this endpoint and cannot be disabled",
	// "requires adaptive thinking", "reasoning effort none is not allowed",
	// "reasoning_effort are not supported" (the field itself, not a value).
	switch {
	case strings.Contains(lower, "cannot be disabled"),
		strings.Contains(lower, "is mandatory"),
		strings.Contains(lower, "requires adaptive thinking"),
		strings.Contains(lower, "requires reasoning"),
		strings.Contains(lower, "reasoning is required"):
		r.Required = true
	case strings.Contains(lower, "is not allowed") && containsEffortWord(lower):
		r.Required = true
	}

	r.Known = len(r.Allowed) > 0 || r.Required
	if !r.Known {
		return ReasoningRule{}
	}
	// A list that is not an effort vocabulary is not ours to keep: "off" is a
	// plausible value in many enums (tool_choice, truncation, …), so require at
	// least two members the gateway can actually rank as effort levels.
	if len(r.Allowed) > 0 && rankedCount(r.Allowed) < 2 {
		r.Allowed = nil
		r.Known = r.Required
		if !r.Known {
			return ReasoningRule{}
		}
	}
	return r
}

// rankedCount counts the members the effort rank table understands.
func rankedCount(vs []string) int {
	n := 0
	for _, v := range vs {
		if _, ok := rankOf(v); ok {
			n++
		}
	}
	return n
}

func containsEffortWord(lower string) bool {
	for _, w := range []string{"reasoning", "thinking", "effort"} {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

// splitVocab parses "minimal, low, medium" (and tolerates quotes/spaces).
func splitVocab(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, p := range parts {
		v := strings.ToLower(strings.Trim(strings.TrimSpace(strings.Trim(p, `"'`)), `"'`))
		v = strings.TrimSpace(v)
		if v == "" || len(v) > 16 || seen[v] {
			continue
		}
		// Bearings: only word-like values are vocabulary members.
		ok := true
		for _, ch := range v {
			if !(ch >= 'a' && ch <= 'z') && ch != '_' && ch != '-' {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) > maxVocabulary {
		out = out[:maxVocabulary]
	}
	return out
}
