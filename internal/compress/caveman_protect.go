package compress

// ── caveman protection layer ────────────────────────────────────────────────
// The safety core of the only lossy engine. Caveman condenses PROSE; this file
// decides what is not prose. Every protected span is lifted out before any rule
// runs and put back verbatim afterwards, so a rule can never rewrite a path, a
// URL, an identifier or a quoted string — it can only ever touch the words
// between them.
//
// The design contract (docs/compression-design.md §caveman):
//   - fenced regions are atomic and untouched (splitUnits groups them)
//   - inline spans are replaced by placeholders that no rule pattern matches
//   - after rewriting, every span must still be present, in order (the
//     span-integrity gate in caveman.go) — otherwise the original is kept
//
// Placeholders are deliberately ugly and non-word-bearing (\x00N\x00) so that
// (a) no rule pack pattern can accidentally match them and (b) their presence
// in the output is trivially verifiable. NUL cannot appear in valid JSON
// strings, so a stray one is a bug we would catch in tests, never silently ship.

import (
	"fmt"
	"regexp"
	"strings"
)

// protectSpans are the inline classes that must survive caveman verbatim.
// Order matters: the first match at a position wins, so the more specific
// patterns (URLs, emails, fenced/inline code) come before the general ones
// (identifiers, numbers). Each entry is anchored enough to avoid eating
// ordinary prose — an over-broad pattern here is worse than no protection.
var protectSpans = []*regexp.Regexp{
	// URLs and bare hosts with a port.
	regexp.MustCompile(`https?://[^\s<>"']+`),
	regexp.MustCompile(`\b[\w.-]+\.(?:com|org|net|io|dev|me|ai|sh|xyz|co)\b(?::\d+)?(?:/[^\s<>"']*)?`),
	// Emails.
	regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`),
	// IPv4 / IPv6-ish with optional port.
	regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?::\d+)?\b`),
	// Inline code spans (`...`) — never cross a newline.
	regexp.MustCompile("`[^`\n]+`"),
	// Absolute paths, and relative paths that look like files/dirs.
	regexp.MustCompile(`(?:/|~/|\$[A-Z_]+/|\.?\.?/)[\w./-]+[\w.-]`),
	// Windows-ish paths.
	regexp.MustCompile(`\b[A-Za-z]:\\[\w.\\-]+`),
	// snake_case and kebab-case identifiers with a dot-or-underscore-or-hyphen
	// AND at least one ASCII letter on each side (so "well-known" prose is not
	// eaten but "min_compress_ratio" and "e2e-test" are).
	regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9]*(?:[_-][A-Za-z0-9]+)+\b`),
	// camelCase / PascalCase identifiers.
	regexp.MustCompile(`\b[A-Za-z]+(?:[A-Z][a-z0-9]+)+\b`),
	// Quoted strings (single or double) — a user's literal text stays literal.
	regexp.MustCompile(`"(?:[^"\\\n]|\\.)*"`),
	regexp.MustCompile(`'(?:[^'\\\n]|\\.)*'`),
	// Numbers with units, versions, hex, percentages.
	regexp.MustCompile(`\b0[xX][0-9a-fA-F]+\b`),
	regexp.MustCompile(`\b\d+(?:\.\d+)+(?:\.\d+)*\b`),
	regexp.MustCompile(`\b\d+(?:\.\d+)?\s?(?:%|ms|s|kb|mb|gb|kB|MB|GB|TB|k|M|G|T|px|em|rem|deg|rpm)\b`),
	regexp.MustCompile(`\b\d{1,3}(?:,\d{3})+\b`),
}

// placeholder wraps a span index in NULs. \x00 is not a word character, so no
// rule pattern using \b or [\w] can match it, and it cannot appear in the JSON
// we ship (encoding/json escapes it as \u0000, which is still inert here
// because protection happens on the decoded Go string).
func placeholder(i int) string { return fmt.Sprintf("\x00%d\x00", i) }

// span is one protected byte range [start,end) in the original text.
type span struct{ start, end int }

// protect lifts every protected span out of s, returning the placeholder'd
// text and the spans in order. Restoring is a straight index lookup, so the
// output of restore(protect(s)) is byte-identical to s — that round trip is a
// tested invariant, because everything downstream depends on it.
func protect(s string) (string, []string) {
	var spans []string
	// Mask matched regions so overlapping patterns cannot double-claim bytes.
	masked := make([]bool, len(s))
	var out strings.Builder
	var found []span

	for _, re := range protectSpans {
		for _, m := range re.FindAllStringIndex(s, -1) {
			a, b := m[0], m[1]
			if b <= a {
				continue // zero-width match: nothing to protect
			}
			// Reject the match if ANY byte in [a,b) is already claimed. Checking
			// only the endpoints is not enough: a later pattern can start inside
			// an existing span and end outside it, which yields overlapping
			// ranges — and overlapping ranges make the placeholder rebuild slice
			// backwards (prev > sp.start) and panic. Overlap ⇒ skip, so spans
			// stay strictly disjoint and the earlier (more specific) claim wins.
			overlaps := false
			for i := a; i < b; i++ {
				if masked[i] {
					overlaps = true
					break
				}
			}
			if overlaps {
				continue
			}
			for i := a; i < b; i++ {
				masked[i] = true
			}
			found = append(found, span{a, b})
		}
	}
	// Apply in text order so placeholders read left-to-right like the prose.
	sortSpans(found)
	prev := 0
	for _, sp := range found {
		if sp.start < prev {
			// Unreachable with disjoint spans; guarded so a future pattern can
			// never turn a bad range into a panic in the request path.
			continue
		}
		out.WriteString(s[prev:sp.start])
		out.WriteString(placeholder(len(spans)))
		spans = append(spans, s[sp.start:sp.end])
		prev = sp.end
	}
	out.WriteString(s[prev:])
	return out.String(), spans
}

// sortSpans orders by start (then by longer first, though masking makes
// overlaps impossible). Small insertion sort: span counts per message are tiny
// and this avoids pulling "sort" into the hot path for a 5-element slice.
func sortSpans(sp []span) {
	for i := 1; i < len(sp); i++ {
		for j := i; j > 0 && sp[j].start < sp[j-1].start; j-- {
			sp[j], sp[j-1] = sp[j-1], sp[j]
		}
	}
}

// restore puts every span back. A missing or malformed placeholder means the
// text was damaged — ok=false tells the caller to discard the rewrite and keep
// the original bytes (fail-open, contract rule 3).
func restore(s string, spans []string) (string, bool) {
	var out strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '\x00' {
			end := strings.IndexByte(s[i+1:], '\x00')
			if end < 0 {
				return "", false // truncated placeholder
			}
			numStr := s[i+1 : i+1+end]
			var idx int
			if _, err := fmt.Sscanf(numStr, "%d", &idx); err != nil || idx < 0 || idx >= len(spans) {
				return "", false // unknown span index
			}
			out.WriteString(spans[idx])
			i += end + 2
			continue
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String(), true
}

// countWords is the denominator for the word-loss ceiling. Deliberately crude:
// caveman's job is to remove *filler*, and filler is made of words, so counting
// whitespace-separated tokens is the right granularity. It must never be
// confused for a tokenizer — see EstimateTokens for the size estimate.
func countWords(s string) int {
	return len(strings.Fields(s))
}
