// Package server exposes ezllm's HTTP surface: three inference protocols
// (OpenAI chat, Anthropic messages, OpenAI Responses) plus /healthz and the
// admin API.
//
// Middleware stack: accessLog -> recoverer -> mux. The statusWriter forwards
// Flush so streaming responses stay unbuffered end to end.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/c4lyp5o/ezllm/internal/compress"
	"github.com/c4lyp5o/ezllm/internal/githubcopilot"
	"github.com/c4lyp5o/ezllm/internal/jsonbody"
	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/registration"
	"github.com/c4lyp5o/ezllm/internal/router"
	"github.com/c4lyp5o/ezllm/internal/rules"
	"github.com/c4lyp5o/ezllm/internal/store"
)

type ctxKey int

const reqInfoKey ctxKey = 1

// reqInfo is created per-request by the access log (outermost) and mutated
// downstream by auth/routing, so the log line can report attribution.
// Pointer semantics: same goroutine, set before the log line is written.
type reqInfo struct {
	client  string
	account string
	model   string
	surface string
}

// Auth resolves a bearer token to a client name + roles. Implemented by the store.
type Auth interface {
	ClientByToken(ctx context.Context, token string) (name string, roles []string, err error)
}

type Server struct {
	auth       Auth
	resolver   *router.Resolver
	dispatcher *proxy.Dispatcher
	db         *store.DB
	registry   *provider.Registry
	tester     *registration.Tester
	// rulesEngine is the eligibility engine (M5). Admin writes to model_rules
	// reload it so a new cap/window applies on the next request instead of
	// waiting for the background ticker. Nil in unit tests that don't care.
	rulesEngine *rules.Engine
	// sessMemo is the committed-prefix ledger (WS-C, scoped): it replays
	// already-shipped transforms so accepted history never flip-flops back
	// to original bytes as a conversation grows. Process-local, TTL'd,
	// session+profile scoped (internal/compress/sessionmemo.go).
	sessMemo *compress.SessionMemo
	// compression policy (addendum #5): the request-time gates for the
	// absolute floor and the CPU cap. Zero values = no gate.
	compressionMinAbs int64
	compressionMaxMS  int64
	// shadowSampler decides whether a request runs the discarded shadow pass
	// (addendum #7). Injectable for tests; nil = never.
	shadowSampler func() bool
	log           *slog.Logger
	mux           *http.ServeMux
	started       time.Time
	maxBody       int64
	web           fs.FS // dashboard source: embedded dist/ or EZLLM_WEB_DIR
	corsOrigins   []string
	// usageTZ is the default zone for group_by=day. Never nil: UTC unless
	// EZLLM_USAGE_TZ says otherwise (validated at boot).
	usageTZ *time.Location

	// sweeps tracks the in-flight protocol sweeps started by POST
	// /admin/accounts/{id}/sync (the "test all models" action), keyed by
	// account. Without it a caller would fire a multi-minute sweep and have no
	// way to ask whether it is still running or to stop it. The probe writes
	// verdicts per model as it goes, so progress is read from the model list;
	// this registry answers only "running since when" and holds the cancel.
	sweepsMu sync.Mutex
	sweeps   map[int64]*sweepRun
	// comboAutoProbe: a combo write probes its untested hop models
	// (combo_autoprobe.go). Off in unit tests unless they ask for it.
	comboAutoProbe bool
	// pricesWanted is set when a catalog save added models, so the refresh
	// pass prices them now instead of at the next daily price run.
	pricesWanted atomic.Bool
	// priceRefreshOn: StartPriceRefresh ran (EZLLM_PRICE_REFRESH not off),
	// so pricing new models is wanted. Never set in unit tests: no network.
	priceRefreshOn atomic.Bool
	// bg owns detached background work (background.go).
	bg *background
	// reports shares dashboard usage reports across callers (reportcache.go).
	reports *reportCache
	// cspOnce/csp cache the dashboard Content-Security-Policy (spa.go).
	cspOnce sync.Once
	csp     string
	// setupCode gates first-boot setup (dashboard_auth.go).
	setupCode string
	// trustedProxies: whose forwarding headers sourceIP believes.
	trustedProxies []netip.Prefix

	// ghMu guards ghDevices: the in-flight GitHub device flows started from
	// the dashboard (POST /admin/github/device). An entry holds GitHub's
	// device code and — once authorized — the OAuth session, which leaves
	// only through a key add naming the handle (device_handle). The session
	// is never serialized into a response.
	ghMu      sync.Mutex
	ghDevices map[string]*ghDevice
	// ghCfg points the device flow at GitHub. Zero value = production
	// endpoints; tests inject a routed HTTP client so no test ever dials
	// github.com (internal/githubcopilot's own test technique, one level up).
	ghCfg githubcopilot.Config
}

// sweepRun is one in-flight protocol sweep for an account. cancel stops the
// probe loop (already-written verdicts stay — probes are independent).
// deadline is the run's hard stop (budget + grace): past it the entry is
// treated as dead by sweepStatus and replaceable by beginSweep, so no sweep
// can ever wedge its account.
type sweepRun struct {
	cancel   context.CancelFunc
	started  time.Time
	deadline time.Time
	surfaces []string
	probing  int
}

// sweepDeadlineGrace is the slack past a sweep's own budget before its
// registry entry self-heals. Budget already covers every probe plus the
// final batch persist; grace covers the goroutine's wind-down.
const sweepDeadlineGrace = 5 * time.Minute

// Options configures New.
type Options struct {
	Auth        Auth
	Resolver    *router.Resolver
	Dispatcher  *proxy.Dispatcher
	DB          *store.DB
	Registry    *provider.Registry // key tests + sync (optional in unit tests)
	Tester      *registration.Tester
	Rules       *rules.Engine // M5: reload after model_rules writes (optional)
	Log         *slog.Logger
	MaxBodyMiB  int      // request body cap; default 32
	CORSOrigins []string // EZLLM_CORS_ORIGINS: empty = any origin, /v1 only
	// UsageTZ is the default timezone for group_by=day bucketing, from
	// EZLLM_USAGE_TZ. Empty = UTC (the historical behaviour). A per-request
	// ?tz= overrides it. It must already RESOLVE: main.go validates it at
	// startup so a typo is a boot error, not a silent UTC shift.
	UsageTZ string
	// Compression policy (addendum #5): the request-time gates. Zero values
	// mean no gate, which is what unit tests get.
	CompressionMinAbs int64
	CompressionMaxMS  int64
	// ShadowSampler (addendum #7): per-request lambda for the discarded
	// shadow pass. Nil = no shadow.
	ShadowSampler func() bool
	// GitHubDevice configures the dashboard's GitHub Copilot device flow.
	// Zero value = the production GitHub endpoints (public CLI client id);
	// tests inject a routed HTTP client. See internal/githubcopilot.
	GitHubDevice githubcopilot.Config
	// ComboAutoProbe probes a combo's untested hop models, in the background,
	// whenever the combo is created or its hops change. main.go turns it on
	// unless EZLLM_COMBO_AUTOPROBE=off.
	ComboAutoProbe bool
	// SetupCode is the one-time first-boot code POST /admin/setup requires
	// (main.go mints and prints it). Empty = setup refused; the operator
	// restarts for a code or sets EZLLM_DASHBOARD_PASSWORD.
	SetupCode string
	// TrustedProxies are the reverse proxies whose X-Forwarded-For the auth
	// rate limiter believes (clientip.go). Empty = the socket peer only.
	TrustedProxies []netip.Prefix
}

