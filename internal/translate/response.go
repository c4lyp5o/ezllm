package translate

import (
	"encoding/json"
	"strings"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// Response translates an OpenAI chat-completions response body into an
// Anthropic messages response body.
//
// The Anthropic envelope always carries a valid stop_reason and a
// content array (possibly empty); a client that renders nothing is
// better than one that chokes on a missing field.
func Response(body []byte, clientModel string) ([]byte, error) {
	opts := CodecOptions{Legacy: true}
	ir, err := DecodeResponse(body, provider.SurfaceOpenAI, opts)
	if err != nil {
		return nil, err
	}
	return EncodeResponse(ir, provider.SurfaceAnthropic, clientModel, opts)
}

// parseToolArguments turns the OpenAI `arguments` JSON string into the
// object Anthropic's tool_use.input expects. Empty or malformed
// arguments degrade to an empty object / the raw string rather than an
// error — a tool call with odd arguments must not fail the whole reply.
func parseToolArguments(arguments string) any {
	if strings.TrimSpace(arguments) == "" {
		return map[string]any{}
	}
	var input any
	if err := json.Unmarshal([]byte(arguments), &input); err != nil {
		return arguments
	}
	return input
}

// mapStopReason converts an OpenAI finish_reason to the Anthropic
// stop_reason vocabulary. Unknown or empty values become end_turn.
func mapStopReason(finishReason string) string {
	switch finishReason {
	case "stop", "":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

// anthropicErrorEnvelope is the error shape Anthropic clients parse.
type anthropicErrorEnvelope struct {
	Type  string                  `json:"type"`
	Error anthropicErrorEnvelope_ `json:"error"`
}

type anthropicErrorEnvelope_ struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Error converts an OpenAI error body into an Anthropic error envelope,
// preserving the upstream's own words. Best-effort: an unparseable body
// is wrapped verbatim in the message field so the client still sees a
// well-formed anthropic error instead of a wire-shape mismatch.
func Error(body []byte) []byte {
	kind, msg := "api_error", "upstream error"
	var up struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &up); err == nil && up.Error.Message != "" {
		msg = up.Error.Message
		if up.Error.Type != "" {
			kind = up.Error.Type
		}
	}
	out, _ := json.Marshal(anthropicErrorEnvelope{
		Type:  "error",
		Error: anthropicErrorEnvelope_{Type: kind, Message: msg},
	})
	return out
}
