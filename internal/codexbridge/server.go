package codexbridge

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// maxBodyBytes caps a request (images arrive inline as data: URLs).
const maxBodyBytes = 32 << 20

// catalogTTL bounds how stale /v1/models and request validation may be.
const catalogTTL = 10 * time.Minute

// Executor is what the HTTP layer needs from the CLI. *Runner implements it;
// tests inject a fake so the surface runs without spawning anything.
type Executor interface {
	Run(ctx context.Context, req Request, sink func(delta string) error) (Result, error)
	Models(ctx context.Context) ([]Model, error)
	RateLimits(ctx context.Context) (RateLimits, error)
}

// Server exposes OpenAI Chat Completions over the Codex CLI. Only ezllm dials it.
type Server struct {
	Runner  Executor
	Token   string
	Version string
	Info    map[string]any
	Logf    func(format string, args ...any)
	Now     func() time.Time
	// ToolsOff, when set, is why client tools are refused on this host
	// (decision §4.7: no Landlock and no operator override).
	ToolsOff string

	mu        sync.Mutex
	catalog   []Model
	catalogAt time.Time
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Handler builds the mux. Endpoints: /healthz (two fields, house contract),
// /status (operator detail), /v1/models, /v1/usage (plan windows for the
// provider card), /v1/chat/completions.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": s.Version})
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		info := make(map[string]any, len(s.Info)+1)
		for k, v := range s.Info {
			info[k] = v
		}
		if p, ok := s.Runner.(interface{ ParkedTurns() int }); ok {
			info["parked_tool_turns"] = p.ParkedTurns()
		}
		writeJSON(w, http.StatusOK, info)
	})
	mux.HandleFunc("GET /v1/models", s.authed(s.listModels))
	mux.HandleFunc("GET /v1/usage", s.authed(s.usage))
	mux.HandleFunc("POST /v1/chat/completions", s.authed(s.chat))
	return mux
}

func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.Token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
			writeRaw(w, http.StatusUnauthorized,
				ErrorEnvelope("authentication_error", "codex-bridge: missing or wrong bearer token", "unauthorized"))
			return
		}
		next(w, r)
	}
}

// models returns the cached catalog, refreshing it when stale. A failed
// refresh keeps serving the previous catalog.
func (s *Server) models(ctx context.Context) ([]Model, error) {
	s.mu.Lock()
	cached, at := s.catalog, s.catalogAt
	s.mu.Unlock()
	if cached != nil && s.now().Sub(at) < catalogTTL {
		return cached, nil
	}
	fresh, err := s.Runner.Models(ctx)
	if err != nil {
		if cached != nil {
			return cached, nil
		}
		return nil, err
	}
	s.mu.Lock()
	s.catalog, s.catalogAt = fresh, s.now()
	s.mu.Unlock()
	return fresh, nil
}

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.models(r.Context())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]any{
			"id": m.ID, "object": "model", "created": 0, "owned_by": "openai",
			"display_name": m.DisplayName, "hidden": m.Hidden,
			"reasoning_efforts": m.Efforts, "default_reasoning_effort": m.DefaultEffort,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	rl, err := s.Runner.RateLimits(r.Context())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	body, ok := UsageBody(rl, s.now())
	if !ok {
		writeRaw(w, http.StatusNotFound, ErrorEnvelope("not_found_error",
			"codex-bridge: the login reported no plan window", "no_reading"))
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// validate checks model and effort against the catalog before anything is
// spawned. Without a catalog the CLI itself decides.
func (s *Server) validate(ctx context.Context, req Request) error {
	models, err := s.models(ctx)
	if err != nil || len(models) == 0 {
		return nil
	}
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
		if m.ID != req.Model {
			continue
		}
		if req.Effort != "" && len(m.Efforts) > 0 && !slices.Contains(m.Efforts, req.Effort) {
			return &NativeError{Status: 400, Type: "invalid_request_error", Code: "invalid_request",
				Message: "codex-bridge: " + req.Model + " does not offer reasoning_effort " + req.Effort +
					" (offers " + strings.Join(m.Efforts, ", ") + ")"}
		}
		return nil
	}
	return &NativeError{Status: 400, Type: "invalid_request_error", Code: "model_not_found",
		Message: "codex-bridge: unknown model " + req.Model + " (this login serves " + strings.Join(ids, ", ") + ")"}
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeRaw(w, http.StatusBadRequest, ErrorEnvelope("invalid_request_error", "codex-bridge: cannot read request body: "+err.Error(), "invalid_request"))
		return
	}
	if len(body) > maxBodyBytes {
		writeRaw(w, http.StatusRequestEntityTooLarge, ErrorEnvelope("invalid_request_error", "codex-bridge: request body too large", "invalid_request"))
		return
	}
	req, err := ParseRequest(body)
	if err != nil {
		var u Unsupported
		code := "invalid_request"
		if errors.As(err, &u) {
			code = u.Code
		}
		writeRaw(w, http.StatusBadRequest, ErrorEnvelope("invalid_request_error", err.Error(), code))
		return
	}
	if len(req.Tools) > 0 && s.ToolsOff != "" {
		writeRaw(w, http.StatusBadRequest, ErrorEnvelope("invalid_request_error",
			"client tools are disabled on this host: "+s.ToolsOff+". See docs/codex-bridge-runbook.md#platform-requirements--read-this-first",
			"chat_tools_unsupported"))
		return
	}
	if err := s.validate(r.Context(), req); err != nil {
		s.writeErr(w, err)
		return
	}

	id := "chatcmpl-" + randomHex(12)
	created := s.now().Unix()
	if !req.Stream {
		res, err := s.Runner.Run(r.Context(), req, nil)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		msg := map[string]any{"role": "assistant", "content": res.Text}
		finish := "stop"
		if len(res.ToolCalls) > 0 {
			msg["tool_calls"] = openAIToolCalls(res.ToolCalls, false)
			if res.Text == "" {
				msg["content"] = nil
			}
			finish = "tool_calls"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "object": "chat.completion", "created": created, "model": req.Model,
			"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finish}},
			"usage":   openAIUsage(res.Usage),
		})
		return
	}

	sw := &sseWriter{w: w, id: id, created: created, model: req.Model}
	res, err := s.Runner.Run(r.Context(), req, sw.delta)
	if err != nil {
		if !sw.started {
			// Nothing reached the client yet: the failure keeps its real status.
			s.writeErr(w, err)
			return
		}
		ne := asNative(err)
		sw.event(json.RawMessage(ErrorEnvelope(ne.Type, ne.Message, ne.Code)))
		sw.done()
		s.logf("codex-bridge: stream failed: %s", err)
		return
	}
	sw.begin()
	if len(res.ToolCalls) > 0 {
		sw.chunk(map[string]any{"tool_calls": openAIToolCalls(res.ToolCalls, true)}, "")
		sw.chunk(map[string]any{}, "tool_calls")
	} else {
		sw.chunk(map[string]any{}, "stop")
	}
	if req.IncludeUsage {
		sw.event(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created,
			"model": req.Model, "choices": []any{}, "usage": openAIUsage(res.Usage)})
	}
	sw.done()
}

