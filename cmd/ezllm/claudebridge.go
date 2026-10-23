package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/claudebridge"
	"github.com/c4lyp5o/ezllm/internal/server"
)

// Claude subscription surface. One process, one container: this serves
// claudebridge's Anthropic Messages surface on a LOOPBACK-ONLY listener so an
// ordinary `anthropic-compatible` account can dial it, while the official Claude
// Code CLI — bind-mounted from the host — stays the only thing that talks to the
// subscription. No second service, no second image, no inter-service token.
// Decisions: docs/claude-subscription-design.md.
const (
	envBridge      = "EZLLM_CLAUDE_BRIDGE"            // "on" enables the surface
	envBridgeBin   = "EZLLM_CLAUDE_BIN"               // official CLI binary (mounted)
	envBridgeDir   = "EZLLM_CLAUDE_CONFIG_DIR"        // subscription login directory
	envBridgeAddr  = "EZLLM_CLAUDE_BRIDGE_ADDR"       // default 127.0.0.1:8790
	envBridgeToken = "EZLLM_CLAUDE_BRIDGE_TOKEN_FILE" // shared secret for the account key
)

// flags this bridge depends on. Asserted at boot: a CLI release may drop one
// while it still "works" (measured on 2.1.299: --max-turns and --system-prompt-file
// are accepted but absent from --help), and silent drift here breaks every request.
var claudeRequiredFlags = []string{
	"--input-format", "--output-format", "--include-partial-messages",
	"--setting-sources", "--system-prompt-file", "--strict-mcp-config",
	"--permission-mode", "--no-session-persistence", "--session-id", "--max-turns",
}

// startClaudeBridge brings up the loopback surface when enabled. Enabling it
// without a usable CLI or login is a boot failure, not a silent no-op: this
// process would otherwise look healthy while every subscription request 503s.
func startClaudeBridge(ctx context.Context, log *slog.Logger) (func(), error) {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv(envBridge)), "on") {
		return func() {}, nil
	}
	bin := strings.TrimSpace(os.Getenv(envBridgeBin))
	if bin == "" {
		bin = "/opt/claude/claude"
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("claude bridge: %s=%q does not resolve to the official Claude Code CLI: %w", envBridgeBin, bin, err)
	}
	dir := strings.TrimSpace(os.Getenv(envBridgeDir))
	if dir == "" {
		dir = "/claude-sub"
	}
	addr := strings.TrimSpace(os.Getenv(envBridgeAddr))
	if addr == "" {
		addr = "127.0.0.1:8790"
	}
	if err := assertLoopback(addr); err != nil {
		return nil, err
	}

	cliVersion, drift := claudeCLICheck(resolved)
	login, plan := claudeLoginCheck(resolved, dir)
	token, err := claudeBridgeToken(strings.TrimSpace(os.Getenv(envBridgeToken)))
	if err != nil {
		return nil, err
	}

	srv := &claudebridge.Server{
		Runner: &claudebridge.Runner{
			Bin:           resolved,
			ConfigDir:     dir,
			Timeout:       300 * time.Second,
			MaxConcurrent: 2,
			Logf:          log.Info,
		},
		Token:   token,
		Version: server.Version,
		Info: map[string]any{
			"status":            "ok",
			"version":           server.Version,
			"cli_bin":           resolved,
			"cli_version":       cliVersion,
			"claude_config_dir": dir,
			"login":             login,
			"subscription":      plan,
			"tools":             "client tools pass through; never executed by the bridge (M2)",
		},
		Logf:   log.Info,
		Limits: &claudebridge.Limits{},
	}

	httpSrv := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() {
		log.Info("claude bridge listening", "addr", addr, "cli", cliVersion, "login", login, "plan", plan, "flags_drift", drift)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("claude bridge serve", "err", err)
		}
	}()
	return func() { shutdownBridgeHTTP(httpSrv) }, nil
}

// assertLoopback keeps this surface invisible from outside the container: it
// holds the subscription login and is meant for one consumer (this process).
func assertLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("claude bridge: bad %s %q: %w", envBridgeAddr, addr, err)
	}
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return nil
	}
	return fmt.Errorf("claude bridge: %s=%q must be loopback (127.0.0.1): this surface holds the subscription login", envBridgeAddr, addr)
}

// claudeCLICheck proves the binary runs and still accepts every flag the design
// depends on. Flag drift is reported, not fatal: the surface can still serve, and
// the per-request behaviour is exercised by the live proof.
func claudeCLICheck(bin string) (version, drift string) {
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		return "unavailable", "cannot run --version"
	}
	version = strings.TrimSpace(string(out))
	help, err := exec.Command(bin, "--help").CombinedOutput()
	if err != nil {
		return version, "cannot run --help"
	}
	var missing []string
	for _, f := range claudeRequiredFlags {
		if !strings.Contains(string(help), f) {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		drift = strings.Join(missing, ",")
	}
	return version, drift
}

// claudeLoginCheck reports the subscription login state. The CLI answers
// "logged out" instantly (no upstream call) when its process identity is wrong,
// so USER and LOGNAME must exist — checked here so a container misconfig fails
// the boot instead of every request.
func claudeLoginCheck(bin, dir string) (state, plan string) {
	if os.Getenv("USER") == "" || os.Getenv("LOGNAME") == "" {
		return "misconfigured", "USER and LOGNAME must be set (the CLI's login check reads process identity)"
	}
	if _, err := os.Stat(filepath.Join(dir, ".credentials.json")); err != nil {
		return "logged-out", "no subscription login in " + dir
	}
	cmd := exec.Command(bin, "auth", "status")
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "unverified", "claude auth status failed"
	}
	state, plan = "unknown", "unknown"
	for _, line := range strings.Split(string(out), "\n") {
		l := strings.ToLower(strings.TrimSpace(line))
		switch {
		case strings.Contains(l, "logged"):
			switch {
			case strings.Contains(l, "not logged") || strings.Contains(l, "false"):
				state = "logged-out"
			case strings.Contains(l, "true") || strings.Contains(l, "as ") ||
				strings.Contains(l, "oauth") || strings.Contains(l, "claude.ai"):
				state = "logged-in"
			}
		case strings.Contains(l, "subscription"):
			plan = strings.TrimSpace(line)
		}
	}
	return state, plan
}

// claudeBridgeToken returns the shared secret the account key carries. Like the
// master key, the default lives on the data volume so it is backed up with the
// ledger and survives a container rebuild.
func claudeBridgeToken(path string) (string, error) {
	if path == "" {
		path = "/data/claude-bridge.token"
	}
	if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) != "" {
		return strings.TrimSpace(string(b)), nil
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("claude bridge: cannot generate token: %w", err)
	}
	token := hex.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("claude bridge: cannot write %s: %w", path, err)
	}
	return token, nil
}
