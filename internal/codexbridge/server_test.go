package codexbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeExec struct {
	reasoning []string
	calls     []ToolCall
	deltas    []string
	err       error
	errAt     int // fail after this many deltas (0 = before any)
	usage     Usage
	runs      int
	last      Request
}

func (f *fakeExec) Run(_ context.Context, req Request, sink func(Delta) error) (Result, error) {
	f.runs++
	f.last = req
	var text, reasoning strings.Builder
	for _, r := range f.reasoning {
		reasoning.WriteString(r)
		if sink != nil {
			if err := sink(Delta{Reasoning: r}); err != nil {
				return Result{}, err
			}
		}
	}
	for i, d := range f.deltas {
		if f.err != nil && i == f.errAt {
			return Result{}, f.err
		}
		text.WriteString(d)
		if sink != nil {
			if err := sink(Delta{Content: d}); err != nil {
				return Result{}, err
			}
		}
	}
	if f.err != nil {
		return Result{}, f.err
	}
	return Result{Text: text.String(), Reasoning: reasoning.String(), Usage: f.usage, ToolCalls: f.calls}, nil
}

func (f *fakeExec) Models(context.Context) ([]Model, error) {
	return []Model{{ID: "gpt-6.1-sol", Efforts: []string{"low", "high"}}, {ID: "gpt-reserve", Hidden: true}}, nil
}

func (f *fakeExec) RateLimits(context.Context) (RateLimits, error) {
	return RateLimits{PlanType: "plus",
		Primary:   &Window{UsedPercent: 1, WindowDurationMins: 300, ResetsAt: 1792739595},
		Secondary: &Window{UsedPercent: 12.5, WindowDurationMins: 10080, ResetsAt: 1793326395}}, nil
}

func testServer(f *fakeExec) *Server {
	return &Server{Runner: f, Token: "secret", Version: "test", Info: map[string]any{"status": "ok"},
		Logf: func(string, ...any) {}, Now: func() time.Time { return time.Unix(1792720000, 0) }}
}

func do(t *testing.T, s *Server, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

const hiBody = `{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"hi"}]}`

func TestHealthzHasExactlyTwoFields(t *testing.T) {
	rec := do(t, testServer(&fakeExec{}), "GET", "/healthz", "", "")
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if rec.Code != 200 || len(m) != 2 || m["status"] != "ok" || m["version"] != "test" {
		t.Fatalf("healthz = %d %v, want exactly {status, version}", rec.Code, m)
	}
}

func TestEndpointsRequireTheBearer(t *testing.T) {
	s := testServer(&fakeExec{})
	for _, tc := range []struct{ method, path, token string }{
		{"POST", "/v1/chat/completions", ""}, {"POST", "/v1/chat/completions", "wrong"},
		{"GET", "/v1/models", ""}, {"GET", "/v1/usage", "wrong"},
	} {
		if rec := do(t, s, tc.method, tc.path, hiBody, tc.token); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s token=%q: %d, want 401", tc.method, tc.path, tc.token, rec.Code)
		}
	}
}

