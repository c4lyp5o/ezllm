package main

import (
	"testing"
	"time"
)

func TestCopilotLimits(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	c, q, d, err := copilotLimits(env(nil))
	if err != nil || c != 2 || q != 8 || d != 300*time.Second {
		t.Fatalf("defaults = %d %d %v %v", c, q, d, err)
	}
	c, q, d, err = copilotLimits(env(map[string]string{envCopilotConcurrency: "4", envCopilotQueue: "16", envCopilotTimeout: "90s"}))
	if err != nil || c != 4 || q != 16 || d != 90*time.Second {
		t.Fatalf("overrides = %d %d %v %v", c, q, d, err)
	}
	for _, bad := range []map[string]string{
		{envCopilotConcurrency: "0"}, {envCopilotConcurrency: "x"}, {envCopilotQueue: "-1"},
		{envCopilotTimeout: "300"}, {envCopilotTimeout: "0s"},
	} {
		if _, _, _, err := copilotLimits(env(bad)); err == nil {
			t.Errorf("%v accepted, want a boot failure", bad)
		}
	}
}
