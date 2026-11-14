package copilotbridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCLI is the CLI's side of the channel. Its replies and event sequences are
// the shapes measured against CLI 1.0.105 (2026-11-05).
type fakeCLI struct {
	t        *testing.T
	protocol int
	// createErr makes session.create fail with an RPC error.
	createErr string
	sendErr   string
	// events follow session.send, oldest first. Aborted follows session.abort.
	events, aborted []string
	// ask, if set, is a server request sent before the first event.
	ask string

	mu      sync.Mutex
	calls   []string
	params  map[string]json.RawMessage
	replied bool // the client answered ask
}

func (f *fakeCLI) called(m string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == m {
			return true
		}
	}
	return false
}

func (f *fakeCLI) serve(in *bufio.Reader, out io.Writer) {
	// Buffered like a real OS pipe: the client may answer a server request while
	// more events are on their way to it.
	outbox := make(chan string, 64)
	defer close(outbox)
	go func() {
		for v := range outbox {
			fmt.Fprintf(out, "Content-Length: %d\r\n\r\n%s", len(v), v)
		}
	}()
	send := func(v string) { outbox <- v }
	event := func(e string) {
		send(`{"jsonrpc":"2.0","method":"session.event","params":{"sessionId":"s1","event":` + e + `}}`)
	}
	for {
		var size int
		for {
			line, err := in.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				break
			}
			size, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:")))
		}
		body := make([]byte, size)
		if _, err := io.ReadFull(in, body); err != nil {
			return
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(body, &msg) != nil {
			f.t.Errorf("client sent bad JSON: %s", body)
			return
		}
		if msg.Method == "" {
			f.mu.Lock()
			f.replied = len(msg.Error) > 0
			f.mu.Unlock()
			continue
		}
		f.mu.Lock()
		f.calls = append(f.calls, msg.Method)
		if f.params == nil {
			f.params = map[string]json.RawMessage{}
		}
		f.params[msg.Method] = msg.Params
		f.mu.Unlock()
		reply := func(r string) { send(`{"jsonrpc":"2.0","id":` + string(msg.ID) + `,"result":` + r + `}`) }
		switch msg.Method {
		case "models.list":
			reply(visionCatalogFixture)
		case "ping":
			reply(fmt.Sprintf(`{"message":"pong","protocolVersion":%d}`, f.protocol))
		case "session.create":
			if f.createErr != "" {
				send(`{"jsonrpc":"2.0","id":` + string(msg.ID) + `,"error":{"code":-32603,"message":` + strconv.Quote(f.createErr) + `}}`)
				continue
			}
			reply(`{"sessionId":"s1"}`)
		case "session.send":
			if f.sendErr != "" {
				send(`{"jsonrpc":"2.0","id":` + string(msg.ID) + `,"error":{"code":-32603,"message":` + strconv.Quote(f.sendErr) + `}}`)
				continue
			}
			reply(`{"messageId":"m1"}`)
			if f.ask != "" {
				send(`{"jsonrpc":"2.0","id":"srv-1","method":"` + f.ask + `","params":{}}`)
			}
			for _, e := range f.events {
				event(e)
			}
		case "session.abort":
			reply(`{"success":true}`)
			for _, e := range f.aborted {
				event(e)
			}
		default:
			reply(`{"success":true}`)
		}
	}
}

// drive runs one turn against the fake, on real pipes.
func (f *fakeCLI) drive(t *testing.T, turn ChannelTurn, delta func(string) error) (Result, error) {
	t.Helper()
	f.t = t
	if f.protocol == 0 {
		f.protocol = channelProtocolVersion
	}
	cr, sw := io.Pipe() // client reads what the server writes
	sr, cw := io.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); f.serve(bufio.NewReader(sr), sw) }()
	t.Cleanup(func() { cw.Close(); sw.Close(); <-done })
	c := &rpcConn{w: cw, r: bufio.NewReader(cr), closeRead: func() { cr.Close() }}
	var res Result
	var err error
	within(t, 10*time.Second, func() { res, err = driveTurn(c, turn, delta, nil) })
	return res, err
}

