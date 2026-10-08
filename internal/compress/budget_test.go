package compress

import (
	"encoding/json"
	"testing"
)

func TestBudgetPreservesEnvelopeAndLatest(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "system", "content": "permanent instruction"},
		map[string]any{"role": "user", "content": "initial request"},
		map[string]any{"role": "assistant", "content": "old answer with lots of detail"},
		map[string]any{"role": "user", "content": "old follow-up with lots of detail"},
		map[string]any{"role": "assistant", "content": "latest answer"},
		map[string]any{"role": "user", "content": "latest question"},
	}
	out := budget(msgs, map[string]any{"max_tokens": 30})
	if len(out) >= len(msgs) {
		t.Fatalf("budget kept all messages: %d", len(out))
	}
	for _, want := range []string{"permanent instruction", "initial request", "latest question"} {
		if !containsContent(out, want) {
			t.Errorf("lost protected content %q", want)
		}
	}
}

func TestBudgetPreservesToolPair(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "system", "content": "rules"},
		map[string]any{"role": "assistant", "content": "old", "tool_calls": []any{map[string]any{"id": "call_1"}}},
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "tool result"},
		map[string]any{"role": "user", "content": "latest"},
	}
	out := budget(msgs, map[string]any{"max_tokens": 8})
	if !containsRole(out, "tool") || !containsToolCall(out, "call_1") {
		t.Fatalf("tool pair was split: %s", mustJSON(out))
	}
}

func TestBudgetWithoutLimitIsNoOp(t *testing.T) {
	msgs := []any{map[string]any{"role": "user", "content": "same"}}
	out := budget(msgs, nil)
	if len(out) != 1 || mustJSON(out) != mustJSON(msgs) {
		t.Fatalf("no-limit budget changed messages: %s", mustJSON(out))
	}
}

func containsContent(msgs []any, want string) bool {
	for _, msg := range msgs {
		m, ok := msg.(map[string]any)
		if ok && m["content"] == want {
			return true
		}
	}
	return false
}

func containsRole(msgs []any, want string) bool {
	for _, msg := range msgs {
		if role, _ := messageRole(msg); role == want {
			return true
		}
	}
	return false
}

func containsToolCall(msgs []any, want string) bool {
	for _, msg := range msgs {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		calls, ok := m["tool_calls"].([]any)
		if !ok {
			continue
		}
		for _, call := range calls {
			c, _ := call.(map[string]any)
			if c["id"] == want {
				return true
			}
		}
	}
	return false
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
