package compress

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/jsonbody"
)

// A shipped rewrite leaves the request's shared decode untouched.
func TestApplyDoesNotMutateTheSharedDoc(t *testing.T) {
	body := benchConversation(4)
	doc := jsonbody.Open(body)
	before, _ := json.Marshal(doc.Map())
	p := memoProfile(Stage{Engine: "lite"}, Stage{Engine: "caveman", Options: map[string]any{"intensity": "standard"}})
	_, res := ApplyWithSession(body, p, doc, "s-doc", NewSessionMemo(nil))
	if !res.Applied {
		t.Skipf("no rewrite shipped: %s", res.Err)
	}
	after, _ := json.Marshal(doc.Map())
	if string(before) != string(after) {
		t.Fatal("ApplyWithSession changed doc.Map()")
	}
}

// A message that mentions ``` mid-sentence arrives with an odd fence count;
// that is the client's text and must not turn compression off.
func TestIntegrityAllowsAnOddFenceTheClientSent(t *testing.T) {
	odd := map[string]any{"role": "user", "content": "Basically, type ``` to start a block, and really that is it."}
	if msg := integrityFailure([]any{odd}, []any{odd}); msg != "" {
		t.Fatalf("integrity = %q for an unchanged odd message", msg)
	}
	balanced := map[string]any{"role": "user", "content": "```\ncode\n```"}
	broken := map[string]any{"role": "user", "content": "```\ncode\n"}
	if msg := integrityFailure([]any{balanced}, []any{broken}); msg == "" {
		t.Fatal("a rewrite that unbalanced the fences passed")
	}
	// Tool results count too (Anthropic tool_result blocks).
	tr := func(s string) map[string]any {
		return map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": s}}}
	}
	if msg := integrityFailure([]any{tr("```\nx\n```")}, []any{tr("```\nx\n")}); msg == "" {
		t.Fatal("an unbalanced tool_result passed")
	}
}

// A memo hit refreshes the session's own LRU, so the session cap evicts the
// least recently READ entry, not the conversation's opening message.
func TestSessionMemoHitRefreshesSessionLRU(t *testing.T) {
	m := NewSessionMemo(&SessionMemoOptions{MaxPerKey: 2})
	first := textMsg("system", "the instructions")
	m.PutAt("s", "p", first, 0, first)
	m.PutAt("s", "p", textMsg("user", "two"), 0, textMsg("user", "two"))
	if _, ok := m.GetAt("s", "p", first, 0); !ok {
		t.Fatal("first entry missing")
	}
	m.PutAt("s", "p", textMsg("user", "three"), 0, textMsg("user", "three"))
	if _, ok := m.GetAt("s", "p", first, 0); !ok {
		t.Fatal("the just-read opening message was evicted first")
	}
}

// The hygiene gate measures the true shortest match.
func TestMinMatchLenIsExact(t *testing.T) {
	for pattern, want := range map[string]int{
		`(?:hello world|hi)`: 2, `[abcdef]x`: 2, `in order to`: 11, `\bfoo\s+`: 4, `(?:please )?ok`: 2, `a{3}`: 3,
	} {
		if got := minMatchLen(mustCompileRe(t, pattern)); got != want {
			t.Errorf("minMatchLen(%q) = %d, want %d", pattern, got, want)
		}
	}
}

// A replacement with $1 would be emitted literally; the pack loader refuses it.
func TestRuleRejectsCaptureReference(t *testing.T) {
	repl := "$1 "
	if _, msg, ok := validateRule("en", "dedup", rawRule{Name: "r", Pattern: `([.!?])\s{5,}`, Replacement: &repl}); ok || !strings.Contains(msg, "capture reference") {
		t.Fatalf("ok=%v msg=%q, want a capture-reference rejection", ok, msg)
	}
	packs, _ := parsePacks(embeddedPacks())
	out, _, _ := applyPacks(packs, "Hello.  World, fine;   ok", intensityStandard)
	if strings.Contains(out, "$") {
		t.Fatalf("packs wrote a literal capture reference: %q", out)
	}
}

// Progress bars redraw with \r plus erase-line; the old bar must not show.
func TestRenderTerminalEraseLine(t *testing.T) {
	for in, want := range map[string]string{
		"Downloading [===>] 40%\r\x1b[2KDone": "Done",
		"Downloading [===>] 40%\r\x1b[KDone":  "Done",
		"progress 99%\rok\x1b[0K":             "ok",
		"plain \x1b[1mbold\x1b[0m text":       "plain bold text",
		"abc\x1b[2Kd":                         "   d",
	} {
		if got := renderTerminal(in); got != want {
			t.Errorf("renderTerminal(%q) = %q, want %q", in, got, want)
		}
	}
}
