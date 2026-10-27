package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/ratelimit"
)

// A caller that hangs up is not a hop failure, and must not take the hop out of
// rotation.
//
// Measured in prod: every 502 in a seven-day window (80 rows) was
// "context canceled", 38 of them on `daily`/anthropic, and eight later requests
// were answered "all combo endpoints are in five-minute cooldown". The cause is
// one line: classifyHopPolicy treats a canceled caller context as a transport
// error, banks a strike on the hop, and the default threshold parks it for the
// full cooldown — so closing a browser tab removes a healthy endpoint for five
// minutes and the next real request is refused from a local counter.
func TestClientCancelDoesNotParkTheHop(t *testing.T) {
	up := upstub(t, http.StatusOK, `{"ok":true,"model":"day"}`)
	defer up.Close()

	d := newDispatcher()
	req, body := reqWithBody(`{"model":"day","messages":[]}`)
	rec := httptest.NewRecorder()

	// The caller is already gone: every dial fails with context.Canceled
	// before a byte reaches the upstream.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	routes := []Route{textRoute(up.URL, "day"), textRoute(up.URL+"/second", "night")}
	_, res, err := d.ForwardCandidates(ctx, rec, req, routes, body)
	if err == nil {
		t.Fatal("want the cancellation to surface as an error")
	}

	if cos := d.Cooldowns(); len(cos) != 0 {
		t.Fatalf("Cooldowns() = %+v, want none — a client hanging up must not park a healthy hop", cos)
	}

	// The trail is the operator's explanation of the 502; it must not dress a
	// cancellation as a transport fault on the hop.
	for _, a := range res.Attempts {
		if strings.Contains(a.Note, "transport") {
			t.Errorf("attempt note = %q, want it to name the cancellation, not a transport fault", a.Note)
		}
	}
}

// Once the caller has left, the failover loop stops: no later hop is dialed,
// charged against its account's request budget, or written into the trail.
// Before the fix every remaining hop ran reserveRate on the dead context, so a
// closed tab during hop 1 spent a budget slot on each later account and the
// next real request was refused as "at its request budget".
func TestClientCancelStopsTheFailoverLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Hop 1: the caller hangs up while this hop is answering with a failover
	// status, so without the fix the loop would move on to the next hop.
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer first.Close()
	var laterHits int32
	later := countingStub(t, http.StatusOK, `{"ok":true}`, &laterHits)
	defer later.Close()

	d := newDispatcher()
	d.SetRetries(0)
	d.SetLimiter(ratelimit.New(nil))
	budgeted := rateRoute(later.URL, "night", 200, 1, time.Minute) // ONE request per minute
	routes := []Route{
		rateRoute(first.URL, "day", 100, 0, 0),
		budgeted,
		rateRoute(later.URL, "dusk", 300, 0, 0),
	}

	req, body := reqWithBody(`{"model":"day","messages":[]}`)
	_, res, err := d.ForwardCandidates(ctx, httptest.NewRecorder(), req, routes, body)
	if err == nil {
		t.Fatal("want the cancellation to surface as an error")
	}
	if len(res.Attempts) != 1 {
		t.Errorf("attempt trail = %+v, want only the hop the caller abandoned", res.Attempts)
	}

	// The budgeted account's single slot must still be there.
	req, body = reqWithBody(`{"model":"night","messages":[]}`)
	rec := httptest.NewRecorder()
	if _, _, err := d.ForwardCandidates(context.Background(), rec, req, []Route{budgeted}, body); err != nil {
		t.Fatalf("follow-up request refused: %v — the cancelled request spent this account's budget", err)
	}
	if got := atomic.LoadInt32(&laterHits); got != 1 {
		t.Errorf("later upstream hits = %d, want 1 (only the follow-up)", got)
	}
}
