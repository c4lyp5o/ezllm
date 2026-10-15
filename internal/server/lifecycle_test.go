package server

// Lifecycle tests: what happens to the UPSTREAM when the CLIENT goes away.
//
// Both tests assert the same two things, and neither uses a bare time.Sleep as
// a synchronization primitive:
//
//  1. the mock upstream observes its request context canceled, within an
//     explicit deadline (`select` against `time.After`, never an unbounded
//     wait) — this is the whole point, because a gateway that leaked the
//     upstream connection would keep paying the provider for a stream nobody
//     is reading;
//  2. the server-side goroutine count returns to its pre-request baseline.
//
// GOROUTINE-LEAK METHOD: runtime.NumGoroutine() deltas with a bounded poll.
// `go.uber.org/goleak` was NOT used. The module is fetchable, but wiring it in
// edits go.mod and go.sum, which is outside this task's additive-only envelope
// (new files plus a Makefile target); adding a dependency is the maintainer's
// call, not a benchmark's. The delta method is weaker than goleak — it cannot
// name the leaked stack — so it is given a slack allowance and a deadline, and
// a failure prints the before/after counts.

import (
	"bytes"
	"context"

	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/proxy/benchutil"
)

// goroutineSlack absorbs the handful of goroutines the runtime and the test
// harness legitimately keep around across a request (timers, the ledger
// writer's wake-up, a connection's background reader winding down). A genuine
// leak of the upstream connection or the stream copier would add at least one
// goroutine per request and blow past this.
const goroutineSlack = 4

