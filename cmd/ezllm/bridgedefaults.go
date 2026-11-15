package main

// Default CLI binaries: the updater-managed layout (scripts/bridge_update.py
// BRIDGES), <bridge-root>/current/<bin>, so a compose that mounts the bridge
// root at /opt/<name>-bridge needs no *_BIN override.
const (
	defaultClaudeBin  = "/opt/claude-bridge/current/claude"
	defaultCodexBin   = "/opt/codex-bridge/current/bin/codex"
	defaultAgyBin     = "/opt/agy-bridge/current/agy"
	defaultCopilotBin = "/opt/copilot-bridge/current/copilot"
)
