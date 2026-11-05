package copilotbridge

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const maxBodyBytes = 32 << 20

// Model is one routable id as the bridge advertises it.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

// Server exposes OpenAI Chat Completions over the CLI. Only ezllm dials it.
type Server struct {
	Runner  Executor
	Token   string
	Version string
	Models  []Model
	Info    map[string]any
	Logf    func(format string, args ...any)
	Now     func() time.Time
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

// Handler builds the mux: /healthz (house contract), /status (operator detail,
// including what this hop CANNOT do), /v1/models, /v1/chat/completions.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": s.Version})
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		info := map[string]any{}
		for k, v := range s.Info {
			info[k] = v
		}
		// Never let a bridge card imply a capability this hop does not have.
		info["client_tools"] = "refused: the CLI runs its own agent loop (see design §10)"
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
			writeRaw(w, http.StatusUnauthorized, ErrorEnvelope("invalid_request_error",
				"copilot-bridge: missing or wrong bearer token", "unauthorized"))
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
		writeRaw(w, http.StatusBadRequest, ErrorEnvelope("invalid_request_error",
			"copilot-bridge: cannot read request body", "invalid_request"))
		return
	}
	if len(body) > maxBodyBytes {
		writeRaw(w, http.StatusRequestEntityTooLarge, ErrorEnvelope("invalid_request_error",
			"copilot-bridge: request body too large", "invalid_request"))
		return
	}
	req, err := ParseRequest(body)
	if err != nil {
		code := "invalid_request"
		var u Unsupported
		if errors.As(err, &u) {
			code = u.Code
		}
		writeRaw(w, http.StatusBadRequest, ErrorEnvelope("invalid_request_error", err.Error(), code))
		return
	}
	// Refused before a spawn: learning "unknown model" from the CLI costs a
	// multi-second process start plus a premium request.
	if req.Model != "" && req.Model != "auto" && !s.knownModel(req.Model) {
		ids := make([]string, 0, len(s.Models))
		for _, m := range s.Models {
			ids = append(ids, m.ID)
		}
		writeRaw(w, http.StatusBadRequest, ErrorEnvelope("invalid_request_error",
			"copilot-bridge: model "+req.Model+" is not on this login's plan (have: "+strings.Join(ids, ", ")+")", "unknown_model"))
		return
	}

	id := "chatcmpl-" + randomHex(12)
	created := s.now().Unix()
	model := req.Model
	if model == "" {
		model = "auto"
	}

	if !req.Stream {
		res, err := s.Runner.Run(r.Context(), req.Model, req.Prompt, nil)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "object": "chat.completion", "created": created, "model": servedModel(model, res),
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": res.Text}}},
			"usage": openAIUsage(res.Usage),
		})
		return
	}

	sw := &sseWriter{w: w, id: id, created: created, model: model, rc: http.NewResponseController(w)}
	defer sw.bind(r.Context())()
	res, err := s.Runner.Run(r.Context(), req.Model, req.Prompt, sw.delta)
	if err != nil {
		if !sw.started {
			s.writeErr(w, err)
			return
		}
		ne := asEnvelope(err)
		sw.event(json.RawMessage(ErrorEnvelope(ne.Type, ne.Message, ne.Code)))
		sw.done()
		s.logf("copilot-bridge: stream failed: %s", err)
		return
	}
	sw.model = servedModel(model, res)
	sw.begin()
	if !sw.sent && res.Text != "" {
		// The CLI sent only a final assistant.message (no deltas): without this
		// the client would get a 200 stream with an empty answer.
		sw.chunk(map[string]any{"content": res.Text}, "")
	}
	sw.chunk(map[string]any{}, "stop")
	if req.IncludeUsage {
		sw.event(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created,
			"model": sw.model, "choices": []any{}, "usage": openAIUsage(res.Usage)})
	}
	sw.done()
}

func servedModel(requested string, res Result) string {
	if res.Usage.Model != "" {
		return res.Usage.Model
	}
	if requested == "auto" {
		return "auto"
	}
	return requested
}

// openAIUsage maps the CLI's measured split onto the OpenAI envelope the usage
// tap reads. prompt_tokens is INCLUSIVE (cold input + cache read + cache write)
// and the cached slice rides prompt_tokens_details, which is exactly the shape
// the ledger's cold-token derivation expects.
func openAIUsage(u Usage) map[string]any {
	return map[string]any{
		"prompt_tokens":             u.PromptTokens,
		"completion_tokens":         u.CompletionTokens,
		"total_tokens":              u.PromptTokens + u.CompletionTokens,
		"prompt_tokens_details":     map[string]any{"cached_tokens": u.CachedRead},
		"completion_tokens_details": map[string]any{"reasoning_tokens": u.ReasoningTokens},
		// Not an OpenAI field; kept so the ledger's raw_usage shows what the
		// subscription actually charged (1 per turn by default).
		"copilot_premium_requests": u.PremiumRequests,
	}
}