func TestNonStreamCompletion(t *testing.T) {
	f := &fakeExec{deltas: []string{"po", "ng"}, usage: Usage{InputTokens: 5093, CachedTokens: 4992, OutputTokens: 5, ReasoningTokens: 2}}
	rec := do(t, testServer(f), "POST", "/v1/chat/completions", hiBody, "secret")
	var got struct {
		Object  string `json:"object"`
		Choices []struct {
			Message      struct{ Role, Content string } `json:"message"`
			FinishReason string                         `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Prompt     int64 `json:"prompt_tokens"`
			Completion int64 `json:"completion_tokens"`
			Details    struct {
				Cached int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got.Object != "chat.completion" || got.Choices[0].Message.Content != "pong" || got.Choices[0].FinishReason != "stop" ||
		got.Usage.Prompt != 5093 || got.Usage.Completion != 5 || got.Usage.Details.Cached != 4992 {
		t.Fatalf("response = %+v", got)
	}
}

func TestStreamEmitsChunksUsageAndDone(t *testing.T) {
	f := &fakeExec{deltas: []string{"po", "ng"}, usage: Usage{InputTokens: 10, OutputTokens: 2}}
	rec := do(t, testServer(f), "POST", "/v1/chat/completions",
		`{"model":"gpt-6.1-sol","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`, "secret")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	events := strings.Split(strings.TrimSpace(rec.Body.String()), "\n\n")
	if events[len(events)-1] != "data: [DONE]" {
		t.Fatalf("stream does not end in [DONE]: %q", events[len(events)-1])
	}
	var content strings.Builder
	var sawRole, sawStop, sawUsage bool
	for _, e := range events[:len(events)-1] {
		var c struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta        map[string]any `json:"delta"`
				FinishReason *string        `json:"finish_reason"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(e, "data: ")), &c); err != nil || c.Object != "chat.completion.chunk" {
			t.Fatalf("bad event %q", e)
		}
		if c.Usage != nil {
			sawUsage = true
			continue
		}
		if c.Choices[0].Delta["role"] == "assistant" {
			sawRole = true
		}
		if s, ok := c.Choices[0].Delta["content"].(string); ok {
			content.WriteString(s)
		}
		if c.Choices[0].FinishReason != nil && *c.Choices[0].FinishReason == "stop" {
			sawStop = true
		}
	}
	if content.String() != "pong" || !sawRole || !sawStop || !sawUsage {
		t.Fatalf("content=%q role=%v stop=%v usage=%v", content.String(), sawRole, sawStop, sawUsage)
	}
}

// A failure before the first token keeps its real status (429 lets a combo
// skip the hop); after it, it travels in-stream.
func TestStreamFailureBeforeFirstByteKeepsItsStatus(t *testing.T) {
	f := &fakeExec{deltas: []string{"x"}, err: &NativeError{Status: 429, Type: "rate_limit_error", Code: "rate_limit", Message: "codex-bridge: usage limit"}}
	rec := do(t, testServer(f), "POST", "/v1/chat/completions",
		`{"model":"gpt-6.1-sol","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "secret")
	if rec.Code != 429 || !strings.Contains(rec.Body.String(), "usage limit") {
		t.Fatalf("%d %s, want 429 JSON", rec.Code, rec.Body)
	}
}

func TestStreamFailureMidStreamIsAnErrorEvent(t *testing.T) {
	f := &fakeExec{deltas: []string{"a", "b"}, errAt: 1, err: &NativeError{Status: 502, Type: "api_error", Code: "native_closed", Message: "codex-bridge: exited"}}
	rec := do(t, testServer(f), "POST", "/v1/chat/completions",
		`{"model":"gpt-6.1-sol","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "secret")
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `"code":"native_closed"`) || !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Fatalf("%d %s", rec.Code, body)
	}
}

// Without Landlock (and no override) tool requests are refused before any
// spawn, with the reason and the runbook pointer (decision §4.7).
func TestToolsAreRefusedWhenTheHostHasNoLandlock(t *testing.T) {
	f := &fakeExec{}
	s := testServer(f)
	s.ToolsOff = "Landlock is unavailable (the kernel has no Landlock (needs Linux 5.13+))"
	rec := do(t, s, "POST", "/v1/chat/completions",
		`{"model":"gpt-6.1-sol","tools":[{"type":"function","function":{"name":"f"}}],"messages":[{"role":"user","content":"hi"}]}`, "secret")
	body := rec.Body.String()
	if rec.Code != 400 || !strings.Contains(body, "chat_tools_unsupported") || !strings.Contains(body, "client tools are disabled on this host: Landlock is unavailable") ||
		!strings.Contains(body, "docs/codex-bridge-runbook.md#platform-requirements--read-this-first") || f.runs != 0 {
		t.Fatalf("%d %s runs=%d", rec.Code, body, f.runs)
	}
	// Plain text still works on the same host.
	f.deltas = []string{"ok"}
	if rec := do(t, s, "POST", "/v1/chat/completions", hiBody, "secret"); rec.Code != 200 {
		t.Fatalf("text request on a no-Landlock host: %d %s", rec.Code, rec.Body)
	}
}

func TestToolCallsAreReturnedInBothShapes(t *testing.T) {
	f := &fakeExec{calls: []ToolCall{{ID: "call-a", Name: "get_weather", Arguments: `{"city":"KL"}`}}}
	s := testServer(f)
	body := `{"model":"gpt-6.1-sol","tools":[{"type":"function","function":{"name":"get_weather"}}],"messages":[{"role":"user","content":"weather?"}]}`
	rec := do(t, s, "POST", "/v1/chat/completions", body, "secret")
	var got struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct{ Name, Arguments string }
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	c := got.Choices[0]
	if c.FinishReason != "tool_calls" || c.Message.Content != nil || len(c.Message.ToolCalls) != 1 ||
		c.Message.ToolCalls[0].ID != "call-a" || c.Message.ToolCalls[0].Function.Arguments != `{"city":"KL"}` || len(f.last.Tools) != 1 {
		t.Fatalf("non-stream = %+v (tools passed: %d)", c, len(f.last.Tools))
	}

	rec = do(t, s, "POST", "/v1/chat/completions", strings.Replace(body, `"model"`, `"stream":true,"model"`, 1), "secret")
	stream := rec.Body.String()
	if !strings.Contains(stream, `"tool_calls":[{"function":{"arguments":"{\"city\":\"KL\"}","name":"get_weather"},"id":"call-a","index":0,"type":"function"}]`) ||
		!strings.Contains(stream, `"finish_reason":"tool_calls"`) || !strings.HasSuffix(strings.TrimSpace(stream), "data: [DONE]") {
		t.Fatalf("stream = %s", stream)
	}
}

func TestUnknownModelAndEffortAreRefusedBeforeAnySpawn(t *testing.T) {
	f := &fakeExec{}
	s := testServer(f)
	rec := do(t, s, "POST", "/v1/chat/completions", `{"model":"gpt-nope","messages":[{"role":"user","content":"hi"}]}`, "secret")
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "model_not_found") {
		t.Fatalf("unknown model: %d %s", rec.Code, rec.Body)
	}
	rec = do(t, s, "POST", "/v1/chat/completions", `{"model":"gpt-6.1-sol","reasoning_effort":"ultra","messages":[{"role":"user","content":"hi"}]}`, "secret")
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "offers low, high") {
		t.Fatalf("bad effort: %d %s", rec.Code, rec.Body)
	}
	if f.runs != 0 {
		t.Fatalf("runs = %d, want 0", f.runs)
	}
}

func TestModelsListsHiddenModels(t *testing.T) {
	rec := do(t, testServer(&fakeExec{}), "GET", "/v1/models", "", "secret")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"gpt-reserve"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// The provider card reads this through provider.ClassifyQuota's percent shape.
func TestUsageServesThePlanWindows(t *testing.T) {
	rec := do(t, testServer(&fakeExec{}), "GET", "/v1/usage", "", "secret")
	var got struct {
		Usage map[string]struct {
			Percent  float64 `json:"percent"`
			ResetsAt string  `json:"resetsAt"`
		} `json:"usage"`
		Plan string `json:"plan"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got.Usage["rolling"].Percent != 1 || got.Usage["weekly"].Percent != 12.5 || got.Plan != "plus" ||
		got.Usage["weekly"].ResetsAt != time.Unix(1793326395, 0).UTC().Format(time.RFC3339) {
		t.Fatalf("usage = %+v", got)
	}
}
