package main

import "testing"

func TestClaudeHomeUsesTheUniformName(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if h := claudeHome(env(nil)); h != "/claude-sub" {
		t.Fatalf("default = %q, want /claude-sub", h)
	}
	if h := claudeHome(env(map[string]string{"EZLLM_CLAUDE_HOME": "/x"})); h != "/x" {
		t.Fatalf("explicit = %q, want /x", h)
	}
}

// A retired setting fails the boot, naming its replacement, instead of being
// silently ignored.
func TestRetiredBridgeSettingsFailTheBoot(t *testing.T) {
	if err := checkRetiredEnv(func(string) string { return "" }); err != nil {
		t.Fatalf("clean env refused: %v", err)
	}
	for name := range retiredEnv {
		err := checkRetiredEnv(func(k string) string {
			if k == name {
				return "on"
			}
			return ""
		})
		if err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for _, name := range []string{"EZLLM_CLAUDE_CONFIG_DIR", "EZLLM_COPILOT_BRIDGE_SESSION_CACHE", "EZLLM_AGY_BRIDGE_SESSION_CACHE", "EZLLM_CODEX_SANDBOX", "EZLLM_CODEX_TOOLS_WITHOUT_LANDLOCK", "EZLLM_TRANSLATION_UNIVERSAL_ENABLED", "EZLLM_WEB_DIR"} {
		if _, ok := retiredEnv[name]; !ok {
			t.Errorf("%s is not retired", name)
		}
	}
}
