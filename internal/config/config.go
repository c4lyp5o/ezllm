// Package config loads and validates ezllm's YAML configuration.
//
// The config file is BOOTSTRAP ONLY: it seeds accounts and client tokens into
// SQLite on first boot. Provider keys are entered through the dashboard; client
// tokens continue to use env-var indirection and Load reports missing vars by
// NAME, never by value.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultListen is loopback-only unless EZLLM_ADDR / -addr says otherwise.
const DefaultListen = "127.0.0.1:20129"

// DefaultDataDir is repo-relative so Docker can bind-mount ./data (Calypso #5).
const DefaultDataDir = "data"

// namespaceRE constrains account namespaces: lowercase alnum plus _ and -,
// 1-32 chars, and crucially NO '/' (a slash would collide with the
// "<namespace>/<model>" routing form).
var namespaceRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// validKinds mirrors provider.ValidKinds, duplicated as strings so the config
// package does not import provider (keeps validation dependency-free).
var validKinds = []string{"opencode-go", "openai-compatible", "anthropic-compatible"}

type TokenRef struct {
	Name     string `yaml:"name"`
	TokenEnv string `yaml:"token_env"`
	Roles    string `yaml:"roles"` // csv: infer,admin (default "infer")
}

type KeyRef struct {
	Label string `yaml:"label"`
}

// Provider seeds one account. `namespace` is how clients address it:
// "<namespace>/<model>".
type Provider struct {
	Namespace             string            `yaml:"namespace"`
	Kind                  string            `yaml:"kind"` // opencode-go | openai-compatible | anthropic-compatible
	BaseURL               string            `yaml:"base_url"`
	Keys                  []KeyRef          `yaml:"keys"`
	RequiresSessionHeader *bool             `yaml:"requires_session_header,omitempty"`
	ProbeDelayMs          int               `yaml:"probe_delay_ms"`
	QuotaMode             string            `yaml:"quota_mode"`
	CustomHeaders         map[string]string `yaml:"custom_headers"`
}

// RequiresSession reports whether the adapter must inject x-opencode-session.
// Defaults to true for opencode-go (verified: missing header -> 400
// MissingSessionID) and false otherwise.
func (p Provider) RequiresSession() bool {
	if p.RequiresSessionHeader != nil {
		return *p.RequiresSessionHeader
	}
	return p.Kind == "opencode-go"
}

type Ledger struct {
	BatchSize int    `yaml:"batch_size"`
	BatchWait string `yaml:"batch_wait"`
}

type Cooldown struct {
	On429     string `yaml:"on_429"`
	On5xx     string `yaml:"on_5xx"`
	MaxPerKey int    `yaml:"max_per_key"`
}

type Config struct {
	Listen             string              `yaml:"listen"`
	DataDir            string              `yaml:"data_dir"`
	MasterKeyFile      string              `yaml:"master_key_file"`
	MaxBodyMiB         int                 `yaml:"max_body_mib"`
	UpstreamTimeoutStr string              `yaml:"upstream_timeout"`
	ClientTokens       []TokenRef          `yaml:"client_tokens"`
	Providers          map[string]Provider `yaml:"providers"`
	Ledger             Ledger              `yaml:"ledger"`
	Cooldown           Cooldown            `yaml:"cooldown"`

	// derived
	UpstreamTimeout time.Duration
	LedgerBatchWait time.Duration
}

