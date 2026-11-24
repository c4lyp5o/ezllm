package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/jsonbody"
	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

func turnFor(t *testing.T, body string, surface provider.Surface) string {
	t.Helper()
	return turnOf(jsonbody.Open([]byte(body)), surface, "")
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

// Claude Code sends its tool results with text blocks beside them (system
// reminders). Those are still agent steps, not a person speaking.
func TestTurnKeyIgnoresToolResultsWithText(t *testing.T) {
	ask := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"go"}]}]}`
	step := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"go"}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"r","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"x"},{"type":"text","text":"<system-reminder>todo</system-reminder>"}]}]}`
	if a, b := turnFor(t, ask, provider.SurfaceAnthropic), turnFor(t, step, provider.SurfaceAnthropic); a == "" || a != b {
		t.Fatalf("tool_result with text opened a turn: %q vs %q", a, b)
	}
}

func TestTurnChainScopesAndEvicts(t *testing.T) {
	var c turnChain
	c.remember("my-app", "resp_1", "turnA")
	if got := c.lookup("my-app", "resp_1"); got != "turnA" {
		t.Fatalf("lookup = %q", got)
	}
	if got := c.lookup("other", "resp_1"); got != "" {
		t.Fatalf("another client joined the turn: %q", got)
	}
	for i := 0; i < turnChainSize; i++ {
		c.remember("my-app", fmt.Sprintf("r%d", i), "t")
	}
	if got := c.lookup("my-app", "resp_1"); got != "" {
		t.Fatalf("oldest id survived eviction: %q", got)
	}
	if len(c.turns) != turnChainSize {
		t.Fatalf("map size = %d, want %d", len(c.turns), turnChainSize)
	}
}

// A stateful Responses loop sends previous_response_id and only tool outputs,
// with no human message. Each step joins the turn of the response it
// continues, through both a streamed and a plain reply.
func TestTurnKeyFollowsStatefulResponses(t *testing.T) {
	var n atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		id := fmt.Sprintf("resp_%d", n.Add(1))
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":%q}}\n\n"+
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":%q,\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n", id, id)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":%q,"object":"response","usage":{"input_tokens":3,"output_tokens":1}}`, id)
	})
	steps := []string{
		`{"model":"up/gpt-6-luna","input":[{"role":"user","content":"task one"}]}`,
		`{"model":"up/gpt-6-luna","stream":true,"previous_response_id":"resp_1","input":[{"type":"function_call_output","call_id":"c1","output":"r"}]}`,
		`{"model":"up/gpt-6-luna","previous_response_id":"resp_2","input":[{"type":"function_call_output","call_id":"c2","output":"r"}]}`,
		`{"model":"up/gpt-6-luna","previous_response_id":"resp_unknown","input":[{"type":"function_call_output","call_id":"c3","output":"r"}]}`,
	}
	for _, b := range steps {
		if code, _, body := h.do("POST", "/v1/responses", "tok-infer", b); code != 200 {
			t.Fatalf("responses = %d %s", code, body)
		}
	}
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}
	rows, _ := h.db.RecentCalls(context.Background(), 4) // newest first
	if len(rows) != 4 {
		t.Fatalf("rows = %d", len(rows))
	}
	open := rows[3].TurnID
	if open == "" || rows[2].TurnID != open || rows[1].TurnID != open {
		t.Fatalf("stateful loop split: %q %q %q", open, rows[2].TurnID, rows[1].TurnID)
	}
	if rows[0].TurnID != "" {
		t.Fatalf("unknown previous_response_id got turn %q", rows[0].TurnID)
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

func TestTurnKeySeparatesConversationHistories(t *testing.T) {
	body := func(task string) string {
		return fmt.Sprintf(`{"messages":[{"role":"system","content":"be useful"},{"role":"user","content":%q},{"role":"assistant","content":"done"},{"role":"user","content":"continue"}]}`, task)
	}
	a, b := turnFor(t, body("fix A"), provider.SurfaceChat), turnFor(t, body("fix B"), provider.SurfaceChat)
	if a == "" || a == b {
		t.Fatalf("different histories share turn: %q vs %q", a, b)
	}
	doc := jsonbody.Open([]byte(body("fix A")))
	if turnOf(doc, provider.SurfaceChat, "session-a") == turnOf(doc, provider.SurfaceChat, "session-b") {
		t.Fatal("different sessions share turn")
	}
}

func TestTurnKeySeparatesStatefulHumanTurns(t *testing.T) {
	body := func(previous string) string {
		return fmt.Sprintf(`{"previous_response_id":%q,"input":[{"role":"user","content":"continue"}]}`, previous)
	}
	a, b := turnFor(t, body("resp_a"), provider.SurfaceResponses), turnFor(t, body("resp_b"), provider.SurfaceResponses)
	if a == "" || a == b {
		t.Fatalf("different stateful histories share turn: %q vs %q", a, b)
	}
}

// Claude Code moves its cache breakpoint to the newest message on every
// request: the human message carries cache_control on the first step and loses
// it once tool results follow. The marker must not split the turn.
func TestTurnKeyIgnoresMovingCacheControl(t *testing.T) {
	step1 := `{"messages":[{"role":"user","content":[{"type":"text","text":"fix it","cache_control":{"type":"ephemeral"}}]}]}`
	step2 := `{"messages":[{"role":"user","content":[{"type":"text","text":"fix it"}]},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"x","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok","cache_control":{"type":"ephemeral"}}]}]}`
	a, b := turnFor(t, step1, provider.SurfaceAnthropic), turnFor(t, step2, provider.SurfaceAnthropic)
	if a == "" || a != b {
		t.Fatalf("cache breakpoint split one turn: %q vs %q", a, b)
	}
	next := `{"messages":[{"role":"user","content":[{"type":"text","text":"fix it"}]},{"role":"assistant","content":"done"},{"role":"user","content":[{"type":"text","text":"now B","cache_control":{"type":"ephemeral"}}]}]}`
	if c := turnFor(t, next, provider.SurfaceAnthropic); c == a {
		t.Fatal("next human message kept the old turn")
	}
}
