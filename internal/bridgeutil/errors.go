package bridgeutil

import "encoding/json"

// OpenAIErrorEnvelope encodes {"error":{"type","message","code"}}, the error
// body of the OpenAI-shaped bridges (Codex, Antigravity). Claude keeps its
// Anthropic envelope and Copilot keeps its "param": null variant.
func OpenAIErrorEnvelope(errType, message, code string) []byte {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"type": errType, "message": message, "code": code}})
	return b
}
