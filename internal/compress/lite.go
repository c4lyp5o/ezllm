package compress

import (
	"encoding/json"
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
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			out = append(out, m)
			continue
		}
		content, isStr := mm["content"].(string)
		if !isStr || content == "" {
			out = append(out, m)
			continue
		}
		trimmed := strings.TrimSpace(content)
		if trimmed != "" && (strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")) {
			if json.Valid([]byte(trimmed)) {
				out = append(out, m) // pure JSON payload: headroom's territory
				continue
			}
		}
		cleaned, ok := cleanText(content, keepBlanks)
		if !ok || len(cleaned) >= len(content) {
			out = append(out, m) // integrity skip or no gain
			continue
		}
		cp := make(map[string]any, len(mm))
		for k, v := range mm {
			cp[k] = v
		}
		cp["content"] = cleaned
		out = append(out, cp)
	}
	return out
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
