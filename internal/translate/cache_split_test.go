package translate

// The cache split on translated hops. Every client wire projects UsageIR
// differently (that is the whole point of the codecs), and the ledger tap
// reads the PROJECTED body with rt.Surface = the client's surface — so a
// projection that drops the split is indistinguishable, downstream, from an
// upstream that reported no cache. These tests pin each wire's half of the
// contract: in + cached must always equal the vendor's inclusive total, and
// the split must arrive in the field that wire actually defines.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func usageOf(raw string) map[string]any {
	var ev map[string]any
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		return nil
	}
	u, _ := ev["usage"].(map[string]any)
	return u
}

// Anthropic destination (the Claude Code leg: openai hop -> anthropic client).
// Both encoders must agree, and this is the path the legacy translate.Response
// takes — CodecOptions{Legacy: true} returns before optionalMessagesUsage.
func TestAnthropicProjectionCarriesCacheSplit(t *testing.T) {
	ir := &ResponseIR{
		ID: "c", Finish: FinishCompleted,
		Blocks: []Block{{Kind: BlockText, Text: "hi"}},
		Usage: UsageIR{Input: intPtr(1000), Output: intPtr(42),
			CacheRead: intPtr(900), CacheWrite: intPtr(0)},
	}
	for _, legacy := range []bool{true, false} {
		body, err := EncodeResponse(ir, provider.SurfaceAnthropic, "m", CodecOptions{Legacy: legacy})
		if err != nil {
			t.Fatalf("legacy=%v encode: %v", legacy, err)
		}
		u := usageOf(string(body))
		if u == nil {
			t.Fatalf("legacy=%v: no usage object in %s", legacy, body)
		}
		// input EXCLUDES the 900 cached; in + cached = the vendor's 1000.
		if u["input_tokens"] != float64(100) {
			t.Errorf("legacy=%v input_tokens = %v, want 100 (1000 exclusive of the 900 cached)", legacy, u["input_tokens"])
		}
		if u["cache_read_input_tokens"] != float64(900) {
			t.Errorf("legacy=%v cache_read_input_tokens = %v, want 900", legacy, u["cache_read_input_tokens"])
		}
		// Upstream reported an explicit 0 write: it must be carried, not dropped.
		if u["cache_creation_input_tokens"] != float64(0) {
			t.Errorf("legacy=%v cache_creation_input_tokens = %v, want explicit 0", legacy, u["cache_creation_input_tokens"])
		}
		if u["output_tokens"] != float64(42) {
			t.Errorf("legacy=%v output_tokens = %v, want 42", legacy, u["output_tokens"])
		}
	}
}

// An inconsistent upstream (cache > total) must not emit negative input; the
// cache halves are still reported verbatim so the operator sees the lie.
func TestAnthropicProjectionClampsNegativeInput(t *testing.T) {
	ir := &ResponseIR{ID: "c", Finish: FinishCompleted,
		Usage: UsageIR{Input: intPtr(500), Output: intPtr(1), CacheRead: intPtr(700)}}
	for _, legacy := range []bool{true, false} {
		body, err := EncodeResponse(ir, provider.SurfaceAnthropic, "m", CodecOptions{Legacy: legacy})
		if err != nil {
			t.Fatalf("legacy=%v: %v", legacy, err)
		}
		u := usageOf(string(body))
		if u["input_tokens"] != float64(0) {
			t.Errorf("legacy=%v input_tokens = %v, want 0 (clamped)", legacy, u["input_tokens"])
		}
		if u["cache_read_input_tokens"] != float64(700) {
			t.Errorf("legacy=%v cache_read = %v, want 700", legacy, u["cache_read_input_tokens"])
		}
	}
}

// No split reported -> no cache fields at all. This is the legacy byte
// contract: an upstream that reports no details marshals exactly as before,
// and the tap falls back to cached=0 the way it always did.
func TestNoReportedSplitEmitsNoCacheFields(t *testing.T) {
	ir := &ResponseIR{ID: "c", Finish: FinishCompleted,
		Usage: UsageIR{Input: intPtr(123), Output: intPtr(7)}}
	body, err := EncodeResponse(ir, provider.SurfaceAnthropic, "m", CodecOptions{Legacy: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "cache_") {
		t.Errorf("cache fields appeared with no upstream split: %s", body)
	}
	u := usageOf(string(body))
	if u["input_tokens"] != float64(123) || u["output_tokens"] != float64(7) {
		t.Errorf("usage = %v, want input 123 / output 7", u)
	}
}

// OpenAI destination keeps prompt_tokens INCLUSIVE (that is the OpenAI
// contract: the tap subtracts prompt_tokens_details.cached_tokens) and fills
// the details object the same tap reads.
func TestOpenAIProjectionCarriesCacheSplit(t *testing.T) {
	ir := &ResponseIR{ID: "c", Finish: FinishCompleted,
		Usage: UsageIR{Input: intPtr(1000), Output: intPtr(42), CacheRead: intPtr(900)}}

	body, err := EncodeResponse(ir, provider.SurfaceOpenAI, "m", CodecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	u := usageOf(string(body))
	if u["prompt_tokens"] != float64(1000) {
		t.Errorf("prompt_tokens = %v, want 1000 (inclusive — OpenAI's own contract)", u["prompt_tokens"])
	}
	det, _ := u["prompt_tokens_details"].(map[string]any)
	if det == nil || det["cached_tokens"] != float64(900) {
		t.Errorf("prompt_tokens_details.cached_tokens = %v, want 900", det)
	}
	// total_tokens still covers the whole prompt (cached included).
	if u["total_tokens"] != float64(1042) {
		t.Errorf("total_tokens = %v, want 1042", u["total_tokens"])
	}
}

// Responses destination: input_tokens includes cached (tap subtracts), and the
// details object must carry the real number instead of the old hardcoded 0.
func TestResponsesProjectionCarriesCacheSplit(t *testing.T) {
	ir := &ResponseIR{ID: "r", Finish: FinishCompleted,
		Usage: UsageIR{Input: intPtr(1000), Output: intPtr(42), CacheRead: intPtr(900)}}
	body, err := EncodeResponse(ir, provider.SurfaceResponses, "m", CodecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if out.Usage["input_tokens"] != float64(1000) {
		t.Errorf("input_tokens = %v, want the inclusive 1000", out.Usage["input_tokens"])
	}
	det, _ := out.Usage["input_tokens_details"].(map[string]any)
	if det == nil || det["cached_tokens"] != float64(900) {
		t.Errorf("input_tokens_details.cached_tokens = %v, want 900 (was hardcoded 0)", det)
	}
}

// The stream encoder must agree with the non-stream one for the same wire:
// message_delta is what the tap merges the final totals from.
func TestStreamMessagesUsageMatchesEncode(t *testing.T) {
	u := UsageIR{Input: intPtr(1000), Output: intPtr(42), CacheRead: intPtr(900), CacheWrite: intPtr(0)}
	got := string(streamMessagesUsage(u))
	var m map[string]int
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("not JSON: %v (%s)", err, got)
	}
	want := map[string]int{"input_tokens": 100, "cache_read_input_tokens": 900,
		"cache_creation_input_tokens": 0, "output_tokens": 42}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %d, want %d (full map: %v)", k, m[k], v, m)
		}
	}
	if len(m) != len(want) {
		t.Errorf("unexpected keys in %v", m)
	}
}
