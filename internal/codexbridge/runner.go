package codexbridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// LockdownFeatures are the Codex features switched off for every child. With
// them off, the M0 sentinel ("create a file, touch a marker, curl a URL",
// default instructions) produced no model-driven execve under strace; with
// them on, Codex ran all three inside its own sandbox
// (docs/codex-subscription-design.md §2.5). in_app_updates keeps the pinned
// binary pinned.
var LockdownFeatures = []string{
	"shell_tool", "unified_exec", "unified_exec_tty", "shell_snapshot",
	"apps", "plugins", "remote_plugin", "multi_agent",
	"browser_use", "browser_use_external", "browser_use_full_cdp_access",
	"computer_use", "in_app_browser", "image_generation", "view_image",
	"sleep_tool", "hooks", "skill_search", "tool_suggest", "goals",
	"code_mode_host", "workspace_dependencies", "worktrees",
	"realtime_conversation", "skill_mcp_dependency_install",
	"tool_call_mcp_elicitation", "in_app_updates",
}

// SandboxMode names the Codex filesystem policy for one child. Default is
// read-only; writable is an explicit operator opt-in via EZLLM_CODEX_SANDBOX.
type SandboxMode string

const (
	// SandboxReadOnly is the default: the model cannot write or execute
	// on the host (proven by the §2.5 sentinel, not assumed from flags).
	SandboxReadOnly SandboxMode = "read-only"
	// SandboxWritable lets Codex write inside the request's own temp workdir
	// (Codex's workspace-write policy). Landlock still confines the child:
	// the writable set is unchanged (the temp dir + CODEX_HOME), everything
	// else stays read-only. Nothing outside the domain becomes writable.
	SandboxWritable SandboxMode = "workspace-write"
)

// SandboxEnvKey is the operator opt-in: EZLLM_CODEX_SANDBOX=writable.
// Any other value (including unset) means read-only.
const SandboxEnvKey = "EZLLM_CODEX_SANDBOX"

// SandboxModeFromEnv reads the operator opt-in. Fail-closed: unknown
// values return read-only, never writable.
func SandboxModeFromEnv(getenv func(string) string) SandboxMode {
	if getenv == nil {
		getenv = os.Getenv
	}
	if strings.EqualFold(strings.TrimSpace(getenv(SandboxEnvKey)), "writable") {
		return SandboxWritable
	}
	return SandboxReadOnly
}

// ApprovalFor keeps both modes noninteractive. The bridge has no approval UI
// and declines approval RPCs; on-request would advertise an unusable escalation
// path. The sandbox independently controls which operations are allowed.
func (m SandboxMode) ApprovalFor() string {
	return "never"
}

// sandboxMode returns the effective policy, fail-closed: anything that is
// not explicitly writable reads back as read-only.
func (r *Runner) sandboxMode() SandboxMode {
	if r != nil && r.SandboxMode == SandboxWritable {
		return SandboxWritable
	}
	return SandboxReadOnly
}

// LockdownArgs is the argv tail for a Codex child. codeMode leaves the
// code-mode host on: client tools are reachable only through its JavaScript
// isolate in 0.163.0 (§2.12), so it is on exactly for tool-bearing requests.
func LockdownArgs(codeMode bool) []string {
	args := make([]string, 0, 2*len(LockdownFeatures)+4)
	for _, f := range LockdownFeatures {
		if codeMode && f == "code_mode_host" {
			continue
		}
		args = append(args, "--disable", f)
	}
	return append(args, "-c", `approval_policy="never"`, "-c", "tools.web_search=false", "-c", `web_search="disabled"`)
}

