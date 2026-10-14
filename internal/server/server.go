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
	"mime/multipart"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
	log         *slog.Logger
	mux         *http.ServeMux
	started     time.Time
	maxBody     int64
	web         fs.FS // dashboard source: embedded dist/ or EZLLM_WEB_DIR
	corsOrigins []string
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
}

// sweepRun is one in-flight protocol sweep for an account. cancel stops the
// probe loop (already-written verdicts stay — probes are independent).
type sweepRun struct {
	cancel   context.CancelFunc
	started  time.Time
	surfaces []string
	probing  int
}

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
		usageTZ: usageTZOrDefault(opts.UsageTZ),
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
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("POST /admin/login", s.handleDashboardLogin)
	s.mux.HandleFunc("GET /v1/models", s.authed(s.handleModels))
	s.mux.HandleFunc("POST /v1/chat/completions", s.authed(s.inference(provider.SurfaceOpenAI)))
	s.mux.HandleFunc("POST /v1/messages", s.authed(s.inference(provider.SurfaceAnthropic)))
	s.mux.HandleFunc("POST /v1/responses", s.authed(s.inference(provider.SurfaceResponses)))

	// Phase 1: New inference endpoints
	s.mux.HandleFunc("POST /v1/embeddings", s.authed(s.inference(provider.SurfaceEmbeddings)))
	s.mux.HandleFunc("POST /v1/images/generations", s.authed(s.inference(provider.SurfaceImagesGen)))
	s.mux.HandleFunc("POST /v1/audio/speech", s.authed(s.inference(provider.SurfaceAudioTTS)))
	s.mux.HandleFunc("POST /v1/audio/transcriptions", s.authed(s.inference(provider.SurfaceAudioSTT)))

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
	s.mux.HandleFunc("PUT /admin/accounts/{id}/model-pin", s.admin(s.handleSetModelPin))
	// Curated capability verdicts (operator-set, probe-protected). Same
	// body-carried model id as model-pin: ids can contain "/".
	s.mux.HandleFunc("PUT /admin/accounts/{id}/model-protocol", s.admin(s.handleSetModelProtocol))
	// v8: versioned model prices + derived cost (admin only — like overview,
	// this is where detail that /healthz no longer leaks lives).
	s.mux.HandleFunc("GET /admin/prices", s.admin(s.handleListPrices))
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
	inner := s.accessLog(s.recoverer(s.mux))
	// A preflight must be answered BEFORE the mux. Go's ServeMux only offers
	// HEAD/OPTIONS on a path when no method-specific pattern matches it; with
	// "POST /v1/chat/completions" registered it answers OPTIONS with 405 before
	// any handler — so CORS headers can never be attached there. Pre-dispatch
	// interception is the only correct place for it.
	return s.withCORS(s.mux, inner)
}

