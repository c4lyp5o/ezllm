package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/agybridge"
	"github.com/c4lyp5o/ezllm/internal/server"
)

// Antigravity subscription surface: agybridge's OpenAI Chat Completions surface
// on a LOOPBACK-ONLY listener, dialled by an ordinary `openai-compatible`
// account, while the official Antigravity CLI (`agy`) — bind-mounted from the
// host — stays the only thing that talks to the Google subscription.
// Decisions: docs/antigravity-subscription-design.md; operation:
// docs/agy-bridge-runbook.md.
const (
	envAgyBridge       = "EZLLM_AGY_BRIDGE"            // "on" enables the surface
	envAgyBin          = "EZLLM_AGY_BIN"               // official CLI binary (updater layout: /opt/agy-bridge/current/agy)
	envAgyHome         = "EZLLM_AGY_HOME"              // HOME holding the login's token file
	envAgyAddr         = "EZLLM_AGY_BRIDGE_ADDR"       // default 127.0.0.1:8792
	envAgyToken        = "EZLLM_AGY_BRIDGE_TOKEN_FILE" // shared secret for the account key
	envAgyConcurrency  = "EZLLM_AGY_BRIDGE_CONCURRENCY"
	envAgyTools        = "EZLLM_AGY_EXPERIMENTAL_TOOLS"
	envAgySessionCache = "EZLLM_AGY_BRIDGE_CACHE"
)

// startAgyBridge brings up the loopback surface when enabled. A missing binary
// or login, or a CLI that dropped a flag this bridge needs, is a boot failure.
func startAgyBridge(ctx context.Context, log *slog.Logger, report bridgeReport) (func(), error) {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv(envAgyBridge)), "on") {
		return func() {}, nil
	}
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("agy bridge: %s=on is a Linux feature (this build is %s); run ezllm in a Linux container — docs/agy-bridge-runbook.md", envAgyBridge, runtime.GOOS)
	}
	bin := envOrDefault(envAgyBin, defaultAgyBin)
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("agy bridge: %s=%q does not resolve to the official Antigravity CLI: %w", envAgyBin, bin, err)
	}
	home := envOrDefault(envAgyHome, "/agy-sub")
	addr := strings.TrimSpace(os.Getenv(envAgyAddr))
	if addr == "" {
		addr = "127.0.0.1:8792"
	}
	if err := assertAgyLoopback(addr); err != nil {
		return nil, err
	}
	// ~207 MB RSS per CLI process (measured, 1.4.7).
	concurrency, err := bridgeConcurrency(os.Getenv, envAgyConcurrency, "agy")
	if err != nil {
		return nil, err
	}
	queue, err := bridgeQueue(os.Getenv, "agy")
	if err != nil {
		return nil, err
	}

	pre, err := agybridge.RunPreflight(ctx, resolved, home)
	if err != nil {
		return nil, err
	}
	experimentalTools := strings.EqualFold(strings.TrimSpace(os.Getenv(envAgyTools)), "on")
	if experimentalTools && !pre.StructuredOutput {
		return nil, fmt.Errorf("agy bridge: %s=on requires a CLI with --json-schema", envAgyTools)
	}
	sessionCache, err := agySessionCache(os.Getenv, experimentalTools, pre.ConversationResume)
	if err != nil {
		return nil, err
	}
	clientTools := "disabled (text chat only)"
	if experimentalTools {
		clientTools = "experimental (structured output; caller executes)"
	}
	token, err := bridgeTokenFromFile(strings.TrimSpace(os.Getenv(envAgyToken)), "agy")
	if err != nil {
		return nil, err
	}
	qualified := releaseState(bin, pre.Version, "")
	ids := make([]string, 0, len(pre.Models))
	for _, m := range pre.Models {
		ids = append(ids, m.ID)
	}

	runner := &agybridge.Runner{Bin: resolved, Home: home, Timeout: 300 * time.Second, MaxConcurrent: concurrency, MaxQueue: queue, Logf: log.Info, SessionCache: sessionCache}
	srv := &agybridge.Server{
		ExperimentalTools: experimentalTools,
		Runner:            runner,
		Token:             token,
		Version:           server.Version,
		Models:            pre.Models,
		Info: map[string]any{
			"status":        "ok",
			"version":       server.Version,
			"cli_bin":       resolved,
			"cli_version":   pre.Version,
			"cli_release":   qualified,
			"agy_home":      home,
			"login":         "logged in (token file; models listed at boot)",
			"models":        strings.Join(ids, ", "),
			"concurrency":   concurrency,
			"queue":         queue,
			"client_tools":  clientTools,
			"session_cache": sessionCache,
		},
		Logf: log.Info,
	}

	if report != nil {
		report("antigravity-sub", addr, token, bridgeUpdateFile(bin), srv.Info, runner.AdmissionStats)
	}
	log.Info("agy bridge listening", "addr", addr, "cli", pre.Version, "release", qualified, "models", len(pre.Models))
	httpSrv := serveBridge(log, "agy", addr, srv.Handler())
	return func() { shutdownBridgeHTTP(httpSrv); runner.Close() }, nil
}

// assertAgyLoopback keeps this surface invisible from outside the container:
// it fronts the subscription login and has one consumer (this process).
func assertAgyLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("agy bridge: bad %s %q: %w", envAgyAddr, addr, err)
	}
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return nil
	}
	return fmt.Errorf("agy bridge: %s=%q must be loopback (127.0.0.1): this surface holds the subscription login", envAgyAddr, addr)
}

// agySessionCache is on by default wherever it can work: with the tool adapter
// on and a CLI that resumes conversations. An explicit "on" that cannot work
// fails the boot instead of silently serving uncached.
func agySessionCache(get func(string) string, tools, resume bool) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(get(envAgySessionCache))) {
	case "":
		return tools && resume, nil
	case "off", "false", "0":
		return false, nil
	case "on", "true", "1":
	default:
		return false, fmt.Errorf("agy bridge: %s must be on or off", envAgySessionCache)
	}
	if !tools {
		return false, fmt.Errorf("agy bridge: %s requires %s=on", envAgySessionCache, envAgyTools)
	}
	if !resume {
		return false, fmt.Errorf("agy bridge: %s=on requires --conversation", envAgySessionCache)
	}
	return true, nil
}
