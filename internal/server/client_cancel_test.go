package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
)

// switchTransport hangs while `hang` is set and passes through otherwise. The
// hang is the shape production sees when a browser tab closes or an agent run is
// cancelled mid-flight; the pass-through lets the same server serve the
// follow-up request that proves the hop was not parked.
type switchTransport struct{ hang atomic.Bool }

func (s *switchTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if s.hang.Load() {
		<-r.Context().Done()
		return nil, r.Context().Err()
	}
	return http.DefaultTransport.RoundTrip(r)
}

// A caller that hangs up must be RECORDED as a cancellation and must not take
// the hop out of rotation.
//
// This is the black-box half of the same defect the proxy test pins: prod showed
// 80 "context canceled" 502s in seven days and eight follow-up requests answered
// "all combo endpoints are in five-minute cooldown". Asserting the ledger row
// alone would pass while the hop stayed parked, so the second half sends a real
// request afterwards — the only thing that proves the park is gone.
func TestClientCancelIsRecordedAs499AndDoesNotParkTheHop(t *testing.T) {
	// The dispatcher hangs until the caller gives up, then reports the cancel.
	tr := &switchTransport{}
	tr.hang.Store(true)
	hanging := proxy.NewDispatcher(&http.Client{Transport: tr}, provider.NewRegistry(&http.Client{}))
	tu := newTwoUpstream(t, false, func(o *Options) { o.Dispatcher = hanging })

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"daily","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer tok-infer")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	tu.handler.ServeHTTP(rec, req)

	// The ledger row: 499, named as a cancellation, not a gateway fault.
	deadline := time.Now().Add(2 * time.Second)
	for {
		calls, err := tu.db.RecentCalls(context.Background(), 5)
		if err != nil {
			t.Fatalf("RecentCalls: %v", err)
		}
		if len(calls) > 0 {
			c := calls[0]
			if c.Status != statusClientClosedRequest {
				t.Fatalf("ledger status = %d, want %d (client closed request), err=%q", c.Status, statusClientClosedRequest, c.Err)
			}
			if !strings.Contains(c.Err, "client cancelled") {
				t.Errorf("ledger err = %q, want it to name the cancellation", c.Err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no ledger row appeared for the cancelled request")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The hop is still usable: the next request must be SERVED, not refused from
	// the five-minute park a cancellation would otherwise have set on both hops.
	tr.hang.Store(false)
	code, body := tu.chat(`{"model":"daily","messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 {
		t.Fatalf("follow-up request = %d %s, want 200 — the cancellation parked a healthy hop", code, body)
	}
}
