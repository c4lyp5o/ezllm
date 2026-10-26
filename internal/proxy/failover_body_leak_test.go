package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// Every response body handed to the failover loop must be closed exactly once.
// The loop parks a failed hop's response in `fallbackResp` so the last real
// upstream error can still be committed; that parked body used to be abandoned
// on one early return (a later hop whose request body cannot be prepared),
// leaking the response and its upstream connection.
type probeBody struct {
	r      io.Reader
	closes *atomic.Int32
}

func (b *probeBody) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b *probeBody) Close() error {
	b.closes.Add(1)
	return nil
}

type seqTransport struct {
	statuses []int
	bodies   []string
	calls    *atomic.Int32
	closes   *atomic.Int32
}

func (t *seqTransport) RoundTrip(*http.Request) (*http.Response, error) {
	i := int(t.calls.Add(1)) - 1
	if i >= len(t.statuses) {
		i = len(t.statuses) - 1
	}
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode: t.statuses[i],
		Header:     h,
		Body:       &probeBody{r: strings.NewReader(t.bodies[i]), closes: t.closes},
		Request:    &http.Request{},
	}, nil
}

func leakDispatcher(statuses []int, bodies []string, calls, closes *atomic.Int32) *Dispatcher {
	d := NewDispatcher(
		&http.Client{Transport: &seqTransport{statuses: statuses, bodies: bodies, calls: calls, closes: closes}, Timeout: 5 * time.Second},
		provider.NewRegistry(&http.Client{Timeout: 5 * time.Second}))
	// One dial per hop: this is about failover ownership, not retry policy.
	d.SetRetries(0)
	return d
}

func comboRoutes() []Route {
	// Distinct models so the cooldown key (account|base|model) can never make
	// the second hop read as "the same parked hop".
	rt1 := testRoute("http://hop1.invalid", provider.SurfaceChat)
	rt1.Model = "model-one"
	rt1.Alias = "combo"
	rt2 := testRoute("http://hop2.invalid", provider.SurfaceChat)
	rt2.Model = "model-two"
	rt2.Alias = "combo"
	return []Route{rt1, rt2}
}

// The early return: hop 1 answers 500 (so it is parked), then hop 2's body
// cannot be prepared for its wire, and the loop leaves before the code that
// would have closed the parked response.
func TestFailoverClosesParkedResponseOnEarlyReturn(t *testing.T) {
	var calls, closes atomic.Int32
	d := leakDispatcher([]int{http.StatusInternalServerError}, []string{`{"error":"boom"}`}, &calls, &closes)

	routes := comboRoutes()
	// A wire pair with no converter: prepareHopBody fails with a plain error,
	// which is the early-return branch (a typed portability refusal would skip
	// the hop instead).
	routes[1].WireSurface = provider.SurfaceEmbeddings

	req, body := reqWithBody(`{"model":"combo","messages":[{"role":"user","content":"hi"}]}`)
	w := httptest.NewRecorder()

	_, _, err := d.ForwardCandidatesDoc(context.Background(), w, req, routes, body, nil)
	if err == nil {
		t.Fatalf("expected the non-portability error to surface, got nil")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("only the first hop should have been dialed, got %d dials", n)
	}
	if n := closes.Load(); n != 1 {
		t.Fatalf("the parked fallback must be closed exactly once on an early return, got %d closes", n)
	}
}

// The ordinary failover path: hop 1 is parked, hop 2 serves, and the parked
// response is discarded (closed) before commit takes the winner.
func TestFailoverClosesParkedResponseWhenLaterHopServes(t *testing.T) {
	var calls, closes atomic.Int32
	d := leakDispatcher(
		[]int{http.StatusInternalServerError, http.StatusOK},
		[]string{`{"error":"boom"}`, `{"ok":true}`},
		&calls, &closes)

	req, body := reqWithBody(`{"model":"combo","messages":[{"role":"user","content":"hi"}]}`)
	w := httptest.NewRecorder()

	served, res, err := d.ForwardCandidatesDoc(context.Background(), w, req, comboRoutes(), body, nil)
	if err != nil {
		t.Fatalf("hop 2 should have served, got error: %v", err)
	}
	if served.Model != "model-two" {
		t.Fatalf("expected hop 2 to serve, got %q", served.Model)
	}
	if res.Status != http.StatusOK {
		t.Fatalf("expected 200 to the client, got %d", res.Status)
	}
	// Two bodies were opened: the parked 500 and the committed 200.
	if n := closes.Load(); n != 2 {
		t.Fatalf("both opened bodies must be closed (parked + committed), got %d closes", n)
	}
}
