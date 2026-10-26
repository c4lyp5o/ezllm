package proxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	rt := testRoute(url, provider.SurfaceChat)
	rt.Model = model
	rt.Alias = "combo"
	return rt
}

func reqWithBody(body string) (*http.Request, []byte) {
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req, []byte(body)
}

// Retry and failover are DIFFERENT questions. A 429 must never be retried on
// the same hop — retrying a rate-limited credential spends the very budget the
// upstream just reported as exhausted, which is what turned one over-budget
// call into a minute-long storm. It must still fail over, so a hop with budget
// can serve the request.
func TestRetryableUpstreamStatus(t *testing.T) {
	for _, code := range []int{http.StatusRequestTimeout, http.StatusTooEarly, 500, 502, 503, 504} {
		if !retryableUpstreamStatus(code) {
			t.Errorf("%d should be retryable", code)
		}
	}
	// 429 is a failover signal, not a retry signal: not retryable, but a
	// failover trigger. Asserting both halves keeps a future edit from
	// "fixing" one by breaking the other.
	if retryableUpstreamStatus(http.StatusTooManyRequests) {
		t.Error("429 must not be retryable on the same hop — it is a failover signal")
	}
	if !failoverStatus(http.StatusTooManyRequests) {
		t.Error("429 must fail over — otherwise a rate-limited hop strands the request")
	}
	for _, code := range []int{400, 401, 403, 404, 422} {
		if retryableUpstreamStatus(code) {
			t.Errorf("%d should not be retryable", code)
		}
	}
}

// THE core failover claim: hop 1 answers 429, hop 2 answers 200, and the client
// must see ONLY hop 2 — the 429 is never observable. If this breaks, failover
// is not failover, it is a leak of an error the caller did nothing to deserve.
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
	d.recordHop(rt, hopOutcome{failed: true, cooldown: defaultHopCooldown}, time.Now())
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
	d.recordHop(rt, hopOutcome{failed: true, cooldown: defaultHopCooldown}, now.Add(-defaultHopCooldown-time.Second))
	ok, probe := d.acquireHop(rt, now)
	if !ok || !probe {
		t.Fatalf("first expired-cooldown caller=(%v,%v), want (true,true)", ok, probe)
	}
	ok, probe = d.acquireHop(rt, now)
	if ok || probe {
		t.Fatalf("concurrent caller=(%v,%v), want (false,false)", ok, probe)
	}
	d.recordHop(rt, hopOutcome{}, now)
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

// ── park visibility and probe bounds ────────────────────────────────────────

// An EXPIRED park must not keep refusing callers just because one recovery probe
// is slow. Live 2026-10-11: a direct probe to opencode-go was answered with
// "all combo endpoints are in five-minute cooldown" while combo traffic reached
// the same account seconds earlier — a probe against an 11-14s-TTFT provider was
// holding the gate closed on a park that had already expired.
func TestExpiredParkDoesNotBlockBehindAStaleProbe(t *testing.T) {
	d := newDispatcher()
	rt := textRoute("http://upstream.invalid", "model")
	now := time.Now()
	d.recordHop(rt, hopOutcome{failed: true, cooldown: defaultHopCooldown, reason: "transport error"},
		now.Add(-defaultHopCooldown-time.Second))

	// First caller after expiry wins the half-open probe.
	ok, probe := d.acquireHop(rt, now)
	if !ok || !probe {
		t.Fatalf("expired-cooldown caller = (%v,%v), want (true,true)", ok, probe)
	}
	// A concurrent caller is held off while that probe is FRESH — the
	// anti-herd intent of the election is preserved.
	if ok, _ := d.acquireHop(rt, now.Add(time.Second)); ok {
		t.Error("a fresh probe must hold off concurrent callers")
	}
	// Past probeHold the hop is eligible again: an expired park must never read
	// as parked, or a healthy endpoint silently disappears from every combo.
	if ok, _ := d.acquireHop(rt, now.Add(probeHold+time.Second)); !ok {
		t.Error("a stale probe must not keep an expired park closed")
	}
}

