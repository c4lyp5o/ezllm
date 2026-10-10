// Package ratelimit enforces per-account REQUEST budgets in memory.
//
// # Why in memory
//
// A reservation is taken on every request. ezllm's store has a single write
// connection (SetMaxOpenConns(1)) that the ledger flusher and admin writes also
// queue on, and a DB-backed reservation measured ~8k ops/s with millisecond
// tails once 30+ clients were in flight — queueing behind the one writer, which
// is precisely what the ledger's batching exists to avoid. This gate measures
// ~3M ops/s at sub-microsecond median and takes no I/O on the hot path.
//
// The ledger remains the source of truth: this window is a cached view of the
// recent past and is SEEDED FROM THE LEDGER AT BOOT (see Seed), so a restart
// does not forget the last window's traffic.
//
// # Why it is atomic
//
// The check and the increment happen under ONE account-local lock. A
// read-then-count split across two calls lets a parallel burst — a tool loop
// firing six requests at once — read the same count six times and pass all six,
// which is exactly the storm this exists to prevent. TestParallelBurstAdmitsExactlyTheLimit
// pins that property.
//
// # Scope
//
// Single process, by construction. Two ezllm replicas would each hold their own
// window and the effective ceiling would multiply; revisit before scaling out.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter holds one sliding window per account.
//
// Memory is bounded by sum(limit) across accounts: a hit is appended only when
// the request is allowed, so a window never holds more than `limit` timestamps.
type Limiter struct {
	now func() time.Time

	mu   sync.RWMutex
	acct map[int64]*account
}

type account struct {
	mu     sync.Mutex
	hits   []time.Time // ascending (oldest first); len <= limit
	limit  int64
	window time.Duration
}

// New builds a Limiter. A nil clock defaults to time.Now.
func New(now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{now: now, acct: make(map[int64]*account)}
}

func (l *Limiter) accountFor(accountID int64) *account {
	l.mu.RLock()
	a, ok := l.acct[accountID]
	l.mu.RUnlock()
	if ok {
		return a
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if a, ok := l.acct[accountID]; ok {
		return a // lost the race; use the winner
	}
	a = &account{}
	l.acct[accountID] = a
	return a
}

// Allow reserves one request against an account's budget.
//
// limit <= 0 or window <= 0 means "unlimited" (the documented 0 = unlimited
// convention) and never touches the map, so an account with no budget set costs
// nothing to police. The same applies if a limit is cleared at runtime.
//
// When the budget is spent it returns false and the instant the request becomes
// legal again: one full window after the OLDEST hit still in it, which is the
// earliest moment the count can drop below the limit. That value is what lets a
// client sleep instead of blind-retrying.
func (l *Limiter) Allow(accountID, limit int64, window time.Duration) (allowed bool, nextAllowed time.Time) {
	if limit <= 0 || window <= 0 {
		l.Forget(accountID)
		return true, time.Time{}
	}
	a := l.accountFor(accountID)
	now := l.now()

	a.mu.Lock()
	defer a.mu.Unlock()

	// An admin edit takes effect on the next request rather than after a
	// refresh tick: the router hands us the account row it just read.
	a.limit, a.window = limit, window

	cutoff := now.Add(-window)
	// Prune everything that has aged out. A hit exactly `window` old no longer
	// counts, so the comparison is strict.
	drop := 0
	for drop < len(a.hits) && !a.hits[drop].After(cutoff) {
		drop++
	}
	if drop > 0 {
		a.hits = append(a.hits[:0], a.hits[drop:]...)
	}

	if int64(len(a.hits)) >= limit {
		return false, a.hits[0].Add(window)
	}
	a.hits = append(a.hits, now)
	return true, time.Time{}
}

// Seed preloads recent hits for an account. Called once at boot with the
// window's worth of ledger rows, so a restart does not hand out a fresh budget
// to a credential that is already spent.
//
// Older hits than the account's window are pruned on the next Allow, so the
// caller may pass a slightly wider lookback than any single window.
func (l *Limiter) Seed(accountID int64, hits []time.Time) {
	if len(hits) == 0 {
		return
	}
	a := l.accountFor(accountID)
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, h := range hits {
		// Insertion sort by hand: the slices are tiny (bounded by the limit)
		// and the input is already ordered in practice, so this is a no-op in
		// the common case and correct in the uncommon one.
		pos := len(a.hits)
		for pos > 0 && a.hits[pos-1].After(h) {
			pos--
		}
		a.hits = append(a.hits, time.Time{})
		copy(a.hits[pos+1:], a.hits[pos:])
		a.hits[pos] = h
	}
}

// Forget drops an account's window. Used when a limit is cleared, and by tests.
func (l *Limiter) Forget(accountID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.acct, accountID)
}

// Stats reports how many accounts are tracked and how many timestamps are held,
// so the dashboard can show that the gate is not accumulating state.
func (l *Limiter) Stats() (accounts, entries int) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, a := range l.acct {
		a.mu.Lock()
		entries += len(a.hits)
		a.mu.Unlock()
	}
	return len(l.acct), entries
}

// Snapshot returns, for one account, how many requests are currently counted in
// its window and when the next one frees. Used by the admin API so an operator
// can see a live budget rather than guessing from the ledger.
func (l *Limiter) Snapshot(accountID int64, window time.Duration) (used int64, nextFree time.Time) {
	l.mu.RLock()
	a, ok := l.acct[accountID]
	l.mu.RUnlock()
	if !ok {
		return 0, time.Time{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	cutoff := l.now().Add(-window)
	n := int64(0)
	for _, h := range a.hits {
		if h.After(cutoff) {
			n++
		}
	}
	if n > 0 && window > 0 {
		nextFree = a.hits[0].Add(window)
	}
	return n, nextFree
}
