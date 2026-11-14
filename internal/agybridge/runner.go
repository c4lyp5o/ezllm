package agybridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Usage is the result envelope's usage block. Measured shape (headless docs,
// 1.4.12): output_tokens includes thinking_tokens; input_tokens is cold input,
// separate from cache_read_tokens. Native total = cold input + output.
type Usage struct {
	InputTokens    int `json:"input_tokens"`
	OutputTokens   int `json:"output_tokens"`
	ThinkingTokens int `json:"thinking_tokens"`
	CacheRead      int `json:"cache_read_tokens"`
	TotalTokens    int `json:"total_tokens"`
}

// Result is what one turn produced.
type Result struct {
	Text             string
	StructuredOutput json.RawMessage
	Usage            Usage
	ConversationID   string
	ToolSteps        int // agent tool steps the turn attempted (logged, never surfaced)
	decoded          *decodedReply
}

// Executor runs one request. *Runner implements it; tests inject a fake.
type Executor interface {
	Run(ctx context.Context, req Request, delta func(string) error) (Result, error)
}

// Runner spawns the official Antigravity CLI once per request: `agy -p` in
// headless stream-json mode. Workspaces are fresh by default; opt-in conversation
// reuse retains only a matched private workspace and metadata. It is the only thing
// in this package that talks to the subscription — the login never leaves the
// CLI's own token file under Home.
type Runner struct {
	Bin           string
	Home          string // HOME for the child: holds .gemini/antigravity-cli/antigravity-oauth-token
	Timeout       time.Duration
	MaxConcurrent int
	Logf          func(format string, args ...any)
	// SessionCache keeps bounded metadata and workspaces, never idle processes.
	SessionCache     bool
	CacheIdleTimeout time.Duration
	cacheMu          sync.Mutex
	cached           map[*cachedConversation]struct{}
	cacheClosed      bool
	cacheHits        uint64

	sem  chan struct{}
	once sync.Once
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

// argv is the headless invocation. No --dangerously-skip-permissions: shell
// commands stay on Ask and are soft-denied in print mode, and writes are
// confined to the empty per-request workspace. --disable-slash-commands keeps a
// prompt that starts with "/" from running a CLI command or skill.
//
// --print takes the prompt as its VALUE: a bare -p followed by a flag swallows
// that flag (measured, 1.4.7: exit 2, "-p took --output-format as its
// prompt"). The prompt arrives on stdin, so the value is explicitly empty.
func argv(req Request) []string {
	a := []string{"--print=", "--output-format", "stream-json", "--input-format", "stream-json",
		"--disable-slash-commands", "--model", req.Model}
	if req.Effort != "" {
		a = append(a, "--effort", req.Effort)
	}
	return a
}

// frame is one stdout NDJSON line.
type frame struct {
	Event          string `json:"event"`
	ConversationID string `json:"conversation_id"`
	StepType       string `json:"step_type"`
	TextDelta      string `json:"text_delta"`
	State          string `json:"state"`
	ToolName       string `json:"tool_name"`
	StepUpdate     *struct {
		ConversationID string `json:"conversation_id"`
		StepType       string `json:"step_type"`
		TextDelta      string `json:"text_delta"`
		State          string `json:"state"`
		ToolName       string `json:"tool_name"`
	} `json:"step_update"`
	Init *struct {
		PermissionMode string `json:"permission_mode"`
	} `json:"init"`
	Result *struct {
		ConversationID   string          `json:"conversation_id"`
		Status           string          `json:"status"`
		Response         string          `json:"response"`
		StructuredOutput json.RawMessage `json:"structured_output"`
		Error            string          `json:"error"`
		Usage            Usage           `json:"usage"`
	} `json:"result"`
}

// Run executes one turn: spawn, write the prompt, stream deltas to delta.
func (r *Runner) Run(ctx context.Context, req Request, delta func(string) error) (Result, error) {
	runCtx := ctx
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	select {
	case r.slots() <- struct{}{}:
		defer func() { <-r.slots() }()
	case <-runCtx.Done():
		return Result{}, &NativeError{Status: 504, Type: "api_error", Code: "timeout",
			Message: "agy-bridge: timed out waiting for a free slot"}
	}

	if r.SessionCache && req.Adapter != nil {
		return r.runCached(runCtx, req, delta)
	}
	return r.run(runCtx, req, delta)
}

func (r *Runner) run(runCtx context.Context, req Request, delta func(string) error) (Result, error) {
	work := req.cacheWork
	if work == "" {
		var err error
		work, err = os.MkdirTemp("", "agy-bridge-")
		if err != nil {
			return Result{}, fmt.Errorf("agy-bridge: cannot create work dir: %w", err)
		}
		defer os.RemoveAll(work)
	}

	args := argv(req)
	if req.cacheConversation != "" {
		args = append(args, "--conversation", req.cacheConversation)
	}
	if req.Adapter != nil {
		schemaPath := filepath.Join(work, "caller-tools-schema.json")
		if err := os.WriteFile(schemaPath, req.Adapter.Schema, 0o600); err != nil {
			return Result{}, fmt.Errorf("agy-bridge: cannot write tool output schema: %w", err)
		}
		args = append(args, "--json-schema", schemaPath)
		// Adapter envelopes must be validated in full before any client output.
		delta = nil
	}
	cmd := exec.Command(r.Bin, args...)
	cmd.Dir = work
	cmd.Env = childEnv(r.Home)
	setProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Result{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, err
	}
	if err := cmd.Start(); err != nil {
		return Result{}, &NativeError{Status: 503, Type: "api_error", Code: "agy_missing",
			Message: "agy-bridge: cannot start the Antigravity CLI: " + err.Error()}
	}
	waited := false
	defer func() {
		killGroup(cmd)
		if !waited {
			_ = cmd.Wait()
		}
	}()
	done := make(chan struct{})
	defer close(done)

	var stderrTail tailBuffer
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			stderrTail.add(sc.Text())
		}
	}()

	// The prompt goes in on stdin, never argv: it stays out of ps and off
	// ARG_MAX. Closing stdin ends the session after this one turn.
	stopStdin := context.AfterFunc(runCtx, func() {
		_ = stdin.Close()
		// Also unblock a native process that emitted a result but has not exited.
		killGroup(cmd)
	})
	defer stopStdin()
	msg, _ := json.Marshal(map[string]any{"event": "user", "message": map[string]any{"content": req.Prompt}})
	if _, err := stdin.Write(append(msg, '\n')); err != nil {
		if runCtx.Err() != nil {
			return Result{}, deadlineErr(runCtx.Err())
		}
		return Result{}, &NativeError{Status: 502, Type: "api_error", Code: "native_closed",
			Message: "agy-bridge: the CLI stopped reading input: " + err.Error() + r.stderrNote(&stderrTail)}
	}
	_ = stdin.Close()

	lines := make(chan []byte)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 256*1024), 8*1024*1024)
		for sc.Scan() {
			select {
			case lines <- append([]byte(nil), sc.Bytes()...):
			case <-done:
				return
			}
		}
	}()

	var res Result
	var streamed strings.Builder
	var final *frame
	for final == nil {
		var line []byte
		var ok bool
		select {
		case line, ok = <-lines:
		case <-runCtx.Done():
			return res, deadlineErr(runCtx.Err())
		}
		if !ok {
			break
		}
		var f frame
		if err := json.Unmarshal(line, &f); err != nil {
			continue // not a protocol line; the CLI owns its stdout
		}
		switch f.Event {
		case "init":
			// The CLI's own statement of how it will treat tool calls. Anything
			// but review-only means a tool could run unasked on this host: the
			// turn is killed before the model is reached (fail closed).
			if f.Init != nil && !SafePermissionMode(f.Init.PermissionMode) {
				return res, &NativeError{Status: 500, Type: "api_error", Code: "lockdown",
					Message: "agy-bridge: the CLI reports permission_mode " + f.Init.PermissionMode + "; refusing the turn (docs/agy-bridge-runbook.md#lockdown)"}
			}
		case "step_update":
			// Current releases nest step payloads; retain flat-frame support for
			// previously qualified releases and fixtures.
			if step := f.StepUpdate; step != nil {
				f.ConversationID, f.StepType, f.TextDelta, f.State, f.ToolName = step.ConversationID, step.StepType, step.TextDelta, step.State, step.ToolName
			}
			if f.ConversationID != "" {
				res.ConversationID = f.ConversationID
			}
			switch f.StepType {
			case "agent_response":
				if f.TextDelta == "" {
					continue
				}
				if delta != nil {
					streamed.WriteString(f.TextDelta)
				}
				if delta != nil {
					if err := delta(f.TextDelta); err != nil {
						return res, err
					}
				}
			case "tool":
				if req.Adapter != nil {
					// finish is the CLI's structured-output submission, not a
					// filesystem or command operation. Its result still passes
					// envelope validation; every other native tool fails closed.
					if f.ToolName == "finish" {
						continue
					}
					return res, &NativeError{Status: 502, Type: "api_error", Code: "invalid_tool_output", Message: "agy-bridge: tool adapter turn attempted a native agent tool"}
				}
				if f.State == "DONE" {
					res.ToolSteps++
				}
			}
		case "result":
			if f.Result != nil {
				final = &f
			}
		}
	}
	// Drain stderr before Wait closes it, so a failure carries the CLI's
	// words; bounded, since a grandchild may still hold the pipe open.
	select {
	case <-stderrDone:
	case <-time.After(2 * time.Second):
	}
	_ = cmd.Wait()
	waited = true

	if final == nil {
		code := cmd.ProcessState.ExitCode()
		return res, &NativeError{Status: 502, Type: "api_error", Code: "native_error",
			Message: fmt.Sprintf("agy-bridge: the CLI exited %d without a result%s", code, r.stderrNote(&stderrTail))}
	}
	out := final.Result
	res.Usage = out.Usage
	if out.ConversationID != "" {
		res.ConversationID = out.ConversationID
	}
	if res.ToolSteps > 0 {
		r.logf("agy-bridge: turn attempted %d agent tool step(s)", res.ToolSteps)
	}
	if out.Status != "SUCCESS" {
		m := out.Error
		if m == "" {
			m = stderrTail.tail()
		}
		return res, mapError(out.Status, m)
	}
	res.Text = out.Response
	res.StructuredOutput = out.StructuredOutput
	// A turn that streamed nothing (or less than its answer) still delivers
	// the whole response: the envelope is authoritative.
	if delta != nil && streamed.Len() == 0 && res.Text != "" {
		if err := delta(res.Text); err != nil {
			return res, err
		}
	}
	return res, nil
}

func (r *Runner) stderrNote(t *tailBuffer) string {
	if s := t.tail(); s != "" {
		return " — " + urlPattern.ReplaceAllString(s, "[link withheld]")
	}
	return ""
}

func deadlineErr(err error) error {
	if err == context.DeadlineExceeded {
		return &NativeError{Status: 504, Type: "api_error", Code: "timeout", Message: "agy-bridge: the turn timed out"}
	}
	if err == context.Canceled {
		return &NativeError{Status: 499, Type: "api_error", Code: "client_closed", Message: "agy-bridge: client went away"}
	}
	if err == io.EOF {
		return &NativeError{Status: 502, Type: "api_error", Code: "native_closed", Message: "agy-bridge: the CLI closed its output"}
	}
	return err
}

// tailBuffer keeps the last lines of the child's stderr — diagnosis, not noise.
type tailBuffer struct {
	mu    sync.Mutex
	lines []string
}

func (t *tailBuffer) add(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, line)
	if len(t.lines) > 40 {
		t.lines = t.lines[len(t.lines)-40:]
	}
}

func (t *tailBuffer) tail() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, " | ")
}