// usageTZOrDefault resolves the day-bucketing zone. Empty or whitespace means
// UTC — the historical behaviour, which must not silently change. A value that
// does not resolve also falls back to UTC *here*, because main.go has already
// rejected it at boot, so reaching this line means a caller hand-built Options;
// the request path still refuses a bad ?tz= outright.
func usageTZOrDefault(tz string) *time.Location {
	if strings.TrimSpace(tz) == "" {
		return time.UTC
	}
	if loc, err := time.LoadLocation(strings.TrimSpace(tz)); err == nil {
		return loc
	}
	return time.UTC
}

func New(opts Options) *Server {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	maxBody := int64(opts.MaxBodyMiB) << 20
	if opts.MaxBodyMiB <= 0 {
		maxBody = 32 << 20
	}
	s := &Server{
		auth: opts.Auth, resolver: opts.Resolver, dispatcher: opts.Dispatcher,
		db: opts.DB, registry: opts.Registry, tester: opts.Tester,
		log: opts.Log, mux: http.NewServeMux(), rulesEngine: opts.Rules,
		started: time.Now(), maxBody: maxBody,
		web: webFS(), corsOrigins: opts.CORSOrigins,
		usageTZ:           usageTZOrDefault(opts.UsageTZ),
		compressionMinAbs: opts.CompressionMinAbs, compressionMaxMS: opts.CompressionMaxMS,
		shadowSampler:  opts.ShadowSampler,
		ghDevices:      map[string]*ghDevice{},
		ghCfg:          opts.GitHubDevice,
		comboAutoProbe: opts.ComboAutoProbe,
		setupCode:      opts.SetupCode,
		trustedProxies: opts.TrustedProxies,
	}
	// A server without a tester cannot run the key test; default one so the
	// admin routes fail loudly at construction rather than nil-panic per request.
	if s.tester == nil && opts.Registry != nil && opts.DB != nil {
		s.tester = registration.NewTester(&http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     30 * time.Second,
			},
		}, opts.Registry, opts.DB)
	}
	// The gemini-web bridge: loopback-internal OpenAI-wire routes the
	// gemini-web adapter rewrites hops to. Distinct patterns from the public
	// routes, so registration order is irrelevant.
	s.routes()
	// Committed-prefix ledger (WS-C): process-local, TTL'd, session+profile
	// scoped. Nil-safe — a nil memo means legacy stateless compression.
	if s.sessMemo == nil {
		s.sessMemo = compress.NewSessionMemo(nil)
	}
	s.bg = newBackground()
	s.reports = newReportCache(reportTTL, time.Now)
	if opts.DB != nil {
		newGeminiWebBridge(opts.DB, "").register(s.mux)
		newClaudeWebBridge(opts.DB, nil).register(s.mux)
	}
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("POST /admin/login", s.handleDashboardLogin)
	s.mux.HandleFunc("POST /admin/logout", s.handleLogout)
	// First-boot setup: public, one-shot, gated by the boot-log setup code.
	s.mux.HandleFunc("GET /admin/setup", s.handleSetupStatus)
	s.mux.HandleFunc("POST /admin/setup", s.handleSetup)
	s.mux.HandleFunc("GET /v1/models", s.authed(s.handleModels))
	// Public price catalog (infer token): current/historical rates per
	// account+model, versioned by effective_from.
	s.mux.HandleFunc("GET /v1/prices", s.authed(s.handleCurrentPrices))
	s.mux.HandleFunc("POST /v1/chat/completions", s.authed(s.inference(provider.SurfaceOpenAI)))
	s.mux.HandleFunc("POST /v1/messages", s.authed(s.inference(provider.SurfaceAnthropic)))
	s.mux.HandleFunc("POST /v1/responses", s.authed(s.inference(provider.SurfaceResponses)))

	// Phase 1: New inference endpoints
	s.mux.HandleFunc("POST /v1/embeddings", s.authed(s.inference(provider.SurfaceEmbeddings)))
	s.mux.HandleFunc("POST /v1/images/generations", s.authed(s.inference(provider.SurfaceImagesGen)))
	s.mux.HandleFunc("POST /v1/audio/speech", s.authed(s.inference(provider.SurfaceAudioTTS)))
	s.mux.HandleFunc("POST /v1/audio/transcriptions", s.authed(s.inference(provider.SurfaceAudioSTT)))
	s.mux.HandleFunc("POST /v1/images/edits", s.authed(s.inference(provider.SurfaceImagesEdit)))
	s.mux.HandleFunc("POST /v1/moderations", s.authed(s.inference(provider.SurfaceModeration)))
	s.mux.HandleFunc("POST /v1/rerank", s.authed(s.inference(provider.SurfaceRerank)))
	s.mux.HandleFunc("POST /v1/videos/generations", s.authed(s.inference(provider.SurfaceVideosGen)))
	s.mux.HandleFunc("POST /v1/music/generations", s.authed(s.inference(provider.SurfaceMusicGen)))

	// VS Code / Headerless tokenized aliases (OmniRoute compatibility)
	s.mux.HandleFunc("GET /api/v1/vscode/{token}/", s.vscodeAuthed(s.handleModels))
	s.mux.HandleFunc("GET /api/v1/vscode/{token}/models", s.vscodeAuthed(s.handleModels))
	s.mux.HandleFunc("POST /api/v1/vscode/{token}/chat/completions", s.vscodeAuthed(s.inference(provider.SurfaceOpenAI)))
	s.mux.HandleFunc("POST /api/v1/vscode/{token}/responses", s.vscodeAuthed(s.inference(provider.SurfaceResponses)))

	// ── Dashboard account settings ──
	s.mux.HandleFunc("POST /admin/settings/password", s.admin(s.handleDashboardPassword))

	// ── M3 admin API (docs/API.md) ──
	s.mux.HandleFunc("GET /admin/health", s.admin(s.handleAdminHealth))
	s.mux.HandleFunc("GET /admin/usage", s.admin(s.handleUsage))
	s.mux.HandleFunc("GET /admin/overview", s.admin(s.handleOverview))
	s.mux.HandleFunc("GET /admin/requests", s.admin(s.handleRequests))
	s.mux.HandleFunc("GET /admin/stream", s.admin(s.handleStream))
	s.mux.HandleFunc("GET /admin/notifications", s.admin(s.handleNotifications))
	s.mux.HandleFunc("POST /admin/notifications/read", s.admin(s.handleNotificationsRead))

	// Dashboard GitHub device flow: the browser taps Authorize, the server
	// does every other step (code request, poll, mint, session storage) so
	// the OAuth session never reaches a client. The handle a POST returns is
	// then named as device_handle on the normal key-add route.
	// docs/github-copilot-runbook.md § Adding an account.
	s.mux.HandleFunc("POST /admin/github/device", s.admin(s.handleGitHubDeviceStart))
	s.mux.HandleFunc("GET /admin/github/device/{handle}", s.admin(s.handleGitHubDeviceStatus))
	s.mux.HandleFunc("DELETE /admin/github/device/{handle}", s.admin(s.handleGitHubDeviceCancel))

	s.mux.HandleFunc("GET /admin/accounts", s.admin(s.handleAccounts))
	s.mux.HandleFunc("POST /admin/accounts", s.admin(s.handleAccounts))
	s.mux.HandleFunc("GET /admin/accounts/{id}", s.admin(s.handleAccount))
	s.mux.HandleFunc("PATCH /admin/accounts/{id}", s.admin(s.handleAccount))
	s.mux.HandleFunc("DELETE /admin/accounts/{id}", s.admin(s.handleAccount))

	s.mux.HandleFunc("GET /admin/accounts/{id}/keys", s.admin(s.handleListKeys))
	s.mux.HandleFunc("POST /admin/accounts/{id}/keys", s.admin(s.handleAddKey))
	s.mux.HandleFunc("PATCH /admin/accounts/{id}/keys/{keyId}", s.admin(s.handleToggleKey))
	s.mux.HandleFunc("DELETE /admin/accounts/{id}/keys/{keyId}", s.admin(s.handleDeleteKey))
	s.mux.HandleFunc("POST /admin/accounts/{id}/keys/{keyId}/retest", s.admin(s.handleRetest))

	s.mux.HandleFunc("POST /admin/accounts/{id}/sync", s.admin(s.handleSync))
	// Sweep liveness + cancel for the test-all action: the probe is a
	// multi-minute background job, so the operator can ask if it is running
	// and stop it without waiting out the budget.
	s.mux.HandleFunc("GET /admin/accounts/{id}/sync", s.admin(s.handleSyncStatus))
	s.mux.HandleFunc("DELETE /admin/accounts/{id}/sync", s.admin(s.handleSyncStatus))
	s.mux.HandleFunc("GET /admin/accounts/{id}/models", s.admin(s.handleListModels))
	s.mux.HandleFunc("GET /admin/services", s.admin(s.handleServices))
	s.mux.HandleFunc("PUT /admin/services/{surface}", s.admin(s.handleServiceBinding))
	s.mux.HandleFunc("POST /admin/services/{surface}/test", s.admin(s.handleServiceTest))
	s.mux.HandleFunc("POST /admin/services/{surface}/generate", s.admin(s.handleServiceGenerate))
	s.mux.HandleFunc("PUT /admin/accounts/{id}/model-pin", s.admin(s.handleSetModelPin))
	// Curated capability verdicts (operator-set, probe-protected). Same
	// body-carried model id as model-pin: ids can contain "/".
	s.mux.HandleFunc("PUT /admin/accounts/{id}/model-protocol", s.admin(s.handleSetModelProtocol))
	// v8: versioned model prices + derived cost (admin only — like overview,
	// this is where detail that /healthz no longer leaks lives).
	s.mux.HandleFunc("GET /admin/prices", s.admin(s.handleListPrices))
	s.mux.HandleFunc("POST /admin/prices/import", s.admin(s.handleImportPrices))
	s.mux.HandleFunc("POST /admin/prices/refresh", s.admin(s.handleRefreshPrices))
	s.mux.HandleFunc("PUT /admin/accounts/{id}/prices", s.admin(s.handleSetPrice))
	s.mux.HandleFunc("DELETE /admin/accounts/{id}/prices", s.admin(s.handleDeletePrice))
	s.mux.HandleFunc("GET /admin/cost", s.admin(s.handleCost))
	s.mux.HandleFunc("POST /admin/chat", s.admin(s.inference(provider.SurfaceOpenAI)))
	s.mux.HandleFunc("GET /admin/accounts/{id}/quota", s.admin(s.handleAccountQuota))

	s.mux.HandleFunc("GET /admin/combos", s.admin(s.handleCombos))
	s.mux.HandleFunc("POST /admin/combos", s.admin(s.handleCombos))
	s.mux.HandleFunc("GET /admin/combos/{id}", s.admin(s.handleCombo))
	s.mux.HandleFunc("PATCH /admin/combos/{id}", s.admin(s.handleCombo))
	s.mux.HandleFunc("DELETE /admin/combos/{id}", s.admin(s.handleCombo))
	s.mux.HandleFunc("POST /admin/combos/{id}/hops", s.admin(s.handleComboHops))
	s.mux.HandleFunc("PUT /admin/combos/{id}/hops", s.admin(s.handleComboHops))
	s.mux.HandleFunc("POST /admin/combos/{id}/test", s.admin(s.handleComboTest))

	s.mux.HandleFunc("GET /admin/model-rules", s.admin(s.handleModelRules))
	s.mux.HandleFunc("POST /admin/model-rules", s.admin(s.handleModelRules))
	s.mux.HandleFunc("GET /admin/model-rules/{id}", s.admin(s.handleModelRule))
	s.mux.HandleFunc("PATCH /admin/model-rules/{id}", s.admin(s.handleModelRule))
	s.mux.HandleFunc("DELETE /admin/model-rules/{id}", s.admin(s.handleModelRule))

	s.mux.HandleFunc("GET /admin/tokens", s.admin(s.handleTokens))
	s.mux.HandleFunc("POST /admin/tokens", s.admin(s.handleTokens))
	s.mux.HandleFunc("PATCH /admin/tokens/{id}", s.admin(s.handleToken))
	s.mux.HandleFunc("DELETE /admin/tokens/{id}", s.admin(s.handleToken))

	s.mux.HandleFunc("GET /admin/compression-profiles", s.admin(s.handleProfiles))
	s.mux.HandleFunc("POST /admin/compression-profiles", s.admin(s.handleProfiles))
	s.mux.HandleFunc("GET /admin/compression-profiles/{id}", s.admin(s.handleProfile))
	s.mux.HandleFunc("PATCH /admin/compression-profiles/{id}", s.admin(s.handleProfile))
	s.mux.HandleFunc("DELETE /admin/compression-profiles/{id}", s.admin(s.handleProfile))
	s.mux.HandleFunc("GET /admin/quota", s.admin(s.handleQuotas))
	s.mux.HandleFunc("POST /admin/keys/{keyId}/quota", s.admin(s.handleKeyQuota))
	s.mux.HandleFunc("GET /admin/export", s.admin(s.handleExport))

	// Catch-all: the dashboard. Registered last, but Go's mux resolves by
	// specificity, so every route above wins over this — the SPA and the API
	// share the origin the unlock gate promises.
	s.mux.HandleFunc("GET /", s.spa)
}

