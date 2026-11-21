package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/c4lyp5o/ezllm/internal/bridgeutil"
)

// Default CLI binaries: the updater-managed layout (scripts/bridge_update.py
// BRIDGES), <bridge-root>/current/<bin>, so a compose that mounts the bridge
// root at /opt/<name>-bridge needs no *_BIN override.
const (
	defaultClaudeBin  = "/opt/claude-bridge/current/claude"
	defaultCodexBin   = "/opt/codex-bridge/current/bin/codex"
	defaultAgyBin     = "/opt/agy-bridge/current/agy"
	defaultCopilotBin = "/opt/copilot-bridge/current/copilot"
)

// bridgeConcurrency reads a bridge's *_BRIDGE_CONCURRENCY: how many CLI
// processes run at once. Every bridge defaults to 1: each slot is a full CLI
// process, and a subscription's quota does not grow with parallelism. A
// malformed value fails the boot rather than silently falling back.
func bridgeConcurrency(get func(string) string, env, label string) (int, error) {
	v := strings.TrimSpace(get(env))
	if v == "" {
		return 1, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s bridge: %s=%q must be a positive integer", label, env, v)
	}
	return n, nil
}

// envBridgeQueue is the one queue size every subscription bridge uses.
const envBridgeQueue = "EZLLM_BRIDGE_QUEUE"

// bridgeQueue reads EZLLM_BRIDGE_QUEUE: how many requests may wait for a CLI
// slot before a bridge answers 429 queue_full. One value for all bridges: the
// right size follows the clients' burst and the shared runner timeout, not the
// bridge (concurrency stays per bridge). Same positive-integer rule as
// bridgeConcurrency; the default is bridgeutil.DefaultMaxQueue.
func bridgeQueue(get func(string) string, label string) (int, error) {
	if strings.TrimSpace(get(envBridgeQueue)) == "" {
		return bridgeutil.DefaultMaxQueue, nil
	}
	return bridgeConcurrency(get, envBridgeQueue, label)
}

// bridgeTokenFromFile mints (once) and reads a bridge's loopback shared secret,
// 0600. Every bridge uses it; label names the default file.
// The account key ezllm stores is this value; nothing else authenticates the
// bridge surface.
func bridgeTokenFromFile(path, label string) (string, error) {
	path = bridgeTokenPath(path, label)
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

// bridgeTokenPath defaults an unset path to /data/<label>-bridge.token, on the
// data volume like the other bridges' tokens, so it is backed up with the ledger.
func bridgeTokenPath(path, label string) string {
	if path == "" {
		return "/data/" + label + "-bridge.token"
	}
	return path
}
