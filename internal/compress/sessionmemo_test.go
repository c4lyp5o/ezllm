package compress

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// WS-C committed-prefix stability (scoped): the session memo replays
// already-shipped transforms, and the ratio floor judges NEW work only.
// These tests pin the exact defect the design kills: an accepted rewrite
// being REJECTED on a later request as history grows, reverting
// already-transmitted history to its original bytes.

// aCompressible is a message whose content lite (whitespace cleanup)
// reliably shrinks: trailing spaces and blank-line runs.
func aCompressible() map[string]any {
	return map[string]any{"role": "user", "content": "tell me about the thing  \n" + strings.Repeat("padding padding padding   \n\n\n", 40)}
}

func memoProfile(stages ...Stage) *Profile {
	return &Profile{Name: "p1", Enabled: true, Stages: stages, ExemptLastTurn: true, MinCompressRatio: 0.05, FailOpen: true}
}

// The defect, pinned: with a stateless pipeline the SAME history is accepted
// at request k and rejected at k+1 (its saved/pre shrinks as history grows),
// so the transmitted bytes for that history revert to original. With the
// memo the committed form ships byte-identical.
func TestCommittedPrefixSurvivesGrowth(t *testing.T) {
	memo := NewSessionMemo(nil)
	p := memoProfile(Stage{Engine: "lite"})
	heavy := aCompressible()

	// request k: one compressible message, accepted.
	body := bodyOf(heavy, textMsg("user", "q1"))
	out1, res1 := ApplyWithSession(body, p, nil, "s1", memo)
	if !res1.Applied {
		t.Fatalf("request k must apply: %s", res1.Err)
	}
	hist1 := msgContent(t, out1, 0)
	if hist1 == msgContent(t, body, 0) {
		t.Fatal("the compressible message did not actually compress")
	}

	// request k+1: the SAME history plus tail noise that drowns its ratio.
	// Stateless, the floor now rejects and history reverts; with the memo it
	// does not.
	body2 := bodyOf(heavy, textMsg("user", "q1"), textMsg("user", strings.Repeat("zzz ", 4000)))
	out2, res2 := ApplyWithSession(body2, p, nil, "s1", memo)
	if !res2.Applied {
		t.Fatalf("request k+1 must still apply (committed history rides along): %s", res2.Err)
	}
	if got := msgContent(t, out2, 0); got != hist1 {
		t.Errorf("committed history changed across requests:\n  k:   %q\n  k+1: %q", hist1[:min(40, len(hist1))], got[:min(40, len(got))])
	}
}

// The exempt window slides: a message that shipped untouched (it was the
// exempt final turn) must NOT be rewritten when it becomes history.
func TestExemptWindowDoesNotRewriteCommittedOriginal(t *testing.T) {
	memo := NewSessionMemo(nil)
	p := memoProfile(Stage{Engine: "lite"})
	heavy := aCompressible()

	// request k: heavy is the EXEMPT last message -> ships verbatim.
	body := bodyOf(textMsg("system", "sys"), textMsg("user", "q"), heavy)
	out1, res1 := ApplyWithSession(body, p, nil, "s2", memo)
	_ = res1
	if got := msgContent(t, out1, 2); got != msgContent(t, body, 2) {
		t.Fatal("the exempt final message must ship verbatim")
	}

	// request k+1: heavy is now history (a new final message follows).
	body2 := bodyOf(textMsg("system", "sys"), textMsg("user", "q"), heavy, textMsg("user", "final"))
	out2, _ := ApplyWithSession(body2, p, nil, "s2", memo)
	if got := msgContent(t, out2, 2); got != msgContent(t, body2, 2) {
		t.Error("a message that shipped untouched must not be rewritten when the exempt window slides off it")
	}
}

