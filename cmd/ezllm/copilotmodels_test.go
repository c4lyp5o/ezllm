package main

import "testing"

// The key test probes the FIRST candidate; "auto" is the one id every plan
// answers, so it must stay first and ids must be unique.
func TestCopilotModelSeedStartsWithAutoAndHasNoDuplicates(t *testing.T) {
	if len(copilotModelIDs) == 0 || copilotModelIDs[0] != "auto" {
		t.Fatalf("copilotModelIDs = %v, want auto first", copilotModelIDs)
	}
	seen := map[string]bool{}
	for _, id := range copilotModelIDs {
		if seen[id] {
			t.Errorf("duplicate id %q", id)
		}
		seen[id] = true
	}
}
