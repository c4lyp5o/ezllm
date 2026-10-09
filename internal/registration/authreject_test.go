package registration

import (
	"net/http"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider" // adjust if module path differs
)

func TestIsAuthRejectFor(t *testing.T) {
	gem := provider.NewGeminiOpenAI(http.DefaultClient)
	oai := provider.NewOpenAICompatible(http.DefaultClient)

	if !isAuthRejectFor(gem, 400) {
		t.Error("gemini: 400 must count as auth reject")
	}
	if !isAuthRejectFor(gem, 401) || !isAuthRejectFor(oai, 401) {
		t.Error("401 must always count as auth reject")
	}
	if isAuthRejectFor(oai, 400) {
		t.Error("openai-compatible: 400 must NOT count as auth reject")
	}
	if isAuthRejectFor(gem, 404) {
		t.Error("gemini: 404 must not count as auth reject")
	}
}
