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
// (docs/codex-subscription-design.md §2.5). code_mode_host stays off until
// stage 2 brings Landlock; in_app_updates keeps the pinned binary pinned.
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

// LockdownArgs is the argv tail for `codex app-server --stdio`.
func LockdownArgs() []string {
	args := make([]string, 0, 2*len(LockdownFeatures)+4)
	for _, f := range LockdownFeatures {
		args = append(args, "--disable", f)
	}
	return append(args, "-c", "tools.web_search=false", "-c", `web_search="disabled"`)
}

// Runner spawns one app-server child per request (claude-bridge's shape): a
// private temp HOME and CWD, an allowlisted environment, its own process
// group, and a concurrency cap.
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

	sem  chan struct{}
	once sync.Once
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

// Result is what one turn produced.
type Result struct {
	Text  string
	Usage Usage
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
}

// spawn starts the child and completes the initialize handshake.
func (r *Runner) spawn(ctx context.Context, onNotify func(string, json.RawMessage), onRequest RequestHandler) (*child, error) {
	tmp, err := os.MkdirTemp("", "codex-bridge-")
	if err != nil {
		return nil, fmt.Errorf("codex-bridge: cannot create work dir: %w", err)
	}
	c := &child{tmp: tmp, cwd: filepath.Join(tmp, "cwd"), stderr: &tailBuffer{}, readDone: make(chan struct{})}
	home := filepath.Join(tmp, "home")
	for _, d := range []string{c.cwd, home} {
		if err := os.Mkdir(d, 0o700); err != nil {
			os.RemoveAll(tmp)
			return nil, fmt.Errorf("codex-bridge: cannot create work dir: %w", err)
		}
	}

	cmd := exec.Command(r.Bin, append([]string{"app-server", "--stdio"}, LockdownArgs()...)...)
	cmd.Dir = c.cwd
	cmd.Env = childEnv(home, r.CodexHome)
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

	if _, err := c.conn.call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "ezllm-codex-bridge", "version": r.Version},
	}); err != nil {
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
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	killGroup(c.cmd)
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Wait()
	}
	<-c.readDone
	os.RemoveAll(c.tmp)
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
	return &NativeError{Status: 502, Type: "api_error", Code: "native_closed", Message: msg}
}

// declineRequests answers every server→client request in a text-only run:
// approvals are declined (approvalPolicy=never should prevent them) and
// nothing else is served.
func declineRequests(method string, _ json.RawMessage) (any, *rpcError) {
	if strings.Contains(strings.ToLower(method), "approval") {
		return map[string]any{"decision": "decline"}, nil
	}
	return nil, &rpcError{Code: -32601, Message: "codex-bridge does not serve " + method}
}

// Session runs fn against a fresh child — catalog and rate-limit reads.
func (r *Runner) Session(ctx context.Context, fn func(ctx context.Context, c *rpcConn) error) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, err := r.spawn(ctx, nil, declineRequests)
	if err != nil {
		return err
	}
	defer c.close()
	if err := fn(ctx, c.conn); err != nil {
		return r.childErr(ctx, c, "read", err)
	}
	return nil
}

// turnState collects one turn's notifications (called on the reader goroutine).
type turnState struct {
	mu       sync.Mutex
	sink     func(delta string) error
	sinkErr  error
	text     strings.Builder
	lastItem string
	usage    Usage
	fail     *NativeError
	done     chan struct{}
	finished bool
}

func (t *turnState) finish() {
	if !t.finished {
		t.finished = true
		close(t.done)
	}
}

func (t *turnState) handle(method string, params json.RawMessage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
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
		t.lastItem = p.ItemID
		t.text.WriteString(delta)
		if t.sink != nil && t.sinkErr == nil {
			if err := t.sink(delta); err != nil {
				t.sinkErr = err
				t.finish()
			}
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
		t.finish()
	}
}

// Run executes one request as a single turn on an ephemeral thread. sink, when
// set, receives text deltas as they stream. An app-server child that dies
// before its first byte is retried once on the exec channel (§4.1); after the
// first byte the client already holds part of the answer, so it is not.
func (r *Runner) Run(ctx context.Context, req Request, sink func(delta string) error) (Result, error) {
	select {
	case r.slots() <- struct{}{}:
		defer func() { <-r.slots() }()
	case <-ctx.Done():
		return Result{}, &NativeError{Status: 504, Type: "api_error", Code: "timeout",
			Message: "codex-bridge: timed out waiting for a free slot"}
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()
	if r.Channel == ChannelExec {
		return r.runExec(ctx, req, sink)
	}
	emitted := false
	res, err := r.runAppServer(ctx, req, func(d string) error {
		emitted = true
		if sink != nil {
			return sink(d)
		}
		return nil
	})
	var ne *NativeError
	if err != nil && !emitted && ctx.Err() == nil && errors.As(err, &ne) && ne.Code == "native_closed" {
		r.logf("codex-bridge: app-server failed before the first byte, retrying on exec", "err", ne.Message)
		return r.runExec(ctx, req, sink)
	}
	return res, err
}

func (r *Runner) runAppServer(ctx context.Context, req Request, sink func(delta string) error) (Result, error) {
	st := &turnState{sink: sink, done: make(chan struct{})}
	c, err := r.spawn(ctx, st.handle, declineRequests)
	if err != nil {
		return Result{}, err
	}
	defer c.close()

	raw, err := c.conn.call(ctx, "thread/start", map[string]any{
		"model":            req.Model,
		"cwd":              c.cwd,
		"ephemeral":        true,
		"sandbox":          "read-only",
		"approvalPolicy":   "never",
		"baseInstructions": req.Instructions,
	})
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
	turn := map[string]any{"threadId": started.Thread.ID, "input": req.Input}
	if req.Effort != "" {
		turn["effort"] = req.Effort
	}
	if _, err := c.conn.call(ctx, "turn/start", turn); err != nil {
		return Result{}, r.startErr(ctx, c, "turn/start", err)
	}

	select {
	case <-st.done:
	case <-c.readDone:
		st.mu.Lock()
		finished := st.finished
		st.mu.Unlock()
		if !finished {
			return Result{}, r.childErr(ctx, c, "turn", errors.New("the CLI exited mid-turn"))
		}
	case <-ctx.Done():
		return Result{}, r.childErr(ctx, c, "turn", ctx.Err())
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if st.sinkErr != nil {
		return Result{}, st.sinkErr
	}
	if st.fail != nil {
		return Result{}, st.fail
	}
	return Result{Text: st.text.String(), Usage: st.usage}, nil
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
