package router

import (
	"context"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// multiKeyCatalog is the fake catalog plus PickKeys: account 1 ("demo") has
// three keys, account 2 ("initech") one.
type multiKeyCatalog struct {
	*fakeCatalog
	multi map[int64][]store.KeyPick
}

func (m *multiKeyCatalog) PickKeys(_ context.Context, id int64) ([]store.KeyPick, error) {
	if k, ok := m.multi[id]; ok {
		return k, nil
	}
	return nil, store.ErrNotFound
}

func newMultiKey(c *store.Combo) *multiKeyCatalog {
	f := newFake()
	if c != nil {
		f = withCombo(f, c)
	}
	return &multiKeyCatalog{fakeCatalog: f, multi: map[int64][]store.KeyPick{
		1: {{ID: 11, Hint: "a"}, {ID: 12, Hint: "b"}, {ID: 13, Hint: "c"}},
		2: {{ID: 22, Hint: "z"}},
	}}
}

func keyIDs(t *testing.T, r *Resolver, requested string) []int64 {
	t.Helper()
	routes, err := r.ResolveCandidates(context.Background(), requested, provider.SurfaceChat, "")
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, len(routes))
	for i, rt := range routes {
		ids[i] = rt.KeyID
		if rt.KeyHint == "" {
			t.Errorf("route %d has no key hint: %+v", i, rt)
		}
	}
	return ids
}

// A hop on an account with three keys becomes three candidates, one per key,
// kept next to each other in the strategy's order; the first key rotates on
// every request.
func TestComboHopExpandsToEveryKeyAndRotates(t *testing.T) {
	c := &store.Combo{ID: 91, Name: "rot", Strategy: "failover", Enabled: true, Hops: []store.ComboHop{
		{AccountID: 1, ModelID: "gpt-6-luna"}, {AccountID: 2, ModelID: "gpt-6-luna"},
	}}
	r := New(newMultiKey(c))
	first := keyIDs(t, r, "rot")
	if len(first) != 4 || first[3] != 22 {
		t.Fatalf("candidates = %v, want 3 demo keys then initech's 22", first)
	}
	starts := map[int64]bool{}
	prev := first
	for i := 0; i < 3; i++ {
		starts[prev[0]] = true
		next := keyIDs(t, r, "rot")
		// Each request starts one key further on, wrapping around.
		if next[0] != prev[1] || next[1] != prev[2] || next[2] != prev[0] {
			t.Fatalf("request %d keys = %v after %v: not rotated by one", i+1, next, prev)
		}
		prev = next
	}
	if len(starts) != 3 {
		t.Fatalf("first keys over three requests = %v, want all three", starts)
	}
}

// Two hops on the same account start on the same key in one request: one turn
// per account per request, not one per hop.
func TestKeysOneTurnPerAccountPerRequest(t *testing.T) {
	c := &store.Combo{ID: 92, Name: "two", Strategy: "failover", Enabled: true, Hops: []store.ComboHop{
		{AccountID: 1, ModelID: "gpt-6-luna"}, {AccountID: 1, ModelID: "qwen3.8-flash"},
	}}
	r := New(newMultiKey(c))
	ids := keyIDs(t, r, "two")
	if len(ids) != 6 || ids[0] != ids[3] {
		t.Fatalf("keys = %v, want both hops starting on the same key", ids)
	}
}

// A direct route expands too.
func TestDirectRouteExpandsKeys(t *testing.T) {
	r := New(newMultiKey(nil))
	if ids := keyIDs(t, r, "demo/gpt-6-luna"); len(ids) != 3 {
		t.Fatalf("direct candidates = %v, want one per key", ids)
	}
	if ids := keyIDs(t, r, "initech/gpt-6-luna"); len(ids) != 1 || ids[0] != 22 {
		t.Fatalf("one-key direct = %v", ids)
	}
}

// Explain lists each hop once with its key count and moves no key turn.
func TestExplainCountsKeysWithoutRotating(t *testing.T) {
	c := &store.Combo{ID: 93, Name: "ex", Strategy: "failover", Enabled: true, Hops: []store.ComboHop{
		{AccountID: 1, ModelID: "gpt-6-luna"}, {AccountID: 2, ModelID: "gpt-6-luna"},
	}}
	r := New(newMultiKey(c))
	before := keyIDs(t, r, "ex")
	for i := 0; i < 2; i++ {
		ex := r.Explain(context.Background(), "ex", provider.SurfaceChat)
		if len(ex.Hops) != 2 || ex.Hops[0].KeyCount != 3 || ex.Hops[1].KeyCount != 1 {
			t.Fatalf("explain hops = %+v", ex.Hops)
		}
	}
	after := keyIDs(t, r, "ex")
	if after[0] != before[1] {
		t.Fatalf("explain moved the key turn: %v then %v", before, after)
	}
}

// Explaining a direct route lists it once, with its key count, and moves no
// key turn either.
func TestExplainDirectRouteListsOnce(t *testing.T) {
	r := New(newMultiKey(nil))
	before := keyIDs(t, r, "demo/qwen3.8-flash")
	ex := r.Explain(context.Background(), "demo/qwen3.8-flash", provider.SurfaceChat)
	if len(ex.Hops) != 1 || ex.Hops[0].KeyCount != 3 {
		t.Fatalf("direct explain hops = %+v, want one hop with 3 keys", ex.Hops)
	}
	if after := keyIDs(t, r, "demo/qwen3.8-flash"); after[0] != before[1] {
		t.Fatalf("explain moved the key turn: %v then %v", before, after)
	}
}
