package router

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// keyLister is a Catalog that can list every usable key of an account, not
// just the first. store.DB and its snapshot both are; a catalog that is not
// keeps the one-key behaviour.
type keyLister interface {
	PickKeys(ctx context.Context, accountID int64) ([]store.KeyPick, error)
}

// keyTurns is each account's key-rotation counter: which of its keys goes
// first on the next request. In-process on purpose, like blockCursors: losing
// it on restart only means rotation starts over at the oldest key.
var keyTurns sync.Map // int64 accountID -> *atomic.Uint64

func nextKeyTurn(accountID int64) uint64 {
	v, ok := keyTurns.Load(accountID)
	if !ok {
		v, _ = keyTurns.LoadOrStore(accountID, new(atomic.Uint64))
	}
	return v.(*atomic.Uint64).Add(1) - 1
}

// expandKeys turns each route into one route per usable key of its account,
// so a key that is rate limited fails over to the account's next key before
// the request moves on to another hop. The first key rotates per request:
// with two keys, alternate requests start on each. One turn is taken per
// account per request, so every hop of that account starts on the same key.
// A dry run (route explain) is returned unchanged: it must not move a turn.
func (r *Resolver) expandKeys(ctx context.Context, routes []proxy.Route) []proxy.Route {
	kl, ok := r.catalog.(keyLister)
	if !ok || explaining(ctx) {
		return routes
	}
	type rotation struct {
		keys  []store.KeyPick
		start int
	}
	seen := map[int64]rotation{}
	out := make([]proxy.Route, 0, len(routes))
	for _, rt := range routes {
		rot, done := seen[rt.Account.ID]
		if !done {
			keys, err := kl.PickKeys(ctx, rt.Account.ID)
			if err == nil && len(keys) > 1 {
				rot = rotation{keys: keys, start: int(nextKeyTurn(rt.Account.ID) % uint64(len(keys)))}
			}
			seen[rt.Account.ID] = rot
		}
		if len(rot.keys) < 2 {
			out = append(out, rt)
			continue
		}
		for i := range rot.keys {
			k := rot.keys[(rot.start+i)%len(rot.keys)]
			kr := rt
			kr.KeyID, kr.KeyPlain, kr.KeyHint = k.ID, k.Plaintext, k.Hint
			out = append(out, kr)
		}
	}
	return out
}

// keyCount reports how many usable keys an account has, for route explain.
func (r *Resolver) keyCount(ctx context.Context, accountID int64) int {
	if kl, ok := r.catalog.(keyLister); ok {
		if keys, err := kl.PickKeys(ctx, accountID); err == nil {
			return len(keys)
		}
		return 0
	}
	if _, err := r.catalog.PickKey(ctx, accountID); err == nil {
		return 1
	}
	return 0
}
