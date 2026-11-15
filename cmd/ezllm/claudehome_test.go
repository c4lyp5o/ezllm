package main

import "testing"

func TestClaudeHomeUsesTheUniformNameAndRefusesTheRetiredOne(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if h, err := claudeHome(env(nil)); err != nil || h != "/claude-sub" {
		t.Fatalf("default = %q %v, want /claude-sub", h, err)
	}
	if h, err := claudeHome(env(map[string]string{"EZLLM_CLAUDE_HOME": "/x"})); err != nil || h != "/x" {
		t.Fatalf("explicit = %q %v, want /x", h, err)
	}
	if _, err := claudeHome(env(map[string]string{"EZLLM_CLAUDE_CONFIG_DIR": "/old"})); err == nil {
		t.Fatal("the retired EZLLM_CLAUDE_CONFIG_DIR must fail the boot, not be ignored")
	}
}
