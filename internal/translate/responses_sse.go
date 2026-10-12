package translate

import (
	"github.com/c4lyp5o/ezllm/internal/provider"
	"io"
)

// NewResponsesSSEReader composes the Responses event decoder and Chat event
// encoder in legacy compatibility mode. New portable conversions use the same
// modules through NewStreamReader without the legacy policy.
func NewResponsesSSEReader(r io.Reader, clientModel string) *StreamReader {
	return NewStreamReader(r, provider.SurfaceResponses, provider.SurfaceOpenAI, clientModel, CodecOptions{Legacy: true})
}
