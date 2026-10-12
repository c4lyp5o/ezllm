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
		ordered, _ := blockOrder(combo, routes)
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
	ordered, _ := blockOrder(combo, routes)
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
	if ordered, pos := blockOrder(combo, routes); len(ordered) != 1 || ordered[0].Account.ID != 9 || pos != 1 {
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
	lastGood.Store(combo.Name, lastGoodEntry{accountID: 2, model: "m", at: time.Now().Add(-2 * time.Minute)})
	ordered := stickyOrder(combo, routes)
	if ordered[0].Account.ID != 1 {
		t.Errorf("stale sticky hop still leads (%d) — sticky_idle_s is not being honoured",
			ordered[0].Account.ID)
	}
	// And the stale entry is dropped, so it cannot come back.
	if _, ok := lastGood.Load(combo.Name); ok {
		t.Error("expired sticky entry was not deleted")
	}
}

// Inside the window the last-good hop still leads — that is the strategy.
func TestStickyHoldsWithinIdleWindow(t *testing.T) {
	routes := []proxy.Route{hop(1, "m"), hop(2, "m")}
	combo := &store.Combo{ID: 202, Name: "sticky2", Strategy: "sticky_last_good", StickyIdleS: 1800}

	lastGood.Store(combo.Name, lastGoodEntry{accountID: 2, model: "m", at: time.Now()})
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

	lastGood.Store(combo.Name, lastGoodEntry{accountID: 2, model: "m", at: time.Now().Add(-24 * time.Hour)})
	if ordered := stickyOrder(combo, routes); ordered[0].Account.ID != 2 {
		t.Errorf("hop %d leads, want the sticky hop 2 with idle release disabled", ordered[0].Account.ID)
	}
}

// ── true_round_robin: a genuine dice roll ──────────────────────────────────

// It is random, not a rotation: both hops must be reached, and neither may
// dominate. Bounds are deliberately loose — this must not flake — while still
// failing a strategy that always returns the same hop.
func TestTrueRoundRobinIsRandom(t *testing.T) {
	routes := []proxy.Route{hop(1, "m"), hop(2, "m")}
	const draws = 400
	counts := map[int64]int{}
	for i := 0; i < draws; i++ {
		ordered := randomOrder(routes)
		if len(ordered) != 2 {
			t.Fatalf("order length = %d, want 2", len(ordered))
		}
		counts[ordered[0].Account.ID]++
		// The tail keeps position order behind whichever hop won the roll, so
		// failover still works.
		if ordered[1].Account.ID == ordered[0].Account.ID {
			t.Fatalf("tail repeats the head: %v", ordered)
		}
	}
	for _, id := range []int64{1, 2} {
		if counts[id] < draws/4 || counts[id] > draws*3/4 {
			t.Errorf("hop %d won %d of %d rolls — not random (want roughly half)", id, counts[id], draws)
		}
	}
}

// A single hop has nothing to roll for.
func TestRandomOrderSingleHopUnchanged(t *testing.T) {
	routes := []proxy.Route{hop(5, "m")}
	if ordered := randomOrder(routes); len(ordered) != 1 || ordered[0].Account.ID != 5 {
		t.Errorf("single hop was reordered: %v", ordered)
	}
}

// ── strict_round_robin: advance only on success, from the hop that served ───

// The core semantics: after a hop DELIVERS, the next request starts at the one
// after it — walking the list and wrapping.
func TestStrictRoundRobinAdvancesAfterSuccess(t *testing.T) {
	combo := &store.Combo{ID: 301, Name: "strict1", Strategy: "strict_round_robin"}
	routes := []proxy.Route{hop(1, "m"), hop(2, "m"), hop(3, "m")}

	// Nothing delivered yet: position order.
	if ordered := strictOrder(combo, routes); ordered[0].Account.ID != 1 {
		t.Fatalf("cold start head = %d, want 1", ordered[0].Account.ID)
	}
	// Hop 1 answered → next request starts at hop 2.
	lastGood.Store(combo.Name, lastGoodEntry{accountID: 1, model: "m", at: time.Now()})
	if ordered := strictOrder(combo, routes); ordered[0].Account.ID != 2 {
		t.Fatalf("after hop 1 served, head = %d, want 2", ordered[0].Account.ID)
	}
	// Hop 2 answered → hop 3.
	lastGood.Store(combo.Name, lastGoodEntry{accountID: 2, model: "m", at: time.Now()})
	if ordered := strictOrder(combo, routes); ordered[0].Account.ID != 3 {
		t.Fatalf("after hop 2 served, head = %d, want 3", ordered[0].Account.ID)
	}
	// Hop 3 answered → wraps to hop 1.
	lastGood.Store(combo.Name, lastGoodEntry{accountID: 3, model: "m", at: time.Now()})
	if ordered := strictOrder(combo, routes); ordered[0].Account.ID != 1 {
		t.Fatalf("after hop 3 served, head = %d, want the rotation to wrap to 1", ordered[0].Account.ID)
	}
}