// The bound above must not have opened the gate on a park that is still LIVE.
func TestLiveParkStillRefusesCallers(t *testing.T) {
	d := newDispatcher()
	rt := textRoute("http://upstream.invalid", "model")
	now := time.Now()
	d.recordHop(rt, hopOutcome{failed: true, cooldown: defaultHopCooldown, reason: "429 rate limited"}, now)
	if ok, probe := d.acquireHop(rt, now.Add(time.Minute)); ok || probe {
		t.Errorf("live 5-minute park caller = (%v,%v), want (false,false)", ok, probe)
	}
}

// Cooldowns() is the ONLY place a parked hop is visible: the request never
// reaches an upstream, so no ledger row is written and the only other symptom is
// a combo quietly using fewer hops than it has.
func TestCooldownsReportsCause(t *testing.T) {
	d := newDispatcher()
	rt := textRoute("http://upstream.invalid", "model")
	now := time.Now()
	d.recordHop(rt, hopOutcome{failed: true, cooldown: defaultHopCooldown, reason: "429 rate limited"}, now)

	got := d.Cooldowns()
	if len(got) != 1 {
		t.Fatalf("cooldowns = %d, want 1", len(got))
	}
	if got[0].Namespace != "test" {
		t.Errorf("namespace = %q, want the parked account's namespace", got[0].Namespace)
	}
	if got[0].Reason != "429 rate limited" {
		t.Errorf("reason = %q, want the recorded cause", got[0].Reason)
	}
	if !got[0].Until.After(now) {
		t.Errorf("until = %v, want a future instant for a live park", got[0].Until)
	}

	// An expired, idle park holds nothing back and must not be reported as if
	// it did — the payload is read as "what is being skipped right now".
	idle := newDispatcher()
	idle.recordHop(rt, hopOutcome{failed: true, cooldown: defaultHopCooldown, reason: "transport error"},
		now.Add(-defaultHopCooldown-time.Minute))
	if l := idle.Cooldowns(); len(l) != 0 {
		t.Errorf("expired idle park reported: %+v", l)
	}
}

// classifyHopPolicy must name the cause it parks for, or the health payload has
// a reason field that is always empty.
func TestClassifyHopRecordsReason(t *testing.T) {
	d := newDispatcher()
	hdr := http.Header{}
	if got := d.classifyHopPolicy(context.Background(), nil, http.StatusTooManyRequests, hdr, true); got.reason == "" {
		t.Error("429 park must carry a reason")
	}
	if got := d.classifyHopPolicy(context.Background(), nil, http.StatusBadGateway, hdr, true); !strings.Contains(got.reason, "502") {
		t.Errorf("5xx reason = %q, want the status named", got.reason)
	}
	// A 4xx that describes the REQUEST must not park, and so has no reason.
	if got := d.classifyHopPolicy(context.Background(), nil, http.StatusBadRequest, hdr, true); got.failed || got.reason != "" {
		t.Errorf("400 = %+v, want no park and no reason", got)
	}
}

// ── park scope and failover visibility ──────────────────────────────────────

