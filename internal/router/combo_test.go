package router

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// boolPtr builds a *bool for ComboHop.Enabled.
func boolPtr(b bool) *bool { return &b }

// dayNightCombo mirrors the real use case: one combo holding a day model and a
// night model so the caller never has to pick.
func dayNightCombo() *store.Combo {
	return &store.Combo{
		ID: 7, Name: "daily", Strategy: "failover", Enabled: true,
		Hops: []store.ComboHop{
			{AccountID: 1, ModelID: "gpt-6-luna", Weight: 1},
			{AccountID: 2, ModelID: "gpt-6-luna", Weight: 1},
		},
	}
}

func withCombo(f *fakeCatalog, c *store.Combo) *fakeCatalog {
	f.combos = map[string]*store.Combo{c.Name: c}
	return f
}

// A bare combo name must resolve — this is the path that previously 404'd with
// "combo name (M4) not implemented".
func TestResolveComboBareName(t *testing.T) {
	cat := withCombo(newFake(), dayNightCombo())
	r := New(cat)

	routes, err := r.ResolveCandidates(context.Background(), "daily", provider.SurfaceOpenAI, "test")
	if err != nil {
		t.Fatalf("combo resolution: %v", err)
	}
	if len(routes) != 2 {
		t.Fatalf("candidates = %d, want 2", len(routes))
	}
	if routes[0].Alias != "daily" {
		t.Errorf("alias = %q, want \"daily\" (ledger groups by alias)", routes[0].Alias)
	}
	if routes[0].Account.ID != 1 || routes[1].Account.ID != 2 {
		t.Errorf("hop order = %d,%d want 1,2 (position order for failover)", routes[0].Account.ID, routes[1].Account.ID)
	}
	// Each route must carry a usable key, or Forward will fail at attempt time.
	for i, rt := range routes {
		if rt.KeyPlain == "" {
			t.Errorf("route %d has no key", i)
		}
	}
}

// Resolve (the Rewriter contract) returns the FIRST candidate, so existing
// single-route callers keep working unchanged.
func TestResolveReturnsFirstCandidate(t *testing.T) {
	cat := withCombo(newFake(), dayNightCombo())
	r := New(cat)

	rt, err := r.Resolve(context.Background(), "daily", provider.SurfaceOpenAI, "test")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rt.Account.ID != 1 {
		t.Errorf("first candidate account = %d, want 1", rt.Account.ID)
	}
}

