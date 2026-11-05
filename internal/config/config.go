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
var validKinds = []string{"opencode-go", "openai-compatible", "anthropic-compatible", "gemini-openai", "gemini-web", "github-copilot", "claude-web"}

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
	// RequestAttemptBudget is the request-WIDE upstream-attempt budget
	// (addendum #3): same-hop retries and failover hops counted together.
	// 0 = unlimited (legacy behaviour; recorded as unlimited, never as
	// zero-left). Default 8 — the home box's combos are small, and this
	// stops one client request from hammering N hops x retries upstream.
	RequestAttemptBudget int `yaml:"request_attempt_budget"`
	// DispatchDeadline bounds the SELECTION loop only — the time spent
	// choosing and waiting between attempts. The winning attempt keeps its
	// own (deliberately unbounded for streaming) upstream timeout, so this
	// never truncates a live stream. "" = no deadline.
	DispatchDeadlineStr string `yaml:"dispatch_deadline"`
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
// proto_chat=true and proto_anthropic is false/null, and those flags are NULL
// until a probe sweep or an operator curation sets them. Set `enabled: false`
// in config.yaml, or EZLLM_TRANSLATION_ENABLED=false (the env wins, kept as the
// rollback lever with no rebuild), to turn it back off.
//
// enabled is the ONE switch (merged 2026-10-22): it covers the pairwise
// translators and the universal matrix alike. universal_enabled is the
// deprecated former matrix switch; Load folds it into enabled.
type Translation struct {
	Enabled bool `yaml:"enabled"`
	// ResponsesModels lists hops that must be dialed on OpenAI's Responses wire
	// — for models whose chat path refuses tools+reasoning (tokenrouter
	// gpt-6.1-sol) and which have no /v1/messages at all. Applies to chat AND
	// anthropic clients: chat bodies ride the chat↔responses translator,
	// anthropic bodies (Claude Code) the anthropic↔responses one.
	//
	// Each entry is a REGEX matched FULLY against "<namespace>/<model_id>"
	// (both ends anchored), so a plain id matches only itself and a family
	// pattern like `stationine/gpt-6\.1-sol` covers its variants. Compile
	// errors are a boot failure naming the entry. Empty list (default) = the
	// wire override is inert. This list is its own opt-in; translation.enabled
	// gates the separate anthropic→chat leg.
	ResponsesModels []string `yaml:"responses_models"`
	// UniversalEnabled is DEPRECATED (merged into Enabled 2026-10-22). After
	// Load it always equals Enabled. An explicit `universal_enabled: false`
	// still turns translation off (either key can switch it off, so an old
	// rollback keeps working) and records a deprecation warning. A
	// wrong-typed value still fails boot naming the field.
	UniversalEnabled bool `yaml:"universal_enabled"`
}

// Compression holds the request-time compression policy (addendum #5). Both
// values are global policy — per-profile variants would need a schema column
// on compression_profiles; these are the honest defaults until then.
type Compression struct {
	// MinAbsoluteTokens: the new work must remove at least this many
	// estimated tokens before it ships. A ratio alone lets a 3-token saving
	// through — real bytes and cache churn for no measurable benefit.
	MinAbsoluteTokens int64 `yaml:"min_absolute_tokens"`
	// MaxPipelineMS: the CPU limit for the stage loop. A pipeline that spends
	// more than this stops at the next stage boundary and ships what ran.
	MaxPipelineMS int64 `yaml:"max_pipeline_ms"`
	// ShadowPct (addendum #7): the percentage of compressed requests that
	// also run a DISCARDED shadow pass over everything, measuring potential
	// reduction for tuning. The shadow never ships and never contributes to
	// applied savings. 0 = off (the default — it costs CPU).
	ShadowPct int `yaml:"shadow_pct"`
}

type Backup struct {
	Directory  string `yaml:"directory"`
	SecretFile string `yaml:"secret_file"`
}

