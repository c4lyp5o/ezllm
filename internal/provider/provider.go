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

	"github.com/c4lyp5o/ezllm/internal/config"
)

// Kind selects an adapter. Adding a provider = one new Kind + one adapter file.
type Kind string

const (
	KindOpenCodeGo       Kind = "opencode-go"
	KindOpenAICompatible Kind = "openai-compatible"
	KindAnthropicCompat  Kind = "anthropic-compatible"
	// KindGeminiOpenAI is Gemini's OpenAI-compatible endpoint. It rejects a bad
	// key with 400 INVALID_ARGUMENT, not 401, so it opts into AuthRejectStatuser.
	KindGeminiOpenAI Kind = "gemini-openai"
)

// Surface is a wire protocol. Each adapter reports which it can serve, per model.
type Surface string

const (
	SurfaceOpenAI     Surface = "openai"      // POST /chat/completions
	SurfaceAnthropic  Surface = "anthropic"   // POST /messages
	SurfaceResponses  Surface = "responses"   // POST /responses
	SurfaceEmbeddings Surface = "embeddings"  // POST /embeddings
	SurfaceImagesGen  Surface = "images_gen"  // POST /images/generations
	SurfaceAudioTTS   Surface = "audio_tts"   // POST /audio/speech
	SurfaceAudioSTT   Surface = "audio_stt"   // POST /audio/transcriptions
	SurfaceImagesEdit Surface = "images_edit" // POST /images/edits (multipart)
	SurfaceModeration Surface = "moderations" // POST /moderations
	SurfaceRerank     Surface = "rerank"      // POST /rerank (Cohere/Voyage shape)
	SurfaceVideosGen  Surface = "videos_gen"  // POST /videos/generations
	SurfaceMusicGen   Surface = "music_gen"   // POST /music/generations
)

// SurfacePath returns the upstream path (relative to BaseURL) for a surface.
func SurfacePath(s Surface) string {
	switch s {
	case SurfaceAnthropic:
		return "/messages"
	case SurfaceResponses:
		return "/responses"
	case SurfaceEmbeddings:
		return "/embeddings"
	case SurfaceImagesGen:
		return "/images/generations"
	case SurfaceAudioTTS:
		return "/audio/speech"
	case SurfaceAudioSTT:
		return "/audio/transcriptions"
	case SurfaceImagesEdit:
		return "/images/edits"
	case SurfaceModeration:
		return "/moderations"
	case SurfaceRerank:
		return "/rerank"
	case SurfaceVideosGen:
		return "/videos/generations"
	case SurfaceMusicGen:
		return "/music/generations"
	default:
		return "/chat/completions"
	}
}

// UpstreamURL joins an account's base URL with an upstream path for one call.
// Every kind except anthropic-compatible stores a base that already ends at the
// API version (e.g. https://host/v1) and appends the path verbatim.
// Anthropic-compatible accounts follow the Anthropic SDK convention: base_url is
// the bare host (https://api.anthropic.com) and the /v1 version segment is
// added HERE, version-anchoring every path (/v1/messages, /v1/models). A base
// that already ends in /v1 is tolerated — the suffix is not doubled.
func UpstreamURL(baseURL string, kind Kind, path string) string {
	b := strings.TrimSuffix(baseURL, "/")
	if kind == KindAnthropicCompat && !strings.HasSuffix(b, "/v1") {
		b += "/v1"
	}
	return b + path
}

// ValidKinds lists the kinds this build can register (used by config/admin validation).
func ValidKinds() []Kind {
	return []Kind{KindOpenCodeGo, KindOpenAICompatible, KindAnthropicCompat, KindGeminiOpenAI, KindGeminiWeb, KindGitHubCopilot, KindClaudeWeb}
}

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
// UsageConvention is how an upstream Messages (Anthropic-shape) wire counts
// input_tokens relative to cache tokens. It is a per-account OPERATOR fact:
// adapters and wiring cannot reveal it, and a guess silently corrupts cost.
// UsageConventionAuto is "unstated" and reads as exclusive, in the ledger and
// on cross-wire Messages conversions alike.
type UsageConvention string

const (
	UsageConventionAuto      UsageConvention = "auto"      // unstated — never inferred, read as exclusive
	UsageConventionAnthropic UsageConvention = "anthropic" // input_tokens EXCLUDES cache tokens
	UsageConventionTotal     UsageConvention = "total"     // input_tokens INCLUDES cache tokens
)

// ParseUsageConvention validates an operator-supplied value.
func ParseUsageConvention(s string) (UsageConvention, error) {
	switch UsageConvention(s) {
	case UsageConventionAuto, UsageConventionAnthropic, UsageConventionTotal:
		return UsageConvention(s), nil
	}
	return "", fmt.Errorf("usage_convention must be auto|anthropic|total")
}

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
	// RateLimit/RateWindow are the account-WIDE request budget, enforced by the
	// proxy's in-memory limiter before a hop is dispatched. Unlike CapTokens
	// (a token budget enforced by the rules engine at resolution), a request
	// rate is checked at dispatch so an exhausted hop can be SKIPPED and the
	// combo can fail over instead of refusing the whole request.
	RateLimit  int64         // requests per RateWindow; 0 = unlimited
	RateWindow time.Duration // 0 = off
	Notes      string
	// MaxContextTokens (addendum #2/#6): the operator-declared context window
	// for this account. A request whose estimated token count exceeds it is
	// filtered BEFORE dispatch — no paid upstream call, no wasted round-trip.
	// 0 = unknown/unlimited (the default and the backward-compatible value).
	MaxContextTokens int64
	// ManagementKey is an OPTIONAL second credential used by providers whose
	// balance/quota endpoint needs a management (not inference) key, e.g.
	// TokenRouter. Encrypted at rest alongside the inference keys.
	ManagementKey string
	// UsageConvention is the operator-declared input_tokens accounting of the
	// upstream Messages wire (see UsageConvention above). Empty on hand-built
	// values is treated as UsageConventionAuto.
	UsageConvention UsageConvention
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

	// AuthOracle returns the cheapest call that fails on a bad credential
	// (see probe.go). Registration gates the key test on this — never on
	// ListModels, which is public on some providers.
	AuthOracle() AuthProbe

	// CatalogIsPublic reports whether ListModels answers 200 for an invalid
	// key (true for opencode-go). A public catalog is recorded as info only.
	CatalogIsPublic() bool
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

// NewRegistry builds the default registry with all v1 adapters. The
// gemini-web adapter is registered with the DEFAULT listen address as its
// bridge base; callers that know their real listen address (main) re-register
// it — Register overwrites by kind.
func NewRegistry(client *http.Client) *Registry {
	r := &Registry{m: make(map[Kind]Adapter)}
	r.Register(NewOpenCodeGo(client))
	r.Register(NewOpenAICompatible(client))
	r.Register(NewAnthropicCompatible(client))
	r.Register(NewGeminiOpenAI(client))
	r.Register(NewGeminiWeb("http://"+config.DefaultListen+"/internal/geminiweb/v1", client))
	r.Register(NewGitHubCopilot(client))
	r.Register(NewClaudeWeb("http://"+config.DefaultListen+"/internal/claudeweb/v1", client))
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

// Kinds lists the registered kinds (unordered).
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