// Handler wires the middleware stack: access log outermost, recover inside it
// so panics surface as logged 500s, mux innermost.
func (s *Server) Handler() http.Handler {
	inner := s.accessLog(s.recoverer(securityHeaders(s.mux)))
	// A preflight must be answered BEFORE the mux. Go's ServeMux only offers
	// HEAD/OPTIONS on a path when no method-specific pattern matches it; with
	// "POST /v1/chat/completions" registered it answers OPTIONS with 405 before
	// any handler — so CORS headers can never be attached there. Pre-dispatch
	// interception is the only correct place for it.
	return s.withCORS(inner)
}

// securityHeaders sets the headers every response gets: no MIME sniffing
// (an error body is never reinterpreted as HTML or script) and a referrer
// that stays on this origin. The dashboard shell adds CSP and framing rules
// in spa.go.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// withCORS routes: OPTIONS on /v1/* is answered immediately (no body, never
// reaching inference or the ledger); everything else passes to next, with
// CORS headers attached when the origin is allowed. Scope: /v1/* only — see
// cors.go.
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The grant is scoped to the inference surface. /admin stays
		// same-origin on purpose: the dashboard keeps the admin token in
		// localStorage, so letting any origin read it cross-origin would be a
		// real escalation.
		origin := r.Header.Get("Origin")
		onV1 := strings.HasPrefix(r.URL.Path, "/v1/")
		if origin != "" && onV1 && corsAllows(s.corsOrigins, origin) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", corsPreflightHeaders)
			h.Set("Access-Control-Expose-Headers", corsExposedHeaders)
			h.Set("Access-Control-Max-Age", "600")
		}
		// A preflight carries no body, no credential, and must never reach
		// inference or the ledger. It is only granted for /v1/*.
		if r.Method == http.MethodOptions && onV1 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// WrapForTest applies the same middleware stack to an arbitrary handler.