const (
	evBatch = `{"type":"assistant.message","data":{"content":"","model":"gpt-6-luna","toolRequests":[` +
		`{"toolCallId":"call_a","name":"get_weather","arguments":{"city":"Paris"}},` +
		`{"toolCallId":"call_b","name":"get_time","arguments":{"city":"Paris"}}]}}`
	// The numbers are written as the channel writes them: 1.0, not 1.
	evUsage = `{"type":"session.usage_checkpoint","data":{"totalNanoAiu":3780000,"totalPremiumRequests":1,"accountingSnapshot":{` +
		`"totalPremiumRequestCost":1.0,"totalUserRequests":1,"totalNanoAiu":3780000.0,"aiCreditsStatus":"complete",` +
		`"tokenDetails":{"input":{"tokenCount":10},"cache_read":{"tokenCount":0},"cache_write":{"tokenCount":90},"output":{"tokenCount":5}}}}}`
	evIdle = `{"type":"session.idle","data":{}}`
)

func channelTurn() ChannelTurn {
	var a, b toolDef
	a.Type, a.Function.Name, a.Function.Parameters = "function", "get_weather", json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)
	b.Type, b.Function.Name = "function", "get_time"
	return ChannelTurn{Model: "gpt-6-luna", System: "Be brief.", Prompt: "User: weather?", Tools: []toolDef{a, b}}
}

