package router

import (
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// hop builds a minimal route for ordering tests; only the account id and model
// identity matter to the strategies.
func hop(id int64, model string) proxy.Route {
	return proxy.Route{Account: provider.Account{ID: id, Namespace: "ns"}, Model: model}
}

// blockHeads runs n resolves and returns the account id leading each one.
func blockHeads(combo *store.Combo, routes []proxy.Route, n int) []int64 {
	out := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		ordered := blockOrder(combo, routes)
		out = append(out, ordered[0].Account.ID)
	}
	return out
}

func eqIDs(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// The core claim: one hop serves block_size consecutive dispatches, then the
// rotation advances. This is what keeps a provider's prompt cache warm across an
// agent tool loop instead of striping every request to a different account.
func TestBlockRoundRobinServesConsecutiveBlocks(t *testing.T) {
	combo := &store.Combo{ID: 101, Name: "blocks", Strategy: "block_round_robin", BlockSize: 3}
	routes := []proxy.Route{hop(1, "m"), hop(2, "m")}

	got := blockHeads(combo, routes, 6)
	want := []int64{1, 1, 1, 2, 2, 2}
	if !eqIDs(got, want) {
		t.Fatalf("hop sequence = %v, want %v", got, want)
	}
	// And it keeps rotating rather than parking on the last hop.
	if got := blockHeads(combo, routes, 3); !eqIDs(got, []int64{1, 1, 1}) {
		t.Errorf("second lap = %v, want the rotation to wrap to hop 1", got)
	}
}

// block_size 1 must degenerate to plain per-request round robin, so the new
// strategy cannot be mistaken for something different at its smallest setting.
func TestBlockSizeOneIsPerRequestRotation(t *testing.T) {
	combo := &store.Combo{ID: 102, Name: "trr", Strategy: "block_round_robin", BlockSize: 1}
	routes := []proxy.Route{hop(1, "m"), hop(2, "m")}

	got := blockHeads(combo, routes, 4)
	want := []int64{1, 2, 1, 2}
	if !eqIDs(got, want) {
		t.Fatalf("hop sequence = %v, want %v", got, want)
	}
}

// A zero block_size (a row written before the column existed, or a hand-edited
// value) must behave as 1 rather than producing an empty rotation.
func TestBlockSizeZeroTreatedAsOne(t *testing.T) {
	combo := &store.Combo{ID: 103, Name: "zero", Strategy: "block_round_robin", BlockSize: 0}
	routes := []proxy.Route{hop(1, "m"), hop(2, "m")}
	if got := blockHeads(combo, routes, 2); !eqIDs(got, []int64{1, 2}) {
		t.Errorf("hop sequence = %v, want per-request rotation", got)
	}
}

// The block's hop goes FIRST and everything else keeps position order behind it,
// so the dispatcher can still fail over inside a block. Ordering the tail
// wrongly would silently break failover for every block-round-robin combo.
func TestBlockOrderKeepsFailoverTail(t *testing.T) {
	combo := &store.Combo{ID: 104, Name: "tail", Strategy: "block_round_robin", BlockSize: 2}
	routes := []proxy.Route{hop(1, "m"), hop(2, "m"), hop(3, "m")}

	blockOrder(combo, routes) // hop 1, slot 1
	blockOrder(combo, routes) // hop 1, block spent
	ordered := blockOrder(combo, routes)
	if ordered[0].Account.ID != 2 {
		t.Fatalf("new block starts at hop %d, want hop 2", ordered[0].Account.ID)
	}
	// Hop 2 leads, then 3, then 1 — position order preserved behind the target.
	if !eqIDs([]int64{ordered[0].Account.ID, ordered[1].Account.ID, ordered[2].Account.ID}, []int64{2, 3, 1}) {
		t.Errorf("order = %d,%d,%d, want 2,3,1",
			ordered[0].Account.ID, ordered[1].Account.ID, ordered[2].Account.ID)
	}
}

// A hop that drops out of the eligible list mid-block must not silently re-point
// the block at whatever now sits at the same index. The cursor is hop IDENTITY,
// so a vanished target starts a fresh block at the top of the list.
func TestBlockTargetLeavingEligibleListStartsFreshBlock(t *testing.T) {
	combo := &store.Combo{ID: 105, Name: "vanish", Strategy: "block_round_robin", BlockSize: 3}
	full := []proxy.Route{hop(1, "m"), hop(2, "m"), hop(3, "m")}
	blockOrder(combo, full) // hop 1, remaining 2
	blockOrder(combo, full) // hop 1, remaining 1

	// Hop 1 leaves (out of budget, cooling down, catalog change).
	reduced := []proxy.Route{hop(2, "m"), hop(3, "m")}
	// The first call after the target vanishes starts a FRESH block at the top
	// of the list, and that block is BlockSize long — so three of these four are
	// hop 2, then the rotation advances. Not the remainder of hop 1's block, and
	// not a hop chosen by index arithmetic.
	got := blockHeads(combo, reduced, 4)
	if !eqIDs(got, []int64{2, 2, 2, 3}) {
		t.Errorf("after the target vanished = %v, want 2,2,2,3 (a fresh full block on hop 2)", got)
	}
}

// A single-hop combo has nothing to rotate: ordering must leave it alone.
func TestBlockOrderSingleHopUnchanged(t *testing.T) {
	combo := &store.Combo{ID: 106, Name: "solo", Strategy: "block_round_robin", BlockSize: 5}
	routes := []proxy.Route{hop(9, "m")}
	if ordered := blockOrder(combo, routes); len(ordered) != 1 || ordered[0].Account.ID != 9 {
		t.Errorf("single hop was reordered: %v", ordered)
	}
}

// ── sticky_last_good: the idle window must actually apply ───────────────────

// Regression: the entry's timestamp was written and never read, so
// sticky_last_good pinned the first hop that ever answered forever and
// sticky_idle_s had no effect at all. A stale entry must be released.
func TestStickyReleasesAfterIdleWindow(t *testing.T) {
	routes := []proxy.Route{hop(1, "m"), hop(2, "m")}
	combo := &store.Combo{ID: 201, Name: "sticky", Strategy: "sticky_last_good", StickyIdleS: 30}

	// Hop 2 answered two minutes ago: past the 30s window.
	stickyLast.Store(combo.ID, stickyEntry{route: routes[1], at: time.Now().Add(-2 * time.Minute)})
	ordered := stickyOrder(combo, routes)
	if ordered[0].Account.ID != 1 {
		t.Errorf("stale sticky hop still leads (%d) — sticky_idle_s is not being honoured",
			ordered[0].Account.ID)
	}
	// And the stale entry is dropped, so it cannot come back.
	if _, ok := stickyLast.Load(combo.ID); ok {
		t.Error("expired sticky entry was not deleted")
	}
}

// Inside the window the last-good hop still leads — that is the strategy.
func TestStickyHoldsWithinIdleWindow(t *testing.T) {
	routes := []proxy.Route{hop(1, "m"), hop(2, "m")}
	combo := &store.Combo{ID: 202, Name: "sticky2", Strategy: "sticky_last_good", StickyIdleS: 1800}

	stickyLast.Store(combo.ID, stickyEntry{route: routes[1], at: time.Now()})
	ordered := stickyOrder(combo, routes)
	if ordered[0].Account.ID != 2 {
		t.Fatalf("fresh sticky hop lost the lead (%d), want hop 2", ordered[0].Account.ID)
	}
	if len(ordered) != 2 || ordered[1].Account.ID != 1 {
		t.Errorf("tail not preserved: %v", ordered)
	}
}

// sticky_idle_s = 0 means "never release on idle", which is the documented
// escape hatch for a combo that should pin one account.
func TestStickyZeroIdleNeverReleases(t *testing.T) {
	routes := []proxy.Route{hop(1, "m"), hop(2, "m")}
	combo := &store.Combo{ID: 203, Name: "sticky3", Strategy: "sticky_last_good", StickyIdleS: 0}

	stickyLast.Store(combo.ID, stickyEntry{route: routes[1], at: time.Now().Add(-24 * time.Hour)})
	if ordered := stickyOrder(combo, routes); ordered[0].Account.ID != 2 {
		t.Errorf("hop %d leads, want the sticky hop 2 with idle release disabled", ordered[0].Account.ID)
	}
}
