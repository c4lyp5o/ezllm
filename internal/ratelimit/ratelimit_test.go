package ratelimit

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// clock is a manually advanced clock so every window assertion is exact.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock {
	return &clock{t: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestAllowsExactlyTheLimitThenRefuses(t *testing.T) {
	c := newClock()
	l := New(c.Now)
	const limit = 5
	window := time.Minute

	for i := 0; i < limit; i++ {
		ok, _ := l.Allow(1, limit, window)
		if !ok {
			t.Fatalf("request %d refused, want allowed (budget is %d)", i+1, limit)
		}
	}
	ok, next := l.Allow(1, limit, window)
	if ok {
		t.Fatalf("request %d allowed, want refused", limit+1)
	}
	// The next legal instant is one window after the OLDEST hit, not after now.
	want := c.Now().Add(window)
	if !next.Equal(want) {
		t.Errorf("nextAllowed = %v, want %v (one window after the oldest hit)", next, want)
	}
}

func TestWindowSlidesSoBudgetReturns(t *testing.T) {
	c := newClock()
	l := New(c.Now)
	const limit = 2
	window := 10 * time.Second

	// Two hits, five seconds apart, so partial expiry is observable.
	if ok, _ := l.Allow(7, limit, window); !ok {
		t.Fatal("first hit refused")
	}
	c.Advance(5 * time.Second)
	if ok, _ := l.Allow(7, limit, window); !ok {
		t.Fatal("second hit refused")
	}
	if ok, _ := l.Allow(7, limit, window); ok {
		t.Fatal("budget exhausted but request allowed")
	}

	// At t0+10s the FIRST hit is exactly one window old and has aged out; the
	// second (t0+5s) is still inside. Exactly one slot must free.
	// The window is half-open — a hit exactly `window` old does NOT count —
	// which is the honest reading of "5 requests within 1 minute".
	c.Advance(5 * time.Second)
	if ok, _ := l.Allow(7, limit, window); !ok {
		t.Error("one slot should have freed after the oldest hit aged out")
	}
	if ok, _ := l.Allow(7, limit, window); ok {
		t.Error("only one slot should have freed, but a second request passed")
	}

	// Now the second hit has aged out too: the budget is fully back.
	c.Advance(5 * time.Second)
	if ok, _ := l.Allow(7, limit, window); !ok {
		t.Error("both hits should have aged out")
	}
}

// THE property the whole design exists for: a parallel burst — the shape of an
// agent tool loop firing every call at once — must admit exactly `limit`, never
// more. A read-then-increment split across two calls fails this.
func TestParallelBurstAdmitsExactlyTheLimit(t *testing.T) {
	c := newClock()
	l := New(c.Now)
	const (
		limit    = 5
		window   = time.Minute
		callers  = 200
		attempts = 8
	)

	var allowed int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release them together to maximise the race
			for j := 0; j < attempts; j++ {
				if ok, _ := l.Allow(42, limit, window); ok {
					atomic.AddInt64(&allowed, 1)
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	if allowed != limit {
		t.Fatalf("allowed = %d across %d parallel attempts, want exactly %d", allowed, callers*attempts, limit)
	}
}

func TestUnlimitedNeverTracks(t *testing.T) {
	c := newClock()
	l := New(c.Now)

	for i := 0; i < 100; i++ {
		if ok, next := l.Allow(3, 0, time.Minute); !ok {
			t.Fatal("limit 0 must mean unlimited")
		} else if !next.IsZero() {
			t.Fatal("unlimited must not report a nextAllowed")
		}
	}
	// A zero window is the same story, and must not be read as "every request
	// already aged out" — the fail-open bug an unguarded window comparison
	// would produce.
	if ok, _ := l.Allow(3, 5, 0); !ok {
		t.Error("window 0 must mean unlimited rather than instant refusal")
	}
	if accounts, entries := l.Stats(); accounts != 0 || entries != 0 {
		t.Errorf("unlimited accounts were tracked: accounts=%d entries=%d", accounts, entries)
	}
}

func TestAccountsAreIndependent(t *testing.T) {
	c := newClock()
	l := New(c.Now)
	const limit = 2
	window := time.Minute

	for i := 0; i < limit; i++ {
		if ok, _ := l.Allow(1, limit, window); !ok {
			t.Fatal("account 1 primed")
		}
	}
	if ok, _ := l.Allow(1, limit, window); ok {
		t.Fatal("account 1 should be spent")
	}
	// Account 2 has its own budget: one account hitting its ceiling must never
	// gag another, which is why a global limiter was rejected.
	for i := 0; i < limit; i++ {
		if ok, _ := l.Allow(2, limit, window); !ok {
			t.Fatalf("account 2 request %d refused by account 1's spend", i+1)
		}
	}
}

func TestSeededWindowStartsSpent(t *testing.T) {
	c := newClock()
	l := New(c.Now)
	window := time.Minute
	// Five calls in the last minute, as read back from the ledger at boot.
	hits := []time.Time{
		c.Now().Add(-50 * time.Second),
		c.Now().Add(-40 * time.Second),
		c.Now().Add(-30 * time.Second),
		c.Now().Add(-20 * time.Second),
		c.Now().Add(-10 * time.Second),
	}
	l.Seed(9, hits)

	if ok, next := l.Allow(9, 5, window); ok {
		t.Fatal("a restarted process handed out a fresh budget to a spent credential")
	} else if !next.Equal(c.Now().Add(-50 * time.Second).Add(window)) {
		t.Errorf("nextAllowed = %v, want 10s from now (oldest seeded hit + window)", next)
	}
	// A wider lookback than the window must not inflate the count.
	l2 := New(c.Now)
	l2.Seed(9, append([]time.Time{c.Now().Add(-10 * time.Hour)}, hits...))
	if used, _ := l2.Snapshot(9, window); used != 5 {
		t.Errorf("seed with an out-of-window hit counted %d, want 5", used)
	}
}

func TestSeedKeepsOrderRegardlessOfInputOrder(t *testing.T) {
	c := newClock()
	l := New(c.Now)
	window := time.Minute
	newest := c.Now().Add(-5 * time.Second)
	oldest := c.Now().Add(-55 * time.Second)
	l.Seed(1, []time.Time{newest, oldest}) // deliberately out of order

	// Two seeded hits against a budget of 1: already spent, and the reported
	// release time must come from the OLDEST hit, not the order they arrived in.
	ok, next := l.Allow(1, 1, window)
	if ok {
		t.Fatal("window seeded with two hits must be spent at limit 1")
	}
	if !next.Equal(oldest.Add(window)) {
		t.Errorf("nextAllowed = %v, want %v — the OLDEST hit governs", next, oldest.Add(window))
	}
}

func TestLimitChangeTakesEffectImmediately(t *testing.T) {
	c := newClock()
	l := New(c.Now)
	window := time.Minute

	if ok, _ := l.Allow(1, 10, window); !ok {
		t.Fatal("primed with a generous limit")
	}
	// An operator tightens the budget to 1: the existing hit already fills it.
	if ok, _ := l.Allow(1, 1, window); ok {
		t.Error("lowered limit not enforced on the next request")
	}
	// And clearing it releases the budget entirely.
	if ok, _ := l.Allow(1, 0, window); !ok {
		t.Error("cleared limit should mean unlimited")
	}
	if accounts, _ := l.Stats(); accounts != 0 {
		t.Errorf("cleared account still tracked: %d", accounts)
	}
}

func TestForgetReleasesBudget(t *testing.T) {
	c := newClock()
	l := New(c.Now)
	window := time.Minute
	if ok, _ := l.Allow(5, 1, window); !ok {
		t.Fatal("primed")
	}
	if ok, _ := l.Allow(5, 1, window); ok {
		t.Fatal("should be spent")
	}
	l.Forget(5)
	if ok, _ := l.Allow(5, 1, window); !ok {
		t.Error("after Forget the budget should be available again")
	}
}

func TestSnapshotReportsLiveUsage(t *testing.T) {
	c := newClock()
	l := New(c.Now)
	window := 30 * time.Second

	if used, next := l.Snapshot(1, window); used != 0 || !next.IsZero() {
		t.Errorf("empty snapshot = (%d,%v), want (0, zero)", used, next)
	}
	l.Allow(1, 5, window)
	c.Advance(10 * time.Second)
	l.Allow(1, 5, window)
	if used, _ := l.Snapshot(1, window); used != 2 {
		t.Errorf("used = %d, want 2", used)
	}
	c.Advance(window)
	if used, _ := l.Snapshot(1, window); used != 0 {
		t.Errorf("used after the window elapsed = %d, want 0", used)
	}
}
