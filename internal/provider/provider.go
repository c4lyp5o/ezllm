// Package provider defines the adapter contract and implements the v1 kinds.
//
// An adapter owns everything provider-specific: how to authenticate, which
// header quirks exist (opencode-go requires x-opencode-session), how to read
// quota, and how to parse usage. The proxy layer stays provider-agnostic.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Kind selects an adapter. Adding a provider = one new Kind + one adapter file.
type Kind string

const (
	KindOpenCodeGo       Kind = "opencode-go"
	KindOpenAICompatible Kind = "openai-compatible"
	KindAnthropicCompat  Kind = "anthropic-compatible"
)

// Surface is a wire protocol. Each adapter reports which it can serve, per model.
type Surface string

const (
	SurfaceOpenAI    Surface = "openai"    // POST /chat/completions
	SurfaceAnthropic Surface = "anthropic" // POST /messages
	SurfaceResponses Surface = "responses" // POST /responses
)

// SurfacePath returns the upstream path (relative to BaseURL) for a surface.
func SurfacePath(s Surface) string {
	switch s {
	case SurfaceAnthropic:
		return "/messages"
	case SurfaceResponses:
		return "/responses"
	default:
		return "/chat/completions"
	}
}

// ValidKinds lists the kinds this build can register (used by config/admin validation).
func ValidKinds() []Kind { return []Kind{KindOpenCodeGo, KindOpenAICompatible, KindAnthropicCompat} }

// ParseKind validates a kind string.
func ParseKind(s string) (Kind, error) {
	k := Kind(strings.TrimSpace(s))
	for _, v := range ValidKinds() {
		if k == v {
			return k, nil
		}
	}
	return "", fmt.Errorf("provider: unknown kind %q (want one of %v)", s, ValidKinds())
}

// Account is the runtime view of a registered provider account. The store layer
// builds this; adapters consume it. Key material is already DECRYPTED here and
// must never be logged.
type Account struct {
	ID                    int64
	Name                  string
	Namespace             string
	Kind                  Kind
	BaseURL               string
	Enabled               bool
	RequiresSessionHeader bool
	CustomHeaders         map[string]string
	ProbeDelay            time.Duration
	QuotaMode             string // probe|percent|money|tokens|none
	CapWindow             string // 5h|daily|weekly|monthly
	CapTokens             int64  // 0 = unlimited
	Notes                 string
}

// Adapter translates between ezllm's generic request path and one provider kind.
type Adapter interface {
	Kind() Kind

	// PrepareRequest mutates the outbound request: auth headers, provider
	// quirks (session ids), and stripping of client-supplied headers that
	// must not be forwarded. body has already had `model` swapped.
	PrepareRequest(ctx context.Context, req *http.Request, acct Account, key string, surface Surface) error

	// StripHeaders lists request headers that must NOT be forwarded upstream.
	StripHeaders() []string

	// ReadQuota fetches provider-reported usage/quota. Returns QuotaUnsupported
	// when the provider has no such endpoint — that is not an error.
	ReadQuota(ctx context.Context, acct Account, key string) (Quota, error)

	// ListModels fetches the upstream catalog.
	ListModels(ctx context.Context, acct Account, key string) ([]ModelInfo, error)
}

// ModelInfo is one catalog entry.
type ModelInfo struct {
	ID      string
	OwnedBy string
	Created int64
	Display string
}

// Quota is the discriminated quota result. Kind decides which fields are
// meaningful; Raw always carries the verbatim JSON so a new provider shape is
// data, not a migration.
type Quota struct {
	Kind   QuotaKind
	Raw    string
	ReadAt time.Time

	// percent shape (opencode-go: GET /v1/usage)
	PercentRolling, PercentWeekly, PercentMonthly    *float64
	RollingResetsAt, WeeklyResetsAt, MonthlyResetsAt string

	// money shape (ssn-gpt / space.stationine: GET /v1/usage)
	Balance     *float64
	BalanceUnit string
	CostTotal   *float64
	CostToday   *float64
	TokensToday *int64
	ReqsToday   *int64
}

// QuotaKind discriminates the provider's quota vocabulary.
type QuotaKind string

const (
	QuotaPercent QuotaKind = "percent"
	QuotaMoney   QuotaKind = "money"
	QuotaTokens  QuotaKind = "tokens"
	QuotaNone    QuotaKind = "none"
	QuotaError   QuotaKind = "error"
)

// ErrQuotaUnsupported signals "this provider has no quota endpoint". Callers
// record kind='none' rather than treating it as a failure.
var ErrQuotaUnsupported = errors.New("provider: quota endpoint unsupported")

// Registry maps Kind -> Adapter.
type Registry struct {
	m map[Kind]Adapter
}

// NewRegistry builds the default registry with all v1 adapters.
func NewRegistry(client *http.Client) *Registry {
	r := &Registry{m: make(map[Kind]Adapter)}
	r.Register(NewOpenCodeGo(client))
	r.Register(NewOpenAICompatible(client))
	r.Register(NewAnthropicCompatible(client))
	return r
}

// Register adds an adapter (used by tests and future kinds).
func (r *Registry) Register(a Adapter) { r.m[a.Kind()] = a }

// Get returns the adapter for a kind.
func (r *Registry) Get(k Kind) (Adapter, error) {
	if a, ok := r.m[k]; ok {
		return a, nil
	}
	return nil, fmt.Errorf("provider: no adapter registered for kind %q", k)
}

// HTTPClient returns the shared client (adapters hold it).
func (r *Registry) Kinds() []Kind {
	out := make([]Kind, 0, len(r.m))
	for k := range r.m {
		out = append(out, k)
	}
	return out
}

// --- shared helpers used by adapters ---

// jsonFloat digs a float out of a nested map by dot path ("usage.weekly.percent").
func jsonFloat(m map[string]any, path string) *float64 {
	cur := m
	parts := strings.Split(path, ".")
	for i, p := range parts {
		v, ok := cur[p]
		if !ok {
			return nil
		}
		if i == len(parts)-1 {
			switch n := v.(type) {
			case float64:
				f := n
				return &f
			case int:
				f := float64(n)
				return &f
			default:
				return nil
			}
		}
		next, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		cur = next
	}
	return nil
}

func jsonStr(m map[string]any, path string) string {
	cur := m
	parts := strings.Split(path, ".")
	for i, p := range parts {
		v, ok := cur[p]
		if !ok {
			return ""
		}
		if i == len(parts)-1 {
			s, _ := v.(string)
			return s
		}
		next, ok := v.(map[string]any)
		if !ok {
			return ""
		}
		cur = next
	}
	return ""
}

func f64ptr(f float64) *float64 { return &f }

func i64ptr(i int64) *int64 { return &i }