// Parking is per (account, endpoint, MODEL). A 429 is nearly always a per-model
// condition — opencode-go meters each model separately — so an account-wide key
// meant one throttled model removed every other model on that account from every
// combo that referenced it. Measured live 2026-10-11: a single unavailable
// opencode-go model took all 38 of the account's models out of routing for 60s
// at a time while the other models were perfectly healthy.
func TestParkIsScopedToTheModel(t *testing.T) {
	d := newDispatcher()
	now := time.Now()
	throttled := textRoute("http://upstream.invalid", "step-5-preview-free")
	healthy := textRoute("http://upstream.invalid", "kimi-k3")
	if throttled.Account.ID != healthy.Account.ID {
		t.Fatal("fixture: both models must share one account for this test to mean anything")
	}

	d.recordHop(throttled, hopOutcome{failed: true, cooldown: defaultHopCooldown, reason: "429 rate limited"}, now)

	if ok, _ := d.acquireHop(throttled, now.Add(time.Second)); ok {
		t.Error("the throttled model must be parked")
	}
	if ok, _ := d.acquireHop(healthy, now.Add(time.Second)); !ok {
		t.Error("a sibling model on the SAME account was parked too — park scope is account-wide again")
	}

	// The health payload must say WHICH model is cooling, not just the account.
	got := d.Cooldowns()
	if len(got) != 1 {
		t.Fatalf("cooldowns = %d, want 1", len(got))
	}
	if got[0].Model != "step-5-preview-free" {
		t.Errorf("cooldown model = %q, want the parked model named", got[0].Model)
	}
}

// Failover is invisible to the client by design, so it must be visible to the
// operator: a skipped (parked) hop and a failing-over hop both write NO ledger
// row — the request never leaves the box, or never commits — which is how a combo
// silently ran on one hop for hours with no explanation anywhere.
func TestFailoverIsLoggedForTheOperator(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, nil)))
	defer slog.SetDefault(prev)

	d := newDispatcher()
	now := time.Now()
	rt := textRoute("http://upstream.invalid", "model")
	d.recordHop(rt, hopOutcome{failed: true, cooldown: defaultHopCooldown, reason: "429 rate limited"}, now)

	// A skipped hop must say so, and say why.
	if ok, _ := d.acquireHop(rt, now); ok {
		t.Fatal("fixture: the hop should be parked")
	}
	if ok, _ := d.acquireHop(textRoute("http://upstream.invalid", "other"), now); !ok {
		t.Fatal("fixture: a sibling model must stay eligible")
	}
	req, body := reqWithBody(`{"model":"model","messages":[]}`)
	rec := httptest.NewRecorder()
	_, _, _ = d.ForwardCandidates(context.Background(), rec, req, []Route{rt}, body)

	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if !strings.Contains(out, "hop skipped") {
		t.Errorf("a parked hop must be logged; log was:\n%s", out)
	}
}

// lockedWriter keeps the test's buffer safe: the dispatcher logs from the
// request goroutine while the test reads after it returns, and `go test -race`
// would otherwise flag an unsynchronised write.
type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// A transport failure must leave the hop parked. The error branch once
// recorded a second, empty outcome that read as success and deleted the park
// it had just set — while the log still said "hop parked".
func TestTransportFailureParksTheHop(t *testing.T) {
	ok := upstub(t, http.StatusOK, `{"ok":true}`)
	defer ok.Close()

	d := newDispatcher()
	d.SetRetries(0)
	req, body := reqWithBody(`{"model":"day","messages":[]}`)
	routes := []Route{textRoute("http://127.0.0.1:1", "day"), textRoute(ok.URL, "night")}
	if _, _, err := d.ForwardCandidates(context.Background(), httptest.NewRecorder(), req, routes, body); err != nil {
		t.Fatalf("should have failed over: %v", err)
	}
	if allowed, _ := d.acquireHop(routes[0], time.Now()); allowed {
		t.Error("dead hop is not parked after a transport error")
	}
}

// A hop retried on a 5xx until the request's attempt budget ran out DID
// fail: the budget refusal must not erase that, or the answered 503 is
// recorded as a success and clears the strikes the hop had banked.
func TestBudgetSpentAfterA5xxStillStrikesTheHop(t *testing.T) {
	bad := upstub(t, http.StatusServiceUnavailable, `{"error":"down"}`)
	defer bad.Close()

	d := newDispatcher()
	d.SetRetries(1)
	d.SetAttemptBudget(1)
	d.ConfigureCooldown(0, 0, 3)
	routes := []Route{textRoute(bad.URL, "day"), textRoute(bad.URL, "night")}
	d.recordHop(routes[0], hopOutcome{failed: true, cooldown: time.Minute}, time.Now())

	req, body := reqWithBody(`{"model":"day","messages":[]}`)
	_, _, _ = d.ForwardCandidates(context.Background(), httptest.NewRecorder(), req, routes, body)
	if st := d.cooldowns[hopKey(routes[0])]; st.strikes != 2 {
		t.Errorf("hop strikes = %d, want 2 (banked strike kept, 503 added)", st.strikes)
	}
}

