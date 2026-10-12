package translate

// The anthropic→responses leg (Claude Code ↔ a responses-only hop):
// request mapping, response mapping, and the SSE stream. The hop this exists
// for is tokenrouter/openai/gpt-6.1-sol, whose chat path refuses
// tools+reasoning and which has no /v1/messages — verified live 2026-10-12.

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestAnthropicToResponsesRequest(t *testing.T) {
	in := `{
	  "model": "daily",
	  "max_tokens": 4096,
	  "temperature": 0.2,
	  "stream": true,
	  "system": [{"type":"text","text":"You are Claude Code."},{"type":"text","text":"Be terse."}],
	  "tools": [{"name":"Bash","description":"run","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}}],
	  "tool_choice": {"type":"auto"},
	  "messages": [
	    {"role":"user","content":"list the files"},
	    {"role":"assistant","content":[
	      {"type":"thinking","thinking":"hidden"},
	      {"type":"text","text":"on it"},
	      {"type":"tool_use","id":"toolu_1","name":"Bash","input":{"cmd":"ls"}}]},
	    {"role":"user","content":[
	      {"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"a.txt"}]},
	      {"type":"image","source":{"type":"base64","data":"x"}},
	      {"type":"text","text":"and now?"}]}
	  ]
	}`
	out, err := AnthropicToResponses([]byte(in), "super-ssn/gpt-6.1-sol")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Model           string `json:"model"`
		Instructions    string `json:"instructions"`
		MaxOutputTokens int    `json:"max_output_tokens"`
		Stream          bool   `json:"stream"`
		ToolChoice      string `json:"tool_choice"`
		Input           []struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Output    string `json:"output"`
		} `json:"input"`
		Tools []struct {
			Type       string          `json:"type"`
			Name       string          `json:"name"`
			Parameters json.RawMessage `json:"parameters"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "super-ssn/gpt-6.1-sol" || got.MaxOutputTokens != 4096 || !got.Stream {
		t.Fatalf("envelope = %+v", got)
	}
	if got.Instructions != "You are Claude Code.\nBe terse." {
		t.Fatalf("instructions = %q (system blocks must flatten)", got.Instructions)
	}
	if len(got.Tools) != 1 || got.Tools[0].Type != "function" || got.Tools[0].Name != "Bash" ||
		!strings.Contains(string(got.Tools[0].Parameters), `"cmd"`) {
		t.Fatalf("tools = %+v", got.Tools)
	}
	if got.ToolChoice != "auto" {
		t.Fatalf("tool_choice = %q, want auto", got.ToolChoice)
	}
	// Order: user text, assistant text, function_call, function_call_output, trailing user text.
	want := []struct{ kind, val string }{
		{"text", "list the files"},
		{"text", "on it"},
		{"function_call", "toolu_1"},
		{"function_call_output", "a.txt"},
		{"text", "and now?"},
	}
	if len(got.Input) != len(want) {
		t.Fatalf("input items = %d (%+v), want %d — thinking/image must be dropped, order preserved",
			len(got.Input), got.Input, len(want))
	}
	for i, w := range want {
		item := got.Input[i]
		switch w.kind {
		case "text":
			if item.Role == "" || item.Content != w.val {
				t.Fatalf("item %d = %+v, want text %q", i, item, w.val)
			}
		case "function_call":
			if item.Type != "function_call" || item.CallID != w.val || item.Name != "Bash" || item.Arguments != `{"cmd":"ls"}` {
				t.Fatalf("item %d = %+v, want function_call %q", i, item, w.val)
			}
		case "function_call_output":
			if item.Type != "function_call_output" || item.Output != w.val || item.CallID != "toolu_1" {
				t.Fatalf("item %d = %+v, want function_call_output %q", i, item, w.val)
			}
		}
	}
}

func TestResponsesToAnthropicResponse(t *testing.T) {
	in := `{
	  "id":"resp_abc","model":"gpt-6.1-sol","status":"completed",
	  "output":[
	    {"type":"reasoning","summary":[]},
	    {"type":"message","role":"assistant","content":[{"type":"output_text","text":"working on it"}]},
	    {"type":"function_call","call_id":"call_9","name":"Bash","arguments":"{\"cmd\":\"ls -la\"}"}
	  ],
	  "usage":{"input_tokens":1234,"output_tokens":56}
	}`
	out, err := ResponsesToAnthropic([]byte(in), "daily")
	if err != nil {
		t.Fatal(err)
	}
	var got AnthropicResponse
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "message" || got.Role != "assistant" || got.Model != "daily" {
		t.Fatalf("envelope = %+v", got)
	}
	if !strings.HasPrefix(got.ID, "msg_") {
		t.Fatalf("id = %q, want a msg_ prefix", got.ID)
	}
	if got.StopReason != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", got.StopReason)
	}
	if got.Usage.InputTokens != 1234 || got.Usage.OutputTokens != 56 {
		t.Fatalf("usage = %+v", got.Usage)
	}
	if len(got.Content) != 2 {
		t.Fatalf("content = %+v (reasoning must be dropped)", got.Content)
	}
	if got.Content[0].Type != "text" || got.Content[0].Text != "working on it" {
		t.Fatalf("block 0 = %+v", got.Content[0])
	}
	block := got.Content[1]
	if block.Type != "tool_use" || block.ID != "call_9" || block.Name != "Bash" {
		t.Fatalf("block 1 = %+v", block)
	}
	input, ok := block.Input.(map[string]any)
	if !ok || input["cmd"] != "ls -la" {
		t.Fatalf("tool input = %#v, want the parsed arguments object", block.Input)
	}
}

func TestResponsesAnthropicSSEStreamsAnthropicFrames(t *testing.T) {
	in := `event: response.created
data: {"type":"response.created","response":{"id":"resp_x","created_at":1}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"Hel"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"lo"}

event: response.output_item.added
data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_1","name":"Bash"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"call_1","delta":"{\"cmd\""}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"call_1","delta":":\"ls\"}"}

