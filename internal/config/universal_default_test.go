package config

// translation.universal_enabled is the switch for the three NEW matrix cells
// (chat→messages, responses→chat, responses→messages) and the universal
// selection ordering. It defaults ON (LiteLLM-style: combos are universal by
// default). A wrong-typed value still fails boot NAMING the field. The env var
// EZLLM_TRANSLATION_UNIVERSAL_ENABLED remains the no-rebuild rollback lever.

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
