package translate

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// sseFrame is one parsed `event:`/`data:` pair from an SSE stream.
type sseFrame struct {
	Event string
	Data  string
}

func runSSE(t *testing.T, stream, clientModel string) ([]byte, []sseFrame) {
	t.Helper()
	r := NewStreamReader(strings.NewReader(stream), provider.SurfaceChat, provider.SurfaceAnthropic, clientModel, CodecOptions{Legacy: true})
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("reading translated stream: %v", err)
	}
	return buf.Bytes(), parseFrames(t, buf.Bytes())
}

func parseFrames(t *testing.T, b []byte) []sseFrame {
	t.Helper()
	var frames []sseFrame
	for _, block := range strings.Split(string(b), "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var f sseFrame
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				f.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				f.Data = strings.TrimPrefix(line, "data: ")
			default:
				t.Fatalf("malformed SSE line %q in block %q", line, block)
			}
		}
		frames = append(frames, f)
	}
	return frames
}

func TestSSESynthesisGolden(t *testing.T) {
	in := string(fixture(t, "openai_sse_stream.txt"))
	got, _ := runSSE(t, in, "claude-3-5-sonnet-20240620")
	want := fixture(t, "anthropic_sse_golden.txt")
	if !bytes.Equal(got, want) {
		t.Errorf("golden mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}
}

// TestSSEFrameDiscipline pins the two things a strict Anthropic client
// relies on: every data payload parses as JSON and carries a `type`
// matching its event name.
func TestSSEFrameDiscipline(t *testing.T) {
	in := string(fixture(t, "openai_sse_stream.txt"))
	_, frames := runSSE(t, in, "claude-3-5-sonnet-20240620")
	if len(frames) == 0 {
		t.Fatal("no frames emitted")
	}
	for _, f := range frames {
		var payload struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(f.Data), &payload); err != nil {
			t.Errorf("event %s: data is not JSON: %v (%s)", f.Event, err, f.Data)
			continue
		}
		if payload.Type != f.Event {
			t.Errorf("event %s carries type %q", f.Event, payload.Type)
		}
	}
}

func TestSSEEventOrderGoldenStream(t *testing.T) {
	in := string(fixture(t, "openai_sse_stream.txt"))
	_, frames := runSSE(t, in, "claude-3-5-sonnet-20240620")
	want := []string{
		"ping", "message_start",
		"content_block_start", "content_block_delta", "content_block_delta",
		"content_block_start", "content_block_delta",
		"content_block_stop", "content_block_stop",
		"message_delta", "message_stop",
	}
	if len(frames) != len(want) {
		t.Fatalf("got %d frames, want %d: %+v", len(frames), len(want), frames)
	}
	for i, name := range want {
		if frames[i].Event != name {
			t.Errorf("frame %d = %s, want %s", i, frames[i].Event, name)
		}
	}
}

func TestSSEDoubleToolCall(t *testing.T) {
	in := `data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"t1","function":{"name":"f1","arguments":"{\"a\":1}"}}]},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"t2","function":{"name":"f2","arguments":"{\"b\":2}"}}]},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: {"id":"c1","usage":{"prompt_tokens":5,"completion_tokens":7}}

data: [DONE]

`
	_, frames := runSSE(t, in, "client-model")
	want := []string{
		"ping", "message_start",
		"content_block_start", "content_block_delta",
		"content_block_start", "content_block_delta",
		"content_block_stop", "content_block_stop",
		"message_delta", "message_stop",
	}
	if len(frames) != len(want) {
		t.Fatalf("got %d frames, want %d: %+v", len(frames), len(want), frames)
	}
	for i, name := range want {
		if frames[i].Event != name {
			t.Errorf("frame %d = %s, want %s", i, frames[i].Event, name)
		}
	}
	// Blocks are allocated sequentially: tool 0 -> anthropic 0, tool 1 -> anthropic 1.
	for i, tc := range []struct {
		index, id, name, partial string
	}{
		{"0", "t1", "f1", `{"a":1}`},
		{"1", "t2", "f2", `{"b":2}`},
	} {
		start := frames[2+i*2]
		if !strings.Contains(start.Data, `"index":`+tc.index) ||
			!strings.Contains(start.Data, `"id":"`+tc.id+`"`) ||
			!strings.Contains(start.Data, `"name":"`+tc.name+`"`) {
			t.Errorf("block start %d = %s", i, start.Data)
		}
		delta := frames[3+i*2]
		var payload struct {
			Delta struct {
				Partial string `json:"partial_json"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(delta.Data), &payload); err != nil {
			t.Fatalf("delta payload: %v", err)
		}
		if payload.Delta.Partial != tc.partial {
			t.Errorf("partial_json %d = %q, want %q", i, payload.Delta.Partial, tc.partial)
		}
	}
	if !strings.Contains(frames[8].Data, `"stop_reason":"tool_use"`) {
		t.Errorf("message_delta = %s", frames[8].Data)
	}
	if !strings.Contains(frames[8].Data, `"input_tokens":5`) || !strings.Contains(frames[8].Data, `"output_tokens":7`) {
		t.Errorf("usage not carried: %s", frames[8].Data)
	}
}

// TestSSEMidStreamError: an upstream error frame becomes an Anthropic
// error event followed by EOF — never a bare close.
func TestSSEMidStreamError(t *testing.T) {
	in := `data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"content":"par"},"finish_reason":null}]}

data: {"error":{"message":"upstream exploded","type":"server_error"}}

`
	out, frames := runSSE(t, in, "client-model")
	if got := frames[len(frames)-1].Event; got != "error" {
		t.Fatalf("last frame = %s, want error\n%s", got, out)
	}
	last := frames[len(frames)-1].Data
	if !strings.Contains(last, `"type":"api_error"`) || !strings.Contains(last, "upstream exploded") {
		t.Errorf("error frame = %s", last)
	}
	if !strings.HasSuffix(string(out), "\n\n") {
		t.Error("stream must end with a complete frame")
	}
}

// TestSSENoUsage: a provider that ignores include_usage still gets a
// well-formed close with honest zeros (locked decision Q5).
func TestSSENoUsage(t *testing.T) {
	in := `data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	_, frames := runSSE(t, in, "client-model")
	last := frames[len(frames)-1]
	if last.Event != "message_stop" {
		t.Fatalf("last frame = %s, want message_stop: %+v", last.Event, frames)
	}
	delta := frames[len(frames)-2]
	if delta.Event != "message_delta" {
		t.Fatalf("second-to-last = %s, want message_delta", delta.Event)
	}
	if !strings.Contains(delta.Data, `"input_tokens":0`) || !strings.Contains(delta.Data, `"output_tokens":0`) {
		t.Errorf("usage = %s, want honest zeros", delta.Data)
	}
	if !strings.Contains(delta.Data, `"stop_reason":"end_turn"`) {
		t.Errorf("stop_reason = %s", delta.Data)
	}
}

