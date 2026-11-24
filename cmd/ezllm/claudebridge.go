package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
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
// Decisions: docs/design/claude-subscription-design.md.
const (
	envBridge            = "EZLLM_CLAUDE_BRIDGE"             // "on" enables the surface
	envBridgeBin         = "EZLLM_CLAUDE_BIN"                // official CLI binary (mounted)
	envBridgeDir         = "EZLLM_CLAUDE_HOME"               // subscription login directory (the CLI's CLAUDE_CONFIG_DIR)
	envBridgeAddr        = "EZLLM_CLAUDE_BRIDGE_ADDR"        // default 127.0.0.1:8790
	envBridgeToken       = "EZLLM_CLAUDE_BRIDGE_TOKEN_FILE"  // shared secret for the account key
	envClaudeConcurrency = "EZLLM_CLAUDE_BRIDGE_CONCURRENCY" // concurrent CLI processes (default 1)
)

// claudeQualifiedVersion is the CLI release the bridge's measurements and live
// proof were taken on. Any other version boots and serves, reported unqualified
// until the updater's live proof records it (bridgecheck.go releaseState).
const claudeQualifiedVersion = "2.1.306"

// claudeVersion is the release number from `claude --version`
// ("2.1.306 (Claude Code)").
func claudeVersion(raw string) string {
	if f := strings.Fields(raw); len(f) > 0 {
		return f[0]
	}
	return raw
}

// flags this bridge depends on. Asserted at boot (claudeCLICheck): a CLI release
// may drop one, and silent drift here breaks every request.
var claudeRequiredFlags = []string{
	"--input-format", "--output-format", "--include-partial-messages",
	"--setting-sources", "--system-prompt-file", "--strict-mcp-config",
	"--permission-mode", "--no-session-persistence", "--session-id", "--max-turns",
}

// bridgeReport tells the dashboard a bridge is running (server.EnableBridge):
// its address, the secret its account key carries, its boot status, and a
// live admission snapshot (in flight, queued, refused) read on each
// /admin/bridges call. Nil in tests that only exercise the boot checks.
type bridgeReport func(id, addr, token, updateFile string, info map[string]any, runtime func() map[string]any)

// startClaudeBridge brings up the loopback surface when enabled. Enabling it
// without a usable CLI or login is a boot failure, not a silent no-op: this
// process would otherwise look healthy while every subscription request 503s.
func startClaudeBridge(ctx context.Context, log *slog.Logger, report bridgeReport) (func(), error) {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv(envBridge)), "on") {
		return func() {}, nil
	}
	bin := envOrDefault(envBridgeBin, defaultClaudeBin)
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return nil, missingCLI("claude", envBridgeBin, bin, "Claude Code", envBridge, err)
	}
	dir := claudeHome(os.Getenv)
	addr := strings.TrimSpace(os.Getenv(envBridgeAddr))
	if addr == "" {
		addr = "127.0.0.1:8790"
	}
	if err := assertLoopback(addr); err != nil {
		return nil, err
	}

	cliVersion, drift := claudeCLICheck(resolved)
	release := releaseState(bin, claudeVersion(cliVersion), claudeQualifiedVersion)
	login, plan := claudeLoginCheck(resolved, dir)
	token, err := bridgeTokenFromFile(strings.TrimSpace(os.Getenv(envBridgeToken)), "claude")
	if err != nil {
		return nil, err
	}
	concurrency, err := bridgeConcurrency(os.Getenv, envClaudeConcurrency, "claude")
	if err != nil {
		return nil, err
	}
	queue, err := bridgeQueue(os.Getenv, "claude")
	if err != nil {
		return nil, err
	}

	runner := &claudebridge.Runner{
		Bin:           resolved,
		ConfigDir:     dir,
		Timeout:       300 * time.Second,
		MaxConcurrent: concurrency,
		MaxQueue:      queue,
		Logf:          log.Info,
	}
	srv := &claudebridge.Server{
		Runner:  runner,
		Token:   token,
		Version: server.Version,
		Info: map[string]any{
			"status":            "ok",
			"version":           server.Version,
			"cli_bin":           resolved,
			"cli_version":       cliVersion,
			"cli_release":       release,
			"claude_config_dir": dir,
			"login":             login,
			"subscription":      plan,
			"tools":             "client tools pass through; never executed by the bridge (M2)",
		},
		Logf:   log.Info,
		Limits: &claudebridge.Limits{},
	}

	if report != nil {
		report("claude-sub", addr, token, bridgeUpdateFile(bin), srv.Info, runner.AdmissionStats)
	}
	log.Info("claude bridge listening", "addr", addr, "cli", cliVersion, "release", release, "login", login, "plan", plan, "flags_drift", drift)
	httpSrv := serveBridge(log, "claude", addr, srv.Handler())
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
// depends on. `--help` cannot answer that — it omits --max-turns and
// --system-prompt-file while the parser accepts them (measured on 2.1.299 and
// 2.1.306) — so each flag goes to the parser itself: `-p FLAG` with no prompt
// and stdin at EOF fails before any network call, with "unknown option" only
// when the flag is gone. Flag drift is reported, not fatal, at boot: the surface
// can still serve, and the per-request behaviour is exercised by the live proof.
func claudeCLICheck(bin string) (version, drift string) {
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		return "unavailable", "cannot run --version"
	}
	version = strings.TrimSpace(string(out))
	var missing []string
	for _, f := range claudeRequiredFlags {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		out, err := exec.CommandContext(ctx, bin, "-p", f).CombinedOutput()
		timedOut := ctx.Err() != nil
		cancel()
		if err == nil || timedOut || strings.Contains(string(out), "unknown option '"+f+"'") {
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

// claudeHome is the bridge's login directory, EZLLM_CLAUDE_HOME like every
// other bridge's *_HOME (handed to the CLI as CLAUDE_CONFIG_DIR).
func claudeHome(get func(string) string) string {
	if v := strings.TrimSpace(get(envBridgeDir)); v != "" {
		return v
	}
	return "/claude-sub"
}
