package bridgeutil

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
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

	name     string       // bridge label for the refusal warning
	log      *slog.Logger // nil = slog.Default()
	lastWarn atomic.Int64 // unix nanos of the last refusal warning
	quiet    atomic.Int64 // refusals since that warning
}

// rejectWarnEvery bounds the refusal warning to one line per window, so a
// burst of refused requests cannot flood stdout; the line counts the rest.
const rejectWarnEvery = 10 * time.Second

// Named sets the bridge label the refusal warning carries.
func (s *Slots) Named(name string) *Slots {
	s.name = name
	return s
}

// warnRejected logs that the bridge is turning requests away. Saturation is
// otherwise visible only to the client (429 queue_full) and in /status.
func (s *Slots) warnRejected() {
	now := time.Now().UnixNano()
	last := s.lastWarn.Load()
	if (last != 0 && now-last < int64(rejectWarnEvery)) || !s.lastWarn.CompareAndSwap(last, now) {
		s.quiet.Add(1)
		return
	}
	log := s.log
	if log == nil {
		log = slog.Default()
	}
	log.Warn("bridge queue full; refusing requests with 429 queue_full",
		"bridge", s.name, "max_concurrent", cap(s.sem), "max_queue", s.maxQueue,
		"rejected_total", s.rejected.Load(), "refused_since_last_warning", s.quiet.Swap(0))
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
	if sw := slotWaitFrom(ctx); sw != nil {
		start := time.Now()
		defer func() { sw.add(time.Since(start)) }()
	}
	select {
	case s.sem <- struct{}{}:
	default:
		if int(s.queued.Add(1)) > s.maxQueue {
			s.queued.Add(-1)
			s.rejected.Add(1)
			s.warnRejected()
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
		// The scheme is case-insensitive (RFC 9110); a bare token without it
		// is not a Bearer credential.
		if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
			got = strings.TrimSpace(h[7:])
		}
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

// SlotWaitHeader carries how long the request waited for a CLI slot, in whole
// milliseconds, from a bridge to the proxy, which records it in the ledger
// (calls.slot_wait_ms). Without it total_ms mixes queueing with CLI work.
const SlotWaitHeader = "X-Ezllm-Slot-Wait-Ms"

type slotWaitKey struct{}

// SlotWait accumulates the time one HTTP request spent in Acquire (a request
// that acquires twice, such as a fallback turn, reports the sum).
type SlotWait struct {
	ns       atomic.Int64
	acquired atomic.Bool
}

func (w *SlotWait) add(d time.Duration) {
	w.ns.Add(int64(d))
	w.acquired.Store(true)
}

// Waited reports the total wait and whether Acquire ran at all.
func (w *SlotWait) Waited() (time.Duration, bool) {
	return time.Duration(w.ns.Load()), w.acquired.Load()
}

func slotWaitFrom(ctx context.Context) *SlotWait {
	sw, _ := ctx.Value(slotWaitKey{}).(*SlotWait)
	return sw
}

// TrackSlotWait makes Acquire calls under r's context record their wait, and
// returns a writer that sends SlotWaitHeader with the response headers. The
// slot is always taken before the first header is written (streams delay
// their headers until the first delta), so the value is final when it is sent.
// Requests that never ask for a slot (models, status) carry no header.
func TrackSlotWait(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, *http.Request) {
	sw := &SlotWait{}
	return &slotWaitWriter{ResponseWriter: w, sw: sw}, r.WithContext(context.WithValue(r.Context(), slotWaitKey{}, sw))
}

type slotWaitWriter struct {
	http.ResponseWriter
	sw    *SlotWait
	wrote bool
}

func (w *slotWaitWriter) WriteHeader(code int) {
	if !w.wrote {
		w.wrote = true
		if d, ok := w.sw.Waited(); ok {
			w.Header().Set(SlotWaitHeader, strconv.FormatInt(d.Milliseconds(), 10))
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *slotWaitWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach Flush and SetWriteDeadline.
func (w *slotWaitWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
