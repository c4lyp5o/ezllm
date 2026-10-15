package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/jsonbody"
)

// TestCancelPropagationLatency grades spec §18's "client cancellation to
// upstream cancellation initiation | p95 ≤ 20 ms". The cancellation tests in
// cancel_test.go prove CORRECTNESS within a generous 5 s deadline; this one
// measures the actual DISTRIBUTION so the target is a certificate rather
// than an assumption.
//
// Interval: t0 = immediately before the client context is canceled; t1 = the
// instant the upstream (mock) observes its request context Done. Includes the
// context tree cancellation, the http.Client's teardown of the in-flight
// request, and the loopback delivery of the disconnect to the mock. Loopback
// only — a WAN upstream would see its own transit delay.
//
// Sync is channels + explicit deadlines (select + time.After as a FAILURE
// guard only). The only deliberate pacing is the mock's own event loop.
func TestCancelPropagationLatency(t *testing.T) {
	const iterations = 1000
	latencies := make([]time.Duration, 0, iterations)

	for i := 0; i < iterations; i++ {
		started := make(chan struct{})
		upstreamCanceled := make(chan time.Time, 1)
		mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			_, _ = w.Write([]byte("data: {\"delta\":1}\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			close(started)
			// Stream until the client goes away; the ctx Done timestamp is
			// the measurement point. Ticker pacing is the mock's delivery
			// cadence, not a test sleep.
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-r.Context().Done():
					upstreamCanceled <- time.Now()
					return
				case <-tick.C:
					_, _ = w.Write([]byte("data: {\"delta\":1}\n\n"))
					if flusher != nil {
						flusher.Flush()
					}
				}
			}
		}))

		d := newDispatcher()
		rt := textRoute(mock.URL, "hop/model")
		body := []byte(`{"model":"m","stream":true,"messages":[]}`)
		req, _ := reqWithBody(string(body))

		ctx, cancel := context.WithCancel(context.Background())
		dispatchDone := make(chan struct{})
		go func() {
			defer close(dispatchDone)
			_, _, _ = d.ForwardCandidatesDoc(ctx, newDiscardWriter(), req,
				[]Route{rt}, body, jsonbody.Open(body))
		}()

		select {
		case <-started:
		case <-time.After(5 * time.Second):
			cancel()
			mock.Close()
			t.Fatalf("iteration %d: mock never started", i)
		}

		t0 := time.Now()
		cancel()
		var observed time.Time
		select {
		case observed = <-upstreamCanceled:
		case <-time.After(5 * time.Second):
			mock.Close()
			t.Fatalf("iteration %d: upstream context was never canceled after 5s", i)
		}
		latencies = append(latencies, observed.Sub(t0))

		// Wait for the dispatch to fully unwind before closing the mock:
		// a goroutine left touching a closed server is a flake generator.
		select {
		case <-dispatchDone:
		case <-time.After(5 * time.Second):
			mock.Close()
			t.Fatalf("iteration %d: dispatch did not unwind", i)
		}
		mock.Close()
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	n := len(latencies)
	at := func(p float64) time.Duration {
		idx := int(float64(n)*p+0.999999) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= n {
			idx = n - 1
		}
		return latencies[idx]
	}
	t.Logf("── cancel propagation ── n=%d (nearest-rank)", n)
	t.Logf("  client-cancel -> upstream-cancel  p50=%-10s p95=%-10s p99=%-10s min=%-10s max=%s",
		at(0.50), at(0.95), at(0.99), latencies[0], latencies[n-1])

	// The spec target, asserted so it is a gate and not a printout. A loaded
	// home box occasionally spikes (GC, swap); p95 is the contract, so a rare
	// max above 20 ms is information, not failure.
	if p95 := at(0.95); p95 > 20*time.Millisecond {
		t.Errorf("spec §18 FAIL: cancel propagation p95 = %v > 20ms", p95)
	}
}
