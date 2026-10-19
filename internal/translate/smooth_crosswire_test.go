package translate

// Cross-wire smoothness: what real clients send must reach a Messages (or
// chat) upstream without a refusal, and what the upstream answers must reach
// the client without a stream kill. The bodies below are the shapes Hermes
// (chat and Responses) and Codex-style Responses clients actually send.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func universal(t *testing.T, body string, client, upstream provider.Surface) map[string]any {
	t.Helper()
	out, err := RequestUniversal([]byte(body), client, upstream, "up/model")
	if err != nil {
		t.Fatalf("RequestUniversal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("not JSON: %v (%s)", err, out)
	}
	return m
}

const hermesChatBody = `{
 "model":"daily","stream":true,"stream_options":{"include_usage":true},
 "reasoning_effort":"medium","prompt_cache_key":"sess-1","safety_identifier":"u1",
 "parallel_tool_calls":false,"response_format":{"type":"text"},
 "messages":[
  {"role":"system","content":[{"type":"text","text":"You are Hermes.","cache_control":{"type":"ephemeral"}}]},
  {"role":"user","content":"list files"},
  {"role":"assistant","content":"","reasoning_content":"hmm","tool_calls":[{"id":"functions.terminal:0","type":"function","function":{"name":"terminal","arguments":"{\"command\":\"ls\"}"}}]},
  {"role":"tool","tool_call_id":"functions.terminal:0","content":"a.txt\nb.txt","cache_control":{"type":"ephemeral"}},
  {"role":"assistant","content":""},
  {"role":"user","content":[{"type":"text","text":"thanks","cache_control":{"type":"ephemeral"}}]}
 ],
 "tools":[
  {"type":"function","function":{"name":"terminal","description":"run","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}},
  {"type":"function","function":{"name":"noargs","description":"x"}}
 ]}`

// A Hermes chat body reaches the Messages wire: an output limit is supplied,
// reasoning_effort and unknown extensions are dropped, tool ids are made
// Messages-legal on both ends, a tool without a schema gets an empty object
// schema, parallel_tool_calls=false is honored, an empty assistant turn is
// dropped, and the client's own cache markers are carried.
func TestChatToMessagesHermesBody(t *testing.T) {
	m := universal(t, hermesChatBody, provider.SurfaceOpenAI, provider.SurfaceAnthropic)

	if m["max_tokens"] != float64(defaultMessagesMaxTokens) {
		t.Errorf("max_tokens = %v, want the default %d", m["max_tokens"], defaultMessagesMaxTokens)
	}
	for _, k := range []string{"reasoning_effort", "output_config", "prompt_cache_key", "safety_identifier", "response_format", "parallel_tool_calls"} {
		if _, has := m[k]; has {
			t.Errorf("%s leaked onto the messages wire", k)
		}
	}
	if tc, _ := m["tool_choice"].(map[string]any); tc == nil || tc["type"] != "auto" || tc["disable_parallel_tool_use"] != true {
		t.Errorf("tool_choice = %v, want auto with disable_parallel_tool_use", m["tool_choice"])
	}
	tools, _ := m["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %v", m["tools"])
	}
	if s, _ := tools[1].(map[string]any)["input_schema"].(map[string]any); s == nil || s["type"] != "object" {
		t.Errorf("schema-less tool input_schema = %v, want an object schema", tools[1].(map[string]any)["input_schema"])
	}

	msgs, _ := m["messages"].([]any)
	var roles []string
	var useID, resultID string
	var marks []string
	for _, x := range msgs {
		mm := x.(map[string]any)
		roles = append(roles, mm["role"].(string))
		blocks, _ := mm["content"].([]any)
		if len(blocks) == 0 {
			t.Errorf("empty %s message reached the messages wire", mm["role"])
		}
		for _, b := range blocks {
			bm := b.(map[string]any)
			switch bm["type"] {
			case "tool_use":
				useID = bm["id"].(string)
			case "tool_result":
				resultID = bm["tool_use_id"].(string)
			}
			if bm["cache_control"] != nil {
				marks = append(marks, bm["type"].(string))
			}
		}
	}
	if strings.Join(roles, ",") != "user,assistant,user" {
		t.Errorf("roles = %v, want user,assistant,user (empty turn dropped, same-role turns merged)", roles)
	}
	if useID != "functions_terminal_0" || resultID != useID {
		t.Errorf("tool ids = %q / %q, want both functions_terminal_0", useID, resultID)
	}
	if strings.Join(marks, ",") != "tool_result,text" {
		t.Errorf("message cache markers on %v, want tool_result,text", marks)
	}
	sys, _ := m["system"].([]any)
	if len(sys) != 1 || sys[0].(map[string]any)["cache_control"] == nil {
		t.Errorf("system = %v, want the client's marker carried", m["system"])
	}
}