// TestClientCancelMidStreamCancelsUpstream: a client that cancels while an SSE
// stream is in flight must cause the UPSTREAM request to be canceled — not
// merely the client-side copy to stop.
func TestClientCancelMidStreamCancelsUpstream(t *testing.T) {
	// A long, slow stream: it must still be running when the client cancels, or
	// the test would be asserting cancellation of a finished request.
	mock := benchutil.New(benchutil.Options{Chunks: 4000, ChunkBytes: 256, Delay: 2 * time.Millisecond})
	defer mock.Close()

	client := benchutil.NewClient()
	h := newHarnessAt(t, mock.Server(), func(o *Options) {
		o.MaxBodyMiB = 32
		o.Dispatcher = proxy.NewDispatcher(client, provider.NewRegistry(client))
		quiet(o)
	})
	gw := httptest.NewServer(h.handler)
	defer gw.Close()

	baseline := settleGoroutines(t, 2*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	body := benchutil.StreamChatBody(benchServerModel, 1<<10)
	req, err := http.NewRequestWithContext(ctx, "POST", gw.URL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok-infer")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("streaming request: %v", err)
	}
	defer resp.Body.Close()

	// Synchronize on the stream actually having started, from both ends: the
	// mock has written bytes AND the client has read one. A blocking Read is the
	// signal; nothing here sleeps and hopes.
	select {
	case <-mock.WroteFirst():
	case <-time.After(5 * time.Second):
		t.Fatal("mock never wrote a first byte — the stream never started")
	}
	if _, err := resp.Body.Read(make([]byte, 64)); err != nil {
		t.Fatalf("client could not read the first stream byte: %v", err)
	}
	if mock.CancelCount() != 0 {
		t.Fatal("fixture: the upstream was already reported canceled before the client canceled")
	}

	cancel()

	select {
	case <-mock.Canceled():
	case <-time.After(5 * time.Second):
		t.Fatalf("the upstream request context was NOT canceled within 5s of the client canceling "+
			"(mock wrote %d bytes, still running) — the gateway leaked the upstream stream",
			mock.BytesWritten())
	}
	if n := mock.CancelCount(); n < 1 {
		t.Fatalf("CancelCount = %d, want >= 1", n)
	}

	if after := waitGoroutines(t, baseline, goroutineSlack, 5*time.Second); after > baseline+goroutineSlack {
		t.Errorf("goroutines did not return to baseline: before=%d after=%d (slack %d)",
			baseline, after, goroutineSlack)
	}
}

// TestClientCancelMidNonStreamReadCancelsUpstream: the same claim for a
// non-streaming request. The gateway buffers the whole upstream body before it
// writes anything to the client, so the client cannot observe progress — the
// mock's own write counter is the synchronization point instead.
func TestClientCancelMidNonStreamReadCancelsUpstream(t *testing.T) {
	// 1 MiB in 128 instalments, 5ms apart: the upstream read is demonstrably
	// still in progress when the cancel lands.
	mock := benchutil.New(benchutil.Options{ResponseBytes: 1 << 20, BodyPieces: 128, Delay: 5 * time.Millisecond})
	defer mock.Close()

	client := benchutil.NewClient()
	h := newHarnessAt(t, mock.Server(), func(o *Options) {
		o.MaxBodyMiB = 32
		o.Dispatcher = proxy.NewDispatcher(client, provider.NewRegistry(client))
		quiet(o)
	})
	gw := httptest.NewServer(h.handler)
	defer gw.Close()

	baseline := settleGoroutines(t, 2*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := benchutil.ChatBody(benchServerModel, 1<<10)
	req, err := http.NewRequestWithContext(ctx, "POST", gw.URL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok-infer")
	req.Header.Set("Content-Type", "application/json")

	done := make(chan error, 1)
	go func() {
		resp, err := client.Do(req)
		if err != nil {
			done <- err
			return
		}
		defer resp.Body.Close()
		_, err = io.Copy(io.Discard, resp.Body)
		done <- err
	}()

	select {
	case <-mock.WroteFirst():
	case <-time.After(5 * time.Second):
		t.Fatal("mock never started writing its body")
	}
	cancel()

	select {
	case <-mock.Canceled():
	case <-time.After(5 * time.Second):
		t.Fatalf("the upstream request context was NOT canceled within 5s of the client canceling "+
			"(mock wrote %d of %d bytes) — the gateway leaked the upstream request",
			mock.BytesWritten(), int64(mock.Options().ResponseBytes))
	}
	// The client's own call must also unwind rather than hang.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the client call did not unwind within 5s of the cancel")
	}

	if after := waitGoroutines(t, baseline, goroutineSlack, 5*time.Second); after > baseline+goroutineSlack {
		t.Errorf("goroutines did not return to baseline: before=%d after=%d (slack %d)",
			baseline, after, goroutineSlack)
	}
}

// TestUpstreamNotReportedCanceledOnNormalCompletion is the control for the two
// tests above: it proves the cancellation signal means what they claim.
//
// net/http cancels a request's context as a matter of course once the handler
// returns, so a naive watcher would fire on EVERY request and both tests above
// would pass vacuously. The mock suppresses that case; this test fails if the
// suppression is ever removed.
func TestUpstreamNotReportedCanceledOnNormalCompletion(t *testing.T) {
	mock := benchutil.New(benchutil.Options{Chunks: 8, ChunkBytes: 32, ResponseBytes: 512})
	defer mock.Close()

	client := benchutil.NewClient()
	h := newHarnessAt(t, mock.Server(), func(o *Options) {
		o.MaxBodyMiB = 32
		o.Dispatcher = proxy.NewDispatcher(client, provider.NewRegistry(client))
		quiet(o)
	})
	gw := httptest.NewServer(h.handler)
	defer gw.Close()

	for i := 0; i < 5; i++ {
		for _, body := range [][]byte{
			benchutil.StreamChatBody(benchServerModel, 1<<10),
			benchutil.ChatBody(benchServerModel, 1<<10),
		} {
			req, err := http.NewRequest("POST", gw.URL+"/v1/chat/completions", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer tok-infer")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("request %d: %v", i, err)
			}
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				t.Fatalf("request %d body: %v", i, err)
			}
			resp.Body.Close()
		}
	}

	if n := mock.CancelCount(); n != 0 {
		t.Errorf("CancelCount = %d after 10 completed requests, want 0 — the cancellation signal "+
			"fires on normal completion, which would make the cancel tests vacuous", n)
	}
	if mock.Hits() != 10 {
		t.Errorf("mock hits = %d, want 10", mock.Hits())
	}
}

// ── goroutine helpers ───────────────────────────────────────────────────────

// settleGoroutines waits for the count to stop moving and returns it: the
// baseline must be taken from a quiet process, not from a moment when the
// harness is still starting listeners.
func settleGoroutines(t *testing.T, deadline time.Duration) int {
	t.Helper()
	end := time.Now().Add(deadline)
	prev := runtime.NumGoroutine()
	for time.Now().Before(end) {
		time.Sleep(20 * time.Millisecond)
		cur := runtime.NumGoroutine()
		if cur == prev {
			return cur
		}
		prev = cur
	}
	return prev
}

// waitGoroutines polls until the count is at or below target+slack, or the
// deadline passes; it returns the last count either way.
func waitGoroutines(t *testing.T, target, slack int, deadline time.Duration) int {
	t.Helper()
	end := time.Now().Add(deadline)
	cur := runtime.NumGoroutine()
	for time.Now().Before(end) {
		if cur <= target+slack {
			return cur
		}
		time.Sleep(20 * time.Millisecond)
		cur = runtime.NumGoroutine()
	}
	return cur
}
