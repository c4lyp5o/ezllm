package main

import (
	"os/exec"
	"strings"
	"testing"
)

// A trimmed capture of `copilot help config` (CLI 1.0.105): the model values sit
// under `model`, and the NEXT setting's quoted words must not leak in.
const helpConfigSample = `
  ` + "`model`" + `: AI model to use for Copilot CLI; can be changed with /model command or --model flag option.
    - "claude-sonnet-5.5"
    - "gpt-6.1-sol"
    - "gpt-6-luna"
    - "kimi-k3"

  ` + "`contextTier`" + `: context window tier (e.g., "default" or "long_context").
    - "default"
    - "long_context"
`

func TestParseHelpConfigModelsStopsAtTheNextSetting(t *testing.T) {
	got := strings.Join(parseHelpConfigModels(helpConfigSample), ",")
	if got != "claude-sonnet-5.5,gpt-6.1-sol,gpt-6-luna,kimi-k3" {
		t.Errorf("parsed = %q (context-tier values must not leak in)", got)
	}
	if parseHelpConfigModels("a help page of another shape") != nil {
		t.Error("an unrecognised page must yield nothing so the built-in list stands")
	}
}

// The key test probes the FIRST candidate; "auto" is the one id every plan
// answers, so it must lead, whatever the live read returns.
func TestCopilotCatalogLeadsWithAutoAndMergesWithoutDuplicates(t *testing.T) {
	cat := copilotCatalog([]string{"gpt-6.1-sol", "brand-new-model", "auto"})
	if cat[0] != "auto" {
		t.Fatalf("catalog starts %q, want auto", cat[0])
	}
	seen := map[string]bool{}
	for _, id := range cat {
		if seen[id] {
			t.Errorf("duplicate %q", id)
		}
		seen[id] = true
	}
	if !seen["brand-new-model"] || !seen["claude-opus-5.5"] {
		t.Error("catalog must hold the live ids AND the built-in fallback")
	}
	if got := copilotCatalog(nil); len(got) != len(copilotModelIDs) {
		t.Errorf("offline catalog = %d ids, want the %d built-in", len(got), len(copilotModelIDs))
	}
}

// Tripwire against format drift: when the Copilot CLI is installed, its real
// `help config` page must still parse into a catalog that holds our known ids.
func TestParseHelpConfigAgainstTheRealCLI(t *testing.T) {
	bin, err := exec.LookPath("copilot")
	if err != nil {
		t.Skip("copilot CLI not installed")
	}
	live := copilotLiveModels(bin)
	if len(live) < 10 {
		t.Fatalf("parsed %d ids from the real `help config` (%v): the page changed shape", len(live), live)
	}
	for _, id := range live {
		if id == "default" || id == "long_context" {
			t.Errorf("context-tier value %q leaked into the model list", id)
		}
	}
}
