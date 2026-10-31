package config

// Tool pruning (docs/tool-pruning-design.md): one switch, self-sufficient.
// Default OFF — nothing changes without an operator action. The env override
// (EZLLM_TOOL_PRUNING_MODE) lives in cmd/ezllm/main.go as the no-rebuild
// rollback lever.

import (
	"strings"
	"testing"
)

func TestToolPruningDefaultsOff(t *testing.T) {
	cfg, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ToolPruning.Mode != "off" {
		t.Errorf("mode = %q, want off by default", cfg.ToolPruning.Mode)
	}
	if cfg.ToolPruning.MinUnusedSessions != 3 {
		t.Errorf("min_unused_sessions = %d, want 3 (the paper's rule)", cfg.ToolPruning.MinUnusedSessions)
	}
	if len(cfg.ToolPruning.Keep) != 0 {
		t.Errorf("keep = %v, want empty by default (tool names are operator knowledge)", cfg.ToolPruning.Keep)
	}
}

func TestToolPruningExplicitValues(t *testing.T) {
	cfg, err := Load(writeTemp(t, validYAML+"\ntool_pruning:\n  mode: shadow\n  min_unused_sessions: 5\n  keep: [\"Bash\", \"Read\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ToolPruning.Mode != "shadow" || cfg.ToolPruning.MinUnusedSessions != 5 || len(cfg.ToolPruning.Keep) != 2 {
		t.Errorf("explicit values did not land: %+v", cfg.ToolPruning)
	}
}

// A partial section must keep the seeded defaults (the translation rule).
func TestToolPruningPartialSectionKeepsDefaults(t *testing.T) {
	cfg, err := Load(writeTemp(t, validYAML+"\ntool_pruning:\n  keep: [\"Bash\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ToolPruning.Mode != "off" || cfg.ToolPruning.MinUnusedSessions != 3 {
		t.Errorf("partial section must keep defaults, got %+v", cfg.ToolPruning)
	}
}

// A typo'd mode must fail boot naming the field — silently measuring nothing
// is the failure mode this prevents.
func TestToolPruningInvalidModeFailsBoot(t *testing.T) {
	_, err := Load(writeTemp(t, validYAML+"\ntool_pruning:\n  mode: mesure\n"))
	if err == nil || !strings.Contains(err.Error(), "tool_pruning.mode") {
		t.Fatalf("err = %v, want a tool_pruning.mode-named refusal", err)
	}
}

// A wrong-TYPED leaf gets a field-named error (the yaml diagnostic names only
// the type) — the universalValueError rule.
func TestToolPruningWrongTypesNameTheField(t *testing.T) {
	for _, case_ := range []struct{ yaml, field string }{
		{"tool_pruning:\n  mode: 5\n", "tool_pruning.mode"},
		{"tool_pruning:\n  min_unused_sessions: three\n", "tool_pruning.min_unused_sessions"},
		{"tool_pruning:\n  keep: Bash\n", "tool_pruning.keep"},
	} {
		_, err := Load(writeTemp(t, validYAML+"\n"+case_.yaml))
		if err == nil || !strings.Contains(err.Error(), case_.field) {
			t.Errorf("yaml %q: err = %v, want it to name %s", case_.yaml, err, case_.field)
		}
	}
}

func TestToolPruningBounds(t *testing.T) {
	for _, y := range []string{
		"\ntool_pruning:\n  min_unused_sessions: 0\n",
		"\ntool_pruning:\n  min_unused_sessions: 1001\n",
		"\ntool_pruning:\n  keep: [\"Bash\", \"\"]\n",
	} {
		if _, err := Load(writeTemp(t, validYAML+y)); err == nil {
			t.Errorf("yaml %q must be refused", y)
		}
	}
}
