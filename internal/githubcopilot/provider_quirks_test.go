package githubcopilot

import "testing"

// The stealth rule: the client's persona and personal context must never reach
// a web provider. It is provider-scoped, not model-scoped.

func TestPrepareBodyDropsSystemMessages(t *testing.T) {
	body := map[string]any{
		"model": "gpt-5.5",
		"messages": []any{
			map[string]any{"role": "system", "content": "SOUL.md: you are Melissa"},
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "system", "content": "MEMORY.md: he loves you"},
			map[string]any{"role": "assistant", "content": "hello"},
			map[string]any{"role": "user", "content": "again"},
		},
	}
	PrepareBody("gpt-5.5", body)
	msgs, ok := body["messages"].([]any)
	if !ok {
		t.Fatal("messages missing")
	}
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3 (both system turns dropped): %+v", len(msgs), msgs)
	}
	for i, mm := range msgs {
		if msg, ok := mm.(map[string]any); ok && msg["role"] == "system" {
			t.Fatalf("message %d still carries role=system: %+v", i, msg)
		}
	}
	// Non-system turns survive, in order.
	want := []string{"user", "assistant", "user"}
	for i, role := range want {
		if got := msgs[i].(map[string]any)["role"]; got != role {
			t.Errorf("message %d role = %v, want %s", i, got, role)
		}
	}
}

// A request whose only content was persona is left with an empty list: the
// upstream's own "messages is empty" error is the honest answer. Sending the
// persona anyway would defeat the rule in exactly the case it matters most.
func TestPrepareBodyDropsSystemEvenWhenItEmptiesTheList(t *testing.T) {
	body := map[string]any{
		"model":    "gpt-5.5",
		"messages": []any{map[string]any{"role": "system", "content": "SOUL.md"}},
	}
	PrepareBody("gpt-5.5", body)
	if got, _ := body["messages"].([]any); len(got) != 0 {
		t.Fatalf("messages = %+v, want empty (persona must not be sent)", got)
	}
}

// claude models skip the /chat/completions-only quirks, but the privacy rule is
// not a quirk — it applies to every model on this provider.
func TestPrepareBodyDropsSystemOnClaudeModels(t *testing.T) {
	body := map[string]any{
		"model": "claude-sonnet-5",
		"messages": []any{
			map[string]any{"role": "system", "content": "persona"},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	PrepareBody("claude-sonnet-5", body)
	if got, _ := body["messages"].([]any); len(got) != 1 {
		t.Fatalf("messages = %+v, want only the user turn", got)
	}
}

// Copilot serves claude* on /v1/messages and gemini* on /chat/completions;
// neither answers on /responses (400 unsupported_api_for_model).
func TestResponsesUnsupported(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  bool
	}{
		{"claude-sonnet-5", true},
		{"CLAUDE-OPUS-4.6", true},
		{"claude-haiku-4.5", true},
		{"gemini-3.5-flash", true},
		{"Gemini-3.7-Flash", true},
		{"gpt-5.6-luna", false},
		{"gpt-5.3-codex", false},
		{"deepseek-v4.1-flash", false},
		{"", false},
	} {
		if got := ResponsesUnsupported(tc.model); got != tc.want {
			t.Errorf("ResponsesUnsupported(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}
