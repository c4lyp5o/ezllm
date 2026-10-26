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
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/codexbridge"
	"github.com/c4lyp5o/ezllm/internal/server"
)

// ChatGPT subscription surface: codexbridge's OpenAI Chat Completions surface on
// a LOOPBACK-ONLY listener, dialled by an ordinary `openai-compatible` account,
// while the official Codex CLI — bind-mounted from the host — stays the only
// thing that talks to the subscription. Decisions:
// docs/codex-subscription-design.md; operation: docs/codex-bridge-runbook.md.
const (
	envCodexBridge      = "EZLLM_CODEX_BRIDGE"            // "on" enables the surface
	envCodexBin         = "EZLLM_CODEX_BIN"               // official CLI binary (mounted release: /opt/codex/bin/codex)
	envCodexHome        = "EZLLM_CODEX_HOME"              // isolated ChatGPT login (CODEX_HOME)
	envCodexAddr        = "EZLLM_CODEX_BRIDGE_ADDR"       // default 127.0.0.1:8791
	envCodexToken       = "EZLLM_CODEX_BRIDGE_TOKEN_FILE" // shared secret for the account key
	envCodexConcurrency = "EZLLM_CODEX_BRIDGE_CONCURRENCY"
	envCodexToolsNoLL   = "EZLLM_CODEX_TOOLS_WITHOUT_LANDLOCK" // "on" = client tools without Landlock
)

// codexTools decides client tools for this host (decision §4.7): with Landlock
// every child is confined and tools are on; without it they are off unless the
// operator overrides, and a missing code-mode host turns them off regardless.
type codexTools struct {
	sandbox  *codexbridge.Sandbox
	landlock string // /status: available (ABI n) | unavailable: reason
	status   string // /status: enabled | disabled (…) | enabled without Landlock (operator override)
	off      string // non-empty: why tool requests are refused
}

