package router

// F7 regression: the model-existence check used to fail OPEN —
//
//	if ok, err := r.catalog.ModelExists(...); err == nil && !ok { ... }
//
// A store error therefore SKIPPED the existence check entirely, so a typo'd
// model went upstream and burned a call on every attempt until the database
// recovered. checkPin above already fails closed for the same reason: an
// operator decision must not be bypassed by a transient error, and a catalog
// lookup is no different.

import (
	"context"
	"errors"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// A ModelExists that errors must refuse the route: no route, no upstream call.
type deadCatalog struct{ *fakeCatalog }

func (d deadCatalog) ModelExists(_ context.Context, _ int64, _ string) (bool, error) {
	return false, errors.New("db busy")
}

func TestModelLookupFailureFailsClosed(t *testing.T) {
	r := New(deadCatalog{newFake()})
	rt, err := r.Resolve(context.Background(), "super-ssn/gpt-6-luna", provider.SurfaceOpenAI, "c")
	if err == nil {
		t.Fatal("model lookup error must refuse the request, got nil — a store error would skip the existence check and burn an upstream call")
	}
	if rt.Account.Namespace != "" {
		t.Fatalf("no route may be returned alongside the error, got %+v", rt.Account)
	}
	if !contains(err.Error(), "model lookup") {
		t.Fatalf("refusal must name the cause, got: %v", err)
	}
}

// The same must hold on the candidates path used by combos and failover, so a
// dead store cannot turn into an upstream call there either.
func TestModelLookupFailureFailsClosedOnCandidates(t *testing.T) {
	r := New(deadCatalog{newFake()})
	routes, err := r.ResolveCandidates(context.Background(), "super-ssn/gpt-6-luna", provider.SurfaceOpenAI, "c")
	if err == nil {
		t.Fatalf("candidates path must refuse on a lookup error, got %d routes", len(routes))
	}
	if len(routes) != 0 {
		t.Fatalf("refusal must carry no routes, got %d", len(routes))
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
