package server

import (
	"encoding/json"
	"github.com/c4lyp5o/ezllm/internal/provider"
	"testing"
)

func TestCopilotComboProbeOmitsUnsupportedOutputCap(t *testing.T) {
	for _, kind := range []provider.Kind{provider.KindCopilotBridge, provider.KindOpenAICompatible} {
		var body map[string]any
		if err := json.Unmarshal(probeBodyFor(kind, "gpt-6-luna"), &body); err != nil {
			t.Fatal(err)
		}
		_, capped := body["max_tokens"]
		if capped != (kind != provider.KindCopilotBridge) {
			t.Fatalf("kind %s: capped=%v", kind, capped)
		}
		if body["model"] != "gpt-6-luna" {
			t.Fatalf("wrong model: %v", body)
		}
	}
}
