package agybridge

import (
	"crypto/rand"
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
		runtime := map[string]any{}
		if a, ok := s.Runner.(interface{ AdmissionStats() map[string]any }); ok {
			runtime = a.AdmissionStats()
		}
		if cache, ok := s.Runner.(interface{ CacheStats() map[string]any }); ok {
			runtime["conversation_cache"] = cache.CacheStats()
		}
		if len(runtime) > 0 {
			info["runtime"] = runtime
		}
		writeJSON(w, http.StatusOK, info)
	})
	mux.HandleFunc("GET /v1/models", s.authed(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": s.Models})
	}))
	mux.HandleFunc("POST /v1/chat/completions", s.authed(s.chat))
	return mux
}

// authed admits only the bridge token (bridgeutil.Authorized, constant-time).
// a refusal is a 401 in this bridge's own error shape.
func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !bridgeutil.Authorized(r, s.Token, false) {
			writeRaw(w, http.StatusUnauthorized, bridgeutil.OpenAIErrorEnvelope("invalid_request_error", "agy-bridge: missing or wrong bearer token", "unauthorized"))
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
		writeRaw(w, http.StatusBadRequest, bridgeutil.OpenAIErrorEnvelope("invalid_request_error", "agy-bridge: cannot read request body: "+err.Error(), "invalid_request"))
		return
	}
	if len(body) > maxBodyBytes {
		writeRaw(w, http.StatusRequestEntityTooLarge, bridgeutil.OpenAIErrorEnvelope("invalid_request_error", "agy-bridge: request body too large", "invalid_request"))
		return
	}
	req, err := parseRequest(body, s.ExperimentalTools)
	if err != nil {
		code := "invalid_request"
		var u Unsupported
		if errors.As(err, &u) {
			code = u.Code
		}
		writeRaw(w, http.StatusBadRequest, bridgeutil.OpenAIErrorEnvelope("invalid_request_error", err.Error(), code))
		return
	}
	// Refused before a spawn: an unknown model costs a ~3s CLI start to learn.
	if !s.knownModel(req.Model) {
		ids := make([]string, 0, len(s.Models))
		for _, m := range s.Models {
			ids = append(ids, m.ID)
		}
		writeRaw(w, http.StatusBadRequest, bridgeutil.OpenAIErrorEnvelope("invalid_request_error",
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
			s.writeRunErr(w, err, res.Usage)
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
	sw := newSSEWriter(w, transport, id, created, req.Model)
	res, err := s.Runner.Run(r.Context(), req, sw.delta)
	if err != nil {
		if !sw.Started() {
			// Nothing reached the client yet: the failure keeps its real status.
			s.writeRunErr(w, err, res.Usage)
			return
		}
		ne := asNative(err)
		sw.Event(json.RawMessage(bridgeutil.OpenAIErrorEnvelope(ne.Type, ne.Message, ne.Code)))
		sw.Done()
		s.logf("agy-bridge: stream failed: %s", err)
		return
	}
	sw.Begin()
	sw.Chunk(map[string]any{}, "stop")
	if req.IncludeUsage {
		sw.Usage(openAIUsage(res.Usage))
	}
	sw.Done()
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
// sseWriter streams chat chunks through the shared encoder. Headers are
// written on the first delta, so a failure before any output keeps its status.
type sseWriter struct{ *bridgeutil.ChatChunks }

func newSSEWriter(w http.ResponseWriter, transport *bridgeutil.Stream, id string, created int64, model string) sseWriter {
	c := bridgeutil.NewChatChunks(w, transport, id, created, model)
	c.RoleChunk = true
	return sseWriter{c}
}

func (s sseWriter) delta(text string) error {
	s.Begin()
	return s.Chunk(map[string]any{"content": text}, "")
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
	if ne.Code == "queue_full" {
		w.Header().Set("Retry-After", bridgeutil.QueueFullRetryAfter)
	}
	writeRaw(w, ne.Status, bridgeutil.OpenAIErrorEnvelope(ne.Type, ne.Message, ne.Code))
}

// writeRunErr is writeErr for a failure after native may have generated. A
// billed generation keeps its usage next to the error so the gateway ledger
// records the spend (proxy discardAttempt reads usage on error bodies).
func (s *Server) writeRunErr(w http.ResponseWriter, err error, usage Usage) {
	if usage == (Usage{}) {
		s.writeErr(w, err)
		return
	}
	ne := asNative(err)
	body, merr := json.Marshal(map[string]any{
		"error": map[string]any{"type": ne.Type, "message": ne.Message, "code": ne.Code},
		"usage": openAIUsage(usage),
	})
	if merr != nil {
		s.writeErr(w, err)
		return
	}
	writeRaw(w, ne.Status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writeRaw(w, http.StatusInternalServerError, bridgeutil.OpenAIErrorEnvelope("api_error", "agy-bridge: encode failure", "encode"))
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
		s.writeRunErr(w, err, res.Usage)
		return
	}
	text, calls, err := req.Adapter.decode(res)
	if err != nil {
		s.writeRunErr(w, err, res.Usage)
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
	sw := newSSEWriter(w, transport, id, created, req.Model)
	sw.Begin()
	delta := map[string]any{"content": text}
	if len(calls) > 0 {
		indexed := make([]map[string]any, 0, len(calls))
		for i, c := range calls {
			indexed = append(indexed, map[string]any{"index": i, "id": c.ID, "type": c.Type, "function": c.Function})
		}
		delta = map[string]any{"tool_calls": indexed}
	}
	if sw.Chunk(delta, "") != nil {
		return
	}
	if sw.Chunk(map[string]any{}, finish) != nil {
		return
	}
	if req.IncludeUsage {
		if sw.Usage(openAIUsage(res.Usage)) != nil {
			return
		}
	}
	sw.Done()
}