// Runner spawns one app-server child per request (claude-bridge's shape): a
// private temp HOME and CWD, an allowlisted environment, its own process
// group, and a concurrency cap. A turn that stops on client tool calls parks
// its child until the client's follow-up request answers them (§4.6).
type Runner struct {
	Bin           string
	CodexHome     string
	Timeout       time.Duration
	MaxConcurrent int
	Version       string
	Logf          func(format string, args ...any)
	// Channel is ChannelAppServer (default) or ChannelExec, the fallback boot
	// selects when the app-server lacks a method this bridge needs.
	Channel string
	// Sandbox, when set, runs every child inside Landlock.
	Sandbox *Sandbox
	// SandboxMode is the Codex filesystem policy, from SandboxModeFromEnv.
	// Zero value = read-only (fail-closed).
	SandboxMode SandboxMode
	// ToolHold bounds how long a parked turn waits for its tool results
	// (default 10 minutes); MaxParked caps parked children (default 8, the
	// oldest is evicted). An evicted or expired turn's follow-up replays.
	ToolHold  time.Duration
	MaxParked int

	// ForkCache reuses empty configuration templates, never caller dialogue.
	ForkCache bool
	// NativeHistory uses the qualified raw-item API to retain conversation roles.
	NativeHistory bool
	cache         templateCache

	sem  chan struct{}
	once sync.Once

	pmu    sync.Mutex
	parked map[string]*parkedTurn
	shut   bool // Shutdown ran: close tool stops instead of parking them
}

// Usage is the turn's token envelope (tokenUsage.total of a one-turn,
// ephemeral thread). Cached is INCLUDED in Input (measured: 14,155 input with
// 12,288 cached) — the OpenAI convention.
type Usage struct {
	InputTokens     int64 `json:"inputTokens"`
	CachedTokens    int64 `json:"cachedInputTokens"`
	OutputTokens    int64 `json:"outputTokens"`
	ReasoningTokens int64 `json:"reasoningOutputTokens"`
}

func (u Usage) minus(o Usage) Usage {
	return Usage{InputTokens: u.InputTokens - o.InputTokens, CachedTokens: u.CachedTokens - o.CachedTokens,
		OutputTokens: u.OutputTokens - o.OutputTokens, ReasoningTokens: u.ReasoningTokens - o.ReasoningTokens}
}

// Result is what one turn — or one segment of it, up to tool calls — produced.
type Result struct {
	Text      string
	Reasoning string
	Usage     Usage
	ToolCalls []ToolCall
}

// Delta is one streamed piece: answer text, or reasoning-summary text (which
// reaches chat clients as reasoning_content).
type Delta struct {
	Content   string
	Reasoning string
}

func (r *Runner) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

func (r *Runner) slots() chan struct{} {
	r.once.Do(func() {
		n := r.MaxConcurrent
		if n <= 0 {
			n = 2
		}
		r.sem = make(chan struct{}, n)
	})
	return r.sem
}

func (r *Runner) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return 300 * time.Second
}

// child is one running app-server.
type child struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	conn     *rpcConn
	tmp      string
	cwd      string
	stderr   *tailBuffer
	readDone chan struct{}
	once     sync.Once
	lease    *templateLease
	keepWork bool
	unbind   func() bool // set by bind; stops the cancellation hook
}

// bind ties the child to the request using it: cancelling ctx closes the
// child. Killing the process group is what unblocks a stdin write stuck on a
// CLI that stopped reading, and frees the writer mutex queued behind it.
func (c *child) bind(ctx context.Context) {
	c.unbind = context.AfterFunc(ctx, c.close)
}

// release detaches the child from its request before it parks (a parked
// child outlives the request by design). False means cancellation already
// started closing it: the child must not be parked.
func (c *child) release() bool {
	if c.unbind == nil {
		return true
	}
	ok := c.unbind()
	c.unbind = nil
	return ok
}

type spawnOpts struct {
	tools        bool
	experimental bool
	workRoot     string
}