// withCORS routes: OPTIONS on an allowed origin is answered immediately (no
// body, never reaching inference or the ledger); everything else passes to next
// with CORS headers attached when the origin is allowed. Scope decides where it
// is mounted — /v1/* only — see cors.go.
func (s *Server) withCORS(next http.Handler, respond http.Handler) http.Handler {
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
		if respond != nil {
			respond.ServeHTTP(w, r)
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
// reports liveness only. It leaks nothing: no DB path, counts, versions, or
// uptime. Detailed stats are behind the admin token at /admin/overview.
// A failing DB ping returns 503 so the probe still means something.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if s.db != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.db.Ping(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "down"})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
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

// inference builds the handler for one wire protocol. All three share the same
// body: read -> resolve -> swap model -> forward -> ledger.
func (s *Server) inference(surface provider.Surface) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// Audio STT is multipart/form-data, not JSON: the model lives in a
		// form field, not a JSON key, so the generic JSON reader below would
		// reject it. Parse it here and re-serialize to a shape the rest of
		// the pipeline can consume.
		if surface == provider.SurfaceAudioSTT {
			s.inferenceSTT(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, s.maxBody))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_request_error", "could not read body: "+err.Error())
			return
		}
		if len(body) == 0 {
			writeErr(w, http.StatusBadRequest, "invalid_request_error", "empty body")
			return
		}

		requested, err := extractModel(body)
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
		if info := reqInfoOf(r); info != nil {
			info.surface = string(surface)
		}

		// Candidates, not a single route: a combo yields every hop that is
		// enabled AND eligible right now, in strategy order. The dispatcher
		// tries them before committing a byte to the client, so a 429 from
		// hop 1 is invisible when hop 2 answers.
		routes, err := s.resolver.ResolveCandidates(r.Context(), requested, surface, client)
		if err != nil {
			var nf *router.ErrNotFound
			var nu *router.ErrUnavailable
			var np *router.ErrPinned
			var nw *router.ErrWireUnsupported
			switch {
			case errors.As(err, &nf):
				writeErrExtra(w, http.StatusNotFound, "model_not_found", err.Error(),
					map[string]any{"available": nf.Hints})
			case errors.As(err, &np):
				// The operator pinned this model to a surface and the
				// client called a different one. That is a client
				// mistake, not an upstream failure: 400, and the body
				// says which endpoint to use. Never rerouted — ezllm
				// passes bodies through untranslated, so switching the
				// path upstream would send the wrong shape.
				writeErrExtra(w, http.StatusBadRequest, "pinned_surface", err.Error(),
					map[string]any{"pinned": np.Pinned, "requested_surface": np.Got})
			case errors.As(err, &nw):
				// Wire-first selection found no wire to dial: the model's own
				// surface is probed impossible and nothing entitles it to
				// another (no responses override, no translation). The
				// request problem is the answer — 400, not a gateway 502.
				writeErrExtra(w, http.StatusBadRequest, "wire_unsupported", err.Error(),
					map[string]any{"requested_surface": string(nw.Wire)})
			case errors.As(err, &nu):
				// A rule (cap / allowed hours) says no — not a routing miss.
				// The cause (when it is a *rules.Refusal) carries the
				// structured fields a client can act on: WHICH kind of rule
				// refused, and (for windows) when the model opens next, so it
				// can sleep until then instead of blind-retrying.
				extra := map[string]any{"reason": nu.Reason}
				var ref *rules.Refusal
				if errors.As(err, &ref) {
					extra["rule"] = map[string]any{
						"kind":   ref.Reason.Kind,
						"detail": ref.Reason.Detail,
					}
					if !ref.Reason.NextAllowed.IsZero() {
						extra["rule"].(map[string]any)["next_allowed"] =
							ref.Reason.NextAllowed.Format(time.RFC3339)
					}
				}
				writeErrExtra(w, http.StatusTooManyRequests, "model_unavailable", err.Error(), extra)
			default:
				writeErr(w, http.StatusBadGateway, "upstream_error", err.Error())
			}
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
		// same compressed body, because hops only swap `model`.
		body, compRes := s.applyCompression(r, requested, body)

		// v8 (M9): shape capture for cacheability research. Runs AFTER
		// compression so the hash describes what actually goes upstream —
		// if a profile rewrites history, that IS a cache break and must
		// measure as one. Best-effort: a failed parse records nothing and
		// changes nothing about the request.
		sh := captureShape(body, surface)

		// served is the route that ACTUALLY committed a response — it may be
		// hop 2 or 3 after failover. Every ledger field below (account, key,
		// model) must describe that hop: crediting routes[0] would bill a
		// failed hop for work it did not do, which is exactly the number a
		// per-account usage cap would later be measured against.
		served, res, ferr := s.dispatcher.ForwardCandidates(r.Context(), w, r, routes, body)
		if served.Alias == "" {
			// Resolution or swap failed before any hop committed: the ledger
			// still needs the REQUESTED target, or the row lands with no alias
			// and disappears from alias-grouped usage.
			served = routes[0]
		}
		// Record which hop actually served. sticky_last_good needs an OUTCOME and
		// cannot get one from resolution alone: it keeps using the hop that last
		// answered. Only a delivered 2xx counts — a failed attempt must not move
		// that cursor, or a hop that just broke would look like one that
		// worked. A bare model name is always a combo, so an alias without '/'
		// identifies one; a direct namespace/model route has no rotation to
		// inform.
		// A hop-attributable failure releases a hold_until_failure block so the
		// next dispatch rotates; a 4xx is the REQUEST's fault and keeps the
		// hold (operator decision 2026-10-13).
		if hopAttributableFailure(res, ferr) && !strings.Contains(served.Alias, "/") {
			s.resolver.MarkHopBad(served.Alias, served)
		}
		// A translated stream that ended in a late error frame or a truncated
		// source delivered a 200 header but did NOT serve the request: the
		// positive signal must not fire, or a hop that cuts its answers looks
		// healthy and sticky_last_good keeps choosing it. The ledger row keeps
		// the delivered status (that is what the client saw) and carries the
		// outcome in its err column.
		if ferr == nil && res.Status >= 200 && res.Status < 300 && res.StreamCompleted() && !strings.Contains(served.Alias, "/") {
			s.resolver.MarkHopGood(served.Alias, served)
		}
		if info := reqInfoOf(r); info != nil {
			info.account = served.Account.Namespace
			info.model = served.Model
		}
		status := res.Status
		if ferr != nil {
			var rl *proxy.RateLimited
			if errors.As(ferr, &rl) {
				// Every candidate hop was out of its configured request budget,
				// so NO upstream call was made. That is a refusal, not a gateway
				// fault: answer 429 and name the earliest moment the request
				// could succeed, so a client can wait instead of blind-retrying.
				status = http.StatusTooManyRequests
				extra := map[string]any{
					"reason": ferr.Error(),
					"rule": map[string]any{
						"kind":   "rate",
						"detail": fmt.Sprintf("%d requests per %s", rl.Limit, rl.Window),
					},
				}
				if !rl.NextAllowed.IsZero() {
					extra["rule"].(map[string]any)["next_allowed"] = rl.NextAllowed.Format(time.RFC3339)
				}
				writeErrExtra(w, status, "model_unavailable", ferr.Error(), extra)
			} else {
				// Nothing (or only headers) reached the client. Emit an explicit
				// 502 so a transport failure never looks like an empty 200 —
				// ResponseWriter defaults to 200 when WriteHeader is never called.
				// The gate is Committed, NOT Status: commit can fail on the
				// non-stream body read while still carrying the upstream's 200,
				// and guarding on Status would let that through as an empty 200.
				status = http.StatusBadGateway
				if !res.Committed {
					writeErr(w, status, "upstream_error", "upstream request failed: "+ferr.Error())
				}
			}
		} else if status == 0 {
			status = http.StatusBadGateway
		}
		call := store.Call{
			TS: start, Client: client, IP: cip, Surface: store2surface(surface), Alias: served.Alias,
			AccountID: served.Account.ID,
			Account:   served.Account.Namespace, KeyID: served.KeyID, KeyHint: served.KeyHint, Model: served.Model,
			Status: status, Stream: res.Stream, TTFTms: res.TTFTms, Totalms: res.Totalms,
			EndpointID: res.EndpointID, UpstreamModel: res.UpstreamModel,
			// v8 research capture: request shape + operator session key.
			PrefixSHA: sh.PrefixSHA, MsgCount: sh.MsgCount, ToolCount: sh.ToolCount,
			SessionID: sessionOf(r), ReqBytes: sh.ReqBytes,
		}
		attachCompressionMetrics(&call, compRes)
		if res.Usage != nil {
			call.TokensIn = res.Usage.In
			call.TokensOut = res.Usage.Out
			call.TokensCachedRead = res.Usage.CachedRead
			call.TokensCachedWrite = res.Usage.CachedWrite
			call.ReasoningTokens = res.Usage.Reasoning
			call.RawUsage = store.RawJSON(res.Usage.Raw)
		}
		if ferr != nil {
			call.Err = ferr.Error()
		} else if res.Err != "" {
			call.Err = res.Err
		}
		// Failover trail: every candidate that did NOT serve, appended to the
		// served row's err. One request stays ONE ledger row (the billing
		// truth), but the hops that "hopped" become visible in the dashboard —
		// the operator asked for exactly this, and without it a fallback run
		// looks like the rotation never moved.
		if note := failoverTrail(res.Attempts); note != "" {
			if call.Err == "" {
				call.Err = note
			} else {
				call.Err = call.Err + "; " + note
			}
		}
		call.BlockPos = served.BlockPos
		// Attribution for hops whose WIRE differs from the client's surface —
		// the request/response bodies were translated, so the row's tokens_in
		// is the vendor's INCLUSIVE total rather than our cold-token
		// convention. That covers both the anthropic->openai-chat leg
		// (Translated) and a hop curated onto the Responses wire
		// (ResponsesWire): one marker, one meaning, no second vocabulary.
		call.Translated = served.Translated || served.ResponsesWire
		s.ledger(call)
	}
}