// Tests use it to exercise the recoverer with a deliberately panicking route.
func (s *Server) WrapForTest(h http.Handler) http.Handler {
	return s.accessLog(s.recoverer(h))
}

// ── handlers ────────────────────────────────────────────────────────────────

// handleHealth is public (monitors, Traefik, the compose healthcheck), so it
// reports liveness plus the release version — deliberately the ONLY extra
// field: no DB path, counts, or uptime. Detailed stats are behind the admin
// token at /admin/overview. The version pairing is asserted against
// VERSION.md by version_test.go.
// A failing DB ping returns 503 so the probe still means something.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if s.db != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.db.Ping(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "down", "version": Version})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": Version})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	protocol := r.URL.Query().Get("protocol")
	entries, err := s.db.ListModelEntries(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	data := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		if protocol != "" && !supportsProtocol(e, protocol) {
			continue
		}
		if protocol == "anthropic" {
			// Anthropic's catalog shape: {data:[{type:"model",id,display_name}]}
			data = append(data, map[string]any{"type": "model", "id": e.ID, "display_name": e.ID})
			continue
		}
		m := map[string]any{
			"id":       e.ID,
			"object":   "model",
			"created":  e.Created,
			"owned_by": e.OwnedBy,
		}
		if p := protocolMap(e); p != nil {
			m["protocol"] = p
		}
		data = append(data, m)
	}

	// Enabled combos are routable ids too: a bare name (combos have no
	// namespace, and ValidateCombo rejects any '/' in a name) resolves through
	// the combo path, so it can never collide with a "<ns>/<model>" id. Without
	// them here, a client that builds its picker from /v1/models can never
	// select one — the combo is invisible to that UI even though it routes.
	// Disabled combos are omitted: the resolver refuses them, so listing one
	// would be a lie.
	combos, err := s.db.ListCombos(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	for _, c := range combos {
		if !c.Enabled {
			continue
		}
		if protocol == "anthropic" {
			// Anthropic's shape stays exactly {type,id,display_name}; context_size
			// is advertised only in OpenAI-compatible model metadata.
			data = append(data, map[string]any{"type": "model", "id": c.Name, "display_name": c.Name})
			continue
		}
		hops := make([]map[string]any, 0, len(c.Hops))
		for _, h := range c.Hops {
			hops = append(hops, map[string]any{
				"account_id": h.AccountID, "model": h.ModelID,
				"enabled": h.Enabled == nil || *h.Enabled,
			})
		}
		data = append(data, map[string]any{
			"id": c.Name, "object": "model", "created": 0, "owned_by": "ezllm",
			"context_size": c.ContextSize,
			"combo":        map[string]any{"strategy": c.Strategy, "hops": hops},
		})
	}

	if protocol == "anthropic" {
		writeJSON(w, http.StatusOK, map[string]any{
			"data": data, "has_more": false,
			"first_id": edgeID(data, true), "last_id": edgeID(data, false),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// protocolMap exposes probed per-surface support so a client can tell whether a
// model can serve /v1/messages or /v1/responses (upstream catalogs omit this).
func protocolMap(e store.ModelEntry) map[string]any {
	if e.Protocol.OpenAI == nil && e.Protocol.Anthropic == nil && e.Protocol.Responses == nil {
		return nil
	}
	return map[string]any{
		"openai": e.Protocol.OpenAI, "anthropic": e.Protocol.Anthropic, "responses": e.Protocol.Responses,
	}
}

// supportsProtocol filters by probed capability. Untested (nil) is ALLOWED:
// a fresh account that has not synced yet must still route, and the upstream
// is authoritative — it rejects an unsupported model itself.
func supportsProtocol(e store.ModelEntry, p string) bool {
	switch p {
	case "anthropic":
		return e.Protocol.Anthropic == nil || *e.Protocol.Anthropic
	case "responses":
		return e.Protocol.Responses == nil || *e.Protocol.Responses
	default:
		return e.Protocol.OpenAI == nil || *e.Protocol.OpenAI
	}
}

func edgeID(d []map[string]any, first bool) string {
	if len(d) == 0 {
		return ""
	}
	i := len(d) - 1
	if first {
		i = 0
	}
	s, _ := d[i]["id"].(string)
	return s
}

// maxOutputTokens reads the request's declared output ceiling. OpenAI chat
// uses max_tokens/max_completion_tokens; Responses uses max_output_tokens.
// 0 means "not declared" — the caller reserves nothing rather than guessing.
func maxOutputTokens(m map[string]any) int64 {
	const maxReserve = 10_000_000 // absurd max_tokens must not overflow the budget check
	for _, k := range []string{"max_completion_tokens", "max_tokens", "max_output_tokens"} {
		var n int64
		switch v := m[k].(type) {
		case json.Number:
			if x, err := strconv.ParseInt(v.String(), 10, 64); err == nil {
				n = x
			}
		case float64:
			n = int64(v)
		case int64:
			n = v
		case int:
			n = int64(v)
		}
		if n > 0 {
			if n > maxReserve {
				return maxReserve
			}
			return n
		}
	}
	return 0
}

// inference builds the handler for one wire protocol. All three share the same
// body: read -> resolve -> swap model -> forward -> ledger.
func (s *Server) inference(surface provider.Surface) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// Audio STT and image edits are multipart/form-data, not JSON: the
		// model lives in a form field, not a JSON key, so the generic JSON
		// reader below would reject it.
		if surface == provider.SurfaceAudioSTT || surface == provider.SurfaceImagesEdit {
			s.inferenceMultipart(w, r, surface)
			return
		}
		// The deadline is lifted only after a complete read (see limitBodyRead).
		liftDeadline := limitBodyRead(w, inferenceBodyTimeout)
		body, err := io.ReadAll(io.LimitReader(r.Body, s.maxBody+1))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_request_error", "could not read body: "+err.Error())
			return
		}
		// The extra byte detects an oversized body: a LimitReader alone stops
		// silently at the cap and the truncated bytes would be parsed (or, for
		// multipart, forwarded) as if they were the whole request.
		if int64(len(body)) > s.maxBody {
			writeErr(w, http.StatusRequestEntityTooLarge, "invalid_request_error",
				fmt.Sprintf("request body exceeds %d bytes", s.maxBody))
			return
		}
		if len(body) == 0 {
			writeErr(w, http.StatusBadRequest, "invalid_request_error", "empty body")
			return
		}
		liftDeadline()

		// Single-parse dispatch (docs/single-parse-dispatch-design.md): one
		// decode of the client body serves model extraction, compression,
		// shape capture and every hop's model swap. Decode problems are not
		// checked here — each consumer keeps its own documented decision
		// (extractModel answers 400 for them, exactly as before).
		doc := jsonbody.Open(body)

		requested, err := extractModelDoc(doc)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		client := clientName(r)
		cip := clientIP(r)
		if requested == "" {
			var ok bool
			requested, ok = s.resolveServiceRequestModel(w, r, surface, start, client, cip)
			if !ok {
				return
			}
		}
		// Per-key model allowlist, checked before a combo is expanded.
		if !s.allowModel(w, r, client, requested) {
			return
		}
		// Reserve the request's max output tokens against the key budget
		// before dispatch. Once dispatched, the request's ledger row owns the
		// reservation and the flush ends it in the transaction that counts
		// the usage; releasing on return instead left a window where the
		// spent budget was neither reserved nor counted. The defer covers
		// every exit that never reaches dispatch.
		var reservation int64
		if s.db != nil {
			if n := maxOutputTokens(doc.Map()); n > 0 {
				if id, ok, rerr := s.db.ReserveClientBudget(r.Context(), client, n); rerr != nil {
					// No token row (the dashboard's own chat) means no budget,
					// not a failure worth a warning on every request.
					if !errors.Is(rerr, store.ErrNotFound) {
						slog.Warn("client budget reservation failed", "client", client, "err", rerr)
					}
				} else if !ok {
					writeErr(w, http.StatusTooManyRequests, "rate_limited",
						fmt.Sprintf("token budget cannot cover max_tokens=%d for %q", n, client))
					return
				} else if id != 0 {
					reservation = id
					defer func() {
						if reservation != 0 {
							s.db.ReleaseClientBudget(context.Background(), reservation)
						}
					}()
				}
			}
		}
		if info := reqInfoOf(r); info != nil {
			info.surface = string(surface)
		}

		// Candidates, not a single route: a combo yields every hop that is
		// enabled AND eligible right now, in strategy order. The dispatcher
		// tries them before committing a byte to the client, so a 429 from
		// hop 1 is invisible when hop 2 answers.
		routes, err := s.resolver.ResolveCandidates(r.Context(), requested, surface, client)
		if err != nil {
			writeResolveErr(w, err)
			s.ledger(store.Call{
				TS: start, Client: client, IP: cip, Surface: store2surface(surface), Alias: requested,
				Model: requested, Status: errorStatus(err), Err: err.Error(),
				// Compression never ran (the request died at resolution), but
				// the stamp still says what WOULD have happened — "off" or the
				// profile name — so the feed never has to guess.
				CompressionProfile: s.compressionProfileOf(r, requested),
			})
			return
		}
		if info := reqInfoOf(r); info != nil {
			info.account = routes[0].Account.Namespace
			info.model = routes[0].Model
		}

		// Compression (M5): resolve the profile (header → combo), run the
		// pipeline ONCE over the body — every candidate then receives the
		// same compressed body, because hops only swap `model`. The shared
		// doc is threaded through so the pipeline reuses the one decode
		// (older code parsed the body again here).
		body, compRes := s.applyCompression(r, requested, body, doc)
		// A shipped rewrite produces NEW bytes, so the shared decode no
		// longer describes them: re-open on that path only (compression
		// almost always declines on hot prefixes, so the extra decode is the
		// rare case). Equal bytes — the normal path — keep the doc.
		if !bytes.Equal(body, doc.Bytes()) {
			doc = jsonbody.Open(body)
		}

		// v8 (M9): shape capture for cacheability research. Runs AFTER
		// compression so the hash describes what actually goes upstream —
		// if a profile rewrites history, that IS a cache break and must
		// measure as one. Best-effort: a failed parse records nothing and
		// changes nothing about the request.
		sh := captureShapeDoc(doc, surface)

		// served is the route that ACTUALLY committed a response (see
		// finishDispatch); the request-shape and compression fields are this
		// surface's own, the rest is shared with the multipart path.
		served, res, ferr := s.dispatcher.ForwardCandidatesDoc(r.Context(), w, r, routes, body, doc)
		call := store.Call{
			TS: start, Client: client, IP: cip, Surface: store2surface(surface),
			// v8 research capture: request shape + operator session key.
			PrefixSHA: sh.PrefixSHA, MsgCount: sh.MsgCount, ToolCount: sh.ToolCount,
			SessionID: sessionOf(r), ReqBytes: sh.ReqBytes,
			ReservationID: reservation,
		}
		reservation = 0 // handed to the ledger row
		attachCompressionMetrics(&call, compRes)
		s.finishDispatch(w, r, call, routes, served, res, ferr)
	}
}

// inferenceMultipart handles the multipart/form-data surfaces
// (/v1/audio/transcriptions, /v1/images/edits), where the model lives in a
// form field rather than a JSON key. The raw multipart body is forwarded
// upstream with the model field swapped inside prepareHopBody.
func (s *Server) inferenceMultipart(w http.ResponseWriter, r *http.Request, surface provider.Surface) {
	start := time.Now()
	// Read the raw multipart body FIRST so it can be forwarded byte-for-byte
	// (ParseMultipartForm would consume the stream).
	liftDeadline := limitBodyRead(w, inferenceBodyTimeout)
	body, err := io.ReadAll(io.LimitReader(r.Body, s.maxBody+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "could not read multipart body: "+err.Error())
		return
	}
	// The extra byte detects an oversized body: a LimitReader alone stops
	// silently at the cap and the truncated bytes would be parsed (or, for
	// multipart, forwarded) as if they were the whole request.
	if int64(len(body)) > s.maxBody {
		writeErr(w, http.StatusRequestEntityTooLarge, "invalid_request_error",
			fmt.Sprintf("request body exceeds %d bytes", s.maxBody))
		return
	}
	if len(body) == 0 {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "empty body")
		return
	}
	liftDeadline()
	// Extract the model from the multipart form (read a copy — the original
	// body bytes stay intact for forwarding).
	requested, err := extractModelMultipart(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	client := clientName(r)
	cip := clientIP(r)
	if requested == "" {
		var ok bool
		requested, ok = s.resolveServiceRequestModel(w, r, surface, start, client, cip)
		if !ok {
			return
		}
	}
	if !s.allowModel(w, r, client, requested) {
		return
	}
	if info := reqInfoOf(r); info != nil {
		info.surface = string(surface)
	}

	routes, err := s.resolver.ResolveCandidates(r.Context(), requested, surface, client)
	if err != nil {
		writeResolveErr(w, err)
		s.ledger(store.Call{
			TS: start, Client: client, IP: cip, Surface: store2surface(surface),
			Alias: requested, Model: requested, Status: errorStatus(err), Err: err.Error(),
		})
		return
	}
	if info := reqInfoOf(r); info != nil {
		info.account = routes[0].Account.Namespace
		info.model = routes[0].Model
	}

	// Forward the raw multipart body; prepareHopBody detects multipart and
	// swaps the model field per hop. The outcome goes through the same
	// finishDispatch as JSON: 429 for a rate-limited combo, hop health, the
	// failover trail.
	served, res, ferr := s.dispatcher.ForwardCandidates(r.Context(), w, r, routes, body)
	s.finishDispatch(w, r, store.Call{TS: start, Client: client, IP: cip, Surface: store2surface(surface)},
		routes, served, res, ferr)
}

// extractModelMultipart reads the "model" form field from a multipart body.
func extractModelMultipart(body []byte) (string, error) {
	return proxy.MultipartField(body, "model")
}

// resolveErrInfo is the one mapping from a resolution failure to its HTTP
// answer: status, error type and the structured extras a client can act on.
// writeResolveErr sends it and errorStatus reads its status for the ledger,
// so the row and the wire can never disagree.
func resolveErrInfo(err error) (status int, typ string, extra map[string]any) {
	var nf *router.ErrNotFound
	var nu *router.ErrUnavailable
	var np *router.ErrPinned
	var nw *router.ErrWireUnsupported
	switch {
	case errors.As(err, &nf):
		return http.StatusNotFound, "model_not_found", map[string]any{"available": nf.Hints}
	case errors.As(err, &np):
		// The operator pinned this model to a surface and the client called
		// a different one. That is a client mistake, not an upstream
		// failure: 400, and the body says which endpoint to use. Never
		// rerouted — switching the path upstream would send the wrong shape.
		return http.StatusBadRequest, "pinned_surface",
			map[string]any{"pinned": np.Pinned, "requested_surface": np.Got}
	case errors.As(err, &nw):
		// Wire-first selection found no wire to dial: the model's own
		// surface is probed impossible and nothing entitles it to another
		// (no responses override, no translation). 400, not a gateway 502.
		return http.StatusBadRequest, "wire_unsupported",
			map[string]any{"requested_surface": string(nw.Wire)}
	case errors.As(err, &nu):
		// A rule (cap / allowed hours) says no — not a routing miss. The
		// cause (a *rules.Refusal) carries WHICH kind of rule refused and,
		// for windows, when the model opens next, so a client can sleep
		// until then instead of blind-retrying.
		extra := map[string]any{"reason": nu.Reason}
		var ref *rules.Refusal
		if errors.As(err, &ref) {
			rule := map[string]any{"kind": ref.Reason.Kind, "detail": ref.Reason.Detail}
			if !ref.Reason.NextAllowed.IsZero() {
				rule["next_allowed"] = ref.Reason.NextAllowed.Format(time.RFC3339)
			}
			extra["rule"] = rule
		}
		return http.StatusTooManyRequests, "model_unavailable", extra
	default:
		return http.StatusBadGateway, "upstream_error", nil
	}
}

// writeResolveErr answers a resolution failure — one mapping for the JSON and
// multipart handlers, so a pin or wire refusal is a 400 on every endpoint
// rather than a 502 on the multipart ones.
func writeResolveErr(w http.ResponseWriter, err error) {
	status, typ, extra := resolveErrInfo(err)
	writeErrExtra(w, status, typ, err.Error(), extra)
}

func errorStatus(err error) int {
	status, _, _ := resolveErrInfo(err)
	return status
}

// maxWindowSpan bounds any [from,to) the admin APIs will aggregate over.
//
// from/to were unbounded: an admin could ask for a GROUP BY spanning the
// entire ledger from its first row to now, and every such request re-scanned
// the whole table — cost that scales with the requested span is cost the caller
// controls, so it is capped exactly like `limit` already is. 400 days is
// deliberately generous for an admin dashboard, and the bound is stated rather
// than silent.
const maxWindowSpan = 400 * 24 * time.Hour

// parseWindow reads from/to (RFC3339) with a default span, and refuses a window
// wider than maxWindowSpan or inverted. The default `to` carries a +1min grace
// because ledger rows are stamped at request start and written asynchronously —
// a strict `ts < now` would silently drop the freshest row.
func parseWindow(w http.ResponseWriter, r *http.Request, defaultSpan time.Duration) (from, to time.Time, ok bool) {
	q := r.URL.Query()
	now := time.Now().UTC()
	from, to = now.Add(-defaultSpan), now.Add(time.Minute)
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_request_error", "from must be RFC3339")
			return time.Time{}, time.Time{}, false
		}
		from = t.UTC()
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_request_error", "to must be RFC3339")
			return time.Time{}, time.Time{}, false
		}
		to = t.UTC()
	}
	if !to.After(from) {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "to must be after from")
		return time.Time{}, time.Time{}, false
	}
	if d := to.Sub(from); d > maxWindowSpan {
		writeErr(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("window of %s exceeds the %s cap — narrow from/to", d, maxWindowSpan))
		return time.Time{}, time.Time{}, false
	}
	return from, to, true
}