// spawn starts the child and completes the initialize handshake.
func (r *Runner) spawn(ctx context.Context, opts spawnOpts, onNotify func(string, json.RawMessage), onRequest RequestHandler) (*child, error) {
	tmp := opts.workRoot
	var err error
	if tmp == "" {
		tmp, err = os.MkdirTemp("", "codex-bridge-")
	}
	if err != nil {
		return nil, fmt.Errorf("codex-bridge: cannot create work dir: %w", err)
	}
	c := &child{keepWork: opts.workRoot != "", tmp: tmp, cwd: filepath.Join(tmp, "cwd"), stderr: &tailBuffer{}, readDone: make(chan struct{})}
	home, tmpdir := filepath.Join(tmp, "home"), filepath.Join(tmp, "tmp")
	for _, d := range []string{c.cwd, home, tmpdir} {
		if err := os.Mkdir(d, 0o700); err != nil {
			os.RemoveAll(tmp)
			return nil, fmt.Errorf("codex-bridge: cannot create work dir: %w", err)
		}
	}

	bin, args := r.Bin, append([]string{"app-server", "--stdio"}, LockdownArgs(opts.tools)...)
	env := childEnv(home, r.CodexHome)
	if r.Sandbox != nil {
		bin, args = r.Sandbox.wrap(bin, args, tmp, r.CodexHome)
	}
	if r.Sandbox != nil || opts.workRoot != "" {
		env = append(env, "TMPDIR="+tmpdir) // private temp path, stable for template forks
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = c.cwd
	cmd.Env = env
	setProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		os.RemoveAll(tmp)
		return nil, &NativeError{Status: 503, Type: "api_error", Code: "binary_missing",
			Message: "codex-bridge: cannot start the Codex CLI: " + err.Error()}
	}
	c.cmd, c.stdin = cmd, stdin
	c.conn = newConn(stdin, onNotify, onRequest)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			c.stderr.add(sc.Text())
		}
	}()
	go func() {
		_ = c.conn.readLoop(stdout)
		close(c.readDone)
	}()
	c.bind(ctx)

	init := map[string]any{"clientInfo": map[string]any{"name": "ezllm-codex-bridge", "version": r.Version}}
	if opts.tools || opts.experimental {
		init["capabilities"] = map[string]any{"experimentalApi": true} // dynamicTools is experimental
	}
	if _, err := c.conn.call(ctx, "initialize", init); err != nil {
		c.close()
		return nil, r.childErr(ctx, c, "initialize", err)
	}
	if err := c.conn.notify("initialized", nil); err != nil {
		c.close()
		return nil, r.childErr(ctx, c, "initialized", err)
	}
	return c, nil
}

// close ends the child and everything it spawned, reaps it, and removes the
// work dir. Safe to call more than once.
func (c *child) close() {
	c.once.Do(func() {
		if c.stdin != nil {
			_ = c.stdin.Close()
		}
		killGroup(c.cmd)
		if c.cmd != nil && c.cmd.Process != nil {
			_ = c.cmd.Wait()
		}
		<-c.readDone
		if !c.keepWork {
			os.RemoveAll(c.tmp)
		}
	})
}

func (c *child) dispose() {
	c.close()
	if c.lease != nil {
		c.lease.release()
	}
}

func (r *Runner) childErr(ctx context.Context, c *child, step string, err error) error {
	var ne *NativeError
	if errors.As(err, &ne) {
		return ne
	}
	if ctx.Err() != nil {
		return &NativeError{Status: 504, Type: "api_error", Code: "timeout",
			Message: "codex-bridge: the Codex CLI did not finish in time (" + step + ")"}
	}
	msg := "codex-bridge: " + step + " failed: " + err.Error()
	if tail := c.stderr.tail(); tail != "" {
		msg += " | stderr: " + tail
	}
	return &NativeError{Status: 502, Type: "api_error", Code: "native_closed", Message: msg, BeforeInference: step == "initialize" || step == "initialized" || step == "thread/start"}
}

// declineRequests answers every server→client request a run does not serve:
// approvals are declined (approvalPolicy=never should prevent them) and
// nothing else is served.
func declineRequests(_ json.RawMessage, method string, _ json.RawMessage) (any, *rpcError) {
	if strings.Contains(strings.ToLower(method), "approval") {
		return map[string]any{"decision": "decline"}, nil
	}
	return nil, &rpcError{Code: -32601, Message: "codex-bridge does not serve " + method}
}

