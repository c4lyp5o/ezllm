package compress

import (
	"encoding/json"
	"strings"
	"testing"
)

// toolBodies wraps one tool output in all three wire shapes, as the result of
// tool `name`, followed by a short final user turn (so the output is history,
// not the exempt last message).
func toolBodies(name, output string) map[string][]byte {
	chat := []any{
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": name, "arguments": "{}"}}}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": output},
		map[string]any{"role": "user", "content": "go"},
	}
	messages := []any{
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "c1", "name": name, "input": map[string]any{}}}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "c1", "content": output}}},
		map[string]any{"role": "user", "content": "go"},
	}
	responses := []any{
		map[string]any{"type": "function_call", "call_id": "c1", "name": name, "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "c1", "output": output},
		map[string]any{"type": "message", "role": "user", "content": "go"},
	}
	mk := func(key string, list []any) []byte {
		b, _ := json.Marshal(map[string]any{"model": "m", key: list})
		return b
	}
	return map[string][]byte{"chat": mk("messages", chat), "messages": mk("messages", messages), "responses": mk("input", responses)}
}

// toolOutputAfter runs one stage over the output in every shape and returns
// the resulting tool text per shape.
func toolOutputAfter(t *testing.T, st Stage, name, output string) map[string]string {
	t.Helper()
	p := &Profile{Name: "p", Enabled: true, ExemptLastTurn: true, MinCompressRatio: 0, Stages: []Stage{st}}
	got := map[string]string{}
	for shape, body := range toolBodies(name, output) {
		out, _ := Apply(body, p)
		texts := editedTexts(t, out)
		got[shape] = texts[0]
	}
	return got
}

// sameInEveryShape fails unless all three shapes produced want.
func sameInEveryShape(t *testing.T, label string, got map[string]string, want string) {
	t.Helper()
	for shape, g := range got {
		if g != want {
			t.Errorf("%s/%s:\n got %q\nwant %q", label, shape, g, want)
		}
	}
}

// ── ansi: what a terminal would never have displayed ───────────────────────

// Colour and other escape sequences go; the text a terminal shows stays.
func TestANSIStripsEscapeSequences(t *testing.T) {
	in := "\x1b[1m\x1b[32m✓\x1b[0m compiled \x1b[33mpkg/a\x1b[39m\n" +
		"\x1b]0;window title\x07\x1b[2Kbuilding\x1b[K\n" +
		"\x1b(Bplain\n" + strings.Repeat("filler line\n", 4) + "done"
	want := "✓ compiled pkg/a\nbuilding\nplain\n" + strings.Repeat("filler line\n", 4) + "done"
	sameInEveryShape(t, "ansi", toolOutputAfter(t, Stage{Engine: "ansi"}, "bash", in), want)
}

// A line rewritten with carriage returns (a progress bar) collapses to what
// the terminal finally showed — including the tail a shorter overwrite leaves
// behind, and a CRLF line ending.
func TestANSICollapsesCarriageReturnOverwrites(t *testing.T) {
	in := "Downloading  10%\rDownloading  55%\rDownloading 100%\n" +
		"abcdef\rXY\n" +
		"windows line\r\n" +
		"back\b\bCK\n" + strings.Repeat("filler line\n", 4) + "done"
	want := "Downloading 100%\nXYcdef\nwindows line\nbaCK\n" + strings.Repeat("filler line\n", 4) + "done"
	sameInEveryShape(t, "ansi", toolOutputAfter(t, Stage{Engine: "ansi"}, "bash", in), want)
}

// File reads are verbatim (an escape byte in a file is the file), and so is
// output with nothing to clean.
func TestANSILeavesFileReadsAndCleanOutputAlone(t *testing.T) {
	colored := "\x1b[31mred\x1b[0m\n" + strings.Repeat("x\n", 6)
	sameInEveryShape(t, "ansi file read", toolOutputAfter(t, Stage{Engine: "ansi"}, "read_file", colored), colored)
	clean := strings.Repeat("plain line\n", 8)
	sameInEveryShape(t, "ansi clean", toolOutputAfter(t, Stage{Engine: "ansi"}, "bash", clean), clean)
}

// ── fold: exact repeats become one line plus a count ───────────────────────

func TestFoldCollapsesExactRepeats(t *testing.T) {
	in := "start\n" + strings.Repeat("waiting for lock\n", 6) + "got it\n" + "a\na\nb\n" + "done"
	want := "start\nwaiting for lock\n[ezllm/fold: previous line repeated 5 more times]\ngot it\na\na\nb\ndone"
	sameInEveryShape(t, "fold", toolOutputAfter(t, Stage{Engine: "fold"}, "bash", in), want)
}

// min_repeat sets how long a run must be before it folds (default 3); lines
// that differ by anything (a counter) are different lines, never folded; and
// a run folds only when the marker is shorter than the lines it replaces.
func TestFoldRespectsMinRepeatAndNearMisses(t *testing.T) {
	line := "npm WARN deprecated inflight@1.0.6: This module is not supported"
	in := strings.Repeat(line+"\n", 3) + "tick 1\ntick 2\ntick 3\ntick 4\n" + "x\nx\nx\nx\nx\ndone"
	sameInEveryShape(t, "fold min_repeat=4", toolOutputAfter(t, Stage{Engine: "fold", Options: map[string]any{"min_repeat": 4}}, "bash", in), in)
	sameInEveryShape(t, "fold default", toolOutputAfter(t, Stage{Engine: "fold"}, "bash", in),
		line+"\n[ezllm/fold: previous line repeated 2 more times]\ntick 1\ntick 2\ntick 3\ntick 4\nx\nx\nx\nx\nx\ndone")
}

// Fenced regions are atomic, file reads are verbatim, JSON is never edited.
func TestFoldLeavesFencesFileReadsAndJSONAlone(t *testing.T) {
	fenced := "```\nsame\nsame\nsame\nsame\n```\nend"
	sameInEveryShape(t, "fold fence", toolOutputAfter(t, Stage{Engine: "fold"}, "bash", fenced), fenced)
	repeats := strings.Repeat("same\n", 8) + "end"
	sameInEveryShape(t, "fold file read", toolOutputAfter(t, Stage{Engine: "fold"}, "read_file", repeats), repeats)
	js := "[\n" + strings.Repeat("  1,\n", 6) + "  1\n]"
	sameInEveryShape(t, "fold json", toolOutputAfter(t, Stage{Engine: "fold"}, "bash", js), js)
}
