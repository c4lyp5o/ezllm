package proxy

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/jsonbody"
	"github.com/c4lyp5o/ezllm/internal/logx"
	"github.com/c4lyp5o/ezllm/internal/provider"
)

// syncBuf is a concurrency-safe sink: the dispatcher logs from its own path.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// A clamp REWRITES the client's request. It has to be visible: a client that
// asked for "none" and got "minimal" otherwise has no way to find out why.
func TestClampIsLoggedWithBothValues(t *testing.T) {
	sink := &syncBuf{}
	tr := &captureTransport{}
	d := NewDispatcher(captureHTTPClient(tr), provider.NewRegistry(captureHTTPClient(tr)))
	d.SetLogger(slog.New(logx.NewHandler(sink, logx.Options{Level: slog.LevelInfo})))

	req, body := reqWithBody(`{"model":"alias","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`)
	rt := testRoute("http://up.invalid", provider.SurfaceOpenAI)
	rt.Reasoning = sparkRule()
	if _, _, err := d.ForwardCandidatesDoc(context.Background(), httptest.NewRecorder(), req, []Route{rt}, body, jsonbody.Open(body)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	out := sink.String()
	if !strings.Contains(out, "reasoning effort clamped for this hop") {
		t.Fatalf("the clamp was silent:\n%s", out)
	}
	for _, want := range []string{"asked=none", "sent=minimal", "model=resolved-model"} {
		if !strings.Contains(out, want) {
			t.Fatalf("clamp line is missing %q:\n%s", want, out)
		}
	}
}

// The common path must stay silent: a request whose effort the model already
// accepts writes no line at all.
func TestAllowedEffortLogsNothing(t *testing.T) {
	sink := &syncBuf{}
	tr := &captureTransport{}
	d := NewDispatcher(captureHTTPClient(tr), provider.NewRegistry(captureHTTPClient(tr)))
	d.SetLogger(slog.New(logx.NewHandler(sink, logx.Options{Level: slog.LevelDebug})))

	req, body := reqWithBody(`{"model":"alias","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`)
	rt := testRoute("http://up.invalid", provider.SurfaceOpenAI)
	rt.Reasoning = sparkRule()
	if _, _, err := d.ForwardCandidatesDoc(context.Background(), httptest.NewRecorder(), req, []Route{rt}, body, jsonbody.Open(body)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if out := sink.String(); strings.Contains(out, "reasoning effort") {
		t.Fatalf("an accepted effort logged a clamp:\n%s", out)
	}
}

// A mandatory-reasoning model gets the field DROPPED, which is a different
// statement from a rewrite and must not be reported as one.
func TestDroppedEffortIsLoggedAsDropped(t *testing.T) {
	sink := &syncBuf{}
	tr := &captureTransport{}
	d := NewDispatcher(captureHTTPClient(tr), provider.NewRegistry(captureHTTPClient(tr)))
	d.SetLogger(slog.New(logx.NewHandler(sink, logx.Options{Level: slog.LevelInfo})))

	req, body := reqWithBody(`{"model":"alias","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`)
	rt := testRoute("http://up.invalid", provider.SurfaceOpenAI)
	rt.Reasoning = provider.ReasoningRule{Required: true, Known: true}
	if _, _, err := d.ForwardCandidatesDoc(context.Background(), httptest.NewRecorder(), req, []Route{rt}, body, jsonbody.Open(body)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	out := sink.String()
	if !strings.Contains(out, "reasoning effort dropped for this hop") || !strings.Contains(out, "asked=none") {
		t.Fatalf("drop was not reported as a drop:\n%s", out)
	}
	if strings.Contains(out, "clamped") {
		t.Fatalf("a drop was reported as a clamp:\n%s", out)
	}
}

// The changed path keeps working: the hop body still carries the clamped value
// and the model swap — moving the clamp behind a wrapper must not break either.
func TestClampedBodyStillReachesTheWire(t *testing.T) {
	tr := &captureTransport{}
	d := NewDispatcher(captureHTTPClient(tr), provider.NewRegistry(captureHTTPClient(tr)))
	d.SetLogger(slog.New(logx.NewHandler(&syncBuf{}, logx.Options{Level: slog.LevelError})))

	req, body := reqWithBody(`{"model":"alias","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`)
	rt := testRoute("http://up.invalid", provider.SurfaceOpenAI)
	rt.Reasoning = sparkRule()
	if _, _, err := d.ForwardCandidatesDoc(context.Background(), httptest.NewRecorder(), req, []Route{rt}, body, jsonbody.Open(body)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	sent := tr.last()
	if !strings.Contains(sent, `"reasoning_effort":"minimal"`) || !strings.Contains(sent, rt.Model) {
		t.Fatalf("wire body = %s, want the clamped effort and the hop model", sent)
	}
}

// captureHTTPClient exists because the tests above build a dispatcher from a
// transport, and *http.Client needs a real timeout to avoid hanging a stream.
func captureHTTPClient(tr *captureTransport) *http.Client {
	return &http.Client{Transport: tr, Timeout: 5 * time.Second}
}
