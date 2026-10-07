package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
data_dir: ./data
client_tokens:
  - { name: hermes, token_env: EZLLM_TOK, roles: "infer,admin" }
providers:
  opencode-main:
    namespace: opengo
    kind: opencode-go
    base_url: https://opencode.ai/zen/go/v1
    keys: [{ env: P1_KEY, label: primary }]
ledger:
  batch_size: 16
  batch_wait: 100ms
`

func TestLoadValid(t *testing.T) {
	setEnv(t, map[string]string{"EZLLM_TOK": "tok-abc", "P1_KEY": "pk"})
	cfg, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatalf("expected valid config, got: %v", err)
	}
	if cfg.DataDir != "./data" {
		t.Errorf("data_dir = %q", cfg.DataDir)
	}
	if cfg.MaxBodyMiB != 32 {
		t.Errorf("max_body_mib default = %d, want 32", cfg.MaxBodyMiB)
	}
	if cfg.UpstreamTimeout != 0 {
		t.Errorf("upstream_timeout default = %v, want 0 (unbounded for streaming)", cfg.UpstreamTimeout)
	}
	if cfg.Ledger.BatchSize != 16 || cfg.LedgerBatchWait != 100*time.Millisecond {
		t.Errorf("ledger = %+v / %v", cfg.Ledger, cfg.LedgerBatchWait)
	}
	if cfg.Cooldown.On429 != "300s" || cfg.Cooldown.MaxPerKey != 3 {
		t.Errorf("cooldown defaults = %+v", cfg.Cooldown)
	}
	if ns := cfg.Namespace("opencode-main"); ns != "opengo" {
		t.Errorf("namespace = %q, want opengo", ns)
	}
	if !cfg.Providers["opencode-main"].RequiresSession() {
		t.Error("opencode-go should default to requiring session headers")
	}
	if names := cfg.ProviderNames(); len(names) != 1 || names[0] != "opencode-main" {
		t.Errorf("ProviderNames = %v", names)
	}
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	setEnv(t, map[string]string{}) // nothing set
	yaml := `
client_tokens:
  - { name: hermes, token_env: MISSING_TOK }
providers:
  p1:
    kind: not-a-kind
    base_url: not-a-url
    keys: []
`
	_, err := Load(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{
		"MISSING_TOK is not set",
		"base_url must be an absolute",
		`unknown kind "not-a-kind"`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q\ngot:\n%s", want, msg)
		}
	}
	// "p1" IS a valid namespace (defaults to the provider map key), so it must
	// NOT appear as a namespace error here.
	if strings.Contains(msg, "namespace") {
		t.Errorf("p1 is a valid namespace; unexpected namespace error:\n%s", msg)
	}
}

// A namespace containing '/' would collide with the <namespace>/<model> routing
// form — it must be rejected, not silently accepted.
func TestNamespaceRejectsSlashAndBadForm(t *testing.T) {
	setEnv(t, map[string]string{"T": "t", "K": "k"})
	for _, ns := range []string{"has/slash", "UPPER", "-leading", "a b", strings.Repeat("x", 33)} {
		yaml := `
client_tokens: [{ name: c, token_env: T }]
providers:
  p1:
    namespace: "` + ns + `"
    kind: openai-compatible
    base_url: https://x.example/v1
    keys: [{ env: K }]
`
		_, err := Load(writeTemp(t, yaml))
		if err == nil {
			t.Errorf("namespace %q should be rejected", ns)
			continue
		}
		if !strings.Contains(err.Error(), "namespace") {
			t.Errorf("namespace %q: error should mention namespace, got %v", ns, err)
		}
	}
}

func TestNamespaceAcceptsValidForms(t *testing.T) {
	setEnv(t, map[string]string{"T": "t", "K": "k"})
	for _, ns := range []string{"opengo", "super-ssn", "a", "x_1", "abc-123_x", strings.Repeat("y", 32)} {
		yaml := `
client_tokens: [{ name: c, token_env: T }]
providers:
  p1:
    namespace: "` + ns + `"
    kind: openai-compatible
    base_url: https://x.example/v1
    keys: [{ env: K }]
`
		if _, err := Load(writeTemp(t, yaml)); err != nil {
			t.Errorf("namespace %q should be valid: %v", ns, err)
		}
	}
}

// Two accounts claiming one namespace would make routing ambiguous.
func TestDuplicateNamespaceRejected(t *testing.T) {
	setEnv(t, map[string]string{"T": "t", "K": "k"})
	yaml := `
