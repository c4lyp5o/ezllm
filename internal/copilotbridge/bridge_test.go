package copilotbridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// One captured headless turn, trimmed of the opaque blobs. This is the SHAPE
// measured on CLI 1.0.105 (piped prompt, `-s --output-format=json`): the
// numbers below are from that run, so a parser change that reorders or
// re-includes the cache slices fails here rather than in production.
const transcript = `
{"type":"session.mcp_server_status_changed","data":{"serverName":"github-mcp-server","status":"pending"},"ephemeral":true}
{"type":"session.auto_mode_resolved","data":{"chosenModel":"gpt-6.1-sol","availableModels":["gpt-6.1-sol","mai-code-1.1-flash"]}}
{"type":"user.message","data":{"content":"Reply with exactly: piped-ok"}}
{"type":"assistant.message_start","data":{"messageId":"m1"}}
{"type":"assistant.message_delta","data":{"messageId":"m1","deltaContent":"piped-"}}
{"type":"assistant.message_delta","data":{"messageId":"m1","deltaContent":"ok"}}
{"type":"assistant.message","data":{"messageId":"m1","model":"gpt-6.1-sol","providerId":"copilot","content":"piped-ok","toolRequests":[]}}
{"type":"session.usage_checkpoint","data":{"totalPremiumRequests":1,"accountingSnapshot":{"totalPremiumRequestCost":1,"tokenDetails":{"input":{"tokenCount":3},"cache_read":{"tokenCount":0},"cache_write":{"tokenCount":13960},"output":{"tokenCount":7}},"providerModelMetrics":[{"provider":{"id":"copilot","kind":"copilot"},"modelId":"gpt-6.1-sol","metrics":{"requests":{"count":1,"cost":1},"usage":{"inputTokens":13963,"outputTokens":7,"cacheReadTokens":0,"cacheWriteTokens":13960,"reasoningTokens":0}}}]}}}
{"type":"assistant.idle","data":{}}
{"type":"result","timestamp":"2026-11-05T04:29:25.525Z","sessionId":"sess-123","exitCode":0,"usage":{"premiumRequests":1,"totalApiDurationMs":2698,"sessionDurationMs":6274}}
`

func TestParseStreamMatchesTheMeasuredTranscript(t *testing.T) {
	var deltas []string
	res, err := parseStream(strings.NewReader(transcript), func(s string) error {
		deltas = append(deltas, s)
		return nil
	})
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if got := strings.Join(deltas, ""); got != "piped-ok" {
		t.Errorf("streamed deltas = %q, want %q (streaming must be real, not one blob)", got, "piped-ok")
	}
	if res.Text != "piped-ok" {
		t.Errorf("final text = %q, want the assistant message content", res.Text)
	}
	if res.SessionID != "sess-123" {
		t.Errorf("session id = %q, want sess-123", res.SessionID)
	}
	u := res.Usage
	// prompt_tokens is INCLUSIVE: input (cold 3) + cache_read (0) + cache_write
	// (13,960) = 13,963, which is the CLI's own prompt_tokens for this turn.
	if u.PromptTokens != 13963 {
		t.Errorf("prompt tokens = %d, want 13963 (input + cache_read + cache_write)", u.PromptTokens)
	}
	if u.CachedWrite != 13960 || u.CachedRead != 0 {
		t.Errorf("cache split = read %d / write %d, want 0 / 13960", u.CachedRead, u.CachedWrite)
	}
	if u.CompletionTokens != 7 {
		t.Errorf("completion tokens = %d, want 7", u.CompletionTokens)
	}
	if u.PremiumRequests != 1 {
		t.Errorf("premium requests = %g, want 1", u.PremiumRequests)
	}
	if u.Model != "gpt-6.1-sol" {
		t.Errorf("model = %q, want gpt-6.1-sol", u.Model)
	}
}

