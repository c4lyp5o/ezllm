package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTemp writes a config body to a temp file and returns its path.
func writeTemp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// setEnv sets env vars for the duration of a test.
func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

const validYAML = `
listen: "127.0.0.1:20129"
client_tokens:
  - { name: hermes, token_env: EZLLM_TOK }
providers:
  p1:
    base_url: https://example.com/v1
    keys: [{ env: P1_KEY }]
aliases:
  ez/a: { provider: p1, model: m1, fallback: [ { provider: p1, model: m2 } ] }
`

func TestLoadValid(t *testing.T) {
	setEnv(t, map[string]string{"EZLLM_TOK": "tok-abc", "P1_KEY": "pk"})
	cfg, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatalf("expected valid config, got: %v", err)
	}
	// defaults applied
	if cfg.Ledger.Path != "data/ezllm.sqlite" {
		t.Errorf("ledger default = %q", cfg.Ledger.Path)
	}
	if cfg.Cooldown.On429 != "300s" || cfg.Cooldown.MaxPerKey != 3 {
		t.Errorf("cooldown defaults = %+v", cfg.Cooldown)
	}
	// auth round-trip
	if got := cfg.Authenticate("tok-abc"); got != "hermes" {
		t.Errorf("Authenticate = %q, want hermes", got)
	}
	if got := cfg.Authenticate("wrong"); got != "" {
		t.Errorf("Authenticate(wrong) = %q, want empty", got)
	}
	if names := cfg.AliasNames(); len(names) != 1 || names[0] != "ez/a" {
		t.Errorf("AliasNames = %v", names)
	}
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	setEnv(t, map[string]string{}) // no env vars set
	yaml := `
client_tokens:
  - { name: hermes, token_env: MISSING_TOK }
providers:
  p1:
    base_url: not-a-url
    keys: []
aliases:
  ez/a: { provider: nope, model: "" }
`
	_, err := Load(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{
		"MISSING_TOK is not set",
		"base_url must be an absolute",
		"at least one key required",
		`unknown provider "nope"`,
		`aliases[ez/a]: model is required`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q\ngot:\n%s", want, msg)
		}
	}
}

func TestUnknownProviderInFallbackRejected(t *testing.T) {
	setEnv(t, map[string]string{"EZLLM_TOK": "t", "P1_KEY": "k"})
	yaml := `
client_tokens: [{ name: c, token_env: EZLLM_TOK }]
providers: { p1: { base_url: https://x/v1, keys: [{ env: P1_KEY }] } }
aliases:
  ez/a: { provider: p1, model: m1, fallback: [ { provider: ghost, model: m } ] }
`
	_, err := Load(writeTemp(t, yaml))
	if err == nil || !strings.Contains(err.Error(), `fallback[0]: unknown provider "ghost"`) {
		t.Fatalf("got %v", err)
	}
}

func TestDuplicateTokenValueRejected(t *testing.T) {
	setEnv(t, map[string]string{"T_A": "same", "T_B": "same", "P1_KEY": "k"})
	yaml := `
client_tokens:
  - { name: a, token_env: T_A }
  - { name: b, token_env: T_B }
providers: { p1: { base_url: https://x/v1, keys: [{ env: P1_KEY }] } }
aliases: { ez/a: { provider: p1, model: m } }
`
	_, err := Load(writeTemp(t, yaml))
	if err == nil || !strings.Contains(err.Error(), "duplicate token value") {
		t.Fatalf("got %v", err)
	}
}
