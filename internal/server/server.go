// Package server exposes ezllm's OpenAI-compatible HTTP surface.
//
// M1 scope: /healthz (unauthenticated, minimal), /v1/models and
// /v1/chat/completions (both token-gated; chat returns 501 until M2).
// The middleware stack (recover -> access log -> mux) and the statusWriter
// with Flush passthrough are final — M2's SSE path depends on them.
package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/c4lyp5o/ezllm/internal/config"
)

type ctxKey int

const reqInfoKey ctxKey = 1

// reqInfo is created per-request by the access log (outermost) and mutated
// downstream by auth, so the log line can report which client authenticated.
// Pointer semantics: same goroutine, set before the log line is written.
type reqInfo struct {
	client string
}

type Server struct {
	cfg     *config.Config
	log     *slog.Logger
	mux     *http.ServeMux
	started time.Time
}

func New(cfg *config.Config, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, log: log, mux: http.NewServeMux(), started: time.Now()}
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /v1/models", s.authed(s.handleModels))
	s.mux.HandleFunc("POST /v1/chat/completions", s.authed(s.handleChat))
	return s
}

// Handler wires the middleware stack: access log outermost, recover inside it
// so panics surface as logged 500s, mux innermost.
func (s *Server) Handler() http.Handler {
	return s.accessLog(s.recoverer(s.mux))
}

// --- handlers ---

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"version":   "m1",
		"uptime_s":  int(time.Since(s.started).Seconds()),
		"aliases":   len(s.cfg.Aliases),
		"providers": len(s.cfg.Providers),
		"clients":   len(s.cfg.ClientTokens),
	})
}

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	data := make([]map[string]any, 0, len(s.cfg.Aliases))
	for _, name := range s.cfg.AliasNames() {
		a := s.cfg.Aliases[name]
		data = append(data, map[string]any{
			"object":   "model",
			"id":       name,
			"owned_by": a.Provider,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]any{
		"error": map[string]any{
			"message": "ezllm M1: chat passthrough ships in M2 (see EXPLORING.md)",
			"type":    "not_implemented",
			"code":    http.StatusNotImplemented,
		},
	})
	// drain body so keep-alive clients aren't stalled
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
}

// --- middleware ---

func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := ""
		if h := r.Header.Get("Authorization"); len(h) > 7 && h[:7] == "Bearer " {
			name = s.cfg.Authenticate(trimBearer(h))
		}
		if name == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": map[string]any{"message": "missing or invalid bearer token", "type": "invalid_request_error", "code": 401},
			})
			return
		}
		if info, ok := r.Context().Value(reqInfoKey).(*reqInfo); ok && info != nil {
			info.client = name
		}
		next(w, r)
	}
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic", "err", rec, "path", r.URL.Path, "stack", string(debug.Stack()))
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error": map[string]any{"message": "internal error", "type": "server_error", "code": 500},
				})
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
		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"ms", time.Since(start).Milliseconds(),
			"client", info.client, // token NAME only — never the token
		)
	})
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func trimBearer(h string) string {
	if len(h) > 7 {
		return h[7:]
	}
	return ""
}