// inferenceSTT handles /v1/audio/transcriptions: multipart/form-data, where the
// model lives in a form field rather than a JSON key. The raw multipart body is
// forwarded upstream with the model field swapped inside prepareHopBody.
func (s *Server) inferenceSTT(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	// Read the raw multipart body FIRST so it can be forwarded byte-for-byte
	// (ParseMultipartForm would consume the stream).
	body, err := io.ReadAll(io.LimitReader(r.Body, s.maxBody))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "could not read multipart body: "+err.Error())
		return
	}
	if len(body) == 0 {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "empty body")
		return
	}
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
		requested, ok = s.resolveServiceRequestModel(w, r, provider.SurfaceAudioSTT, start, client, cip)
		if !ok {
			return
		}
	}
	if info := reqInfoOf(r); info != nil {
		info.surface = string(provider.SurfaceAudioSTT)
	}

	routes, err := s.resolver.ResolveCandidates(r.Context(), requested, provider.SurfaceAudioSTT, client)
	if err != nil {
		var nf *router.ErrNotFound
		var nu *router.ErrUnavailable
		switch {
		case errors.As(err, &nf):
			writeErrExtra(w, http.StatusNotFound, "model_not_found", err.Error(),
				map[string]any{"available": nf.Hints})
		case errors.As(err, &nu):
			writeErrExtra(w, http.StatusTooManyRequests, "model_unavailable", err.Error(),
				map[string]any{"reason": nu.Reason})
		default:
			writeErr(w, http.StatusBadGateway, "upstream_error", err.Error())
		}
		s.ledger(store.Call{
			TS: start, Client: client, IP: cip, Surface: store.SurfaceAudioSTT,
			Alias: requested, Model: requested, Status: errorStatus(err), Err: err.Error(),
		})
		return
	}
	if info := reqInfoOf(r); info != nil {
		info.account = routes[0].Account.Namespace
		info.model = routes[0].Model
	}

	// Forward the raw multipart body; prepareHopBody detects multipart and
	// swaps the model field per hop.
	served, res, ferr := s.dispatcher.ForwardCandidates(r.Context(), w, r, routes, body)
	if served.Alias == "" {
		served = routes[0]
	}
	status := res.Status
	if ferr != nil {
		status = http.StatusBadGateway
		if !res.Committed {
			writeErr(w, status, "upstream_error", "upstream request failed: "+ferr.Error())
		}
	} else if status == 0 {
		status = http.StatusBadGateway
	}
	call := store.Call{
		TS: start, Client: client, IP: cip, Surface: store.SurfaceAudioSTT,
		Alias: served.Alias, AccountID: served.Account.ID, Account: served.Account.Namespace,
		KeyID: served.KeyID, KeyHint: served.KeyHint, Model: served.Model,
		Status: status, Stream: res.Stream, Totalms: res.Totalms,
	}
	if res.Usage != nil {
		call.TokensIn = res.Usage.In
		call.TokensOut = res.Usage.Out
	}
	if ferr != nil {
		call.Err = ferr.Error()
	} else if res.Err != "" {
		call.Err = res.Err
	}
	s.ledger(call)
}

