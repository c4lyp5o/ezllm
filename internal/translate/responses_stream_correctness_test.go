package translate

// M0 — stream correctness contracts for the Responses->(chat|anthropic) SSE
// readers. The earlier fixtures used the SAME string for item.id and call_id,
// so a wrong state-map key could never fail, and both readers silently dropped
// terminal usage on response.incomplete.
//
// These assertions must FAIL against the pre-M0 readers (RED), then pass after
// the fix. Nothing here talks to a provider.

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// One source stream with DISTINCT item ids and call ids, plus a text preamble.
// item.id is what argument deltas reference; call_id is what the client-visible
// tool id must be.
func toolStream(two bool) string {
	s := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1","created_at":1760000000}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_item_a","type":"function_call","call_id":"call_public_a","name":"read"}}` + "\n\n" +
		"event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_item_a","output_index":0,"delta":"{\"path\":\"/a\"}"}` + "\n\n"
	if two {
		s += "event: response.output_item.added\n" +
			`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_item_b","type":"function_call","call_id":"call_public_b","name":"write"}}` + "\n\n" +
			// Interleaved: B's fragment arrives while A is still open.
			"event: response.function_call_arguments.delta\n" +
			`data: {"type":"response.function_call_arguments.delta","item_id":"fc_item_b","output_index":1,"delta":"{\"path\":\"/b\"}"}` + "\n\n" +
			"event: response.function_call_arguments.delta\n" +
			`data: {"type":"response.function_call_arguments.delta","item_id":"fc_item_a","output_index":0,"delta":"_more"}` + "\n\n"
	}
	return s
}

// readAll drains a translated reader to EOF, failing only on unexpected errors.
func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read translated stream: %v", err)
	}
	return string(b)
}

// chatArgs maps chat tool_call index -> concatenated arguments fragments,
// and records the ids used when each index was opened.
func chatArgs(t *testing.T, out string) (map[int]string, map[int]string) {
	t.Helper()
	args := map[int]string{}
	ids := map[int]string{}
	seenDone := false
	for _, line := range strings.Split(out, "\n") {
		p := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if p == "" || p == "[DONE]" {
			if p == "[DONE]" {
				seenDone = true
			}
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(p), &chunk); err != nil {
			t.Fatalf("malformed translated chunk %q: %v", p, err)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		for _, tc := range chunk.Choices[0].Delta.ToolCalls {
			if tc.ID != "" {
				ids[tc.Index] = tc.ID
			}
			args[tc.Index] += tc.Function.Arguments
		}
	}
	if !seenDone {
		t.Error("translated chat stream has no [DONE] terminator")
	}
	return args, ids
}

// anthropicArgs maps content_block index -> concatenated partial_json, and
// records the public tool id seen at each block start.
func anthropicArgs(t *testing.T, out string) (map[int]string, map[int]string) {
	t.Helper()
	args := map[int]string{}
	ids := map[int]string{}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "event:") {
			continue // the event name; the data line carries the JSON
		}
		p := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if p == "" {
			continue
		}
		var ev struct {
			Type         string `json:"type"`
			ContentBlock *struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			} `json:"content_block"`
			Index int `json:"index"`
			Delta *struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(p), &ev); err != nil {
			t.Fatalf("malformed anthropic frame %q: %v", p, err)
		}
		switch ev.Type {
		case "content_block_start":
			if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
				ids[ev.Index] = ev.ContentBlock.ID
			}
		case "content_block_delta":
			if ev.Delta != nil && ev.Delta.Type == "input_json_delta" {
				args[ev.Index] += ev.Delta.PartialJSON
			}
		}
	}
	return args, ids
}

func TestChatStreamCarriesArgsWhenItemIDDiffersFromCallID(t *testing.T) {
	out := readAll(t, NewResponsesSSEReader(strings.NewReader(toolStream(false)), "proof"))
	args, ids := chatArgs(t, out)

	if got := args[0]; got != `{"path":"/a"}` {
		t.Errorf("chat index 0 arguments = %q, want the full fragment set", got)
	}
	if ids[0] != "call_public_a" {
		t.Errorf("chat index 0 tool id = %q, want the public call_id", ids[0])
	}
}

func TestAnthropicStreamCarriesArgsWhenItemIDDiffersFromCallID(t *testing.T) {
	out := readAll(t, NewResponsesAnthropicSSEReader(strings.NewReader(toolStream(false)), "proof"))
	args, ids := anthropicArgs(t, out)

	if got := args[0]; got != `{"path":"/a"}` {
		t.Errorf("anthropic block 0 partial_json = %q, want the full fragment set", got)
	}
	if ids[0] != "call_public_a" {
		t.Errorf("anthropic block 0 tool id = %q, want the public call_id", ids[0])
	}
}

func TestChatStreamKeepsInterleavedToolsSeparate(t *testing.T) {
	out := readAll(t, NewResponsesSSEReader(strings.NewReader(toolStream(true)), "proof"))
	args, ids := chatArgs(t, out)

	if got := args[0]; got != `{"path":"/a"}_more` {
		t.Errorf("chat index 0 arguments = %q, want %q", got, `{"path":"/a"}_more`)
	}
	if got := args[1]; got != `{"path":"/b"}` {
		t.Errorf("chat index 1 arguments = %q, want %q", got, `{"path":"/b"}`)
	}
	if ids[0] == "" || ids[1] == "" || ids[0] == ids[1] {
		t.Errorf("chat tool ids = %#v, want two distinct public call ids", ids)
	}
}