func (s *Server) handleAdminHealth(w http.ResponseWriter, r *http.Request) {
	if !hasRole(r, "admin") {
		writeErr(w, http.StatusForbidden, "forbidden", "admin role required")
		return
	}
	var accounts, keys, combos, models, calls int
	var oldest, newest string
	// Errors are RETURNED, not swallowed into a zero. Reporting accounts: 0
	// because a COUNT query failed states an untruth in the one endpoint whose
	// whole job is telling the operator whether the box is healthy.
	q := func(query string, dest *int) error {
		return s.db.Reader().QueryRowContext(r.Context(), query).Scan(dest)
	}
	for _, c := range []struct {
		q    string
		dest *int
	}{
		{`SELECT COUNT(*) FROM accounts`, &accounts},
		{`SELECT COUNT(*) FROM provider_keys`, &keys},
		{`SELECT COUNT(*) FROM combos`, &combos},
		{`SELECT COUNT(*) FROM models`, &models},
		{`SELECT COUNT(*) FROM calls`, &calls},
	} {
		if err := q(c.q, c.dest); err != nil {
			writeErr(w, http.StatusServiceUnavailable, "health_check_failed",
				"could not read ledger counts: "+err.Error())
			return
		}
	}
	if err := s.db.Reader().QueryRowContext(r.Context(),
		`SELECT COALESCE(MIN(ts),''),COALESCE(MAX(ts),'') FROM calls`).Scan(&oldest, &newest); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "health_check_failed",
			"could not read ledger span: "+err.Error())
		return
	}
	// A parked hop is invisible everywhere else: the request never reaches an
	// upstream, so no ledger row is written and the only symptom is a combo
	// quietly using fewer hops. Report which account is being skipped, until
	// when, and why.
	cooldowns := []proxy.Cooldown{}
	if s.dispatcher != nil {
		cooldowns = s.dispatcher.Cooldowns()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":             "ok",
		"schema_version":     s.db.SchemaVersion(),
		"db_path":            s.db.Path(),
		"accounts":           accounts,
		"provider_keys":      keys,
		"models":             models,
		"combos":             combos,
		"ledger_rows":        calls,
		"ledger_span":        map[string]string{"oldest": oldest, "newest": newest},
		"dropped_rows":       store.DroppedCalls(),
		"lost_rows":          store.LostRows(),
		"unmetered_rows":     store.UnmeteredRows(),
		"endpoint_cooldowns": cooldowns,
		"uptime_s":           int(time.Since(s.started).Seconds()),
	})
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if !hasRole(r, "admin") {
		writeErr(w, http.StatusForbidden, "forbidden", "admin role required")
		return
	}
	groupBy := r.URL.Query().Get("group_by")
	// Window: [from, to), default 24h with the inclusive +1min grace (rows are
	// stamped at request start and the ledger is async, so a strict `ts < now`
	// would silently drop the freshest row) and a capped span.
	from, to, ok := parseWindow(w, r, 24*time.Hour)
	if !ok {
		return
	}

	// Day bucketing zone. Precedence: ?tz= > EZLLM_USAGE_TZ > UTC. The env
	// default is what makes this usable without every caller knowing the zone
	// (the point of F9: a deployer sets one variable and the dashboard's day
	// agrees with the rule's day), and an explicit ?tz= wins for ad-hoc checks.
	//
	// Only group_by=day consults it. Every other grouping is unchanged, and a
	// tz with any other group_by is rejected rather than ignored — a silently
	// ignored parameter is how a caller believes a number means something it
	// does not.
	loc := s.usageTZ
	if v := strings.TrimSpace(r.URL.Query().Get("tz")); v != "" {
		if groupBy != "day" {
			writeErr(w, http.StatusBadRequest, "invalid_request_error",
				"tz applies only to group_by=day")
			return
		}
		l, err := time.LoadLocation(v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_request_error",
				fmt.Sprintf("tz %q does not resolve to a time zone on this host (use an IANA name like Asia/Kuala_Lumpur)", v))
			return
		}
		loc = l
	}

	var usedTZ string
	if groupBy == "day" {
		usedTZ = loc.String()
	}
	// Shared across callers for reportTTL (reportcache.go). An open-ended
	// window ("until now", no `to`) is keyed by its from rounded down to the
	// TTL: every poll computes a fresh from, which would otherwise defeat the
	// cache; within the TTL the served window is at most that much older.
	key := fmt.Sprintf("usage|%s|%s|%s|", groupBy, usedTZ, from.Truncate(reportTTL).Format(time.RFC3339))
	if r.URL.Query().Get("to") != "" {
		key = fmt.Sprintf("usage|%s|%s|%s|%s", groupBy, usedTZ, from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano))
	}
	v, age, err := s.reports.do(key, func() (any, error) {
		ctx, cancel := context.WithTimeout(s.bg.ctx, reportTimeout)
		defer cancel()
		var res usageResult
		var err error
		if groupBy == "day" {
			res.rows, err = s.db.UsageDayReportTZ(ctx, from, to, loc)
		} else {
			res.rows, err = s.db.UsageReport(ctx, from, to, groupBy)
		}
		if err != nil {
			return nil, usageErr{http.StatusBadRequest, "invalid_request_error", err}
		}
		// The totals P50 cannot be a sum of group P50s — medians don't add
		// up — so it was never set and the card read "0ms · not measured
		// yet" forever. One separate median over the whole window, same
		// convention as the per-group fillP50 (upper of the two middle values).
		if res.tp50, err = s.db.TotalP50TTFTms(ctx, from, to); err != nil {
			return nil, usageErr{http.StatusInternalServerError, "api_error", err}
		}
		return res, nil
	})
	if err != nil {
		var ue usageErr
		if errors.As(err, &ue) {
			writeErr(w, ue.status, ue.typ, ue.err.Error())
		} else {
			writeErr(w, http.StatusInternalServerError, "api_error", err.Error())
		}
		return
	}
	res := v.(usageResult)
	rows := res.rows // shared with other callers: read only
	w.Header().Set("Age", strconv.Itoa(int(age.Seconds())))
	var totals store.UsageRow
	totals.Key = "total"
	for _, x := range rows {
		totals.Calls += x.Calls
		totals.TokensIn += x.TokensIn
		totals.TokensOut += x.TokensOut
		totals.CachedRead += x.CachedRead
		totals.CachedWrite += x.CachedWrite
		totals.Reasoning += x.Reasoning
		totals.TokensSaved += x.TokensSaved
		totals.AppliedSaved += x.AppliedSaved
		totals.ContextPre += x.ContextPre
		totals.ContextSaved += x.ContextSaved
		totals.Errors += x.Errors
	}
	totals.P50TTFTms = res.tp50
	out := map[string]any{
		"from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339),
		"group_by": orDefault(groupBy, "account"),
		"groups":   rows, "totals": totals,
	}
	// Echo the zone the day buckets were actually computed in. A day-grouped
	// number is meaningless without it: the same rows bucket differently in
	// different zones, and a caller comparing against a rule's refusal has to
	// know which zone produced the figure.
	if groupBy == "day" {
		out["tz"] = usedTZ
	}
	writeJSON(w, http.StatusOK, out)
}

