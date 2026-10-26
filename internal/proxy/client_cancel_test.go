package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
