package registration

import (
	"encoding/json"
	"testing"
)

// A probe must budget for the model it probes. Measured on the Claude
// subscription surface: Haiku 5.5 has MANDATORY thinking, and a probe body that
// allows 8–16 output tokens comes back with `max_output_tokens` — so the key
// test reports a failed inference step for a perfectly good key. This is the
// same class of failure as picking a probe model by name instead of by
// measurement (see the NIM probe work).
func TestProbeBodyBudgetsForThinkingModels(t *testing.T) {
	for name, body := range map[string]string{
		"chat":      chatProbeBody("claude-haiku-5-5"),
		"responses": responsesProbeBody("claude-haiku-5-5"),
	} {
		var m map[string]any
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatalf("%s: probe body is not JSON: %v (%s)", name, err, body)
		}
		got, ok := m["max_tokens"].(float64)
		if !ok {
			got, ok = m["max_output_tokens"].(float64)
		}
		if !ok {
			t.Fatalf("%s: probe body carries no output budget: %s", name, body)
		}
		if got < 256 {
			t.Errorf("%s: output budget = %v, want >= 256 so a thinking model can answer (body: %s)", name, got, body)
		}
		if m["model"] != "claude-haiku-5-5" {
			t.Errorf("%s: model = %v, want the model under test", name, m["model"])
		}
	}
}
