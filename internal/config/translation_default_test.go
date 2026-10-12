package config

// translation.enabled defaults ON (global enable, 2026-10-12). The default is
// seeded BEFORE the YAML decode, so an absent key keeps it while an explicit
// `enabled: false` still wins. The env override (EZLLM_TRANSLATION_ENABLED)
// lives in cmd/ezllm/main.go — it stays the no-rebuild rollback lever.

import (
	"strings"
	"testing"
)

func TestTranslationDefaultsOn(t *testing.T) {
	cfg, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Translation.Enabled {
		t.Error("translation.enabled must default ON when the key is absent")
	}
}

func TestTranslationExplicitFalseWins(t *testing.T) {
	cfg, err := Load(writeTemp(t, validYAML+"\ntranslation:\n  enabled: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Translation.Enabled {
		t.Error("an explicit enabled: false must keep translation off")
	}
}

// The default must survive a partial translation section: the operator
// curates responses_models without re-stating enabled.
func TestTranslationPartialSectionKeepsDefault(t *testing.T) {
	cfg, err := Load(writeTemp(t, validYAML+"\ntranslation:\n  responses_models: [\"tp/openai/gpt-6.1-sol\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Translation.Enabled {
		t.Error("partial translation section must keep the enabled default ON")
	}
	if len(cfg.Translation.ResponsesModels) != 1 || !strings.Contains(cfg.Translation.ResponsesModels[0], "gpt-6.1-sol") {
		t.Errorf("responses_models = %v", cfg.Translation.ResponsesModels)
	}
}

// responses_models entries are regexes; a bad pattern must be a BOOT failure
// naming the entry, never a silently-dropped one (the operator would be left
// wondering why a hop still rides the chat wire).
func TestResponsesModelsPatternsValidatedAtBoot(t *testing.T) {
	if _, err := Load(writeTemp(t, validYAML+"\ntranslation:\n  responses_models: [\"stationine/gpt-6\\\\.1-sol\", \"vercel-ai-gateway/openai/gpt-6.1-sol.*\"]\n")); err != nil {
		t.Fatalf("valid patterns rejected: %v", err)
	}
	_, err := Load(writeTemp(t, validYAML+"\ntranslation:\n  responses_models: [\"stationine/gpt-6.1-sol\", \"[\"]\n"))
	if err == nil {
		t.Fatal("an uncompilable pattern must fail the boot")
	}
	if !strings.Contains(err.Error(), "responses_models[1]") {
		t.Errorf("error must name the offending entry, got: %v", err)
	}
	if _, err := Load(writeTemp(t, validYAML+"\ntranslation:\n  responses_models: [\"  \"]\n")); err == nil {
		t.Error("a blank pattern must fail the boot")
	}
}
