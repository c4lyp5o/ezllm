package claudebridge

import (
	"context"
	"encoding/json"
	"github.com/c4lyp5o/ezllm/internal/bridgeutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
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

func (r *Runner) deadlineErr(err error, tb *bridgeutil.TailBuffer, at int) error {
	if err == context.DeadlineExceeded || err == context.Canceled {
		return &NativeError{Status: 504, Type: "api_error", Code: "timeout",
			Message: "claude-bridge: native run did not finish in time"}
	}
	msg := "claude-bridge: native output ended unexpectedly after " + strconv.Itoa(at) + " frames: " + err.Error()
	if tail := tb.Tail(); tail != "" {
		msg += " | stderr: " + tail
	}
	return &NativeError{Status: 502, Type: "api_error", Code: "native_closed", Message: msg}
}

// childEnv is the process environment for one native run. It is an ALLOWLIST
// of what the CLI needs to start and reach the network from a container —
// everything else in ezllm's environment (EZLLM_MASTER_KEY, provider and cloud
// credentials) stays out of a process that has no reason to see it.
//
// Within the allowlist, conflicting auth and backend overrides are still
// removed: the subscription login in ConfigDir is the only credential path, and
// an inherited ANTHROPIC_BASE_URL would silently redirect it. HOME points at
// the per-request temp dir so nothing reads or writes the operator's ~/.claude
// state.
func (r *Runner) childEnv(tmp string, maxTokens int) []string {
	env := make([]string, 0, 32)
	for _, kv := range os.Environ() {
		key, _, ok := strings.Cut(kv, "=")
		if ok && childEnvAllowed(key) {
			env = append(env, kv)
		}
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

// linkKeychains makes the operator's macOS keychains visible in the per-request
// HOME. The CLI keeps its login there on a Mac, and macOS finds keychains through
// HOME (measured: without the link every run answers "Not logged in"). Only this
// folder is linked, so the rest of the home stays out of reach. Other systems
// keep the login in ConfigDir and need nothing. RemoveAll drops the link without
// following it.
func linkKeychains(goos, home, tmp string) error {
	if goos != "darwin" || home == "" {
		return nil
	}
	keychains := filepath.Join(home, "Library", "Keychains")
	if _, err := os.Stat(keychains); err != nil {
		return nil // no keychain: the CLI reports the missing login itself
	}
	library := filepath.Join(tmp, "Library")
	if err := os.Mkdir(library, 0o700); err != nil {
		return err
	}
	return os.Symlink(keychains, filepath.Join(library, "Keychains"))
}

// childEnvPass are inherited verbatim: process identity (the CLI's login check
// reads USER/LOGNAME), locale, temp dir, CA trust and outbound proxy settings.
var childEnvPass = map[string]bool{
	"PATH": true, "USER": true, "LOGNAME": true, "SHELL": true, "TERM": true,
	"LANG": true, "LANGUAGE": true, "TZ": true, "TMPDIR": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "NODE_EXTRA_CA_CERTS": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true, "all_proxy": true,
}

// childEnvDrop are CLI settings that would replace the subscription login or
// override what childEnv sets itself — removed even though their prefix passes.
var childEnvDrop = map[string]bool{
	"CLAUDE_CODE_USE_BEDROCK":  true,
	"CLAUDE_CODE_USE_VERTEX":   true,
	"CLAUDE_CODE_USE_FOUNDRY":  true,
	"CLAUDE_CODE_EXTRA_BODY":   true,
	"CLAUDE_CODE_EFFORT_LEVEL": true,
}

// childEnvAllowed: the pass list, LC_* locale, and the operator's own CLI
// tuning (CLAUDE_CODE_*, DISABLE_*, e.g. DISABLE_TELEMETRY) minus the drop
// list. ANTHROPIC_* (API keys, base URL) and CLAUDE_CONFIG_DIR never pass.
func childEnvAllowed(key string) bool {
	switch {
	case childEnvPass[key], strings.HasPrefix(key, "LC_"):
		return true
	case childEnvDrop[key]:
		return false
	case strings.HasPrefix(key, "CLAUDE_CODE_"), strings.HasPrefix(key, "DISABLE_"):
		return true
	}
	return false
}
