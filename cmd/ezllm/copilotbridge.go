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
	"strconv"
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
	envCopilotBridge      = "EZLLM_COPILOT_BRIDGE"             // "on" enables the surface
	envCopilotBin         = "EZLLM_COPILOT_BIN"                // official CLI binary (mounted)
	envCopilotHome        = "EZLLM_COPILOT_HOME"               // the CLI's own login directory
	envCopilotBridgeAddr  = "EZLLM_COPILOT_BRIDGE_ADDR"        // default 127.0.0.1:8793
	envCopilotBridgeToken = "EZLLM_COPILOT_BRIDGE_TOKEN_FILE"  // shared secret for the account key
	envCopilotConcurrency = "EZLLM_COPILOT_BRIDGE_CONCURRENCY" // concurrent CLI children (default 2)
	envCopilotQueue       = "EZLLM_COPILOT_BRIDGE_QUEUE"       // requests allowed to wait for a slot (default 8)
	envCopilotTimeout     = "EZLLM_COPILOT_BRIDGE_TIMEOUT"     // per-turn limit, a Go duration (default 300s)
)

// copilotLimits reads the three runner knobs. A malformed value fails the boot:
// a typo that silently fell back to the default would look like a working
// override.
func copilotLimits(get func(string) string) (concurrency, queue int, timeout time.Duration, err error) {
	concurrency, queue, timeout = 2, 8, 300*time.Second
	for _, k := range []struct {
		env string
		dst *int
	}{{envCopilotConcurrency, &concurrency}, {envCopilotQueue, &queue}} {
		if v := strings.TrimSpace(get(k.env)); v != "" {
			n, perr := strconv.Atoi(v)
			if perr != nil || n < 1 {
				return 0, 0, 0, fmt.Errorf("copilot bridge: %s=%q must be a positive integer", k.env, v)
			}
			*k.dst = n
		}
	}
	if v := strings.TrimSpace(get(envCopilotTimeout)); v != "" {
		d, perr := time.ParseDuration(v)
		if perr != nil || d < time.Second {
			return 0, 0, 0, fmt.Errorf("copilot bridge: %s=%q must be a duration of at least 1s (e.g. 300s)", envCopilotTimeout, v)
		}
		timeout = d
	}
	return concurrency, queue, timeout, nil
}

// copilotQualifiedVersion is the CLI release the bridge's parser fixture and live
// proof were taken on (measured 2026-11-05). Another version boots and serves,
// reported unqualified until the updater's live proof records it.
const copilotQualifiedVersion = "1.0.105"

// copilotModelIDs is the built-in copy of the ids `copilot help config` documents
// (CLI 1.0.105): the catalog the bridge falls back to when the live read fails.
// There is no non-interactive "list models" command and the help text does not
// say which ids a plan includes, so the catalog is every id the CLI KNOWS and
// the plan decides at request time: a model the login lacks fails with a typed
// 403 model_not_in_plan, never a silent success. "auto" MUST stay first: the key
// test probes the first candidate, and "auto" is the one id every plan answers.
var copilotModelIDs = []string{
	"auto", // displayed as "Copilot Fusion"; the stored id is "auto"
	"gpt-6.1-sol", "gpt-6-sol", "gpt-6-luna", "gpt-6-astra",
	"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "gpt-5.4", "gpt-5.4-mini",
	"gpt-5.3-codex", "gpt-5-mini",
	"claude-sonnet-5.5", "claude-sonnet-5", "claude-fable-5.1", "claude-fable-5",
	"claude-opus-5.5", "claude-opus-5", "claude-opus-4.8", "claude-opus-4.8-fast",
	"claude-sonnet-4.6", "claude-haiku-5.5", "claude-haiku-4.5",
	"mai-code-1.1-flash", "gemini-4-argon", "gemini-3.8-flash", "gemini-3.7-flash",
	"grok-4.7", "grok-4.6", "grok-4.5", "kimi-k3",
}

// parseHelpConfigModels reads the model ids out of `copilot help config`: the
// quoted values under the `model` setting, up to the next setting. A CLI whose
// help text changes shape yields nothing, and the built-in list stands.
func parseHelpConfigModels(out string) []string {
	var ids []string
	in := false
	for _, line := range strings.Split(out, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "`model`:") {
			in = true
			continue
		}
		if in && strings.HasPrefix(trim, "`") {
			break // the next setting
		}
		if in && strings.HasPrefix(trim, "- \"") && strings.HasSuffix(trim, "\"") {
			id := strings.Trim(strings.TrimPrefix(trim, "- "), "\"")
			if id != "" && !strings.ContainsAny(id, " \t") {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// copilotCatalog is `auto`, then the ids the CLI documents (read live so a CLI
// update is picked up at the next boot), then any built-in id the live read did
// not return. Order is stable and duplicates are dropped.
func copilotCatalog(live []string) []string {
	out := []string{"auto"}
	seen := map[string]bool{"auto": true}
	for _, list := range [][]string{live, copilotModelIDs} {
		for _, id := range list {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

// copilotLiveModels runs `copilot help config` (a local help page: no network,
// no premium request) and parses the documented model ids.
func copilotLiveModels(bin string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "help", "config").Output()
	if err != nil {
		return nil
	}
	return parseHelpConfigModels(string(out))
}

// printfLogger adapts a slog logger to the bridge's printf-style Logf. Passing
// log.Info directly treats the format string as the message and every argument
// as a key, which logged "session=%s ... !BADKEY=..." for every turn.
func printfLogger(log *slog.Logger) func(string, ...any) {
	return func(format string, args ...any) { log.Info(fmt.Sprintf(format, args...)) }
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

	concurrency, queue, timeout, err := copilotLimits(os.Getenv)
	if err != nil {
		return nil, err
	}
	cliVersion := copilotCLIVersion(resolved)
	release := releaseState(bin, cliVersion, copilotQualifiedVersion)
	token, err := bridgeTokenFromFile(strings.TrimSpace(os.Getenv(envCopilotBridgeToken)), "copilot")
	if err != nil {
		return nil, err
	}

	catalog := copilotCatalog(copilotLiveModels(resolved))
	models := make([]copilotbridge.Model, 0, len(catalog))
	for _, id := range catalog {
		models = append(models, copilotbridge.Model{ID: id, Object: "model", OwnedBy: "copilot-cli"})
	}
	info := map[string]any{
		"status":      "ok",
		"version":     server.Version,
		"cli_bin":     resolved,
		"cli_version": cliVersion,
		"cli_release": release,
		"home":        home,
		"concurrency": concurrency,
		"queue":       queue,
		"timeout":     timeout.String(),
		"models":      catalog,
		// Measured cost of the CLI's own agent scaffolding, so nobody reads the
		// ledger and thinks the bridge is leaking tokens.
		"scaffolding_tokens_per_call": "~13-14k (27 built-in tools ≈ 9.3k)",
		"auth":                        "the CLI's stored login in COPILOT_HOME; no token in ezllm",
	}
	srv := &copilotbridge.Server{
		Runner: &copilotbridge.Runner{
			Bin:           resolved,
			Home:          home,
			Timeout:       timeout,
			MaxConcurrent: concurrency,
			MaxQueue:      queue,
			Logf:          printfLogger(log),
		},
		Token:   token,
		Version: server.Version,
		Models:  models,
		Info:    info,
		Logf:    printfLogger(log),
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
