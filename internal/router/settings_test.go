package router

import (
	"context"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/settings"
)

func TestSettingsTranslationUsesAdmittedPolicy(t *testing.T) {
	cat := withProbes(withCombo(newFake(), singleHopCombo("gpt-6.1-sol", 2)))
	cat.noProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceChat)] = true
	cat.yesProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceResponses)] = true
	r := universalResolver(cat)
	p := settings.Defaults()
	admitted := settings.With(context.Background(), p)
	p.Translation = false
	next := settings.With(context.Background(), p)
	if _, err := r.ResolveCandidates(next, wireCombo, provider.SurfaceChat, "test"); err == nil {
		t.Fatal("disabled translation admitted translated route")
	}
	routes, err := r.ResolveCandidates(admitted, wireCombo, provider.SurfaceChat, "test")
	if err != nil || len(routes) != 1 || !routes[0].Translated {
		t.Fatal(routes, err)
	}
}