// A missing usage block must stay ZERO — the bridge reports "unknown", never an
// invented number and never a silent zero that reads as a free call.
func TestParseStreamWithoutUsageBlock(t *testing.T) {
	res, err := parseStream(strings.NewReader(`{"type":"assistant.message","data":{"content":"hi"}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "hi" {
		t.Fatalf("text = %q", res.Text)
	}
	if res.Usage.PromptTokens != 0 || res.Usage.CompletionTokens != 0 {
		t.Errorf("usage = %+v, want zero (unknown, not estimated)", res.Usage)
	}
}

func TestParseRequestFlattensHistoryAndSystem(t *testing.T) {
	body := `{"model":"gpt-6.1-sol","messages":[
		{"role":"system","content":"You are terse."},
		{"role":"user","content":"first"},
		{"role":"assistant","content":"answer one"},
		{"role":"user","content":[{"type":"text","text":"second"}]}
	]}`
	req, err := ParseRequest([]byte(body))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	for _, want := range []string{"System instructions:", "You are terse.", "User: first", "Assistant: answer one", "User: second"} {
		if !strings.Contains(req.Prompt, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, req.Prompt)
		}
	}
	if req.Turns != 3 {
		t.Errorf("turns = %d, want 3", req.Turns)
	}
	if strings.Index(req.Prompt, "User: second") < strings.Index(req.Prompt, "User: first") {
		t.Error("prompt order broken: the latest user turn must come last")
	}
}

func TestParseRequestRefusesWhatTheCLICannotCarry(t *testing.T) {
	cases := map[string]struct{ body, code string }{
		"non-function tool": {`{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"type":"custom","custom":{"name":"f"}}]}`, "chat_tools_unsupported"},
		"duplicate tool":    {`{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"f"}},{"type":"function","function":{"name":"f"}}]}`, "invalid_request"},
		"choice not listed": {`{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":{"type":"function","function":{"name":"g"}}}`, "invalid_request"},
		"legacy functions":  {`{"model":"m","messages":[{"role":"user","content":"x"}],"functions":[{"name":"f"}]}`, "chat_tools_unsupported"},
		"n>1":               {`{"model":"m","n":2,"messages":[{"role":"user","content":"x"}]}`, "invalid_request"},
		"json format":       {`{"model":"m","response_format":{"type":"json_object"},"messages":[{"role":"user","content":"x"}]}`, "protocol_unsupported"},
		"image part":        {`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:x"}}]}]}`, "invalid_request"},
		"no user turn":      {`{"model":"m","messages":[{"role":"system","content":"x"}]}`, "invalid_request"},
	}
	for name, tc := range cases {
		_, err := ParseRequest([]byte(tc.body))
		if err == nil {
			t.Errorf("%s: accepted, want refusal", name)
			continue
		}
		var u Unsupported
		if !strings.Contains(err.Error(), "") {
			t.Fatal("unreachable")
		}
		if ok := asUnsupported(err, &u); !ok {
			t.Errorf("%s: error is not a typed Unsupported (%T)", name, err)
			continue
		}
		if u.Code != tc.code {
			t.Errorf("%s: code = %q, want %q", name, u.Code, tc.code)
		}
	}
}

func asUnsupported(err error, out *Unsupported) bool {
	var u Unsupported
	if e, ok := err.(Unsupported); ok {
		u = e
	} else {
		return false
	}
	*out = u
	return true
}

// The usage map is the bridge's contract with the ledger's usage tap: an
// inclusive prompt total plus the cached slice beside it.
func TestOpenAIUsageShape(t *testing.T) {
	b, _ := json.Marshal(openAIUsage(Usage{PromptTokens: 13963, CompletionTokens: 7, CachedRead: 1536, CachedWrite: 12427, PremiumRequests: 1}))
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["prompt_tokens"].(float64) != 13963 || got["completion_tokens"].(float64) != 7 {
		t.Errorf("usage = %v", got)
	}
	details, _ := got["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"].(float64) != 1536 {
		t.Errorf("cached_tokens = %v, want 1536", details["cached_tokens"])
	}
}

// A CLI that sends only the final assistant.message (no deltas) must still
// deliver the answer on a stream.
type fakeExec struct {
	res Result
	err error
}

func (f fakeExec) Run(_ context.Context, _, _ string, _ func(string) error) (Result, error) {
	return f.res, f.err
}

