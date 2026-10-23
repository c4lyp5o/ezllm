package compress

import (
	"strings"
)

// lite is omniroute's safe formatting-cleanup tier (their Lite mode: zero
// semantic change, always-on). It never rewrites words — filler-stripping
// and phrase condensation belong to caveman, which ships only after its own
// safety design pass. What lite does:
//
//  1. strips trailing spaces/tabs on lines OUTSIDE code fences,
//  2. collapses runs of blank lines down to blank_lines (default 1),
//  3. trims blank lines at the very start and end of the content.
//
// Fences are atomic: splitUnits groups each ``` region as one unit and lite
// passes those through byte-identical (same rule as rtk — trailing spaces
// inside code are preserved, an unclosed fence skips the content whole).
// Content that parses as pure JSON (object or array) is skipped whole so
// headroom owns payloads and stays honest about its savings baseline.
//
// Like every engine: if the result is not strictly smaller, the original
// message is kept unchanged.
func lite(msgs []any, opts map[string]any) []any {
	keepBlanks := intOption(opts, "blank_lines", 1)
	if keepBlanks < 0 {
		keepBlanks = 1
	}
	// Every text part, prose or tool output, in every shape (parts.go) —
	// except preserve-class tool output: a file read must reach the model
	// byte-identical, or the agent's next edit (which quotes it back) fails
	// to match.
	classes := toolClassOverrides(opts)
	return editAll(msgs, callNames(msgs, opts), func(p textPart) (string, bool) {
		if p.text == "" || isJSONPayload(p.text) {
			return "", false // pure JSON payload: headroom's territory
		}
		if p.kind == partToolOutput && toolClassFor(p.tool, classes) == toolPreserve {
			return "", false
		}
		cleaned, ok := cleanText(p.text, keepBlanks)
		if !ok || len(cleaned) >= len(p.text) {
			return "", false // integrity skip or no gain
		}
		return cleaned, true
	})
}

// cleanText runs the whitespace pass over content, keeping fenced regions
// byte-identical. ok=false = unclosed fence, leave the content alone.
func cleanText(content string, keepBlanks int) (string, bool) {
	units, ok := splitUnits(content)
	if !ok {
		return "", false
	}
	lines := make([]string, 0, len(units))
	blankRun := 0
	// A blank unit only counts toward a run when it sits between plain
	// lines; a fence resets the run so blanks on either side are judged
	// independently.
	for _, u := range units {
		if u.inside {
			lines = append(lines, u.text)
			blankRun = 0
			continue
		}
		if strings.TrimSpace(u.text) == "" {
			blankRun++
			if blankRun <= keepBlanks {
				lines = append(lines, "")
			}
			continue
		}
		blankRun = 0
		lines = append(lines, strings.TrimRight(u.text, " \t"))
	}
	// Edge trim: no leading/trailing blank lines in the final text.
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n"), true
}
