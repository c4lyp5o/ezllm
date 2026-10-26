package compress

// C1 regression: session_dedup used to drop ANY later byte-identical
// system|user|assistant turn. With ExemptLastTurn (the default) that leaves
// [U1, A1, U2, A2] rewritten to [U1, A1, A2] when U2 == U1 — the shape of a
// user who says "continue", "ok" or "yes" twice with an assistant turn
// between. The provider then receives an assistant reply with NO preceding
// question. Only system re-sends are the documented 92.5% case; repeated
// user/assistant turns are conversation content and must survive.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRepeatedUserTurnSurvivesSessionDedup(t *testing.T) {
	// The doc's exact scenario: U2 is byte-identical to U1.
	const u1 = "continue"
	const u2 = "continue"
	body := bodyOf(
		textMsg("user", u1),
		textMsg("assistant", "Ran the tests, 3 failures."),
		textMsg("user", u2),
		textMsg("assistant", "Fixed them."),
	)
	out, res := ApplyWithSession(body, fullProfile(Stage{Engine: "session_dedup"}), nil, "", nil)

	msgs := decodeMsgs(t, out)
	if len(msgs) != 4 {
		t.Fatalf("C1 REGRESSION: %d messages survived, want all 4 — a repeated user turn was dropped (applied=%v err=%q)", len(msgs), res.Applied, res.Err)
	}
	// The final assistant reply must still follow the repeated question.
	last := msgs[3].(map[string]any)
	if last["content"] != "Fixed them." {
		t.Fatalf("final message changed: %v", last["content"])
	}
	if c := msgs[2].(map[string]any)["content"]; c != u2 {
		t.Fatalf("repeated user turn U2 lost/changed: %v", c)
	}
}

// The rewrite must still compress what it is documented to compress: a
// re-sent system prompt is dropped, and a repeated user turn in the same
// conversation survives.
func TestSystemDedupStillFiresAndUserTurnsSurvive(t *testing.T) {
	sys := strings.Repeat("You are a terse, precise assistant. ", 40)
	task := strings.Repeat("Compile the tree and report every failing package. ", 20)
	body := bodyOf(
		textMsg("system", sys),
		textMsg("user", task),
		textMsg("assistant", "running it"),
		textMsg("system", sys), // re-sent system prompt: drop (documented case)
		textMsg("user", task),  // repeated user turn: KEEP (C1)
		textMsg("assistant", "3 packages failed"),
	)
	out, res := ApplyWithSession(body, fullProfile(Stage{Engine: "session_dedup"}), nil, "", nil)
	if !res.Applied {
		t.Fatalf("expected applied, got skip: %s (saved=%d)", res.Err, res.Saved)
	}

	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	msgs, _ := root["messages"].([]any)

	var sysN, userN int
	for _, m := range msgs {
		switch m.(map[string]any)["role"] {
		case "system":
			sysN++
		case "user":
			userN++
		}
	}
	if sysN != 1 {
		t.Errorf("re-sent system prompt not dropped: system=%d", sysN)
	}
	if userN != 2 {
		t.Errorf("repeated user turn dropped: user=%d — C1 regression", userN)
	}
	if len(msgs) != 5 {
		t.Errorf("message count = %d, want 5 (6 minus the duplicated system)", len(msgs))
	}
	// Non-message fields must survive the rewrite.
	if root["x_custom"] == nil || root["temperature"] == nil {
		t.Errorf("non-message fields lost: %v", root)
	}
}
