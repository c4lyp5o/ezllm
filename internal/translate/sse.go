package translate

import (
	"encoding/json"
	"github.com/c4lyp5o/ezllm/internal/provider"
	"io"
)

// NewSSEReader is the historic Chat->Messages stream entry point. Legacy
// mode retains its exact ping/placeholder/close policy; strict new cells use
// NewStreamReader directly and never synthesize a successful truncated finish.
func NewSSEReader(r io.Reader, clientModel string) *StreamReader {
	return NewStreamReader(r, provider.SurfaceOpenAI, provider.SurfaceAnthropic, clientModel, CodecOptions{Legacy: true})
}

// mustMarshalString JSON-encodes s; it cannot fail for a string.
func mustMarshalString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// --- upstream chunk decode types (OpenAI streaming shape) -------------

type openaiToolDelta struct {
	Index    int              `json:"index"`
	ID       string           `json:"id,omitempty"`
	Function *openaiFuncDelta `json:"function,omitempty"`
}

type openaiFuncDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}