type Config struct {
	Backup             Backup              `yaml:"backup"`
	Explicit           map[string]bool     `yaml:"-"`
	UsageTZ            string              `yaml:"usage_timezone"`
	PriceRefresh       *bool               `yaml:"price_refresh"`
	AutoProbe          *bool               `yaml:"automatic_probing"`
	Listen             string              `yaml:"listen"`
	DataDir            string              `yaml:"data_dir"`
	MaxBodyMiB         int                 `yaml:"max_body_mib"`
	UpstreamTimeoutStr string              `yaml:"upstream_timeout"`
	Providers          map[string]Provider `yaml:"providers"`
	Ledger             Ledger              `yaml:"ledger"`
	Cooldown           Cooldown            `yaml:"cooldown"`
	Compression        Compression         `yaml:"compression"`
	Retry              Retry               `yaml:"retry"`
	ModelRefresh       ModelRefresh        `yaml:"model_refresh"`
	Translation        Translation         `yaml:"translation"`

	// Warnings are non-fatal findings from Load (deprecated keys); main logs
	// them at boot.
	Warnings []string `yaml:"-"`

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
	cfg := Config{
		Translation: Translation{Enabled: true, UniversalEnabled: true},
		Retry:       Retry{RequestAttemptBudget: 8},
		Compression: Compression{MinAbsoluteTokens: 24, MaxPipelineMS: 250, ShadowPct: 0},
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		// The yaml diagnostic names only the type, never the key. When the
		// offender is a wrong-typed translation.universal_enabled, say so.
		if uerr := universalValueError(raw); uerr != nil {
			return nil, uerr
		}
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	var tree map[string]any
	_ = yaml.Unmarshal(raw, &tree)
	cfg.Explicit = map[string]bool{}
	var walk func(string, map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, v := range m {
			name := prefix + k
			if child, ok := v.(map[string]any); ok {
				walk(name+".", child)
			} else {
				cfg.Explicit[name] = true
			}
		}
	}
	walk("", tree)
	if universalKeySet(raw) {
		cfg.Warnings = append(cfg.Warnings,
			"translation.universal_enabled is deprecated: translation.enabled is the one switch now")
	}
	cfg.Translation.Enabled = cfg.Translation.Enabled && cfg.Translation.UniversalEnabled
	cfg.Translation.UniversalEnabled = cfg.Translation.Enabled
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// universalKeySet reports whether config.yaml names the deprecated
// translation.universal_enabled key at all.
func universalKeySet(raw []byte) bool {
	var tree map[string]any
	if yaml.Unmarshal(raw, &tree) != nil {
		return false
	}
	sec, ok := tree["translation"].(map[string]any)
	if !ok {
		return false
	}
	_, ok = sec["universal_enabled"]
	return ok
}

// universalValueError reports a non-boolean translation.universal_enabled as
// a field-named error so the operator knows which key to fix.
func universalValueError(raw []byte) error {
	var tree map[string]any
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		return nil
	}
	sec, ok := tree["translation"].(map[string]any)
	if !ok {
		return nil
	}
	v, ok := sec["universal_enabled"]
	if !ok || v == nil {
		return nil
	}
	if _, isBool := v.(bool); isBool {
		return nil
	}
	return fmt.Errorf("config: translation.universal_enabled must be a boolean (true/false), got %T", v)
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
	// request-wide attempt budget: 0 = unlimited, max 64 (default 8, seeded
	// before decode so an absent key keeps it while an explicit 0 overrides).
	if c.Retry.RequestAttemptBudget < 0 || c.Retry.RequestAttemptBudget > 64 {
		errs = append(errs, "retry.request_attempt_budget: must be between 0 (unlimited) and 64")
	}
	if c.Retry.DispatchDeadlineStr != "" {
		if d, err := time.ParseDuration(c.Retry.DispatchDeadlineStr); err != nil || d < time.Second {
			errs = append(errs, "retry.dispatch_deadline: must be a duration of at least 1s")
		}
	}
	// compression policy: absolute floor 0..100000 tokens, CPU cap 1..5000ms.
	if c.Compression.MinAbsoluteTokens < 0 || c.Compression.MinAbsoluteTokens > 100000 {
		errs = append(errs, "compression.min_absolute_tokens: must be between 0 and 100000")
	}
	if c.Compression.MaxPipelineMS < 0 || c.Compression.MaxPipelineMS > 5000 {
		errs = append(errs, "compression.max_pipeline_ms: must be between 0 and 5000")
	}
	if c.Compression.ShadowPct < 0 || c.Compression.ShadowPct > 100 {
		errs = append(errs, "compression.shadow_pct: must be between 0 and 100")
	}
	if c.ModelRefresh.Interval == "" {
		c.ModelRefresh.Interval = "12h"
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

	// --- translation validation ---
	// responses_models entries are REGEXES matched fully against
	// "<ns>/<model>" (the router compiles the same list). A pattern that does
	// not compile is a boot error naming the entry — a silently-dropped entry
	// would leave an operator wondering why a hop still rides chat.
	for i, p := range c.Translation.ResponsesModels {
		if strings.TrimSpace(p) == "" {
			errs = append(errs, fmt.Sprintf("translation.responses_models[%d]: empty pattern", i))
			continue
		}
		if _, err := regexp.Compile(`\A(?:` + p + `)\z`); err != nil {
			errs = append(errs, fmt.Sprintf("translation.responses_models[%d]: %q is not a valid regexp: %v", i, p, err))
		}
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

// ErrNoProviders is returned by helpers that require at least one provider.
var ErrNoProviders = errors.New("config: no providers configured")