// reportTTL bounds how stale a shared dashboard report may be: under the
// 15s overview poll, so one open tab still sees every refresh as fresh work
// while extra tabs and simultaneous polls share it.
const reportTTL = 10 * time.Second

// reportTimeout bounds one shared report computation. It runs on the server's
// background context, not the first caller's request, so one caller going
// away cannot fail the others waiting on it.
const reportTimeout = 30 * time.Second

// usageResult is one /admin/usage computation, shared through reportCache.
type usageResult struct {
	rows []store.UsageRow
	tp50 int64
}

// usageErr carries the status a failed usage computation answers with.
type usageErr struct {
	status int
	typ    string
	err    error
}

func (e usageErr) Error() string { return e.err.Error() }

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// failoverTrail renders the dispatcher's abandoned-candidate list for the
// ledger err column. The marker word is `failover:` so the dashboard can tell
// the served hop's own failure (which comes first) from the hops that hopped.
func failoverTrail(attempts []proxy.Attempt) string {
	if len(attempts) == 0 {
		return ""
	}
	parts := make([]string, 0, len(attempts))
	for _, a := range attempts {
		if a.Status > 0 {
			parts = append(parts, fmt.Sprintf("failover: %s/%s — %s", a.Namespace, a.Model, a.Note))
		} else {
			parts = append(parts, fmt.Sprintf("failover: %s/%s skipped (%s)", a.Namespace, a.Model, a.Note))
		}
	}
	// Attempt notes are bounded individually; clip the joined string so a
	// pathological candidate list cannot blow the column up. Cut on a rune
	// boundary — the column is TEXT and the dashboard renders it.
	s := strings.Join(parts, "; ")
	if len(s) > 400 {
		cut := 400
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

// ledger records a call without ever blocking the response path.
func (s *Server) ledger(c store.Call) {
	if s.db == nil {
		return
	}
	s.db.RecordCall(c)
}

// ── middleware ──────────────────────────────────────────────────────────────

func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			writeErr(w, http.StatusUnauthorized, "invalid_request_error", "missing bearer token")
			return
		}
		if r, ok := s.authenticate(w, r, token); ok {
			next(w, r)
		}
	}
}