// A hop that FAILED does not move the cursor, so it deprioritises itself: hop 1
// failed, hop 2 answered, and the next request therefore starts at hop 3 rather
// than going straight back to the hop that just broke. It is not blacklisted —
// the rotation returns to it on the next lap.
func TestStrictRoundRobinDeprioritisesAFailedHop(t *testing.T) {
	combo := &store.Combo{ID: 302, Name: "strict2", Strategy: "strict_round_robin"}
	routes := []proxy.Route{hop(1, "m"), hop(2, "m"), hop(3, "m")}

	// Request 1 started at hop 1 (position order), hop 1 failed, hop 2 served.
	lastGood.Store(combo.Name, lastGoodEntry{accountID: 2, model: "m", at: time.Now()})
	if ordered := strictOrder(combo, routes); ordered[0].Account.ID != 3 {
		t.Fatalf("head = %d, want 3 — the request must not restart at the hop that just failed",
			ordered[0].Account.ID)
	}
	// Then hop 3 serves, and the rotation comes back round to hop 1.
	lastGood.Store(combo.Name, lastGoodEntry{accountID: 3, model: "m", at: time.Now()})
	if ordered := strictOrder(combo, routes); ordered[0].Account.ID != 1 {
		t.Errorf("head = %d, want 1 — a failed hop must be retried, not skipped forever",
			ordered[0].Account.ID)
	}
}

// The remembered hop can leave the eligible set. Position order is the honest
// fallback rather than an index guess.
func TestStrictRoundRobinFallsBackWhenServedHopIsGone(t *testing.T) {
	combo := &store.Combo{ID: 303, Name: "strict3", Strategy: "strict_round_robin"}
	routes := []proxy.Route{hop(1, "m"), hop(2, "m")}
	lastGood.Store(combo.Name, lastGoodEntry{accountID: 99, model: "m", at: time.Now()})
	if ordered := strictOrder(combo, routes); ordered[0].Account.ID != 1 {
		t.Errorf("head = %d, want position order (1) when the served hop is no longer eligible",
			ordered[0].Account.ID)
	}
}

func TestStrictRoundRobinSingleHopUnchanged(t *testing.T) {
	combo := &store.Combo{ID: 304, Name: "strict4", Strategy: "strict_round_robin"}
	routes := []proxy.Route{hop(7, "m")}
	lastGood.Store(combo.Name, lastGoodEntry{accountID: 1, model: "m", at: time.Now()})
	if ordered := strictOrder(combo, routes); len(ordered) != 1 || ordered[0].Account.ID != 7 {
		t.Errorf("single hop was reordered: %v", ordered)
	}
}

// ── the success hook itself ────────────────────────────────────────────────

// Keyed by combo NAME because the writer is the server, which knows the alias
// the client asked for and not the combo's row id. An empty name is ignored.
func TestMarkHopGoodIsKeyedByComboName(t *testing.T) {
	r := New(withCombo(newFake(), dayNightCombo()))
	routes := []proxy.Route{hop(1, "m"), hop(2, "m")}

	r.MarkHopGood("daily", routes[1])
	if v, ok := lastGood.Load("daily"); !ok {
		t.Fatal("MarkHopGood stored nothing under the combo name")
	} else if e := v.(lastGoodEntry); e.accountID != 2 {
		t.Errorf("stored hop = %d, want 2", e.accountID)
	}
	// A different combo is unaffected — the map is per combo, not global.
	strict := &store.Combo{ID: 305, Name: "other", Strategy: "strict_round_robin"}
	if ordered := strictOrder(strict, routes); ordered[0].Account.ID != 1 {
		t.Errorf("another combo inherited daily's last-good hop (head %d)", ordered[0].Account.ID)
	}
	// An empty alias (a route with no combo) must not create an entry.
	r.MarkHopGood("", routes[0])
	if _, ok := lastGood.Load(""); ok {
		t.Error("MarkHopGood stored an entry for an empty combo name")
	}
}
