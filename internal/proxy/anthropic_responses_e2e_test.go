package proxy

// The anthropic→responses leg end to end: an anthropic client (Claude Code)
// whose hop is curated onto the responses wire. The upstream must receive a
// translated RESPONSES body and answer on /responses; the client must receive
// an ANTHROPIC body — non-streaming and streaming.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func responsesWireAnthropicRoute(url, model string) Route {
	rt := testRoute(url, provider.SurfaceAnthropic)
	rt.Model = model
	rt.Alias = "daily"
	rt.ResponsesWire = true
	return rt
}

func TestAnthropicClientOnResponsesWireNonStream(t *testing.T) {
	var got []byte
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello from sol"}]}],"usage":{"input_tokens":11,"output_tokens":3}}`))
	}))
	defer upstream.Close()

	clientBody := `{"model":"daily","max_tokens":128,"system":"be terse","messages":[{"role":"user","content":"hi"}]}`
	d := newDispatcher()
	d.SetRetries(0)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(clientBody))
	rec := httptest.NewRecorder()
	if _, _, err := d.ForwardCandidates(context.Background(), rec, req,
		[]Route{responsesWireAnthropicRoute(upstream.URL, "gpt-6.1-sol")}, []byte(clientBody)); err != nil {
		t.Fatal(err)
	}

	if !strings.HasSuffix(gotPath, "/responses") {
		t.Fatalf("upstream path = %q, want the responses wire", gotPath)
	}
	var sent struct {
		Model           string `json:"model"`
		Instructions    string `json:"instructions"`
		MaxOutputTokens int    `json:"max_output_tokens"`
		Input           []any  `json:"input"`
	}
	if err := json.Unmarshal(got, &sent); err != nil {
		t.Fatalf("upstream body is not responses-shaped: %v (%s)", err, got)
	}
	if sent.Model != "gpt-6.1-sol" || sent.MaxOutputTokens != 128 || sent.Instructions != "be terse" || len(sent.Input) != 1 {
		t.Fatalf("upstream body = %s", got)
	}
	body := rec.Body.String()
	var out struct {
		Type    string `json:"type"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("client body is not anthropic-shaped: %v (%s)", err, body)
	}
	if out.Type != "message" || out.Model != "daily" || len(out.Content) != 1 || out.Content[0].Text != "hello from sol" {
		t.Fatalf("client body = %s", body)
	}
}

func TestAnthropicClientOnResponsesWireStreams(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: response.created
data: {"type":"response.created","response":{"id":"resp_s"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"streamed"}

event: response.completed
data: {"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":2}}}

`))
	}))
	defer upstream.Close()

	clientBody := `{"model":"daily","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	d := newDispatcher()
	d.SetRetries(0)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(clientBody))
	rec := httptest.NewRecorder()
	if _, _, err := d.ForwardCandidates(context.Background(), rec, req,
		[]Route{responsesWireAnthropicRoute(upstream.URL, "gpt-6.1-sol")}, []byte(clientBody)); err != nil {
		t.Fatal(err)
	}

	body := rec.Body.String()
	for _, want := range []string{"event: message_start", "event: content_block_delta", `"text_delta"`, "streamed", "event: message_stop"} {
		if !strings.Contains(body, want) {
			t.Fatalf("client stream missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `"choices"`) {
		t.Fatalf("chat-shaped bytes leaked to an anthropic client:\n%s", body)
	}
}
