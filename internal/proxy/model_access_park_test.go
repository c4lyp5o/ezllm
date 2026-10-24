package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

const accessDeniedBody = `{"error":{"message":"Access to model denied. Please make sure your account has access to this model."}}`

// hostCountingTransport answers a scripted sequence of responses and records
// how many dials each upstream host received, so a test can tell "skipped"
// from "dialled and failed".
type hostCountingTransport struct {
	statuses []int
	bodies   []string
	mu       sync.Mutex
	perHost  map[string]int
	seq      int
}

func newHostCounting(statuses []int, bodies []string) *hostCountingTransport {
	return &hostCountingTransport{statuses: statuses, bodies: bodies, perHost: map[string]int{}}
}

func (t *hostCountingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.perHost[r.URL.Host]++
	i := t.seq
	if i >= len(t.statuses) {
		i = len(t.statuses) - 1
	}
	t.seq++
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode: t.statuses[i],
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(t.bodies[i])),
		Request:    r,
	}, nil
}

func (t *hostCountingTransport) dials(host string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.perHost[host]
}

func parkDispatcher(tr *hostCountingTransport) *Dispatcher {
	d := NewDispatcher(
		&http.Client{Transport: tr, Timeout: 5 * time.Second},
		provider.NewRegistry(&http.Client{Timeout: 5 * time.Second}))
	d.SetRetries(0)
	return d
}

func comboDispatch(d *Dispatcher, routes []Route) (Route, Result) {
	req, body := reqWithBody(`{"model":"combo","messages":[{"role":"user","content":"hi"}]}`)
	served, res, _ := d.ForwardCandidatesDoc(context.Background(), httptest.NewRecorder(), req, routes, body, nil)
	return served, res
}

// A 403 whose body names MODEL access is a fact about the hop: the same request
// is refused on every retry, so re-dialling it on each request of a block only
// spends a round trip and forces the failover hop (measured 2026-10-23: 12
// re-dials, then the cold-cache hop at 27% instead of 98%).
func TestModelAccessDeniedParksTheHop(t *testing.T) {
	// Dial order: 1st dispatch = hop1 (403) then hop2 (200); 2nd dispatch =
	// hop2 only, because hop1 is parked.
	tr := newHostCounting(
		[]int{http.StatusForbidden, http.StatusOK, http.StatusOK},
		[]string{accessDeniedBody, `{"ok":true}`, `{"ok":true}`})
	d := parkDispatcher(tr)
	routes := comboRoutes()

	comboDispatch(d, routes)
	if n := tr.dials("hop1.invalid"); n != 1 {
		t.Fatalf("first dispatch should dial the denied hop once, got %d", n)
	}

	// The second request must SKIP the parked hop entirely.
	comboDispatch(d, routes)
	if n := tr.dials("hop1.invalid"); n != 1 {
		t.Fatalf("the denied hop must be parked after the first refusal, dials went to %d", n)
	}

	cooldowns := d.Cooldowns()
	if len(cooldowns) != 1 {
		t.Fatalf("expected exactly one parked hop, got %d", len(cooldowns))
	}
	if cooldowns[0].Reason != "model access denied" {
		t.Fatalf("park reason = %q, want %q", cooldowns[0].Reason, "model access denied")
	}
	if cooldowns[0].Model != "model-one" {
		t.Fatalf("park must be model-scoped, got model %q", cooldowns[0].Model)
	}
}

// The control: a 403 that describes the REQUEST must not park a healthy hop.
// One wasted round trip beats taking a working endpoint out of rotation on a
// caller's mistake.
func TestRequestShaped403DoesNotParkTheHop(t *testing.T) {
	const body = `{"error":{"message":"tools are not supported for this request shape"}}`
	tr := newHostCounting(
		[]int{http.StatusForbidden, http.StatusOK, http.StatusForbidden, http.StatusOK},
		[]string{body, `{"ok":true}`, body, `{"ok":true}`})
	d := parkDispatcher(tr)
	routes := comboRoutes()

	comboDispatch(d, routes)
	comboDispatch(d, routes)
	if n := tr.dials("hop1.invalid"); n != 2 {
		t.Fatalf("a request-shaped 403 must not park the hop: expected 2 dials, got %d", n)
	}
	if c := d.Cooldowns(); len(c) != 0 {
		t.Fatalf("no hop should be parked, got %d", len(c))
	}
}
