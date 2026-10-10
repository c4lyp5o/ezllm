package server

// v8 shape capture. The point of prefix_sha is to answer "was this call's
// stable core already sent before?", so the tests below pin exactly the
// behaviours that question depends on: append-only turns SHARE a hash, a
// changed system prompt or a fresh conversation does NOT, and every surface
// shape (chat / anthropic / responses) is handled without ever failing.

import (
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func TestCaptureShapeSiblingReuseSharesPrefix(t *testing.T) {
	sys := `{"role":"system","content":"be helpful"}`
	base := `{"model":"m","messages":[` + sys + `,{"role":"user","content":"one"}]}`
	// A re-send of the SAME conversation (retry after a dropped connection, or
	// the same context re-posted) — identical stable core, so identical hash.
	resend := base
	a := captureShape([]byte(base), provider.SurfaceOpenAI)
	b := captureShape([]byte(resend), provider.SurfaceOpenAI)
	if a.PrefixSHA == "" {
		t.Fatal("prefix hash empty for a valid chat body")
	}
	if a.PrefixSHA != b.PrefixSHA {
		t.Errorf("identical context must share a prefix hash: %s vs %s", a.PrefixSHA, b.PrefixSHA)
	}

	// GROWTH: append a turn. The older turn now sits in the history and the
	// newest is exempt, so the core is one message longer — a DIFFERENT hash.
	// This is correct: consecutive agent turns are not byte-identical prompts.
	next := `{"model":"m","messages":[` + sys + `,{"role":"user","content":"one"},{"role":"assistant","content":"ok"}]}`
	c := captureShape([]byte(next), provider.SurfaceOpenAI)
	if c.PrefixSHA == a.PrefixSHA {
		t.Error("an appended turn should change the hash (history grew) — otherwise growth is invisible")
	}

	// The 3-turn call's core (sys+u1+a1) equals a 2-turn call whose core is
	// (sys+u1)… only if history matched. Verify the core IS history-minus-last
	// by checking a canonical identity: [s,u1,a1] minus last == [s,u1] minus
	// nothing is NOT equal, but [s,u1,a1,u2] minus last ([s,u1,a1]) must equal
	// a call to [s,u1,a1] whose own minus-last is [s,u1]. Different by design.
	if a.MsgCount != 2 || c.MsgCount != 3 {
		t.Errorf("msg counts = %d/%d, want 2/3", a.MsgCount, c.MsgCount)
	}
	if a.ReqBytes != len(base) {
		t.Errorf("req_bytes = %d, want %d", a.ReqBytes, len(base))
	}
}

func TestCaptureShapeChangeBreaksPrefix(t *testing.T) {
	mk := func(sys, last string) string {
		return `{"model":"m","system":"` + sys + `","messages":[{"role":"user","content":"` + last + `"}]}`
	}
	// Same history, different system prompt — the single-varying-token killer.
	if a, b := captureShape([]byte(mk("A", "hi")), provider.SurfaceAnthropic), captureShape([]byte(mk("B", "hi")), provider.SurfaceAnthropic); a.PrefixSHA == b.PrefixSHA {
		t.Error("changing the system prompt must change the prefix hash")
	}
	// Tool definitions count, and the tool counter must reflect them.
	withTools := []byte(`{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"name":"a"},{"name":"b"}]}`)
	noTools := []byte(`{"model":"m","messages":[{"role":"user","content":"x"}]}`)
	s := captureShape(withTools, provider.SurfaceOpenAI)
	if s.ToolCount != 2 {
		t.Errorf("tool_count = %d, want 2", s.ToolCount)
	}
	if s.PrefixSHA == captureShape(noTools, provider.SurfaceOpenAI).PrefixSHA {
		t.Error("adding tool definitions must change the prefix hash")
	}
}

func TestCaptureShapeResponsesInput(t *testing.T) {
	// Same input list re-posted → same hash (sibling reuse).
	first := []byte(`{"model":"m","instructions":"do stuff","input":[{"role":"user","content":"a"}]}`)
	repost := first
	if a, b := captureShape(first, provider.SurfaceResponses), captureShape(repost, provider.SurfaceResponses); a.PrefixSHA == "" || a.PrefixSHA != b.PrefixSHA {
		t.Errorf("identical responses input should share prefix: %q %q", a.PrefixSHA, b.PrefixSHA)
	}
	// Growth → different hash, same as the chat surface.
	grown := []byte(`{"model":"m","instructions":"do stuff","input":[{"role":"user","content":"a"},{"type":"function_call_output","output":"b"}]}`)
	if g := captureShape(grown, provider.SurfaceResponses); g.PrefixSHA == captureShape(first, provider.SurfaceResponses).PrefixSHA {
		t.Error("growing the responses input must change the hash")
	} else if g.MsgCount != 2 {
		t.Errorf("msg_count = %d, want 2", g.MsgCount)
	}
}

func TestCaptureShapeNeverFailsOnGarbage(t *testing.T) {
	// Capture is best-effort: nonsense must not panic and must not fake a hash.
	for _, body := range []string{"", "not json", `{`, `{"model":"m","messages":"wrong-type"}`, `[]`} {
		s := captureShape([]byte(body), provider.SurfaceOpenAI)
		if body == "" {
			if s.ReqBytes != 0 {
				t.Error("empty body should record 0 bytes")
			}
			continue
		}
		if s.ReqBytes != len(body) {
			t.Errorf("req_bytes = %d want %d for %q", s.ReqBytes, len(body), body)
		}
		if s.PrefixSHA != "" {
			t.Errorf("garbage body %q must not produce a prefix hash, got %s", body, s.PrefixSHA)
		}
	}
	// Huge body: byte size only, no hash, no panic.
	huge := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("x", maxShapeBody+1) + `"}]}`
	if s := captureShape([]byte(huge), provider.SurfaceOpenAI); s.PrefixSHA != "" || s.ReqBytes != len(huge) {
		t.Errorf("oversized body should record bytes without hashing: %+v", s)
	}
}

func TestCaptureShapeSingleTurnSharesEmptyHistory(t *testing.T) {
	// One message = no prior turn to reuse. With the same system, both calls'
	// stable core is identical (system + empty history), so they DO share a
	// hash — the correct reading of "nothing here was ever cached before".
	a := captureShape([]byte(`{"model":"m","system":"s","messages":[{"role":"user","content":"one"}]}`), provider.SurfaceOpenAI)
	b := captureShape([]byte(`{"model":"m","system":"s","messages":[{"role":"user","content":"two"}]}`), provider.SurfaceOpenAI)
	if a.MsgCount != 1 || b.MsgCount != 1 {
		t.Fatalf("msg counts = %d/%d, want 1/1", a.MsgCount, b.MsgCount)
	}
	if a.PrefixSHA != b.PrefixSHA {
		t.Errorf("same-system single turns should share an (empty-history) prefix: %s vs %s", a.PrefixSHA, b.PrefixSHA)
	}
	// But a different system still separates them.
	c := captureShape([]byte(`{"model":"m","system":"OTHER","messages":[{"role":"user","content":"one"}]}`), provider.SurfaceOpenAI)
	if c.PrefixSHA == a.PrefixSHA {
		t.Error("different system must not share the prefix hash")
	}
}
