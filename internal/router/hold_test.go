package router

import (
	"testing"

	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// hold_until_failure (operator decision 2026-10-13): in block_round_robin a
// hop that the operator checked keeps its block while it succeeds — its
// dispatches do NOT consume the block, so it never rotates on schedule — and
// releases on a hop-attributable failure. Unchecked hops keep today's exact
// N-dispatch rotation.

// isolate clears the package-level block cursor for a combo id so tests that
// reuse ids cannot leak state into one another.
func isolate(id int64) { blockCursors.Delete(id) }

func TestBlockHoldNeverRotatesWhileHealthy(t *testing.T) {
	isolate(401)
	combo := &store.Combo{ID: 401, Name: "hold1", Strategy: "block_round_robin", BlockSize: 2}
	routes := []proxy.Route{hop(1, "m"), hop(2, "m")}
	routes[0].HoldUntilFailure = true
	for i := 0; i < 6; i++ {
		ordered, pos := orderRoutes(combo, routes)
		if ordered[0].Account.ID != 1 {
			t.Fatalf("dispatch %d: served hop %d, want the held hop 1", i, ordered[0].Account.ID)
		}
		if pos != 1 {
			t.Fatalf("dispatch %d: block pos = %d, want 1 (a held hop's dispatches must not sink)", i, pos)
		}
	}
}

func TestBlockHoldReleasesOnFailure(t *testing.T) {
	isolate(402)
	combo := &store.Combo{ID: 402, Name: "hold2", Strategy: "block_round_robin", BlockSize: 2}
	routes := []proxy.Route{hop(1, "m"), hop(2, "m")}
	routes[0].HoldUntilFailure = true
	if ordered, _ := orderRoutes(combo, routes); ordered[0].Account.ID != 1 {
		t.Fatalf("held hop must serve first: %+v", ordered)
	}
	r := New(withCombo(newFake(), combo))
	r.MarkHopBad("hold2", routes[0])
	ordered, _ := orderRoutes(combo, routes)
	if ordered[0].Account.ID != 2 {
		t.Fatalf("after a hop-attributable failure the block must rotate: served hop %d", ordered[0].Account.ID)
	}
	// And it rotated, not skipped: the released hop is now a normal candidate
	// later in the rotation.
	ordered2, _ := orderRoutes(combo, routes)
	if ordered2[0].Account.ID != 2 {
		t.Fatalf("the next hop should take its own block: %+v", ordered2)
	}
}

// A failure reported for an UNCHECKED hop does nothing: its rotation is purely
// the dispatch count, and releasing it would fabricate a health signal the
// operator never asked for.
func TestMarkHopBadIgnoresUnheldHop(t *testing.T) {
	isolate(403)
	combo := &store.Combo{ID: 403, Name: "hold3", Strategy: "block_round_robin", BlockSize: 3}
	routes := []proxy.Route{hop(1, "m"), hop(2, "m")}
	if _, pos := orderRoutes(combo, routes); pos != 1 {
		t.Fatalf("cold block should start at pos 1, got %d", pos)
	}
	r := New(withCombo(newFake(), combo))
	r.MarkHopBad("hold3", routes[0])
	// Normal decrement: the second dispatch is still inside hop 1's block.
	ordered, pos := orderRoutes(combo, routes)
	if pos != 2 || ordered[0].Account.ID != 1 {
		t.Fatalf("unchecked hop keeps its block: pos=%d hop=%d", pos, ordered[0].Account.ID)
	}
}

// The direct-route guard mirrors MarkHopGood: only a combo route rotates.
func TestMarkHopBadIgnoresDirectRoute(t *testing.T) {
	isolate(404)
	combo := &store.Combo{ID: 404, Name: "hold4", Strategy: "block_round_robin", BlockSize: 2}
	routes := []proxy.Route{hop(1, "m")}
	r := New(withCombo(newFake(), combo))
	r.MarkHopBad("ns/model", routes[0]) // must not panic, must not touch state
	r.MarkHopBad("", routes[0])
	if ordered, _ := orderRoutes(combo, routes); ordered[0].Account.ID != 1 {
		t.Fatalf("direct routes do not rotate blocks: %+v", ordered)
	}
}
