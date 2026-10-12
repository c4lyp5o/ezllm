package translate

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func runResponsesSSE(t *testing.T, stream, clientModel string) []byte {
	t.Helper()
	r := NewResponsesSSEReader(strings.NewReader(stream), clientModel)
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("reading translated stream: %v", err)
	}
	return buf.Bytes()
}

// ── request: chat → responses ──────────────────────────────────────────────

func TestChatToResponsesGolden(t *testing.T) {
	got, err := ChatToResponses(fixture(t, "chat_request.json"), "step-5-upstream")
	if err != nil {
		t.Fatalf("ChatToResponses: %v", err)
	}
	want := fixture(t, "responses_request_golden.json")
	if !bytes.Equal(got, want) {
		t.Errorf("golden mismatch\nwant: %s\ngot:  %s", want, got)
	}
}

func TestChatToResponsesDeterminism(t *testing.T) {
	in := fixture(t, "chat_request.json")
	first, err := ChatToResponses(in, "step-5-upstream")
	if err != nil {
		t.Fatal(err)
	}
	second, err := ChatToResponses(in, "step-5-upstream")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("non-deterministic\nfirst:  %s\nsecond: %s", first, second)
	}
	if orig := fixture(t, "chat_request.json"); !bytes.Equal(in, orig) {
		t.Error("ChatToResponses mutated its input slice")
	}
}

