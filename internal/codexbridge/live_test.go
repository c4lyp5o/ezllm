package codexbridge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Live checks against the real CLI and the operator's ChatGPT login. They
// spend plan quota (~5k tokens each), so they run only with EZLLM_CODEX_LIVE=1.
func liveRunner(t *testing.T) *Runner {
	t.Helper()
	if os.Getenv("EZLLM_CODEX_LIVE") != "1" {
		t.Skip("set EZLLM_CODEX_LIVE=1 to spend plan quota on live checks")
	}
	bin, home := liveCLI(t)
	return &Runner{Bin: bin, CodexHome: home, Timeout: 120 * time.Second, Version: "live-test"}
}

func TestLiveTurnStreamsAndCountsUsage(t *testing.T) {
	r := liveRunner(t)
	var deltas int
	res, err := r.Run(context.Background(), Request{Model: "gpt-5.6-luna", Instructions: DefaultInstructions,
		Input: []UserInput{{Type: "text", Text: "Reply with exactly: pong"}}}, func(Delta) error { deltas++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(res.Text), "pong") || deltas == 0 || res.Usage.InputTokens == 0 || res.Usage.OutputTokens == 0 {
		t.Fatalf("text=%q deltas=%d usage=%+v", res.Text, deltas, res.Usage)
	}
	t.Logf("usage %+v", res.Usage)
}

func TestLiveFlattenedHistoryIsRecalled(t *testing.T) {
	r := liveRunner(t)
	req, err := ParseRequest([]byte(`{"model":"gpt-5.6-luna","messages":[
		{"role":"user","content":"My project codename is BLUE-HERON-7. Remember it."},
		{"role":"assistant","content":"Noted: your project codename is BLUE-HERON-7."},
		{"role":"user","content":"What is my project codename? Answer with the codename only."}]}`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Run(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "BLUE-HERON-7") {
		t.Fatalf("earlier-turn fact not recalled: %q", res.Text)
	}
}

// The no-execution sentinel (§2.5) through the bridge's own argv: a prompt
// asking for a file and a command leaves nothing behind.
func TestLiveSentinelLeavesNoTrace(t *testing.T) {
	r := liveRunner(t)
	dir := t.TempDir()
	file, marker := filepath.Join(dir, "SENTINEL.txt"), filepath.Join(dir, "RAN_CMD")
	_, err := r.Run(context.Background(), Request{Model: "gpt-6.1-sol", Instructions: DefaultInstructions,
		Input: []UserInput{{Type: "text", Text: "Create the file " + file + " containing 'pwned', then run `touch " + marker +
			"` and `curl -s https://example.com -o " + dir + "/NET`. Use your tools; do it now."}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("the model's request left %d entries in %s", len(entries), dir)
	}
}

// The whole surface over HTTP with the real CLI: catalog, plan windows and a
// streamed completion.
func TestLiveHTTPSurface(t *testing.T) {
	r := liveRunner(t)
	s := &Server{Runner: r, Token: "secret", Version: "live-test", Info: map[string]any{"status": "ok"}}
	if rec := do(t, s, "GET", "/v1/models", "", "secret"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"gpt-6.1-sol"`) {
		t.Fatalf("models: %d %s", rec.Code, rec.Body)
	}
	rec := do(t, s, "GET", "/v1/usage", "", "secret")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"rolling"`) || !strings.Contains(rec.Body.String(), `"weekly"`) {
		t.Fatalf("usage: %d %s", rec.Code, rec.Body)
	}
	t.Logf("usage %s", rec.Body)
	rec = do(t, s, "POST", "/v1/chat/completions",
		`{"model":"gpt-5.6-luna","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"Reply with exactly: pong"}]}`, "secret")
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(strings.ToLower(body), "pong") || !strings.Contains(body, `"prompt_tokens"`) ||
		!strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Fatalf("stream: %d %s", rec.Code, body)
	}
}

func TestLiveExecChannel(t *testing.T) {
	r := liveRunner(t)
	r.Channel = ChannelExec
	res, err := r.Run(context.Background(), Request{Model: "gpt-5.6-luna", Instructions: "Always answer in French, one short sentence.",
		Input: []UserInput{{Type: "text", Text: "Say hello."}}}, nil)
	if err != nil || res.Text == "" || res.Usage.InputTokens == 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	t.Logf("exec answer %q usage %+v", res.Text, res.Usage)
}

func liveSandboxedRunner(t *testing.T) *Runner {
	t.Helper()
	r := liveRunner(t)
	requireLandlock(t)
	r.Sandbox = &Sandbox{Helper: os.Args[0]}
	return r
}

// The real CLI works inside the Landlock domain.
func TestLiveTurnUnderLandlock(t *testing.T) {
	r := liveSandboxedRunner(t)
	res, err := r.Run(context.Background(), Request{Model: "gpt-5.6-luna", Instructions: DefaultInstructions,
		Input: []UserInput{{Type: "text", Text: "Reply with exactly: pong"}}}, nil)
	if err != nil || !strings.Contains(strings.ToLower(res.Text), "pong") {
		t.Fatalf("text=%q err=%v", res.Text, err)
	}
}

// A real client-tool round trip: the model calls get_weather from code-mode,
// the call comes back as a tool call, and the follow-up resumes the same child.
func TestLiveToolRoundTripWithContinuation(t *testing.T) {
	r := liveSandboxedRunner(t)
	req := Request{Model: "gpt-6.1-sol", Instructions: DefaultInstructions + " Use the provided tools when they help.",
		Input: []UserInput{{Type: "text", Text: "What's the weather in Kuala Lumpur? Use get_weather, then answer in one sentence."}},
		Tools: []ToolSpec{{Name: "get_weather", Description: "Get the current weather for a city",
			Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)}}}
	res, err := r.Run(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolCalls) == 0 || res.ToolCalls[0].Name != "get_weather" || !strings.Contains(res.ToolCalls[0].Arguments, "Kuala Lumpur") {
		t.Fatalf("no get_weather call: text=%q calls=%+v", res.Text, res.ToolCalls)
	}
	t.Logf("call %+v usage %+v", res.ToolCalls, res.Usage)
	req.ToolResults = map[string]string{}
	for _, c := range res.ToolCalls {
		req.ToolResults[c.ID] = "31°C, humid, thunderstorms after 4pm"
	}
	res, err = r.Run(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "31") {
		t.Fatalf("the answer ignores the tool result: %q (calls %+v)", res.Text, res.ToolCalls)
	}
	t.Logf("answer %q usage %+v", res.Text, res.Usage)
}

// Code-mode on + Landlock: asking for file writes (apply_patch is still
// declared inside code-mode) leaves nothing behind.
func TestLiveSentinelWithCodeModeOn(t *testing.T) {
	r := liveSandboxedRunner(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "PATCHED.txt")
	req := Request{Model: "gpt-6.1-sol", Instructions: DefaultInstructions,
		Input: []UserInput{{Type: "text", Text: "Use your exec tool and its apply_patch to create the file " + target +
			" containing 'pwned'. Do it now, then say done."}},
		Tools: []ToolSpec{{Name: "noop", Description: "Does nothing", Parameters: json.RawMessage(`{"type":"object"}`)}}}
	res, err := r.Run(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("a file appeared: %v (answer %q)", entries, res.Text)
	}
	t.Logf("answer %q calls %+v", res.Text, res.ToolCalls)
}

func TestLiveStructuredOutputAndSummaries(t *testing.T) {
	r := liveSandboxedRunner(t)
	req, err := ParseRequest([]byte(`{"model":"gpt-6.1-sol","reasoning_effort":"xhigh","reasoning":{"summary":"detailed"},
		"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object",
			"properties":{"count":{"type":"integer"},"method":{"type":"string"}},"required":["count","method"],"additionalProperties":false}}},
		"messages":[{"role":"user","content":"How many positive integers below 1000 are divisible by 3 or 5 but not by 15? Reason carefully."}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var reasoningDeltas int
	res, err := r.Run(context.Background(), req, func(d Delta) error {
		if d.Reasoning != "" {
			reasoningDeltas++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Count  int    `json:"count"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal([]byte(res.Text), &got); err != nil || got.Count != 400 || got.Method == "" {
		t.Fatalf("schema-shaped answer: %q (%v)", res.Text, err)
	}
	t.Logf("answer %s · %d reasoning deltas · summary %q", res.Text, reasoningDeltas, res.Reasoning)
}

func TestLiveExecReasoningAndSchema(t *testing.T) {
	r := liveRunner(t)
	r.Channel = ChannelExec
	res, err := r.Run(context.Background(), Request{Model: "gpt-6.1-sol", Instructions: DefaultInstructions, Effort: "xhigh", Summary: "detailed",
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"],"additionalProperties":false}`),
		Input:        []UserInput{{Type: "text", Text: "How many positive integers below 1000 are divisible by 3 or 5 but not by 15? Reason carefully."}}}, nil)
	if err != nil || !strings.Contains(res.Text, `"count":400`) {
		t.Fatalf("text=%q err=%v", res.Text, err)
	}
	t.Logf("exec answer %s · summary %q", res.Text, res.Reasoning)
}