func decideCodexTools(helper, bin string, override bool) codexTools {
	var t codexTools
	abi, err := codexbridge.ProbeLandlock(helper)
	if err == nil {
		t.sandbox = &codexbridge.Sandbox{Helper: helper}
		t.landlock = "available (ABI " + strconv.Itoa(abi) + ")"
	} else {
		t.landlock = "unavailable: " + err.Error()
	}
	host := codexbridge.CodeModeHost(bin)
	switch {
	case !fileExists(host):
		t.status = "disabled (code-mode host missing)"
		t.off = "the code-mode host the CLI needs for tools is missing (" + host + ")"
	case err == nil:
		t.status = "enabled"
	case override:
		t.status = "enabled without Landlock (operator override)"
	default:
		t.status = "disabled (no Landlock)"
		t.off = "Landlock is unavailable (" + err.Error() + ")"
	}
	return t
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// startCodexBridge brings up the loopback surface when enabled. A missing
// binary or login, a lockdown flag the CLI refuses, or a protocol method it
// dropped is a boot failure, never a silent degrade.
func startCodexBridge(ctx context.Context, log *slog.Logger, report bridgeReport) (func(), error) {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv(envCodexBridge)), "on") {
		return func() {}, nil
	}
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("codex bridge: %s=on is a Linux feature (this build is %s); run ezllm in a Linux container — docs/codex-bridge-runbook.md", envCodexBridge, runtime.GOOS)
	}
	bin := strings.TrimSpace(os.Getenv(envCodexBin))
	if bin == "" {
		bin = "/opt/codex/bin/codex"
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("codex bridge: %s=%q does not resolve to the official Codex CLI: %w", envCodexBin, bin, err)
	}
	home := strings.TrimSpace(os.Getenv(envCodexHome))
	if home == "" {
		home = "/codex-sub"
	}
	addr := strings.TrimSpace(os.Getenv(envCodexAddr))
	if addr == "" {
		addr = "127.0.0.1:8791"
	}
	if err := assertCodexLoopback(addr); err != nil {
		return nil, err
	}
	concurrency := 2
	if v := strings.TrimSpace(os.Getenv(envCodexConcurrency)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("codex bridge: %s=%q must be a positive integer", envCodexConcurrency, v)
		}
		concurrency = n
	}

	pre, err := codexbridge.RunPreflight(ctx, resolved, home)
	if err != nil {
		return nil, err
	}
	token, err := codexBridgeToken(strings.TrimSpace(os.Getenv(envCodexToken)))
	if err != nil {
		return nil, err
	}
	helper, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("codex bridge: cannot locate this binary for the Landlock helper: %w", err)
	}
	tools := decideCodexTools(helper, resolved, strings.EqualFold(strings.TrimSpace(os.Getenv(envCodexToolsNoLL)), "on"))
	qualified := "qualified"
	if !pre.Qualified {
		qualified = "unqualified (measured on " + codexbridge.QualifiedVersion + "; re-run the live proof)"
	}

	runner := &codexbridge.Runner{
		Bin:           resolved,
		CodexHome:     home,
		Timeout:       300 * time.Second,
		MaxConcurrent: concurrency,
		Version:       server.Version,
		Logf:          log.Info,
		Channel:       pre.Channel,
		Sandbox:       tools.sandbox,
	}
	srv := &codexbridge.Server{
		Runner:   runner,
		ToolsOff: tools.off,
		Token:    token,
		Version:  server.Version,
		Info: map[string]any{
			"status":       "ok",
			"version":      server.Version,
			"cli_bin":      resolved,
			"cli_version":  pre.Version,
			"cli_release":  qualified,
			"codex_home":   home,
			"login":        pre.Login,
			"lockdown":     "verified at boot",
			"channel":      channelInfo(pre),
			"concurrency":  concurrency,
			"landlock":     tools.landlock,
			"client_tools": tools.status,
		},
		Logf: log.Info,
	}

	if report != nil {
		report("chatgpt-sub", addr, token, srv.Info)
	}
	httpSrv := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() {
		if pre.Channel == codexbridge.ChannelExec {
			log.Warn("codex bridge on the exec fallback: no streaming deltas, no catalog, no plan windows",
				"missing", strings.Join(pre.MissingRPCs, ","))
		}
		log.Info("codex bridge listening", "addr", addr, "cli", pre.Version, "release", qualified, "login", pre.Login,
			"channel", pre.Channel, "landlock", tools.landlock, "client_tools", tools.status)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("codex bridge serve", "err", err)
		}
	}()
	// main calls stop after the gateway drain (its handlers proxy here):
	// drain this surface, then reap the parked tool turns that outlive any
	// HTTP request by design.
	return func() {
		shutdownBridgeHTTP(httpSrv)
		runner.Shutdown()
	}, nil
}

func channelInfo(p codexbridge.Preflight) string {
	if p.Channel == codexbridge.ChannelExec {
		return "exec fallback (app-server lacks " + strings.Join(p.MissingRPCs, ", ") + "): answers arrive as one chunk; /v1/models and /v1/usage answer 404"
	}
	return "app-server"
}

// assertCodexLoopback keeps this surface invisible from outside the container:
// it fronts the subscription login and has one consumer (this process).
func assertCodexLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("codex bridge: bad %s %q: %w", envCodexAddr, addr, err)
	}
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return nil
	}
	return fmt.Errorf("codex bridge: %s=%q must be loopback (127.0.0.1): this surface holds the subscription login", envCodexAddr, addr)
}

// codexBridgeToken returns the shared secret the account key carries. The
// default lives on the data volume so it is backed up with the ledger.
func codexBridgeToken(path string) (string, error) {
	if path == "" {
		path = "/data/codex-bridge.token"
	}
	if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) != "" {
		return strings.TrimSpace(string(b)), nil
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("codex bridge: cannot generate token: %w", err)
	}
	token := hex.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("codex bridge: cannot write %s: %w", path, err)
	}
	return token, nil
}
