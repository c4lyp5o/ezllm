package codexbridge

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// Explicit opt-in: a synthetic two-tool turn, no caller files or shell execution.
func TestLiveSequentialUsageAudit(t *testing.T) {
	if os.Getenv("EZLLM_CODEX_USAGE_AUDIT") != "1" {
		t.Skip("live subscription usage audit only")
	}
	bin, home := liveCLI(t)
	r := &Runner{Bin: bin, CodexHome: home, Timeout: 90 * time.Second}
	defer r.Shutdown()
	req := Request{Model: "gpt-6.1-sol", Instructions: "Use the provided tool for the task. Be terse.", Input: []UserInput{{Type: "text", Text: "Call get_weather for KL, await its result, then call get_weather for Paris, await its result, then say DONE. Use a single code-mode script with sequential awaits if available."}}, Tools: []ToolSpec{{Name: "get_weather", Description: "Get weather for a city", Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}`)}}}
	var total Usage
	for i := 0; i < 5; i++ {
		res, err := r.Run(context.Background(), req, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("segment=%d tools=%d usage=%+v", i, len(res.ToolCalls), res.Usage)
		total.InputTokens += res.Usage.InputTokens
		total.OutputTokens += res.Usage.OutputTokens
		if len(res.ToolCalls) == 0 {
			if total.InputTokens <= 0 || total.OutputTokens <= 0 {
				t.Fatalf("missing aggregate usage: %+v", total)
			}
			return
		}
		if req.ToolResults == nil {
			req.ToolResults = map[string]string{}
		}
		for _, c := range res.ToolCalls {
			req.ToolResults[c.ID] = "31C"
		}
	}
	t.Fatal("turn exceeded bounded tool segments")
}