event: response.completed
data: {"type":"response.completed","response":{"usage":{"input_tokens":42,"output_tokens":7}}}

`
	out, _ := io.ReadAll(NewResponsesAnthropicSSEReader(strings.NewReader(in), "daily"))
	events := parseAnthropicFrames(t, string(out))
	want := []string{
		"ping", "message_start",
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop",
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop",
	}
	if len(events) != len(want) {
		t.Fatalf("frames = %v (%d), want %d", events, len(events), len(want))
	}
	for i, w := range want {
		if events[i].event != w {
			t.Fatalf("frame %d = %s, want %s (all: %v)", i, events[i].event, w, events)
		}
	}
	if !strings.Contains(events[1].data, `"model":"daily"`) || !strings.Contains(events[1].data, `"id":"msg_resp_x"`) {
		t.Fatalf("message_start = %s", events[1].data)
	}
	if !strings.Contains(events[2].data, `"type":"text"`) {
		t.Fatalf("text block start = %s", events[2].data)
	}
	if !strings.Contains(events[6].data, `"type":"tool_use"`) || !strings.Contains(events[6].data, `"name":"Bash"`) {
		t.Fatalf("tool block start = %s", events[6].data)
	}
	if !strings.Contains(events[7].data, `"input_json_delta"`) || !strings.Contains(events[7].data, `partial_json`) {
		t.Fatalf("tool delta = %s", events[7].data)
	}
	last := events[len(events)-2]
	if !strings.Contains(last.data, `"stop_reason":"tool_use"`) ||
		!strings.Contains(last.data, `"input_tokens":42`) || !strings.Contains(last.data, `"output_tokens":7`) {
		t.Fatalf("message_delta = %s, want stop_reason + real usage", last.data)
	}
}

// A transport cut before response.completed must NOT be reported as a
// successful turn. Until M0 this emitted stop_reason "end_turn" (and a
// message_stop), which told the client — and the hop-health hook — that a
// cut-off answer had completed. It now closes with an explicit error event
// while still ending the stream so the client cannot hang.
func TestResponsesAnthropicSSECutWithoutCompletedReportsTruncation(t *testing.T) {
	in := `event: response.created
data: {"type":"response.created","response":{"id":"resp_y"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"partial"}

`
	out, _ := io.ReadAll(NewResponsesAnthropicSSEReader(strings.NewReader(in), "daily"))
	events := parseAnthropicFrames(t, string(out))
	if len(events) == 0 || events[len(events)-1].event != "error" {
		t.Fatalf("cut stream must end with an error event: %v", events)
	}
	if !strings.Contains(events[len(events)-1].data, "without a terminal event") {
		t.Fatalf("truncation error = %s", events[len(events)-1].data)
	}
	for _, ev := range events {
		if strings.Contains(ev.data, `"stop_reason":"end_turn"`) {
			t.Fatalf("truncated stream must not report end_turn: %s", ev.data)
		}
	}
}

type anthropicFrame struct{ event, data string }

// parseAnthropicFrames splits an SSE body and asserts the discipline every
// strict Anthropic client relies on: each data payload is JSON and carries a
// type matching its event name.
func parseAnthropicFrames(t *testing.T, body string) []anthropicFrame {
	t.Helper()
	var out []anthropicFrame
	for _, block := range strings.Split(strings.TrimSpace(body), "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var f anthropicFrame
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				f.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				f.data = strings.TrimPrefix(line, "data: ")
			default:
				t.Fatalf("malformed SSE line %q in %q", line, block)
			}
		}
		if f.event == "" || f.data == "" {
			t.Fatalf("incomplete frame: %+v", f)
		}
		var payload struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(f.data), &payload); err != nil {
			t.Fatalf("frame %s: data is not JSON: %v (%s)", f.event, err, f.data)
		}
		if payload.Type != f.event {
			t.Fatalf("frame %s carries type %q", f.event, payload.Type)
		}
		out = append(out, f)
	}
	return out
}