// extractModelMultipart reads the "model" form field from a multipart body.
func extractModelMultipart(body []byte) (string, error) {
	// Parse the boundary from the first line: --<boundary>\r\n
	eol := bytes.IndexAny(body, "\r\n")
	if eol < 0 {
		return "", errors.New("multipart body has no boundary line")
	}
	boundary := strings.TrimPrefix(string(body[:eol]), "--")
	if boundary == "" {
		return "", errors.New("multipart body has empty boundary")
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read multipart part: %w", err)
		}
		if part.FormName() == "model" {
			value, err := io.ReadAll(part)
			if err != nil {
				return "", fmt.Errorf("read model field: %w", err)
			}
			return strings.TrimSpace(string(value)), nil
		}
	}
	return "", nil
}

func errorStatus(err error) int {
	var nf *router.ErrNotFound
	if errors.As(err, &nf) {
		return http.StatusNotFound
	}
	var nu *router.ErrUnavailable
	if errors.As(err, &nu) {
		return http.StatusTooManyRequests
	}
	var np *router.ErrPinned
	if errors.As(err, &np) {
		return http.StatusBadRequest
	}
	return http.StatusBadGateway
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

	var rows []store.UsageRow
	var usedTZ string
	var err error
	if groupBy == "day" {
		rows, err = s.db.UsageDayReportTZ(r.Context(), from, to, loc)
		usedTZ = loc.String()
	} else {
		rows, err = s.db.UsageReport(r.Context(), from, to, groupBy)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
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
		totals.Errors += x.Errors
	}
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
		name, roles, err := s.auth.ClientByToken(r.Context(), token)
		if err != nil || name == "" {
			writeErr(w, http.StatusUnauthorized, "invalid_request_error", "invalid bearer token")
			return
		}
		ctx := context.WithValue(r.Context(), rolesKey{}, roles)
		ctx = context.WithValue(ctx, clientKey{}, name)
		if info := reqInfoOf(r); info != nil {
			info.client = name
		}
		next(w, r.WithContext(ctx))
	}
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
			"method", r.Method, "path", r.URL.Path, "status", status,
			"ms", time.Since(start).Milliseconds(), "bytes", sw.bytes,
			"client", info.client, "account", info.account,
			"model", info.model, "surface", info.surface,
		)
	})
}

// ── helpers ─────────────────────────────────────────────────────────────────

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
func extractModel(body []byte) (string, error) {
	var m struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return "", errors.New("body is not a JSON object")
	}
	return strings.TrimSpace(m.Model), nil
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
	default:
		return store.SurfaceOpenAI
	}
}