// Session runs fn against a fresh child — catalog and rate-limit reads.
func (r *Runner) Session(ctx context.Context, fn func(ctx context.Context, c *rpcConn) error) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, err := r.spawn(ctx, spawnOpts{}, nil, declineRequests)
	if err != nil {
		return err
	}
	defer c.close()
	if err := fn(ctx, c.conn); err != nil {
		return r.childErr(ctx, c, "read", err)
	}
	return nil
}

// toolBatchWindow is how long the bridge waits after a tool call for siblings
// (Promise.all in the model's script) before returning the batch.
const toolBatchWindow = 250 * time.Millisecond

// A stalled sink can retain at most one delivery batch plus this pending
// queue. Both bytes and event count are bounded: tiny deltas must not grow
// the slice indefinitely either. Overflow ends the turn rather than blocking
// the stdout reader, which must stay free for cancellation and cleanup.
const (
	maxQueuedOutputBytes  = 1 << 20
	maxQueuedOutputEvents = 4096
)

// turnState collects one turn's notifications and tool calls. Its methods run
// on the child's reader goroutine; wake tells the waiting request to look.
// Deltas are queued, not delivered here: a sink blocked on a client that
// stopped reading must never stall the reader (and with it child cleanup).
type turnState struct {
	caller      string
	hasHistory  bool
	historyHash [32]byte
	mu          sync.Mutex
	sink        func(Delta) error
	out         []Delta // queued for the request goroutine; the reader never calls sink
	outBytes    int
	abort       func() // asynchronously reaps the child on backlog overflow
	text        strings.Builder
	lastItem    string
	threadID    string // cache mode: ignore template and unrelated thread events

	reasoning     strings.Builder
	reasoningPart bool // a new summary part began: separate it
	usage         Usage
	reported      Usage
	fail          *NativeError
	finished      bool

	calls      []ToolCall
	replyTo    map[string]json.RawMessage // callId → JSON-RPC id
	batch      *time.Timer
	callsReady bool

	wake chan struct{}
}

func newTurnState(sink func(Delta) error) *turnState {
	return &turnState{sink: sink, replyTo: map[string]json.RawMessage{}, wake: make(chan struct{}, 1)}
}

func (t *turnState) bindChild(c *child) {
	t.mu.Lock()
	t.abort = func() { go c.close() }
	failed := t.finished && t.fail != nil
	t.mu.Unlock()
	if failed {
		go c.close()
	}
}

func (t *turnState) signal() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

func (t *turnState) handle(method string, params json.RawMessage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}
	if t.threadID != "" {
		var envelope struct {
			ThreadID string `json:"threadId"`
		}
		if json.Unmarshal(params, &envelope) != nil || envelope.ThreadID != t.threadID {
			return
		}
	}
	switch method {
	case "item/agentMessage/delta":
		var p struct {
			ItemID string `json:"itemId"`
			Delta  string `json:"delta"`
		}
		if json.Unmarshal(params, &p) != nil || p.Delta == "" {
			return
		}
		delta := p.Delta
		if t.lastItem != "" && p.ItemID != t.lastItem {
			delta = "\n\n" + delta // a second agent message (commentary, then answer)
		}
		if t.queue(Delta{Content: delta}) {
			t.lastItem = p.ItemID
			t.text.WriteString(delta)
		}
	case "item/reasoning/summaryPartAdded":
		t.reasoningPart = t.reasoning.Len() > 0
	case "item/reasoning/summaryTextDelta":
		var p struct {
			Delta string `json:"delta"`
		}
		if json.Unmarshal(params, &p) != nil || p.Delta == "" {
			return
		}
		delta := p.Delta
		if t.reasoningPart {
			delta, t.reasoningPart = "\n\n"+delta, false
		}
		if t.queue(Delta{Reasoning: delta}) {
			t.reasoning.WriteString(delta)
		}
	case "thread/tokenUsage/updated":
		var p struct {
			TokenUsage struct {
				Total Usage `json:"total"`
			} `json:"tokenUsage"`
		}
		if json.Unmarshal(params, &p) == nil {
			t.usage = p.TokenUsage.Total
		}
	case "error":
		var p struct {
			Error     turnError `json:"error"`
			WillRetry bool      `json:"willRetry"`
		}
		if json.Unmarshal(params, &p) == nil && !p.WillRetry {
			t.fail = MapTurnError(p.Error)
		}
	case "turn/completed":
		var p struct {
			Turn struct {
				Status string     `json:"status"`
				Error  *turnError `json:"error"`
			} `json:"turn"`
		}
		if json.Unmarshal(params, &p) == nil && p.Turn.Status != "completed" && t.fail == nil {
			te := turnError{Message: "turn ended with status " + p.Turn.Status}
			if p.Turn.Error != nil {
				te = *p.Turn.Error
			}
			t.fail = MapTurnError(te)
		}
		t.finished = true
		t.signal()
	}
}

