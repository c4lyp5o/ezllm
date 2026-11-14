package agybridge

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/bridgeutil"
)

const maxBodyBytes = 32 << 20

// Server exposes OpenAI Chat Completions over the CLI. Only ezllm dials it.
type Server struct {
	ExperimentalTools bool
	Runner            Executor
	Token             string
	Version           string
	Models            []Model // the login's catalog from boot (`agy models`)
	Info              map[string]any
	Logf              func(format string, args ...any)
	Now               func() time.Time
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

// Handler builds the mux: /healthz (two fields, house contract), /status
// (operator detail), /v1/models, /v1/chat/completions.
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
		if cache, ok := s.Runner.(interface{ CacheStats() map[string]any }); ok {
			info["runtime"] = map[string]any{"conversation_cache": cache.CacheStats()}
		}
		writeJSON(w, http.StatusOK, info)
	})
	mux.HandleFunc("GET /v1/models", s.authed(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": s.Models})
	}))
	mux.HandleFunc("POST /v1/chat/completions", s.authed(s.chat))
	return mux
}

func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.Token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
			writeRaw(w, http.StatusUnauthorized, ErrorEnvelope("invalid_request_error", "agy-bridge: missing or wrong bearer token", "unauthorized"))
			return
		}
		next(w, r)
	}
}

func (s *Server) knownModel(id string) bool {
	for _, m := range s.Models {
		if m.ID == id {
			return true
		}
	}
	return false
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeRaw(w, http.StatusBadRequest, ErrorEnvelope("invalid_request_error", "agy-bridge: cannot read request body: "+err.Error(), "invalid_request"))
		return
	}
	if len(body) > maxBodyBytes {
		writeRaw(w, http.StatusRequestEntityTooLarge, ErrorEnvelope("invalid_request_error", "agy-bridge: request body too large", "invalid_request"))
		return
	}
	req, err := parseRequest(body, s.ExperimentalTools)
	if err != nil {
		code := "invalid_request"
		var u Unsupported
		if errors.As(err, &u) {
			code = u.Code
		}
		writeRaw(w, http.StatusBadRequest, ErrorEnvelope("invalid_request_error", err.Error(), code))
		return
	}
	// Refused before a spawn: an unknown model costs a ~3s CLI start to learn.
	if !s.knownModel(req.Model) {
		ids := make([]string, 0, len(s.Models))
		for _, m := range s.Models {
			ids = append(ids, m.ID)
		}
		writeRaw(w, http.StatusBadRequest, ErrorEnvelope("invalid_request_error",
			"agy-bridge: model "+req.Model+" is not on this login's plan (have: "+strings.Join(ids, ", ")+")", "unknown_model"))
		return
	}

	if req.Adapter != nil {
		s.adaptedChat(w, r, req)
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
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "object": "chat.completion", "created": created, "model": req.Model,
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": res.Text}}},
			"usage": openAIUsage(res.Usage),
		})
		return
	}

	transport := bridgeutil.NewStream(r.Context(), w, bridgeutil.StreamWriteStall)
	defer transport.Close()
	sw := &sseWriter{w: w, id: id, created: created, model: req.Model, transport: transport}
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
		s.logf("agy-bridge: stream failed: %s", err)
		return
	}
	sw.begin()
	sw.chunk(map[string]any{}, "stop")
	if req.IncludeUsage {
		sw.event(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created,
			"model": req.Model, "choices": []any{}, "usage": openAIUsage(res.Usage)})
	}
	sw.done()
}

// openAIUsage maps the result envelope to OpenAI usage. Thinking tokens are
// part of output; native cold input excludes cache reads, while OpenAI includes them.
func openAIUsage(u Usage) map[string]any {
	return map[string]any{
		"prompt_tokens":             u.InputTokens + u.CacheRead,
		"completion_tokens":         u.OutputTokens,
		"total_tokens":              u.InputTokens + u.CacheRead + u.OutputTokens,
		"prompt_tokens_details":     map[string]any{"cached_tokens": u.CacheRead},
		"completion_tokens_details": map[string]any{"reasoning_tokens": u.ThinkingTokens},
	}
}

// sseWriter owns OpenAI event encoding and header timing. Transport is shared.
type sseWriter struct {
	w         http.ResponseWriter
	id        string
	created   int64
	model     string
	started   bool
	transport *bridgeutil.Stream
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
	_, err = s.transport.Write([]byte("data: " + string(b) + "\n\n"))
	return err
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
	_, _ = s.transport.Write([]byte("data: [DONE]\n\n"))
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
		writeRaw(w, http.StatusInternalServerError, ErrorEnvelope("api_error", "agy-bridge: encode failure", "encode"))
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

// adaptedChat buffers only this turn's envelope. Tool execution belongs to the
// caller; the agy child has exited before tool calls ship.
// An opt-in runner may remember validated conversation metadata for exact replay.
func (s *Server) adaptedChat(w http.ResponseWriter, r *http.Request, req Request) {
	res, err := s.Runner.Run(r.Context(), req, nil)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	text, calls, err := req.Adapter.decode(res)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	id, created := "chatcmpl-"+randomHex(12), s.now().Unix()
	finish := "stop"
	message := map[string]any{"role": "assistant", "content": text}
	if len(calls) > 0 {
		finish = "tool_calls"
		message["content"] = nil
		message["tool_calls"] = calls
	}
	if !req.Stream {
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "object": "chat.completion", "created": created, "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": message}}, "usage": openAIUsage(res.Usage)})
		return
	}
	transport := bridgeutil.NewStream(r.Context(), w, bridgeutil.StreamWriteStall)
	defer transport.Close()
	sw := &sseWriter{w: w, id: id, created: created, model: req.Model, transport: transport}
	sw.begin()
	delta := map[string]any{"content": text}
	if len(calls) > 0 {
		indexed := make([]map[string]any, 0, len(calls))
		for i, c := range calls {
			indexed = append(indexed, map[string]any{"index": i, "id": c.ID, "type": c.Type, "function": c.Function})
		}
		delta = map[string]any{"tool_calls": indexed}
	}
	if sw.chunk(delta, "") != nil {
		return
	}
	if sw.chunk(map[string]any{}, finish) != nil {
		return
	}
	if req.IncludeUsage {
		if sw.event(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model, "choices": []any{}, "usage": openAIUsage(res.Usage)}) != nil {
			return
		}
	}
	sw.done()
}
