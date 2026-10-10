package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ── 429 is a failover signal, never a retry signal ───────────────────────────
//
// The live incident this pins: a Hermes tool loop sent 6 calls in one minute at
// an account whose tier allows 5. The hop answered 429, and the dispatcher then
// retried it 3 more times on the SAME hop (retry(retries)=3), spending the very
// budget the 429 was reporting. One over-budget call became four, the minute's
// budget vanished in seconds, and the client's turn died.
//
// These tests pin the three halves of the fix: no same-hop retry, failover to a
// hop with budget, and no synthetic refusal when there is no alternate hop.

// countingStub answers a fixed status and counts hits.
func countingStub(t *testing.T, status int, body string, hits *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

// A 429 must be attempted exactly ONCE per hop. Before the fix the retry budget
// multiplied it by retries+1.
func TestRateLimitIsNotRetriedOnTheSameHop(t *testing.T) {
	var hits int32
	up := countingStub(t, http.StatusTooManyRequests, `{"error":"Maximum 5 requests within 1 minutes"}`, &hits)
	defer up.Close()

	d := newDispatcher()
	d.SetRetries(3) // the production default that made one 429 into four requests

	req, body := reqWithBody(`{"model":"m","messages":[]}`)
	rec := httptest.NewRecorder()
	_, res, err := d.ForwardCandidates(context.Background(), rec, req, []Route{textRoute(up.URL, "m")}, body)
	if err != nil {
		t.Fatalf("ForwardCandidates: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("upstream hits = %d, want exactly 1 — a rate limit must not be retried on the same hop", got)
	}
	if res.Status != http.StatusTooManyRequests || rec.Code != http.StatusTooManyRequests {
		t.Fatalf("ledger=%d client=%d, want 429 both", res.Status, rec.Code)
	}
}

// A hop that is out of budget must still be failover-eligible: the client sees
// the answer from the hop that could serve it, not the 429.
func TestRateLimitedHopFailsOverAndIsThenSkipped(t *testing.T) {
	var badHits, goodHits int32
	bad := countingStub(t, http.StatusTooManyRequests, `{"error":"quota"}`, &badHits)
	defer bad.Close()
	good := countingStub(t, http.StatusOK, `{"ok":true}`, &goodHits)
	defer good.Close()

	d := newDispatcher()
	d.SetRetries(3)
	// A long park keeps the assertions deterministic: the second call must skip
	// the parked hop, which it can only do while the window is still open.
	d.ConfigureCooldown(5*time.Second, 5*time.Second)

	routes := []Route{textRoute(bad.URL, "day"), textRoute(good.URL, "night")}
	for i := 0; i < 2; i++ {
		req, body := reqWithBody(`{"model":"day","messages":[]}`)
		rec := httptest.NewRecorder()
		if _, _, err := d.ForwardCandidates(context.Background(), rec, req, routes, body); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: client status = %d, want 200 (the 429 must be invisible)", i, rec.Code)
		}
	}
	if got := atomic.LoadInt32(&badHits); got != 1 {
		t.Errorf("rate-limited hop hits = %d, want 1 — once parked it must be skipped, not re-probed", got)
	}
	if got := atomic.LoadInt32(&goodHits); got != 2 {
		t.Errorf("good hop hits = %d, want 2", got)
	}
}

// The availability guard: with no alternate hop, a 429 must NOT park the only
// hop. A synthetic cooldown refusal would turn the provider's 60-second window
// into a multi-minute outage of our own making.
func TestSoleHopRateLimitIsNotParked(t *testing.T) {
	var hits int32
	up := countingStub(t, http.StatusTooManyRequests, `{"error":"quota"}`, &hits)
	defer up.Close()

	d := newDispatcher()
	d.SetRetries(3)
	// Deliberately generous: if the sole hop were parked, the second call would
	// be refused locally and never reach the upstream.
	d.ConfigureCooldown(time.Hour, time.Hour)

	rt := textRoute(up.URL, "m")
	for i := 0; i < 2; i++ {
		req, body := reqWithBody(`{"model":"m","messages":[]}`)
		rec := httptest.NewRecorder()
		_, _, err := d.ForwardCandidates(context.Background(), rec, req, []Route{rt}, body)
		if err != nil {
			t.Fatalf("request %d: %v (a sole hop must never be locally refused)", i, err)
		}
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("request %d: client status = %d, want the upstream's real 429", i, rec.Code)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("upstream hits = %d, want 2 — the sole hop must be retried for real next call", got)
	}
}

// ── hopCooldown: evidence beats guessing ────────────────────────────────────

func TestHopCooldownHonoursRetryAfter(t *testing.T) {
	d := newDispatcher()
	hdr := http.Header{}

	hdr.Set("Retry-After", "7")
	if got := d.hopCooldown(nil, http.StatusTooManyRequests, hdr, true); got != 7*time.Second {
		t.Errorf("Retry-After: 7 → %v, want 7s", got)
	}

	// A value the upstream cannot seriously mean is rejected rather than
	// clamped, so the operator's configured duration applies instead.
	hdr.Set("Retry-After", "86400")
	if got := d.hopCooldown(nil, http.StatusTooManyRequests, hdr, true); got != d.on429 {
		t.Errorf("absurd Retry-After → %v, want the configured on_429 (%v)", got, d.on429)
	}

	// Garbage must not pin a hop.
	hdr.Set("Retry-After", "soon")
	if got := d.hopCooldown(nil, http.StatusTooManyRequests, hdr, true); got != d.on429 {
		t.Errorf("garbage Retry-After → %v, want the configured on_429 (%v)", got, d.on429)
	}

	// Zero is not a valid delay: "come back now" is what we are already doing.
	hdr.Set("Retry-After", "0")
	if got := d.hopCooldown(nil, http.StatusTooManyRequests, hdr, true); got != d.on429 {
		t.Errorf("Retry-After: 0 → %v, want the configured on_429 (%v)", got, d.on429)
	}
}

func TestConfigureCooldownWiresOperatorValues(t *testing.T) {
	d := newDispatcher()
	d.ConfigureCooldown(42*time.Second, 5*time.Second)
	if d.on429 != 42*time.Second {
		t.Errorf("on429 = %v, want 42s", d.on429)
	}
	if d.on5xx != 5*time.Second {
		t.Errorf("on5xx = %v, want 5s", d.on5xx)
	}
	// A zero value must not silently disable cooling — the default stands.
	d.ConfigureCooldown(0, 0)
	if d.on429 != 42*time.Second || d.on5xx != 5*time.Second {
		t.Errorf("zero config clobbered the durations: on429=%v on5xx=%v", d.on429, d.on5xx)
	}
}

// ── ledger: an error must explain itself ────────────────────────────────────

// The blank-429 problem: the incident row read status=429, err="" because the
// upstream's own sentence was streamed to the client and then discarded. Without
// it an incident is undiagnosable after the fact.
func TestUpstreamErrorBodyIsRecordedInLedger(t *testing.T) {
	const upstreamMsg = `{"error":{"message":"Maximum 5 requests within 1 minutes","type":"rate_limit"}}`
	up := upstub(t, http.StatusTooManyRequests, upstreamMsg)
	defer up.Close()

	d := newDispatcher()
	req, body := reqWithBody(`{"model":"m","messages":[]}`)
	rec := httptest.NewRecorder()
	_, res, err := d.ForwardCandidates(context.Background(), rec, req, []Route{textRoute(up.URL, "m")}, body)
	if err != nil {
		t.Fatalf("ForwardCandidates: %v", err)
	}
	if res.Err == "" {
		t.Fatal("ledger err is empty — the upstream's reason was discarded")
	}
	if !strings.Contains(res.Err, "Maximum 5 requests within 1 minutes") {
		t.Errorf("ledger err = %q, want the upstream message in it", res.Err)
	}
	if !strings.Contains(res.Err, "429") {
		t.Errorf("ledger err = %q, want the status prefixed", res.Err)
	}
	// The client must still receive the body byte-for-byte.
	if rec.Body.String() != upstreamMsg {
		t.Errorf("client body = %q, want it unchanged", rec.Body.String())
	}
}

func TestUpstreamErrorSnippetIsBoundedAndSingleLine(t *testing.T) {
	// Multi-line provider bodies must not wreck the dashboard's inline render.
	multi := "line one\n\tline two\r\nline three"
	got := upstreamErrorSnippet(500, []byte(multi))
	if strings.ContainsAny(got, "\n\r\t") {
		t.Errorf("snippet %q contains whitespace control characters", got)
	}

	// A chatty body is truncated, rune-safely, and marked as truncated.
	long := strings.Repeat("é", 4000) // 2 bytes per rune, so byte-slicing would split one
	got = upstreamErrorSnippet(400, []byte(long))
	if len(got) > maxErrSnippet+64 {
		t.Errorf("snippet length = %d, want it bounded near %d", len(got), maxErrSnippet)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("snippet %q should be marked as truncated", got)
	}
	if !strings.Contains(got, "é") {
		t.Errorf("snippet lost its content entirely: %q", got)
	}

	// An empty body still yields a self-describing row.
	if got := upstreamErrorSnippet(503, nil); !strings.Contains(got, "503") {
		t.Errorf("empty-body snippet %q should carry the status", got)
	}
}