// Sequential two-tool stream — the REAL Responses shape (items are appended
// and completed one at a time). Each tool is opened, fragmented, and closed
// before the next one opens.
func toolStreamSequential() string {
	return "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1","created_at":1760000000}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_item_a","type":"function_call","call_id":"call_public_a","name":"read"}}` + "\n\n" +
		"event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_item_a","output_index":0,"delta":"{\"path\":\"/a\"}"}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_item_b","type":"function_call","call_id":"call_public_b","name":"write"}}` + "\n\n" +
		"event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_item_b","output_index":1,"delta":"{\"path\":\"/b\"}"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":9,"output_tokens":4}}}` + "\n\n"
}

// Interleaved fragments (a tool's arguments resume after a LATER tool opened)
// are not expressible on the messages wire: a stopped content_block cannot be
// reopened. The reader must fail visibly instead of emitting into a closed
// block.
func toolStreamInterleaved() string {
	return "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_item_a","type":"function_call","call_id":"call_public_a","name":"read"}}` + "\n\n" +
		"event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_item_a","output_index":0,"delta":"{\"path\":\"/a\"}"}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_item_b","type":"function_call","call_id":"call_public_b","name":"write"}}` + "\n\n" +
		"event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_item_a","output_index":0,"delta":"_more"}` + "\n\n"
}

func TestAnthropicStreamKeepsSequentialToolsSeparate(t *testing.T) {
	out := readAll(t, NewResponsesAnthropicSSEReader(strings.NewReader(toolStreamSequential()), "proof"))
	args, ids := anthropicArgs(t, out)

	if got := args[0]; got != `{"path":"/a"}` {
		t.Errorf("anthropic block 0 partial_json = %q, want %q", got, `{"path":"/a"}`)
	}
	if got := args[1]; got != `{"path":"/b"}` {
		t.Errorf("anthropic block 1 partial_json = %q, want %q", got, `{"path":"/b"}`)
	}
	if ids[0] == "" || ids[1] == "" || ids[0] == ids[1] {
		t.Errorf("anthropic tool ids = %#v, want two distinct public call ids", ids)
	}
	if !strings.Contains(out, `"input_tokens":9`) {
		t.Error("completed terminal usage must reach message_delta")
	}
}

func TestAnthropicStreamRefusesUnrepresentableInterleave(t *testing.T) {
	out := readAll(t, NewResponsesAnthropicSSEReader(strings.NewReader(toolStreamInterleaved()), "proof"))

	if !strings.Contains(out, `"type":"error"`) {
		t.Errorf("a fragment for an already-stopped block must fail visibly, not reopen it:\n%s", out)
	}
	if strings.Contains(out, `"_more"`) {
		t.Errorf("no delta may be emitted into a stopped block:\n%s", out)
	}
}

func TestChatStreamIncompleteKeepsUsage(t *testing.T) {
	in := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n" +
		"event: response.incomplete\n" +
		`data: {"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":12345,"output_tokens":6789}}}` + "\n\n"
	out := readAll(t, NewResponsesSSEReader(strings.NewReader(in), "proof"))

	if !strings.Contains(out, `"finish_reason":"length"`) {
		t.Error("incomplete terminal must map to finish_reason length")
	}
	if !strings.Contains(out, `"prompt_tokens":12345`) || !strings.Contains(out, `"completion_tokens":6789`) {
		t.Errorf("incomplete terminal dropped usage; frame set:\n%s", out)
	}
}

func TestAnthropicStreamIncompleteKeepsUsage(t *testing.T) {
	in := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n" +
		"event: response.incomplete\n" +
		`data: {"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":12345,"output_tokens":6789}}}` + "\n\n"
	out := readAll(t, NewResponsesAnthropicSSEReader(strings.NewReader(in), "proof"))

	if !strings.Contains(out, `"stop_reason":"max_tokens"`) {
		t.Error("incomplete terminal must map to stop_reason max_tokens")
	}
	if !strings.Contains(out, `"input_tokens":12345`) || !strings.Contains(out, `"output_tokens":6789`) {
		t.Errorf("incomplete terminal dropped usage; frame set:\n%s", out)
	}
}

func TestChatStreamFailedKeepsUsage(t *testing.T) {
	in := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n" +
		"event: response.failed\n" +
		`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"server_error","message":"boom"},"usage":{"input_tokens":11,"output_tokens":22}}}` + "\n\n"
	out := readAll(t, NewResponsesSSEReader(strings.NewReader(in), "proof"))

	if !strings.Contains(out, "boom") {
		t.Error("failed terminal must surface the upstream error words")
	}
	if !strings.Contains(out, `"prompt_tokens":11`) || !strings.Contains(out, `"completion_tokens":22`) {
		t.Errorf("failed terminal dropped usage; frame set:\n%s", out)
	}
}

// EOF without a terminal event is a truncated stream, not a successful
// completion: no success finish frame may be synthesized.
func TestChatStreamEOFWithoutTerminalIsNotSuccess(t *testing.T) {
	in := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"partial"}` + "\n\n"
	out := readAll(t, NewResponsesSSEReader(strings.NewReader(in), "proof"))

	if strings.Contains(out, `"finish_reason":"stop"`) || strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Errorf("clean EOF without a terminal event synthesized success:\n%s", out)
	}
	if !strings.Contains(out, `"error"`) {
		t.Errorf("truncated stream must report an error frame:\n%s", out)
	}
}

func TestAnthropicStreamEOFWithoutTerminalIsNotSuccess(t *testing.T) {
	in := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"partial"}` + "\n\n"
	out := readAll(t, NewResponsesAnthropicSSEReader(strings.NewReader(in), "proof"))

	if strings.Contains(out, `"stop_reason":"end_turn"`) || strings.Contains(out, `"stop_reason":"tool_use"`) {
		t.Errorf("clean EOF without a terminal event synthesized success:\n%s", out)
	}
	if !strings.Contains(out, `"type":"error"`) {
		t.Errorf("truncated stream must report an error event:\n%s", out)
	}
}