func (f fakeExec) RunChannel(_ context.Context, _ ChannelTurn, _ func(string) error) (Result, error) {
	return f.res, f.err
}

func TestStreamEmitsFinalTextWhenNoDeltasArrived(t *testing.T) {
	s := &Server{Runner: fakeExec{res: Result{Text: "final-only"}}, Token: "t", Models: []Model{{ID: "auto"}}}
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"auto","stream":true,"messages":[{"role":"user","content":"x"}]}`))
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"content":"final-only"`) {
		t.Fatalf("stream lost the answer:\n%s", rec.Body.String())
	}
}

func TestChildEnvDropsSecrets(t *testing.T) {
	env := childEnv([]string{"PATH=/bin", "USER=calypso", "XDG_CACHE_HOME=/c", "EZLLM_ADMIN_TOKEN=s3", "OPENAI_API_KEY=k", "HOME=/h"}, "/ch")
	got := strings.Join(env, "\n")
	for _, bad := range []string{"EZLLM_ADMIN_TOKEN", "OPENAI_API_KEY"} {
		if strings.Contains(got, bad) {
			t.Errorf("child env leaks %s", bad)
		}
	}
	for _, want := range []string{"PATH=/bin", "HOME=/h", "USER=calypso", "XDG_CACHE_HOME=/c", "COPILOT_HOME=/ch"} {
		if !strings.Contains(got, want) {
			t.Errorf("child env missing %s", want)
		}
	}
}

func TestClassifyExitIsSpecific(t *testing.T) {
	code := func(stderr string) string {
		var f *Failure
		errors.As(ClassifyExit(errors.New("exit 1"), stderr), &f)
		return f.Code
	}
	for in, want := range map[string]string{
		"Error: not logged in":                     "not_logged_in",
		"model gpt-x is not included in your plan": "model_not_in_plan",
		"premium request limit reached":            "quota_exhausted",
		"update available: feature not available":  "cli_failed",
		"something else":                           "cli_failed",
	} {
		if got := code(in); got != want {
			t.Errorf("%q -> %s, want %s", in, got, want)
		}
	}
}

func TestFailureErrorToleratesNilCause(t *testing.T) {
	if got := (&Failure{Msg: "m"}).Error(); got != "m" {
		t.Errorf("got %q", got)
	}
}