// openAIToolCalls renders calls as OpenAI tool_calls (indexed in a stream).
func openAIToolCalls(calls []ToolCall, indexed bool) []map[string]any {
	out := make([]map[string]any, 0, len(calls))
	for i, c := range calls {
		tc := map[string]any{"id": c.ID, "type": "function",
			"function": map[string]any{"name": c.Name, "arguments": c.Arguments}}
		if indexed {
			tc["index"] = i
		}
		out = append(out, tc)
	}
	return out
}

// openAIUsage maps the turn envelope to OpenAI usage. Reasoning tokens are
// part of output (measured: 32 output with 25 reasoning).
func openAIUsage(u Usage) map[string]any {
	return map[string]any{
		"prompt_tokens":             u.InputTokens,
		"completion_tokens":         u.OutputTokens,
		"total_tokens":              u.InputTokens + u.OutputTokens,
		"prompt_tokens_details":     map[string]any{"cached_tokens": u.CachedTokens},
		"completion_tokens_details": map[string]any{"reasoning_tokens": u.ReasoningTokens},
	}
}

// sseWriter emits chat.completion.chunk events. Headers are written on the
// first delta, so a failure before any output still gets its real status.
type sseWriter struct {
	w       http.ResponseWriter
	id      string
	created int64
	model   string
	started bool
}

func (s *sseWriter) begin() {
	if s.started {
		return
	}
	s.started = true
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
	s.chunk(map[string]any{"role": "assistant", "content": ""}, "")
}

func (s *sseWriter) event(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := s.w.Write([]byte("data: " + string(b) + "\n\n")); err != nil {
		return err
	}
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func (s *sseWriter) chunk(delta map[string]any, finish string) error {
	choice := map[string]any{"index": 0, "delta": delta, "finish_reason": nil}
	if finish != "" {
		choice["finish_reason"] = finish
	}
	return s.event(map[string]any{"id": s.id, "object": "chat.completion.chunk",
		"created": s.created, "model": s.model, "choices": []any{choice}})
}

func (s *sseWriter) delta(text string) error {
	s.begin()
	return s.chunk(map[string]any{"content": text}, "")
}

func (s *sseWriter) done() {
	_, _ = s.w.Write([]byte("data: [DONE]\n\n"))
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
}

func asNative(err error) *NativeError {
	var ne *NativeError
	if errors.As(err, &ne) {
		return ne
	}
	return &NativeError{Status: 502, Type: "api_error", Code: "native_error", Message: err.Error()}
}

func (s *Server) writeErr(w http.ResponseWriter, err error) {
	ne := asNative(err)
	writeRaw(w, ne.Status, ErrorEnvelope(ne.Type, ne.Message, ne.Code))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writeRaw(w, http.StatusInternalServerError, ErrorEnvelope("api_error", "codex-bridge: encode failure", "encode"))
		return
	}
	writeRaw(w, status, b)
}

func writeRaw(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
