// Package config loads and validates ezllm's YAML configuration.
//
// Provider definitions are non-secret bootstrap metadata. Credentials are managed in SQLite.
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

// DefaultListen is loopback-only unless overridden with -addr.
const DefaultListen = "127.0.0.1:20129"

// DefaultDataDir is repo-relative so Docker can bind-mount ./data (Calypso #5).
const DefaultDataDir = "data"

// namespaceRE constrains account namespaces: lowercase alnum plus _ and -,
// 1-32 chars, and crucially NO '/' (a slash would collide with the
// "<namespace>/<model>" routing form).
var namespaceRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// validKinds mirrors provider.ValidKinds, duplicated as strings so the config
// package does not import provider (keeps validation dependency-free).
var validKinds = []string{"opencode-go", "openai-compatible", "anthropic-compatible", "gemini-openai"}

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

type Retry struct {
	Retries int `yaml:"retries"`
}

type ModelRefresh struct {
	Interval string `yaml:"interval"`
}

// Translation is the wire-protocol translation layer: a request from an
// anthropic-shaped client (Claude Code, /v1/messages) may be translated onto a
// hop that is PROVEN to speak only the OpenAI shape.
//
// Default ON (global enable, 2026-10-12): the layer is inert on its own —
// eligibility additionally requires a hop whose probed capability says
// proto_openai=true and proto_anthropic is false/null, and those flags are NULL
// until a probe sweep or an operator curation sets them. Set `enabled: false`
// in config.yaml, or EZLLM_TRANSLATION_ENABLED=false (the env wins, kept as the
// rollback lever with no rebuild), to turn it back off.
type Translation struct {
	Enabled bool `yaml:"enabled"`
	// ResponsesModels lists "<namespace>/<model_id>" hops that must be dialed
	// on OpenAI's Responses wire — for models whose chat path refuses
	// tools+reasoning (tokenrouter gpt-6.1-sol) and which have no
	// /v1/messages at all. Applies to chat AND anthropic clients: chat bodies
	// ride the chat↔responses translator, anthropic bodies (Claude Code) the
	// anthropic↔responses one. Empty list (default) = the wire override is
	// inert. This list is its own opt-in; translation.enabled gates the
	// separate anthropic→chat leg.
	ResponsesModels []string `yaml:"responses_models"`
}

type Config struct {
	Listen             string              `yaml:"listen"`
	DataDir            string              `yaml:"data_dir"`
	MaxBodyMiB         int                 `yaml:"max_body_mib"`
	UpstreamTimeoutStr string              `yaml:"upstream_timeout"`
	Providers          map[string]Provider `yaml:"providers"`
	Ledger             Ledger              `yaml:"ledger"`
	Cooldown           Cooldown            `yaml:"cooldown"`
	Retry              Retry               `yaml:"retry"`
	ModelRefresh       ModelRefresh        `yaml:"model_refresh"`
	Translation        Translation         `yaml:"translation"`

	// derived
	UpstreamTimeout time.Duration
	LedgerBatchWait time.Duration
	CooldownOn429   time.Duration
	CooldownOn5xx   time.Duration
}

// Load reads, parses and validates the config file at path.
// Defaults are seeded BEFORE the YAML is decoded so a key that is absent keeps
// its default while an explicit value still wins. translation.enabled is the
// one default that is ON: translated hops need a proven/curated openai-only
// flag, so the layer stays inert until an operator (or a sweep) marks one.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	cfg := Config{Translation: Translation{Enabled: true}}
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

	// --- retry policy ---
	if c.Retry.Retries <= 0 {
		c.Retry.Retries = 3
	} else if c.Retry.Retries > 9 {
		errs = append(errs, "retry.retries: must be between 1 and 9")
	}
	if c.ModelRefresh.Interval == "" {
		c.ModelRefresh.Interval = "6h"
	} else if d, err := time.ParseDuration(c.ModelRefresh.Interval); err != nil || d < time.Minute {
		errs = append(errs, "model_refresh.interval: must be a duration of at least 1m")
	}

	// Providers are managed in SQLite through the dashboard; an empty bootstrap
	// map is valid for a fresh install.
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
		// 60s matches the tightest tier we actually talk to (TokenRouter:
		// "Maximum 5 requests within 1 minutes"). Parking a hop for longer than
		// the budget it is waiting on keeps a healthy hop out for no reason.
		c.Cooldown.On429 = "60s"
	}
	if c.Cooldown.On5xx == "" {
		c.Cooldown.On5xx = "60s"
	}
	if c.Cooldown.MaxPerKey == 0 {
		c.Cooldown.MaxPerKey = 3
	}
	for _, d := range []struct {
		label, v string
		dst      *time.Duration
	}{
		{"on_429", c.Cooldown.On429, &c.CooldownOn429},
		{"on_5xx", c.Cooldown.On5xx, &c.CooldownOn5xx},
	} {
		dur, err := time.ParseDuration(d.v)
		if err != nil {
			errs = append(errs, fmt.Sprintf("cooldown.%s: %v", d.label, err))
			continue
		}
		if dur <= 0 {
			errs = append(errs, fmt.Sprintf("cooldown.%s: must be positive", d.label))
			continue
		}
		*d.dst = dur
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
