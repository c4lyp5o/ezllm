package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// translatedRoute is a hop that speaks only the OpenAI wire shape while the
// CLIENT speaks anthropic: Surface stays the client's surface, Translated
// marks the wire translation.
func translatedRoute(url, model string) Route {
	rt := testRoute(url, provider.SurfaceAnthropic)
	rt.Model = model
	rt.Alias = "daily"
	rt.Translated = true
	return rt
}

// Invariant 1 — passthrough identity. With translation off, the bytes that
// leave ezllm are exactly what today's model-swap path produces: a
// regression here means the translation feature leaked into the native path.
func TestPassthroughUnchangedWhenTranslationOff(t *testing.T) {
	var got []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer upstream.Close()

	clientBody := `{"model":"whatever","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"top_k":40}`
	want, _, err := SwapModel([]byte(clientBody), "resolved-model")
	if err != nil {
		t.Fatal(err)
	}

	d := newDispatcher()
	d.SetRetries(0)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(clientBody))
	rec := httptest.NewRecorder()
	if _, _, err := d.ForwardCandidates(context.Background(), rec, req,
		[]Route{testRoute(upstream.URL, provider.SurfaceAnthropic)}, []byte(clientBody)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("native path bytes changed\nwant: %s\ngot:  %s", want, got)
	}
}

// A translated hop serves an anthropic client: the upstream must receive the
// translated openai body, and the client an anthropic message envelope.
func TestTranslatedHopServesAnthropicClient(t *testing.T) {
	var upstreamBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("translated hop must dial the openai path, got %s", r.URL.Path)
		}
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"hello!"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	clientBody := `{"model":"daily","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	d := newDispatcher()
	d.SetRetries(0)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(clientBody))
	rec := httptest.NewRecorder()
	served, res, err := d.ForwardCandidates(context.Background(), rec, req,
		[]Route{translatedRoute(upstream.URL, "step-5-preview-free")}, []byte(clientBody))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != 200 {
		t.Fatalf("status = %d", res.Status)
	}
	if !served.Translated {
		t.Error("served route lost its translated marker")
	}

	wantUpstream := `{"model":"step-5-preview-free","messages":[{"role":"user","content":"hi"}],"max_tokens":64}`
	if string(upstreamBody) != wantUpstream {
		t.Errorf("upstream body\nwant: %s\ngot:  %s", wantUpstream, upstreamBody)
	}

	wantClient := `{"id":"msg_c1","type":"message","role":"assistant","content":[{"type":"text","text":"hello!"}],"model":"daily","stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":5}}`
	if rec.Body.String() != wantClient {
		t.Errorf("client body\nwant: %s\ngot:  %s", wantClient, rec.Body.String())
	}

	// The usage tap must read the CLIENT surface's convention, which is what
	// the translated envelope carries.
	if res.Usage == nil || res.Usage.In != 10 || res.Usage.Out != 5 {
		t.Errorf("usage tap = %+v, want in=10 out=5", res.Usage)
	}
	// The ledger surface is the CLIENT's, not the hop's wire shape: the
	// server derives it from the served route, which keeps Surface=anthropic.
	if served.Surface != provider.SurfaceAnthropic {
		t.Errorf("served surface = %v, want the client's anthropic surface", served.Surface)
	}
}

// A translated stream: upstream openai chunks in, byte-exact anthropic SSE out.
func TestTranslatedHopSSEStreams(t *testing.T) {
	upstreamStream := `data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"c1","usage":{"prompt_tokens":3,"completion_tokens":4}}

