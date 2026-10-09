package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// upstub is a fake upstream that answers with a fixed status and body.
func upstub(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
}

// textRoute is testRoute pointed at a specific stub URL with a specific model —
// failover tests need several routes with DIFFERENT models, which the shared
// helper cannot express.
func textRoute(url, model string) Route {
	rt := testRoute(url, provider.SurfaceOpenAI)
	rt.Model = model
	rt.Alias = "combo"
	return rt
}

func reqWithBody(body string) (*http.Request, []byte) {
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req, []byte(body)
}

// THE core failover claim: hop 1 answers 429, hop 2 answers 200, and the client
// must see ONLY hop 2 — the 429 is never observable. If this breaks, failover
// is not failover, it is a leak of an error the caller did nothing to deserve.
func TestRetryableUpstreamStatus(t *testing.T) {
	for _, code := range []int{http.StatusTooManyRequests, http.StatusRequestTimeout, http.StatusTooEarly, 500, 502, 503, 504} {
		if !retryableUpstreamStatus(code) {
			t.Errorf("%d should be retryable", code)
		}
	}
	for _, code := range []int{400, 401, 403, 404, 422} {
		if retryableUpstreamStatus(code) {
			t.Errorf("%d should not be retryable", code)
		}
	}
}

func TestFailoverHidesFailedHopFromClient(t *testing.T) {
	bad := upstub(t, http.StatusTooManyRequests, `{"error":"quota"}`)
	defer bad.Close()
	good := upstub(t, http.StatusOK, `{"ok":true,"model":"night"}`)
	defer good.Close()

	d := newDispatcher()
	req, body := reqWithBody(`{"model":"day","messages":[]}`)
	rec := httptest.NewRecorder()

	routes := []Route{textRoute(bad.URL, "day"), textRoute(good.URL, "night")}
	_, res, err := d.ForwardCandidates(context.Background(), rec, req, routes, body)
	if err != nil {
		t.Fatalf("ForwardCandidates: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("client saw status %d, want 200 (failed hop leaked)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "quota") {
		t.Errorf("failed hop's error body leaked to the client: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "night") {
		t.Errorf("expected hop 2's body, got %s", rec.Body.String())
	}
	if res.Status != http.StatusOK {
		t.Errorf("ledger status = %d, want 200 (the committed hop)", res.Status)
	}
}

// A 400 describes the BODY we are about to replay unchanged. Retrying it would
// burn every key in the combo to earn the identical rejection — so the last
// (or only) candidate's 400 must reach the client as-is, and no earlier hop
// should be attempted on a body error.
func TestBodyErrorsAreNotRetried(t *testing.T) {
	var calls int
	badReq := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"bad request"}`)
	}))
	defer badReq.Close()
	ok := upstub(t, http.StatusOK, `{"ok":true}`)
	defer ok.Close()

	d := newDispatcher()
	req, body := reqWithBody(`{"model":"day","messages":[]}`)
	rec := httptest.NewRecorder()

	routes := []Route{textRoute(badReq.URL, "day"), textRoute(ok.URL, "night")}
	_, _, err := d.ForwardCandidates(context.Background(), rec, req, routes, body)
	if err != nil {
		t.Fatalf("ForwardCandidates: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 passed through (body errors are not hop-specific)", rec.Code)
	}
	if calls != 1 {
		t.Errorf("upstream called %d times, want 1 — a 400 must not trigger failover", calls)
	}
}

// When EVERY hop fails, the client gets the real last status, not an
// invented one. Fabricating a 200/502 here would hide a genuine outage.
func TestAllHopsFailingReturnsRealStatus(t *testing.T) {
	a := upstub(t, http.StatusServiceUnavailable, `{"error":"down-a"}`)
	defer a.Close()
	b := upstub(t, http.StatusTooManyRequests, `{"error":"quota-b"}`)
	defer b.Close()

	d := newDispatcher()
	req, body := reqWithBody(`{"model":"day","messages":[]}`)
	rec := httptest.NewRecorder()

	routes := []Route{textRoute(a.URL, "day"), textRoute(b.URL, "night")}
	_, res, err := d.ForwardCandidates(context.Background(), rec, req, routes, body)
	if err != nil {
		t.Fatalf("a committed response is not an error: %v", err)
	}
	// Hop 1's 503 is abandoned; hop 2 is final and must be committed verbatim.
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 (the LAST hop's real answer)", rec.Code)
	}
	if res.Status != http.StatusTooManyRequests {
		t.Errorf("ledger status = %d, want 429", res.Status)
	}
	if !strings.Contains(rec.Body.String(), "quota-b") {
		t.Errorf("want the last hop's body, got %q", rec.Body.String())
	}
}

func TestEndpointCooldownSkipsFailedHopOnNextRequest(t *testing.T) {
	var deadHits, goodHits int
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadHits++
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"down"}`)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodHits++
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer good.Close()

	d := newDispatcher()
	d.SetRetries(0)
	routes := []Route{textRoute(bad.URL, "day"), textRoute(good.URL, "night")}
	for i := 0; i < 2; i++ {
		req, body := reqWithBody(`{"model":"day","messages":[]}`)
		rec := httptest.NewRecorder()
		_, _, err := d.ForwardCandidates(context.Background(), rec, req, routes, body)
		if err != nil || rec.Code != http.StatusOK {
			t.Fatalf("request %d: status=%d err=%v body=%s", i, rec.Code, err, rec.Body.String())
		}
	}
	if deadHits != 1 || goodHits != 2 {
		t.Fatalf("hits bad=%d good=%d, want 1 and 2 (bad endpoint should cool down)", deadHits, goodHits)
	}
}

