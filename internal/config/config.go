// Package config loads and validates ezllm's YAML configuration.
//
// Secrets NEVER live in the config file: every credential is an env-var
// indirection (`token_env`, `keys[].env`) resolved at load time. Load fails
// fast with the names of missing env vars (never their values).
package config

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultListen is loopback-only unless EZLLM_ADDR / -addr says otherwise.
const DefaultListen = "127.0.0.1:20129"

type TokenRef struct {
	Name     string `yaml:"name"`
	TokenEnv string `yaml:"token_env"`
}

type KeyRef struct {
	Env string `yaml:"env"`
}

type Provider struct {
	BaseURL string   `yaml:"base_url"`
	Keys    []KeyRef `yaml:"keys"`
}

// Hop is one provider/model step in a request's routing chain.
type Hop struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

type Alias struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
	Fallback []Hop  `yaml:"fallback,omitempty"`
}

type Ledger struct {
	Path string `yaml:"path"`
}

type Cooldown struct {
	On429     string `yaml:"on_429"`
	On5xx     string `yaml:"on_5xx"`
	MaxPerKey int    `yaml:"max_per_key"`
}

type Config struct {
	Listen       string              `yaml:"listen"`
	ClientTokens []TokenRef          `yaml:"client_tokens"`
	Providers    map[string]Provider `yaml:"providers"`
	Aliases      map[string]Alias    `yaml:"aliases"`
	Ledger       Ledger              `yaml:"ledger"`
	Cooldown     Cooldown            `yaml:"cooldown"`

	// tokenValues maps resolved token value -> client name (in-memory only,
	// never logged). Built by Validate.
	tokenValues map[string]string
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

// Validate checks structural integrity, resolves env indirections and applies
// defaults. It collects every problem into one error so a fresh checkout
// reports all missing env vars in a single run.
func (c *Config) Validate() error {
	var errs []string

	// --- listen ---
	if c.Listen == "" {
		c.Listen = DefaultListen
	} else if !strings.Contains(c.Listen, ":") || strings.HasSuffix(c.Listen, ":") {
		errs = append(errs, fmt.Sprintf("listen: %q must be host:port", c.Listen))
	}

	// --- client tokens ---
	c.tokenValues = make(map[string]string, len(c.ClientTokens))
	if len(c.ClientTokens) == 0 {
		errs = append(errs, "client_tokens: at least one required")
	}
	for _, t := range c.ClientTokens {
		if t.Name == "" || t.TokenEnv == "" {
			errs = append(errs, "client_tokens: each entry needs name + token_env")
			continue
		}
		v := os.Getenv(t.TokenEnv)
		if v == "" {
			errs = append(errs, fmt.Sprintf("client_tokens[%s]: env %s is not set", t.Name, t.TokenEnv))
			continue
		}
		if _, dup := c.tokenValues[v]; dup {
			errs = append(errs, fmt.Sprintf("client_tokens[%s]: duplicate token value", t.Name))
			continue
		}
		c.tokenValues[v] = t.Name
	}

	// --- providers ---
	if len(c.Providers) == 0 {
		errs = append(errs, "providers: at least one required")
	}
	for name, p := range c.Providers {
		if !strings.HasPrefix(p.BaseURL, "https://") && !strings.HasPrefix(p.BaseURL, "http://") {
			errs = append(errs, fmt.Sprintf("providers[%s]: base_url must be an absolute http(s) URL, got %q", name, p.BaseURL))
		}
		if len(p.Keys) == 0 {
			errs = append(errs, fmt.Sprintf("providers[%s]: at least one key required", name))
		}
		for i, k := range p.Keys {
			if k.Env == "" {
				errs = append(errs, fmt.Sprintf("providers[%s].keys[%d]: env is required", name, i))
				continue
			}
			if os.Getenv(k.Env) == "" {
				errs = append(errs, fmt.Sprintf("providers[%s].keys[%d]: env %s is not set", name, i, k.Env))
			}
		}
	}

	// --- aliases ---
	if len(c.Aliases) == 0 {
		errs = append(errs, "aliases: at least one required")
	}
	for name, a := range c.Aliases {
		if a.Model == "" {
			errs = append(errs, fmt.Sprintf("aliases[%s]: model is required", name))
		}
		if _, ok := c.Providers[a.Provider]; !ok {
			errs = append(errs, fmt.Sprintf("aliases[%s]: unknown provider %q", name, a.Provider))
		}
		for i, h := range a.Fallback {
			if _, ok := c.Providers[h.Provider]; !ok {
				errs = append(errs, fmt.Sprintf("aliases[%s].fallback[%d]: unknown provider %q", name, i, h.Provider))
			}
			if h.Model == "" {
				errs = append(errs, fmt.Sprintf("aliases[%s].fallback[%d]: model is required", name, i))
			}
		}
	}

	// --- ledger defaults ---
	if c.Ledger.Path == "" {
		c.Ledger.Path = "data/ezllm.sqlite"
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
	for label, d := range map[string]string{"on_429": c.Cooldown.On429, "on_5xx": c.Cooldown.On5xx} {
		if _, err := time.ParseDuration(d); err != nil {
			errs = append(errs, fmt.Sprintf("cooldown.%s: %v", label, err))
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

// Authenticate resolves a bearer token to its client name (constant-time per
// candidate). Returns "" when the token is unknown.
func (c *Config) Authenticate(token string) string {
	name := ""
	for v, n := range c.tokenValues {
		if len(v) == len(token) && constantTimeEq(v, token) {
			name = n // keep scanning: no early exit on match
		}
	}
	return name
}

// constantTimeEq compares equal-length strings without early exit.
func constantTimeEq(a, b string) bool {
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// AliasNames returns sorted alias names (stable /v1/models output).
func (c *Config) AliasNames() []string {
	names := make([]string, 0, len(c.Aliases))
	for n := range c.Aliases {
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