func TestChatToResponsesTable(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		check func(t *testing.T, out map[string]any)
	}{
		{
			name: "reasoning_effort maps to reasoning object",
			in:   `{"model":"m","messages":[],"max_tokens":8,"reasoning_effort":"medium"}`,
			check: func(t *testing.T, out map[string]any) {
				r, ok := out["reasoning"].(map[string]any)
				if !ok || r["effort"] != "medium" {
					t.Errorf("reasoning = %v", out["reasoning"])
				}
			},
		},
		{
			name: "stop and stream_options are dropped",
			in:   `{"model":"m","messages":[],"max_tokens":8,"stop":["X"],"stream_options":{"include_usage":true}}`,
			check: func(t *testing.T, out map[string]any) {
				if _, ok := out["stop"]; ok {
					t.Error("stop leaked into responses body")
				}
				if _, ok := out["stream_options"]; ok {
					t.Error("stream_options leaked into responses body")
				}
			},
		},
		{
			name: "stream true passes through, false omitted",
			in:   `{"model":"m","messages":[],"max_tokens":8,"stream":true}`,
			check: func(t *testing.T, out map[string]any) {
				if out["stream"] != true {
					t.Errorf("stream = %v", out["stream"])
				}
			},
		},
		{
			name: "named tool_choice flattens",
			in:   `{"model":"m","messages":[],"max_tokens":8,"tool_choice":{"type":"function","function":{"name":"f"}}}`,
			check: func(t *testing.T, out map[string]any) {
				tc, ok := out["tool_choice"].(map[string]any)
				if !ok || tc["type"] != "function" || tc["name"] != "f" {
					t.Errorf("tool_choice = %v", out["tool_choice"])
				}
			},
		},
		{
			name: "second system message is dropped",
			in:   `{"model":"m","max_tokens":8,"messages":[{"role":"system","content":"first"},{"role":"system","content":"second"}]}`,
			check: func(t *testing.T, out map[string]any) {
				if out["instructions"] != "first" {
					t.Errorf("instructions = %v", out["instructions"])
				}
				s := string(mustJSON(t, out))
				if strings.Contains(s, "second") {
					t.Error("second system message leaked")
				}
			},
		},
		{
			name: "chat content blocks flatten to text",
			in:   `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"data:x"}},{"type":"text","text":"b"}]}]}`,
			check: func(t *testing.T, out map[string]any) {
				input := out["input"].([]any)
				if len(input) != 1 {
					t.Fatalf("input = %v", input)
				}
				if input[0].(map[string]any)["content"] != "a\nb" {
					t.Errorf("content = %v", input[0])
				}
			},
		},
		{
			name: "assistant null content emits no text item",
			in:   `{"model":"m","max_tokens":8,"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`,
			check: func(t *testing.T, out map[string]any) {
				input := out["input"].([]any)
				if len(input) != 1 {
					t.Fatalf("input = %v", input)
				}
				item := input[0].(map[string]any)
				if item["type"] != "function_call" || item["call_id"] != "c" {
					t.Errorf("item = %v", item)
				}
			},
		},
		{
			name: "parallel_tool_calls passes through",
			in:   `{"model":"m","messages":[],"max_tokens":8,"parallel_tool_calls":false}`,
			check: func(t *testing.T, out map[string]any) {
				if v, ok := out["parallel_tool_calls"].(bool); !ok || v {
					t.Errorf("parallel_tool_calls = %v", out["parallel_tool_calls"])
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := ChatToResponses([]byte(tc.in), "upstream")
			if err != nil {
				t.Fatalf("ChatToResponses: %v", err)
			}
			var out map[string]any
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatalf("output not JSON: %v\n%s", err, raw)
			}
			tc.check(t, out)
		})
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ── response: responses → chat ─────────────────────────────────────────────

func TestResponsesToChatGolden(t *testing.T) {
	got, err := ResponsesToChat(fixture(t, "responses_response.json"), "daily")
	if err != nil {
		t.Fatalf("ResponsesToChat: %v", err)
	}
	want := fixture(t, "chat_response_golden.json")
	if !bytes.Equal(got, want) {
		t.Errorf("golden mismatch\nwant: %s\ngot:  %s", want, got)
	}
}

func TestResponsesToChatTable(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		check func(t *testing.T, out map[string]any)
	}{
		{
			name: "text-only completion ends stop",
			in:   `{"id":"r","created_at":5,"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":1,"output_tokens":2}}`,
			check: func(t *testing.T, out map[string]any) {
				choice := out["choices"].([]any)[0].(map[string]any)
				if choice["finish_reason"] != "stop" {
					t.Errorf("finish_reason = %v", choice["finish_reason"])
				}
				msg := choice["message"].(map[string]any)
				if msg["content"] != "hi" || msg["tool_calls"] != nil {
					t.Errorf("message = %v", msg)
				}
				if out["object"] != "chat.completion" || out["created"] != float64(5) {
					t.Errorf("envelope = %v %v", out["object"], out["created"])
				}
			},
		},
		{
			name: "incomplete maps to length",
			in:   `{"id":"r","created_at":5,"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","content":[{"type":"output_text","text":"cut"}]}],"usage":{}}`,
			check: func(t *testing.T, out map[string]any) {
				choice := out["choices"].([]any)[0].(map[string]any)
				if choice["finish_reason"] != "length" {
					t.Errorf("finish_reason = %v", choice["finish_reason"])
				}
			},
		},
		{
			name: "missing usage decodes as zeros",
			in:   `{"id":"r","created_at":5,"status":"completed","output":[],"usage":{}}`,
			check: func(t *testing.T, out map[string]any) {
				u := out["usage"].(map[string]any)
				if u["prompt_tokens"] != float64(0) || u["completion_tokens"] != float64(0) {
					t.Errorf("usage = %v", u)
				}
				choice := out["choices"].([]any)[0].(map[string]any)
				msg := choice["message"].(map[string]any)
				if _, has := msg["content"]; has {
					t.Errorf("empty output must omit content: %v", msg)
				}
			},
		},
		{
			name: "multiple output_text items join with newline",
			in:   `{"id":"r","created_at":5,"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"a"}]},{"type":"message","content":[{"type":"output_text","text":"b"}]}],"usage":{}}`,
			check: func(t *testing.T, out map[string]any) {
				choice := out["choices"].([]any)[0].(map[string]any)
				if got := choice["message"].(map[string]any)["content"]; got != "a\nb" {
					t.Errorf("content = %v", got)
				}
			},
		},
		{
			name: "reasoning items are dropped",
			in:   `{"id":"r","created_at":5,"status":"completed","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"secret thoughts"}]},{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{}}`,
			check: func(t *testing.T, out map[string]any) {
				raw := string(mustJSON(t, out))
				if strings.Contains(raw, "secret thoughts") {
					t.Errorf("reasoning leaked: %s", raw)
				}
				choice := out["choices"].([]any)[0].(map[string]any)
				if choice["message"].(map[string]any)["content"] != "ok" {
					t.Errorf("content = %v", choice["message"])
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := ResponsesToChat([]byte(tc.in), "client")
			if err != nil {
				t.Fatalf("ResponsesToChat: %v", err)
			}
			var out map[string]any
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatalf("output not JSON: %v\n%s", err, raw)
			}
			tc.check(t, out)
		})
	}
}

func TestResponsesToChatDeterminism(t *testing.T) {
	in := fixture(t, "responses_response.json")
	first, err := ResponsesToChat(in, "daily")
	if err != nil {
		t.Fatal(err)
	}
	second, err := ResponsesToChat(in, "daily")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("non-deterministic\nfirst:  %s\nsecond: %s", first, second)
	}
	if orig := fixture(t, "responses_response.json"); !bytes.Equal(in, orig) {
		t.Error("ResponsesToChat mutated its input slice")
	}
}

