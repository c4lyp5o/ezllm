package router

// M1 — wire identity and wire-first routing.
//
// Today the capability gate runs BEFORE the curated responses-wire override and
// before any notion of "the wire we will actually dial":
//
//   * a curated responses hop that is ALSO probed unable to serve the client
//     surface is routed onto the CHAT wire (translation) or dropped, never onto
//     responses — the operator's explicit intent is silently ignored;
//   * a responses-wire hop's PIN is checked against the CLIENT's surface, not
//     against responses, so the pin guards the wrong wire;
//   * a DIRECT route never consults capability at all, so "ns/model" and the
//     same hop inside a combo can dial different wires for identical inputs.
//
// These tests pin the corrected contract. They are also the compatibility
// baseline: with no override and no proven-impossible capability, every route
// must keep the exact wire it dials today.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

const wireCombo = "wire-combo"

// singleHopCombo builds a one-hop combo (so ordering cannot muddy the wire
// decision under test).
func singleHopCombo(model string, acctID int64) *store.Combo {
	return &store.Combo{
		ID: 9, Name: wireCombo, Strategy: "failover", Enabled: true,
		Hops: []store.ComboHop{{AccountID: acctID, ModelID: model, Weight: 1}},
	}
}

// withProbes returns the fixture with its capability maps allocated, so a test
// can mark probe verdicts without tripping over nil maps.
func withProbes(f *fakeCatalog) *fakeCatalog {
	f.noProto = map[string]bool{}
	f.yesProto = map[string]bool{}
	return f
}

// probeKey mirrors the fake catalog's capability key.
func probeKey(acctID int64, model string, s provider.Surface) string {
	return fmt.Sprintf("%d/%s/%s", acctID, model, s)
}

func TestCuratedResponsesHopBeatsTheCapabilityGate(t *testing.T) {
	cat := withProbes(withCombo(newFake(), singleHopCombo("gpt-6.1-sol", 2)))
	// Native anthropic is PROVEN impossible, and the hop IS proven
	// openai-capable — so today the translation gate claims it and dials the
	// chat wire, discarding the operator's responses override.
	cat.noProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceAnthropic)] = true
	cat.yesProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceOpenAI)] = true

	r := New(cat).WithTranslation(true).WithResponsesModels([]string{`super-ssn/gpt-6\.1-sol`})
	routes, err := r.ResolveCandidates(context.Background(), wireCombo, provider.SurfaceAnthropic, "test")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("candidates = %d, want 1", len(routes))
	}
	rt := routes[0]
	if rt.EffectiveWire() != provider.SurfaceResponses {
		t.Errorf("wire = %q, want responses (the curated override must win over the capability gate)", rt.EffectiveWire())
	}
	if !rt.ResponsesWire || rt.Translated {
		t.Errorf("flags: ResponsesWire=%v Translated=%v, want true/false", rt.ResponsesWire, rt.Translated)
	}
	if rt.Surface != provider.SurfaceAnthropic {
		t.Errorf("client surface = %q, want anthropic (ledger attribution stays the client's)", rt.Surface)
	}
}

func TestCuratedResponsesHopRefusedWhenResponsesProbedFalse(t *testing.T) {
	cat := withProbes(withCombo(newFake(), singleHopCombo("gpt-6.1-sol", 2)))
	// The override names the responses wire, and responses is PROVEN
	// impossible for this hop: dialing it anyway wastes a request on a wire we
	// already know cannot answer.
	cat.noProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceResponses)] = true
	cat.yesProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceOpenAI)] = true

	r := New(cat).WithTranslation(true).WithResponsesModels([]string{`super-ssn/gpt-6\.1-sol`})
	_, err := r.ResolveCandidates(context.Background(), wireCombo, provider.SurfaceAnthropic, "test")
	if err == nil {
		t.Fatal("a hop whose curated destination wire is probed impossible must not become a candidate")
	}
	var wu *ErrWireUnsupported
	if !errors.As(err, &wu) {
		t.Errorf("error = %v, want ErrWireUnsupported", err)
	}
}

func TestResponsesWireHopChecksPinAgainstResponses(t *testing.T) {
	cat := withProbes(withCombo(newFake(), singleHopCombo("gpt-6.1-sol", 2)))
	// Pinned to responses + curated onto responses = the pin is satisfied.
	// Today the pin is compared against the CLIENT's surface (anthropic), so
	// this perfectly consistent configuration is refused.
	cat.pins = map[string]string{"2/gpt-6.1-sol": "responses"}

	r := New(cat).WithResponsesModels([]string{`super-ssn/gpt-6\.1-sol`})
	routes, err := r.ResolveCandidates(context.Background(), wireCombo, provider.SurfaceAnthropic, "test")
	if err != nil {
		t.Fatalf("pin to the wire we actually dial was refused: %v", err)
	}
	if len(routes) != 1 || routes[0].EffectiveWire() != provider.SurfaceResponses {
		t.Fatalf("routes = %d wire = %q, want 1 on responses", len(routes), routes[0].EffectiveWire())
	}
}

