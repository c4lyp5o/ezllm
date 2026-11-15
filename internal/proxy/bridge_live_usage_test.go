package proxy

import (
	"github.com/c4lyp5o/ezllm/internal/provider"
	"testing"
)

// Counters from the 2026-11-14 synthetic Hermes-shaped live probes, with
// cache-write and thinking details restored by the bridge fixes. Verify the
// production ledger tap's interpretation without spending subscription quota.
func TestLiveBridgeUsageCountersReachLedgerTap(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		surface                        provider.Surface
		lines                          []string
		cold, out, read, write, reason int64
	}{
		{"claude", provider.SurfaceAnthropic, []string{
			`data: {"type":"message_start","message":{"usage":{"input_tokens":4,"output_tokens":4,"cache_creation_input_tokens":9648,"cache_read_input_tokens":2615}}}`,
			`data: {"type":"message_delta","usage":{"input_tokens":4,"output_tokens":26,"cache_creation_input_tokens":9648,"cache_read_input_tokens":2615,"output_tokens_details":{"thinking_tokens":22}}}`,
		}, 4, 26, 2615, 9648, 22},
		{"copilot", provider.SurfaceChat, []string{`data: {"usage":{"prompt_tokens":7408,"completion_tokens":15,"prompt_tokens_details":{"cached_tokens":0,"cache_write_tokens":7405},"completion_tokens_details":{"reasoning_tokens":8}}}`}, 3, 15, 0, 7405, 8},
		{"copilot retained", provider.SurfaceChat, []string{`data: {"usage":{"prompt_tokens":7476,"completion_tokens":23,"prompt_tokens_details":{"cached_tokens":7405,"cache_write_tokens":68},"completion_tokens_details":{"reasoning_tokens":16}}}`}, 3, 23, 7405, 68, 16},
		{"agy", provider.SurfaceChat, []string{`data: {"usage":{"prompt_tokens":17745,"completion_tokens":19,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}}`}, 17745, 19, 0, 0, 0},
		{"agy resumed", provider.SurfaceChat, []string{`data: {"usage":{"prompt_tokens":18097,"completion_tokens":19,"prompt_tokens_details":{"cached_tokens":15697},"completion_tokens_details":{"reasoning_tokens":0}}}`}, 2400, 19, 15697, 0, 0},
		{"agy tool result", provider.SurfaceChat, []string{`data: {"usage":{"prompt_tokens":18466,"completion_tokens":200,"prompt_tokens_details":{"cached_tokens":15733},"completion_tokens_details":{"reasoning_tokens":157}}}`}, 2733, 200, 15733, 0, 157},
		{"codex", provider.SurfaceChat, []string{`data: {"usage":{"prompt_tokens":12595,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}}`}, 12595, 5, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tap := NewUsageTapperFor(tc.surface, provider.UsageConventionAuto)
			for _, line := range tc.lines {
				tap.Observe([]byte(line))
			}
			u := tap.Usage()
			if u == nil || u.In != tc.cold || u.Out != tc.out || u.CachedRead != tc.read || u.CachedWrite != tc.write || u.Reasoning != tc.reason {
				t.Fatalf("ledger usage = %+v", u)
			}
		})
	}
}