// Load reads, parses and validates the config file at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate checks structural integrity and applies defaults. It collects every
// problem into one error so a fresh checkout reports all issues in a single run.
func (c *Config) Validate() error {
	var errs []string

	// --- listen ---
	if c.Listen == "" {
		c.Listen = DefaultListen
	} else if !strings.Contains(c.Listen, ":") || strings.HasSuffix(c.Listen, ":") {
		errs = append(errs, fmt.Sprintf("listen: %q must be host:port", c.Listen))
	}

	// --- data dir / master key ---
	if c.DataDir == "" {
		c.DataDir = DefaultDataDir
	}
	// master_key_file may be empty (env or default path is used); only validate
	// its SHAPE when given, and never read it here — main.go owns key loading.
	if c.MasterKeyFile != "" && strings.ContainsAny(c.MasterKeyFile, "\n\t") {
		errs = append(errs, "master_key_file: must be a single path")
	}

	// --- body cap ---
	if c.MaxBodyMiB <= 0 {
		c.MaxBodyMiB = 32
	} else if c.MaxBodyMiB > 256 {
		errs = append(errs, "max_body_mib: above 256 is almost certainly a mistake")
	}

	// --- upstream timeout ---
	if c.UpstreamTimeoutStr == "" {
		c.UpstreamTimeout = 0 // unbounded: streaming responses have no natural deadline
	} else if d, err := time.ParseDuration(c.UpstreamTimeoutStr); err != nil {
		errs = append(errs, fmt.Sprintf("upstream_timeout: %v", err))
	} else {
		c.UpstreamTimeout = d
	}

	// --- client tokens ---
	if len(c.ClientTokens) == 0 {
		errs = append(errs, "client_tokens: at least one required")
	}
	seenNames := map[string]bool{}
	seenEnvs := map[string]bool{}
	for _, t := range c.ClientTokens {
		if t.Name == "" || t.TokenEnv == "" {
			errs = append(errs, "client_tokens: each entry needs name + token_env")
			continue
		}
		if seenNames[t.Name] {
			errs = append(errs, fmt.Sprintf("client_tokens: duplicate name %q", t.Name))
		}
		seenNames[t.Name] = true
		// Two tokens sharing one env var would make attribution ambiguous.
		if seenEnvs[t.TokenEnv] {
			errs = append(errs, fmt.Sprintf("client_tokens: env %s reused by %q (attribution would be ambiguous)", t.TokenEnv, t.Name))
		}
		seenEnvs[t.TokenEnv] = true
		if os.Getenv(t.TokenEnv) == "" {
			errs = append(errs, fmt.Sprintf("client_tokens[%s]: env %s is not set", t.Name, t.TokenEnv))
		}
		for _, role := range strings.Split(orDefault(t.Roles, "infer"), ",") {
			switch strings.TrimSpace(role) {
			case "infer", "admin":
			default:
				errs = append(errs, fmt.Sprintf("client_tokens[%s]: unknown role %q (want infer|admin)", t.Name, role))
			}
		}
	}

	// --- providers ---
	if len(c.Providers) == 0 {
		errs = append(errs, "providers: at least one required")
	}
	seenNS := map[string]string{} // namespace -> provider name
	for name, p := range c.Providers {
		if !strings.HasPrefix(p.BaseURL, "https://") && !strings.HasPrefix(p.BaseURL, "http://") {
			errs = append(errs, fmt.Sprintf("providers[%s]: base_url must be an absolute http(s) URL, got %q", name, p.BaseURL))
		}
		if strings.HasSuffix(p.BaseURL, "/") {
			errs = append(errs, fmt.Sprintf("providers[%s]: base_url must not end in '/' (adapter appends paths)", name))
		}
		if p.Kind == "" {
			errs = append(errs, fmt.Sprintf("providers[%s]: kind is required (one of %s)", name, strings.Join(validKinds, ", ")))
		} else if !contains(validKinds, p.Kind) {
			errs = append(errs, fmt.Sprintf("providers[%s]: unknown kind %q (want one of %s)", name, p.Kind, strings.Join(validKinds, ", ")))
		}
		ns := p.Namespace
		if ns == "" {
			ns = name // default: the provider map key
		}
		if !namespaceRE.MatchString(ns) {
			errs = append(errs, fmt.Sprintf("providers[%s]: namespace %q invalid — want ^[a-z0-9][a-z0-9_-]{0,31}$ and no '/'", name, ns))
		}
		if prev, dup := seenNS[ns]; dup {
			errs = append(errs, fmt.Sprintf("providers[%s]: namespace %q already used by %q", name, ns, prev))
		}
		seenNS[ns] = name

		if p.ProbeDelayMs < 0 || p.ProbeDelayMs > 10000 {
			errs = append(errs, fmt.Sprintf("providers[%s]: probe_delay_ms %d out of range 0..10000", name, p.ProbeDelayMs))
		}
		switch p.QuotaMode {
		case "", "probe", "percent", "money", "tokens", "none":
		default:
			errs = append(errs, fmt.Sprintf("providers[%s]: quota_mode %q invalid (want probe|percent|money|tokens|none)", name, p.QuotaMode))
		}
	}

	// --- ledger defaults + validation ---
	if c.Ledger.BatchSize <= 0 {
		c.Ledger.BatchSize = 32
	} else if c.Ledger.BatchSize > 1000 {
		errs = append(errs, "ledger.batch_size: above 1000 delays visibility for no benefit")
	}
	if c.Ledger.BatchWait == "" {
		c.LedgerBatchWait = 250 * time.Millisecond
	} else if d, err := time.ParseDuration(c.Ledger.BatchWait); err != nil {
		errs = append(errs, fmt.Sprintf("ledger.batch_wait: %v", err))
	} else if d < 10*time.Millisecond {
		errs = append(errs, "ledger.batch_wait: below 10ms defeats batching")
	} else {
		c.LedgerBatchWait = d
	}

	// --- cooldown defaults + validation ---
	if c.Cooldown.On429 == "" {
		c.Cooldown.On429 = "300s"
	}
	if c.Cooldown.On5xx == "" {
		c.Cooldown.On5xx = "60s"
	}
	if c.Cooldown.MaxPerKey == 0 {
		c.Cooldown.MaxPerKey = 3
	}
	for _, d := range []struct{ label, v string }{{"on_429", c.Cooldown.On429}, {"on_5xx", c.Cooldown.On5xx}} {
		if _, err := time.ParseDuration(d.v); err != nil {
			errs = append(errs, fmt.Sprintf("cooldown.%s: %v", d.label, err))
		}
	}
	if c.Cooldown.MaxPerKey < 1 {
		errs = append(errs, "cooldown.max_per_key: must be >= 1")
	}

	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("config invalid (%d problem(s)):\n  - %s", len(errs), strings.Join(errs, "\n  - "))
	}
	return nil
}

// Namespace returns the effective namespace for a provider (explicit or the map key).
func (c *Config) Namespace(providerName string) string {
	p, ok := c.Providers[providerName]
	if !ok || p.Namespace == "" {
		return providerName
	}
	return p.Namespace
}

// ProviderNames returns sorted provider names (stable seeding/logging order).
func (c *Config) ProviderNames() []string {
	names := make([]string, 0, len(c.Providers))
	for n := range c.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ClientNames returns the configured client token names (for logs/status).
func (c *Config) ClientNames() []string {
	names := make([]string, 0, len(c.ClientTokens))
	for _, t := range c.ClientTokens {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return names
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// ErrNoProviders is returned by helpers that require at least one provider.
var ErrNoProviders = errors.New("config: no providers configured")