// authenticate is the one client-token gate: identity, expiry, then the
// per-key token budget. Every way a token can arrive (bearer/x-api-key, the
// VS Code path alias) goes through it, so a new check can never cover one
// route and miss another. On refusal it has already written the response
// and reports false; on success it returns r carrying the client's identity.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request, token string) (*http.Request, bool) {
	name, roles, err := s.auth.ClientByToken(r.Context(), token)
	if err != nil || name == "" {
		if errors.Is(err, store.ErrTokenExpired) {
			writeErr(w, http.StatusUnauthorized, "invalid_request_error", "token expired")
			return r, false
		}
		writeErr(w, http.StatusUnauthorized, "invalid_request_error", "invalid bearer token")
		return r, false
	}
	// Per-key token budget. Checked before any upstream work, so an
	// exhausted key costs one indexed lookup and zero provider calls.
	if s.db != nil {
		if budget, berr := s.db.ClientBudgetStatus(r.Context(), name); berr == nil {
			if budget.Limit > 0 && budget.Used >= budget.Limit {
				s.noteBudgetReached(name, budget)
				writeErrExtra(w, http.StatusTooManyRequests, "rate_limited",
					fmt.Sprintf("token budget reached (%d of %d tokens this %s)", budget.Used, budget.Limit, budget.Window),
					map[string]any{"limit": budget.Limit, "used": budget.Used, "window": budget.Window})
				return r, false
			}
		} else if !errors.Is(berr, store.ErrNotFound) {
			slog.Warn("client budget check failed", "client", name, "err", berr)
		}
	}
	ctx := context.WithValue(r.Context(), rolesKey{}, roles)
	ctx = context.WithValue(ctx, clientKey{}, name)
	if info := reqInfoOf(r); info != nil {
		info.client = name
	}
	return r.WithContext(ctx), true
}

type rolesKey struct{}
type clientKey struct{}

func hasRole(r *http.Request, want string) bool {
	roles, _ := r.Context().Value(rolesKey{}).([]string)
	for _, x := range roles {
		if strings.TrimSpace(x) == want {
			return true
		}
	}
	return false
}

