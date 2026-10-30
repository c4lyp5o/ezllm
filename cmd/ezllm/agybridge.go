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
	envAgyBridge      = "EZLLM_AGY_BRIDGE"            // "on" enables the surface
	envAgyBin         = "EZLLM_AGY_BIN"               // official CLI binary (mounted: /opt/agy/agy)
	envAgyHome        = "EZLLM_AGY_HOME"              // HOME holding the login's token file
	envAgyAddr        = "EZLLM_AGY_BRIDGE_ADDR"       // default 127.0.0.1:8792
	envAgyToken       = "EZLLM_AGY_BRIDGE_TOKEN_FILE" // shared secret for the account key
	envAgyConcurrency = "EZLLM_AGY_BRIDGE_CONCURRENCY"
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
	bin := envOrDefault(envAgyBin, "/opt/agy/agy")
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
	// ~207 MB RSS per CLI process (measured, 1.4.7): the default stays low.
	concurrency := 2
	if v := strings.TrimSpace(os.Getenv(envAgyConcurrency)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("agy bridge: %s=%q must be a positive integer", envAgyConcurrency, v)
		}
		concurrency = n
	}

	pre, err := agybridge.RunPreflight(ctx, resolved, home)
	if err != nil {
		return nil, err
	}
	token, err := agyBridgeToken(strings.TrimSpace(os.Getenv(envAgyToken)))
	if err != nil {
		return nil, err
	}
	qualified := releaseState(bin, pre.Version, agybridge.QualifiedVersion)
	ids := make([]string, 0, len(pre.Models))
	for _, m := range pre.Models {
		ids = append(ids, m.ID)
	}

	srv := &agybridge.Server{
		Runner: &agybridge.Runner{
			Bin:           resolved,
			Home:          home,
			Timeout:       300 * time.Second,
			MaxConcurrent: concurrency,
			Logf:          log.Info,
		},
		Token:   token,
		Version: server.Version,
		Models:  pre.Models,
		Info: map[string]any{
			"status":       "ok",
			"version":      server.Version,
			"cli_bin":      resolved,
			"cli_version":  pre.Version,
			"cli_release":  qualified,
			"agy_home":     home,
			"login":        "logged in (token file; models listed at boot)",
			"models":       strings.Join(ids, ", "),
			"concurrency":  concurrency,
			"client_tools": "disabled (text chat only)",
		},
		Logf: log.Info,
	}

	if report != nil {
		report("antigravity-sub", addr, token, srv.Info)
	}
	httpSrv := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() {
		log.Info("agy bridge listening", "addr", addr, "cli", pre.Version, "release", qualified, "models", len(pre.Models))
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("agy bridge serve", "err", err)
		}
	}()
	return func() { shutdownBridgeHTTP(httpSrv) }, nil
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

// agyBridgeToken returns the shared secret the account key carries. The
// default lives on the data volume so it is backed up with the ledger.
func agyBridgeToken(path string) (string, error) {
	if path == "" {
		path = "/data/agy-bridge.token"
	}
	if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) != "" {
		return strings.TrimSpace(string(b)), nil
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("agy bridge: cannot generate token: %w", err)
	}
	token := hex.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("agy bridge: cannot write %s: %w", path, err)
	}
	return token, nil
}
