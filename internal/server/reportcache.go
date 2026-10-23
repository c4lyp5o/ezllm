package server

import (
	"sync"
	"time"
)

// reportCache shares dashboard usage reports across callers for a bounded
// lifetime. The overview polls every 15s and every open tab repeats it; each
// report is a full window scan with per-group medians (hundreds of ms at 50k
// rows on a four-connection reader pool). Within ttl a report is computed
// once and served to every caller, and simultaneous callers wait on the one
// computation instead of starting their own. Errors are never kept. Not for
// authentication or budget checks — those must always read the store.
type reportCache struct {
	ttl time.Duration
	now func() time.Time

	mu sync.Mutex
	m  map[string]*reportFlight
}

type reportFlight struct {
	done chan struct{} // closed when val/err are set
	at   time.Time
	val  any
	err  error
}

// reportCacheMax bounds the entry count; stale entries are pruned first.
const reportCacheMax = 256

func newReportCache(ttl time.Duration, now func() time.Time) *reportCache {
	return &reportCache{ttl: ttl, now: now, m: map[string]*reportFlight{}}
}

// do returns key's report, computing it with fn when there is no fresh one.
// age is how old the served report is (0 when this call computed it).
func (c *reportCache) do(key string, fn func() (any, error)) (val any, age time.Duration, err error) {
	c.mu.Lock()
	if f, ok := c.m[key]; ok {
		select {
		case <-f.done:
			if f.err != nil {
				c.mu.Unlock()
				return nil, 0, f.err
			}
			if age := c.now().Sub(f.at); age < c.ttl {
				c.mu.Unlock()
				return f.val, age, nil
			}
		default: // in flight: wait for it
			c.mu.Unlock()
			<-f.done
			if f.err != nil {
				return nil, 0, f.err
			}
			return f.val, c.now().Sub(f.at), nil
		}
	}
	f := &reportFlight{done: make(chan struct{})}
	c.pruneLocked()
	c.m[key] = f
	c.mu.Unlock()

	f.val, f.err = fn()
	f.at = c.now()
	close(f.done)
	if f.err != nil {
		c.mu.Lock()
		if c.m[key] == f {
			delete(c.m, key)
		}
		c.mu.Unlock()
	}
	return f.val, 0, f.err
}

// pruneLocked drops expired entries once the map is full, then arbitrary
// finished ones if it still is. Caller holds mu.
func (c *reportCache) pruneLocked() {
	if len(c.m) < reportCacheMax {
		return
	}
	now := c.now()
	for k, f := range c.m {
		select {
		case <-f.done:
			if now.Sub(f.at) >= c.ttl || len(c.m) >= reportCacheMax {
				delete(c.m, k)
			}
		default:
		}
		if len(c.m) < reportCacheMax/2 {
			return
		}
	}
}
