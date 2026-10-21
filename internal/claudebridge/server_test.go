package claudebridge

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// fixtureFrames loads native output recorded live on CLI 2.1.306 (redacted;
// design §2.1), so the tests replay what native really emits.
func fixtureFrames(t *testing.T, name string) []Frame {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var frames []Frame
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		f, err := ParseFrame(line)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		frames = append(frames, f)
	}
	return frames
}

const weatherToolRequest = `{"model":"claude-haiku-5-5","max_tokens":1024,` +
	`"tools":[{"name":"get_weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],` +
	`"messages":[{"role":"user","content":"Weather in Paris?"}]`

// M2 replaces M1's refusal: a tool-bearing request reaches the CLI and the
// tool call comes back. Native's own error_max_turns ending (it answers the
// unknown tool locally, then hits --max-turns 1) is the success path.
func TestMessagesReturnsToolUseNonStream(t *testing.T) {
	f := &fakeExec{frames: fixtureFrames(t, "m2_single.jsonl")}
	rec := post(t, testServer(f), weatherToolRequest+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(string(f.req.Tools), "get_weather") {
		t.Errorf("tools did not reach the runner: %s", f.req.Tools)
	}
	var msg struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string         `json:"type"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
			Text  string         `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("body is not JSON: %s", rec.Body)
	}
	if msg.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", msg.StopReason)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "tool_use" || msg.Content[0].Name != "get_weather" ||
		msg.Content[0].ID == "" || msg.Content[0].Input["city"] != "Paris" {
		t.Errorf("content = %+v, want one get_weather tool_use for Paris", msg.Content)
	}
	if strings.Contains(rec.Body.String(), "No such tool available") {
		t.Error("native's synthetic tool_result leaked to the client")
	}
}

func TestMessagesStreamsToolUse(t *testing.T) {
	f := &fakeExec{frames: fixtureFrames(t, "m2_single.jsonl")}
	rec := post(t, testServer(f), weatherToolRequest+`,"stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body)
	}
	out := rec.Body.String()
	for _, want := range []string{`"type":"tool_use"`, `"name":"get_weather"`, "input_json_delta",
		`"stop_reason":"tool_use"`, "event: message_stop"} {
		if !strings.Contains(out, want) {
			t.Errorf("stream missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "No such tool available") {
		t.Error("native's synthetic tool_result leaked into the stream")
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
