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

// Like the other bridges, an unset token file lands on the data volume instead
// of refusing the boot.
func TestCopilotBridgeTokenDefaultsToDataVolume(t *testing.T) {
	if got := bridgeTokenPath("", "copilot"); got != "/data/copilot-bridge.token" {
		t.Fatalf("default = %q, want /data/copilot-bridge.token", got)
	}
	if got := bridgeTokenPath("/x/t", "copilot"); got != "/x/t" {
		t.Fatalf("explicit path = %q, want /x/t", got)
	}
}