func TestResponsesWireHopRefusesConflictingPin(t *testing.T) {
	cat := withProbes(withCombo(newFake(), singleHopCombo("gpt-6.1-sol", 2)))
	// Pinned to the CLIENT's surface while curated onto responses: the two
	// instructions contradict each other, and the pin is the operator's hard
	// constraint — it must refuse rather than be silently overridden.
	cat.pins = map[string]string{"2/gpt-6.1-sol": "anthropic"}

	r := New(cat).WithResponsesModels([]string{`super-ssn/gpt-6\.1-sol`})
	_, err := r.ResolveCandidates(context.Background(), wireCombo, provider.SurfaceAnthropic, "test")
	var np *ErrPinned
	if !errors.As(err, &np) {
		t.Fatalf("error = %v, want *ErrPinned", err)
	}
	if np.Pinned != "anthropic" || np.Got != "responses" {
		t.Errorf("pinned=%q got=%q, want the pin checked against the dialed wire", np.Pinned, np.Got)
	}
}

func TestDirectRouteTranslatesLikeTheSameComboHop(t *testing.T) {
	cat := withProbes(withCombo(newFake(), singleHopCombo("gpt-6.1-sol", 2)))
	cat.noProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceAnthropic)] = true
	cat.yesProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceOpenAI)] = true

	r := New(cat).WithTranslation(true)
	combo, cerr := r.ResolveCandidates(context.Background(), wireCombo, provider.SurfaceAnthropic, "test")
	if cerr != nil {
		t.Fatalf("combo: %v", cerr)
	}
	direct, derr := r.ResolveCandidates(context.Background(), "super-ssn/gpt-6.1-sol", provider.SurfaceAnthropic, "test")
	if derr != nil {
		t.Fatalf("direct: %v", derr)
	}
	if len(combo) != 1 || len(direct) != 1 {
		t.Fatalf("candidates: combo=%d direct=%d, want 1 each", len(combo), len(direct))
	}
	if combo[0].EffectiveWire() != provider.SurfaceOpenAI || direct[0].EffectiveWire() != provider.SurfaceOpenAI {
		t.Fatalf("wires: combo=%q direct=%q, want openai for both (same inputs, same wire)",
			combo[0].EffectiveWire(), direct[0].EffectiveWire())
	}
	if !combo[0].Translated || !direct[0].Translated {
		t.Errorf("translated flags: combo=%v direct=%v, want true for both", combo[0].Translated, direct[0].Translated)
	}
	if direct[0].Alias != "super-ssn/gpt-6.1-sol" {
		t.Errorf("direct alias = %q", direct[0].Alias)
	}
}

func TestDirectRouteRefusesProvenImpossibleWire(t *testing.T) {
	cat := withProbes(newFake())
	// Native anthropic proven impossible, no translation entitlement, no
	// override: there is no wire left to dial. Today the direct path ignores
	// capability and burns an upstream call that cannot succeed.
	cat.noProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceAnthropic)] = true

	r := New(cat)
	_, err := r.ResolveCandidates(context.Background(), "super-ssn/gpt-6.1-sol", provider.SurfaceAnthropic, "test")
	if err == nil {
		t.Fatal("a direct route must not be created for a wire the model cannot serve")
	}
	var wu *ErrWireUnsupported
	if !errors.As(err, &wu) {
		t.Fatalf("error = %v, want ErrWireUnsupported", err)
	}
}

func TestUntestedCapabilityStaysNative(t *testing.T) {
	// No probe facts at all: NULL means untested, and untested must keep
	// dialing the client's own wire (fail-open, exactly as today).
	cat := withProbes(withCombo(newFake(), singleHopCombo("gpt-6.1-sol", 2)))
	r := New(cat).WithTranslation(true)

	for _, surface := range []provider.Surface{provider.SurfaceOpenAI, provider.SurfaceAnthropic, provider.SurfaceResponses} {
		routes, err := r.ResolveCandidates(context.Background(), wireCombo, surface, "test")
		if err != nil {
			t.Fatalf("%s: %v", surface, err)
		}
		if routes[0].EffectiveWire() != surface {
			t.Errorf("%s: wire = %q, want native", surface, routes[0].EffectiveWire())
		}
		if routes[0].Translated || routes[0].ResponsesWire {
			t.Errorf("%s: flags set on an untested native route (%v/%v)", surface, routes[0].Translated, routes[0].ResponsesWire)
		}
	}
}

func TestProvenNativeHopKeepsItsWireAndFlagsClear(t *testing.T) {
	cat := withProbes(withCombo(newFake(), singleHopCombo("gpt-6.1-sol", 2)))
	cat.yesProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceAnthropic)] = true
	cat.yesProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceOpenAI)] = true

	r := New(cat).WithTranslation(true)
	routes, err := r.ResolveCandidates(context.Background(), wireCombo, provider.SurfaceAnthropic, "test")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if routes[0].EffectiveWire() != provider.SurfaceAnthropic || routes[0].Translated {
		t.Errorf("native-capable hop dialed %q translated=%v, want native anthropic",
			routes[0].EffectiveWire(), routes[0].Translated)
	}
}

func TestResponsesWireNeverAppliesToResponsesClients(t *testing.T) {
	cat := withProbes(withCombo(newFake(), singleHopCombo("gpt-6.1-sol", 2)))
	r := New(cat).WithResponsesModels([]string{`super-ssn/gpt-6\.1-sol`})
	routes, err := r.ResolveCandidates(context.Background(), wireCombo, provider.SurfaceResponses, "test")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if routes[0].ResponsesWire || routes[0].EffectiveWire() != provider.SurfaceResponses {
		t.Errorf("responses client got flags=%v wire=%q, want a plain native route",
			routes[0].ResponsesWire, routes[0].EffectiveWire())
	}
}
