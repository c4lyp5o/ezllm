package bridgeutil

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
)

// DefaultMaxQueue is how many requests may wait for a CLI slot when the
// operator sets no queue. Past it a bridge refuses at once (429 queue_full)
// instead of parking the caller until its context ends: a 1-slot bridge with a
// 300 s timeout would otherwise hold sockets for minutes behind a stall.
const DefaultMaxQueue = 8

// QueueFullRetryAfter is the Retry-After (seconds) sent with a queue_full 429.
// The router parks the hop for that long and fails over.
const QueueFullRetryAfter = "5"

// ErrQueueFull is returned by Acquire when every slot is busy and the wait
// queue is full.
var ErrQueueFull = errors.New("every CLI slot is busy and the queue is full; retry shortly")

// Slots is the admission gate every subscription bridge uses: a fixed number
// of concurrent CLI turns plus a bounded wait queue, with counters for /status.
type Slots struct {
	sem      chan struct{}
	maxQueue int

	inflight, queued, served, rejected atomic.Int64
}

// NewSlots builds the gate; non-positive values take the defaults (1 slot,
// DefaultMaxQueue waiting).
func NewSlots(maxConcurrent, maxQueue int) *Slots {
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	if maxQueue <= 0 {
		maxQueue = DefaultMaxQueue
	}
	return &Slots{sem: make(chan struct{}, maxConcurrent), maxQueue: maxQueue}
}

// Acquire takes a slot, waiting in the bounded queue when none is free. It
// returns ErrQueueFull when the queue is full, or ctx.Err() when the context
// ends while waiting. The returned release must be called exactly once.
func (s *Slots) Acquire(ctx context.Context) (release func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case s.sem <- struct{}{}:
	default:
		if int(s.queued.Add(1)) > s.maxQueue {
			s.queued.Add(-1)
			s.rejected.Add(1)
			return nil, ErrQueueFull
		}
		select {
		case s.sem <- struct{}{}:
			s.queued.Add(-1)
		case <-ctx.Done():
			s.queued.Add(-1)
			return nil, ctx.Err()
		}
	}
	s.inflight.Add(1)
	var once atomic.Bool
	return func() {
		if once.Swap(true) {
			return
		}
		s.inflight.Add(-1)
		s.served.Add(1)
		<-s.sem
	}, nil
}

// Stats is the admission picture each bridge's /status serves.
func (s *Slots) Stats() map[string]any {
	return map[string]any{
		"in_flight":           s.inflight.Load(),
		"queued":              s.queued.Load(),
		"max_queue":           s.maxQueue,
		"max_concurrent":      cap(s.sem),
		"served":              s.served.Load(),
		"rejected_queue_full": s.rejected.Load(),
	}
}

// Authorized reports whether r carries the bridge token as a Bearer
// credential (or, when allowXAPIKey, as x-api-key, which Anthropic clients
// send). An empty configured token refuses everything. The comparison is
// constant-time.
func Authorized(r *http.Request, token string, allowXAPIKey bool) bool {
	got := ""
	if allowXAPIKey {
		got = r.Header.Get("x-api-key")
	}
	if got == "" {
		got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	return token != "" && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// Cap is the number of concurrent slots.
func (s *Slots) Cap() int { return cap(s.sem) }

// Hold waits for a slot for the bridge's own background work (for example
// cleanup that needs a CLI process). It ignores the queue bound and the
// counters: client pressure must not refuse internal work, and internal work
// is not served traffic.
func (s *Slots) Hold(ctx context.Context) (release func(), err error) {
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var once atomic.Bool
	return func() {
		if !once.Swap(true) {
			<-s.sem
		}
	}, nil
}

// Queued is how many requests are waiting for a slot right now.
func (s *Slots) Queued() int64 { return s.queued.Load() }
