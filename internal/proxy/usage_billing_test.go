package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// A native Messages stream from an account declared usage_convention=total
// (tokenrouter: input_tokens INCLUDES the cache read) splits like the ledger
// promises. The cache breakdown arrives on message_start, the output on
// message_delta; both are merged before normalizing, once.
func TestTapperAppliesTheAccountConvention(t *testing.T) {
	start := `data: {"type":"message_start","message":{"usage":{"input_tokens":63347,"cache_read_input_tokens":50688,"output_tokens":1}}}`
	delta := `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":53}}`
	for conv, wantIn := range map[provider.UsageConvention]int64{
		provider.UsageConventionTotal: 12659,
		provider.UsageConventionAuto:  63347,
	} {
		tp := NewUsageTapperFor(provider.SurfaceAnthropic, conv)
		tp.Observe([]byte(start))
		tp.Observe([]byte(delta))
		u := tp.Usage()
		if u == nil || u.In != wantIn || u.CachedRead != 50688 || u.Out != 53 {
			t.Errorf("%s: %+v, want in=%d read=50688 out=53", conv, u, wantIn)
		}
	}
}

// streamCopy forwards the upstream's bytes EXACTLY — CRLF line endings
// included — and a line longer than the tap's scan limit no longer ends the
// tapping: the usage on the final chunk is still recorded.
func TestStreamCopyIsByteExactAndTapsPastLongLines(t *testing.T) {
	huge := "data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("x", maxScanToken+10) + "\"}}]}\r\n\r\n"
	in := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\r\n\r\n" + huge +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":7}}\r\n\r\ndata: [DONE]\r\n\r\n"
	rec := httptest.NewRecorder()
	_, u, _, err := newDispatcher().streamCopy(context.Background(), rec, streamResponse([]byte(in), 0),
		provider.SurfaceChat, provider.UsageConventionAuto, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != in {
		t.Fatalf("forwarded bytes differ: got %d bytes, want %d", len(got), len(in))
	}
	if u == nil || u.In != 12 || u.Out != 7 {
		t.Fatalf("usage after a long line = %+v, want in=12 out=7", u)
	}
}

// TTFT is the time to the first EVENT. SSE comments are keepalives some
// gateways send the moment the request lands (OpenRouter's
// ": OPENROUTER PROCESSING"); counting one reports a near-zero TTFT that
// hides the prefill it exists to measure.
func TestStreamCopyTTFTIgnoresKeepaliveComments(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		_, _ = io.WriteString(pw, ": OPENROUTER PROCESSING\n\n")
		time.Sleep(60 * time.Millisecond)
		_, _ = io.WriteString(pw, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
		_ = pw.Close()
	}()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: pr}
	ttft, _, _, err := newDispatcher().streamCopy(context.Background(), httptest.NewRecorder(), resp,
		provider.SurfaceChat, provider.UsageConventionAuto, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if ttft < 50*time.Millisecond {
		t.Errorf("ttft = %s: the keepalive comment was counted as the first token", ttft)
	}
}

// message_start's output_tokens is a placeholder (Anthropic sends 1; the
// translated legacy frame does too); message_delta's is the final count. A
// stream whose upstream reported no usage must ledger 0 output, not the
// placeholder — but a later zero still never erases an input the start
// reported and the delta omitted.
func TestTapperFinalOutputCountWins(t *testing.T) {
	tp := NewUsageTapperFor(provider.SurfaceAnthropic, provider.UsageConventionAuto)
	tp.Observe([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":0,"output_tokens":1}}}`))
	tp.Observe([]byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":0,"output_tokens":0}}`))
	if u := tp.Usage(); u == nil || u.Out != 0 {
		t.Fatalf("placeholder output survived: %+v", u)
	}
	tp = NewUsageTapperFor(provider.SurfaceAnthropic, provider.UsageConventionAuto)
	tp.Observe([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":120,"cache_read_input_tokens":900,"output_tokens":1}}}`))
	tp.Observe([]byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":0,"output_tokens":42}}`))
	if u := tp.Usage(); u == nil || u.In != 120 || u.CachedRead != 900 || u.Out != 42 {
		t.Fatalf("merge = %+v, want in=120 read=900 out=42", u)
	}
}
