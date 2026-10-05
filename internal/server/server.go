// Package server exposes ezllm's HTTP surface: three inference protocols
// (OpenAI chat, Anthropic messages, OpenAI Responses) plus /healthz and the
// admin API.
//
// Middleware stack: accessLog -> recoverer -> mux. The statusWriter forwards
// Flush so streaming responses stay unbuffered end to end.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/router"
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
	log        *slog.Logger
	mux        *http.ServeMux
	started    time.Time
	maxBody    int64
}

// Options configures New.
type Options struct {
	Auth       Auth
	Resolver   *router.Resolver
	Dispatcher *proxy.Dispatcher
	DB         *store.DB
	Log        *slog.Logger
	MaxBodyMiB int // request body cap; default 32
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
		db: opts.DB, log: opts.Log, mux: http.NewServeMux(),
		started: time.Now(), maxBody: maxBody,
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /v1/models", s.authed(s.handleModels))
	s.mux.HandleFunc("POST /v1/chat/completions", s.authed(s.inference(provider.SurfaceOpenAI)))
	s.mux.HandleFunc("POST /v1/messages", s.authed(s.inference(provider.SurfaceAnthropic)))
	s.mux.HandleFunc("POST /v1/responses", s.authed(s.inference(provider.SurfaceResponses)))
	s.mux.HandleFunc("GET /admin/health", s.authed(s.handleAdminHealth))
	s.mux.HandleFunc("GET /admin/usage", s.authed(s.handleUsage))
}

// Handler wires the middleware stack: access log outermost, recover inside it
// so panics surface as logged 500s, mux innermost.
func (s *Server) Handler() http.Handler {
	return s.accessLog(s.recoverer(s.mux))
}

// WrapForTest applies the same middleware stack to an arbitrary handler.
// Tests use it to exercise the recoverer with a deliberately panicking route.
func (s *Server) WrapForTest(h http.Handler) http.Handler {
	return s.accessLog(s.recoverer(h))
}

// ── handlers ────────────────────────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	payload := map[string]any{
		"status":   "ok",
		"version":  "m2",
		"uptime_s": int(time.Since(s.started).Seconds()),
	}
	if s.db != nil {
		payload["schema_version"] = s.db.SchemaVersion()
		payload["db"] = s.db.Path()
		if n, err := s.countAccounts(); err == nil {
			payload["accounts"] = n
		}
		if n, err := s.countLedger(); err == nil {
			payload["ledger_rows"] = n
		}
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) countAccounts() (int, error) {
	var n int
	err := s.db.Reader().QueryRow(`SELECT COUNT(*) FROM accounts WHERE enabled=1`).Scan(&n)
	return n, err
}

func (s *Server) countLedger() (int, error) {
	var n int
	err := s.db.Reader().QueryRow(`SELECT COUNT(*) FROM calls`).Scan(&n)
	return n, err
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
		if info := reqInfoOf(r); info != nil {
			info.surface = string(surface)
		}

		rt, err := s.resolver.Resolve(r.Context(), requested, surface, client)
		if err != nil {
			var nf *router.ErrNotFound
			if errors.As(err, &nf) {
				writeErrExtra(w, http.StatusNotFound, "model_not_found", err.Error(),
					map[string]any{"available": nf.Hints})
			} else {
				writeErr(w, http.StatusBadGateway, "upstream_error", err.Error())
			}
			s.ledger(store.Call{
				TS: start, Client: client, Surface: store2surface(surface), Alias: requested,
				Model: requested, Status: errorStatus(err), Err: err.Error(),
			})
			return
		}
		if info := reqInfoOf(r); info != nil {
			info.account = rt.Account.Namespace
			info.model = rt.Model
		}

		swapped, _, err := proxy.SwapModel(body, rt.Model)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}

		res, ferr := s.dispatcher.Forward(r.Context(), w, r, rt, swapped)
		status := res.Status
		if ferr != nil {
			// Nothing (or only headers) reached the client. Emit an explicit
			// 502 so a transport failure never looks like an empty 200 —
			// ResponseWriter defaults to 200 when WriteHeader is never called.
			status = http.StatusBadGateway
			if res.Status == 0 {
				writeErr(w, status, "upstream_error", "upstream request failed: "+ferr.Error())
			}
		} else if status == 0 {
			status = http.StatusBadGateway
		}
		call := store.Call{
			TS: start, Client: client, Surface: store2surface(surface), Alias: rt.Alias,
			Account: rt.Account.Namespace, KeyID: rt.KeyID, KeyHint: rt.KeyHint, Model: rt.Model,
			Status: status, Stream: res.Stream, TTFTms: res.TTFTms, Totalms: res.Totalms,
			EndpointID: res.EndpointID, UpstreamModel: res.UpstreamModel,
		}
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
		s.ledger(call)
	}
}

