package agybridge

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Measured on 1.4.7 against an account that had not passed Google's
// eligibility check (stdout, one line; the sign-in continuation is shortened
// but keeps its shape).
const measuredIneligible = `{"event":"result","result":{"conversation_id":"","status":"ERROR","response":"","error":"Eligibility check failed: Your current account is not eligible for Antigravity. Verify your account to continue.\n\nPlease verify your account in your browser to continue:\nhttps://accounts.google.com/v3/signin/continue?plt=AKgnsbuSECRET&flowName=GlifWebSignIn","duration_seconds":2.1,"num_turns":0,"usage":{"input_tokens":0,"output_tokens":0,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":0}}}`

// Measured `agy models` output (1.4.7, personal plan).
const measuredModels = "Fetching available models...\ngemini-3.8-flash-high\tGemini 3.8 Flash (High)\ngemini-3.8-flash-medium\tGemini 3.8 Flash (Medium)\ngemini-3.8-flash-low\tGemini 3.8 Flash (Low)\nclaude-sonnet-4-6\tClaude Sonnet 4.6 (Thinking)\nclaude-opus-4-6-thinking\tClaude Opus 4.6 (Thinking)\ngpt-oss-120b-medium\tGPT-OSS 120B (Medium)\n"

const okStream = `{"event":"init","conversation_id":"c1","init":{"cwd":"/w","tools":["run_command","write_to_file"],"permission_mode":"request-review"}}
{"event":"step_update","conversation_id":"c1","step_index":0,"state":"DONE","step_type":"user_input"}
{"event":"step_update","conversation_id":"c1","step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":"po"}
{"event":"step_update","conversation_id":"c1","step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":"ng"}
{"event":"step_update","conversation_id":"c1","step_index":1,"state":"DONE","step_type":"agent_response"}
{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"pong","duration_seconds":3.1,"num_turns":1,"usage":{"input_tokens":120,"output_tokens":30,"thinking_tokens":20,"cache_read_tokens":80,"total_tokens":150}}}`

