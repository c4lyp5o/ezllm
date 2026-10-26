package router

import (
	"context"
	"errors"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// A translated hop must satisfy the actual OpenAI wire, not bypass pins.
func TestTranslatedHopChecksActualWirePin(t *testing.T) {
	for _, pin := range []string{"", "chat", "anthropic", "responses"} {
		t.Run(pin, func(t *testing.T) {
			cat, r := translatedFixture()
			cat.pins = map[string]string{"2/gpt-6.1-sol": pin}
			routes, err := r.ResolveCandidates(context.Background(), "daily", provider.SurfaceAnthropic, "test")
			if pin == "anthropic" || pin == "responses" {
				var refusal *ErrPinned
				if !errors.As(err, &refusal) {
					t.Fatalf("want pin refusal, got %v", err)
				}
				if refusal.Requested != "daily" || refusal.Pinned != pin || refusal.Got != "chat" || len(routes) != 0 {
					t.Fatalf("wrong refusal: %+v; candidate count = %d", refusal, len(routes))
				}
				return
			}
			if err != nil {
				t.Fatalf("OpenAI wire satisfies pin %q: %v", pin, err)
			}
			if len(routes) != 2 || routes[0].Translated || !routes[1].Translated || routes[1].Account.ID != 2 || routes[1].Model != "gpt-6.1-sol" {
				t.Fatal("want native first and pinned translated tail")
			}
			if routes[1].Surface != provider.SurfaceAnthropic || routes[1].Alias != "daily" || routes[1].ResponsesWire {
				t.Fatal("pin check must not change the client's surface, alias, or wire override")
			}
		})
	}
}

// With translation OFF, a proven-incompatible hop is still dropped before
// its pin is considered; an untested native hop still hard-refuses the combo.
func TestTranslatedPinDisabledPreservesNativeGate(t *testing.T) {
	for _, provenUnsupported := range []bool{true, false} {
		t.Run(map[bool]string{true: "proven-unsupported", false: "untested"}[provenUnsupported], func(t *testing.T) {
			cat, _ := translatedFixture()
			cat.pins = map[string]string{"2/gpt-6.1-sol": "chat"}
			if !provenUnsupported {
				cat.noProto = nil
			}
			routes, err := New(cat).ResolveCandidates(context.Background(), "daily", provider.SurfaceAnthropic, "test")
			if provenUnsupported {
				if err != nil || len(routes) != 1 || routes[0].Translated || routes[0].Model != "gpt-6-luna" {
					t.Fatalf("disabled translation must drop incompatible hop: %v", err)
				}
				return
			}
			var refusal *ErrPinned
			if !errors.As(err, &refusal) {
				t.Fatalf("want native pin refusal, got %v", err)
			}
			const want = "model \"daily\" is pinned to the chat surface; this request used anthropic — call it on the chat endpoint instead"
			if refusal.Error() != want || refusal.Requested != "daily" || refusal.Pinned != "chat" || refusal.Got != "anthropic" || len(routes) != 0 {
				t.Fatalf("native refusal changed: %+v", refusal)
			}
		})
	}
}

func TestTranslatedPinMatchingNativeSurfaceUnchanged(t *testing.T) {
	cat, r := translatedFixture()
	cat.pins = map[string]string{"2/gpt-6.1-sol": "chat"}
	routes, err := r.ResolveCandidates(context.Background(), "daily", provider.SurfaceChat, "test")
	if err != nil || len(routes) != 2 {
		t.Fatalf("matching native surface must route: %v", err)
	}
	for _, rt := range routes {
		if rt.Translated || rt.Surface != provider.SurfaceChat {
			t.Fatal("matching native route changed")
		}
	}
}

func TestTranslatedPinLookupFailureFailsClosed(t *testing.T) {
	cat, _ := translatedFixture()
	r := New(failingPinCatalog{cat}).WithTranslation(true)
	// Remove the native hop so the failing lookup is on the translated hop.
	cat.accounts["opengo"].Enabled = false
	routes, err := r.ResolveCandidates(context.Background(), "daily", provider.SurfaceAnthropic, "test")
	if err == nil || err.Error() != "model pin lookup for \"daily\": db busy" || len(routes) != 0 {
		t.Fatalf("translated route must not bypass a pin lookup error: %v", err)
	}
}
