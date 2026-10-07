package compress

import (
	"encoding/json"
	"strings"
	"testing"
)

func bodyOf(msgs ...any) []byte {
	b, err := json.Marshal(map[string]any{
		"model": "gpt-6-luna", "stream": true, "messages": msgs,
		"temperature": 0.2, "x_custom": map[string]any{"keep": "me"},
	})
	if err != nil {
		panic(err)
	}
	return b
}

func textMsg(role, text string) map[string]any {
	return map[string]any{"role": role, "content": text}
}

func decodeMsgs(t *testing.T, body []byte) []any {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	m, ok := root["messages"].([]any)
	if !ok {
		t.Fatalf("no messages in body: %s", body)
	}
	return m
}

func fullProfile(stages ...Stage) *Profile {
	return &Profile{
		Name: "test", Enabled: true, Stages: stages,
		ExemptLastTurn: true, MinCompressRatio: 0.05, FailOpen: true,
	}
}

// Contract rule 1: the final message is never modified, even when it is an
// exact duplicate of an earlier one.
func TestExemptFinalMessage(t *testing.T) {
	body := bodyOf(
		textMsg("user", "same words"),
		textMsg("assistant", "ok"),
		textMsg("user", "same words"), // final: would dedup, must NOT
	)
	out, res := Apply(body, fullProfile(Stage{Engine: "session_dedup"}))
	if res.Err != "" && res.Saved == 0 {
		// floor may reject the 0-saving rewrite; that is also compliant
		if string(out) != string(body) {
			t.Fatalf("floor-rejected body must be byte-identical")
		}
		return
	}
	msgs := decodeMsgs(t, out)
	if len(msgs) != 3 {
		t.Fatalf("final message was dropped: %d msgs", len(msgs))
	}
	last := msgs[2].(map[string]any)
	if last["content"] != "same words" {
		t.Fatalf("final message changed: %v", last["content"])
	}
}

// session_dedup core: later exact duplicates dropped, first stays, tool
// pairing and tool_calls carriers never touched.
func TestSessionDedupDropsOnlySafeDuplicates(t *testing.T) {
	// Duplicates must be big enough to clear the 5% floor on their own —
	// tiny dups are exactly what the floor is meant to reject (pinned in
	// TestRatioFloorSendsOriginal).
	big := strings.Repeat("You are a terse, precise assistant. ", 10)
	task := strings.Repeat("Compile the tree and report every failing package. ", 8)
	body := bodyOf(
		textMsg("system", big),
		textMsg("user", task),
		textMsg("assistant", "done"),
		textMsg("system", big), // duplicate system: drop
		textMsg("user", task),  // duplicate user: drop
		map[string]any{"role": "assistant", "content": "calling", "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function",
				"function": map[string]any{"name": "run", "arguments": `{"cmd":"ls"}`}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "file1 file2"},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "file1 file2"}, // dup tool: KEEP (pairing)
	)
	out, res := Apply(body, fullProfile(Stage{Engine: "session_dedup"}))
	if !res.Applied {
		t.Fatalf("expected applied, got skip: %s (saved=%d)", res.Err, res.Saved)
	}
	msgs := decodeMsgs(t, out)
	var sys, usr, tools, calls int
	for _, m := range msgs {
		mm := m.(map[string]any)
		switch mm["role"] {
		case "system":
			sys++
		case "user":
			usr++
		case "tool":
			tools++
		}
		if _, has := mm["tool_calls"]; has {
			calls++
		}
	}
	if sys != 1 || usr != 1 {
		t.Errorf("duplicates not dropped: system=%d user=%d", sys, usr)
	}
	if tools != 2 {
		t.Errorf("tool messages must never drop (pairing): %d", tools)
	}
	if calls != 1 {
		t.Errorf("tool_calls carrier dropped/changed: %d", calls)
	}
	// contract: other fields survive the round trip
	var root map[string]any
	_ = json.Unmarshal(out, &root)
	if root["model"] != "gpt-6-luna" || root["stream"] != true {
		t.Errorf("non-message fields lost: %v", root)
	}
	if root["x_custom"] == nil {
		t.Errorf("unknown field lost from body")
	}
	if _, ok := root["temperature"].(float64); !ok {
		t.Errorf("temperature lost")
	}
}

// rtk: tool_only scope, error lines survive, noise drops, markers inserted.
func TestRTKFiltersToolOutput(t *testing.T) {
	var noisy strings.Builder
	noisy.WriteString("$ npm run build\n")
	for i := 0; i < 40; i++ {
		noisy.WriteString("  compiling node_modules/pkg/file" + string(rune('a'+i%26)) + ".js\n")
	}
	noisy.WriteString("TypeError: cannot read property x\n")
	for i := 0; i < 40; i++ {
		noisy.WriteString("  done: pkg/file" + string(rune('a'+i%26)) + ".js\n")
	}
	noisy.WriteString("Build failed with exit code 1\n")
	toolOut := noisy.String()

	body := bodyOf(
		textMsg("user", "run the build"),                                // prose: untouched
		map[string]any{"role": "tool", "content": toolOut},              // filtered
		map[string]any{"role": "tool", "content": `{"ok":false,"n":1}`}, // JSON: untouched
		textMsg("user", "final"),
	)
	out, res := Apply(body, fullProfile(Stage{Engine: "rtk", Options: map[string]any{"keep_lines": 6}}))
	if !res.Applied {
		t.Fatalf("expected applied, got: %s", res.Err)
	}
	msgs := decodeMsgs(t, out)
	if msgs[0].(map[string]any)["content"] != "run the build" {
		t.Error("rtk touched user prose (scope violation)")
	}
	if msgs[2].(map[string]any)["content"] != `{"ok":false,"n":1}` {
		t.Error("rtk touched JSON content (never mid-JSON)")
	}
	filtered, _ := msgs[1].(map[string]any)["content"].(string)
	if !strings.Contains(filtered, "TypeError: cannot read property") {
		t.Error("error line lost")
	}
	if !strings.Contains(filtered, "Build failed with exit code 1") {
		t.Error("error line lost")
	}
	if !strings.Contains(filtered, "[ezllm/rtk:") {
		t.Error("no elision marker")
	}
	if len(filtered) >= len(toolOut) {
		t.Errorf("no savings: %d -> %d", len(toolOut), len(filtered))
	}
}

