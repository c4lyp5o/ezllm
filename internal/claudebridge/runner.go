package claudebridge

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

// NativeError is a failure the CLI reported, already mapped to an HTTP status so
// the server does not have to re-derive it.
type NativeError struct {
	Status  int
	Type    string
	Code    string
	Message string
}

func (e *NativeError) Error() string { return e.Message }

// Runner spawns the official Claude Code CLI once per request. It is the only
// thing in this package allowed to talk to the subscription — the credential never
// leaves the CLI's own login store (docs/claude-subscription-design.md, decision 1).
type Runner struct {
	Bin           string
	ConfigDir     string
	Timeout       time.Duration
	MaxConcurrent int
	Logf          func(format string, args ...any)

	sem  chan struct{}
	once sync.Once
}

// Result is what one native run produced.
type Result struct {
	Usage       Usage
	Model       string
	MsgID       string
	StopReason  string
	CostUSD     float64
	RateLimit   map[string]any
	NativeError string
	ErrorCode   string
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

// Run executes one request: spawn, replay, generate once, translate frames to sink.
func (r *Runner) Run(ctx context.Context, req Request, alias string, sink func(Frame) error) (Result, error) {
	select {
	case r.slots() <- struct{}{}:
		defer func() { <-r.slots() }()
	case <-ctx.Done():
		return Result{}, &NativeError{Status: 504, Type: "api_error", Code: "timeout",
			Message: "claude-bridge: timed out waiting for a free slot"}
	}

	system, err := SystemText(req.System)
	if err != nil {
		return Result{}, err
	}
	extraBody, err := BuildExtraBody(req)
	if err != nil {
		return Result{}, err
	}
	frames, err := BuildFrames(req)
	if err != nil {
		return Result{}, err
	}

	tmp, err := os.MkdirTemp("", "claude-bridge-")
	if err != nil {
		return Result{}, fmt.Errorf("claude-bridge: cannot create work dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	settings := map[string]any{"env": map[string]any{"CLAUDE_CODE_EXTRA_BODY": extraBody}}
	if err := writeJSON(filepath.Join(tmp, "settings.json"), settings); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(filepath.Join(tmp, "system.md"), []byte(system), 0o600); err != nil {
		return Result{}, fmt.Errorf("claude-bridge: cannot write system prompt: %w", err)
	}
	if err := writeJSON(filepath.Join(tmp, "mcp.json"), map[string]any{"mcpServers": map[string]any{}}); err != nil {
		return Result{}, err
	}

	argv := []string{
		"-p", "--model", alias,
		"--input-format", "stream-json", "--output-format", "stream-json",
		"--verbose", "--include-partial-messages",
		"--tools", "",
		"--settings", filepath.Join(tmp, "settings.json"),
		"--setting-sources", "",
		"--system-prompt-file", filepath.Join(tmp, "system.md"),
		"--strict-mcp-config", "--mcp-config", filepath.Join(tmp, "mcp.json"),
		"--disable-slash-commands", "--max-turns", "1",
		"--permission-mode", "dontAsk", "--no-session-persistence",
		"--session-id", ConversationSessionID(system, req),
	}
	cmd := exec.Command(r.Bin, argv...)
	cmd.Dir = tmp
	cmd.Env = r.childEnv(tmp, req.MaxTokens)
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
		return Result{}, &NativeError{Status: 503, Type: "api_error", Code: "claude_missing",
			Message: "claude-bridge: cannot start the Claude Code CLI: " + err.Error()}
	}
	// Every exit path kills the group AND reaps the child: an early return
	// (timeout, client gone, bad frame) that only killed it left a zombie
	// per failed request. waited avoids a second Wait after the clean path.
	waited := false
	defer func() {
		killGroup(cmd)
		if !waited {
			_ = cmd.Wait()
		}
	}()
	// done releases the stdout reader on an early return; without it the
	// reader blocks forever on an unbuffered send nobody receives (one leaked
	// goroutine plus its 8 MiB scanner buffer per failed request). Deferred
	// after the kill/reap above, so it runs first.
	done := make(chan struct{})
	defer close(done)

	var stderrTail tailBuffer
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			stderrTail.add(sc.Text())
		}
	}()

	runCtx := ctx
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}

	lines := make(chan []byte)
	readErr := make(chan error, 1)
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
		readErr <- sc.Err()
	}()

	next := func() ([]byte, error) {
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					return nil, io.EOF
				}
				return line, nil
			case <-runCtx.Done():
				return nil, runCtx.Err()
			}
		}
	}

	// Replay: non-final user frames are history. Each waits for native's
	// zero-turn acknowledgement before the next frame is written, so the CLI
	// never generates (and never charges the plan) for a replayed frame.
	for i, frame := range frames {
		payload, _ := json.Marshal(frame)
		if _, err := stdin.Write(append(payload, '\n')); err != nil {
			return Result{}, &NativeError{Status: 502, Type: "api_error", Code: "native_closed",
				Message: "claude-bridge: native stopped reading input: " + err.Error()}
		}
		if ack, ok := frame["shouldQuery"].(bool); ok && !ack {
			for {
				line, err := next()
				if err != nil {
					return Result{}, r.deadlineErr(err, &stderrTail, i)
				}
				f, err := ParseFrame(line)
				if err != nil {
					return Result{}, err
				}
				if f.Type == "result" {
					if f.NumTurns != 0 || f.IsError {
						return Result{}, &NativeError{Status: 502, Type: "api_error", Code: "replay_unsupported",
							Message: "claude-bridge: native history replay not supported (expected a zero-turn acknowledgement)"}
					}
					break
				}
			}
		}
	}
	_ = stdin.Close()

	res := Result{}
	for {
		line, err := next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return res, r.deadlineErr(err, &stderrTail, len(frames))
		}
		f, err := ParseFrame(line)
		if err != nil {
			return res, err
		}
		switch f.Type {
		case "assistant":
			if f.Error != "" {
				res.NativeError, res.ErrorCode = firstText(f.Message), f.Error
			}
		case "result":
			res.Usage = parseUsage(f.Usage)
			res.CostUSD = f.TotalCostUSD
			res.StopReason = f.StopReason
			if f.IsError && res.NativeError == "" {
				res.NativeError = f.Result
			}
			if res.NativeError == "" && f.Result != "" && strings.HasPrefix(f.Result, "API Error") {
				res.NativeError = f.Result
			}
		case "rate_limit_event":
			_ = json.Unmarshal(f.RateLimitInfo, &res.RateLimit)
		case "stream_event":
			var ev map[string]any
			if err := json.Unmarshal(f.Event, &ev); err == nil {
				if ev["type"] == "message_start" {
					if m, ok := ev["message"].(map[string]any); ok {
						res.MsgID, _ = m["id"].(string)
						res.Model, _ = m["model"].(string)
					}
				}
			}
		}
		if sink != nil {
			if err := sink(f); err != nil {
				return res, err
			}
		}
	}
	_ = cmd.Wait()
	waited = true
	if res.NativeError != "" {
		status, errType := MapNativeError(res.ErrorCode, res.NativeError)
		return res, &NativeError{Status: status, Type: errType, Code: res.ErrorCode,
			Message: strings.TrimSpace(res.NativeError)}
	}
	return res, nil
}
