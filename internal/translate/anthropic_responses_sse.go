package translate

import (
	"github.com/c4lyp5o/ezllm/internal/provider"
	"io"
)

// NewResponsesAnthropicSSEReader is the compatibility entry point; the source
// decoder and destination encoder are shared with every portable stream cell.
func NewResponsesAnthropicSSEReader(r io.Reader, clientModel string) io.Reader {
	return NewStreamReader(r, provider.SurfaceResponses, provider.SurfaceAnthropic, clientModel, CodecOptions{Legacy: true})
}
