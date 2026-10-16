package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/config"
)

func osWriteFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// Addendum #3: the request-wide upstream-attempt budget. Same-hop retries and
// failover hops count TOGETHER, so one client request cannot dial N hops x
// retries. Without a budget, 2 hops x 3 retries = 8 upstream calls for one
// client request — exactly the hammering class the old combo_e2e test pinned.

// countingFail returns a stub that always 500s (a RETRYABLE status — a 429
// is deliberately failover-only, never retried on the same hop, so it cannot
// exercise the retry side of the budget) and counts its calls.
func countingFail(t *testing.T) (*httptest.Server, *int64) {
	t.Helper()
	var n int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&n, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"upstream broke"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// With a budget, the total dialed attempts across hops cannot exceed it.
func TestAttemptBudgetBoundsFailovers(t *testing.T) {
	a, an := countingFail(t)
	b, bn := countingFail(t)
	d := newDispatcher()
	d.SetRetries(3)       // each hop would try 4 times on its own
	d.SetAttemptBudget(3) // the REQUEST may dial 3 times, total
	req, body := reqWithBody(`{"model":"day","messages":[]}`)
	routes := []Route{textRoute(a.URL, "day"), textRoute(b.URL, "night")}
	rec := httptest.NewRecorder()
	_, res, _ := d.ForwardCandidates(context.Background(), rec, req, routes, body)
	if got := atomic.LoadInt64(an); got != 3 {
		t.Errorf("hop A dialed %d times, want exactly the budget of 3", got)
	}
	if got := atomic.LoadInt64(bn); got != 0 {
		t.Errorf("hop B dialed %d times, want 0 — the budget was spent before it", got)
	}
	if res.UpstreamAttempts != 3 {
		t.Errorf("UpstreamAttempts = %d, want 3", res.UpstreamAttempts)
	}
	if res.AttemptBudget != 3 {
		t.Errorf("AttemptBudget = %d, want 3", res.AttemptBudget)
	}
	// The un-dialed hop must be VISIBLE to the operator, and HONESTLY
	// labelled: the note names the budget as the reason. The loop-level
	// check (not the retry-level one) is what prevents a "transport:" error
	// label on a request that never left the box.
	notes := strings.Join(attemptNotes(res.Attempts), " ")
	if !strings.Contains(notes, "attempt budget") {
		t.Errorf("the un-dialed hop must carry a budget note: %+v", res.Attempts)
	}
	if strings.Contains(notes, "transport") {
		t.Errorf("an un-dialed hop must not be labelled a transport failure: %q", notes)
	}
}

func attemptNotes(as []Attempt) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		if a.Note != "" {
			out = append(out, a.Note)
		}
	}
	return out
}

// Budget 0 = unlimited = the legacy behaviour, and it is RECORDED as
// unlimited (never as zero-left).
func TestAttemptBudgetUnlimitedIsLegacyAndExplicit(t *testing.T) {
	a, an := countingFail(t)
	b, bn := countingFail(t)
	d := newDispatcher()
	d.SetRetries(1) // 2 attempts per hop
	d.SetAttemptBudget(0)
	req, body := reqWithBody(`{"model":"day","messages":[]}`)
	routes := []Route{textRoute(a.URL, "day"), textRoute(b.URL, "night")}
	rec := httptest.NewRecorder()
	_, res, _ := d.ForwardCandidates(context.Background(), rec, req, routes, body)
	if got := atomic.LoadInt64(an) + atomic.LoadInt64(bn); got != 4 {
		t.Errorf("unlimited budget must keep the legacy count: %d, want 4", got)
	}
	if res.AttemptBudget != 0 {
		t.Errorf("AttemptBudget = %d, want 0 (unlimited)", res.AttemptBudget)
	}
}

// When the same-hop retry limit is hit FIRST, the remaining budget serves
// the failover — the two limits compose instead of one hiding the other.
func TestRetryLimitLeavesBudgetForFailover(t *testing.T) {
	a, an := countingFail(t)
	good := upstub(t, http.StatusOK, `{"ok":true,"model":"night"}`)
	defer good.Close()
	d := newDispatcher()
	d.SetRetries(1)       // hop A stops after 2 dials on its OWN limit
	d.SetAttemptBudget(3) // one dial left for the failover
	req, body := reqWithBody(`{"model":"day","messages":[]}`)
	routes := []Route{textRoute(a.URL, "day"), textRoute(good.URL, "night")}
	rec := httptest.NewRecorder()
	_, res, err := d.ForwardCandidates(context.Background(), rec, req, routes, body)
	if err != nil {
		t.Fatalf("ForwardCandidates: %v", err)
	}
	if got := atomic.LoadInt64(an); got != 2 {
		t.Errorf("hop A dialed %d times, want its own retry limit of 2", got)
	}
	if res.Status != http.StatusOK {
		t.Errorf("the leftover budget must serve the failover, got %d", res.Status)
	}
	if res.UpstreamAttempts != 3 {
		t.Errorf("UpstreamAttempts = %d, want 3 (2 on A, 1 on the failover)", res.UpstreamAttempts)
	}
}

