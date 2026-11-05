package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/copilotbridge"
	"github.com/c4lyp5o/ezllm/internal/server"
)

// GitHub Copilot subscription surface: the official Copilot CLI behind a
// LOOPBACK-ONLY listener, exactly like the claude/codex/agy bridges. The CLI's
// own login (a dedicated COPILOT_HOME) is the only credential; ezllm never
// speaks Copilot's private API and never mints a session token — that OAuth
// path is retired by this change (docs/copilot-cli-bridge-design.md).
const (
	envCopilotBridge      = "EZLLM_COPILOT_BRIDGE"            // "on" enables the surface
	envCopilotBin         = "EZLLM_COPILOT_BIN"               // official CLI binary (mounted)
	envCopilotHome        = "EZLLM_COPILOT_HOME"              // the CLI's own login directory
	envCopilotBridgeAddr  = "EZLLM_COPILOT_BRIDGE_ADDR"       // default 127.0.0.1:8793
	envCopilotBridgeToken = "EZLLM_COPILOT_BRIDGE_TOKEN_FILE" // shared secret for the account key
)

// copilotQualifiedVersion is the CLI release the bridge's parser fixture and live
// proof were taken on (measured 2026-11-05). Another version boots and serves,
// reported unqualified until the updater's live proof records it.
const copilotQualifiedVersion = "1.0.105"

// copilotModelIDs is the CURATED catalog. There is no non-interactive "list
// models" command, so these are the ids the CLI documents plus the ones this
// login actually served ("gpt-6.1-sol" default, "mai-code-1.1-flash" chosen by
// `--model auto`). A hop is only proven after a probe answers; the curated list
// seeds routing and nothing more.
var copilotModelIDs = []string{
	"auto",
	"gpt-6.1-sol",
	"gpt-5.3-codex",
	"mai-code-1.1-flash",
	"claude-haiku-4.5",
}

func startCopilotBridge(ctx context.Context, log *slog.Logger, report bridgeReport) (func(), error) {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv(envCopilotBridge)), "on") {
		return func() {}, nil
	}
	bin := envOrDefault(envCopilotBin, "/opt/copilot/copilot")
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("copilot bridge: %s=%q does not resolve to the official Copilot CLI: %w", envCopilotBin, bin, err)
	}
	home := envOrDefault(envCopilotHome, "/copilot-home")
	if st, err := os.Stat(home); err != nil || !st.IsDir() {
		// A missing login directory is a boot failure, not a lazy 401 later: the
		// surface would look healthy while every request failed.
		return nil, fmt.Errorf("copilot bridge: %s=%q is not a directory — mount the CLI's login home there", envCopilotHome, home)
	}
	addr := strings.TrimSpace(os.Getenv(envCopilotBridgeAddr))
	if addr == "" {
		addr = "127.0.0.1:8793"
	}
	if err := assertLoopback(addr); err != nil {
		return nil, err
	}

	cliVersion := copilotCLIVersion(resolved)
	release := releaseState(bin, cliVersion, copilotQualifiedVersion)
	token, err := bridgeTokenFromFile(strings.TrimSpace(os.Getenv(envCopilotBridgeToken)), "copilot")
	if err != nil {
		return nil, err
	}

	models := make([]copilotbridge.Model, 0, len(copilotModelIDs))
	for _, id := range copilotModelIDs {
		models = append(models, copilotbridge.Model{ID: id, Object: "model", OwnedBy: "copilot-cli"})
	}
	info := map[string]any{
		"status":      "ok",
		"version":     server.Version,
		"cli_bin":     resolved,
		"cli_version": cliVersion,
		"cli_release": release,
		"home":        home,
		"models":      copilotModelIDs,
		// Measured cost of the CLI's own agent scaffolding, so nobody reads the
		// ledger and thinks the bridge is leaking tokens.
		"scaffolding_tokens_per_call": "~13-14k (27 built-in tools ≈ 9.3k)",
		"auth":                        "the CLI's stored login in COPILOT_HOME; no token in ezllm",
	}
	srv := &copilotbridge.Server{
		Runner: &copilotbridge.Runner{
			Bin:           resolved,
			Home:          home,
			Timeout:       300 * time.Second,
			MaxConcurrent: 2,
			Logf:          log.Info,
		},
		Token:   token,
		Version: server.Version,
		Models:  models,
		Info:    info,
		Logf:    log.Info,
	}
	if report != nil {
		report("github-copilot", addr, token, bridgeUpdateFile(bin), info)
	}
	httpSrv := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() {
		log.Info("copilot bridge listening", "addr", addr, "cli", cliVersion, "release", release, "home", home)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("copilot bridge serve", "err", err)
		}
	}()
	return func() { shutdownBridgeHTTP(httpSrv) }, nil
}

// copilotCLIVersion reads `copilot --version` ("GitHub Copilot CLI 1.0.105.").
func copilotCLIVersion(bin string) string {
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return "unknown"
	}
	for _, f := range strings.Fields(string(out)) {
		if f[0] >= '0' && f[0] <= '9' {
			return strings.TrimSuffix(f, ".")
		}
	}
	return strings.TrimSpace(string(out))
}

// bridgeTokenFromFile mints (once) and reads the loopback shared secret, 0600.
// The account key ezllm stores is this value; nothing else authenticates the
// bridge surface.
func bridgeTokenFromFile(path, label string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("%s bridge: a token file path is required so the account key can authenticate the surface", label)
	}
	if b, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			return t, nil
		}
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("%s bridge: mint token: %w", label, err)
	}
	token := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("%s bridge: token dir: %w", label, err)
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("%s bridge: write token: %w", label, err)
	}
	return token, nil
}