func TestQueueFullIs429WithRetryAfter(t *testing.T) {
	s := &Server{Runner: fakeExec{err: &Failure{Code: "queue_full", Msg: "busy"}}, Token: "t", Models: []Model{{ID: "auto"}}}
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"x"}]}`))
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "5" {
		t.Fatalf("code=%d retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func TestStatusServesRunnerStats(t *testing.T) {
	r := &Runner{Bin: "/nonexistent", MaxConcurrent: 3}
	s := &Server{Runner: r, Token: "t", Info: map[string]any{"status": "ok"}}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	rt, _ := got["runtime"].(map[string]any)
	if rt == nil || rt["max_concurrent"].(float64) != 3 {
		t.Fatalf("runtime stats missing: %v", got)
	}
}

func TestParseStreamReadsAvailableModels(t *testing.T) {
	res, err := parseStream(strings.NewReader(transcript), nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.AvailableModels, ",") != "gpt-6.1-sol,mai-code-1.1-flash" {
		t.Errorf("available = %v", res.AvailableModels)
	}
}

func TestCatalogLearnsFromAutoTurnsAfterTheSeed(t *testing.T) {
	s := &Server{
		Runner: fakeExec{res: Result{Text: "ok", AvailableModels: []string{"gpt-6.1-sol", "mai-code-1.1-flash", "bad id!", "../x"}}},
		Token:  "t", Models: []Model{{ID: "auto"}, {ID: "gpt-6.1-sol"}},
	}
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer t")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	// Before any turn the unseeded id is refused without a spawn.
	if rec := do("POST", "/v1/chat/completions", `{"model":"mai-code-1.1-flash","messages":[{"role":"user","content":"x"}]}`); rec.Code != 403 || !strings.Contains(rec.Body.String(), "model_not_in_plan") {
		t.Fatalf("unlearned model code = %d body %s", rec.Code, rec.Body)
	}
	if rec := do("POST", "/v1/chat/completions", `{"model":"auto","messages":[{"role":"user","content":"x"}]}`); rec.Code != 200 {
		t.Fatalf("auto turn = %d %s", rec.Code, rec.Body)
	}
	var list struct{ Data []Model }
	_ = json.Unmarshal(do("GET", "/v1/models", "").Body.Bytes(), &list)
	var ids []string
	for _, m := range list.Data {
		ids = append(ids, m.ID)
	}
	if got := strings.Join(ids, ","); got != "auto,gpt-6.1-sol,mai-code-1.1-flash" {
		t.Errorf("catalog = %s (seed first, learned after, invalid ids dropped, no duplicates)", got)
	}
	if rec := do("POST", "/v1/chat/completions", `{"model":"mai-code-1.1-flash","messages":[{"role":"user","content":"x"}]}`); rec.Code != 200 {
		t.Errorf("learned model refused: %d", rec.Code)
	}
}

func TestChildEnvDefaultsAWritableCacheDir(t *testing.T) {
	got := strings.Join(childEnv([]string{"PATH=/bin"}, ""), "\n")
	if !strings.Contains(got, "XDG_CACHE_HOME=") {
		t.Error("no cache dir: the CLI dies on HOME=/ with EACCES mkdir /.cache")
	}
	kept := strings.Join(childEnv([]string{"XDG_CACHE_HOME=/data/c"}, ""), "\n")
	if strings.Count(kept, "XDG_CACHE_HOME=") != 1 || !strings.Contains(kept, "XDG_CACHE_HOME=/data/c") {
		t.Errorf("operator-set cache dir not preserved: %s", kept)
	}
}

// ── client tools ────────────────────────────────────────────────────────────

const weatherTools = `"tools":[{"type":"function","function":{"name":"get_weather","description":"Weather for a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]`

func TestToolsTravelBesideThePromptNotInsideIt(t *testing.T) {
	req, err := ParseRequest([]byte(`{"model":"m","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"weather in Paris?"}],` + weatherTools + `}`))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("Tools = %+v", req.Tools)
	}
	if req.System != "Be brief." || req.Transcript != "User: weather in Paris?" {
		t.Errorf("System = %q, Transcript = %q", req.System, req.Transcript)
	}
	if strings.Contains(req.Prompt, "get_weather") || strings.Contains(req.Prompt, "tool_call") {
		t.Errorf("tool schemas leaked into the prompt: %q", req.Prompt)
	}
}

func TestToolChoiceParsing(t *testing.T) {
	base := `{"model":"m","messages":[{"role":"user","content":"x"}],` + weatherTools
	none, _ := ParseRequest([]byte(base + `,"tool_choice":"none"}`))
	if len(none.Tools) != 0 {
		t.Errorf("tool_choice none must drop the tools: %+v", none.Tools)
	}
	req, _ := ParseRequest([]byte(base + `,"tool_choice":"required"}`))
	if !req.Choice.Required {
		t.Error("required lost")
	}
	req, _ = ParseRequest([]byte(base + `,"tool_choice":{"type":"function","function":{"name":"get_weather"}}}`))
	if req.Choice.Name != "get_weather" {
		t.Error("named choice lost")
	}
}

func TestToolRoundTripIsReplayedIntoTheTranscript(t *testing.T) {
	req, err := ParseRequest([]byte(`{"model":"m","messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"11C rain"},
		{"role":"user","content":"thanks"}],` + weatherTools + `}`))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	for _, want := range []string{`Called get_weather (c1) with {"city":"Paris"}`, "Tool result for get_weather (c1):\n11C rain", "User: thanks"} {
		if !strings.Contains(req.Transcript, want) {
			t.Errorf("transcript missing %q\n---\n%s", want, req.Transcript)
		}
	}
	if strings.Contains(req.Transcript, "<tool_call>") {
		t.Errorf("a text call format invites the model to imitate it: %s", req.Transcript)
	}
}

func TestRenderCallKeepsMalformedArgumentsVisible(t *testing.T) {
	var tc toolCallIn
	tc.ID, tc.Function.Name, tc.Function.Arguments = "c1", "f", "{broken"
	if got := renderCall(tc); !strings.Contains(got, "{broken") {
		t.Errorf("renderCall = %q", got)
	}
}

func postChat(s *Server, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

var parisCall = []ToolCall{{ID: "call_abc", Name: "get_weather", Arguments: `{"city":"Paris"}`}}

func TestToolCallComesBackAsOpenAIToolCalls(t *testing.T) {
	s := &Server{Runner: fakeExec{res: Result{ToolCalls: parisCall}}, Token: "t", Models: []Model{{ID: "auto"}}}
	rec := postChat(s, `{"model":"auto","messages":[{"role":"user","content":"x"}],`+weatherTools+`}`)
	var got struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct{ Name, Arguments string }
				} `json:"tool_calls"`
			}
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Choices) != 1 {
		t.Fatalf("bad body %s (%v)", rec.Body, err)
	}
	c := got.Choices[0]
	if c.FinishReason != "tool_calls" || c.Message.Content != nil || len(c.Message.ToolCalls) != 1 {
		t.Fatalf("choice = %+v", c)
	}
	tc := c.Message.ToolCalls[0]
	if tc.Type != "function" || tc.Function.Name != "get_weather" || tc.Function.Arguments != `{"city":"Paris"}` || tc.ID != "call_abc" {
		t.Errorf("tool call = %+v", tc)
	}
}

