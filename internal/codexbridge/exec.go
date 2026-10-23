package codexbridge

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// The exec channel is the fallback (operator decision §4.1): `codex exec
// --json` is the stable headless surface, but it emits each agent message
// whole (no deltas, §2.2), so a stream degrades to one content chunk. It is
// used when the app-server lacks a method this bridge needs (boot) or a child
// dies before its first byte (per request). Same lockdown, same isolation.

// ChannelAppServer and ChannelExec name the two ways to drive the CLI.
const (
	ChannelAppServer = "app-server"
	ChannelExec      = "exec"
)

// errUnavailableOnExec answers catalog and plan-window reads in exec mode:
// only the app-server serves them, and a 404 keeps the provider card on
// "measured" instead of an error.
func errUnavailableOnExec(what string) error {
	return &NativeError{Status: 404, Type: "not_found_error", Code: "unavailable_on_exec",
		Message: "codex-bridge: " + what + " needs the app-server channel; this bridge runs on the exec fallback"}
}

func (r *Runner) runExec(ctx context.Context, req Request, sink func(string) error) (Result, error) {
	tmp, err := os.MkdirTemp("", "codex-bridge-")
	if err != nil {
		return Result{}, fmt.Errorf("codex-bridge: cannot create work dir: %w", err)
	}
	defer os.RemoveAll(tmp)
	cwd, home := filepath.Join(tmp, "cwd"), filepath.Join(tmp, "home")
	for _, d := range []string{cwd, home} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return Result{}, fmt.Errorf("codex-bridge: cannot create work dir: %w", err)
		}
	}

	args := []string{"exec", "--json", "--ephemeral", "--skip-git-repo-check", "--ignore-user-config",
		"--ignore-rules", "--sandbox", "read-only", "-C", cwd}
	args = append(args, LockdownArgs()...)
	args = append(args, "-m", req.Model, "-c", "instructions="+strconv.Quote(req.Instructions))
	if req.Effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+strconv.Quote(req.Effort))
	}
	var prompt []string
	for i, in := range req.Input {
		switch in.Type {
		case "text":
			prompt = append(prompt, in.Text)
		case "image":
			path, err := writeDataURL(tmp, i, in.URL)
			if err != nil {
				return Result{}, err
			}
			args = append(args, "-i", path)
		}
	}
	args = append(args, "-")

	cmd := exec.Command(r.Bin, args...)
	cmd.Dir = cwd
	cmd.Env = childEnv(home, r.CodexHome)
	cmd.Stdin = strings.NewReader(strings.Join(prompt, "\n\n"))
	setProcessGroup(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	tb := &tailBuffer{}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, err
	}
	if err := cmd.Start(); err != nil {
		return Result{}, &NativeError{Status: 503, Type: "api_error", Code: "binary_missing",
			Message: "codex-bridge: cannot start the Codex CLI: " + err.Error()}
	}
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			tb.add(sc.Text())
		}
	}()
	stop := context.AfterFunc(ctx, func() { killGroup(cmd) })
	defer stop()
	defer func() { killGroup(cmd); _ = cmd.Wait() }()

	var (
		res     Result
		fail    *NativeError
		done    bool
		written bool
	)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var ev struct {
			Type string `json:"type"`
			Item struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
			Usage struct {
				InputTokens     int64 `json:"input_tokens"`
				CachedTokens    int64 `json:"cached_input_tokens"`
				OutputTokens    int64 `json:"output_tokens"`
				ReasoningTokens int64 `json:"reasoning_output_tokens"`
			} `json:"usage"`
			Message string `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "item.completed":
			if ev.Item.Type != "agent_message" || ev.Item.Text == "" {
				continue // "error" items are CLI warnings, not the answer
			}
			text := ev.Item.Text
			if written {
				text = "\n\n" + text
			}
			written = true
			res.Text += text
			if sink != nil {
				if err := sink(text); err != nil {
					return Result{}, err
				}
			}
		case "turn.completed":
			res.Usage = Usage{InputTokens: ev.Usage.InputTokens, CachedTokens: ev.Usage.CachedTokens,
				OutputTokens: ev.Usage.OutputTokens, ReasoningTokens: ev.Usage.ReasoningTokens}
			done = true
		case "turn.failed":
			fail = mapExecError(ev.Error.Message)
			done = true
		}
	}
	if ctx.Err() != nil {
		return Result{}, &NativeError{Status: 504, Type: "api_error", Code: "timeout",
			Message: "codex-bridge: the Codex CLI did not finish in time (exec)"}
	}
	if fail != nil {
		return Result{}, fail
	}
	if !done {
		msg := "codex-bridge: exec output ended before the turn completed"
		if tail := tb.tail(); tail != "" {
			msg += " | stderr: " + tail
		}
		return Result{}, &NativeError{Status: 502, Type: "api_error", Code: "native_closed", Message: msg}
	}
	return res, nil
}

// mapExecError reads exec's turn.failed message, which carries the upstream
// error as a JSON string ({"status":400,"error":{"message":…}}, measured).
func mapExecError(raw string) *NativeError {
	var inner struct {
		Status int `json:"status"`
		Error  struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := raw
	if json.Unmarshal([]byte(raw), &inner) == nil && inner.Error.Message != "" {
		msg = inner.Error.Message
	}
	msg = "codex-bridge: " + msg
	switch inner.Status {
	case 429:
		return &NativeError{Status: 429, Type: "rate_limit_error", Code: "rate_limit", Message: msg}
	case 401, 403:
		return &NativeError{Status: 401, Type: "authentication_error", Code: "unauthorized", Message: msg}
	case 400:
		return &NativeError{Status: 400, Type: "invalid_request_error", Code: "invalid_request", Message: msg}
	}
	if strings.Contains(strings.ToLower(msg), "usage limit") {
		return &NativeError{Status: 429, Type: "rate_limit_error", Code: "rate_limit", Message: msg}
	}
	return &NativeError{Status: 502, Type: "api_error", Code: "native_error", Message: msg}
}

// writeDataURL stores a data: URL image as a file for `exec -i`; exec takes
// files only, so remote image URLs need the app-server channel.
func writeDataURL(dir string, i int, url string) (string, error) {
	meta, data, ok := strings.Cut(strings.TrimPrefix(url, "data:"), ",")
	if !strings.HasPrefix(url, "data:") || !ok || !strings.HasSuffix(meta, ";base64") {
		return "", &NativeError{Status: 400, Type: "invalid_request_error", Code: "invalid_request",
			Message: "codex-bridge: on the exec fallback images must be base64 data: URLs"}
	}
	b, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return "", &NativeError{Status: 400, Type: "invalid_request_error", Code: "invalid_request",
			Message: "codex-bridge: image data: URL is not valid base64"}
	}
	ext := ".png"
	switch {
	case strings.Contains(meta, "jpeg"), strings.Contains(meta, "jpg"):
		ext = ".jpg"
	case strings.Contains(meta, "webp"):
		ext = ".webp"
	case strings.Contains(meta, "gif"):
		ext = ".gif"
	}
	path := filepath.Join(dir, fmt.Sprintf("image-%d%s", i, ext))
	return path, os.WriteFile(path, b, 0o600)
}
