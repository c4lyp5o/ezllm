package compress

import (
	"encoding/json"
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

// Sessions and profile VERSIONS are isolated namespaces.
func TestMemoSessionAndProfileScoping(t *testing.T) {
	memo := NewSessionMemo(nil)
	p := memoProfile(Stage{Engine: "lite"})
	heavy := aCompressible()
	body := bodyOf(heavy, textMsg("user", "q"))
	out, _ := ApplyWithSession(body, p, nil, "sA", memo)
	shipped := msgContent(t, out, 0)

	v1 := ProfileFingerprint(p)
	p2 := memoProfile(Stage{Engine: "lite"})
	p2.Name = "p2"
	v2 := ProfileFingerprint(p2)
	if v1 == v2 {
		t.Fatal("distinct profiles must have distinct fingerprints")
	}
	if _, ok := memo.Get("sB", v1, heavy); ok {
		t.Error("another session must not see sA's commitment")
	}
	if _, ok := memo.Get("sA", v2, heavy); ok {
		t.Error("another profile version must not see v1's commitment")
	}
	if _, ok := memo.Get("sA", v1, heavy); !ok {
		t.Error("the committing session+version must see its own commitment")
	}
	if shipped == "" {
		t.Error("sanity: shipped form missing")
	}
}

// The fingerprint must move with ANY byte-affecting change, and must be
// stable for an equal profile.
func TestProfileFingerprintMovesWithContent(t *testing.T) {
	base := memoProfile(Stage{Engine: "lite"})
	same := memoProfile(Stage{Engine: "lite"})
	if ProfileFingerprint(base) != ProfileFingerprint(same) {
		t.Error("equal effective profiles must fingerprint equal")
	}
	cases := map[string]*Profile{
		"stage option": memoProfile(Stage{Engine: "rtk", Options: map[string]any{"keep_lines": 6}}),
		"floor":        func() *Profile { p := memoProfile(Stage{Engine: "lite"}); p.MinCompressRatio = 0.2; return p }(),
		"exempt":       func() *Profile { p := memoProfile(Stage{Engine: "lite"}); p.ExemptLastTurn = false; return p }(),
		"auto":         func() *Profile { p := memoProfile(Stage{Engine: "lite"}); p.AutoTriggerTokens = 100; return p }(),
		"stages":       memoProfile(Stage{Engine: "lite"}, Stage{Engine: "session_dedup"}),
	}
	fp := ProfileFingerprint(base)
	for name, p := range cases {
		if ProfileFingerprint(p) == fp {
			t.Errorf("%s change must produce a new fingerprint", name)
		}
	}
}

// Addendum #4: an ACTIVE session is never silently re-versioned. A profile
// edit mid-session keeps the session's binding (its committed bytes keep
// replaying under the OLD fingerprint); a NEW session binds the new version.
func TestProfileEditDoesNotRewriteActiveSession(t *testing.T) {
	memo := NewSessionMemo(nil)
	p := memoProfile(Stage{Engine: "lite"})
	heavy := aCompressible()

	// session binds v1 and commits a transformed form.
	body := bodyOf(heavy, textMsg("user", "q"))
	out1, res1 := ApplyWithSession(body, p, nil, "sV", memo)
	if !res1.Applied {
		t.Fatalf("v1 must apply: %s", res1.Err)
	}
	committed := msgContent(t, out1, 0)
	v1, ok := memo.BoundFingerprint("sV")
	if !ok {
		t.Fatal("the session must have bound a version")
	}

	// the operator EDITS the profile (same name, new stage) — a new fingerprint.
	p.Stages = []Stage{{Engine: "lite"}, {Engine: "session_dedup"}}
	v2 := ProfileFingerprint(p)
	if v1 == v2 {
		t.Fatal("the edit must move the fingerprint")
	}
	// the ACTIVE session still replays its committed bytes.
	body2 := bodyOf(heavy, textMsg("user", "q"), textMsg("user", "final"))
	out2, res2 := ApplyWithSession(body2, p, nil, "sV", memo)
	if !res2.Applied {
		t.Fatalf("the active session must keep shipping: %s", res2.Err)
	}
	if got := msgContent(t, out2, 0); got != committed {
		t.Error("a profile edit must NOT rewrite an active session's committed bytes")
	}
	if bound, _ := memo.BoundFingerprint("sV"); bound != v1 {
		t.Errorf("the active session's binding must stay v1, got %s", bound)
	}
	// a NEW session binds the new version.
	ApplyWithSession(body2, p, nil, "sV2", memo)
	if bound, _ := memo.BoundFingerprint("sV2"); bound != v2 {
		t.Errorf("a new session must bind the new version, got %s want %s", bound, v2)
	}
}

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

// The derived key must stay the same as a conversation grows — in every wire
// shape — and differ per client and per conversation opening.
func TestConversationKeyStableAcrossGrowthInEveryShape(t *testing.T) {
	sys := map[string]any{"role": "system", "content": "be terse"}
	first := map[string]any{"role": "user", "content": "first ask"}
	later := []any{map[string]any{"role": "assistant", "content": "a"}, map[string]any{"role": "user", "content": "b"}}
	shapes := map[string]func(extra []any) map[string]any{
		"chat": func(extra []any) map[string]any {
			return map[string]any{"messages": append([]any{sys, first}, extra...)}
		},
		"messages": func(extra []any) map[string]any {
			return map[string]any{"system": "be terse", "messages": append([]any{first}, extra...)}
		},
		"responses": func(extra []any) map[string]any {
			return map[string]any{"instructions": "be terse", "input": append([]any{map[string]any{"type": "message", "role": "user", "content": "first ask"}}, extra...)}
		},
	}
	for name, mk := range shapes {
		k1, k2 := ConversationKey("hermes", mk(nil)), ConversationKey("hermes", mk(later))
		if k1 == "" || k1 != k2 {
			t.Errorf("%s: key moved as the conversation grew: %q -> %q", name, k1, k2)
		}
		if ConversationKey("claude-code", mk(nil)) == k1 {
			t.Errorf("%s: two clients share a key", name)
		}
	}
	other := map[string]any{"messages": []any{sys, map[string]any{"role": "user", "content": "another ask"}}}
	if ConversationKey("hermes", other) == ConversationKey("hermes", shapes["chat"](nil)) {
		t.Error("two conversations with different openings share a key")
	}
}

// A request where session_dedup drops a repeat still COMMITS what shipped —
// the drop at its occurrence, and the exempt last turn verbatim. It used to
// commit nothing whenever a message was dropped, so the next request
// re-compressed the turn that had just shipped untouched (here: lite trimming
// q1's padding), changing an already-sent message and breaking the prefix.
func TestDedupDropIsCommittedPerOccurrence(t *testing.T) {
	memo := NewSessionMemo(nil)
	p := memoProfile(Stage{Engine: "session_dedup"}, Stage{Engine: "lite"})
	p.MinCompressRatio = 0
	sys := textMsg("system", strings.Repeat("follow the style guide exactly. ", 40))
	msgs1 := []any{sys, textMsg("user", "a"), textMsg("assistant", "b"), sys, textMsg("user", "q1   \n\n\n\n\nplease")}
	out1, r1 := ApplyWithSession(bodyOf(msgs1...), p, nil, "auto:x", memo)
	if !r1.Applied {
		t.Fatalf("first request did not drop the repeat: %s", r1.Err)
	}
	msgs2 := append(append([]any{}, msgs1...), textMsg("assistant", "c"), textMsg("user", "q2"))
	out2, _ := ApplyWithSession(bodyOf(msgs2...), p, nil, "auto:x", memo)
	var b1, b2 map[string][]any
	_ = json.Unmarshal(out1, &b1)
	_ = json.Unmarshal(out2, &b2)
	if len(b1["messages"]) != 4 {
		t.Fatalf("request 1 shipped %d messages, want 4 (repeat dropped)", len(b1["messages"]))
	}
	// Request 2's first four shipped messages are request 1's shipped history
	// (q1 included, exempt then and frozen since).
	if string(mustMarshal(b2["messages"][:4])) != string(mustMarshal(b1["messages"])) {
		t.Fatalf("shipped prefix changed between requests:\n1: %s\n2: %s", string(mustMarshal(b1["messages"])), string(mustMarshal(b2["messages"][:4])))
	}
	if strings.Count(string(out2), "follow the style guide") != strings.Count(string(out1), "follow the style guide") {
		t.Error("the first system copy was dropped or the repeat came back")
	}
}

// Eviction drops the LEAST-recently-used entry (a read refreshes recency),
// and expired entries are swept on insert, not only when read.
func TestMemoEvictsLeastRecentlyUsed(t *testing.T) {
	clock := time.Unix(1700000000, 0)
	memo := NewSessionMemo(&SessionMemoOptions{TTL: time.Minute, MaxEntries: 2, Now: func() time.Time { return clock }})
	msg := func(s string) any { return map[string]any{"role": "user", "content": s} }
	memo.Put("s", "p", msg("a"), msg("A"))
	memo.Put("s", "p", msg("b"), msg("B"))
	if _, ok := memo.Get("s", "p", msg("a")); !ok { // a is now the most recent
		t.Fatal("a missing before eviction")
	}
	memo.Put("s", "p", msg("c"), msg("C"))
	if _, ok := memo.Get("s", "p", msg("b")); ok {
		t.Error("b (least recently used) survived eviction")
	}
	for _, k := range []string{"a", "c"} {
		if _, ok := memo.Get("s", "p", msg(k)); !ok {
			t.Errorf("%s evicted, want kept", k)
		}
	}
	clock = clock.Add(2 * time.Minute) // a and c expire
	memo.Put("s", "p", msg("d"), msg("D"))
	if memo.Len() != 1 {
		t.Errorf("expired entries not swept on insert: entries=%d, want 1", memo.Len())
	}
}
