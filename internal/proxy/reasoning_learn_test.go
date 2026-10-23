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

// reasoningRefusalTransport answers every call with opencode-go's own words,
// verbatim from the ledger.
type reasoningRefusalTransport struct {
	mu     sync.Mutex
	bodies []string
}

const sparkRefusalBody = `{"model":"muse-spark-1.3-contributor","error":{"code":null,"message":"reasoning_effort 'none' is not supported for model 'muse-spark-1.3-contributor'. Supported values: [minimal, low, medium, high, xhigh, max]","param":"reasoning.effort","type":"invalid_request_error"}}`

func (t *reasoningRefusalTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.mu.Lock()
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		t.bodies = append(t.bodies, string(b))
	}
	t.mu.Unlock()
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode: http.StatusBadRequest, Header: h, Request: r,
		Body: io.NopCloser(strings.NewReader(sparkRefusalBody)),
	}, nil
}

// The learning half: a refusal that names its vocabulary must travel back to
// the caller (which has the store) WITHOUT the peek eating the body the client
// is about to receive.
func TestRefusedRequestReportsTheLearnedVocabulary(t *testing.T) {
	tr := &reasoningRefusalTransport{}
	d := NewDispatcher(
		&http.Client{Transport: tr, Timeout: 5 * time.Second},
		provider.NewRegistry(&http.Client{Timeout: 5 * time.Second}))
	d.SetRetries(0)

	req, body := reqWithBody(`{"model":"alias","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`)
	rt := testRoute("http://up.invalid", "real-model")

	rec := httptest.NewRecorder()
	_, res, _ := d.ForwardCandidatesDoc(context.Background(), rec, req, []Route{rt}, body, nil)

	if res.LearnedReasoning == nil {
		t.Fatalf("the refusal names a vocabulary; nothing was learned (result=%+v)", res)
	}
	lr := res.LearnedReasoning
	if lr.AccountID != rt.Account.ID || lr.Model != rt.Model {
		t.Fatalf("learned fact tagged %d/%s, want %d/%s", lr.AccountID, lr.Model, rt.Account.ID, rt.Model)
	}
	if len(lr.Rule.Allowed) != 6 || lr.Rule.Allowed[0] != "minimal" {
		t.Fatalf("rule = %+v, want the six values the provider named", lr.Rule)
	}
	// The client must still receive the provider's own refusal, byte-intact:
	// the peek that taught us this must not have consumed it.
	if !strings.Contains(rec.Body.String(), "Supported values: [minimal, low") {
		t.Fatalf("client body lost the upstream refusal: %q", rec.Body.String())
	}
}

// A refusal that says nothing about a vocabulary must not invent one.
func TestUnrelatedRefusalLearnsNothing(t *testing.T) {
	tr := &staticTransport{status: http.StatusBadRequest, body: `{"error":{"message":"context length exceeded"}}`}
	d := NewDispatcher(
		&http.Client{Transport: tr, Timeout: 5 * time.Second},
		provider.NewRegistry(&http.Client{Timeout: 5 * time.Second}))
	d.SetRetries(0)

	req, body := reqWithBody(`{"model":"alias","messages":[]}`)
	rt := testRoute("http://up.invalid", "real-model")
	_, res, _ := d.ForwardCandidatesDoc(context.Background(), httptest.NewRecorder(), req, []Route{rt}, body, nil)
	if res.LearnedReasoning != nil {
		t.Fatalf("learned %+v from an unrelated refusal", res.LearnedReasoning)
	}
}

// hostTransport answers per upstream host, so each hop of a combo can fail its
// own way.
type hostTransport map[string]staticTransport

func (t hostTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	st := t[r.URL.Host]
	return st.RoundTrip(r)
}

// A hop whose refusal names a vocabulary and then fails over (404 here) must
// still report what it taught when EVERY hop fails and the last error is the
// one committed — commit builds a fresh Result on that path too.
func TestFallbackCommitKeepsTheLearnedVocabulary(t *testing.T) {
	tr := hostTransport{
		"a.invalid": {status: http.StatusNotFound, body: sparkRefusalBody},
		"b.invalid": {status: http.StatusBadGateway, body: `{"error":{"message":"bad gateway"}}`},
	}
	d := NewDispatcher(
		&http.Client{Transport: tr, Timeout: 5 * time.Second},
		provider.NewRegistry(&http.Client{Timeout: 5 * time.Second}))
	d.SetRetries(0)

	req, body := reqWithBody(`{"model":"alias","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`)
	a := testRoute("http://a.invalid", "real-model")
	b := testRoute("http://b.invalid", "real-model")
	b.Account.ID, b.Account.Namespace = 2, "b"

	rec := httptest.NewRecorder()
	served, res, _ := d.ForwardCandidatesDoc(context.Background(), rec, req, []Route{a, b}, body, nil)
	if served.Account.ID != b.Account.ID || rec.Code != http.StatusBadGateway {
		t.Fatalf("want the last hop's 502 committed, got account %d status %d", served.Account.ID, rec.Code)
	}
	if res.LearnedReasoning == nil || res.LearnedReasoning.AccountID != a.Account.ID {
		t.Fatalf("the first hop's vocabulary was lost on the fallback commit: %+v", res.LearnedReasoning)
	}
}

// staticTransport answers one canned response.
type staticTransport struct {
	status int
	body   string
}

func (t *staticTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode: t.status, Header: h, Request: r,
		Body: io.NopCloser(strings.NewReader(t.body)),
	}, nil
}