data: [DONE]

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

	want := `event: ping
data: {"type":"ping"}

event: message_start
data: {"type":"message_start","message":{"type":"message","id":"msg_c1","role":"assistant","content":[],"model":"daily","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":3,"output_tokens":4}}

event: message_stop
data: {"type":"message_stop"}

`
	clientBody := `{"model":"daily","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	d := newDispatcher()
	d.SetRetries(0)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(clientBody))
	rec := httptest.NewRecorder()
	_, res, err := d.ForwardCandidates(context.Background(), rec, req,
		[]Route{translatedRoute(upstream.URL, "step-5-preview-free")}, []byte(clientBody))
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != want {
		t.Errorf("client stream\nwant:\n%s\ngot:\n%s", want, got)
	}
	if res.Usage == nil || res.Usage.In != 3 || res.Usage.Out != 4 {
		t.Errorf("stream usage tap = %+v, want in=3 out=4", res.Usage)
	}
	if !res.Stream {
		t.Error("result must record a streamed call")
	}
}

// Invariant 5 — translation never widens the failure set. A translated hop
// that 429s fails over to a native hop exactly as before, and the park
// carries the upstream's own words.
func TestTranslatedHopFailureStillFailsOver(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The translated hop must still have received a translated body.
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"model":"step-5-preview-free"`) {
			t.Errorf("translated hop body = %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"Maximum 5 requests within 1 minutes"}}`))
	}))
	defer failing.Close()

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c2","choices":[{"message":{"role":"assistant","content":"from native hop"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`))
	}))
	defer healthy.Close()

	clientBody := `{"model":"daily","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	d := newDispatcher()
	d.SetRetries(0)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(clientBody))
	rec := httptest.NewRecorder()
	served, res, err := d.ForwardCandidates(context.Background(), rec, req, []Route{
		translatedRoute(failing.URL, "step-5-preview-free"),
		testRoute(healthy.URL, provider.SurfaceAnthropic),
	}, []byte(clientBody))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != 200 {
		t.Fatalf("status = %d, want the native hop's 200", res.Status)
	}
	if served.Translated {
		t.Error("the SERVED route must be the native hop, not the failed translated one")
	}
	if !strings.Contains(rec.Body.String(), "from native hop") {
		t.Errorf("client got %s", rec.Body.String())
	}

	// The failed translated hop is parked with the upstream's own words —
	// existing classifyHop/recordHop machinery, unchanged.
	parks := d.Cooldowns()
	if len(parks) != 1 {
		t.Fatalf("parks = %+v, want exactly the translated hop parked", parks)
	}
	if parks[0].Model != "step-5-preview-free" {
		t.Errorf("parked model = %q", parks[0].Model)
	}
	if !strings.Contains(parks[0].Reason, "429") {
		t.Errorf("park reason = %q, want the 429 cause", parks[0].Reason)
	}
}

// A translated hop's non-2xx body reaches the client as an anthropic error
// envelope, never as openai-shaped JSON.
func TestTranslatedHopErrorBodyIsAnthropicShaped(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"model does not support this protocol"}}`))
	}))
	defer upstream.Close()

	clientBody := `{"model":"daily","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	d := newDispatcher()
	d.SetRetries(0)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(clientBody))
	rec := httptest.NewRecorder()
	_, res, err := d.ForwardCandidates(context.Background(), rec, req,
		[]Route{translatedRoute(upstream.URL, "step-5-preview-free")}, []byte(clientBody))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != http.StatusBadRequest {
		t.Fatalf("status = %d", res.Status)
	}
	body := rec.Body.String()
	// The distinguishing feature is the TOP-LEVEL shape: an anthropic error
	// envelope is {"type":"error","error":{...}}; the nested type value is
	// the upstream's and is preserved verbatim (including the openai honour
	// phrasing).
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("client error body is not JSON: %v (%s)", err, body)
	}
	if env.Type != "error" {
		t.Errorf("top-level type = %q, want \"error\" (anthropic envelope)", env.Type)
	}
	if !strings.Contains(env.Error.Message, "does not support this protocol") {
		t.Errorf("upstream words lost: %q", env.Error.Message)
	}
	// The ledger still records the upstream's own words.
	if !strings.Contains(res.Err, "does not support this protocol") {
		t.Errorf("err column = %q", res.Err)
	}
}
