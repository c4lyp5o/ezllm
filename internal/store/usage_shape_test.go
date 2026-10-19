package store

import (
	"encoding/json"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func rawUsage(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func wantUsage(t *testing.T, label string, got Usage, in, out, read, write int64) {
	t.Helper()
	if got.In != in || got.Out != out || got.CachedRead != read || got.CachedWrite != write {
		t.Errorf("%s: in=%d out=%d read=%d write=%d, want in=%d out=%d read=%d write=%d",
			label, got.In, got.Out, got.CachedRead, got.CachedWrite, in, out, read, write)
	}
}

// Cost is in·p_in + out·p_out + read·p_read + write·p_write, so every token
// must land in exactly one bucket. The bodies below are live ledger rows.
func TestNormalizeUsageEveryTokenInOneBucket(t *testing.T) {
	auto := provider.UsageConventionAuto
	for _, tc := range []struct {
		label, surface, body string
		conv                 provider.UsageConvention
		in, out, read, write int64
	}{
		// tokenrouter, Responses: the cache write is INSIDE input_tokens
		// (29327 = 3 new + 29324 written). It was billed twice.
		{"responses write included", "responses", `{"input_tokens":29327,"input_tokens_details":{"cache_write_tokens":29324,"cached_tokens":0},"output_tokens":10166}`, auto, 3, 10166, 0, 29324},
		{"responses read included", "responses", `{"input_tokens":196984,"input_tokens_details":{"cache_write_tokens":0,"cached_tokens":195840},"output_tokens":830}`, auto, 1144, 830, 195840, 0},
		// A write larger than the total cannot be inside it.
		{"responses write excluded", "responses", `{"input_tokens":10,"input_tokens_details":{"cache_write_tokens":50},"output_tokens":1}`, auto, 10, 1, 0, 50},

		// tokenrouter, Messages: input_tokens INCLUDES the cache read
		// (usage-convention-design). Declared total, it splits.
		{"anthropic total", "anthropic", `{"cache_creation_input_tokens":0,"cache_read_input_tokens":50688,"input_tokens":63347,"output_tokens":53}`, provider.UsageConventionTotal, 12659, 53, 50688, 0},
		{"anthropic auto stays exclusive", "anthropic", `{"cache_read_input_tokens":50688,"input_tokens":63347,"output_tokens":53}`, auto, 63347, 53, 50688, 0},
		// D3: a total smaller than its cache cannot be inclusive.
		{"anthropic total guard", "anthropic", `{"cache_read_input_tokens":34688,"input_tokens":235,"output_tokens":37}`, provider.UsageConventionTotal, 235, 37, 34688, 0},

		// Chat gateways fronting cached models put the write in the details
		// (opencode/openrouter) or at the top level (command-code), inside
		// prompt_tokens like the read.
		{"chat write in details", "openai", `{"prompt_tokens":1000,"prompt_tokens_details":{"cached_tokens":600,"cache_write_tokens":300},"completion_tokens":5}`, auto, 100, 5, 600, 300},
		{"chat write top level", "openai", `{"prompt_tokens":1000,"cache_creation_input_tokens":300,"prompt_tokens_details":{"cached_tokens":600},"completion_tokens":5}`, auto, 100, 5, 600, 300},
		// DeepSeek's own counters, when no details object is sent.
		{"chat deepseek hit tokens", "openai", `{"prompt_tokens":1000,"prompt_cache_hit_tokens":800,"prompt_cache_miss_tokens":200,"completion_tokens":5}`, auto, 200, 5, 800, 0},
		{"chat top-level cached_tokens", "openai", `{"prompt_tokens":1000,"cached_tokens":700,"completion_tokens":5}`, auto, 300, 5, 700, 0},
		// The details object wins when both are present (live opencode row).
		{"chat details win", "openai", `{"cached_tokens":99456,"prompt_tokens":99704,"prompt_tokens_details":{"cached_tokens":99456},"completion_tokens":174}`, auto, 248, 174, 99456, 0},
	} {
		got := NormalizeUsageWith(Surface(tc.surface), rawUsage(t, tc.body), tc.conv)
		wantUsage(t, tc.label, got, tc.in, tc.out, tc.read, tc.write)
	}
}

// Claude's thinking tokens (output_tokens_details.thinking_tokens) are the
// reasoning count. They are already inside output_tokens; cost never adds
// them again.
func TestNormalizeUsageAnthropicThinking(t *testing.T) {
	got := NormalizeUsage(SurfaceAnthropic, rawUsage(t, `{"input_tokens":191,"output_tokens":590,"output_tokens_details":{"thinking_tokens":246}}`))
	if got.Reasoning != 246 || got.Out != 590 {
		t.Errorf("reasoning=%d out=%d, want 246 / 590", got.Reasoning, got.Out)
	}
}