// A body with no cache markers at all gets the two standard breakpoints —
// the end of the system prompt and the end of the newest message — so a
// translated conversation is cached like a native one. Placement is a pure
// function of the body: the same body encodes to the same bytes, and the
// next turn's request keeps every earlier block byte-identical apart from
// the moved marker.
func TestChatToMessagesAddsCacheBreakpoints(t *testing.T) {
	turn1 := `{"model":"m","messages":[{"role":"system","content":"sys"},{"role":"user","content":"q1"}]}`
	turn2 := `{"model":"m","messages":[{"role":"system","content":"sys"},{"role":"user","content":"q1"},{"role":"assistant","content":"a1"},{"role":"user","content":"q2"}]}`
	m := universal(t, turn1, provider.SurfaceOpenAI, provider.SurfaceAnthropic)
	if sys := m["system"].([]any); sys[len(sys)-1].(map[string]any)["cache_control"] == nil {
		t.Errorf("no breakpoint on the system prompt: %v", m["system"])
	}
	msgs := m["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)["content"].([]any)
	if last[len(last)-1].(map[string]any)["cache_control"] == nil {
		t.Errorf("no breakpoint on the newest message: %v", msgs)
	}
	a, _ := RequestUniversal([]byte(turn2), provider.SurfaceOpenAI, provider.SurfaceAnthropic, "up/model")
	b, _ := RequestUniversal([]byte(turn2), provider.SurfaceOpenAI, provider.SurfaceAnthropic, "up/model")
	if !bytes.Equal(a, b) {
		t.Fatal("breakpoint placement is not deterministic")
	}
	m2 := universal(t, turn2, provider.SurfaceOpenAI, provider.SurfaceAnthropic)
	marks := 0
	for _, x := range m2["messages"].([]any) {
		for _, bl := range x.(map[string]any)["content"].([]any) {
			if bl.(map[string]any)["cache_control"] != nil {
				marks++
			}
		}
	}
	if marks != 1 {
		t.Errorf("turn 2 carries %d message breakpoints, want exactly 1 (the newest)", marks)
	}
}

// The Messages wire accepts at most four breakpoints. When a client marks
// more, the first (the stable prefix) and the newest three are kept.
func TestChatToMessagesCapsBreakpointsAtFour(t *testing.T) {
	var msgs []string
	for i := 0; i < 6; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, `{"role":"`+role+`","content":[{"type":"text","text":"t`+string(rune('0'+i))+`","cache_control":{"type":"ephemeral"}}]}`)
	}
	m := universal(t, `{"model":"m","messages":[`+strings.Join(msgs, ",")+`]}`, provider.SurfaceOpenAI, provider.SurfaceAnthropic)
	var kept []string
	for _, x := range m["messages"].([]any) {
		for _, bl := range x.(map[string]any)["content"].([]any) {
			if bm := bl.(map[string]any); bm["cache_control"] != nil {
				kept = append(kept, bm["text"].(string))
			}
		}
	}
	if strings.Join(kept, ",") != "t0,t3,t4,t5" {
		t.Errorf("kept breakpoints on %v, want t0,t3,t4,t5", kept)
	}
}

// What the Responses wire carries per turn — include, text verbosity, a
// reasoning summary request, and replayed reasoning items — is not a reason
// to refuse a hop. Reasoning never crosses wires (the legacy path drops
// thinking the same way); stateful history still refuses.
func TestResponsesClientBodyCrossesWires(t *testing.T) {
	body := `{"model":"m","stream":true,"store":false,
	 "include":["reasoning.encrypted_content"],"text":{"verbosity":"medium","format":{"type":"text"}},
	 "reasoning":{"effort":"high","summary":"auto"},"prompt_cache_key":"k","client_metadata":{"x":1},
	 "input":[
	  {"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
	  {"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"gAAA"},
	  {"type":"function_call","call_id":"call_1","name":"ls","arguments":"{}"},
	  {"type":"function_call_output","call_id":"call_1","output":"a.txt"}
	 ]}`
	for _, up := range []provider.Surface{provider.SurfaceOpenAI, provider.SurfaceAnthropic} {
		m := universal(t, body, provider.SurfaceResponses, up)
		raw, _ := json.Marshal(m)
		if bytes.Contains(raw, []byte("gAAA")) || bytes.Contains(raw, []byte("rs_1")) {
			t.Errorf("%s: reasoning item leaked: %s", up, raw)
		}
		if !bytes.Contains(raw, []byte("a.txt")) {
			t.Errorf("%s: tool output lost: %s", up, raw)
		}
	}
	for name, b := range map[string]string{
		"previous_response_id": `{"model":"m","previous_response_id":"resp_1","input":"hi"}`,
		"json text format":     `{"model":"m","text":{"format":{"type":"json_schema","name":"x","schema":{}}},"input":"hi"}`,
	} {
		if _, err := RequestUniversal([]byte(b), provider.SurfaceResponses, provider.SurfaceOpenAI, "m"); err == nil {
			t.Errorf("%s must still be refused", name)
		}
	}
}

