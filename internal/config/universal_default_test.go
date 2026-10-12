package config

// translation.universal_enabled is the opt-in for the three NEW matrix cells
// (chat→messages, responses→chat, responses→messages) and the universal
// selection ordering. It defaults OFF: enabling it is an operator decision,
// and a malformed value must fail boot NAMING the field — the yaml decoder's
// own error only names the type, which leaves the operator guessing.

import (
	"strings"
	"testing"
)

func TestUniversalDefaultsOff(t *testing.T) {
	cfg, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Translation.UniversalEnabled {
		t.Error("translation.universal_enabled must default OFF when the key is absent")
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

func TestUniversalPartialSectionKeepsOff(t *testing.T) {
	cfg, err := Load(writeTemp(t, validYAML+"\ntranslation:\n  enabled: true\n  responses_models: [\"tp/openai/gpt-6.1-sol\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Translation.UniversalEnabled {
		t.Error("a partial translation section must keep universal_enabled OFF")
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
