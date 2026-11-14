package main

import "testing"

func TestCopilotHomeDefaultsToDedicatedSubFolder(t *testing.T) {
	unset := func(string) string { return "" }
	if got := copilotHome(unset); got != "/copilot-sub" {
		t.Errorf("default copilotHome = %q, want /copilot-sub", got)
	}
	set := func(k string) string {
		if k == envCopilotHome {
			return "/custom"
		}
		return ""
	}
	if got := copilotHome(set); got != "/custom" {
		t.Errorf("copilotHome with %s set = %q, want /custom", envCopilotHome, got)
	}
}