func TestAllCooldownRoutesReturnErrorWithoutUpstreamCall(t *testing.T) {
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer up.Close()
	d := newDispatcher()
	rt := textRoute(up.URL, "model")
	d.recordHop(rt, true, time.Now())
	req, body := reqWithBody(`{"model":"model","messages":[]}`)
	rec := httptest.NewRecorder()
	_, _, err := d.ForwardCandidates(context.Background(), rec, req, []Route{rt}, body)
	if err == nil || !strings.Contains(err.Error(), "cooldown") {
		t.Fatalf("error=%v, want cooldown error", err)
	}
	if hits != 0 || rec.Body.Len() != 0 {
		t.Fatalf("cooldown bypassed: hits=%d body=%q", hits, rec.Body.String())
	}
}

func TestHalfOpenAllowsExactlyOneProbe(t *testing.T) {
	d := newDispatcher()
	rt := textRoute("http://upstream.invalid", "model")
	now := time.Now()
	d.recordHop(rt, true, now.Add(-deadHopCooldown-time.Second))
	ok, probe := d.acquireHop(rt, now)
	if !ok || !probe {
		t.Fatalf("first expired-cooldown caller=(%v,%v), want (true,true)", ok, probe)
	}
	ok, probe = d.acquireHop(rt, now)
	if ok || probe {
		t.Fatalf("concurrent caller=(%v,%v), want (false,false)", ok, probe)
	}
	d.recordHop(rt, false, now)
	ok, probe = d.acquireHop(rt, now)
	if !ok || probe {
		t.Fatalf("recovered endpoint caller=(%v,%v), want (true,false)", ok, probe)
	}
}

func TestTransportFailureFailsOver(t *testing.T) {
	dead := "http://127.0.0.1:1" // nothing listens here
	ok := upstub(t, http.StatusOK, `{"ok":true}`)
	defer ok.Close()

	d := newDispatcher()
	req, body := reqWithBody(`{"model":"day","messages":[]}`)
	rec := httptest.NewRecorder()

	routes := []Route{textRoute(dead, "day"), textRoute(ok.URL, "night")}
	served, res, err := d.ForwardCandidates(context.Background(), rec, req, routes, body)
	if err != nil {
		t.Fatalf("should have failed over to hop 2: %v", err)
	}
	if served.Account.BaseURL != ok.URL {
		t.Errorf("served route = %s, want hop 2 (%s) — ledger must credit the hop that answered", served.Account.BaseURL, ok.URL)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if res.Status != http.StatusOK {
		t.Errorf("ledger status = %d, want 200", res.Status)
	}
}

// A single route must behave exactly as before the refactor: same status, same
// body, one upstream call. The 8 pre-existing Forward tests depend on this.
func TestSingleRouteUnchanged(t *testing.T) {
	up := upstub(t, http.StatusCreated, `{"ok":1}`)
	defer up.Close()

	d := newDispatcher()
	req, body := reqWithBody(`{"model":"m","messages":[]}`)
	rec := httptest.NewRecorder()

	res, err := d.Forward(context.Background(), rec, req, textRoute(up.URL, "m"), body)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if rec.Code != http.StatusCreated || res.Status != http.StatusCreated {
		t.Errorf("status = %d / %d, want 201 / 201", rec.Code, res.Status)
	}
}

// Each hop must get ITS OWN model in the request body. Reusing one pre-swapped
// body would send "day" to the night hop — a silent wrong-model answer, which
// is worse than an error.
func TestEachHopGetsItsOwnModel(t *testing.T) {
	var seen []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, string(b))
		w.WriteHeader(http.StatusBadRequest) // force failover to hop 2
		_, _ = io.WriteString(w, `{"error":"no"}`)
	}))
	defer up.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, string(b))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":1}`)
	}))
	defer second.Close()

	d := newDispatcher()
	req, body := reqWithBody(`{"model":"day","messages":[]}`)
	rec := httptest.NewRecorder()

	routes := []Route{textRoute(up.URL, "day"), textRoute(second.URL, "night")}
	if _, _, err := d.ForwardCandidates(context.Background(), rec, req, routes, body); err != nil {
		t.Fatalf("ForwardCandidates: %v", err)
	}
	// hop 1 gets a 400 → not retried, so hop 2 is never called. Assert the
	// honest outcome: exactly one body, carrying hop 1's model.
	if len(seen) != 1 {
		t.Fatalf("upstream calls = %d, want 1 (400 must not fail over)", len(seen))
	}
	if !strings.Contains(seen[0], `"day"`) {
		t.Errorf("hop 1 body = %s, want its own model", seen[0])
	}
}