// writeErr maps a typed failure to the right status: an auth problem is the
// client's 401 (fail fast, never park the hop as unhealthy), a plan refusal is
// 402/429, anything else is a real 502.
func (s *Server) writeErr(w http.ResponseWriter, err error) {
	var f *Failure
	if errors.As(err, &f) {
		switch f.Code {
		case "not_logged_in":
			writeRaw(w, http.StatusUnauthorized, ErrorEnvelope("authentication_error", f.Msg, f.Code))
			return
		case "quota_exhausted":
			writeRaw(w, http.StatusTooManyRequests, ErrorEnvelope("rate_limit_error", f.Msg, f.Code))
			return
		case "model_not_in_plan":
			writeRaw(w, http.StatusForbidden, ErrorEnvelope("permission_error", f.Msg, f.Code))
			return
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		writeRaw(w, http.StatusGatewayTimeout, ErrorEnvelope("timeout_error", "copilot-bridge: the CLI did not finish in time", "timeout"))
		return
	}
	writeRaw(w, http.StatusBadGateway, ErrorEnvelope("api_error", "copilot-bridge: "+err.Error(), "cli_failed"))
}

// ── SSE ─────────────────────────────────────────────────────────────────────

const streamWriteStall = 60 * time.Second

type sseWriter struct {
	w       http.ResponseWriter
	id      string
	created int64
	model   string
	started bool
	sent    bool // at least one content delta reached the client

	rc        *http.ResponseController
	dmu       sync.Mutex
	cancelled bool
}

func (s *sseWriter) bind(ctx context.Context) func() {
	stop := context.AfterFunc(ctx, func() {
		s.dmu.Lock()
		s.cancelled = true
		_ = s.rc.SetWriteDeadline(time.Now())
		s.dmu.Unlock()
	})
	return func() {
		if stop() {
			_ = s.rc.SetWriteDeadline(time.Time{})
		}
	}
}

func (s *sseWriter) arm() error {
	if s.rc == nil {
		return nil
	}
	s.dmu.Lock()
	defer s.dmu.Unlock()
	if s.cancelled {
		return context.Canceled
	}
	_ = s.rc.SetWriteDeadline(time.Now().Add(streamWriteStall))
	return nil
}

func (s *sseWriter) begin() {
	if s.started {
		return
	}
	s.started = true
	s.w.Header().Set("Content-Type", "text/event-stream")
	s.w.Header().Set("Cache-Control", "no-cache")
	s.w.Header().Set("Connection", "keep-alive")
	s.w.WriteHeader(http.StatusOK)
	if s.rc != nil {
		_ = s.rc.Flush()
	}
}

func (s *sseWriter) event(v any) {
	var b []byte
	switch t := v.(type) {
	case json.RawMessage:
		b = t
	default:
		b, _ = json.Marshal(t)
	}
	if err := s.arm(); err != nil {
		return
	}
	_, _ = fmt.Fprintf(s.w, "data: %s\n\n", b)
	if s.rc != nil {
		_ = s.rc.Flush()
	}
}

func (s *sseWriter) chunk(delta map[string]any, finish string) {
	var fr any
	if finish != "" {
		fr = finish
	}
	s.event(map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": fr}},
	})
}

func (s *sseWriter) delta(text string) error {
	if text == "" {
		return nil
	}
	s.begin()
	s.sent = true
	s.chunk(map[string]any{"content": text}, "")
	return s.arm()
}

func (s *sseWriter) done() {
	if err := s.arm(); err != nil {
		return
	}
	_, _ = io.WriteString(s.w, "data: [DONE]\n\n")
	if s.rc != nil {
		_ = s.rc.Flush()
	}
}

// ── envelopes ───────────────────────────────────────────────────────────────

func ErrorEnvelope(typ, msg, code string) []byte {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": msg, "type": typ, "param": nil, "code": code,
	}})
	return b
}

type envelope struct{ Type, Message, Code string }

func asEnvelope(err error) envelope {
	var f *Failure
	if errors.As(err, &f) {
		switch f.Code {
		case "not_logged_in":
			return envelope{"authentication_error", f.Msg, f.Code}
		case "quota_exhausted":
			return envelope{"rate_limit_error", f.Msg, f.Code}
		case "model_not_in_plan":
			return envelope{"permission_error", f.Msg, f.Code}
		}
		return envelope{"api_error", f.Msg, f.Code}
	}
	return envelope{"api_error", "copilot-bridge: " + err.Error(), "cli_failed"}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeRaw(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
