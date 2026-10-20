package router

import (
	"errors"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// A service pin (embeddings, rerank, ...) names the endpoint the model serves.
// It must ADMIT that endpoint (it used to be refused there: every service
// surface was named "openai") and refuse the others with ErrPinned — never
// "pinned to an unknown wire", never a translation.
func TestServicePinAdmitsItsOwnEndpoint(t *testing.T) {
	cat := withProbes(withCombo(newFake(), singleHopCombo("gpt-6.1-sol", 2)))
	cat.pins = map[string]string{"2/gpt-6.1-sol": "embeddings"}
	for _, res := range []*Resolver{New(cat), universalResolver(cat)} {
		for _, target := range []string{wireCombo, "super-ssn/gpt-6.1-sol"} {
			wire, translated, err := resolveWire(t, res, target, provider.SurfaceEmbeddings)
			if err != nil || wire != provider.SurfaceEmbeddings || translated {
				t.Fatalf("%s: wire=%q translated=%v err=%v; want native embeddings", target, wire, translated, err)
			}
		}
	}
}

func TestServicePinRefusesChatWithPinnedError(t *testing.T) {
	cat := withProbes(withCombo(newFake(), singleHopCombo("gpt-6.1-sol", 2)))
	cat.pins = map[string]string{"2/gpt-6.1-sol": "embeddings"}
	for _, res := range []*Resolver{New(cat), universalResolver(cat)} {
		_, _, err := resolveWire(t, res, "super-ssn/gpt-6.1-sol", provider.SurfaceOpenAI)
		var p *ErrPinned
		if !errors.As(err, &p) || p.Pinned != "embeddings" || p.Got != "openai" {
			t.Fatalf("err = %v, want ErrPinned{pinned embeddings, got openai}", err)
		}
	}
}

// A chat pin must not let a service request through, and must never turn it
// into a translated chat call (a rerank body POSTed to /chat/completions).
func TestChatPinRefusesServiceRequest(t *testing.T) {
	cat := withProbes(withCombo(newFake(), singleHopCombo("gpt-6.1-sol", 2)))
	cat.pins = map[string]string{"2/gpt-6.1-sol": "openai"}
	for _, res := range []*Resolver{New(cat), universalResolver(cat)} {
		wire, translated, err := resolveWire(t, res, "super-ssn/gpt-6.1-sol", provider.SurfaceRerank)
		var p *ErrPinned
		if !errors.As(err, &p) || p.Pinned != "openai" || p.Got != "rerank" {
			t.Fatalf("wire=%q translated=%v err=%v; want ErrPinned{pinned openai, got rerank}", wire, translated, err)
		}
	}
}
