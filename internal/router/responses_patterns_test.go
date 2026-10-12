package router

// responses_models entries are regexes matched FULLY against "<ns>/<model>".
// The anchoring is the part that matters: an unanchored list turns a typo into
// a fleet-wide wire change (a bare `sol` would put every *sol* model on the
// responses wire), and a plain id must still match only itself.

import (
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func TestResponsesPatternsMatchFully(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		ns       string
		model    string
		want     bool
	}{
		{"plain id matches itself", []string{"tokenrouter/openai/gpt-6.1-sol"}, "tokenrouter", "openai/gpt-6.1-sol", true},
		{"plain id does not match another account", []string{"tokenrouter/openai/gpt-6.1-sol"}, "vercel-ai-gateway", "openai/gpt-6.1-sol", false},
		{"family pattern covers variants", []string{`vercel-ai-gateway/openai/gpt-6\.1-sol.*`}, "vercel-ai-gateway", "openai/gpt-6.1-sol-fast", true},
		{"family pattern still anchored at the start", []string{`stationine/gpt-6\.1-sol`}, "not-stationine", "gpt-6.1-sol", false},
		{"unanchored-looking entry cannot match a suffix", []string{`gpt-6\.1-sol`}, "stationine", "gpt-6.1-sol", false},
		{"prefix pattern with wildcard namespace", []string{`.*/gpt-6\.1-sol`}, "space-stationine", "gpt-6.1-sol", true},
		{"empty list is inert", nil, "stationine", "gpt-6.1-sol", false},
		{"blank entry ignored", []string{"   "}, "stationine", "gpt-6.1-sol", false},
		{"invalid regex dropped, valid one still applies", []string{"[", `space-stationine/gpt-6\.1-sol`}, "space-stationine", "gpt-6.1-sol", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := New(nil).WithResponsesModels(c.patterns)
			if got := r.needsResponsesWire(provider.SurfaceOpenAI, c.ns, c.model); got != c.want {
				t.Errorf("needsResponsesWire(%q/%q) = %v, want %v", c.ns, c.model, got, c.want)
			}
		})
	}
}

// A responses-SURFACE client is already on the wire: its hops are never
// re-marked, whatever the curated list says.
func TestResponsesPatternsSkipResponsesClients(t *testing.T) {
	r := New(nil).WithResponsesModels([]string{`.*`})
	if r.needsResponsesWire(provider.SurfaceResponses, "anything", "at-all") {
		t.Error("a responses client must pass through unchanged")
	}
}