// fakeAgy writes a shell script that records its argv, env and stdin, then
// prints stdout. exit is its exit code.
func fakeAgy(t *testing.T, stdout string, exit int) (bin, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake CLI is a shell script")
	}
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "out"), []byte(stdout+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + dir + "/argv\nenv > " + dir + "/env\npwd > " + dir + "/cwd\ncat > " + dir + "/stdin\ncat " + dir + "/out\necho 'some stderr' >&2\nexit " + itoa(exit) + "\n"
	bin = filepath.Join(dir, "agy")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRunStreamsDeltasAndUsage(t *testing.T) {
	bin, dir := fakeAgy(t, okStream, 0)
	t.Setenv("EZLLM_MASTER_KEY", "must-not-leak")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1000/bus")
	t.Setenv("GEMINI_API_KEY", "must-not-leak")
	r := &Runner{Bin: bin, Home: "/agy-sub", Timeout: 10 * time.Second}
	var deltas []string
	res, err := r.Run(context.Background(), Request{Model: "gemini-3.8-flash-low", Effort: "low", Prompt: "say pong"},
		func(s string) error { deltas = append(deltas, s); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(deltas, "") != "pong" || len(deltas) != 2 || res.Text != "pong" {
		t.Fatalf("deltas %q text %q", deltas, res.Text)
	}
	if res.Usage.InputTokens != 120 || res.Usage.OutputTokens != 30 || res.Usage.ThinkingTokens != 20 || res.Usage.CacheRead != 80 {
		t.Fatalf("usage %+v", res.Usage)
	}

	argv := read(t, dir+"/argv")
	for _, want := range []string{"--print=\n", "stream-json", "--disable-slash-commands", "--model\ngemini-3.8-flash-low", "--effort\nlow"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("argv lacks %q:\n%s", want, argv)
		}
	}
	if strings.Contains(argv, "dangerously") || strings.Contains(argv, "say pong") {
		t.Fatalf("argv skips permissions or carries the prompt:\n%s", argv)
	}
	var in struct {
		Event   string `json:"event"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(read(t, dir+"/stdin"))), &in); err != nil || in.Event != "user" || in.Message.Content != "say pong" {
		t.Fatalf("stdin message %+v (%v)", in, err)
	}
	env := read(t, dir+"/env")
	if !strings.Contains(env, "HOME=/agy-sub\n") {
		t.Fatalf("child HOME is not the login dir:\n%s", env)
	}
	for _, leak := range []string{"must-not-leak", "DBUS_SESSION_BUS_ADDRESS"} {
		if strings.Contains(env, leak) {
			t.Fatalf("child env carries %s", leak)
		}
	}
	cwd := strings.TrimSpace(read(t, dir+"/cwd"))
	if !strings.Contains(filepath.Base(cwd), "agy-bridge-") {
		t.Fatalf("child ran in %s, not a fresh workspace", cwd)
	}
	if _, err := os.Stat(cwd); !os.IsNotExist(err) {
		t.Fatalf("workspace %s survived the turn", cwd)
	}
}

func TestRunIneligibleIs403WithoutTheLink(t *testing.T) {
	bin, _ := fakeAgy(t, measuredIneligible, 1)
	r := &Runner{Bin: bin, Home: t.TempDir(), Timeout: 10 * time.Second}
	_, err := r.Run(context.Background(), Request{Model: "m", Prompt: "hi"}, nil)
	ne := asNative(err)
	if ne.Status != 403 || ne.Code != "account_ineligible" || !strings.Contains(ne.Message, "not eligible") {
		t.Fatalf("got %+v", ne)
	}
	if strings.Contains(ne.Message, "accounts.google.com") || strings.Contains(ne.Message, "SECRET") {
		t.Fatalf("the sign-in link reached the client: %s", ne.Message)
	}
}

func TestRunWithoutResultIs502WithStderr(t *testing.T) {
	bin, _ := fakeAgy(t, `not json at all`, 3)
	r := &Runner{Bin: bin, Home: t.TempDir(), Timeout: 10 * time.Second}
	_, err := r.Run(context.Background(), Request{Model: "m", Prompt: "hi"}, nil)
	ne := asNative(err)
	if ne.Status != 502 || !strings.Contains(ne.Message, "exited 3") || !strings.Contains(ne.Message, "some stderr") {
		t.Fatalf("got %+v", ne)
	}
}

func TestMapError(t *testing.T) {
	for msg, want := range map[string]int{
		"authentication required: run agy interactively":          401,
		"RESOURCE_EXHAUSTED: daily quota reached":                 429,
		"You have run out of credits":                             429,
		"unknown model \"x\"":                                     400,
		"Agent execution terminated due to error.":                502,
		"Eligibility check failed: account is not eligible today": 403,
	} {
		if got := mapError("ERROR", msg); got.Status != want {
			t.Errorf("%q → %d, want %d", msg, got.Status, want)
		}
	}
}

func TestParseRequestBuildsTranscript(t *testing.T) {
	req, err := ParseRequest([]byte(`{"model":"m","reasoning_effort":"minimal","messages":[
		{"role":"system","content":"Be terse."},
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"text","text":"hello"}]},
		{"role":"user","content":"again"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := "<instructions>\nBe terse.\n</instructions>\n\nEarlier turns of this conversation, oldest first:\n\n<turn role=\"user\">\nhi\n</turn>\n<turn role=\"assistant\">\nhello\n</turn>\n\nReply to the latest user message:\n\nagain"
	if req.Prompt != want || req.Effort != "low" {
		t.Fatalf("prompt %q effort %q", req.Prompt, req.Effort)
	}
	lone, _ := ParseRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"just this"}]}`))
	if lone.Prompt != "just this" {
		t.Fatalf("a lone message is reframed: %q", lone.Prompt)
	}
}

func TestParseRequestRefusals(t *testing.T) {
	for name, body := range map[string]string{
		"tools":     `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"f"}}]}`,
		"tool msg":  `{"model":"m","messages":[{"role":"user","content":"x"},{"role":"tool","content":"r","tool_call_id":"1"}]}`,
		"image":     `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:"}}]}]}`,
		"n":         `{"model":"m","n":2,"messages":[{"role":"user","content":"x"}]}`,
		"last asst": `{"model":"m","messages":[{"role":"user","content":"x"},{"role":"assistant","content":"y"}]}`,
		"no model":  `{"messages":[{"role":"user","content":"x"}]}`,
	} {
		if _, err := ParseRequest([]byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseModelsMeasured(t *testing.T) {
	ms := ParseModels(measuredModels)
	if len(ms) != 6 || ms[0].ID != "gemini-3.8-flash-high" || ms[3].DisplayName != "Claude Sonnet 4.6 (Thinking)" {
		t.Fatalf("models %+v", ms)
	}
}

type fakeExec struct {
	res   Result
	err   error
	calls int
}

func (f *fakeExec) Run(_ context.Context, req Request, delta func(string) error) (Result, error) {
	f.calls++
	if delta != nil && f.err == nil {
		for _, d := range []string{"po", "ng"} {
			if err := delta(d); err != nil {
				return Result{}, err
			}
		}
	}
	return f.res, f.err
}

func newTestServer(ex Executor) *httptest.Server {
	s := &Server{Runner: ex, Token: "tok", Version: "test", Models: ParseModels(measuredModels),
		Now: func() time.Time { return time.Unix(1700000000, 0) }}
	return httptest.NewServer(s.Handler())
}

func post(t *testing.T, url, token, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestServerChat(t *testing.T) {
	ex := &fakeExec{res: Result{Text: "pong", Usage: Usage{InputTokens: 120, OutputTokens: 30, ThinkingTokens: 20, CacheRead: 80}}}
	ts := newTestServer(ex)
	defer ts.Close()

	if resp := post(t, ts.URL, "wrong", `{}`); resp.StatusCode != 401 {
		t.Fatalf("wrong token = %d", resp.StatusCode)
	}
	resp := post(t, ts.URL, "tok", `{"model":"no-such","messages":[{"role":"user","content":"x"}]}`)
	if resp.StatusCode != 400 || ex.calls != 0 {
		t.Fatalf("unknown model = %d after %d spawns", resp.StatusCode, ex.calls)
	}

	resp = post(t, ts.URL, "tok", `{"model":"gemini-3.8-flash-low","messages":[{"role":"user","content":"x"}]}`)
	var out struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 || out.Choices[0].Message.Content != "pong" || out.Usage["prompt_tokens"] != 120.0 || out.Usage["completion_tokens"] != 30.0 {
		t.Fatalf("non-stream %d %+v", resp.StatusCode, out)
	}

	resp = post(t, ts.URL, "tok", `{"model":"gemini-3.8-flash-low","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"x"}]}`)
	var text strings.Builder
	var sawUsage, sawStop, sawDone bool
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := strings.TrimPrefix(sc.Text(), "data: ")
		if line == "[DONE]" {
			sawDone = true
			continue
		}
		var c struct {
			Choices []struct {
				Delta        struct{ Content string } `json:"delta"`
				FinishReason *string                  `json:"finish_reason"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if json.Unmarshal([]byte(line), &c) != nil {
			continue
		}
		for _, ch := range c.Choices {
			text.WriteString(ch.Delta.Content)
			if ch.FinishReason != nil && *ch.FinishReason == "stop" {
				sawStop = true
			}
		}
		if c.Usage != nil {
			sawUsage = c.Usage["completion_tokens_details"].(map[string]any)["reasoning_tokens"] == 20.0
		}
	}
	if text.String() != "pong" || !sawStop || !sawUsage || !sawDone {
		t.Fatalf("stream text %q stop %v usage %v done %v", text.String(), sawStop, sawUsage, sawDone)
	}
}

func TestServerErrorBeforeOutputKeepsStatus(t *testing.T) {
	ts := newTestServer(&fakeExec{err: mapError("ERROR", "Eligibility check failed: not eligible")})
	defer ts.Close()
	resp := post(t, ts.URL, "tok", `{"model":"gemini-3.8-flash-low","stream":true,"messages":[{"role":"user","content":"x"}]}`)
	if resp.StatusCode != 403 {
		t.Fatalf("stream failure before output = %d, want 403", resp.StatusCode)
	}
}

func TestRunRefusesALoosePermissionMode(t *testing.T) {
	bin, _ := fakeAgy(t, `{"event":"init","conversation_id":"c","init":{"cwd":"/w","tools":["run_command"],"permission_mode":"always-proceed"}}
{"event":"step_update","conversation_id":"c","step_type":"agent_response","text_delta":"leaked"}
{"event":"result","result":{"status":"SUCCESS","response":"leaked"}}`, 0)
	r := &Runner{Bin: bin, Home: t.TempDir(), Timeout: 10 * time.Second}
	var got string
	_, err := r.Run(context.Background(), Request{Model: "m", Prompt: "hi"}, func(s string) error { got += s; return nil })
	if ne := asNative(err); ne.Code != "lockdown" || got != "" {
		t.Fatalf("always-proceed turn = %+v, streamed %q", ne, got)
	}
}

func TestCheckSettings(t *testing.T) {
	write := func(body string) string {
		home := t.TempDir()
		dir := filepath.Join(home, ".gemini", "antigravity-cli")
		_ = os.MkdirAll(dir, 0o700)
		_ = os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o600)
		return home
	}
	// What the interactive first run wrote on the host (1.4.7).
	if err := CheckSettings(write(`{"trustedWorkspaces":["/home/u","/home/u/project"]}`)); err != nil {
		t.Fatalf("first-run settings refused: %v", err)
	}
	if err := CheckSettings(t.TempDir()); err != nil {
		t.Fatalf("no settings refused: %v", err)
	}
	for _, body := range []string{
		`{"toolPermission":"always-proceed"}`,
		`{"toolPermission":"proceed-in-sandbox"}`,
		`{"allowNonWorkspaceAccess":true}`,
		`{"agentMode":"auto"}`,
		`{"permissions":{"allow":["command(git)"]}}`,
	} {
		if err := CheckSettings(write(body)); err == nil {
			t.Errorf("%s accepted", body)
		}
	}
}
