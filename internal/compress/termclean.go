package compress

import (
	"regexp"
	"strconv"
	"strings"
)

// ── terminal-output engines: ansi and fold ─────────────────────────────────
//
// Both act on log- and search-class tool output only (rtk's classes, so a
// profile's tool_classes binds them too). File reads are verbatim — an escape
// byte or a repeated line in a file IS the file — and unknown tools are left
// alone. JSON content is never edited. Each is deterministic per text, so the
// committed-prefix memo freezes the first shipped form. Run them before rtk so
// rtk's head/tail budget is spent on real lines.

// terminalClass reports whether a tool-output part is log or search output.
func terminalClass(p textPart, classes map[string]toolClass) bool {
	if p.kind != partToolOutput || p.text == "" || isJSONPayload(p.text) {
		return false
	}
	c := toolClassFor(p.tool, classes)
	return c == toolLog || c == toolSearch
}

// ansi removes what a terminal would never have displayed: escape sequences
// (colour, cursor control, window titles, charset switches), and lines
// rewritten in place with carriage returns or backspaces (progress bars,
// spinners), which collapse to the state the terminal finally showed. The
// visible text is unchanged, so this is lossless for a reader of the screen.
func ansi(msgs []any, opts map[string]any) []any {
	classes := toolClassOverrides(opts)
	return editAll(msgs, callNames(msgs, opts), func(p textPart) (string, bool) {
		if !terminalClass(p, classes) || !strings.ContainsAny(p.text, "\x1b\r\b") {
			return "", false
		}
		out := renderTerminal(p.text)
		if len(out) >= len(p.text) {
			return "", false
		}
		return out, true
	})
}

var (
	oscSeq     = regexp.MustCompile("\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)") // window title, hyperlinks
	csiSeq     = regexp.MustCompile("\x1b\\[[0-?]*[ -/]*[@-~]")             // colour, cursor, erase
	charsetSeq = regexp.MustCompile("\x1b[()*+][0-9A-Za-z]")                // charset designation
	escSeq     = regexp.MustCompile("\x1b[@-Z\\\\-_]")                      // other two-byte escapes
)

// renderTerminal strips escape sequences, then replays each line's carriage
// returns and backspaces the way a terminal would: \r returns the cursor to
// column 0, \b moves it left one, and later characters overwrite earlier
// ones (a shorter overwrite leaves the old tail visible, as on screen).
func renderTerminal(text string) string {
	text = oscSeq.ReplaceAllString(text, "")
	text = csiSeq.ReplaceAllString(text, "")
	text = charsetSeq.ReplaceAllString(text, "")
	text = escSeq.ReplaceAllString(text, "")
	if !strings.ContainsAny(text, "\r\b") {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, ln := range lines {
		if !strings.ContainsAny(ln, "\r\b") {
			continue
		}
		var buf []rune
		cur := 0
		for _, r := range ln {
			switch r {
			case '\r':
				cur = 0
			case '\b':
				if cur > 0 {
					cur--
				}
			default:
				if cur < len(buf) {
					buf[cur] = r
				} else {
					buf = append(buf, r)
				}
				cur++
			}
		}
		lines[i] = string(buf)
	}
	return strings.Join(lines, "\n")
}

// fold collapses runs of EXACTLY identical lines into the first line plus a
// count marker. Lines that differ by anything (a counter, a timestamp) carry
// different information and are never folded; blank lines are left to lite.
// Fenced regions are atomic (an unclosed fence skips the content). A run folds
// only when the marker is shorter than the lines it replaces. Option:
// min_repeat (default 3, at least 2) — how long a run must be to fold.
func fold(msgs []any, opts map[string]any) []any {
	minRepeat := intOption(opts, "min_repeat", 3)
	if minRepeat < 2 {
		minRepeat = 3
	}
	classes := toolClassOverrides(opts)
	return editAll(msgs, callNames(msgs, opts), func(p textPart) (string, bool) {
		if !terminalClass(p, classes) {
			return "", false
		}
		out, ok := foldRepeats(p.text, minRepeat)
		if !ok || len(out) >= len(p.text) {
			return "", false
		}
		return out, true
	})
}

func foldRepeats(text string, minRepeat int) (string, bool) {
	units, ok := splitUnits(text)
	if !ok {
		return "", false
	}
	out := make([]string, 0, len(units))
	changed := false
	for i := 0; i < len(units); {
		u := units[i]
		j := i + 1
		if !u.inside && strings.TrimSpace(u.text) != "" {
			for j < len(units) && !units[j].inside && units[j].text == u.text {
				j++
			}
		}
		out = append(out, u.text)
		// A run folds only when its marker is shorter than the lines it
		// replaces: three copies of "x" stay three lines.
		if run := j - i; run >= minRepeat && len(foldMarker(run-1)) < (run-1)*(len(u.text)+1) {
			out = append(out, foldMarker(run-1))
			changed = true
		} else {
			for k := i + 1; k < j; k++ {
				out = append(out, units[k].text)
			}
		}
		i = j
	}
	if !changed {
		return "", false
	}
	return strings.Join(out, "\n"), true
}

func foldMarker(more int) string {
	if more == 1 {
		return "[ezllm/fold: previous line repeated 1 more time]"
	}
	return "[ezllm/fold: previous line repeated " + strconv.Itoa(more) + " more times]"
}
