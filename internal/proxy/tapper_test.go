package proxy

import (
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// Both regressions below were found by LIVE probing, not by reasoning: the unit
// tests passed while the real streams recorded wrong numbers. Keep the captured
// payloads verbatim so the fixtures stay honest.

// REGRESSION: Anthropic streams carry usage in TWO events. message_start has
// output_tokens=0 (the message has not been generated yet) and message_delta
// carries the FINAL totals. Tapping only the first hit recorded out=0 live.
func TestTapperMergesAnthropicMessageStartAndDelta(t *testing.T) {
	// Verbatim lines captured from opencode-go /v1/messages streaming.
	const (
		start = `data: {"type":"message_start","message":{"id":"msg_fdd49893-74c4-49ea-8d89-bae64f571790","type":"message","role":"assistant","model":"qwen3.8-flash","content":[],"usage":{"input_tokens":6,"output_tokens":0}}}`
		delta = `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":67,"output_tokens":36,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`
		stop  = `data: {"type":"message_stop"}`
	)

	tp := NewUsageTapper(provider.SurfaceAnthropic)
	tp.Observe([]byte(start))

	// After only message_start the tap must NOT look complete: out=0 is a trap.
	if u := tp.Usage(); u == nil || u.Out != 0 {
		t.Fatalf("after message_start: %+v (expected out=0, proving the trap)", u)
	}

	tp.Observe([]byte(delta))
	tp.Observe([]byte(stop))

	u := tp.Usage()
	if u == nil {
		t.Fatal("no usage after message_delta")
	}
	if u.Out != 36 {
		t.Errorf("output_tokens = %d, want 36 (message_delta is authoritative)", u.Out)
	}
	if u.In != 67 {
		t.Errorf("input_tokens = %d, want 67 from message_delta, not 6 from message_start", u.In)
	}
}

// Same, but with a cache hit: message_start carries the cache breakdown and
// message_delta the final totals — the merge must keep BOTH.
func TestTapperKeepsCacheFromStartAndTotalsFromDelta(t *testing.T) {
	const (
		start = `data: {"type":"message_start","message":{"usage":{"input_tokens":252,"output_tokens":1,"cache_read_input_tokens":1792,"cache_creation_input_tokens":0}}}`
		delta = `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":252,"output_tokens":41,"cache_creation_input_tokens":0,"cache_read_input_tokens":1792}}`
	)
	tp := NewUsageTapper(provider.SurfaceAnthropic)
	tp.Observe([]byte(start))
	tp.Observe([]byte(delta))
	u := tp.Usage()
	if u == nil {
		t.Fatal("no usage")
	}
	if u.CachedRead != 1792 {
		t.Errorf("cached_read = %d, want 1792 (must survive the merge)", u.CachedRead)
	}
	if u.Out != 41 {
		t.Errorf("out = %d, want 41 (delta wins)", u.Out)
	}
	if u.In != 252 {
		t.Errorf("in = %d, want 252 (Anthropic EXCLUDES cache_read from input_tokens)", u.In)
	}
}

// REGRESSION: when max_output_tokens truncates a Responses stream, the terminal
// event is response.incomplete — NOT response.completed — and it carries the
// full usage. Tapping only .completed recorded all-zero usage live.
func TestTapperHandlesResponsesIncomplete(t *testing.T) {
	// Verbatim terminal event captured from opencode-go /v1/responses streaming
	// with max_output_tokens=32 (status "incomplete", reason "max_output_tokens").
	const incomplete = `data: {"type":"response.incomplete","response":{"id":"resp_x","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"model":"gpt-6-luna","usage":{"input_tokens":12,"output_tokens":32,"total_tokens":44,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"output_tokens_details":{"reasoning_tokens":9}}}}`

	tp := NewUsageTapper(provider.SurfaceResponses)
	tp.Observe([]byte(`data: {"type":"response.created","response":{"usage":null}}`))
	tp.Observe([]byte(`data: {"type":"response.in_progress","response":{"usage":null}}`))
	tp.Observe([]byte(`data: {"type":"response.output_text.delta","delta":"1"}`))
	tp.Observe([]byte(incomplete))

	u := tp.Usage()
	if u == nil {
		t.Fatal("truncated Responses stream recorded NO usage — regression")
	}
	if u.In != 12 || u.Out != 32 {
		t.Errorf("in/out = %d/%d, want 12/32", u.In, u.Out)
	}
	if u.Reasoning != 9 {
		t.Errorf("reasoning = %d, want 9", u.Reasoning)
	}
}

// response.failed must also be tapped: a failed stream still consumed tokens,
// and recording zero would under-report spend.
func TestTapperHandlesResponsesFailed(t *testing.T) {
	const failed = `data: {"type":"response.failed","response":{"status":"failed","usage":{"input_tokens":50,"output_tokens":3,"input_tokens_details":{"cached_tokens":10,"cache_write_tokens":0}}}}`
	tp := NewUsageTapper(provider.SurfaceResponses)
	tp.Observe([]byte(failed))
	u := tp.Usage()
	if u == nil {
		t.Fatal("response.failed usage not tapped")
	}
	if u.In != 40 || u.CachedRead != 10 || u.Out != 3 {
		t.Errorf("in=%d cr=%d out=%d, want 40/10/3 (cached excluded from in)", u.In, u.CachedRead, u.Out)
	}
}

// The completed path must keep working (the normal case).
func TestTapperHandlesResponsesCompleted(t *testing.T) {
	const done = `data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":12,"output_tokens":27,"total_tokens":39,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"output_tokens_details":{"reasoning_tokens":13}}}}`
	tp := NewUsageTapper(provider.SurfaceResponses)
	tp.Observe([]byte(done))
	u := tp.Usage()
	if u == nil || u.Out != 27 || u.Reasoning != 13 {
		t.Fatalf("completed path broken: %+v", u)
	}
}

// OpenAI streams put usage only in the final chunk; earlier chunks and [DONE]
// must not clear or corrupt it.
func TestTapperOpenAIFinalChunkWins(t *testing.T) {
	tp := NewUsageTapper(provider.SurfaceOpenAI)
	tp.Observe([]byte(`data: {"choices":[{"delta":{"role":"assistant"}}]}`))
	tp.Observe([]byte(`data: {"choices":[{"delta":{"content":"1"}}]}`))
	if tp.Usage() != nil {
		t.Error("usage tapped before the final chunk")
	}
	tp.Observe([]byte(`data: {"choices":[{"delta":{"content":"2"}}],"usage":{"prompt_tokens":2044,"completion_tokens":72,"prompt_tokens_details":{"cached_tokens":1792},"completion_tokens_details":{"reasoning_tokens":28}}}`))
	tp.Observe([]byte(`data: [DONE]`))
	u := tp.Usage()
	if u == nil {
		t.Fatal("no usage")
	}
	if u.In != 252 || u.Out != 72 || u.CachedRead != 1792 || u.Reasoning != 28 {
		t.Errorf("normalized wrong: in=%d out=%d cr=%d rt=%d (want 252/72/1792/28)",
			u.In, u.Out, u.CachedRead, u.Reasoning)
	}
}

// Non-data lines (event:, comments, blanks) must be ignored, never parsed.
func TestTapperIgnoresNonDataLines(t *testing.T) {
	tp := NewUsageTapper(provider.SurfaceAnthropic)
	for _, l := range []string{
		`event: message_delta`, `: comment`, ``, ` `, `data:`, `data: [DONE]`,
		`data: {"type":"ping"}`, `data: {malformed`,
	} {
		tp.Observe([]byte(l))
	}
	if tp.Usage() != nil {
		t.Error("non-usage lines produced usage")
	}
}

// MergeUsage must prefer non-zero later values without discarding earlier
// non-zero fields the later event omits.
func TestMergeUsagePreservesEarlierFields(t *testing.T) {
	base := &store.Usage{In: 252, Out: 1, CachedRead: 1792, CachedWrite: 64, Reasoning: 5}
	// delta omits cache fields entirely (common: providers only send deltas)
	delta := &store.Usage{In: 252, Out: 41}
	got := MergeUsage(base, delta)
	if got.Out != 41 {
		t.Errorf("out = %d, want 41 (later wins)", got.Out)
	}
	if got.CachedRead != 1792 || got.CachedWrite != 64 || got.Reasoning != 5 {
		t.Errorf("earlier non-zero fields lost: %+v", got)
	}
}
