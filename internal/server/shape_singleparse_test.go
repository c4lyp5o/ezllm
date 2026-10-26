package server

import (
	"testing"

	"github.com/c4lyp5o/ezllm/internal/jsonbody"
	"github.com/c4lyp5o/ezllm/internal/provider"
)

// captureShapeDoc reads the request's single-parse decode
// (docs/single-parse-dispatch-design.md).

// TestCaptureShapeDocContract asserts the observable shape contract on the
// body corpus: what hashes, what does not, and why. An earlier version of this
// file compared captureShape with captureShapeDoc — but captureShape WAS that
// delegation, so it compared a function with itself and could never fail.
// These assertions can.
func TestCaptureShapeDocContract(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		surface provider.Surface
		sha     bool // expect a non-empty prefix hash
		msgs    int
		tools   int
	}{
		{
			name:    "chat body with history hashes the reusable core",
			body:    `{"model":"m","system":"s","tools":[{"type":"function","function":{"name":"f"}}],"messages":[{"role":"user","content":"one"},{"role":"assistant","content":"two"},{"role":"user","content":"three"}]}`,
			surface: provider.SurfaceChat, sha: true, msgs: 3, tools: 1,
		},
		{
			name:    "responses body seen on the chat surface has no chat history",
			body:    `{"model":"m","instructions":"be kind","input":[{"role":"user","content":"a"},{"role":"assistant","content":"b"}]}`,
			surface: provider.SurfaceChat, sha: false, msgs: 0,
		},
		{
			name:    "the same body on the responses surface hashes its input",
			body:    `{"model":"m","instructions":"be kind","input":[{"role":"user","content":"a"},{"role":"assistant","content":"b"}]}`,
			surface: provider.SurfaceResponses, sha: true, msgs: 2,
		},
		{
			name:    "a single message is all-newest, so the core is empty but hashed",
			body:    `{"model":"m","messages":[{"role":"user","content":"only one"}]}`,
			surface: provider.SurfaceChat, sha: true, msgs: 1,
		},
		{
			name:    "no conversation at all is not attributable",
			body:    `{"model":"m"}`,
			surface: provider.SurfaceChat, sha: false, msgs: 0,
		},
		{
			name:    "an unparseable body yields no shape at all",
			body:    `not json`,
			surface: provider.SurfaceChat, sha: false, msgs: 0,
		},
		{
			name:    "an empty history is still a conversation",
			body:    `{"model":"m","messages":[]}`,
			surface: provider.SurfaceChat, sha: true, msgs: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := captureShapeDoc(jsonbody.Open([]byte(tc.body)), tc.surface)
			if got := s.PrefixSHA != ""; got != tc.sha {
				t.Errorf("PrefixSHA set = %v, want %v (hash %q)", got, tc.sha, s.PrefixSHA)
			}
			if s.MsgCount != tc.msgs {
				t.Errorf("MsgCount = %d, want %d", s.MsgCount, tc.msgs)
			}
			if s.ToolCount != tc.tools {
				t.Errorf("ToolCount = %d, want %d", s.ToolCount, tc.tools)
			}
			if s.ReqBytes != len(tc.body) {
				t.Errorf("ReqBytes = %d, want %d", s.ReqBytes, len(tc.body))
			}
		})
	}
}

// The cache-reuse invariant: the hash covers the REUSABLE core, so appending a
// turn must not change it (that is what makes consecutive agent turns share a
// warm prefix), while editing the history or the system prompt must.
func TestCaptureShapeDocHashScope(t *testing.T) {
	core := func(messages string) string {
		return `{"model":"m","system":"s","messages":` + messages + `}`
	}
	one := core(`[{"role":"user","content":"one"}]`)
	two := core(`[{"role":"user","content":"one"},{"role":"assistant","content":"ok"}]`)
	three := core(`[{"role":"user","content":"one"},{"role":"assistant","content":"ok"},{"role":"user","content":"three"}]`)

	hOne := captureShapeDoc(jsonbody.Open([]byte(one)), provider.SurfaceChat).PrefixSHA
	hTwo := captureShapeDoc(jsonbody.Open([]byte(two)), provider.SurfaceChat).PrefixSHA
	hThree := captureShapeDoc(jsonbody.Open([]byte(three)), provider.SurfaceChat).PrefixSHA
	if hOne == "" || hTwo == "" || hThree == "" {
		t.Fatalf("empty hash: %q %q %q", hOne, hTwo, hThree)
	}
	// Appending a turn grows the reusable core, so it MUST read as a break.
	if hTwo == hOne {
		t.Error("appending the assistant turn did not change the prefix hash")
	}
	if hThree == hTwo {
		t.Error("appending the newest turn did not change the prefix hash")
	}
	// Editing the system prompt is a different prefix, always.
	edited := `{"model":"m","system":"OTHER","messages":[{"role":"user","content":"one"}]}`
	if h := captureShapeDoc(jsonbody.Open([]byte(edited)), provider.SurfaceChat).PrefixSHA; h == hOne {
		t.Error("a changed system prompt collided with the original prefix hash")
	}
	// The newest turn alone is exempt: swapping only it keeps the hash.
	swapNewest := core(`[{"role":"user","content":"one"},{"role":"assistant","content":"DIFFERENT"}]`)
	if h := captureShapeDoc(jsonbody.Open([]byte(swapNewest)), provider.SurfaceChat).PrefixSHA; h != hTwo {
		t.Errorf("swapping only the newest turn changed the prefix hash: %q vs %q", h, hTwo)
	}
}

// TestCaptureShapeDocNumberLiteralSensitivity is the INTENDED divergence from
// the old float64 decode: the hash now sees JSON number literals verbatim.
// `1e10` and `10000000000` are different effective prefixes (a tokenizer
// renders different tokens), so they MUST hash differently — the old float64
// normalization collapsed them and could hide a cache break. Big ints keep
// exact digits instead of being rounded through float64.
func TestCaptureShapeDocNumberLiteralSensitivity(t *testing.T) {
	// The literal must sit in the STABLE CORE (any turn but the newest —
	// prefix_sha deliberately drops the last message), or it never reaches
	// the hash at all.
	mk := func(num string) string {
		return `{"model":"m","messages":[{"role":"user","content":"` + num + `"},{"role":"assistant","content":"tail"}]}`
	}
	a := captureShapeDoc(jsonbody.Open([]byte(mk(`1e10`))), provider.SurfaceChat)
	b := captureShapeDoc(jsonbody.Open([]byte(mk(`10000000000`))), provider.SurfaceChat)
	if a.PrefixSHA == "" || a.PrefixSHA == b.PrefixSHA {
		t.Errorf("number-literal sensitivity lost: %q vs %q", a.PrefixSHA, b.PrefixSHA)
	}
}