// queue hands one delta to the request goroutine; mu is held. A full queue
// fails explicitly and kills the child independently of a blocked sink.
func (t *turnState) queue(d Delta) bool {
	n := len(d.Content) + len(d.Reasoning)
	if n > maxQueuedOutputBytes-t.outBytes || len(t.out) >= maxQueuedOutputEvents {
		t.fail = &NativeError{Status: 502, Type: "api_error", Code: "output_backpressure",
			Message: "codex-bridge: output backlog exceeded; the client is not draining the stream fast enough"}
		t.finished = true
		t.out, t.outBytes = nil, 0
		if t.abort != nil {
			t.abort()
		}
		t.signal()
		return false
	}
	t.out = append(t.out, d)
	t.outBytes += n
	t.signal()
	return true
}

// onRequest serves server→client requests for a tool-bearing turn: a dynamic
// tool call is held (deferred) until the client answers it; the rest decline.
func (t *turnState) onRequest(id json.RawMessage, method string, params json.RawMessage) (any, *rpcError) {
	if method != "item/tool/call" {
		return declineRequests(id, method, params)
	}
	var p struct {
		ThreadID  string          `json:"threadId"`
		CallID    string          `json:"callId"`
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.CallID == "" {
		return nil, &rpcError{Code: -32602, Message: "codex-bridge: malformed tool call"}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.threadID != "" && p.ThreadID != t.threadID {
		return nil, &rpcError{Code: -32602, Message: "codex-bridge: tool call belongs to another thread"}
	}
	t.calls = append(t.calls, ToolCall{ID: p.CallID, Name: p.Tool, Arguments: toolArguments(p.Arguments)})
	t.replyTo[p.CallID] = id
	if t.batch == nil {
		t.batch = time.AfterFunc(toolBatchWindow, func() {
			t.mu.Lock()
			t.callsReady = true
			t.mu.Unlock()
			t.signal()
		})
	} else {
		t.batch.Reset(toolBatchWindow)
	}
	return nil, errDeferred
}

// toolArguments renders a call's arguments as the JSON text OpenAI expects.
// The code-mode script may pass an object or a string (its tool methods take
// either); a string that already holds JSON passes through.
func toolArguments(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil && json.Valid([]byte(s)) && strings.HasPrefix(strings.TrimSpace(s), "{") {
		return s
	}
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}

// segment waits until the turn completes, stops on tool calls, or fails.
// park reports a tool stop: the child must stay alive.
func (r *Runner) segment(ctx context.Context, c *child, st *turnState) (res Result, park bool, err error) {
	for {
		st.mu.Lock()
		out, sink := st.out, st.sink
		st.out, st.outBytes = nil, 0
		finished, ready := st.finished, st.callsReady
		st.mu.Unlock()
		// Deliver in order, outside the lock, on this (the request's)
		// goroutine. A blocked delivery holds no lock the reader or
		// cancellation needs; the child stays bound to ctx meanwhile.
		for _, d := range out {
			if sink != nil {
				if err := sink(d); err != nil {
					return Result{}, false, err
				}
			}
		}
		if len(out) > 0 {
			continue // look again: more may have queued during delivery
		}
		switch {
		case finished || ready:
			st.mu.Lock()
			defer st.mu.Unlock()
			if st.fail != nil {
				return Result{}, false, st.fail
			}
			res = Result{Text: st.text.String(), Reasoning: st.reasoning.String(), Usage: st.usage.minus(st.reported)}
			st.reported = st.usage
			st.text.Reset()
			st.reasoning.Reset()
			st.lastItem = ""
			if ready && !finished {
				res.ToolCalls = st.calls
				return res, true, nil
			}
			return res, false, nil
		}
		select {
		case <-st.wake:
		case <-c.readDone:
			st.mu.Lock()
			finished := st.finished
			st.mu.Unlock()
			if !finished {
				return Result{}, false, r.childErr(ctx, c, "turn", errors.New("the CLI exited mid-turn"))
			}
		case <-ctx.Done():
			st.mu.Lock()
			failure := st.fail
			st.mu.Unlock()
			if failure != nil {
				return Result{}, false, failure
			}
			return Result{}, false, r.childErr(ctx, c, "turn", ctx.Err())
		}
	}
}

// Run executes one request. A request carrying the results for a parked
// turn's tool calls resumes that turn; otherwise a new ephemeral thread runs.
// A positively pre-inference app-server failure is retried once on the
// exec channel (§4.1) — text requests only, exec has no client tools.
func (r *Runner) Run(ctx context.Context, req Request, sink func(Delta) error) (Result, error) {
	// The timeout bounds the whole request, the wait for a slot included: a
	// caller with no deadline of its own must not queue forever behind
	// stuck slots.
	ctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()
	select {
	case r.slots() <- struct{}{}:
		defer func() { <-r.slots() }()
	case <-ctx.Done():
		return Result{}, &NativeError{Status: 504, Type: "api_error", Code: "timeout",
			Message: "codex-bridge: timed out waiting for a free slot"}
	}

	if len(req.ToolResults) > 0 {
		pt, takeErr := r.takeParked(req)
		if takeErr != nil {
			return Result{}, takeErr
		}
		if pt != nil {
			compatible := true
			if pt.c.lease != nil {
				key, ok := r.templateKey(req)
				compatible = ok && key == pt.c.lease.key
			}
			if compatible {
				return r.resume(ctx, pt, req, sink)
			}
			pt.c.dispose()
		}
		r.logf("codex-bridge: tool results match no parked turn, replaying the transcript")
	}
	if r.Channel == ChannelExec {
		if len(req.Tools) > 0 {
			return Result{}, &NativeError{Status: 400, Type: "invalid_request_error", Code: "chat_tools_unsupported",
				Message: "codex-bridge: client tools need the app-server channel; this bridge runs on the exec fallback"}
		}
		return r.runExec(ctx, req, sink)
	}
	emitted := false
	res, err := r.runAppServer(ctx, req, func(d Delta) error {
		emitted = true
		if sink != nil {
			return sink(d)
		}
		return nil
	})
	var ne *NativeError
	if err != nil && !emitted && len(req.Tools) == 0 && ctx.Err() == nil && errors.As(err, &ne) && ne.Code == "native_closed" && ne.BeforeInference {
		r.logf("codex-bridge: app-server failed before inference, retrying on exec", "err", ne.Message)
		return r.runExec(ctx, req, sink)
	}
	return res, err
}

func (r *Runner) runAppServer(ctx context.Context, req Request, sink func(Delta) error) (Result, error) {
	st := newTurnState(sink)
	st.caller, st.hasHistory, st.historyHash = req.CallerID, req.hasHistory, req.historyHash
	if !req.hasHistory {
		st.historyHash = inputHash(req.Input)
	}
	tools := len(req.Tools) > 0
	handler := RequestHandler(declineRequests)
	if tools {
		handler = st.onRequest
	}
	lease := r.borrowTemplate(req)
	opts := spawnOpts{tools: tools, experimental: r.NativeHistory}
	if lease != nil {
		opts.workRoot, opts.experimental = lease.root, true
		st.threadID = "awaiting-fork" // ignore parent startup notifications
	}
	c, err := r.spawn(ctx, opts, st.handle, handler)
	if err != nil {
		if lease != nil {
			lease.release()
		}
		return Result{}, err
	}
	c.lease = lease
	st.bindChild(c)
	parked := false
	defer func() {
		if !parked {
			c.dispose()
		}
	}()

	thread := map[string]any{
		"model":            req.Model,
		"cwd":              c.cwd,
		"ephemeral":        true,
		"sandbox":          string(r.sandboxMode()),
		"approvalPolicy":   r.sandboxMode().ApprovalFor(),
		"baseInstructions": req.Instructions,
	}
	if tools {
		specs := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			specs = append(specs, map[string]any{"type": "function", "name": t.Name,
				"description": t.Description, "inputSchema": t.Parameters})
		}
		thread["dynamicTools"] = specs
	}
	var raw json.RawMessage
	if lease != nil {
		raw, err = r.forkTemplate(ctx, c, lease, thread)
	} else {
		raw, err = c.conn.call(ctx, "thread/start", thread)
	}
	if err != nil {
		return Result{}, r.startErr(ctx, c, "thread/start", err)
	}
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(raw, &started); err != nil || started.Thread.ID == "" {
		return Result{}, r.childErr(ctx, c, "thread/start", fmt.Errorf("no thread id in %s", raw))
	}
	if lease != nil {
		st.mu.Lock()
		st.threadID = started.Thread.ID
		st.mu.Unlock()
	}
	input := req.Input
	if r.NativeHistory && len(req.turns) > 0 {
		items, latest, historyErr := nativeConversation(req.turns)
		if historyErr != nil && !errors.Is(historyErr, errUnpairedNativeHistory) {
			return Result{}, historyErr
		}
		if historyErr == nil {
			input = latest
		}
		if len(items) > 0 {
			if _, err := c.conn.call(ctx, "thread/inject_items", map[string]any{"threadId": started.Thread.ID, "items": items}); err != nil {
				return Result{}, r.startErr(ctx, c, "thread/inject_items", err)
			}
		}
	}
	turn := map[string]any{"threadId": started.Thread.ID, "input": input}
	if req.Effort != "" {
		turn["effort"] = req.Effort
	}
	if req.Summary != "" {
		turn["summary"] = req.Summary
	}
	if present(req.OutputSchema) {
		turn["outputSchema"] = req.OutputSchema
	}
	if _, err := c.conn.call(ctx, "turn/start", turn); err != nil {
		return Result{}, r.startErr(ctx, c, "turn/start", err)
	}

	res, park, err := r.segment(ctx, c, st)
	if err != nil {
		return Result{}, err
	}
	if park {
		if !c.release() {
			return Result{}, r.childErr(ctx, c, "turn", ctx.Err())
		}
		r.park(c, st, res)
		parked = true
	} else if lease != nil && ctx.Err() == nil && validTemplateUsage(res.Usage) {
		lease.good.Store(true)
	}
	return res, nil
}

// startErr maps a JSON-RPC refusal of thread/start or turn/start (a model or
// effort the CLI does not serve) to a 400 with the CLI's own words.
func (r *Runner) startErr(ctx context.Context, c *child, step string, err error) error {
	var re *rpcError
	if errors.As(err, &re) && re.Code != -32000 {
		return &NativeError{Status: 400, Type: "invalid_request_error", Code: "invalid_request",
			Message: "codex-bridge: " + step + " refused: " + re.Message}
	}
	return r.childErr(ctx, c, step, err)
}
