package githubcopilot

import (
	"encoding/json"
	"strings"
	"testing"
)

// The stealth rule: the client's persona and personal context must never reach
// a web provider. It is provider-scoped, not model-scoped. It drops the
// personal CONTENT, not the role: a neutral system prompt takes its place so
// the model still gets operating instructions.

// persona is every personal string the fixtures carry; none may survive.
var persona = []string{"SOUL.md", "MEMORY.md", "Melissa", "loves you", "persona"}

func assertNoPersona(t *testing.T, body map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	for _, p := range persona {
		if strings.Contains(string(b), p) {
			t.Fatalf("persona %q reached the body: %s", p, b)
		}
	}
}

func TestPrepareBodyReplacesSystemMessages(t *testing.T) {
	body := map[string]any{
		"model": "gpt-5.5",
		"messages": []any{
			map[string]any{"role": "system", "content": "SOUL.md: you are Melissa"},
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "developer", "content": "MEMORY.md: he loves you"},
			map[string]any{"role": "assistant", "content": "hello"},
			map[string]any{"role": "user", "content": "again"},
		},
	}
	PrepareBody("gpt-5.5", body)
	assertNoPersona(t, body)
	msgs, _ := body["messages"].([]any)
	// One neutral system turn in the first one's place; the rest in order.
	want := []string{"system", "user", "assistant", "user"}
	if len(msgs) != len(want) {
		t.Fatalf("messages = %+v, want roles %v", msgs, want)
	}
	for i, role := range want {
		if got := msgs[i].(map[string]any)["role"]; got != role {
			t.Errorf("message %d role = %v, want %s", i, got, role)
		}
	}
	if got := msgs[0].(map[string]any)["content"]; got != neutralSystemPrompt {
		t.Errorf("system content = %q, want the neutral prompt", got)
	}
}

// A request with tools gets the tool-use line, so a model that lost the
// client's tool instructions still knows how to call them.
func TestStripPersonaAddsToolLineWhenToolsArePresent(t *testing.T) {
	body := map[string]any{
		"messages": []any{map[string]any{"role": "system", "content": "SOUL.md"}},
		"tools":    []any{map[string]any{"type": "function"}},
	}
	StripPersona(body)
	if got := body["messages"].([]any)[0].(map[string]any)["content"]; got != neutralSystemPrompt+neutralToolsLine {
		t.Errorf("system content = %q, want the neutral prompt with the tools line", got)
	}
}

// A request that sent no system content gets none added, and reports no change
// so the caller can keep the body byte-identical.
func TestStripPersonaLeavesARequestWithoutSystemAlone(t *testing.T) {
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}, "input": "hi"}
	if StripPersona(body) {
		t.Fatal("StripPersona reported a change on a body with no system content")
	}
	if msgs := body["messages"].([]any); len(msgs) != 1 {
		t.Fatalf("messages = %+v, want untouched", msgs)
	}
	if _, ok := body["instructions"]; ok {
		t.Fatal("instructions added to a body that sent none")
	}
}

// Messages wire (claude* on Copilot): the system prompt is the top-level
// field, string or block array.
func TestStripPersonaReplacesAnthropicSystemField(t *testing.T) {
	for _, sys := range []any{
		"SOUL.md: you are Melissa",
		[]any{map[string]any{"type": "text", "text": "MEMORY.md", "cache_control": map[string]any{"type": "ephemeral"}}},
	} {
		body := map[string]any{"model": "claude-sonnet-5", "system": sys,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
		PrepareBody("claude-sonnet-5", body)
		assertNoPersona(t, body)
		if body["system"] != neutralSystemPrompt {
			t.Errorf("system = %v, want the neutral prompt", body["system"])
		}
	}
}

// Responses wire: instructions, and system/developer items in input.
func TestStripPersonaReplacesResponsesInstructionsAndInput(t *testing.T) {
	body := map[string]any{
		"instructions": "SOUL.md: you are Melissa",
		"input": []any{
			map[string]any{"role": "developer", "content": "MEMORY.md"},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	if !StripPersona(body) {
		t.Fatal("StripPersona reported no change")
	}
	assertNoPersona(t, body)
	if body["instructions"] != neutralSystemPrompt {
		t.Errorf("instructions = %v, want the neutral prompt", body["instructions"])
	}
	if items := body["input"].([]any); len(items) != 1 || items[0].(map[string]any)["role"] != "user" {
		t.Errorf("input = %+v, want only the user item", items)
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
