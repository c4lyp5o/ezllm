package router

import (
	"context"
	"errors"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// A pin is enforced, never rerouted: a client on the wrong surface gets a
// typed refusal naming the pinned surface, and the path is never changed.
func TestPinnedDirectRouteRefusesWrongSurface(t *testing.T) {
	cat := newFake()
	cat.pins = map[string]string{"2/gpt-6-luna": "responses"}
	r := New(cat)

	_, err := r.Resolve(context.Background(), "super-ssn/gpt-6-luna", provider.SurfaceOpenAI, "c")
	var np *ErrPinned
	if !errors.As(err, &np) {
		t.Fatalf("want *ErrPinned for openai call on responses-pinned model, got %v", err)
	}
	if np.Pinned != "responses" || np.Got != "openai" {
		t.Errorf("pin refusal = pinned %q got %q, want responses/openai", np.Pinned, np.Got)
	}
}

// The right surface passes through unchanged; the pin does not alter the route.
func TestPinnedDirectRouteAcceptsPinnedSurface(t *testing.T) {
	cat := newFake()
	cat.pins = map[string]string{"2/gpt-6-luna": "responses"}
	r := New(cat)

	rt, err := r.Resolve(context.Background(), "super-ssn/gpt-6-luna", provider.SurfaceResponses, "c")
	if err != nil {
		t.Fatalf("pinned surface should route: %v", err)
	}
	if rt.Surface != provider.SurfaceResponses {
		t.Errorf("surface = %v, want responses (pin must not reroute)", rt.Surface)
	}
}

// An unpinned model keeps today's behaviour on every surface.
func TestUnpinnedModelUnaffected(t *testing.T) {
	r := New(newFake())
	for _, s := range []provider.Surface{provider.SurfaceOpenAI, provider.SurfaceAnthropic, provider.SurfaceResponses} {
		if _, err := r.Resolve(context.Background(), "super-ssn/gpt-6-luna", s, "c"); err != nil {
			t.Errorf("unpinned model on %v: %v", s, err)
		}
	}
}

// In a combo a pinned hop on the wrong surface refuses the whole request.
// It must NOT be skipped, because the next hop could change protocol.
func TestPinnedComboHopRefusesInsteadOfSkipping(t *testing.T) {
	cat := withCombo(newFake(), dayNightCombo())
	cat.pins = map[string]string{"1/gpt-6-luna": "responses"}
	r := New(cat)

	_, err := r.ResolveCandidates(context.Background(), "daily", provider.SurfaceOpenAI, "c")
	var np *ErrPinned
	if !errors.As(err, &np) {
		t.Fatalf("combo with pinned hop on wrong surface: want *ErrPinned, got %v", err)
	}
}

// A lookup failure must refuse, not fall through: a pin is an operator
// guarantee and a transient DB error must not silently bypass it.
type failingPinCatalog struct{ *fakeCatalog }

func (f failingPinCatalog) ModelPin(_ context.Context, _ int64, _ string) (string, error) {
	return "", errors.New("db busy")
}

func TestPinLookupFailureFailsClosed(t *testing.T) {
	r := New(failingPinCatalog{newFake()})
	if _, err := r.Resolve(context.Background(), "super-ssn/gpt-6-luna", provider.SurfaceOpenAI, "c"); err == nil {
		t.Fatal("pin lookup error must refuse the request, got nil")
	}
}
