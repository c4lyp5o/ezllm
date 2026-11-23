package main

import (
	"fmt"
	"sort"
	"strings"
)

// retiredEnv maps settings that no longer exist to what replaced them.
// A retired name fails the boot rather than being ignored: ignoring it would
// silently change behavior the operator asked for.
var retiredEnv = map[string]string{
	"EZLLM_CLAUDE_CONFIG_DIR":             "renamed to EZLLM_CLAUDE_HOME in 2.0.0",
	"EZLLM_COPILOT_BRIDGE_SESSION_CACHE":  "renamed to EZLLM_COPILOT_BRIDGE_CACHE in 2.0.0",
	"EZLLM_AGY_BRIDGE_SESSION_CACHE":      "renamed to EZLLM_AGY_BRIDGE_CACHE in 2.0.0",
	"EZLLM_CODEX_SANDBOX":                 "removed in 2.0.0: the Codex sandbox is always writable",
	"EZLLM_CODEX_TOOLS_WITHOUT_LANDLOCK":  "removed in 2.0.0: Codex client tools require Landlock",
	"EZLLM_TRANSLATION_UNIVERSAL_ENABLED": "renamed to EZLLM_TRANSLATION_ENABLED in 2.0.0",
	"EZLLM_WEB_DIR":                       "removed in 2.0.0: the dashboard is always the one built into the binary",
	"EZLLM_COPILOT_BRIDGE_QUEUE":          "replaced by EZLLM_BRIDGE_QUEUE (one queue size for every bridge) in 2.2.0",
}

func checkRetiredEnv(get func(string) string) error {
	names := make([]string, 0, len(retiredEnv))
	for name := range retiredEnv {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.TrimSpace(get(name)) != "" {
			return fmt.Errorf("settings: %s was %s", name, retiredEnv[name])
		}
	}
	return nil
}