// hopAttributableFailure decides whether an attempt FAILED ON THE HOP (so a
// hold_until_failure block must release) or on the REQUEST (so the hold
// survives, because the next request may be perfectly fine).
//
// Hop-attributable: 429 (the hop's budget/window), any 5xx (the hop is broken),
// a transport error (never reached a body), and a translated stream that failed
// or truncated late (bytes came, the answer did not). Not hop-attributable: a
// 2xx/3xx that delivered, and every 4xx — a 400/404/422 is the caller's
// payload or model id, and rotating away from a healthy hop on it would be
// fabricating a health signal.
func hopAttributableFailure(res proxy.Result, ferr error) bool {
	if ferr != nil {
		return true
	}
	if res.Status == 429 || res.Status >= 500 {
		return true
	}
	return !res.StreamCompleted()
}

// clientIP captures the caller address for the ledger's traffic mapping
// (operator decision 2026-10-13, lat/long dropped). Precedence matches the
// usual reverse-proxy contract: X-Real-IP (nginx), then the FIRST address in
// X-Forwarded-For (the client, when the proxy appends), then the socket peer.
// The value is whatever the proxy sent — spoofable on a direct connection, so
// it is attribution, not authentication.
func clientIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Real-Ip")); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); v != "" {
		if i := strings.IndexByte(v, ','); i > 0 {
			v = v[:i]
		}
		return strings.TrimSpace(v)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func clientName(r *http.Request) string {
	s, _ := r.Context().Value(clientKey{}).(string)
	return s
}

func reqInfoOf(r *http.Request) *reqInfo {
	info, _ := r.Context().Value(reqInfoKey).(*reqInfo)
	return info
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic", "err", rec, "path", r.URL.Path, "stack", string(debug.Stack()))
				writeErr(w, http.StatusInternalServerError, "server_error", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusWriter records status/bytes and forwards Flush so SSE streams stay
// unbuffered (invariant: never add buffering on this path).
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Flush forwards to the underlying Flusher (critical for streaming responses).
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the wrapped writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w}
		info := &reqInfo{}
		start := time.Now()
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), reqInfoKey, info)))
		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		// Attribution only — never a token, key, or Authorization value.
		s.log.Info("request",
			"method", r.Method, "path", logPath(r.URL.Path), "status", status,
			"ms", time.Since(start).Milliseconds(), "bytes", sw.bytes,
			"client", info.client, "account", info.account,
			"model", info.model, "surface", info.surface,
		)
	})
}

// ── helpers ─────────────────────────────────────────────────────────────────

// Request-body read budgets. The server sets only ReadHeaderTimeout (no
// global ReadTimeout: a response may stream for minutes), so without these a
// client could trickle a body forever and pin the handler — on /admin/login
// without even authenticating. Vars so tests can shorten them.
var (
	adminBodyTimeout     = 30 * time.Second // JSON admin bodies, ≤1 MiB
	inferenceBodyTimeout = 2 * time.Minute  // up to max_body_mib on a slow link
)

// limitBodyRead puts a deadline on reading this request's body and returns
// the func that lifts it. Call lift after the body was read completely, so
// the budget covers the body and nothing after it (TestStreamOutlivesThe-
// BodyBudget guards that a long response is unaffected). On a failed or
// oversized read, do NOT lift: after the handler returns, net/http drains the
// unread body before it sends the reply, and without the deadline that drain
// waits on the stalled client forever (TestTrickledBodyIsCutOff). Writers
// that cannot take deadlines (httptest recorders) are a no-op.
func limitBodyRead(w http.ResponseWriter, d time.Duration) (lift func()) {
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(d)); err != nil {
		return func() {}
	}
	return func() { _ = rc.SetReadDeadline(time.Time{}) }
}

const vscodePathPrefix = "/api/v1/vscode/"

// logPath is the request path as the access log may show it. The VS Code
// alias carries a live client token as a path segment
// (/api/v1/vscode/{token}/…), so that segment is masked; every other path is
// logged as-is. A reverse proxy in front logs the raw path itself and needs
// its own redaction.
func logPath(p string) string {
	rest, ok := strings.CutPrefix(p, vscodePathPrefix)
	if !ok || rest == "" {
		return p
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return vscodePathPrefix + "***" + rest[i:]
	}
	return vscodePathPrefix + "***"
}

// bearerToken accepts both auth shapes: OpenAI-style Bearer and Anthropic-style
// x-api-key. Claude Code sends the latter.
func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	if k := r.Header.Get("x-api-key"); k != "" {
		return strings.TrimSpace(k)
	}
	return ""
}

func (s *Server) resolveServiceRequestModel(w http.ResponseWriter, r *http.Request, surface provider.Surface, started time.Time, client, ip string) (string, bool) {
	if !store.IsServiceSurface(string(surface)) {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "'model' is required")
		return "", false
	}
	target, err := s.db.ResolveServiceDefault(r.Context(), string(surface))
	if err != nil {
		s.writeStoreErr(w, err)
		return "", false
	}
	if target == "" {
		msg := fmt.Sprintf("%s has no default model configured — bind one in Services or include a model in the request", surface)
		writeErrExtra(w, http.StatusBadRequest, "service_not_configured", msg,
			map[string]any{"service": string(surface), "configure": "/services"})
		s.ledger(store.Call{
			TS: started, Client: client, IP: ip, Surface: store2surface(surface),
			Status: http.StatusBadRequest, Err: msg,
		})
		return "", false
	}
	return target, true
}

// extractModel reads the requested model without disturbing any other field.
// An absent/empty model returns ("", nil): the handler decides per surface
// whether that is a required-model refusal or a service-default lookup.
// Byte form: decodes then delegates; the request path uses extractModelDoc on
// the shared single-parse decode (docs/single-parse-dispatch-design.md).
func extractModel(body []byte) (string, error) {
	return extractModelDoc(jsonbody.Open(body))
}

// extractModelDoc is extractModel over the request's shared decode. Parity
// with the old struct decode is exact: absent or null -> ("", nil); a string
// -> trimmed; a non-string value or any decode problem -> "body is not a JSON
// object" (the old Unmarshal wrapped every failure into that one string).
func extractModelDoc(doc *jsonbody.BodyDoc) (string, error) {
	model, _, err := doc.Model()
	return model, err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr emits an OpenAI-shaped error. Anthropic clients tolerate it, and one
// shape keeps every error path from branching.
func writeErr(w http.ResponseWriter, status int, typ, msg string) {
	writeErrExtra(w, status, typ, msg, nil)
}

func writeErrExtra(w http.ResponseWriter, status int, typ, msg string, extra map[string]any) {
	e := map[string]any{"message": msg, "type": typ, "code": status}
	for k, v := range extra {
		if v != nil {
			e[k] = v
		}
	}
	writeJSON(w, status, map[string]any{"error": e})
}

func store2surface(s provider.Surface) store.Surface {
	switch s {
	case provider.SurfaceAnthropic:
		return store.SurfaceAnthropic
	case provider.SurfaceResponses:
		return store.SurfaceResponses
	case provider.SurfaceEmbeddings:
		return store.SurfaceEmbeddings
	case provider.SurfaceImagesGen:
		return store.SurfaceImagesGen
	case provider.SurfaceAudioTTS:
		return store.SurfaceAudioTTS
	case provider.SurfaceAudioSTT:
		return store.SurfaceAudioSTT
	case provider.SurfaceImagesEdit:
		return store.SurfaceImagesEdit
	case provider.SurfaceModeration:
		return store.SurfaceModeration
	case provider.SurfaceRerank:
		return store.SurfaceRerank
	case provider.SurfaceVideosGen:
		return store.SurfaceVideosGen
	case provider.SurfaceMusicGen:
		return store.SurfaceMusicGen
	default:
		return store.SurfaceOpenAI
	}
}