// ── SSE: responses events → chat chunks ────────────────────────────────────

func TestResponsesSSEGolden(t *testing.T) {
	got := runResponsesSSE(t, string(fixture(t, "responses_sse_stream.txt")), "daily")
	want := fixture(t, "chat_sse_golden.txt")
	if !bytes.Equal(got, want) {
		t.Errorf("golden mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestResponsesSSEDiscipline(t *testing.T) {
	out := string(runResponsesSSE(t, string(fixture(t, "responses_sse_stream.txt")), "daily"))
	if strings.Contains(out, "event: ") {
		t.Errorf("chat wire must carry no event lines:\n%s", out)
	}
	frames := 0
	for block := range strings.SplitSeq(out, "\n\n") {
		line := strings.TrimPrefix(block, "data: ")
		if strings.TrimSpace(line) == "" {
			continue
		}
		frames++
		if line == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			t.Errorf("frame not JSON: %v (%s)", err, line)
			continue
		}
		if chunk["object"] != "chat.completion.chunk" {
			t.Errorf("object = %v in %s", chunk["object"], line)
		}
	}
	if frames != 9 {
		t.Errorf("frames = %d, want 9", frames)
	}
}

func TestResponsesSSEFailedEmitsErrorFrame(t *testing.T) {
	in := `event: response.created
data: {"type":"response.created","response":{"id":"r1","created_at":1}}

event: response.failed
data: {"type":"response.failed","response":{"error":{"message":"model overloaded","type":"server_error"}}}

`
	out := string(runResponsesSSE(t, in, "daily"))
	if !strings.Contains(out, `"error":{"message":"model overloaded"`) {
		t.Errorf("error frame missing:\n%s", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("stream must end with [DONE]:\n%s", out)
	}
}

// A cut stream must still terminate (never hang the client), but it must not
// claim success. Until M0 this synthesized finish_reason "stop" for a stream
// that never sent a terminal event — handing the client a truncated answer
// marked complete and marking the hop healthy.
func TestResponsesSSECutWithoutCompletedReportsTruncation(t *testing.T) {
	in := `event: response.created
data: {"type":"response.created","response":{"id":"r1","created_at":1}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"half"}

`
	out := string(runResponsesSSE(t, in, "daily"))
	if strings.Contains(out, `"finish_reason":"stop"`) || strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Errorf("cut stream must not synthesize a success finish:\n%s", out)
	}
	if !strings.Contains(out, "without a terminal event") {
		t.Errorf("cut stream must report truncation:\n%s", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("stream must end with [DONE]:\n%s", out)
	}
}

func TestResponsesSSETwoToolCallsIndexSeparately(t *testing.T) {
	in := `event: response.created
data: {"type":"response.created","response":{"id":"r1","created_at":1}}

event: response.output_item.added
data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"c1","name":"f1"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"c1","delta":"{}"}

event: response.output_item.added
data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"c2","name":"f2"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"c2","delta":"{}"}

event: response.completed
data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2}}}

`
	out := string(runResponsesSSE(t, in, "daily"))
	if !strings.Contains(out, `"tool_calls":[{"index":0,"id":"c1"`) {
		t.Errorf("call 0 missing:\n%s", out)
	}
	if !strings.Contains(out, `"tool_calls":[{"index":1,"id":"c2"`) {
		t.Errorf("call 1 missing:\n%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Errorf("finish must be tool_calls:\n%s", out)
	}
}

func TestResponsesSSEUnknownEventsSkipped(t *testing.T) {
	in := `event: response.created
data: {"type":"response.created","response":{"id":"r1","created_at":1}}

event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","delta":"thinking..."}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"ok"}

event: response.completed
data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}

`
	out := string(runResponsesSSE(t, in, "daily"))
	if strings.Contains(out, "thinking...") {
		t.Errorf("reasoning summary leaked:\n%s", out)
	}
	if !strings.Contains(out, `"content":"ok"`) {
		t.Errorf("text delta lost:\n%s", out)
	}
}

func TestResponsesSSEEmptyStream(t *testing.T) {
	out := string(runResponsesSSE(t, "", "daily"))
	// No upstream events at all: an honest empty close, not a hang or garbage.
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("empty stream must close with [DONE]:\n%s", out)
	}
}
