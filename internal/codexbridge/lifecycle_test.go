package codexbridge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Regression tests for the 2026-10-23 lifecycle audit (C2, C3, C5, C6).

// waitGone fails unless pid disappears within d.
func waitGone(t *testing.T, pid int, d time.Duration) {
	t.Helper()
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("needs /proc to observe the child")
	}
	deadline := time.Now().Add(d)
	for {
		if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child %d still alive after %s", pid, d)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// C2: a CLI that stops reading stdin blocks the bridge's write of a request
// bigger than the pipe buffer. Cancellation must still end the call and reap
// the child; before, the write held on until the pipe was closed by hand.
func TestCancelUnblocksAWedgedStdinWrite(t *testing.T) {
	r, home := fakeRunner(t, "no_read")
	req := simpleRequest()
	req.Instructions = strings.Repeat("x", 4<<20) // far past any pipe buffer
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, req, nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned nil from a wedged child")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run still blocked in the stdin write 5s after cancellation")
	}
	_, _, pid, _ := readStarted(t, home)
	waitGone(t, pid, 3*time.Second)
}

// C3: a sink stuck on a client that stopped reading must not keep the child
// alive past cancellation. Before, the reader goroutine called the sink while
// holding the turn lock, so cancellation waited on that lock and cleanup
// waited on the reader.
func TestCancelReapsTheChildWhileTheSinkIsStuck(t *testing.T) {
	r, home := fakeRunner(t, "hang")
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	var once sync.Once
	done := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, simpleRequest(), func(Delta) error {
			once.Do(func() {
				cancel()
				<-release // the stuck HTTP write
			})
			return nil
		})
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(home + "/started.json"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, _, pid, _ := readStarted(t, home)
	waitGone(t, pid, 3*time.Second) // while the sink is still stuck
	close(release)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned nil after cancel")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return once the sink was released")
	}
}

// C5: the runner timeout bounds the whole request, including the wait for a
// concurrency slot. Before, a request with no deadline of its own queued for
// as long as the slots stayed taken.
func TestRunnerTimeoutCoversTheSlotWait(t *testing.T) {
	r := &Runner{Bin: os.Args[0], Timeout: 100 * time.Millisecond, MaxConcurrent: 1}
	r.slots() <- struct{}{} // the only slot, held elsewhere
	defer func() { <-r.slots() }()
	start := time.Now()
	_, err := r.Run(context.Background(), simpleRequest(), nil)
	var ne *NativeError
	if !errors.As(err, &ne) || ne.Status != 504 {
		t.Fatalf("err = %v, want a 504 timeout", err)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("queued %s past a 100ms runner timeout", waited)
	}
}

// C6: Shutdown reaps parked children. They outlive their HTTP request by
// design, so an HTTP drain alone left them running for the 10-minute hold.
func TestShutdownReapsParkedTurns(t *testing.T) {
	r, home := fakeRunner(t, "tools")
	res, err := r.Run(context.Background(), weatherRequest(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolCalls) == 0 || r.ParkedTurns() != 1 {
		t.Fatalf("setup: tool calls %d, parked %d; want a parked turn", len(res.ToolCalls), r.ParkedTurns())
	}
	_, _, pid, _ := readStarted(t, home)
	r.Shutdown()
	if n := r.ParkedTurns(); n != 0 {
		t.Fatalf("parked after Shutdown = %d, want 0", n)
	}
	waitGone(t, pid, 3*time.Second)
}

// stallWriter is a ResponseWriter on a client that stopped reading: after the
// first write, Write blocks until a write deadline comes due (what net/http's
// conn does once the deadline passes).
type stallWriter struct {
	h        http.Header
	mu       sync.Mutex
	writes   int
	deadline chan struct{}
	once     sync.Once
}

func (s *stallWriter) Header() http.Header { return s.h }
func (s *stallWriter) WriteHeader(int)     {}
func (s *stallWriter) Write(b []byte) (int, error) {
	s.mu.Lock()
	s.writes++
	n := s.writes
	s.mu.Unlock()
	if n == 1 {
		return len(b), nil
	}
	<-s.deadline
	return 0, errors.New("i/o timeout")
}
func (s *stallWriter) SetWriteDeadline(d time.Time) error {
	// Only a deadline already due releases the write, as on a real conn; the
	// per-write stall deadline lies a minute ahead and must not.
	if !d.IsZero() && !d.After(time.Now()) {
		s.once.Do(func() { close(s.deadline) })
	}
	return nil
}

// floodExec streams deltas until its sink fails.
type floodExec struct{ fakeExec }

func (f *floodExec) Run(ctx context.Context, _ Request, sink func(Delta) error) (Result, error) {
	for {
		if err := sink(Delta{Content: "x"}); err != nil {
			return Result{}, err
		}
	}
}

// C3, HTTP half: cancelling the request must unblock a stream write stuck on
// a client that stopped reading. Before, nothing interrupted the write.
func TestCancelUnblocksAStalledStreamWrite(t *testing.T) {
	srv := &Server{Runner: &floodExec{}}
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-6.1-sol","stream":true,"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	w := &stallWriter{h: http.Header{}, deadline: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		srv.chat(w, r)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond) // the stream is now stuck in its second write
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler still stuck in a stream write 5s after cancellation")
	}
}