client_tokens: [{ name: c, token_env: T }]
providers:
  p1: { namespace: same, kind: openai-compatible, base_url: https://a/v1, keys: [{env: K}] }
  p2: { namespace: same, kind: openai-compatible, base_url: https://b/v1, keys: [{env: K}] }
`
	_, err := Load(writeTemp(t, yaml))
	if err == nil || !strings.Contains(err.Error(), "already used by") {
		t.Fatalf("duplicate namespace must be rejected, got %v", err)
	}
}

// Two client tokens sharing one env var makes attribution ambiguous.
func TestDuplicateTokenEnvRejected(t *testing.T) {
	setEnv(t, map[string]string{"T_A": "same", "K": "k"})
	yaml := `
client_tokens:
  - { name: a, token_env: T_A }
  - { name: b, token_env: T_A }
providers: { p1: { kind: openai-compatible, base_url: https://x/v1, keys: [{env: K}] } }
`
	_, err := Load(writeTemp(t, yaml))
	if err == nil || !strings.Contains(err.Error(), "attribution would be ambiguous") {
		t.Fatalf("shared token env must be rejected, got %v", err)
	}
}

func TestDuplicateClientNameRejected(t *testing.T) {
	setEnv(t, map[string]string{"A": "1", "B": "2", "K": "k"})
	yaml := `
client_tokens:
  - { name: same, token_env: A }
  - { name: same, token_env: B }
providers: { p1: { kind: openai-compatible, base_url: https://x/v1, keys: [{env: K}] } }
`
	_, err := Load(writeTemp(t, yaml))
	if err == nil || !strings.Contains(err.Error(), "duplicate name") {
		t.Fatalf("duplicate client name must be rejected, got %v", err)
	}
}

func TestUnknownRoleRejected(t *testing.T) {
	setEnv(t, map[string]string{"T": "t", "K": "k"})
	yaml := `
client_tokens: [{ name: c, token_env: T, roles: "infer,superuser" }]
providers: { p1: { kind: openai-compatible, base_url: https://x/v1, keys: [{env: K}] } }
`
	_, err := Load(writeTemp(t, yaml))
	if err == nil || !strings.Contains(err.Error(), `unknown role "superuser"`) {
		t.Fatalf("bad role must be rejected, got %v", err)
	}
}

func TestKindValidation(t *testing.T) {
	setEnv(t, map[string]string{"T": "t", "K": "k"})
	for _, kind := range []string{"opencode-go", "openai-compatible", "anthropic-compatible"} {
		yaml := `
client_tokens: [{ name: c, token_env: T }]
providers: { p1: { kind: ` + kind + `, base_url: https://x/v1, keys: [{env: K}] } }
`
		if _, err := Load(writeTemp(t, yaml)); err != nil {
			t.Errorf("kind %q should be valid: %v", kind, err)
		}
	}
	for _, kind := range []string{"", "github-copilot", "openai", "OpenAI-Compatible"} {
		yaml := `
client_tokens: [{ name: c, token_env: T }]
providers: { p1: { kind: "` + kind + `", base_url: https://x/v1, keys: [{env: K}] } }
`
		if _, err := Load(writeTemp(t, yaml)); err == nil {
			t.Errorf("kind %q should be rejected", kind)
		}
	}
}

// base_url must not end in '/' — the adapter appends paths, so a trailing slash
// would produce '//chat/completions'.
func TestBaseURLTrailingSlashRejected(t *testing.T) {
	setEnv(t, map[string]string{"T": "t", "K": "k"})
	yaml := `
client_tokens: [{ name: c, token_env: T }]
providers: { p1: { kind: openai-compatible, base_url: https://x/v1/, keys: [{env: K}] } }
`
	if _, err := Load(writeTemp(t, yaml)); err == nil || !strings.Contains(err.Error(), "must not end in '/'") {
		t.Fatalf("trailing slash must be rejected, got %v", err)
	}
}

func TestRequiresSessionDefaultsPerKind(t *testing.T) {
	setEnv(t, map[string]string{"T": "t", "K": "k"})
	// opencode-go defaults to true (verified: missing header -> 400 MissingSessionID)
	cfg, err := Load(writeTemp(t, `
client_tokens: [{ name: c, token_env: T }]
providers: { p1: { kind: opencode-go, base_url: https://x/v1, keys: [{env: K}] } }
`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Providers["p1"].RequiresSession() {
		t.Error("opencode-go must default to requiring a session header")
	}
	// others default false
	cfg, err = Load(writeTemp(t, `
client_tokens: [{ name: c, token_env: T }]
providers: { p1: { kind: openai-compatible, base_url: https://x/v1, keys: [{env: K}] } }
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers["p1"].RequiresSession() {
		t.Error("openai-compatible must not require session headers by default")
	}
	// explicit override wins
	cfg, err = Load(writeTemp(t, `
client_tokens: [{ name: c, token_env: T }]
providers: { p1: { kind: openai-compatible, base_url: https://x/v1, requires_session_header: true, keys: [{env: K}] } }
`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Providers["p1"].RequiresSession() {
		t.Error("explicit requires_session_header: true must win")
	}
}

func TestUpstreamTimeoutAndBodyCap(t *testing.T) {
	setEnv(t, map[string]string{"T": "t", "K": "k"})
	yaml := `
client_tokens: [{ name: c, token_env: T }]
providers: { p1: { kind: openai-compatible, base_url: https://x/v1, keys: [{env: K}] } }
upstream_timeout: 90s
max_body_mib: 8
`
	cfg, err := Load(writeTemp(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UpstreamTimeout != 90*time.Second {
		t.Errorf("upstream_timeout = %v", cfg.UpstreamTimeout)
	}
	if cfg.MaxBodyMiB != 8 {
		t.Errorf("max_body_mib = %d", cfg.MaxBodyMiB)
	}
	if _, err := Load(writeTemp(t, strings.Replace(yaml, "90s", "notaduration", 1))); err == nil {
		t.Error("bad upstream_timeout must be rejected")
	}
	if _, err := Load(writeTemp(t, strings.Replace(yaml, "max_body_mib: 8", "max_body_mib: 9999", 1))); err == nil {
		t.Error("absurd max_body_mib must be rejected")
	}
}

func TestLedgerValidation(t *testing.T) {
	setEnv(t, map[string]string{"T": "t", "K": "k"})
	prov := `
client_tokens: [{ name: c, token_env: T }]
providers: { p1: { kind: openai-compatible, base_url: https://x/v1, keys: [{env: K}] } }
`
	if _, err := Load(writeTemp(t, prov+"ledger:\n  batch_size: 99999\n")); err == nil {
		t.Error("absurd batch_size must be rejected")
	}
	if _, err := Load(writeTemp(t, prov+"ledger:\n  batch_wait: 1ms\n")); err == nil {
		t.Error("batch_wait below 10ms must be rejected")
	}
	if _, err := Load(writeTemp(t, prov+"ledger:\n  batch_wait: nonsense\n")); err == nil {
		t.Error("bad batch_wait must be rejected")
	}
}

func TestQuotaModeValidation(t *testing.T) {
	setEnv(t, map[string]string{"T": "t", "K": "k"})
	for _, m := range []string{"probe", "percent", "money", "tokens", "none"} {
		yaml := `
client_tokens: [{ name: c, token_env: T }]
providers: { p1: { kind: openai-compatible, base_url: https://x/v1, quota_mode: ` + m + `, keys: [{env: K}] } }
`
		if _, err := Load(writeTemp(t, yaml)); err != nil {
			t.Errorf("quota_mode %q should be valid: %v", m, err)
		}
	}
	yaml := `
client_tokens: [{ name: c, token_env: T }]
providers: { p1: { kind: openai-compatible, base_url: https://x/v1, quota_mode: bogus, keys: [{env: K}] } }
`
	if _, err := Load(writeTemp(t, yaml)); err == nil {
		t.Error("bogus quota_mode must be rejected")
	}
}

func TestProbeDelayRange(t *testing.T) {
	setEnv(t, map[string]string{"T": "t", "K": "k"})
	yaml := `
client_tokens: [{ name: c, token_env: T }]
providers: { p1: { kind: openai-compatible, base_url: https://x/v1, probe_delay_ms: 99999, keys: [{env: K}] } }
`
	if _, err := Load(writeTemp(t, yaml)); err == nil {
		t.Error("probe_delay_ms above 10000 must be rejected (it exists to pace probing, not stall it)")
	}
}

func TestListenValidation(t *testing.T) {
	setEnv(t, map[string]string{"T": "t", "K": "k"})
	prov := `
client_tokens: [{ name: c, token_env: T }]
providers: { p1: { kind: openai-compatible, base_url: https://x/v1, keys: [{env: K}] } }
`
	for _, bad := range []string{"nohostport", "127.0.0.1:"} {
		if _, err := Load(writeTemp(t, "listen: \""+bad+"\"\n"+prov)); err == nil {
			t.Errorf("listen %q must be rejected", bad)
		}
	}
	// default applies when omitted
	cfg, err := Load(writeTemp(t, prov))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != DefaultListen {
		t.Errorf("listen default = %q, want %q", cfg.Listen, DefaultListen)
	}
	if cfg.DataDir != DefaultDataDir {
		t.Errorf("data_dir default = %q, want %q", cfg.DataDir, DefaultDataDir)
	}
}
