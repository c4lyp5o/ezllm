package router

import (
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/store"
)

func luRoute(acct int64, model string) proxy.Route {
	return proxy.Route{Account: provider.Account{ID: acct}, Model: model}
}

func order(rs []proxy.Route) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Model
	}
	return out
}

// M5: least_used was accepted and documented but kept hop order. It now
// leads with the hop carrying the least recent traffic.
func TestLeastUsedOrder(t *testing.T) {
	combo := &store.Combo{Name: "lu-test-" + t.Name(), Strategy: "least_used"}
	a, b, c := luRoute(1, "a"), luRoute(1, "b"), luRoute(2, "c")
	routes := []proxy.Route{a, b, c}
	r := &Resolver{}
	t0 := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

	if got := order(leastUsedOrder(combo, routes, t0)); got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("cold combo = %v, want position order", got)
	}
	r.noteUseAt(combo.Name, a, t0)
	r.noteUseAt(combo.Name, a, t0)
	r.noteUseAt(combo.Name, b, t0)
	if got := order(leastUsedOrder(combo, routes, t0)); got[0] != "c" || got[1] != "b" || got[2] != "a" {
		t.Fatalf("after a×2, b×1 = %v, want [c b a]", got)
	}
	// Decay: a's two uses an hour ago weigh less than b's one use just now.
	t1 := t0.Add(time.Hour)
	r.noteUseAt(combo.Name, b, t1)
	r.noteUseAt(combo.Name, c, t1)
	if got := order(leastUsedOrder(combo, routes, t1)); got[0] != "a" {
		t.Fatalf("hour-old traffic must decay: order = %v, want a first", got)
	}
	// Another combo sharing the hop keeps its own count.
	other := &store.Combo{Name: combo.Name + "-other", Strategy: "least_used"}
	if got := order(leastUsedOrder(other, routes, t1)); got[0] != "a" || got[1] != "b" {
		t.Fatalf("usage leaked across combos: %v", got)
	}
}

func TestNoteUseIgnoresDirectRoutes(t *testing.T) {
	r := &Resolver{}
	r.NoteUse("ns/model", luRoute(1, "model"))
	r.NoteUse("", luRoute(1, "model"))
	if _, ok := hopUse.Load(useKey("ns/model", 1, "model")); ok {
		t.Fatal("a direct namespace/model route is not a combo and must not be counted")
	}
}

// A renamed block_round_robin combo: a held hop's failure (reported by the
// NEW name) must still release the block.
func TestBlockCursorFollowsComboRename(t *testing.T) {
	r := &Resolver{}
	id := time.Now().UnixNano() // a cursor key no other test uses
	hop := luRoute(1, "held")
	hop.HoldUntilFailure = true
	routes := []proxy.Route{hop, luRoute(1, "next")}
	before := &store.Combo{ID: id, Name: "old-name", BlockSize: 5}
	blockOrder(before, routes)
	renamed := &store.Combo{ID: id, Name: "new-name", BlockSize: 5}
	blockOrder(renamed, routes)
	r.MarkHopBad("new-name", hop)
	got, _ := blockOrder(renamed, routes)
	if got[0].Model != "next" {
		t.Fatalf("after the held hop failed under the new name, order = %v, want next first", order(got))
	}
}