func TestChannelTurnReturnsTheWholeBatchThenAbortsAndCollectsUsage(t *testing.T) {
	f := &fakeCLI{events: []string{evBatch}, aborted: []string{evUsage, evIdle}}
	res, err := f.drive(t, channelTurn(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolCalls) != 2 ||
		res.ToolCalls[0] != (ToolCall{ID: "call_a", Name: "get_weather", Arguments: `{"city":"Paris"}`}) ||
		res.ToolCalls[1] != (ToolCall{ID: "call_b", Name: "get_time", Arguments: `{"city":"Paris"}`}) {
		t.Fatalf("ToolCalls = %+v", res.ToolCalls)
	}
	if !f.called("session.abort") {
		t.Error("the turn must be aborted: the model would otherwise wait on a call nobody answers")
	}
	if !f.called("session.delete") {
		t.Error("the session must be deleted, or COPILOT_HOME grows one directory per turn")
	}
	if res.Usage.PromptTokens != 100 || res.Usage.CompletionTokens != 5 || res.Usage.PremiumRequests != 1 || res.Usage.NanoAIU != 3780000 {
		t.Errorf("usage arrives after the abort and must still be booked: %+v", res.Usage)
	}
	if res.Usage.Model != "gpt-6-luna" || res.SessionID != "s1" {
		t.Errorf("model = %q, session = %q", res.Usage.Model, res.SessionID)
	}
}

func TestChannelTurnWithoutACallStreamsTheAnswer(t *testing.T) {
	f := &fakeCLI{events: []string{
		`{"type":"assistant.message_delta","data":{"deltaContent":"Hel"}}`,
		`{"type":"assistant.message_delta","data":{"deltaContent":"lo"}}`,
		`{"type":"assistant.message","data":{"content":"Hello","model":"gpt-6-luna","toolRequests":[]}}`,
		evUsage, evIdle}}
	var got []string
	res, err := f.drive(t, channelTurn(), func(s string) error { got = append(got, s); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Hello" || strings.Join(got, "|") != "Hel|lo" || len(res.ToolCalls) != 0 {
		t.Errorf("text = %q, deltas = %v, calls = %+v", res.Text, got, res.ToolCalls)
	}
	if f.called("session.abort") {
		t.Error("nothing to abort on a turn that finished")
	}
}

func TestChannelTurnKeepsGoingPastAnUndeclaredTool(t *testing.T) {
	f := &fakeCLI{events: []string{
		`{"type":"assistant.message","data":{"content":"","toolRequests":[{"toolCallId":"x","name":"invented","arguments":{}}]}}`,
		`{"type":"assistant.message","data":{"content":"done","toolRequests":[]}}`,
		evIdle}}
	res, err := f.drive(t, channelTurn(), nil)
	if err != nil || res.Text != "done" || len(res.ToolCalls) != 0 {
		t.Errorf("res = %+v, err = %v; only a declared tool may reach the caller", res, err)
	}
}

func TestChannelTurnKeepsTextBesideACall(t *testing.T) {
	f := &fakeCLI{events: []string{`{"type":"assistant.message","data":{"content":"Checking.","toolRequests":[{"toolCallId":"a","name":"get_time","arguments":null}]}}`},
		aborted: []string{evIdle}}
	res, err := f.drive(t, channelTurn(), nil)
	if err != nil || res.Text != "Checking." || len(res.ToolCalls) != 1 || res.ToolCalls[0].Arguments != "{}" {
		t.Errorf("res = %+v, err = %v", res, err)
	}
}

func TestSessionConfigShowsOnlyTheCallersTools(t *testing.T) {
	cfg := sessionConfig(channelTurn())
	if got := fmt.Sprint(cfg["availableTools"]); got != "[get_weather get_time]" {
		t.Errorf("availableTools = %s: it is what hides the CLI's own shell and file tools", got)
	}
	if cfg["requestPermission"] != false || cfg["requestUserInput"] != false {
		t.Error("nobody can answer a prompt on this path")
	}
	sys := cfg["systemMessage"].(map[string]any)
	if sys["mode"] != "replace" || !strings.HasPrefix(sys["content"].(string), "Be brief.\n\n") || !strings.Contains(sys["content"].(string), toolNote) {
		t.Errorf("systemMessage = %+v", sys)
	}
	b, _ := json.Marshal(cfg["tools"])
	var tools []map[string]any
	if err := json.Unmarshal(b, &tools); err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools {
		// Without this a caller's tool named `bash` could reach the built-in.
		if tl["overridesBuiltInTool"] != true || tl["skipPermission"] != true {
			t.Errorf("tool = %+v", tl)
		}
	}
	if tools[1]["name"] != "get_time" || fmt.Sprint(tools[1]["parameters"]) != "map[properties:map[] type:object]" {
		t.Errorf("a tool without parameters needs an empty schema: %s", b)
	}
}

// A plain turn must send an EMPTY allowlist, not none. Measured on CLI 1.0.105:
// [] shows the model 0 tools, and a missing key shows 18, including bash.
func TestPlainTurnSendsAnEmptyAllowlist(t *testing.T) {
	cfg := sessionConfig(ChannelTurn{Prompt: "User: hi"})
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["availableTools"]) != "[]" {
		t.Errorf("availableTools = %s, want []: anything else exposes the CLI's own tools", got["availableTools"])
	}
	sys := cfg["systemMessage"].(map[string]any)["content"].(string)
	if sys != plainNote {
		t.Errorf("system = %q", sys)
	}
	if strings.Contains(sys, toolNote) {
		t.Error("a plain turn must not be told it has functions")
	}
	cfg = sessionConfig(ChannelTurn{System: "Be brief.", Prompt: "User: hi"})
	if got := cfg["systemMessage"].(map[string]any)["content"].(string); !strings.HasPrefix(got, "Be brief.\n\n") {
		t.Errorf("system = %q", got)
	}
}

func TestSessionConfigToolChoice(t *testing.T) {
	turn := channelTurn()
	turn.Choice = toolChoice{Name: "get_time"}
	cfg := sessionConfig(turn)
	if got := fmt.Sprint(cfg["availableTools"]); got != "[get_time]" {
		t.Errorf("a named function must be the only visible one: %s", got)
	}
	if !strings.Contains(cfg["systemMessage"].(map[string]any)["content"].(string), "must call the function get_time") {
		t.Error("named choice not stated")
	}
	turn.Choice = toolChoice{Required: true}
	if !strings.Contains(sessionConfig(turn)["systemMessage"].(map[string]any)["content"].(string), "at least one function") {
		t.Error("required not stated")
	}
	turn = channelTurn()
	turn.Model = "auto"
	if _, ok := sessionConfig(turn)["model"]; ok {
		t.Error("auto is the CLI's default and is not named, as on -p")
	}
}

func TestChannelTurnSendsTheTranscriptAsThePrompt(t *testing.T) {
	f := &fakeCLI{events: []string{`{"type":"assistant.message","data":{"content":"x"}}`, evIdle}}
	if _, err := f.drive(t, channelTurn(), nil); err != nil {
		t.Fatal(err)
	}
	var send struct{ SessionID, Prompt string }
	if err := json.Unmarshal(f.params["session.send"], &send); err != nil || send.Prompt != "User: weather?" || send.SessionID != "s1" {
		t.Errorf("session.send = %s (%v)", f.params["session.send"], err)
	}
}

func failureCode(err error) string {
	var f *Failure
	if errors.As(err, &f) {
		return f.Code
	}
	return ""
}

// The gate: another protocol version is refused before a session exists, so the
// failover costs nothing.
func TestWrongProtocolVersionIsRefusedBeforeAnySession(t *testing.T) {
	f := &fakeCLI{protocol: channelProtocolVersion + 1}
	_, err := f.drive(t, channelTurn(), nil)
	if failureCode(err) != "chat_tools_unsupported" {
		t.Fatalf("err = %v", err)
	}
	if f.called("session.create") || f.called("session.send") {
		t.Error("nothing billable may happen behind a failed gate")
	}
}

func TestCreateRefusalsAreTypedAndNeverSend(t *testing.T) {
	for _, tc := range []struct{ msg, code string }{
		// Both measured on CLI 1.0.105.
		{`Request session.create failed with message: Model "claude-sonnet-5.5" is not available.`, "model_not_in_plan"},
		{`Request session.create failed with message: session construction failed: GenericFailure, externalToolDefinitions: tool name "a.b" contains invalid characters.`, "chat_tools_unsupported"},
		{`Request session.create failed with message: boom`, "cli_failed"},
	} {
		f := &fakeCLI{createErr: tc.msg}
		_, err := f.drive(t, channelTurn(), nil)
		if failureCode(err) != tc.code {
			t.Errorf("%s: err = %v, want %s", tc.msg, err, tc.code)
		}
		if f.called("session.send") {
			t.Errorf("%s: sent after a refused create", tc.msg)
		}
	}
}

func TestSessionErrorsAreTyped(t *testing.T) {
	for _, tc := range []struct{ data, code string }{
		// Measured with an empty COPILOT_HOME on CLI 1.0.105.
		{`{"errorType":"query","message":"Execution failed: InvalidArg, No GitHub OAuth token or Copilot HMAC key provided"}`, "not_logged_in"},
		{`{"errorType":"rate_limit","message":"slow down"}`, "quota_exhausted"},
		{`{"errorType":"query","message":"boom"}`, "cli_failed"},
	} {
		f := &fakeCLI{events: []string{`{"type":"session.error","data":` + tc.data + `}`}}
		_, err := f.drive(t, channelTurn(), nil)
		if failureCode(err) != tc.code {
			t.Errorf("%s: err = %v, want %s", tc.data, err, tc.code)
		}
		if !f.called("session.delete") {
			t.Errorf("%s: failed session left behind", tc.data)
		}
	}
}

func TestChannelTurnAnswersAServerRequestInsteadOfHanging(t *testing.T) {
	f := &fakeCLI{ask: "permission.request", events: []string{`{"type":"assistant.message","data":{"content":"ok"}}`, evIdle}}
	res, err := f.drive(t, channelTurn(), nil)
	if err != nil || res.Text != "ok" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	time.Sleep(20 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.replied {
		t.Error("server request went unanswered")
	}
}

func TestDeltaFailureEndsTheTurn(t *testing.T) {
	f := &fakeCLI{events: []string{`{"type":"assistant.message_delta","data":{"deltaContent":"x"}}`}}
	gone := errors.New("client gone")
	if _, err := f.drive(t, channelTurn(), func(string) error { return gone }); !errors.Is(err, gone) {
		t.Fatalf("err = %v", err)
	}
}

func TestRPCReadRejectsAnOversizedFrame(t *testing.T) {
	c := &rpcConn{r: bufio.NewReader(strings.NewReader(fmt.Sprintf("Content-Length: %d\r\n\r\n", maxRPCMessage+1)))}
	if _, err := c.read(); err == nil || !strings.Contains(err.Error(), "bad frame length") {
		t.Fatalf("err = %v", err)
	}
}

// ── the process around the channel ──────────────────────────────────────────

// frames is shell that prints LSP-framed responses for the calls a turn makes,
// in order: ping(1), create(2), send(3), then a final answer and idle.
func frames(protocol int) string {
	var b strings.Builder
	emit := func(s string) { fmt.Fprintf(&b, "printf 'Content-Length: %d\\r\\n\\r\\n%%s' '%s'\n", len(s), s) }
	emit(fmt.Sprintf(`{"id":1,"result":{"protocolVersion":%d}}`, protocol))
	emit(`{"id":2,"result":{"sessionId":"s1"}}`)
	emit(`{"id":3,"result":{"messageId":"m1"}}`)
	emit(`{"method":"session.event","params":{"event":{"type":"assistant.message","data":{"content":"hi"}}}}`)
	emit(`{"method":"session.event","params":{"event":{"type":"session.idle","data":{}}}}`)
	return b.String()
}

func TestRunChannelSpawnsTheHeadlessChannelWithAFilteredEnv(t *testing.T) {
	t.Setenv("EZLLM_ADMIN_TOKEN", "secret")
	dir := t.TempDir()
	argv, env := filepath.Join(dir, "argv"), filepath.Join(dir, "env")
	bin := stubCLI(t, `echo "$*" > `+argv+`
env > `+env+`
`+frames(channelProtocolVersion)+`cat >/dev/null`)
	var lines []string
	r := &Runner{Bin: bin, Home: dir, Timeout: 10 * time.Second, Logf: func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }}
	res, err := r.RunChannel(context.Background(), channelTurn(), nil)
	if err != nil || res.Text != "hi" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	got, _ := os.ReadFile(argv)
	for _, want := range []string{"--headless", "--stdio", "--disable-builtin-mcps"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("argv = %q, missing %s", got, want)
		}
	}
	for _, never := range []string{"--allow-tool", "--allow-all", "--yolo"} {
		if strings.Contains(string(got), never) {
			t.Errorf("argv = %q must never carry %s", got, never)
		}
	}
	e, _ := os.ReadFile(env)
	if strings.Contains(string(e), "EZLLM_ADMIN_TOKEN") {
		t.Error("the channel child must get the same filtered env as -p")
	}
	if !strings.Contains(string(e), "COPILOT_HOME="+dir) {
		t.Error("COPILOT_HOME not passed")
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "turn ok session=s1") || !strings.Contains(lines[0], "tool_calls=0") {
		t.Errorf("log = %v", lines)
	}
	if strings.Contains(strings.Join(lines, ""), "weather") {
		t.Error("a prompt reached the log")
	}
	if st := r.Stats(); st["served"].(int64) != 1 || st["first_byte_ms_avg"] == nil {
		t.Errorf("stats = %+v", st)
	}
	stages := r.Stats()["stage_timings"].(map[string]any)
	for _, name := range []string{"queue", "spawn", "ping", "session.create", "session.send", "first_event", "model_wait", "session.delete"} {
		v, ok := stages[name].(map[string]any)
		if !ok || v["count"] != int64(1) {
			t.Errorf("missing stage %s: %v", name, stages)
		}
	}

}

