package main

import (
	"fmt"
	"sort"
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

// retiredBridgeEnv maps settings that no longer exist to what replaced them.
// A retired name fails the boot rather than being ignored: ignoring it would
// silently change behavior the operator asked for.
var retiredBridgeEnv = map[string]string{
	"EZLLM_CLAUDE_CONFIG_DIR":            "renamed to EZLLM_CLAUDE_HOME",
	"EZLLM_COPILOT_BRIDGE_SESSION_CACHE": "renamed to EZLLM_COPILOT_BRIDGE_CACHE",
	"EZLLM_AGY_BRIDGE_SESSION_CACHE":     "renamed to EZLLM_AGY_BRIDGE_CACHE",
	"EZLLM_CODEX_SANDBOX":                "removed: the Codex sandbox is always writable",
}

func checkRetiredBridgeEnv(get func(string) string) error {
	names := make([]string, 0, len(retiredBridgeEnv))
	for name := range retiredBridgeEnv {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.TrimSpace(get(name)) != "" {
			return fmt.Errorf("bridge settings: %s was %s in 1.9.0", name, retiredBridgeEnv[name])
		}
	}
	return nil
}
