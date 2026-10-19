package router

import (
	"context"
	"errors"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// countingCatalog counts the per-hop catalog reads resolution makes. It has no
// ModelFacts, so wire selection asks one question at a time.
type countingCatalog struct {
	*fakeCatalog
	acctByID, pickKey, pin, supports int
}

func (c *countingCatalog) AccountByID(ctx context.Context, id int64) (*provider.Account, error) {
	c.acctByID++
	return c.fakeCatalog.AccountByID(ctx, id)
}

func (c *countingCatalog) PickKey(ctx context.Context, id int64) (store.KeyPick, error) {
	c.pickKey++
	return c.fakeCatalog.PickKey(ctx, id)
}

func (c *countingCatalog) ModelPin(ctx context.Context, id int64, m string) (string, error) {
	c.pin++
	return c.fakeCatalog.ModelPin(ctx, id, m)
}

func (c *countingCatalog) ModelSupportsSurface(ctx context.Context, id int64, m string, s provider.Surface) (*bool, error) {
	c.supports++
	return c.fakeCatalog.ModelSupportsSurface(ctx, id, m, s)
}

// factsCatalog adds the one-row read the real store offers.
type factsCatalog struct {
	*countingCatalog
	facts int
}

func (c *factsCatalog) ModelFacts(ctx context.Context, id int64, m string) (store.ModelFacts, error) {
	c.facts++
	pin, _ := c.fakeCatalog.ModelPin(ctx, id, m)
	f := store.ModelFacts{Pin: pin}
	f.OpenAI, _ = c.fakeCatalog.ModelSupportsSurface(ctx, id, m, provider.SurfaceOpenAI)
	f.Anthropic, _ = c.fakeCatalog.ModelSupportsSurface(ctx, id, m, provider.SurfaceAnthropic)
	f.Responses, _ = c.fakeCatalog.ModelSupportsSurface(ctx, id, m, provider.SurfaceResponses)
	return f, nil
}

func sharedAccountCombo(f *fakeCatalog) {
	f.combos = map[string]*store.Combo{"mix": {
		Name: "mix", Strategy: "failover", Enabled: true,
		Hops: []store.ComboHop{
			{AccountID: 1, ModelID: "qwen3.8-flash"},
			{AccountID: 1, ModelID: "gpt-6-luna"},
			{AccountID: 2, ModelID: "gpt-6-luna"},
		},
	}}
}

// Hops sharing an account read that account and its key once per resolution,
// and a catalog offering ModelFacts answers wire selection in one read per hop.
func TestComboResolutionReadsEachAccountOnce(t *testing.T) {
	f := newFake()
	sharedAccountCombo(f)
	c := &factsCatalog{countingCatalog: &countingCatalog{fakeCatalog: f}}
	routes, err := New(c).ResolveCandidates(context.Background(), "mix", provider.SurfaceOpenAI, "t")
	if err != nil || len(routes) != 3 {
		t.Fatalf("routes = %d, err = %v; want 3", len(routes), err)
	}
	if c.acctByID != 2 || c.pickKey != 2 {
		t.Errorf("AccountByID = %d, PickKey = %d; want 2 each (one per account)", c.acctByID, c.pickKey)
	}
	if c.facts != 3 || c.pin != 0 || c.supports != 0 {
		t.Errorf("ModelFacts = %d, ModelPin = %d, ModelSupportsSurface = %d; want 3, 0, 0", c.facts, c.pin, c.supports)
	}
	if routes[0].KeyPlain != "sk-opencode-secret" || routes[2].KeyPlain != "sk-ssn-secret" {
		t.Errorf("memoized keys went to the wrong hops: %q, %q", routes[0].KeyPlain, routes[2].KeyPlain)
	}
}

// Without ModelFacts, a question selectWire asks twice is still read once: the
// universal pin check and the destination's checkPin share one ModelPin.
func TestWireSelectionReadsThePinOnce(t *testing.T) {
	f := newFake()
	f.noProto = map[string]bool{"1/gpt-6-luna/anthropic": true}
	f.yesProto = map[string]bool{"1/gpt-6-luna/responses": true}
	c := &countingCatalog{fakeCatalog: f}
	r := New(c).WithTranslation(true).WithUniversal(true)
	routes, err := r.ResolveCandidates(context.Background(), "opengo/gpt-6-luna", provider.SurfaceAnthropic, "t")
	if err != nil {
		t.Fatal(err)
	}
	if routes[0].WireSurface != provider.SurfaceResponses {
		t.Fatalf("wire = %q, want responses", routes[0].WireSurface)
	}
	if c.pin != 1 {
		t.Errorf("ModelPin reads = %d, want 1", c.pin)
	}
}

// brokenSnapshotCatalog offers a snapshot that cannot be read.
type brokenSnapshotCatalog struct{ *countingCatalog }

func (brokenSnapshotCatalog) CatalogSnapshot(context.Context) (*store.CatalogSnapshot, error) {
	return nil, errors.New("snapshot unavailable")
}

// A snapshot read failure must not fail the request: resolution falls back to
// the live catalog, whose own errors keep their fail-closed meaning.
func TestUnreadableSnapshotFallsBackToLiveCatalog(t *testing.T) {
	c := brokenSnapshotCatalog{&countingCatalog{fakeCatalog: newFake()}}
	routes, err := New(c).ResolveCandidates(context.Background(), "opengo/gpt-6-luna", provider.SurfaceOpenAI, "t")
	if err != nil || len(routes) != 1 {
		t.Fatalf("routes = %d, err = %v; want 1 via the live catalog", len(routes), err)
	}
	if c.pickKey != 1 {
		t.Errorf("live PickKey calls = %d, want 1", c.pickKey)
	}
}