func TestToolCallStreamsAsToolCallDeltas(t *testing.T) {
	s := &Server{Runner: fakeExec{res: Result{Text: "Let me check.", ToolCalls: parisCall}}, Token: "t", Models: []Model{{ID: "auto"}}}
	rec := postChat(s, `{"model":"auto","stream":true,"messages":[{"role":"user","content":"x"}],`+weatherTools+`}`)
	out := rec.Body.String()
	for _, want := range []string{`"content":"Let me check."`, `"tool_calls":[{`, `"index":0`, `"name":"get_weather"`, `"finish_reason":"tool_calls"`, "[DONE]"} {
		if !strings.Contains(out, want) {
			t.Errorf("stream missing %q\n%s", want, out)
		}
	}
}

// A tag in a model's plain text is just text: nothing parses replies.
func TestATagInTheAnswerIsJustText(t *testing.T) {
	const tagged = `<tool_call>{"name":"get_weather","arguments":{}}</tool_call>`
	s := &Server{Runner: fakeExec{res: Result{Text: tagged}}, Token: "t", Models: []Model{{ID: "auto"}}}
	rec := postChat(s, `{"model":"auto","messages":[{"role":"user","content":"x"}],`+weatherTools+`}`)
	if !strings.Contains(rec.Body.String(), `"finish_reason":"stop"`) || strings.Contains(rec.Body.String(), `"tool_calls"`) {
		t.Errorf("a text tag must not become a call: %s", rec.Body)
	}
}

// routeExec records which path a request took.
type routeExec struct {
	plain, tools *int
	turn         *ChannelTurn
	delta        *bool
}

func (e routeExec) Run(_ context.Context, _, _ string, _ func(string) error) (Result, error) {
	*e.plain++
	return Result{Text: "ok"}, nil
}

func (e routeExec) RunChannel(_ context.Context, turn ChannelTurn, delta func(string) error) (Result, error) {
	*e.tools++
	*e.turn = turn
	*e.delta = delta != nil
	return Result{Text: "ok"}, nil
}