// TestSSEUsageOnFinishChunk: several providers attach `usage` to the chunk
// that carries finish_reason instead of a dedicated include_usage chunk
// (deepseek-flash and opencode-go both did in prod, which recorded 0/1 tokens
// for every streamed translated call). The real numbers must reach
// message_delta either way.
func TestSSEUsageOnFinishChunk(t *testing.T) {
	in := `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1234,"completion_tokens":56}}

data: [DONE]

`
	_, frames := runSSE(t, in, "client-model")
	delta := frames[len(frames)-2]
	if delta.Event != "message_delta" {
		t.Fatalf("second-to-last = %s, want message_delta: %+v", delta.Event, frames)
	}
	if !strings.Contains(delta.Data, `"input_tokens":1234`) || !strings.Contains(delta.Data, `"output_tokens":56`) {
		t.Fatalf("usage lost when attached to the finish chunk: %s", delta.Data)
	}
	if !strings.Contains(delta.Data, `"stop_reason":"end_turn"`) {
		t.Errorf("stop_reason = %s", delta.Data)
	}
}

// TestSSEUsageOnContentChunk: usage attached to an earlier content chunk is
// captured too, and the stream still closes exactly once.
func TestSSEUsageOnContentChunk(t *testing.T) {
	in := `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}],"usage":{"prompt_tokens":7,"completion_tokens":9}}

data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	out, frames := runSSE(t, in, "client-model")
	delta := frames[len(frames)-2]
	if !strings.Contains(delta.Data, `"input_tokens":7`) || !strings.Contains(delta.Data, `"output_tokens":9`) {
		t.Fatalf("usage lost when attached to a content chunk: %s", delta.Data)
	}
	if n := strings.Count(string(out), "event: message_stop"); n != 1 {
		t.Errorf("message_stop emitted %d times, want 1", n)
	}
}

// TestSSEStreamCutWithoutFinish: a transport cut after content but
// before finish_reason closes the message rather than hanging.
func TestSSEStreamCutWithoutFinish(t *testing.T) {
	in := `data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"content":"half"},"finish_reason":null}]}

`
	out, frames := runSSE(t, in, "client-model")
	last := frames[len(frames)-1].Event
	if last != "message_stop" {
		t.Fatalf("last frame = %s, want message_stop\n%s", last, out)
	}
	if len(frames) < 4 {
		t.Errorf("stream cut lost its content frames: %+v", frames)
	}
}

// TestSSEEmptyStream: nothing at all from upstream still yields an
// error frame instead of a silent empty body. The connect-time ping
// legitimately precedes it.
func TestSSEEmptyStream(t *testing.T) {
	out, frames := runSSE(t, "", "client-model")
	if len(frames) != 2 || frames[0].Event != "ping" || frames[1].Event != "error" {
		t.Fatalf("frames = %+v, want ping then error\n%s", frames, out)
	}
}

func TestSSEPingEmittedOnce(t *testing.T) {
	in := string(fixture(t, "openai_sse_stream.txt"))
	_, frames := runSSE(t, in, "claude-3-5-sonnet-20240620")
	pings := 0
	for _, f := range frames {
		if f.Event == "ping" {
			pings++
		}
	}
	if pings != 1 {
		t.Errorf("pings = %d, want 1", pings)
	}
	if frames[0].Event != "ping" {
		t.Errorf("first frame = %s, want ping", frames[0].Event)
	}
}
