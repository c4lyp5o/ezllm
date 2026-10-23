package main

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// TestDrainWaitsForCancelledHandlers pins audit C4: when the graceful drain
// times out, the handler still blocked on its upstream must be cancelled AND
// finish (recording its ledger row) before drain returns, because the store
// is closed right after. The old ordering returned from a timed-out Shutdown
// with the handler still running, so its row hit a closed ledger.
func TestDrainWaitsForCancelledHandlers(t *testing.T) {
	reqs := newRequestLifetime()
	started := make(chan struct{})
	var recorded atomic.Bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()              // a slow upstream, released only by cancellation
		time.Sleep(20 * time.Millisecond) // the handler's own wrap-up before its ledger write
		recorded.Store(true)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: reqs.track(h), BaseContext: reqs.baseContext}
	go func() { _ = srv.Serve(ln) }()
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String())
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never started")
	}

	reqs.drain(srv, 50*time.Millisecond, 5*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !recorded.Load() {
		t.Fatal("drain returned while the handler was still running; its ledger row would hit a closed store")
	}
}
