package compress

import (
	"encoding/json"
	"strings"
	"testing"
)

// trailingToolBodies ends one conversation with a fresh tool result, in each
// wire shape. extra is appended after it to model the next request.
func trailingToolBodies(extra bool) map[string][]byte {
	log := shapeLog()
	next := func(chat bool) []any {
		if !extra {
			return nil
		}
		if chat {
			return []any{map[string]any{"role": "assistant", "content": "fixed"}, map[string]any{"role": "user", "content": "thanks"}}
		}
		return []any{map[string]any{"type": "message", "role": "assistant", "content": "fixed"}, map[string]any{"type": "message", "role": "user", "content": "thanks"}}
	}
	chat := append([]any{
		map[string]any{"role": "user", "content": "build it"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "bash", "arguments": "{}"}}}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": log},
	}, next(true)...)
	messages := append([]any{
		map[string]any{"role": "user", "content": "build it"},
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "c1", "name": "Bash", "input": map[string]any{}}}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "c1", "content": log}}},
	}, next(true)...)
	responses := append([]any{
		map[string]any{"type": "message", "role": "user", "content": "build it"},
		map[string]any{"type": "function_call", "call_id": "c1", "name": "bash", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "c1", "output": log},
	}, next(false)...)
	mk := func(key string, list []any) []byte {
		b, _ := json.Marshal(map[string]any{"model": "m", key: list})
		return b
	}
	return map[string][]byte{"chat": mk("messages", chat), "messages": mk("messages", messages), "responses": mk("input", responses)}
}

func freshProfile(fresh bool) *Profile {
	return &Profile{Name: "f", Enabled: true, ExemptLastTurn: true, MinCompressRatio: 0.05,
		FreshToolOutput: fresh, Stages: []Stage{{Engine: "rtk"}, {Engine: "caveman", Options: map[string]any{"intensity": "standard"}}}}
}

// compress: a final message that is purely tool output is trimmed on its
// FIRST send, in every shape, and the next request replays exactly that
// form — maximum savings with no already-sent message ever changing.
func TestFreshToolOutputCompressedOnFirstSendAndFrozen(t *testing.T) {
	first, second := trailingToolBodies(false), trailingToolBodies(true)
	for _, shape := range []string{"chat", "messages", "responses"} {
		memo := NewSessionMemo(nil)
		out1, r1 := ApplyWithSession(first[shape], freshProfile(true), nil, "auto:"+shape, memo)
		if !r1.Applied || !strings.Contains(string(out1), "lines elided") {
			t.Fatalf("%s: fresh tool output not trimmed on first send (%s)", shape, r1.Err)
		}
		out2, _ := ApplyWithSession(second[shape], freshProfile(true), nil, "auto:"+shape, memo)
		trimmed := editedTexts(t, out1)
		again := editedTexts(t, out2)[:len(trimmed)]
		if strings.Join(again, "\x00") != strings.Join(trimmed, "\x00") {
			t.Errorf("%s: the shipped tool result changed on the next request", shape)
		}
	}
}

// keep (the default): the trailing tool result is exempt like any last turn.
func TestFreshToolOutputKeepLeavesTheLastTurnAlone(t *testing.T) {
	for shape, body := range trailingToolBodies(false) {
		out, _ := ApplyWithSession(body, freshProfile(false), nil, "", nil)
		if strings.Contains(string(out), "lines elided") {
			t.Errorf("%s: keep trimmed the exempt last turn", shape)
		}
	}
}

// A final message with ANY prose stays exempt even with compress: prose
// engines never touch the live turn, tool result beside it or not.
func TestFreshToolOutputNeverOpensProse(t *testing.T) {
	verbose := "I was wondering if you could basically look at the build because it seems very wrong."
	for name, last := range map[string]any{
		"user prose": map[string]any{"role": "user", "content": verbose},
		"tool_result beside text": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "c1", "content": shapeLog()},
			map[string]any{"type": "text", "text": verbose}}},
	} {
		body := bodyOf(
			map[string]any{"role": "user", "content": "build it"},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "c1", "name": "Bash", "input": map[string]any{}}}},
			last,
		)
		out, _ := ApplyWithSession(body, freshProfile(true), nil, "", nil)
		if !strings.Contains(string(out), verbose) || strings.Contains(string(out), "lines elided") {
			t.Errorf("%s: the final message was edited", name)
		}
	}
}

// The option re-versions a profile only when it is on, so every profile
// fingerprinted before it existed keeps its version.
func TestFreshToolOutputFingerprint(t *testing.T) {
	if ProfileFingerprint(freshProfile(true)) == ProfileFingerprint(freshProfile(false)) {
		t.Error("compress and keep share a fingerprint")
	}
}
