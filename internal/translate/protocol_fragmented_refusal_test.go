package translate

import (
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func TestProtocolFragmentedRefusal(t *testing.T) {
	first := `data: {"id":"c","choices":[{"index":0,"delta":{"role":"assistant","refusal":"I cannot "},"finish_reason":null}]}` + "\n\n"
	second := `data: {"id":"c","choices":[{"index":0,"delta":{"refusal":"help"},"finish_reason":null}]}` + "\n\n"
	end := `data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
	r, out := codecStream(t, first+second+end, provider.SurfaceOpenAI, provider.SurfaceResponses, CodecOptions{})
	if r.Outcome() != FinishRefusal || !strings.Contains(string(out), `I cannot help`) || strings.Contains(string(out), "response.failed") {
		t.Fatalf("refusal truncated: %s outcome %s", out, r.Outcome())
	}
	r, out = codecStream(t, first+second, provider.SurfaceOpenAI, provider.SurfaceResponses, CodecOptions{})
	if r.Outcome() != FinishFailed {
		t.Fatalf("unterminated refusal succeeded: %s", out)
	}
	// A fragment after finish_reason but before [DONE] is still illegal.
	late := strings.TrimSuffix(end, "data: [DONE]\n\n") + second + "data: [DONE]\n\n"
	r, out = codecStream(t, first+late, provider.SurfaceOpenAI, provider.SurfaceResponses, CodecOptions{})
	if r.Outcome() != FinishFailed {
		t.Fatalf("late refusal accepted: %s", out)
	}
}
