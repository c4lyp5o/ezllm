package translate

import (
	"encoding/json"
	"github.com/c4lyp5o/ezllm/internal/provider"
	"io"
)

// NewSSEReader is the historic Chat->Messages stream entry point. Legacy
// mode retains its exact ping/placeholder/close policy; strict new cells use
// NewStreamReader directly and never synthesize a successful truncated finish.
func NewSSEReader(r io.Reader, clientModel string) io.Reader {
	return NewStreamReader(r, provider.SurfaceOpenAI, provider.SurfaceAnthropic, clientModel, CodecOptions{Legacy: true})
}

// mustMarshalString JSON-encodes s; it cannot fail for a string.
func mustMarshalString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// --- upstream chunk decode types (OpenAI streaming shape) -------------

type openaiChunk struct {
	ID      string         `json:"id"`
	Choices []openaiChoice `json:"choices"`
	Usage   *OpenAIUsage   `json:"usage,omitempty"`
	Error   *openaiError   `json:"error,omitempty"`
}

type openaiChoice struct {
	Index        int         `json:"index"`
	Delta        openaiDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type openaiDelta struct {
	Role      string            `json:"role,omitempty"`
	Content   string            `json:"content,omitempty"`
	ToolCalls []openaiToolDelta `json:"tool_calls,omitempty"`
}

type openaiToolDelta struct {
	Index    int              `json:"index"`
	ID       string           `json:"id,omitempty"`
	Function *openaiFuncDelta `json:"function,omitempty"`
}

type openaiFuncDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type openaiError struct {
	Message string `json:"message"`
}
