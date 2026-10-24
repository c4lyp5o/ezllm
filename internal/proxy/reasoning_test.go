package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/jsonbody"
	"github.com/c4lyp5o/ezllm/internal/provider"
)

// The rule every test here uses is the one opencode-go named in the ledger:
// 'none' is not accepted, and the cheapest level it does accept is `minimal`.
func sparkRule() provider.ReasoningRule {
	return provider.ReasoningRule{
		Allowed: []string{"minimal", "low", "medium", "high", "xhigh", "max"},
		Known:   true,
	}
}

func TestClampReasoningRewritesOnlyTheEffort(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none","temperature":0.2}`)

	got, change := clampReasoning(body, sparkRule())
	if change == nil {
		t.Fatalf("'none' is not allowed, the body must be rewritten")
	}
	var doc map[string]any
	if err := json.Unmarshal(got, &doc); err != nil {
		t.Fatalf("clamped body is not JSON: %v", err)
	}
	if doc["reasoning_effort"] != "minimal" {
		t.Fatalf("reasoning_effort = %v, want minimal", doc["reasoning_effort"])
	}
	// Everything else survives: this is a rewrite of one field, not a reshape.
	if doc["model"] != "m" || doc["temperature"] != 0.2 {
		t.Fatalf("clamp disturbed other fields: %v", doc)
	}
	messages, ok := doc["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("messages lost: %v", doc["messages"])
	}
}

func TestClampReasoningIsInertWithoutARule(t *testing.T) {
	body := []byte(`{"model":"m","reasoning_effort":"none"}`)
	for name, rule := range map[string]provider.ReasoningRule{
		"zero":    {},
		"no-rule": {Allowed: []string{"low", "high"}}, // not Known
	} {
		got, change := clampReasoning(body, rule)
		if change != nil {
			t.Fatalf("%s: rule with no knowledge rewrote the body", name)
		}
		if &got[0] != &body[0] {
			t.Fatalf("%s: unchanged body must be the original slice", name)
		}
	}
}

func TestClampReasoningDropsTheFieldWhenReasoningIsMandatory(t *testing.T) {
	body := []byte(`{"model":"m","reasoning_effort":"none"}`)
	got, change := clampReasoning(body, provider.ReasoningRule{Required: true, Known: true})
	if change == nil {
		t.Fatalf("a mandatory-reasoning model must not receive 'none'")
	}
	var doc map[string]any
	if err := json.Unmarshal(got, &doc); err != nil {
		t.Fatal(err)
	}
	if _, present := doc["reasoning_effort"]; present {
		t.Fatalf("the field must be dropped, not rewritten: %v", doc)
	}
}

func TestClampReasoningLeavesAnAbsentFieldAbsent(t *testing.T) {
	body := []byte(`{"model":"m","messages":[]}`)
	if _, change := clampReasoning(body, sparkRule()); change != nil {
		t.Fatalf("a body without reasoning_effort must not be rewritten")
	}
}

// Once a model has a rule, every request to it reaches the clamp. One without
// the field must cost a byte scan, not a decode of the whole body.
func TestClampReasoningDoesNotDecodeABodyWithoutTheField(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("x", 64<<10) + `"}]}`)
	rule := sparkRule()
	allocs := testing.AllocsPerRun(20, func() {
		if _, change := clampReasoning(body, rule); change != nil {
			t.Fatal("a body without reasoning_effort must not be rewritten")
		}
	})
	if allocs != 0 {
		t.Fatalf("clampReasoning allocated %.0f times on a body without the field; want 0", allocs)
	}
}

// captureTransport records what actually went upstream.
type captureTransport struct {
	mu     sync.Mutex
	bodies []string
}

func (c *captureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	var body string
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}
	c.bodies = append(c.bodies, body)
	c.mu.Unlock()
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode: http.StatusOK, Header: h, Request: r,
		Body: io.NopCloser(strings.NewReader(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)),
	}, nil
}

func (c *captureTransport) last() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		return ""
	}
	return c.bodies[len(c.bodies)-1]
}

// The clamp has to survive the whole hop path — the shared doc, the model swap
// and the wire — not just the helper. This is the test that would have caught
// `doc.WithModel` emitting the ORIGINAL bytes past a clamped body.
func TestHopBodySendsTheClampedEffortUpstream(t *testing.T) {
	tr := &captureTransport{}
	d := NewDispatcher(
		&http.Client{Transport: tr, Timeout: 5 * time.Second},
		provider.NewRegistry(&http.Client{Timeout: 5 * time.Second}))

	// A real decoded doc, the way the single-parse dispatch hands one down.
	original := []byte(`{"model":"alias","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none","stream":false}`)
	req, body := reqWithBody(string(original))
	rt := testRoute("http://up.invalid", "real-model")
	rt.Reasoning = sparkRule()

	// The doc is the SHARED parse: if the clamp does not invalidate it, the
	// native path splices the model into the original bytes and the clamped
	// effort never reaches the wire.
	bodyDoc := jsonbody.Open(body)
	if bodyDoc.Err() != nil {
		t.Fatalf("open doc: %v", bodyDoc.Err())
	}
	if _, _, err := d.ForwardCandidatesDoc(context.Background(), httptest.NewRecorder(), req, []Route{rt}, body, bodyDoc); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	sent := tr.last()
	if sent == "" {
		t.Fatalf("nothing reached the upstream")
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(sent), &doc); err != nil {
		t.Fatalf("upstream body is not JSON: %v", err)
	}
	if doc["reasoning_effort"] != "minimal" {
		t.Fatalf("upstream received reasoning_effort=%v, want the clamped minimal (body: %s)", doc["reasoning_effort"], sent)
	}
	if doc["model"] != rt.Model {
		t.Fatalf("model = %v, want the hop's model %q — the clamp must not break the swap", doc["model"], rt.Model)
	}
	if doc["stream"] != false {
		t.Fatalf("stream flag lost through the clamp: %v", doc)
	}

	// The client's own bytes are never mutated in place.
	if string(body) != string(original) {
		t.Fatalf("the shared client body was mutated:\n got %s\nwant %s", body, original)
	}
}

func TestClampReasoningLeadingWhitespace(t *testing.T) {
	for _, prefix := range []string{" ", "\n", "\r\n\t "} {
		t.Run(fmt.Sprintf("%q", prefix), func(t *testing.T) {
			body := []byte(prefix + `{"model":"alias","reasoning_effort":"none"}`)
			rt := testRoute("http://up.invalid", "real-model")
			rt.Reasoning = sparkRule()
			doc := jsonbody.Open(body)
			got, _, err := prepareHopBody(body, rt, doc)
			if err != nil {
				t.Fatal(err)
			}
			var parsed map[string]any
			if err := json.Unmarshal(got, &parsed); err != nil {
				t.Fatal(err)
			}
			if parsed["reasoning_effort"] != "minimal" || parsed["model"] != rt.Model {
				t.Fatalf("hop body = %s, want clamped effort and upstream model", got)
			}
			unchanged := []byte(prefix + `{"reasoning_effort":"low"}`)
			out, change := clampReasoning(unchanged, sparkRule())
			if change != nil || &out[0] != &unchanged[0] {
				t.Fatal("allowed effort must preserve the original slice")
			}
		})
	}
}
