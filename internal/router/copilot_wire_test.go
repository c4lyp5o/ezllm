package router

import (
	"context"
	"errors"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// withCopilot adds a Copilot account (id 4) to the fixture.
func withCopilot(f *fakeCatalog) *fakeCatalog {
	f.accounts["copilot"] = &provider.Account{ID: 4, Name: "copilot", Namespace: "copilot",
		Kind: provider.KindGitHubCopilot, BaseURL: "https://api.githubcopilot.com", Enabled: true}
	f.keys[4] = f.keys[2]
	return f
}

// Copilot serves claude* on /v1/messages only. A Responses client must be
// translated onto that wire at RESOLUTION, with no probe row: dialing
// /responses would only be refused by the adapter after a rate slot, an
// attempt and possibly a token refresh were spent.
func TestCopilotClaudeOnResponsesIsTranslatedNotDialed(t *testing.T) {
	cat := withCopilot(withProbes(withCombo(newFake(), singleHopCombo("claude-sonnet-5", 4))))

	wire, translated, err := resolveWire(t, universalResolver(cat), wireCombo, provider.SurfaceResponses)
	if err != nil || wire != provider.SurfaceAnthropic || !translated {
		t.Fatalf("wire=%q translated=%v err=%v; want anthropic/true", wire, translated, err)
	}
}

// gemini* rides /chat/completions on Copilot.
func TestCopilotGeminiOnResponsesIsTranslatedToChat(t *testing.T) {
	cat := withCopilot(withProbes(withCombo(newFake(), singleHopCombo("gemini-3.5-flash", 4))))

	wire, translated, err := resolveWire(t, universalResolver(cat), wireCombo, provider.SurfaceResponses)
	if err != nil || wire != provider.SurfaceChat || !translated {
		t.Fatalf("wire=%q translated=%v err=%v; want chat/true", wire, translated, err)
	}
}

// With translation off there is no wire left: a direct route is refused at
// resolution (400 with a reason) instead of reaching the adapter's guard.
func TestCopilotClaudeOnResponsesWithoutTranslationIsRefused(t *testing.T) {
	cat := withCopilot(withProbes(newFake()))
	_, err := New(cat).ResolveCandidates(context.Background(), "copilot/claude-sonnet-5", provider.SurfaceResponses, "test")
	var wu *ErrWireUnsupported
	if !errors.As(err, &wu) {
		t.Fatalf("err = %v, want ErrWireUnsupported", err)
	}
}

// Models Copilot DOES serve on /responses keep the client's wire untouched.
func TestCopilotCodexStaysOnResponses(t *testing.T) {
	cat := withCopilot(withProbes(withCombo(newFake(), singleHopCombo("gpt-5.3-codex", 4))))

	wire, translated, err := resolveWire(t, universalResolver(cat), wireCombo, provider.SurfaceResponses)
	if err != nil || wire != provider.SurfaceResponses || translated {
		t.Fatalf("wire=%q translated=%v err=%v; want responses/false", wire, translated, err)
	}
}