// Cooldowns is read by the dashboard and must not reshuffle models that share
// an account between calls.
func TestCooldownsOrderIsStableWithinAnAccount(t *testing.T) {
	d := newDispatcher()
	now := time.Now()
	for _, m := range []string{"c", "a", "b", "e", "d"} {
		rt := textRoute("http://x", m)
		d.recordHop(rt, hopOutcome{failed: true, cooldown: time.Minute, reason: "t"}, now)
	}
	for i := 0; i < 10; i++ {
		var got []string
		for _, c := range d.Cooldowns() {
			got = append(got, c.Model)
		}
		if strings.Join(got, ",") != "a,b,c,d,e" {
			t.Fatalf("order = %v, want a,b,c,d,e", got)
		}
	}
}

// A 400 that says the hop cannot speak this wire at all (opencode-go's
// ModelProtocolUnsupported on /v1/messages) describes the HOP, not the body:
// the next hop must serve it. Live 2026-10-20: daily-pro's opencode-go hop
// returned this to Claude Code while its command-code hop sat unused.
func TestProtocolUnsupportedFailsOver(t *testing.T) {
	refused := upstub(t, http.StatusBadRequest,
		`{"type":"error","error":{"type":"ModelProtocolUnsupported","message":"Model does not support this protocol."}}`)
	defer refused.Close()
	good := upstub(t, http.StatusOK, `{"ok":true,"model":"night"}`)
	defer good.Close()

	d := newDispatcher()
	req, body := reqWithBody(`{"model":"day","messages":[]}`)
	rec := httptest.NewRecorder()

	routes := []Route{textRoute(refused.URL, "day"), textRoute(good.URL, "night")}
	_, res, err := d.ForwardCandidates(context.Background(), rec, req, routes, body)
	if err != nil {
		t.Fatalf("ForwardCandidates: %v", err)
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "night") {
		t.Fatalf("client saw %d %s, want hop 2's 200", rec.Code, rec.Body.String())
	}
	if len(res.Attempts) != 1 || !strings.Contains(res.Attempts[0].Note, "[protocol_unsupported]") {
		t.Errorf("attempt trail = %+v, want the refused hop with its cause", res.Attempts)
	}
	// The refusal will not change in five minutes: the hop is parked so the
	// next call does not pay for the same 400.
	if d.maxStrikes > 1 {
		t.Fatalf("test assumes park-on-first-strike, dispatcher has maxStrikes=%d", d.maxStrikes)
	}
	if allowed, _ := d.acquireHop(routes[0], time.Now()); allowed {
		t.Error("refused hop not parked")
	}
}

// When the refusing hop is the only one, the client still gets the upstream's
// real 400 — failover must not invent a different answer.
func TestProtocolUnsupportedSoleHopPassesThrough(t *testing.T) {
	refused := upstub(t, http.StatusBadRequest,
		`{"type":"error","error":{"type":"ModelProtocolUnsupported","message":"Model does not support this protocol."}}`)
	defer refused.Close()

	d := newDispatcher()
	req, body := reqWithBody(`{"model":"day","messages":[]}`)
	rec := httptest.NewRecorder()
	if _, _, err := d.ForwardCandidates(context.Background(), rec, req, []Route{textRoute(refused.URL, "day")}, body); err != nil {
		t.Fatalf("ForwardCandidates: %v", err)
	}
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "ModelProtocolUnsupported") {
		t.Errorf("client saw %d %s, want the upstream 400 verbatim", rec.Code, rec.Body.String())
	}
}