// THE day/night mechanism: an ineligible hop is DROPPED, not an error. The
// remaining hop must still serve the request.
func TestIneligibleHopIsSkippedNotFatal(t *testing.T) {
	cat := withCombo(newFake(), dayNightCombo())
	r := New(cat).WithEligibility(func(_ context.Context, accountID int64, modelID string) error {
		if accountID == 1 {
			return errors.New("outside allowed hours 22:00-06:00")
		}
		return nil
	})

	routes, err := r.ResolveCandidates(context.Background(), "daily", provider.SurfaceOpenAI, "test")
	if err != nil {
		t.Fatalf("combo must still resolve when one hop is out of window: %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("candidates = %d, want 1 (hop 1 dropped)", len(routes))
	}
	if routes[0].Account.ID != 2 {
		t.Errorf("surviving hop = account %d, want 2 (the in-window night hop)", routes[0].Account.ID)
	}
}

// When EVERY hop is ineligible the error must name a real cause — "no usable
// hops: <reason>" beats a bare "no candidates".
func TestAllHopsIneligibleExplainsWhy(t *testing.T) {
	cat := withCombo(newFake(), dayNightCombo())
	r := New(cat).WithEligibility(func(context.Context, int64, string) error {
		return errors.New("daily cap of 500k tokens spent")
	})

	_, err := r.ResolveCandidates(context.Background(), "daily", provider.SurfaceOpenAI, "test")
	if err == nil {
		t.Fatal("expected an error when no hop is eligible")
	}
	if !strings.Contains(err.Error(), "cap of 500k tokens spent") {
		t.Errorf("error must carry the cause, got: %v", err)
	}
}

// A disabled hop is skipped; a combo with NO hops is an error.
func TestComboSkipsDisabledHops(t *testing.T) {
	c := dayNightCombo()
	c.Hops[1].Enabled = boolPtr(false)
	cat := withCombo(newFake(), c)
	r := New(cat)

	routes, err := r.ResolveCandidates(context.Background(), "daily", provider.SurfaceOpenAI, "test")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(routes) != 1 || routes[0].Account.ID != 1 {
		t.Fatalf("candidates = %d (first=%d), want only hop 1", len(routes), routes[0].Account.ID)
	}
}

// An unknown bare name 404s with hints that include EXISTING combo names —
// otherwise the message can't distinguish "typo'd combo" from "no such thing".
func TestUnknownComboHintsIncludeCombos(t *testing.T) {
	cat := withCombo(newFake(), dayNightCombo())
	r := New(cat)

	_, err := r.ResolveCandidates(context.Background(), "dialy", provider.SurfaceOpenAI, "test")
	var nf *ErrNotFound
	if !errors.As(err, &nf) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	found := false
	for _, h := range nf.Hints {
		if h == "daily" {
			found = true
		}
	}
	if !found {
		t.Errorf("hints %v must include the real combo name", nf.Hints)
	}
}

// Direct ns/model routes keep behaving exactly as before: exactly one
// candidate, and an ineligible model is a HARD stop (ErrUnavailable → 429),
// because there is nothing to fail over to.
func TestDirectRouteSingleCandidate(t *testing.T) {
	r := New(newFake())

	routes, err := r.ResolveCandidates(context.Background(), "opengo/qwen3.8-flash", provider.SurfaceOpenAI, "test")
	if err != nil {
		t.Fatalf("direct resolve: %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("candidates = %d, want 1", len(routes))
	}
	if routes[0].Model != "qwen3.8-flash" {
		t.Errorf("model = %q", routes[0].Model)
	}
}

func TestDirectRouteIneligibleIsUnavailable(t *testing.T) {
	r := New(newFake()).WithEligibility(func(context.Context, int64, string) error {
		return errors.New("daily cap spent")
	})

	_, err := r.ResolveCandidates(context.Background(), "opengo/qwen3.8-flash", provider.SurfaceOpenAI, "test")
	var nu *ErrUnavailable
	if !errors.As(err, &nu) {
		t.Fatalf("want ErrUnavailable (maps to 429), got %v", err)
	}
	if !strings.Contains(nu.Reason, "daily cap spent") {
		t.Errorf("reason = %q, must carry the rule that fired", nu.Reason)
	}
}

// Round-robin must actually rotate. A cursor that never advances is a
// decorative strategy, so pin the behaviour: consecutive calls start at
// different hops.
func TestRoundRobinRotates(t *testing.T) {
	c := dayNightCombo()
	c.Strategy = "strict_round_robin"
	cat := withCombo(newFake(), c)
	r := New(cat)
	ctx := context.Background()

	first := map[int64]bool{}
	for i := 0; i < 4; i++ {
		routes, err := r.ResolveCandidates(ctx, "daily", provider.SurfaceOpenAI, "test")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		first[routes[0].Account.ID] = true
	}
	if len(first) < 2 {
		t.Errorf("round-robin always started at the same hop (saw %v) — cursor not advancing", first)
	}
}

// An unknown strategy must not silently break routing: default to position
// order (failover) rather than returning nothing.
func TestUnknownStrategyFallsBackToPositionOrder(t *testing.T) {
	c := dayNightCombo()
	c.Strategy = "something_new"
	cat := withCombo(newFake(), c)
	r := New(cat)

	routes, err := r.ResolveCandidates(context.Background(), "daily", provider.SurfaceOpenAI, "test")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(routes) != 2 || routes[0].Account.ID != 1 {
		t.Errorf("want position order, got %d routes starting at %d", len(routes), routes[0].Account.ID)
	}
}

// A hop whose model is not in that account's catalog is dropped rather than
// sent upstream to burn a call on an inevitable 404.
func TestHopWithUnknownModelIsDropped(t *testing.T) {
	c := dayNightCombo()
	c.Hops[0].ModelID = "not-a-real-model"
	cat := withCombo(newFake(), c)
	r := New(cat)

	routes, err := r.ResolveCandidates(context.Background(), "daily", provider.SurfaceOpenAI, "test")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(routes) != 1 || routes[0].Account.ID != 2 {
		t.Fatalf("want only the valid hop, got %d routes", len(routes))
	}
}

// A combo whose account was disabled entirely must fail with a message naming
// that account, not a generic "no candidates".
func TestComboAllHopsDisabledAccountNamesCause(t *testing.T) {
	c := dayNightCombo()
	c.Hops = []store.ComboHop{{AccountID: 3, ModelID: "gpt-6-luna"}} // account 3 disabled
	cat := withCombo(newFake(), c)
	r := New(cat)

	_, err := r.ResolveCandidates(context.Background(), "daily", provider.SurfaceOpenAI, "test")
	if err == nil {
		t.Fatal("expected error: every hop points at a disabled account")
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Errorf("error must name the cause, got: %v", err)
	}
}
