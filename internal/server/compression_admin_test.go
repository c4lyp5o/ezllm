package server

import (
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/compress"
)

// The supported-engines message must not drift from the registry. It was
// once hardcoded and silently omitted a registered engine, so an operator
// typing a valid engine got an error that didn't list it as valid.
func TestValidateStagesErrorListsEveryRegisteredEngine(t *testing.T) {
	_, err := validateStages([]byte(`[{"engine":"nope"}]`))
	if err == nil {
		t.Fatal("expected unknown engine to be rejected")
	}
	msg := err.Error()
	for name := range compress.KnownEngines {
		if !strings.Contains(msg, name) {
			t.Errorf("error message must list every registered engine, missing %q: %s", name, msg)
		}
	}
}

// budget was deleted (2026-10-19): dropping whole messages broke prompt
// caching and tool pairing. A profile naming it is an unknown engine now.
func TestValidateStagesRejectsDeletedBudgetEngine(t *testing.T) {
	if _, err := validateStages([]byte(`[{"engine":"budget","options":{"max_tokens":100}}]`)); err == nil {
		t.Fatal("budget must be rejected: the engine no longer exists")
	}
}

// omniroute's aggressive/ultra are pipeline compositions, not prose levels.
// Accepting them here would let a profile author believe a stronger rewrite
// was running when `standard` was — the docs refuse this deliberately.
func TestValidateStagesRejectsCavemanCompositionAliases(t *testing.T) {
	for _, bad := range []string{"aggressive", "ultra", "Aggressive", " ULTRA "} {
		pipe := `[{"engine":"caveman","options":{"intensity":"` + bad + `"}}]`
		_, err := validateStages([]byte(pipe))
		if err == nil {
			t.Errorf("intensity %q must be rejected, not silently accepted", bad)
			continue
		}
		if !strings.Contains(err.Error(), "session_dedup") {
			t.Errorf("rejection must point at the composed pipeline, got: %s", err.Error())
		}
	}
}

func TestValidateStagesAcceptsRealCavemanIntensities(t *testing.T) {
	for _, good := range []string{"lite", "standard"} {
		pipe := `[{"engine":"caveman","options":{"intensity":"` + good + `"}}]`
		if _, err := validateStages([]byte(pipe)); err != nil {
			t.Errorf("intensity %q must stay valid, got: %v", good, err)
		}
	}
	// Absent and empty are both the engine's own default (lite).
	for _, pipe := range []string{
		`[{"engine":"caveman"}]`,
		`[{"engine":"caveman","options":{"intensity":""}}]`,
	} {
		if _, err := validateStages([]byte(pipe)); err != nil {
			t.Errorf("default intensity must validate, %s -> %v", pipe, err)
		}
	}
}

// A non-caveman stage carries no intensity; validating it must be a no-op.
func TestValidateStagesIgnoresOptionsOnOtherEngines(t *testing.T) {
	pipe := `[{"engine":"headroom","options":{"intensity":"aggressive"}}]`
	if _, err := validateStages([]byte(pipe)); err != nil {
		t.Errorf("intensity is meaningless outside caveman and must not be validated: %v", err)
	}
}

// rtk tool_classes: a typo'd class would be ignored silently and the tool
// would keep its built-in class, so the write is refused instead.
func TestValidateStagesChecksRTKToolClasses(t *testing.T) {
	if _, err := validateStages([]byte(`[{"engine":"rtk","options":{"tool_classes":{"terminal":"log","web_extract":"preserve","finder":"Search"}}}]`)); err != nil {
		t.Fatalf("valid tool_classes rejected: %v", err)
	}
	for _, bad := range []string{
		`[{"engine":"rtk","options":{"tool_classes":{"terminal":"logs"}}}]`,
		`[{"engine":"rtk","options":{"tool_classes":{"terminal":3}}}]`,
		`[{"engine":"rtk","options":{"tool_classes":["terminal"]}}]`,
		`[{"engine":"rtk","options":{"tool_classes":{"":"log"}}}]`,
	} {
		if _, err := validateStages([]byte(bad)); err == nil {
			t.Errorf("accepted invalid tool_classes: %s", bad)
		}
	}
}
