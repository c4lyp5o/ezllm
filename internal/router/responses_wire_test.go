package router

import (
	"context"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

func solCombo() *store.Combo {
	return &store.Combo{
		ID: 11, Name: "daily", Strategy: "failover", Enabled: true,
		Hops: []store.ComboHop{
			{AccountID: 2, ModelID: "gpt-6.1-sol", Weight: 1},
			{AccountID: 2, ModelID: "gpt-6-luna", Weight: 1},
		},
	}
}

// A curated hop keeps its normal combo position and carries the wire marker;
// the route's Surface stays the CLIENT's.
func TestResponsesWireHopMarkedInPlace(t *testing.T) {
	cat := withCombo(newFake(), solCombo())
	r := New(cat).WithResponsesModels([]string{"super-ssn/gpt-6.1-sol"})

	routes, err := r.ResolveCandidates(context.Background(), "daily", provider.SurfaceChat, "test")
	if err != nil {
		t.Fatalf("resolution: %v", err)
	}
	if len(routes) != 2 {
		t.Fatalf("candidates = %d, want 2", len(routes))
	}
	if !routes[0].ResponsesWire || routes[0].Model != "gpt-6.1-sol" {
		t.Errorf("hop 0 = %+v, want the marked sol hop in position", routes[0])
	}
	if routes[1].ResponsesWire {
		t.Errorf("unmarked hop carried the wire flag: %+v", routes[1])
	}
	if routes[0].Surface != provider.SurfaceChat {
		t.Errorf("surface = %v, want the client's openai surface", routes[0].Surface)
	}
	if routes[0].Translated {
		t.Error("a responses-wire hop must not also be marked translated")
	}
}

// Empty list (default) marks nothing — the feature is inert.
func TestResponsesWireEmptyListInert(t *testing.T) {
	cat := withCombo(newFake(), solCombo())
	r := New(cat) // no WithResponsesModels

	routes, err := r.ResolveCandidates(context.Background(), "daily", provider.SurfaceChat, "test")
	if err != nil {
		t.Fatal(err)
	}
	for i, rt := range routes {
		if rt.ResponsesWire {
			t.Errorf("route %d marked with no curated list", i)
		}
	}
}

// The list matches namespace+model exactly: a same-name model in another
// namespace, or a different model in the same namespace, stays native.
func TestResponsesWireMatchesNamespaceAndModel(t *testing.T) {
	cat := withCombo(newFake(), solCombo())
	r := New(cat).WithResponsesModels([]string{"opengo/gpt-6.1-sol"})

	routes, err := r.ResolveCandidates(context.Background(), "daily", provider.SurfaceChat, "test")
	if err != nil {
		t.Fatal(err)
	}
	for i, rt := range routes {
		if rt.ResponsesWire {
			t.Errorf("route %d marked by a different namespace: %+v", i, rt)
		}
	}
}

// Direct routes get the same wire fix as combo hops.
func TestResponsesWireDirectRouteMarked(t *testing.T) {
	r := New(newFake()).WithResponsesModels([]string{"super-ssn/gpt-6.1-sol"})

	routes, err := r.ResolveCandidates(context.Background(), "super-ssn/gpt-6.1-sol", provider.SurfaceChat, "test")
	if err != nil {
		t.Fatalf("direct resolution: %v", err)
	}
	if len(routes) != 1 || !routes[0].ResponsesWire {
		t.Fatalf("direct route = %+v, want the wire marker", routes)
	}
}

// Anthropic clients now ride the override too — the anthropic→responses leg
// (Claude Code against a hop whose chat path refuses tools+reasoning and which
// has no /v1/messages at all). Responses clients ARE the wire: never marked.
func TestResponsesWireMarksAnthropicNotResponses(t *testing.T) {
	cat := withCombo(newFake(), solCombo())
	r := New(cat).WithResponsesModels([]string{"super-ssn/gpt-6.1-sol"})

	routes, err := r.ResolveCandidates(context.Background(), "daily", provider.SurfaceAnthropic, "test")
	if err != nil {
		t.Fatal(err)
	}
	marked := 0
	for _, rt := range routes {
		if rt.ResponsesWire {
			marked++
			if rt.Surface != provider.SurfaceAnthropic {
				t.Fatalf("the client surface must never change: %+v", rt)
			}
		}
	}
	if marked == 0 {
		t.Fatalf("an anthropic client on a listed hop must be marked: %+v", routes)
	}

	routes, err = r.ResolveCandidates(context.Background(), "daily", provider.SurfaceResponses, "test")
	if err != nil {
		t.Fatal(err)
	}
	for i, rt := range routes {
		if rt.ResponsesWire {
			t.Errorf("responses route %d wrongly marked: %+v", i, rt)
		}
	}
}
