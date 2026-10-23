package claudebridge

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxBodyBytes caps a request. The upstream body is replayed to native through a
// file, so a runaway client cannot exhaust the container first.
const maxBodyBytes = 32 << 20

// CatalogModel is one row of GET /v1/models. M1 serves the pinned routes the CLI
// resolves its family aliases to; ids are what clients ask for.
type CatalogModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	OwnedBy     string `json:"owned_by"`
}

// DefaultCatalog is the static M1 catalog. The CLI's live picker (an offline
// initialize handshake) can replace it later without touching the router.
func DefaultCatalog() []CatalogModel {
	return []CatalogModel{
		{ID: "claude-haiku-5-5", DisplayName: "Claude Haiku 5.5", OwnedBy: "anthropic"},
		{ID: "claude-haiku-5-5[1m]", DisplayName: "Claude Haiku 5.5 (1M)", OwnedBy: "anthropic"},
		{ID: "claude-sonnet-5-5", DisplayName: "Claude Sonnet 5.5", OwnedBy: "anthropic"},
		{ID: "claude-sonnet-5-5[1m]", DisplayName: "Claude Sonnet 5.5 (1M)", OwnedBy: "anthropic"},
		{ID: "claude-opus-5-5", DisplayName: "Claude Opus 5.5", OwnedBy: "anthropic"},
		{ID: "claude-opus-5-5[1m]", DisplayName: "Claude Opus 5.5 (1M)", OwnedBy: "anthropic"},
	}
}

// Executor runs one request against the CLI. *Runner implements it; tests inject
// a fake so the HTTP layer can be exercised without spawning anything.
type Executor interface {
	Run(ctx context.Context, req Request, alias string, sink func(Frame) error) (Result, error)
}

// Server exposes the Anthropic Messages shape over the CLI. Only ezllm dials it.
type Server struct {
	Runner  Executor
	Token   string
	Version string
	Models  []CatalogModel
	Info    map[string]any
	Logf    func(format string, args ...any)
	// Limits holds the last plan-window reading; nil disables GET /v1/usage.
	Limits *Limits
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// Handler builds the mux. Endpoints: /healthz (two fields, house contract),
// /status (operator detail), /v1/models, /v1/usage (plan windows), /v1/messages.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONBytes(w, http.StatusOK, mustJSON(map[string]any{"status": "ok", "version": s.Version}))
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONBytes(w, http.StatusOK, mustJSON(s.Info))
	})
	mux.HandleFunc("GET /v1/models", s.authed(s.listModels))
	mux.HandleFunc("GET /v1/usage", s.authed(s.usage))
	mux.HandleFunc("POST /v1/messages", s.authed(s.messages))
	return mux
}

func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("x-api-key")
		if got == "" {
			got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if s.Token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
			writeJSONBytes(w, http.StatusUnauthorized,
				ErrorEnvelope("authentication_error", "claude-bridge: missing or wrong x-api-key", "unauthorized"))
			return
		}
		next(w, r)
	}
}

func (s *Server) listModels(w http.ResponseWriter, _ *http.Request) {
	models := s.Models
	if models == nil {
		models = DefaultCatalog()
	}
	writeJSONBytes(w, http.StatusOK, mustJSON(map[string]any{"data": models, "has_more": false}))
}

// usage serves the plan's 5h and weekly windows for the provider card. 404
// until a call has reported them: the account then reads as "measured", and
// the first request fills it in.
func (s *Server) usage(w http.ResponseWriter, _ *http.Request) {
	body, ok := s.Limits.UsageBody()
	if !ok {
		writeJSONBytes(w, http.StatusNotFound, ErrorEnvelope("not_found_error",
			"claude-bridge: no plan-limit reading yet; the CLI reports one with every call", "no_reading"))
		return
	}
	writeJSONBytes(w, http.StatusOK, body)
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeJSONBytes(w, http.StatusBadRequest, ErrorEnvelope("invalid_request_error", "claude-bridge: cannot read request body: "+err.Error(), "invalid_request"))
		return
	}
	if len(body) > maxBodyBytes {
		writeJSONBytes(w, http.StatusRequestEntityTooLarge, ErrorEnvelope("invalid_request_error", "claude-bridge: request body too large", "invalid_request"))
		return
	}
	req, err := ParseRequest(body)
	if err != nil {
		code := "invalid_request"
		status := http.StatusBadRequest
		if u, ok := err.(Unsupported); ok {
			code = "chat_tools_unsupported"
			_ = u
		}
		writeJSONBytes(w, status, ErrorEnvelope("invalid_request_error", err.Error(), code))
		return
	}
	alias, err := ModelAlias(req.Model)
	if err != nil {
		writeJSONBytes(w, http.StatusBadRequest, ErrorEnvelope("invalid_request_error", err.Error(), "unknown_model"))
		return
	}

	if req.Stream {
		s.stream(w, r, req, alias)
		return
	}
	sw := &StreamWriter{}
	res, err := s.Runner.Run(r.Context(), req, alias, sw.Handle)
	s.Limits.Observe(res.RateLimit, time.Now())
	if err != nil {
		s.writeRunError(w, res, err)
		return
	}
	writeJSONBytes(w, http.StatusOK, mustJSON(sw.Message(req.Model)))
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request, req Request, alias string) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if flusher != nil {
		flusher.Flush()
	}
	sw := &StreamWriter{W: w, Flush: func() {
		if flusher != nil {
			flusher.Flush()
		}
	}}
	res, err := s.Runner.Run(r.Context(), req, alias, sw.Handle)
	s.Limits.Observe(res.RateLimit, time.Now())
	if err != nil {
		// The stream is already open: the failure travels as an Anthropic error
		// event, with the CLI's own words, never as a silent truncation.
		status, errType := 502, "api_error"
		code := "native_error"
		if ne, ok := err.(*NativeError); ok {
			status, errType, code = ne.Status, ne.Type, ne.Code
			_ = status
		}
		payload := ErrorEnvelope(errType, err.Error(), code)
		_, _ = w.Write([]byte("event: error\ndata: " + string(payload) + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		s.logf("claude-bridge: stream failed: %s", err)
		return
	}
	if err := sw.Finish(); err != nil {
		s.logf("claude-bridge: stream finish failed: %s", err)
	}
}

func (s *Server) writeRunError(w http.ResponseWriter, _ Result, err error) {
	ne, ok := err.(*NativeError)
	if !ok {
		writeJSONBytes(w, http.StatusBadGateway, ErrorEnvelope("api_error", err.Error(), "native_error"))
		return
	}
	writeJSONBytes(w, ne.Status, ErrorEnvelope(ne.Type, ne.Message, ne.Code))
}

func writeJSONBytes(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"type":"error","error":{"type":"api_error","message":"claude-bridge: encode failure","code":"encode"}}`)
	}
	return b
}
