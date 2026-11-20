package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/jsonbody"
	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

func turnFor(t *testing.T, body string, surface provider.Surface) string {
	t.Helper()
	return turnOf(jsonbody.Open([]byte(body)), surface)
}

// One agent loop keeps its key while tool calls and results pile up; the next
// message a person sends starts a new key, even with the same words.
func TestTurnKeyFollowsTheAgentLoop(t *testing.T) {
	const sys = `{"role":"system","content":"be useful"}`
	ask := `{"model":"m","messages":[` + sys + `,{"role":"user","content":"fix the bug"}]}`
	step := `{"model":"m","messages":[` + sys + `,{"role":"user","content":"fix the bug"},` +
		`{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"file body"}]}`
	next := `{"model":"m","messages":[` + sys + `,{"role":"user","content":"fix the bug"},` +
		`{"role":"assistant","content":"done"},{"role":"user","content":"fix the bug"}]}`
	a, b, c := turnFor(t, ask, provider.SurfaceChat), turnFor(t, step, provider.SurfaceChat), turnFor(t, next, provider.SurfaceChat)
	if a == "" || a != b {
		t.Fatalf("loop split: ask %q, tool step %q", a, b)
	}
	if c == a {
		t.Fatal("a new human message kept the old turn key")
	}
}

// Anthropic tool results arrive as user messages made of tool_result blocks:
// an agent step, not a person.
func TestTurnKeyIgnoresAnthropicToolResults(t *testing.T) {
	ask := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"go"}]}]}`
	step := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"go"}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"ls","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"a b"}]}]}`
	if a, b := turnFor(t, ask, provider.SurfaceAnthropic), turnFor(t, step, provider.SurfaceAnthropic); a == "" || a != b {
		t.Fatalf("tool_result opened a turn: %q vs %q", a, b)
	}
}

func TestTurnKeyResponsesAndOneShots(t *testing.T) {
	ask := `{"model":"m","input":[{"role":"user","content":"hi"}]}`
	step := `{"model":"m","input":[{"role":"user","content":"hi"},{"type":"function_call","call_id":"x","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"x","output":"ok"}]}`
	if a, b := turnFor(t, ask, provider.SurfaceResponses), turnFor(t, step, provider.SurfaceResponses); a == "" || a != b {
		t.Fatalf("responses loop split: %q vs %q", a, b)
	}
	for _, body := range []string{`{"model":"m","input":"one shot"}`, `{"model":"m","prompt":"x"}`, `not json`} {
		if k := turnFor(t, body, provider.SurfaceResponses); k != "" {
			t.Fatalf("%s: key %q, want none", body, k)
		}
	}
}

// End to end: the dispatcher records the key, and /admin/turns groups it.
func TestTurnsEndpointCountsRequestsPerTurn(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","object":"chat.completion","model":"gpt-6-luna","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	})
	loop := []string{
		`{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"task one"}]}`,
		`{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"task one"},{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c","content":"r"}]}`,
		`{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"task one"},{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c","content":"r"},{"role":"assistant","tool_calls":[{"id":"d","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"d","content":"r"}]}`,
		`{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"task two"}]}`,
	}
	for _, b := range loop {
		if code, _, body := h.do("POST", "/v1/chat/completions", "tok-infer", b); code != 200 {
			t.Fatalf("chat = %d %s", code, body)
		}
	}
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}
	rows, _ := h.db.RecentCalls(context.Background(), 4)
	if len(rows) != 4 || rows[3].TurnID == "" || rows[3].TurnID != rows[1].TurnID || rows[0].TurnID == rows[1].TurnID {
		t.Fatalf("turn ids = %v", []string{rows[3].TurnID, rows[2].TurnID, rows[1].TurnID, rows[0].TurnID})
	}
	code, _, body := h.do("GET", "/admin/turns", "tok-admin", "")
	if code != 200 {
		t.Fatalf("/admin/turns = %d %s", code, body)
	}
	var out struct {
		LongTurn int                `json:"long_turn"`
		Rows     []store.TurnLength `json:"rows"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || len(out.Rows) != 1 {
		t.Fatalf("decode %v: %s", err, body)
	}
	r := out.Rows[0]
	if r.Turns != 2 || r.Requests != 4 || r.Max != 3 || r.Median != 2 || r.Single != 0.5 || out.LongTurn != store.LongTurn {
		t.Fatalf("row = %+v", r)
	}
	if code, _, _ := h.do("GET", "/admin/turns", "tok-infer", ""); code != http.StatusForbidden && code != http.StatusUnauthorized {
		t.Fatalf("inference token read /admin/turns: %d", code)
	}
	if code, _, body := h.do("GET", "/admin/turns?from="+time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "tok-admin", ""); code != 400 || !strings.Contains(body, "after") {
		t.Fatalf("bad window = %d %s", code, body)
	}
}