// Structured output changes the answer's contract, so a chat body asking for
// JSON still refuses a wire that cannot promise it.
func TestChatResponseFormatJSONStillRefused(t *testing.T) {
	_, err := RequestUniversal([]byte(`{"model":"m","messages":[{"role":"user","content":"x"}],"response_format":{"type":"json_object"}}`),
		provider.SurfaceOpenAI, provider.SurfaceAnthropic, "m")
	if err == nil {
		t.Fatal("response_format json_object reached the messages wire")
	}
}

// Current Claude models always think: a Messages stream opens with a
// thinking block (deltas, a signature) before the answer. The thinking is
// not portable output, but it must not kill the stream; neither may a
// server-tool block or a citations delta the client cannot render.
func TestMessagesStreamWithThinkingReachesChat(t *testing.T) {
	ev := func(name, data string) string { return "event: " + name + "\ndata: " + data + "\n\n" }
	in := ev("message_start", `{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":5}}}`) +
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`) +
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me see"}}`) +
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`) +
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		ev("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{}}}`) +
		ev("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}`) +
		ev("content_block_stop", `{"type":"content_block_stop","index":1}`) +
		ev("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`) +
		ev("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"answer"}}`) +
		ev("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"citations_delta","citation":{}}}`) +
		ev("content_block_stop", `{"type":"content_block_stop","index":2}`) +
		ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`) +
		ev("message_stop", `{"type":"message_stop"}`)
	for _, to := range []provider.Surface{provider.SurfaceOpenAI, provider.SurfaceResponses} {
		r, out := codecStream(t, in, provider.SurfaceAnthropic, to, CodecOptions{MessagesUsage: UsageExclusive})
		if r.Outcome() != FinishCompleted || !bytes.Contains(out, []byte("answer")) {
			t.Fatalf("%s: outcome %q\n%s", to, r.Outcome(), out)
		}
		if bytes.Contains(out, []byte("let me see")) || bytes.Contains(out, []byte("srv_1")) {
			t.Errorf("%s: non-portable block leaked: %s", to, out)
		}
	}
}

// The newer Messages stop reasons are known outcomes, not malformed
// responses: a context-window stop is a length stop, a paused server-tool
// turn is a completed turn. A truly unknown stop reason still fails.
func TestMessagesNewStopReasons(t *testing.T) {
	body := func(stop string) []byte {
		return []byte(`{"id":"msg_1","type":"message","role":"assistant","stop_reason":"` + stop + `","content":[{"type":"thinking","thinking":"","signature":"s"},{"type":"server_tool_use","id":"srv","name":"web_search","input":{}},{"type":"text","text":"x"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}
	for stop, want := range map[string]FinishKind{"model_context_window_exceeded": FinishLength, "pause_turn": FinishCompleted, "refusal": FinishRefusal} {
		ir, err := DecodeResponse(body(stop), provider.SurfaceAnthropic, CodecOptions{MessagesUsage: UsageExclusive})
		if err != nil || ir.Finish != want {
			t.Errorf("%s: finish=%v err=%v; want %v", stop, ir, err, want)
		}
	}
	if _, err := DecodeResponse(body("mystery"), provider.SurfaceAnthropic, CodecOptions{MessagesUsage: UsageExclusive}); err == nil {
		t.Error("an unknown stop reason became success")
	}
	ev := func(name, data string) string { return "event: " + name + "\ndata: " + data + "\n\n" }
	in := ev("message_start", `{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":5}}}`) +
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) +
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`) +
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"model_context_window_exceeded"},"usage":{"output_tokens":9}}`) +
		ev("message_stop", `{"type":"message_stop"}`)
	r, out := codecStream(t, in, provider.SurfaceAnthropic, provider.SurfaceOpenAI, CodecOptions{MessagesUsage: UsageExclusive})
	if r.Outcome() != FinishLength || !bytes.Contains(out, []byte(`"finish_reason":"length"`)) {
		t.Fatalf("outcome %q\n%s", r.Outcome(), out)
	}
}
