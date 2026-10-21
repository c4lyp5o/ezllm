package config

// translation.universal_enabled is the switch for the three NEW matrix cells
// (chat→messages, responses→chat, responses→messages) and the universal
// selection ordering. It defaults ON (LiteLLM-style: combos are universal by
// default). A wrong-typed value still fails boot NAMING the field.
//
// Since 2026-10-22 it is DEPRECATED: translation.enabled is the one switch, and
// Load folds universal_enabled into it (either key can turn translation off).

import (
	"strings"
	"testing"
)

func TestUniversalDefaultsOn(t *testing.T) {
	cfg, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Translation.UniversalEnabled {
		t.Error("translation.universal_enabled must default ON when the key is absent")
	}
}

func TestUniversalExplicitFalseWins(t *testing.T) {
	cfg, err := Load(writeTemp(t, validYAML+"\ntranslation:\n  universal_enabled: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Translation.UniversalEnabled {
		t.Error("an explicit universal_enabled: false must turn the matrix off")
	}
}

func TestUniversalExplicitTrueWins(t *testing.T) {
	cfg, err := Load(writeTemp(t, validYAML+"\ntranslation:\n  universal_enabled: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Translation.UniversalEnabled {
		t.Error("an explicit universal_enabled: true must turn the matrix on")
	}
}

func TestUniversalPartialSectionKeepsOn(t *testing.T) {
	cfg, err := Load(writeTemp(t, validYAML+"\ntranslation:\n  enabled: true\n  responses_models: [\"tp/openai/gpt-6.1-sol\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Translation.UniversalEnabled {
		t.Error("a partial translation section must keep universal_enabled ON (default)")
	}
	if !cfg.Translation.Enabled {
		t.Error("the partial section must keep translation.enabled ON")
	}
}

func TestUniversalInvalidValueFailsBootNamingField(t *testing.T) {
	for _, bad := range []string{"maybe", "1", "\"true\"", "[true]"} {
		_, err := Load(writeTemp(t, validYAML+"\ntranslation:\n  universal_enabled: "+bad+"\n"))
		if err == nil {
			t.Fatalf("universal_enabled: %s was accepted, want a boot failure", bad)
		}
		if !strings.Contains(err.Error(), "translation.universal_enabled") {
			t.Errorf("universal_enabled: %s error must name the field, got: %v", bad, err)
		}
	}
}

func TestTranslationEnabledFalseCoversTheMatrix(t *testing.T) {
	cfg, err := Load(writeTemp(t, validYAML+"\ntranslation:\n  enabled: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Translation.Enabled || cfg.Translation.UniversalEnabled {
		t.Errorf("enabled: false must turn off every translated wire, got enabled=%v universal=%v",
			cfg.Translation.Enabled, cfg.Translation.UniversalEnabled)
	}
}

func TestDeprecatedUniversalFalseTurnsTranslationOff(t *testing.T) {
	cfg, err := Load(writeTemp(t, validYAML+"\ntranslation:\n  universal_enabled: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Translation.Enabled {
		t.Error("an old universal_enabled: false rollback must still switch translation off")
	}
}

func TestDeprecatedUniversalKeyWarns(t *testing.T) {
	cfg, err := Load(writeTemp(t, validYAML+"\ntranslation:\n  universal_enabled: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "translation.universal_enabled is deprecated") {
		t.Errorf("want one deprecation warning naming the key, got %q", cfg.Warnings)
	}
	plain, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if len(plain.Warnings) != 0 {
		t.Errorf("no deprecated key, want no warnings, got %q", plain.Warnings)
	}
}
