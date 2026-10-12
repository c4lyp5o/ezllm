package translate

import (
	"bytes"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func TestStreamPortablePartialFailedOutput(t *testing.T) {
	in := `data: {"id":"c","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}` + "\n\n" + `data: {"error":{"type":"server_error","message":"boom"}}` + "\n\n"
	_, out := codecStream(t, in, provider.SurfaceOpenAI, provider.SurfaceResponses, CodecOptions{})
	frames := parseFrames(t, out)
	if !strings.Contains(frames[len(frames)-1].Data, `"text":"partial"`) {
		t.Fatalf("failed terminal lost assembled partial output %s", out)
	}
}
func TestStreamPortableSourceLateDataRejected(t *testing.T) {
	input := `data: {"id":"c","choices":[{"index":0,"delta":{"content":"first"},"finish_reason":"stop"}]}` + "\n\n" + `data: {"choices":[{"index":0,"delta":{"content":"illegal late"},"finish_reason":null}]}` + "\n\ndata: [DONE]\n\n"
	r, out := codecStream(t, input, provider.SurfaceOpenAI, provider.SurfaceResponses, CodecOptions{})
	if r.Outcome() != FinishFailed || bytes.Contains(out, []byte(`"delta":"illegal late"`)) {
		t.Fatalf("late data accepted %s", out)
	}
}
func TestStreamPortableIncrementalCacheUsage(t *testing.T) {
	in := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_s\",\"usage\":{\"input_tokens\":3}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"cache_read_input_tokens\":5,\"cache_creation_input_tokens\":2,\"output_tokens\":4}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	_, out := codecStream(t, in, provider.SurfaceAnthropic, provider.SurfaceOpenAI, CodecOptions{MessagesUsage: UsageExclusive})
	if !bytes.Contains(out, []byte(`"prompt_tokens":10`)) {
		t.Fatalf("cache counters arriving after input lost %s", out)
	}
}
func TestStreamPortableUnknownFinishRejected(t *testing.T) {
	input := `data: {"id":"c","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"mystery"}]}` + "\n\ndata: [DONE]\n\n"
	r, out := codecStream(t, input, provider.SurfaceOpenAI, provider.SurfaceResponses, CodecOptions{})
	if r.Outcome() != FinishFailed {
		t.Fatalf("unknown terminal became success %s", out)
	}
}
func TestStreamPortableEventNameMismatch(t *testing.T) {
	input := "event: response.completed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"r\",\"status\":\"failed\",\"output\":[]}}\n\n"
	r, out := codecStream(t, input, provider.SurfaceResponses, provider.SurfaceOpenAI, CodecOptions{})
	if r.Outcome() != FinishFailed {
		t.Fatalf("contradictory event became success %s", out)
	}
}
func TestStreamPortableTerminalOnlyResponse(t *testing.T) {
	in := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_r\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"only terminal\"}]}]}}\n\n"
	_, out := codecStream(t, in, provider.SurfaceResponses, provider.SurfaceOpenAI, CodecOptions{})
	if !bytes.Contains(out, []byte("only terminal")) {
		t.Fatalf("terminal-only output silently dropped %s", out)
	}
}
func TestStreamPortableClosedBlockNeverReopens(t *testing.T) {
	// Interleave is a typed failure, NOT a delta into a stopped index.
	input := `data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":"{}"}},{"index":1,"id":"b","function":{"name":"f","arguments":"{}"}}]},"finish_reason":null}]}` + "\n\n" + `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":" "}}]},"finish_reason":null}]}` + "\n\n"
	r, out := codecStream(t, input, provider.SurfaceOpenAI, provider.SurfaceAnthropic, CodecOptions{})
	if r.Outcome() != FinishFailed || !bytes.Contains(out, []byte("stopped")) {
		t.Fatalf("illegal reopen %s", out)
	}
}

type closeTracker struct {
	io.Reader
	closed atomic.Int32
}

func (c *closeTracker) Close() error { c.closed.Add(1); return nil }
func TestStreamPortableCloseExactlyOnce(t *testing.T) {
	src := &closeTracker{Reader: strings.NewReader(portableChatStream)}
	r := NewStreamReader(src, provider.SurfaceOpenAI, provider.SurfaceResponses, "client", CodecOptions{})
	_, err := io.Copy(io.Discard, r)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	r.Close()
	if src.closed.Load() != 1 {
		t.Fatalf("Close count %d", src.closed.Load())
	}
}
func TestStreamPortableMetadataBound(t *testing.T) {
	r := NewStreamReader(strings.NewReader(portableChatStream), provider.SurfaceOpenAI, provider.SurfaceResponses, "client", CodecOptions{})
	// Delta text fits this cap, but retained per-tool identities do not.
	r.maxOutput = 64
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if r.Outcome() != FinishFailed {
		t.Fatalf("metadata not charged against bound %s", b)
	}
}
func TestResponseCodecUnknownFinishRejected(t *testing.T) {
	_, err := DecodeResponse([]byte(`{"id":"c","choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":"mystery"}]}`), provider.SurfaceOpenAI, CodecOptions{})
	if err == nil {
		t.Fatal("unknown finish became success")
	}
}
