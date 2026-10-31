package main

// EZLLM_TOOL_PRUNING_MODE is the no-rebuild override of the ONE tool-pruning
// switch (docs/tool-pruning-design.md) — the translationFromEnv rule: a
// malformed value is a startup error, never a silent default.

import (
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/config"
)

func TestToolPruningFromEnv(t *testing.T) {
	cases := []struct {
		env      string
		wantMode string
		wantErr  bool
	}{
		{"", "off", false},    // unset keeps config
		{"off", "off", false}, // explicit off
		{"shadow", "shadow", false},
		{"Shadow", "shadow", false}, // case-insensitive
		{"prune", "", true},         // M2 value must not pretend to work in M1
		{"yes", "", true},
	}
	for _, c := range cases {
		tp := config.ToolPruning{Mode: "off"}
		err := toolPruningFromEnv(&tp, func(k string) string {
			if k == "EZLLM_TOOL_PRUNING_MODE" {
				return c.env
			}
			return ""
		})
		if c.wantErr {
			if err == nil {
				t.Errorf("env %q: want an error", c.env)
			} else if !strings.Contains(err.Error(), "EZLLM_TOOL_PRUNING_MODE") {
				t.Errorf("env %q: error must name the variable, got %v", c.env, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("env %q: %v", c.env, err)
		}
		if tp.Mode != c.wantMode {
			t.Errorf("env %q: mode = %q, want %q", c.env, tp.Mode, c.wantMode)
		}
	}
}
