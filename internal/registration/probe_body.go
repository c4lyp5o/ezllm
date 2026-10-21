package registration

import (
	"encoding/json"
	"fmt"
)

// probeMaxTokens is the output budget every probe body carries. It is not a
// cost knob: models with MANDATORY thinking (Claude Haiku 5.5, measured on the
// subscription surface) refuse to answer inside a tiny budget and come back with
// `max_output_tokens` — which the key test then reports as a failed inference
// step for a perfectly good key. A probe must budget for the model it probes.
const probeMaxTokens = 256

// jsonString quotes s as a JSON string. Go's %q is not JSON: it escapes
// control bytes and invalid UTF-8 as \x.., which no JSON parser accepts.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// chatProbeBody is the OpenAI/Messages-shaped generation probe.
func chatProbeBody(model string) string {
	return fmt.Sprintf(`{"model":%s,"max_tokens":%d,"messages":[{"role":"user","content":"ping"}]}`,
		jsonString(model), probeMaxTokens)
}

// responsesProbeBody is the Responses-shaped surface probe.
func responsesProbeBody(model string) string {
	return fmt.Sprintf(`{"model":%s,"max_output_tokens":%d,"input":"ping"}`, jsonString(model), probeMaxTokens)
}