func errorStatus(err error) int {
	var nf *router.ErrNotFound
	if errors.As(err, &nf) {
		return http.StatusNotFound
	}
	return http.StatusBadGateway
}

func (s *Server) handleAdminHealth(w http.ResponseWriter, r *http.Request) {
	if !hasRole(r, "admin") {
		writeErr(w, http.StatusForbidden, "forbidden", "admin role required")
		return
	}
	var accounts, keys, combos, models, calls int
	var oldest, newest string
	q := func(query string, dest *int) {
		_ = s.db.Reader().QueryRowContext(r.Context(), query).Scan(dest)
	}
	q(`SELECT COUNT(*) FROM accounts`, &accounts)
	q(`SELECT COUNT(*) FROM provider_keys`, &keys)
	q(`SELECT COUNT(*) FROM combos`, &combos)
	q(`SELECT COUNT(*) FROM models`, &models)
	q(`SELECT COUNT(*) FROM calls`, &calls)
	_ = s.db.Reader().QueryRowContext(r.Context(),
		`SELECT COALESCE(MIN(ts),''),COALESCE(MAX(ts),'') FROM calls`).Scan(&oldest, &newest)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"schema_version": s.db.SchemaVersion(),
		"db_path":        s.db.Path(),
		"accounts":       accounts,
		"provider_keys":  keys,
		"models":         models,
		"combos":         combos,
		"ledger_rows":    calls,
		"ledger_span":    map[string]string{"oldest": oldest, "newest": newest},
		"dropped_rows":   store.DroppedCalls(),
		"uptime_s":       int(time.Since(s.started).Seconds()),
	})
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if !hasRole(r, "admin") {
		writeErr(w, http.StatusForbidden, "forbidden", "admin role required")
		return
	}
	groupBy := r.URL.Query().Get("group_by")
	to := time.Now().UTC()
	from := to.Add(-24 * time.Hour)
	if v := r.URL.Query().Get("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			from = t
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			to = t
		}
	} else {
		// No explicit `to`: use an INCLUSIVE upper bound. Rows are stamped at
		// request start and the ledger is async, so a row written a moment ago
		// can carry a ts marginally after time.Now() as observed by this
		// handler; a strict `ts < now` filter would silently drop it and
		// under-report usage. Add a small grace window instead.
		to = to.Add(time.Minute)
	}
	rows, err := s.db.UsageReport(r.Context(), from, to, groupBy)
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
	writeJSON(w, http.StatusOK, map[string]any{
		"from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339),
		"group_by": orDefault(groupBy, "account"),
		"groups":   rows, "totals": totals,
	})
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
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

// extractModel reads the requested model without disturbing any other field.
func extractModel(body []byte) (string, error) {
	var m struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return "", errors.New("body is not a JSON object")
	}
	if strings.TrimSpace(m.Model) == "" {
		return "", errors.New("'model' is required")
	}
	return m.Model, nil
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
	default:
		return store.SurfaceOpenAI
	}
}