// A floor-rejected request ships the body byte-identical AND freezes that
// verbatim form: the crossing is only ever an explicit profile change, never
// a silent re-compression of bytes that already went upstream.
func TestFloorRejectionCommitsVerbatim(t *testing.T) {
	memo := NewSessionMemo(nil)
	// floor so high nothing passes as new work.
	p := memoProfile(Stage{Engine: "lite"})
	p.MinCompressRatio = 2.0 // 200% — nothing can pass
	heavy := aCompressible()
	body := bodyOf(heavy, textMsg("user", "q"))
	out, res := ApplyWithSession(body, p, nil, "s3", memo)
	if res.Applied {
		t.Errorf("nothing may be applied at a 200%% floor: applied=%v", res.Applied)
	}
	if string(out) != string(body) {
		t.Error("a floor-rejected request must ship the body byte-identical")
	}
	if memo.Len() == 0 {
		t.Error("the shipped verbatim form must be frozen so it cannot be silently re-compressed later")
	}
}

// New work still compresses independently of committed history.
func TestNewWorkStillCompresses(t *testing.T) {
	memo := NewSessionMemo(nil)
	p := memoProfile(Stage{Engine: "lite"})
	body := bodyOf(textMsg("system", "sys"), aCompressible(), textMsg("user", "q"))
	if _, res := ApplyWithSession(body, p, nil, "s4", memo); !res.Applied {
		t.Fatalf("first request must apply: %s", res.Err)
	}
	// second request: same history (replayed) + a NEW compressible message.
	body2 := bodyOf(textMsg("system", "sys"), aCompressible(), textMsg("user", "q"), aCompressible(), textMsg("user", "final"))
	out, res := ApplyWithSession(body2, p, nil, "s4", memo)
	if !res.Applied {
		t.Fatalf("new work must compress: %s", res.Err)
	}
	// position 3 (the new compressible message) must now be transformed
	if got := msgContent(t, out, 3); got == msgContent(t, body2, 3) {
		t.Error("the new work must be transformed")
	}
	// position 1 (history) must be its committed form
	if got := msgContent(t, out, 1); got == msgContent(t, body2, 1) {
		t.Error("the committed history must not revert to original")
	}
}

// The floor must judge the NEW WORK ONLY: a big already-committed history
// must not drag a legitimate new rewrite below the floor. (This is the
// mutation the growth test cannot catch — there the whole-set ratio still
// passes.)
func TestFloorJudgesNewWorkOnly(t *testing.T) {
	memo := NewSessionMemo(nil)
	p := memoProfile(Stage{Engine: "lite"})
	// incompressible-but-large history (no trailing spaces, no blank runs)
	big := map[string]any{"role": "user", "content": strings.Repeat("plain prose line\n", 2000)}

	// request k: history ships (nothing to compress), committed verbatim.
	body := bodyOf(big, textMsg("user", "q"))
	if _, res := ApplyWithSession(body, p, nil, "s5", memo); res.Applied {
		t.Fatal("nothing should compress in the history alone")
	}
	// request k+1: a NEW compressible message. Its own ratio clears the
	// floor; the whole-set ratio would not. New-work-only judging transforms
	// it; whole-set judging leaves it untouched.
	body2 := bodyOf(big, textMsg("user", "q"), aCompressible(), textMsg("user", "final"))
	out, res := ApplyWithSession(body2, p, nil, "s5", memo)
	if !res.Applied {
		t.Fatalf("the new work must clear its own floor: %s", res.Err)
	}
	if got := msgContent(t, out, 2); got == msgContent(t, body2, 2) {
		t.Error("the new compressible message must be transformed — the floor is judging committed history again")
	}
}

