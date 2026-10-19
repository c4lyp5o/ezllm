package claudebridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeExec struct {
	frames  []Frame
	err     error
	alias   string
	req     Request
	sawSink bool
}

func (f *fakeExec) Run(_ context.Context, req Request, alias string, sink func(Frame) error) (Result, error) {
	f.alias, f.req = alias, req
	for _, fr := range f.frames {
		f.sawSink = true
		if sink != nil {
			if err := sink(fr); err != nil {
				return Result{}, err
			}
		}
	}
	if f.err != nil {
		return Result{}, f.err
	}
	return Result{Usage: Usage{InputTokens: 170, OutputTokens: 69}}, nil
}

func testServer(f Executor) *Server {
	return &Server{Runner: f, Token: "secret", Version: "test", Logf: func(string, ...any) {}}
}

func post(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", "secret")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestMessagesRequiresTheSharedSecret(t *testing.T) {
	s := testServer(&fakeExec{})
	for _, headers := range []map[string]string{
		{},
		{"x-api-key": "wrong"},
		{"Authorization": "Bearer wrong"},
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages",
			strings.NewReader(`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":"hi"}]}`))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("headers %v: status %d, want 401", headers, rec.Code)
		}
	}
}

func TestMessagesRefusesToolRequests(t *testing.T) {
	f := &fakeExec{}
	rec := post(t, testServer(f), `{"model":"claude-haiku-5-5","max_tokens":8,`+
		`"tools":[{"type":"function","function":{"name":"read_file"}}],`+
		`"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
	if f.sawSink {
		t.Fatal("a refused request must never reach the CLI")
	}
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not JSON: %s", rec.Body)
	}
	inner, _ := env["error"].(map[string]any)
	if inner["code"] != "chat_tools_unsupported" {
		t.Errorf("code = %v, want chat_tools_unsupported (a refusal names its reason)", inner["code"])
	}
}

func TestMessagesRejectsUnknownModelWithoutSilentFallback(t *testing.T) {
	rec := post(t, testServer(&fakeExec{}),
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unknown_model") {
		t.Errorf("body = %s", rec.Body)
	}
}

func TestMessagesNonStreamAssemblesMessage(t *testing.T) {
	f := &fakeExec{frames: []Frame{
		frame(t, "stream_event", map[string]any{"type": "message_start",
			"message": map[string]any{"id": "msg_01Z", "model": "claude-haiku-5-5"}}),
		frame(t, "stream_event", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": "Hello"}}),
		frame(t, "stream_event", map[string]any{"type": "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn"},
			"usage": map[string]any{"input_tokens": 170, "output_tokens": 69}}),
		frame(t, "stream_event", map[string]any{"type": "message_stop"}),
	}}
	rec := post(t, testServer(f), `{"model":"claude-haiku-5-5","max_tokens":32,`+
		`"messages":[{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body)
	}
	if f.alias != "haiku" {
		t.Errorf("alias = %q, want haiku (claude-haiku-5-5 is addressed by family)", f.alias)
	}
	var msg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("response is not JSON: %s", rec.Body)
	}
	if msg["id"] != "msg_01Z" {
		t.Errorf("id = %v, want native's msg_01Z", msg["id"])
	}
	usage, _ := msg["usage"].(map[string]any)
	if usage["input_tokens"] != float64(170) {
		t.Errorf("usage = %v", usage)
	}
}

func TestMessagesStreamEmitsAnthropicSSE(t *testing.T) {
	f := &fakeExec{frames: []Frame{
		frame(t, "stream_event", map[string]any{"type": "message_start",
			"message": map[string]any{"id": "msg_01S", "model": "claude-haiku-5-5"}}),
		frame(t, "stream_event", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": "Hi"}}),
		frame(t, "stream_event", map[string]any{"type": "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn"},
			"usage": map[string]any{"output_tokens": 3}}),
		frame(t, "stream_event", map[string]any{"type": "message_stop"}),
	}}
	rec := post(t, testServer(f), `{"model":"claude-sonnet-5-5","stream":true,"max_tokens":32,`+
		`"messages":[{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	out := rec.Body.String()
	for _, want := range []string{"event: message_start", "event: content_block_delta", "event: message_stop"} {
		if !strings.Contains(out, want) {
			t.Errorf("stream missing %q:\n%s", want, out)
		}
	}
	if f.alias != "sonnet" {
		t.Errorf("alias = %q, want sonnet", f.alias)
	}
}

// A plan limit must reach the client as 429 with the CLI's own words, not as an
// undifferentiated 500 — that is what makes ezllm's failover behave.
func TestMessagesMapsNativePlanLimitTo429(t *testing.T) {
	f := &fakeExec{err: &NativeError{Status: 429, Type: "rate_limit_error", Code: "rate_limit",
		Message: "API Error: Server is temporarily limiting requests (not your usage limit)"}}
	rec := post(t, testServer(f), `{"model":"claude-haiku-5-5","max_tokens":8,`+
		`"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "rate_limit_error") {
		t.Errorf("body = %s", rec.Body)
	}
}

func TestHealthzKeepsTheTwoFieldContract(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	testServer(&fakeExec{}).Handler().ServeHTTP(rec, req)
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("healthz is not JSON: %s", rec.Body)
	}
	if len(got) != 2 {
		t.Errorf("healthz must carry exactly {status,version}, got %v", got)
	}
}
