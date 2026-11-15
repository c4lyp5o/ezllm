package main

import (
	"fmt"
	"strconv"
	"strings"
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
