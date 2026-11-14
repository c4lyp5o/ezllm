package agybridge

import "testing"

// Native CLI 1.4.12's resumed Gemini turn reports cold input separately from
// cache reads. The cache can exceed cold input; OpenAI prompt_tokens is inclusive.
func TestUsageAddsNativeCacheReadsToInclusivePrompt(t *testing.T) {
	u := openAIUsage(Usage{InputTokens: 2400, OutputTokens: 19, CacheRead: 15697, TotalTokens: 2419})
	if u["prompt_tokens"] != 18097 || u["total_tokens"] != 18116 {
		t.Fatalf("native cold/cache split lost: %v", u)
	}
}