// rtk: fenced regions are atomic — never split; unclosed fence skips content.
func TestRTKFencesAtomic(t *testing.T) {
	content := "header line\n```js\nkeep this code\nline two\n```\n" +
		strings.Repeat("noise log line\n", 30) + "error: boom"
	body := bodyOf(
		map[string]any{"role": "tool", "content": content},
		textMsg("user", "final"),
	)
	out, res := Apply(body, fullProfile(Stage{Engine: "rtk"}))
	if !res.Applied {
		t.Fatalf("expected applied: %s", res.Err)
	}
	filtered, _ := decodeMsgs(t, out)[0].(map[string]any)["content"].(string)
	if strings.Count(filtered, "```")%2 != 0 {
		t.Fatalf("fence split by rtk: %q", filtered)
	}
	if !strings.Contains(filtered, "keep this code") {
		t.Error("fenced code lost entirely — regions must be kept as units")
	}

	// unclosed fence => skip content entirely (integrity rule 3)
	unclosed := "start\n```js\nnever closed"
	body2 := bodyOf(map[string]any{"role": "tool", "content": unclosed}, textMsg("user", "final"))
	out2, res2 := Apply(body2, fullProfile(Stage{Engine: "rtk"}))
	if res2.Applied {
		t.Error("unclosed fence must fail open")
	}
	if string(out2) != string(body2) {
		t.Error("skip path must be byte-identical")
	}
}

// Contract rule 4: below the ratio floor the ORIGINAL ships, byte-identical.
func TestRatioFloorSendsOriginal(t *testing.T) {
	// one tiny duplicate: saving is far under 5% of total
	body := bodyOf(
		textMsg("system", strings.Repeat("long context. ", 200)),
		textMsg("user", "hi"),
		textMsg("assistant", "ok"),
		textMsg("user", "hi"), // duplicate: saves ~5 chars of ~2800
	)
	out, res := Apply(body, fullProfile(Stage{Engine: "session_dedup"}))
	if res.Applied {
		t.Fatalf("floor should reject a ~0.2%% saving, got applied (saved=%d pre=%d)", res.Saved, res.Pre)
	}
	if string(out) != string(body) {
		t.Fatal("floor path must be byte-identical")
	}
	if res.Pre == 0 {
		t.Fatal("prompt_tokens_pre must still be recorded on skip (rule 6)")
	}
	if res.Saved != 0 {
		t.Fatalf("saved must be 0 when not applied, got %d", res.Saved)
	}
}

// Contract rule 5: unknown engine / bad input fails open with the original.
func TestFailOpen(t *testing.T) {
	body := bodyOf(textMsg("user", "hello world"))
	out, res := Apply(body, fullProfile(Stage{Engine: "caveman"})) // not in M5 registry
	if res.Applied || string(out) != string(body) {
		t.Fatalf("unknown engine must fail open (applied=%v err=%s)", res.Applied, res.Err)
	}

	// no messages array (e.g. /v1/responses shape) -> passthrough
	other, _ := json.Marshal(map[string]any{"model": "m", "input": "hi"})
	out2, res2 := Apply(other, fullProfile(Stage{Engine: "session_dedup"}))
	if res2.Applied || string(out2) != string(other) {
		t.Fatal("non-chat shape must pass through")
	}
	if res2.Profile != "test" {
		t.Error("profile truth must be recorded even on skip (rule 6)")
	}
}

// auto_trigger_tokens: below N the engines never run.
func TestAutoTriggerTokens(t *testing.T) {
	p := fullProfile(Stage{Engine: "session_dedup"})
	p.AutoTriggerTokens = 1000000
	body := bodyOf(
		textMsg("user", "dup"),
		textMsg("assistant", "ok"),
		textMsg("user", "dup"),
	)
	out, res := Apply(body, p)
	if res.Applied {
		t.Fatal("must skip below auto_trigger_tokens")
	}
	if string(out) != string(body) {
		t.Fatal("skip must be byte-identical")
	}
}

// Disabled profile / empty stages = no-op.
func TestDisabledProfileNoop(t *testing.T) {
	body := bodyOf(textMsg("user", "x"))
	p := fullProfile(Stage{Engine: "session_dedup"})
	p.Enabled = false
	out, res := Apply(body, p)
	if res.Applied || string(out) != string(body) {
		t.Fatal("disabled profile must be a no-op")
	}
	if res.Profile != "test" {
		t.Error("name still recorded")
	}
}