// Model swap per hop, on a failover that IS allowed: hop 1 returns 429 (retry),
// hop 2 must receive "night" in the body, not the stale "day".
func TestFailoverSendsCorrectModelToSecondHop(t *testing.T) {
	var secondBody string
	first := upstub(t, http.StatusTooManyRequests, `{"error":"quota"}`)
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		secondBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":1}`)
	}))
	defer second.Close()

	d := newDispatcher()
	req, body := reqWithBody(`{"model":"day","messages":[]}`)
	rec := httptest.NewRecorder()

	routes := []Route{textRoute(first.URL, "day"), textRoute(second.URL, "night")}
	if _, _, err := d.ForwardCandidates(context.Background(), rec, req, routes, body); err != nil {
		t.Fatalf("ForwardCandidates: %v", err)
	}
	if !strings.Contains(secondBody, `"night"`) {
		t.Errorf("second hop received %s, want it swapped to its own model", secondBody)
	}
	if strings.Contains(secondBody, `"day"`) {
		t.Errorf("second hop got hop 1's model: %s", secondBody)
	}
}

// Regression: the ledger must credit the hop that ANSWERED, not hop 1.
//
// Found live after the first end-to-end run: alias=daynight was recorded as
// account=deadprobe status=200 — the FAILED hop billed for hop 2's work. Any
// per-account usage cap reads that column, so the bug would silently meter the
// wrong budget. This pins the fix at the dispatcher boundary.
func TestServedRouteIsTheOneThatCommitted(t *testing.T) {
	bad := upstub(t, http.StatusTooManyRequests, `{"error":"quota"}`)
	defer bad.Close()
	good := upstub(t, http.StatusOK, `{"ok":true}`)
	defer good.Close()

	d := newDispatcher()
	req, body := reqWithBody(`{"model":"day","messages":[]}`)
	rec := httptest.NewRecorder()

	routes := []Route{textRoute(bad.URL, "day"), textRoute(good.URL, "night")}
	served, res, err := d.ForwardCandidates(context.Background(), rec, req, routes, body)
	if err != nil {
		t.Fatalf("ForwardCandidates: %v", err)
	}
	if served.Account.BaseURL != good.URL {
		t.Errorf("served hop = %s, want the GOOD hop — a failed hop must never be credited", served.Account.BaseURL)
	}
	if res.Status != http.StatusOK {
		t.Errorf("ledger status = %d, want 200", res.Status)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("client status = %d, want 200", rec.Code)
	}
}

// When the ONLY hop fails, the served route is still that hop — its real
// answer must be both returned to the client and recorded against it.
func TestServedRouteWhenSoleHopFails(t *testing.T) {
	only := upstub(t, http.StatusTooManyRequests, `{"error":"quota"}`)
	defer only.Close()

	d := newDispatcher()
	req, body := reqWithBody(`{"model":"day","messages":[]}`)
	rec := httptest.NewRecorder()

	routes := []Route{textRoute(only.URL, "day")}
	served, res, err := d.ForwardCandidates(context.Background(), rec, req, routes, body)
	if err != nil {
		t.Fatalf("ForwardCandidates: %v", err)
	}
	if served.Account.BaseURL != only.URL {
		t.Errorf("served = %s, want the sole hop", served.Account.BaseURL)
	}
	if res.Status != http.StatusTooManyRequests || rec.Code != http.StatusTooManyRequests {
		t.Errorf("ledger=%d client=%d, want 429 both", res.Status, rec.Code)
	}
}
