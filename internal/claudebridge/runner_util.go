package claudebridge

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// tailBuffer keeps the last lines of native's stderr — diagnosis, not noise. A
// gateway-only failure that vanishes into /dev/null is undiagnosable later.
type tailBuffer struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func (t *tailBuffer) add(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.max == 0 {
		t.max = 40
	}
	t.lines = append(t.lines, line)
	if len(t.lines) > t.max {
		t.lines = t.lines[len(t.lines)-t.max:]
	}
}

func (t *tailBuffer) tail() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, " | ")
}

// killGroup kills the whole process group: a cancelled request must not leave a
// CLI (or anything it spawned) running on the plan's meter.
func killGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	_ = cmd.Process.Kill()
}

// firstText renders an assistant message's text blocks for error reports.
func firstText(raw json.RawMessage) string {
	var msg struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return strings.TrimSpace(string(raw))
	}
	var parts []string
	for _, b := range msg.Content {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func (r *Runner) deadlineErr(err error, tb *tailBuffer, at int) error {
	if err == context.DeadlineExceeded || err == context.Canceled {
		return &NativeError{Status: 504, Type: "api_error", Code: "timeout",
			Message: "claude-bridge: native run did not finish in time"}
	}
	msg := "claude-bridge: native output ended unexpectedly after " + strconv.Itoa(at) + " frames: " + err.Error()
	if tail := tb.tail(); tail != "" {
		msg += " | stderr: " + tail
	}
	return &NativeError{Status: 502, Type: "api_error", Code: "native_closed", Message: msg}
}

// childEnv is the process environment for one native run. Conflicting auth and
// backend overrides are removed rather than inherited: the subscription login in
// ConfigDir is the only credential path, and an inherited ANTHROPIC_BASE_URL would
// silently redirect it (the plugin refuses to start in that state; we drop it and
// log). HOME points at the per-request temp dir so nothing reads or writes the
// operator's ~/.claude state.
func (r *Runner) childEnv(tmp string, maxTokens int) []string {
	drop := map[string]bool{
		"ANTHROPIC_API_KEY":         true,
		"ANTHROPIC_AUTH_TOKEN":      true,
		"ANTHROPIC_BASE_URL":        true,
		"ANTHROPIC_FOUNDRY_API_KEY": true,
		"CLAUDE_CODE_USE_BEDROCK":   true,
		"CLAUDE_CODE_USE_VERTEX":    true,
		"CLAUDE_CODE_USE_FOUNDRY":   true,
		"CLAUDE_CODE_EXTRA_BODY":    true,
		"CLAUDE_CODE_EFFORT_LEVEL":  true,
		"CLAUDE_CONFIG_DIR":         true,
	}
	env := make([]string, 0, 32)
	for _, kv := range os.Environ() {
		key, _, ok := strings.Cut(kv, "=")
		if ok && drop[key] {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"HOME="+tmp,
		"CLAUDE_CONFIG_DIR="+r.ConfigDir,
		"ENABLE_TOOL_SEARCH=false",
		"CLAUDE_CODE_MAX_RETRIES=0",
		"DISABLE_AUTO_COMPACT=1",
		"DISABLE_COMPACT=1",
		"CLAUDE_CODE_TOTAL_TOKENS_REMINDER=off",
	)
	if maxTokens > 0 {
		env = append(env, "CLAUDE_CODE_MAX_OUTPUT_TOKENS="+strconv.Itoa(maxTokens))
	}
	return env
}