// Sessions and profiles are isolated namespaces.
func TestMemoSessionAndProfileScoping(t *testing.T) {
	memo := NewSessionMemo(nil)
	p := memoProfile(Stage{Engine: "lite"})
	heavy := aCompressible()
	body := bodyOf(heavy, textMsg("user", "q"))
	out, _ := ApplyWithSession(body, p, nil, "sA", memo)
	shipped := msgContent(t, out, 0)

	// another session sees no commitment: its first request re-derives, but
	// determinism gives the same form; a DIFFERENT profile is a fresh namespace.
	p2 := memoProfile(Stage{Engine: "lite"})
	p2.Name = "p2"
	if _, ok := memo.Get("sB", "p1", heavy); ok {
		t.Error("another session must not see sA's commitment")
	}
	if _, ok := memo.Get("sA", "p2", heavy); ok {
		t.Error("another profile must not see p1's commitment")
	}
	if _, ok := memo.Get("sA", "p1", heavy); !ok {
		t.Error("the committing session+profile must see its own commitment")
	}
	if got := msgContent(t, bodyOf2(heavy), 0); got != "" {
		_ = got
	}
	if shipped == "" {
		t.Error("sanity: shipped form missing")
	}
}

func bodyOf2(m map[string]any) []byte { return bodyOf(m) }

// TTL expiry + global cap eviction keep the ledger bounded.
func TestMemoBoundsAndTTL(t *testing.T) {
	now := time.Unix(1700000000, 0)
	clock := now
	memo := NewSessionMemo(&SessionMemoOptions{TTL: time.Minute, MaxEntries: 3, Now: func() time.Time { return clock }})
	heavy := aCompressible()
	p := memoProfile(Stage{Engine: "lite"})

	// four distinct messages, cap 3 -> the LRU entry is evicted.
	for i := 0; i < 4; i++ {
		m := map[string]any{"role": "user", "content": fmt.Sprintf("content %d %s", i, strings.Repeat("x", 50))}
		shipped := map[string]any{"role": "user", "content": "compressed"}
		memo.Put("s", "p", m, shipped)
	}
	if memo.Len() != 3 {
		t.Errorf("cap must hold: entries=%d, want 3", memo.Len())
	}
	// TTL: after the ttl, Get reports not-found and drops the entry.
	memo.Put("s", "p", heavy, map[string]any{"role": "user", "content": "c"})
	clock = clock.Add(2 * time.Minute)
	if _, ok := memo.Get("s", "p", heavy); ok {
		t.Error("an expired entry must not be replayed")
	}
	if memo.Len() != 2 {
		t.Errorf("the expired entry should have been dropped on read: entries=%d, want 2", memo.Len())
	}
	_ = p
}

// Nil memo / empty session = legacy stateless path, and it still compresses.
func TestLegacyPathUnchanged(t *testing.T) {
	p := memoProfile(Stage{Engine: "lite"})
	body := bodyOf(aCompressible(), textMsg("user", "q"))
	out, res := ApplyWithSession(body, p, nil, "", nil)
	if !res.Applied {
		t.Fatalf("legacy path must compress: %s", res.Err)
	}
	if msgContent(t, out, 0) == msgContent(t, body, 0) {
		t.Error("legacy path did not compress")
	}
	// and the plain entry still works
	out2, res2 := Apply(body, p)
	if !res2.Applied || msgContent(t, out2, 0) == "" {
		t.Error("Apply must keep working unchanged")
	}
}

// Concurrency: parallel sessions must not race (run with -race).
func TestMemoConcurrentSessions(t *testing.T) {
	memo := NewSessionMemo(nil)
	p := memoProfile(Stage{Engine: "lite"})
	var wg sync.WaitGroup
	for s := 0; s < 8; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			sid := fmt.Sprintf("sess-%d", s)
			for i := 0; i < 20; i++ {
				body := bodyOf(aCompressible(), textMsg("user", fmt.Sprintf("q%d", i)), textMsg("user", "final"))
				if _, res := ApplyWithSession(body, p, nil, sid, memo); res.Err != "" && !strings.Contains(res.Err, "ratio floor") {
					t.Errorf("unexpected error: %s", res.Err)
					return
				}
			}
		}(s)
	}
	wg.Wait()
}