// When the BUDGET is hit first it stops the same-hop retries: hop A never
// reaches its own retry limit, and the request fails budget-spent rather
// than dialing a hop it cannot afford.
func TestAttemptBudgetStopsSameHopRetries(t *testing.T) {
	a, an := countingFail(t)
	b, bn := countingFail(t)
	d := newDispatcher()
	d.SetRetries(5) // A would dial 6 times on its own
	d.SetAttemptBudget(2)
	req, body := reqWithBody(`{"model":"day","messages":[]}`)
	routes := []Route{textRoute(a.URL, "day"), textRoute(b.URL, "night")}
	rec := httptest.NewRecorder()
	_, res, err := d.ForwardCandidates(context.Background(), rec, req, routes, body)
	if err == nil || !strings.Contains(err.Error(), "attempt budget") {
		t.Fatalf("err = %v, want the budget-spent failure", err)
	}
	if got := atomic.LoadInt64(an); got != 2 {
		t.Errorf("hop A dialed %d times, want exactly the budget of 2", got)
	}
	if got := atomic.LoadInt64(bn); got != 0 {
		t.Errorf("hop B dialed %d times, want 0 — nothing was left in the budget", got)
	}
	if res.UpstreamAttempts != 2 || res.AttemptBudget != 2 {
		t.Errorf("usage = %d/%d, want 2/2", res.UpstreamAttempts, res.AttemptBudget)
	}
}

// The selection deadline refuses to WAIT toward attempts it cannot afford:
// the backoff sleep is cut and the dispatch fails budget-spent rather than
// sleeping a full second per retry. (It bounds waiting, not a live stream —
// see the stream test below.)
func TestDispatchDeadlineCutsBackoffWait(t *testing.T) {
	a, _ := countingFail(t)
	d := newDispatcher()
	d.SetRetries(3) // would back off 1s, 2s, 4s
	d.SetAttemptBudget(10)
	d.SetDispatchDeadline(80 * time.Millisecond)
	req, body := reqWithBody(`{"model":"day","messages":[]}`)
	routes := []Route{textRoute(a.URL, "day")}
	rec := httptest.NewRecorder()
	start := time.Now()
	_, _, err := d.ForwardCandidates(context.Background(), rec, req, routes, body)
	elapsed := time.Since(start)
	if elapsed > 900*time.Millisecond {
		t.Errorf("dispatch took %v — the backoff sleeps are not bounded by the selection deadline", elapsed)
	}
	if err == nil {
		t.Error("a spent deadline must fail the dispatch, not serve a stale answer")
	}
}

// A served stream is never bounded by the selection deadline: the winning
// attempt holds the connection open longer than the deadline.
func TestDispatchDeadlineDoesNotTruncateStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		// A "stream" that runs past the 50ms selection deadline.
		for i := 0; i < 6; i++ {
			_, _ = w.Write([]byte("data: {\"chunk\":true}\n\n"))
			fl.Flush()
			time.Sleep(30 * time.Millisecond)
		}
	}))
	defer srv.Close()
	d := newDispatcher()
	d.SetRetries(0)
	d.SetAttemptBudget(4)
	d.SetDispatchDeadline(50 * time.Millisecond)
	req, body := reqWithBody(`{"model":"day","messages":[],"stream":true}`)
	routes := []Route{textRoute(srv.URL, "day")}
	rec := httptest.NewRecorder()
	_, res, err := d.ForwardCandidates(context.Background(), rec, req, routes, body)
	if err != nil {
		t.Fatalf("ForwardCandidates: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Fatalf("status = %d", res.Status)
	}
	if n := strings.Count(rec.Body.String(), "data:"); n != 6 {
		t.Errorf("stream delivered %d of 6 chunks — the selection deadline truncated a live stream", n)
	}
}

// Config defaults: 8 when absent, explicit 0 = unlimited, out-of-range fails.
func TestAttemptBudgetConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "c.yaml")
		if err := osWriteFile(p, s); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if c, err := config.Load(write("listen: 127.0.0.1:20129\n")); err != nil || c.Retry.RequestAttemptBudget != 8 {
		t.Errorf("absent key: budget = %d err = %v, want default 8", c.Retry.RequestAttemptBudget, err)
	}
	if c, err := config.Load(write("listen: 127.0.0.1:20129\nretry:\n  request_attempt_budget: 0\n")); err != nil || c.Retry.RequestAttemptBudget != 0 {
		t.Errorf("explicit 0 (unlimited) must win: budget = %d err = %v", c.Retry.RequestAttemptBudget, err)
	}
	if _, err := config.Load(write("listen: 127.0.0.1:20129\nretry:\n  request_attempt_budget: 999\n")); err == nil {
		t.Error("999 must fail validation")
	}
	if _, err := config.Load(write("listen: 127.0.0.1:20129\nretry:\n  dispatch_deadline: 100ms\n")); err == nil {
		t.Error("a sub-second dispatch deadline must fail validation")
	}
	if c, err := config.Load(write("listen: 127.0.0.1:20129\nretry:\n  dispatch_deadline: 45s\n")); err != nil || c.Retry.DispatchDeadlineStr != "45s" {
		t.Errorf("45s must be accepted: %q %v", c.Retry.DispatchDeadlineStr, err)
	}
}
