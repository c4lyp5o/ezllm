package translate

import "github.com/c4lyp5o/ezllm/internal/provider"

// ResponsesToAnthropic composes shared response codecs with the characterized
// legacy loss/presence policy. Strict conversions do not use this wrapper.
func ResponsesToAnthropic(body []byte, clientModel string) ([]byte, error) {
	opts := CodecOptions{Legacy: true}
	ir, err := DecodeResponse(body, provider.SurfaceResponses, opts)
	if err != nil {
		return nil, err
	}
	return EncodeResponse(ir, provider.SurfaceAnthropic, clientModel, opts)
}
