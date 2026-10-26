package router

import (
	"context"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// translatedFixture builds a combo whose hop 1 (account 2, gpt-6.1-sol) is
// PROVEN openai-only, with translation enabled. Hop 0 (account 1,
// gpt-6-luna) is untested-native by default.
func translatedFixture() (*fakeCatalog, *Resolver) {
	cat := withCombo(newFake(), &store.Combo{
		ID: 8, Name: "daily", Strategy: "failover", Enabled: true,
		Hops: []store.ComboHop{
			{AccountID: 1, ModelID: "gpt-6-luna", Weight: 1},
			{AccountID: 2, ModelID: "gpt-6.1-sol", Weight: 1},
		},
	})
	// gpt-6.1-sol: cannot speak anthropic, PROVEN; can speak openai, PROVEN.
	cat.noProto = map[string]bool{"2/gpt-6.1-sol/anthropic": true}
	cat.yesProto = map[string]bool{"2/gpt-6.1-sol/chat": true}
	return cat, New(cat).WithTranslation(true)
}

// Native hops keep absolute priority: the translated candidate rides the
// tail, in combo order.
func TestNativeHopsPreferredOverTranslated(t *testing.T) {
	_, r := translatedFixture()

	routes, err := r.ResolveCandidates(context.Background(), "daily", provider.SurfaceAnthropic, "test")
	if err != nil {
		t.Fatalf("resolution: %v", err)
	}
	if len(routes) != 2 {
		t.Fatalf("candidates = %d, want 2 (native + translated): %+v", len(routes), routes)
	}
	if routes[0].Translated {
		t.Error("native hop must come first")
	}
	if routes[0].Model != "gpt-6-luna" || routes[0].Account.ID != 1 {
		t.Errorf("first route = %s/%d", routes[0].Model, routes[0].Account.ID)
	}
	if !routes[1].Translated {
		t.Error("openai-only hop must carry the translated marker")
	}
	// The translated route keeps the CLIENT's surface: the ledger and the
	// usage tap must never see the wire translation.
	if routes[1].Surface != provider.SurfaceAnthropic {
		t.Errorf("translated route surface = %v, want anthropic", routes[1].Surface)
	}
	if routes[1].Model != "gpt-6.1-sol" {
		t.Errorf("translated route model = %q", routes[1].Model)
	}
}

// A translated hop is a NORMAL member of the strategy ordering (operator
// decision 2026-10-13): a successful translated hop is a success and a failure
// is a failure, so the reordering strategy must be able to choose it — and,
// with a real dice roll, must actually do so over enough resolutions.
func TestTranslatedHopParticipatesInStrategyOrdering(t *testing.T) {
	cat := withCombo(newFake(), &store.Combo{
		ID: 9, Name: "daily", Strategy: "true_round_robin", Enabled: true,
		Hops: []store.ComboHop{
			{AccountID: 2, ModelID: "gpt-6-luna", Weight: 1},
			{AccountID: 2, ModelID: "gpt-6.1-sol", Weight: 1},
		},
	})
	cat.noProto = map[string]bool{"2/gpt-6.1-sol/anthropic": true}
	cat.yesProto = map[string]bool{"2/gpt-6.1-sol/chat": true}
	r := New(cat).WithTranslation(true)

	// true_round_robin is a real dice roll; if the translated hop were still
	// parked outside the strategy it could never lead over 50 rolls (p ≈ 2^-50).
	ledFirst := false
	for i := 0; i < 50; i++ {
		routes, err := r.ResolveCandidates(context.Background(), "daily", provider.SurfaceAnthropic, "test")
		if err != nil {
			t.Fatalf("resolution: %v", err)
		}
		if len(routes) != 2 {
			t.Fatalf("candidates = %d, want 2", len(routes))
		}
		if routes[0].Model == "gpt-6.1-sol" {
			if !routes[0].Translated {
				t.Fatalf("iteration %d: the openai-only hop lost its translated marker", i)
			}
			ledFirst = true
		}
		if routes[1].Translated && routes[0].Translated {
			t.Fatalf("iteration %d: both hops translated?", i)
		}
	}
	if !ledFirst {
		t.Error("the translated hop never led in 50 dice rolls — it is still parked outside the strategy")
	}
}

// With translation OFF, the openai-only hop is dropped exactly as before —
// the knob alone changes nothing else about resolution.
func TestTranslationDisabledDropsOpenAIOnlyHop(t *testing.T) {
	cat, _ := translatedFixture()
	r := New(cat) // no WithTranslation: default off

	routes, err := r.ResolveCandidates(context.Background(), "daily", provider.SurfaceAnthropic, "test")
	if err != nil {
		t.Fatalf("resolution: %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("candidates = %d, want 1 (the native hop only): %+v", len(routes), routes)
	}
	if routes[0].Translated || routes[0].Model != "gpt-6-luna" {
		t.Errorf("surviving route = %+v", routes[0])
	}
}

// Without a PROVEN openai capability the hop is never translation-eligible:
// untested stays native-or-dropped, exactly like today.
func TestUnprovenOpenAICapabilityDropsHop(t *testing.T) {
	cat, _ := translatedFixture()
	// Remove the positive openai mark: anthropic=false, openai=UNTESTED.
	cat.yesProto = nil
	r := New(cat).WithTranslation(true)

	routes, err := r.ResolveCandidates(context.Background(), "daily", provider.SurfaceAnthropic, "test")
	if err != nil {
		t.Fatalf("resolution: %v", err)
	}
	if len(routes) != 1 || routes[0].Translated {
		t.Fatalf("candidates = %+v, want only the native hop", routes)
	}
}

// When no native hop can serve (account disabled), the translated tail
// still serves — a translated hop is a last resort, not decoration.
func TestTranslatedTailServesWhenNativeGone(t *testing.T) {
	cat, r := translatedFixture()
	if a := cat.accounts["opengo"]; a != nil {
		a.Enabled = false
	}

	routes, err := r.ResolveCandidates(context.Background(), "daily", provider.SurfaceAnthropic, "test")
	if err != nil {
		t.Fatalf("resolution: %v", err)
	}
	if len(routes) != 1 || !routes[0].Translated {
		t.Fatalf("candidates = %+v, want the translated hop alone", routes)
	}
}

// An anthropic-capable hop is never translated: proto_anthropic=true means
// the native path is the path.
func TestNativeCapableHopNeverTranslated(t *testing.T) {
	cat, _ := translatedFixture()
	// Mark the "openai-only" hop as actually bilingual.
	cat.yesProto["2/gpt-6.1-sol/anthropic"] = true
	r := New(cat).WithTranslation(true)

	routes, err := r.ResolveCandidates(context.Background(), "daily", provider.SurfaceAnthropic, "test")
	if err != nil {
		t.Fatalf("resolution: %v", err)
	}
	if len(routes) != 2 {
		t.Fatalf("candidates = %d, want 2", len(routes))
	}
	for i, rt := range routes {
		if rt.Translated {
			t.Errorf("route %d must be native: %+v", i, rt)
		}
	}
}

// The openai surface never translates (no client needs it), and an untested
// hop on the openai surface behaves exactly as today.
func TestOpenAIClientSurfaceNeverTranslates(t *testing.T) {
	_, r := translatedFixture()

	routes, err := r.ResolveCandidates(context.Background(), "daily", provider.SurfaceChat, "test")
	if err != nil {
		t.Fatalf("resolution: %v", err)
	}
	// The openai-only hop serves NATIVELY here — it speaks this surface.
	if len(routes) != 2 {
		t.Fatalf("candidates = %d, want 2", len(routes))
	}
	for i, rt := range routes {
		if rt.Translated {
			t.Errorf("route %d translated on the openai surface: %+v", i, rt)
		}
	}
}
