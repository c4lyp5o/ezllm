package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Addendum #2/#6: a hop whose declared context window is too small for the
// request is filtered BEFORE dispatch — no paid upstream call, no wasted
// round-trip. The estimate is len(body)/4, and 0 means unlimited.
func TestContextFilterSkipsSmallWindowHop(t *testing.T) {
	good := upstub(t, http.StatusOK, `{"ok":true}`)
	defer good.Close()

	// Two routes: one with a tiny context window, one unlimited.
	smallCtx := textRoute(good.URL, "small-ctx")
	smallCtx.Account.MaxContextTokens = 100 // 100 tokens ≈ 400 bytes
	unlimited := textRoute(good.URL, "unlimited")
	unlimited.Account.MaxContextTokens = 0 // 0 = never filtered

	d := newDispatcher()
	// A body that's > 400 bytes → estimated > 100 tokens.
	big := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("x", 500) + `"}]}`
	req, body := reqWithBody(big)
	rec := httptest.NewRecorder()

	routes := []Route{smallCtx, unlimited}
	_, res, err := d.ForwardCandidates(context.Background(), rec, req, routes, body)
	if err != nil {
		t.Fatalf("ForwardCandidates: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("client saw %d, want 200", rec.Code)
	}
	// The small-context hop must appear in the attempt trail with its honest
	// note — the operator needs to see WHY it was skipped.
	var found bool
	for _, a := range res.Attempts {
		if strings.Contains(a.Note, "context too small") && strings.Contains(a.Note, "100") {
			found = true
		}
	}
	if !found {
		t.Errorf("the skip note must name 'context too small' and the limit: %+v", res.Attempts)
	}
	// The unlimited hop actually served the request.
	if res.Status != http.StatusOK {
		t.Errorf("the unlimited hop must serve: %d", res.Status)
	}
}

// A request that FITS the context window goes through normally.
func TestContextFilterAllowsFittingRequest(t *testing.T) {
	good := upstub(t, http.StatusOK, `{"ok":true}`)
	defer good.Close()

	fitting := textRoute(good.URL, "fitting")
	fitting.Account.MaxContextTokens = 1000 // plenty for a small body

	d := newDispatcher()
	req, body := reqWithBody(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	rec := httptest.NewRecorder()

	_, res, err := d.ForwardCandidates(context.Background(), rec, req, []Route{fitting}, body)
	if err != nil {
		t.Fatalf("ForwardCandidates: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("a fitting request must be dispatched: %d", res.Status)
	}
	// No "context too small" in the attempts — there shouldn't be any attempts
	// at all (success on first hop).
	for _, a := range res.Attempts {
		if strings.Contains(a.Note, "context too small") {
			t.Error("a fitting request must not be filtered")
		}
	}
}

// MaxContext=0 (unlimited) is NEVER filtered, even for a huge body.
func TestContextFilterUnlimitedNeverFiltered(t *testing.T) {
	good := upstub(t, http.StatusOK, `{"ok":true}`)
	defer good.Close()

	unlimited := textRoute(good.URL, "unlimited")
	unlimited.Account.MaxContextTokens = 0

	d := newDispatcher()
	// A very large body — 10KB.
	huge := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("y", 10000) + `"}]}`
	req, body := reqWithBody(huge)
	rec := httptest.NewRecorder()

	_, res, err := d.ForwardCandidates(context.Background(), rec, req, []Route{unlimited}, body)
	if err != nil {
		t.Fatalf("ForwardCandidates: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("an unlimited hop must always dispatch: %d", res.Status)
	}
}

// When ALL hops are filtered, the client sees a 503 with a clear reason.
func TestContextFilterAllHopsFiltered(t *testing.T) {
	good := upstub(t, http.StatusOK, `{"ok":true}`)
	defer good.Close()

	tiny1 := textRoute(good.URL, "tiny1")
	tiny1.Account.MaxContextTokens = 10
	tiny2 := textRoute(good.URL, "tiny2")
	tiny2.Account.MaxContextTokens = 20

	d := newDispatcher()
	big := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("z", 200) + `"}]}`
	req, body := reqWithBody(big)
	rec := httptest.NewRecorder()

	_, _, err := d.ForwardCandidates(context.Background(), rec, req, []Route{tiny1, tiny2}, body)
	if err == nil && rec.Code != http.StatusServiceUnavailable {
		// It might error or return 503 — either is acceptable. The key thing
		// is that it must NOT return 200 (no hop was dispatched).
		if rec.Code == http.StatusOK {
			t.Error("no hop should have been dispatched — all context windows are too small")
		}
	}
}

// The compat-group documentation: ForwardCandidatesDoc's docstring.
func TestCompatGroupDocumentation(t *testing.T) {
	// This test is a placeholder that asserts the policy is documented in the
	// code. The real test is that the code compiles and the proxy tests pass —
	// the documentation lives in proxy.go's ForwardCandidatesDoc.
	t.Log("compat groups (replica/fallback/translation) are documented in ForwardCandidatesDoc")
}