func TestProbeChannelGate(t *testing.T) {
	ok := &Runner{Bin: stubCLI(t, frames(channelProtocolVersion)+"cat >/dev/null")}
	if err := ok.ProbeChannel(context.Background()); err != nil {
		t.Errorf("good version: %v", err)
	}
	wrong := &Runner{Bin: stubCLI(t, frames(channelProtocolVersion+1)+"cat >/dev/null")}
	if err := wrong.ProbeChannel(context.Background()); failureCode(err) != "chat_tools_unsupported" {
		t.Errorf("wrong version: %v", err)
	}
	// A CLI that drops --headless exits at once.
	gone := &Runner{Bin: stubCLI(t, `echo "unknown option --headless" >&2; exit 2`)}
	if err := gone.ProbeChannel(context.Background()); failureCode(err) != "chat_tools_unsupported" {
		t.Errorf("flag gone: %v", err)
	}
	missing := &Runner{Bin: filepath.Join(t.TempDir(), "nope")}
	if err := missing.ProbeChannel(context.Background()); failureCode(err) != "chat_tools_unsupported" {
		t.Errorf("no binary: %v", err)
	}
}

func TestRunChannelKillsAHungChildAtTimeout(t *testing.T) {
	r := &Runner{Bin: stubCLI(t, `exec sleep 60`), Timeout: 200 * time.Millisecond}
	within(t, 5*time.Second, func() {
		if _, err := r.RunChannel(context.Background(), channelTurn(), nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestRunChannelDeathMidTurnIsClassifiedFromStderr(t *testing.T) {
	var b strings.Builder
	// Consume each complete request before replying. Unsolicited replies followed
	// by exit can close stdin before ping is written, testing gate failure instead.
	b.WriteString(`read_request() {
  IFS= read -r header || exit 91
  size=$(printf '%s' "$header" | tr -cd '0-9')
  IFS= read -r blank || exit 92
  request=$(dd bs=1 count="$size" 2>/dev/null)
  case "$request" in
    *\"method\":\"$1\"*) ;;
    *) exit 93 ;;
  esac
}
`)

	emit := func(method, s string) {
		fmt.Fprintf(&b, "read_request %s\n", method)
		fmt.Fprintf(&b, "printf 'Content-Length: %d\\r\\n\\r\\n%%s' '%s'\n", len(s), s)
	}
	emit("ping", fmt.Sprintf(`{"id":1,"result":{"protocolVersion":%d}}`, channelProtocolVersion))
	emit("session.create", `{"id":2,"result":{"sessionId":"s1"}}`)
	emit("session.send", `{"id":3,"result":{"messageId":"m1"}}`)
	r := &Runner{Bin: stubCLI(t, b.String()+`echo "premium request limit reached" >&2; exit 1`), Timeout: 10 * time.Second}
	within(t, 8*time.Second, func() {
		if _, err := r.RunChannel(context.Background(), channelTurn(), nil); failureCode(err) != "quota_exhausted" {
			t.Errorf("err = %v", err)
		}
	})
}

func TestRunChannelSharesTheSlots(t *testing.T) {
	r := &Runner{Bin: stubCLI(t, `sleep 60`), Timeout: 300 * time.Millisecond, MaxConcurrent: 1, MaxQueue: 1}
	r.init()
	r.sem <- struct{}{} // the only slot is busy
	r.queued.Store(1)   // and the queue is full
	if _, err := r.RunChannel(context.Background(), channelTurn(), nil); failureCode(err) != "queue_full" {
		t.Errorf("err = %v", err)
	}
}

// Measured on CLI 1.0.105: the channel writes totalPremiumRequestCost as 1.0, an
// int field refused it, the event was dropped and live turns booked zero usage.
func TestUsageSurvivesFloatAndFractionalNumbers(t *testing.T) {
	for _, tc := range []struct {
		cost string
		want float64
	}{{"1.0", 1}, {"1", 1}, {"0.33", 0.33}} {
		var ev cliEvent
		raw := `{"type":"session.usage_checkpoint","data":{"totalNanoAiu":960000.0,"accountingSnapshot":{"totalPremiumRequestCost":` + tc.cost + `,` +
			`"tokenDetails":{"input":{"tokenCount":71},"cache_read":{"tokenCount":0},"cache_write":{"tokenCount":0},"output":{"tokenCount":5.0}}}}}`
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			t.Fatalf("cost %s lost the event: %v", tc.cost, err)
		}
		var u Usage
		ev.applyUsage(&u)
		if u.PremiumRequests != tc.want || u.PromptTokens != 71 || u.CompletionTokens != 5 || u.NanoAIU != 960000 {
			t.Errorf("cost %s: usage = %+v", tc.cost, u)
		}
	}
	// An odd value is zero, and the rest of the event still counts.
	var ev cliEvent
	if err := json.Unmarshal([]byte(`{"type":"x","data":{"totalNanoAiu":"n/a","accountingSnapshot":{"tokenDetails":{"input":{"tokenCount":7}}}}}`), &ev); err != nil {
		t.Fatalf("odd value lost the event: %v", err)
	}
	var u Usage
	ev.applyUsage(&u)
	if u.PromptTokens != 7 || u.NanoAIU != 0 {
		t.Errorf("usage = %+v", u)
	}
}

func TestSendRefusalCannotAuthorizeFallback(t *testing.T) {
	f := &fakeCLI{sendErr: "externalToolDefinitions: invalid tool"}
	_, err := f.drive(t, channelTurn(), nil)
	failure, ok := err.(*Failure)
	if !ok || failure.BeforeInference {
		t.Fatalf("send failure must not authorize a second generation: %v", err)
	}
	if !f.called("session.send") {
		t.Fatal("send path not exercised")
	}
}
