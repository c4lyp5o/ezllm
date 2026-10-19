package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// responsesWireRoute is a hop curated onto the Responses wire for a CHAT
// client: Surface stays the client's surface, ResponsesWire flips the dial.
func responsesWireRoute(url, model string) Route {
	rt := testRoute(url, provider.SurfaceOpenAI)
	rt.Model = model
	rt.Alias = "daily"
	rt.ResponsesWire = true
	return rt
}

// A chat client through a responses-wire hop: the upstream receives the
// translated /responses body, the client a chat completion envelope.
func TestResponsesWireHopServesChatClient(t *testing.T) {
	var upstreamPath string
	var upstreamBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_9","created_at":1791760934,"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"sol says hi"}]}],"usage":{"input_tokens":11,"output_tokens":3}}`))
	}))
	defer upstream.Close()

	clientBody := `{"model":"daily","messages":[{"role":"system","content":"You are a coding agent."},{"role":"user","content":"hi"}],"max_tokens":64,"tools":[{"type":"function","function":{"name":"noop","description":"d","parameters":{"type":"object"}}}],"reasoning_effort":"high"}`
	d := newDispatcher()
	d.SetRetries(0)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(clientBody))
	rec := httptest.NewRecorder()
	served, res, err := d.ForwardCandidates(context.Background(), rec, req,
		[]Route{responsesWireRoute(upstream.URL, "openai/gpt-6.1-sol")}, []byte(clientBody))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != 200 {
		t.Fatalf("status = %d", res.Status)
	}
	if served.ResponsesWire != true || served.Surface != provider.SurfaceOpenAI {
		t.Errorf("served route = %+v", served)
	}

	if upstreamPath != "/responses" {
		t.Errorf("upstream path = %s, want /responses", upstreamPath)
	}
	wantUpstream := `{"model":"openai/gpt-6.1-sol","input":[{"role":"user","content":"hi"}],"instructions":"You are a coding agent.","tools":[{"type":"function","name":"noop","description":"d","parameters":{"type":"object"}}],"max_output_tokens":64,"reasoning":{"effort":"high"}}`
	if string(upstreamBody) != wantUpstream {
		t.Errorf("upstream body\nwant: %s\ngot:  %s", wantUpstream, upstreamBody)
	}

	wantClient := `{"id":"resp_9","object":"chat.completion","created":1791760934,"model":"daily","choices":[{"index":0,"message":{"role":"assistant","content":"sol says hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":3}}`
	if rec.Body.String() != wantClient {
		t.Errorf("client body\nwant: %s\ngot:  %s", wantClient, rec.Body.String())
	}
	// Usage tapped on the translated chat body, client surface semantics.
	if res.Usage == nil || res.Usage.In != 11 || res.Usage.Out != 3 {
		t.Errorf("usage tap = %+v, want in=11 out=3", res.Usage)
	}
}

// A responses-wire stream: upstream responses events in, chat chunks out.
func TestResponsesWireHopSSEStreams(t *testing.T) {
	upstreamStream := `event: response.created
data: {"type":"response.created","response":{"id":"resp_9","created_at":1791760934}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"Hi"}

event: response.completed
data: {"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":2}}}

`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, upstreamStream)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	want := `data: {"id":"resp_9","object":"chat.completion.chunk","created":1791760934,"model":"daily","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"resp_9","object":"chat.completion.chunk","created":1791760934,"model":"daily","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}

data: {"id":"resp_9","object":"chat.completion.chunk","created":1791760934,"model":"daily","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"resp_9","object":"chat.completion.chunk","created":1791760934,"model":"daily","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2}}

data: [DONE]

`
	clientBody := `{"model":"daily","stream":true,"messages":[{"role":"user","content":"hi"}],"max_tokens":64}`
	d := newDispatcher()
	d.SetRetries(0)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(clientBody))
	rec := httptest.NewRecorder()
	_, res, err := d.ForwardCandidates(context.Background(), rec, req,
		[]Route{responsesWireRoute(upstream.URL, "openai/gpt-6.1-sol")}, []byte(clientBody))
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != want {
		t.Errorf("client stream\nwant:\n%s\ngot:\n%s", want, got)
	}
	if !res.Stream {
		t.Error("result must record a streamed call")
	}
}

// Error bodies pass through byte-identical on the responses wire — chat and
// responses share the {"error":{…}} family, and the upstream's own words must
// reach the client and the ledger untouched.
func TestResponsesWireHopErrorPassthrough(t *testing.T) {
	errBody := `{"error":{"message":"Function tools with reasoning_effort are not supported","type":"invalid_request_error","code":400}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(errBody))
	}))
	defer upstream.Close()

	clientBody := `{"model":"daily","messages":[{"role":"user","content":"hi"}],"max_tokens":64}`
	d := newDispatcher()
	d.SetRetries(0)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(clientBody))
	rec := httptest.NewRecorder()
	_, res, err := d.ForwardCandidates(context.Background(), rec, req,
		[]Route{responsesWireRoute(upstream.URL, "openai/gpt-6.1-sol")}, []byte(clientBody))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != http.StatusBadRequest {
		t.Fatalf("status = %d", res.Status)
	}
	if got := rec.Body.String(); got != errBody {
		t.Errorf("error body must be byte-identical\nwant: %s\ngot:  %s", errBody, got)
	}
	if !strings.Contains(res.Err, "Function tools") {
		t.Errorf("ledger err column = %q, want the upstream's words", res.Err)
	}
}

// A failing responses-wire hop still fails over to a native hop: the wire
// override never widens the failure set.
func TestResponsesWireHopFailureStillFailsOver(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"too_many_requests"}}`))
	}))
	defer failing.Close()

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"from native hop"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`))
	}))
	defer healthy.Close()

	clientBody := `{"model":"daily","messages":[{"role":"user","content":"hi"}],"max_tokens":64}`
	d := newDispatcher()
	d.SetRetries(0)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(clientBody))
	rec := httptest.NewRecorder()
	served, res, err := d.ForwardCandidates(context.Background(), rec, req, []Route{
		responsesWireRoute(failing.URL, "openai/gpt-6.1-sol"),
		testRoute(healthy.URL, provider.SurfaceOpenAI),
	}, []byte(clientBody))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != 200 || !strings.Contains(rec.Body.String(), "from native hop") {
		t.Fatalf("status = %d body = %s", res.Status, rec.Body.String())
	}
	if served.ResponsesWire {
		t.Error("the served route must be the native hop")
	}
}

// An ANTHROPIC client on a responses-wire hop gets upstream errors in the
// Messages envelope — the same shape the chat wire gives it — not the
// responses family it cannot parse. The ledger still keeps the upstream's words.
func TestResponsesWireHopErrorReshapedForAnthropicClient(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down","type":"rate_limit_error","code":429}}`))
	}))
	defer upstream.Close()

	clientBody := `{"model":"daily","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	rt := testRoute(upstream.URL, provider.SurfaceAnthropic)
	rt.Model, rt.Alias, rt.ResponsesWire = "openai/gpt-6.1-sol", "daily", true
	d := newDispatcher()
	d.SetRetries(0)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(clientBody))
	rec := httptest.NewRecorder()
	_, res, err := d.ForwardCandidates(context.Background(), rec, req, []Route{rt}, []byte(clientBody))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`
	if rec.Code != http.StatusTooManyRequests || rec.Body.String() != want {
		t.Errorf("client got %d %s\nwant 429 %s", rec.Code, rec.Body.String(), want)
	}
	if !strings.Contains(res.Err, "slow down") {
		t.Errorf("ledger err = %q, want the upstream's words", res.Err)
	}
}