func TestEveryTurnUsesTheChannel(t *testing.T) {
	var plain, tools int
	var turn ChannelTurn
	var delta bool
	s := &Server{Runner: routeExec{&plain, &tools, &turn, &delta}, Token: "t", Models: []Model{{ID: "auto"}}}
	postChat(s, `{"model":"auto","messages":[{"role":"system","content":"S"},{"role":"user","content":"x"}]}`)
	if plain != 0 || tools != 1 || len(turn.Tools) != 0 || turn.System != "S" || turn.Prompt != "User: x" {
		t.Fatalf("plain chat: -p = %d, channel = %d, turn = %+v", plain, tools, turn)
	}
	postChat(s, `{"model":"auto","tool_choice":"none","messages":[{"role":"user","content":"x"}],`+weatherTools+`}`)
	if plain != 0 || tools != 2 || len(turn.Tools) != 0 {
		t.Fatalf("tool_choice none: -p = %d, channel = %d, turn = %+v", plain, tools, turn)
	}
	postChat(s, `{"model":"auto","stream":true,"messages":[{"role":"user","content":"x"}],`+weatherTools+`}`)
	if plain != 0 || tools != 3 || len(turn.Tools) != 1 {
		t.Fatalf("tools: -p = %d, channel = %d, turn = %+v", plain, tools, turn)
	}
	if !delta {
		t.Error("a channel turn must stream text live, not buffer it")
	}
}

// refusingExec is a CLI whose channel gate fails.
type refusingExec struct {
	plain *int
	gotP  *string
}

func (e refusingExec) Run(_ context.Context, _, prompt string, _ func(string) error) (Result, error) {
	*e.plain++
	*e.gotP = prompt
	return Result{Text: "from -p"}, nil
}

func (e refusingExec) RunChannel(context.Context, ChannelTurn, func(string) error) (Result, error) {
	return Result{}, toolsRefused("protocol 4", nil)
}

func TestPlainChatFallsBackToPWhenTheChannelIsRefused(t *testing.T) {
	var plain int
	var prompt string
	s := &Server{Runner: refusingExec{&plain, &prompt}, Token: "t", Models: []Model{{ID: "auto"}}}
	for _, stream := range []string{"false", "true"} {
		rec := postChat(s, `{"model":"auto","stream":`+stream+`,"messages":[{"role":"system","content":"S"},{"role":"user","content":"x"}]}`)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "from -p") {
			t.Errorf("stream=%s: %d %s", stream, rec.Code, rec.Body)
		}
	}
	if plain != 2 || !strings.Contains(prompt, "S") || !strings.Contains(prompt, "User: x") {
		t.Errorf("-p ran %d times with %q: it needs the flattened system text", plain, prompt)
	}
	// With tools -p is no use: it cannot carry them, so the refusal stands.
	rec := postChat(s, `{"model":"auto","messages":[{"role":"user","content":"x"}],`+weatherTools+`}`)
	if rec.Code != http.StatusBadRequest || plain != 2 {
		t.Errorf("tools: %d %s, -p ran %d times", rec.Code, rec.Body, plain)
	}
}

// Only the gate's refusal falls back: a real failure happened at the CLI, and
// repeating the turn on -p would charge for it twice.
func TestPlainChatDoesNotFallBackOnOtherFailures(t *testing.T) {
	var plain int
	var prompt string
	exec := failingExec{refusingExec{&plain, &prompt}, &Failure{Code: "quota_exhausted", Msg: "no"}}
	s := &Server{Runner: exec, Token: "t", Models: []Model{{ID: "auto"}}}
	rec := postChat(s, `{"model":"auto","messages":[{"role":"user","content":"x"}]}`)
	if rec.Code != http.StatusTooManyRequests || plain != 0 {
		t.Errorf("%d %s, -p ran %d times", rec.Code, rec.Body, plain)
	}
}

type failingExec struct {
	refusingExec
	err error
}

func (e failingExec) RunChannel(context.Context, ChannelTurn, func(string) error) (Result, error) {
	return Result{}, e.err
}

// The router fails over on this exact shape (proxy.go refusesTools) without
// parking the hop, so the refusal costs the operator nothing.
func TestToolRefusalIsThe400TheRouterFailsOverOn(t *testing.T) {
	s := &Server{Runner: fakeExec{err: toolsRefused("protocol 4", nil)}, Token: "t", Models: []Model{{ID: "auto"}}}
	for _, stream := range []string{"false", "true"} {
		rec := postChat(s, `{"model":"auto","stream":`+stream+`,"messages":[{"role":"user","content":"x"}],`+weatherTools+`}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"chat_tools_unsupported"`) {
			t.Errorf("stream=%s: %d %s", stream, rec.Code, rec.Body)
		}
	}
}
