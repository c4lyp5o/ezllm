package copilotbridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/c4lyp5o/ezllm/internal/bridgeutil"
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
	Runner       Executor
	Token        string
	Version      string
	Models       []Model
	ModelCatalog *ModelCatalog
	Info         map[string]any
	Logf         func(format string, args ...any)
	Now          func() time.Time

	// Models is the curated seed. Ids the CLI reports for this login are
	// learned on top of it (learn) and listed AFTER the seed, so the first
	// candidate the key test probes is always one that was proven.
	lmu     sync.Mutex
	learned []string
}

var modelIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,63}$`)

const maxLearnedModels = 64

// learn folds the CLI's own availableModels into the catalog. Ids are
// validated (they end up in /v1/models and error text) and bounded.
func (s *Server) learn(ids []string) {
	if len(ids) == 0 {
		return
	}
	s.lmu.Lock()
	defer s.lmu.Unlock()
	for _, id := range ids {
		if !modelIDPattern.MatchString(id) || len(s.learned) >= maxLearnedModels || s.hasLocked(id) {
			continue
		}
		s.learned = append(s.learned, id)
	}
}

func (s *Server) hasLocked(id string) bool {
	for _, m := range s.Models {
		if m.ID == id {
			return true
		}
	}
	for _, l := range s.learned {
		if l == id {
			return true
		}
	}
	return false
}

// catalog is the seed followed by the learned ids.
func (s *Server) catalog() []Model {
	out := append([]Model(nil), s.Models...)
	if s.ModelCatalog != nil {
		var live bool
		out, live = s.ModelCatalog.Get(context.Background(), out)
		if live {
			return out
		}
	}
	s.lmu.Lock()
	defer s.lmu.Unlock()
	for _, id := range s.learned {
		out = append(out, Model{ID: id, Object: "model", OwnedBy: "copilot-cli"})
	}
	return out
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
		s.lmu.Lock()
		info["learned_models"] = append([]string(nil), s.learned...)
		s.lmu.Unlock()
		if s.ModelCatalog != nil {
			models, live := s.ModelCatalog.Get(context.Background(), s.Models)
			info["model_catalog"] = map[string]any{"live": live, "count": len(models)}
		}
		if st, ok := s.Runner.(interface{ Stats() map[string]any }); ok {
			info["runtime"] = st.Stats()
		}
		writeJSON(w, http.StatusOK, info)
	})
	mux.HandleFunc("GET /v1/models", s.authed(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": s.catalog()})
	}))
	mux.HandleFunc("POST /v1/chat/completions", s.authed(s.chat))
	return mux
}

// authed admits only the bridge token (bridgeutil.Authorized, constant-time).
// a refusal is a 401 in this bridge's own error shape.
func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !bridgeutil.Authorized(r, s.Token, false) {
			writeRaw(w, http.StatusUnauthorized, ErrorEnvelope("invalid_request_error",
				"copilot-bridge: missing or wrong bearer token", "unauthorized"))
			return
		}
		next(bridgeutil.TrackSlotWait(w, r))
	}
}

func (s *Server) knownModel(id string) bool {
	for _, m := range s.catalog() {
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
	// Refused before a spawn: learning "not available" from the CLI costs a
	// multi-second process start plus a premium request. It is the same typed
	// 403 the CLI's own refusal maps to (writeErr), so the router treats both
	// alike: the model is unavailable, the hop is not unhealthy.
	if req.Model != "" && req.Model != "auto" && !s.knownModel(req.Model) {
		writeRaw(w, http.StatusForbidden, ErrorEnvelope("permission_error",
			"copilot-bridge: model "+req.Model+" is not available on this Copilot login (GET /v1/models lists what is)", "model_not_in_plan"))
		return
	}

	id := "chatcmpl-" + randomHex(12)
	created := s.now().Unix()
	model := req.Model
	if model == "" {
		model = "auto"
	}

	var cacheHistory []json.RawMessage
	if enabled, ok := s.Runner.(interface{ SessionCacheEnabled() bool }); ok && enabled.SessionCacheEnabled() && len(req.Attachments) == 0 {
		cacheHistory = canonicalCacheHistory(body)
	}
	run := func(delta func(string) error, onTools func(Result) error) (Result, error) {
		started := false
		var textSink func(string) error
		if delta != nil {
			textSink = func(text string) error { started = true; return delta(text) }
		}
		var toolSink func(Result) error
		if onTools != nil {
			toolSink = func(batch Result) error { started = true; return onTools(batch) }
		}
		res, err := s.Runner.RunChannel(r.Context(), ChannelTurn{Model: req.Model, System: req.System, Prompt: req.Transcript, Tools: req.Tools, Choice: req.Choice, Attachments: req.Attachments, OnTools: toolSink, cacheHistory: cacheHistory}, textSink)
		// The channel refused before anything was billed or streamed. A request
		// with tools needs the channel, so the refusal stands and the router fails
		// over; plain chat does not, and -p can still serve it.
		var f *Failure
		if len(req.Tools) == 0 && len(req.Attachments) == 0 && !started && errors.As(err, &f) && f.Code == "chat_tools_unsupported" && f.BeforeInference && !res.hasWork() {
			s.logf("copilot-bridge: channel unavailable, plain chat falling back to -p: %s", err)
			return s.Runner.Run(r.Context(), req.Model, req.Prompt, delta)
		}
		return res, err
	}

	if !req.Stream {
		res, err := run(nil, nil)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		s.learn(res.AvailableModels)
		msg := map[string]any{"role": "assistant", "content": res.Text}
		finish := "stop"
		if len(res.ToolCalls) > 0 {
			finish = "tool_calls"
			msg["tool_calls"] = openAIToolCalls(res.ToolCalls, false)
			if res.Text == "" {
				msg["content"] = nil
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "object": "chat.completion", "created": created, "model": servedModel(model, res),
			"choices": []map[string]any{{"index": 0, "finish_reason": finish, "message": msg}},
			"usage":   openAIUsage(res.Usage),
		})
		return
	}

	transport := bridgeutil.NewStream(r.Context(), w, bridgeutil.StreamWriteStall)
	defer transport.Close()
	sw := newSSEWriter(w, transport, id, created, model)
	toolsSent := false
	res, err := run(sw.delta, func(batch Result) error {
		sw.Model = servedModel(model, batch)
		sw.Begin()
		if !sw.sent && batch.Text != "" {
			if err := sw.delta(batch.Text); err != nil {
				return err
			}
		}
		if err := sw.Chunk(map[string]any{"tool_calls": openAIToolCalls(batch.ToolCalls, true)}, ""); err != nil {
			return err
		}
		toolsSent = true
		return nil
	})
	if err != nil {
		if !sw.Started() {
			s.writeErr(w, err)
			return
		}
		ne := asEnvelope(err)
		sw.Event(json.RawMessage(ErrorEnvelope(ne.Type, ne.Message, ne.Code)))
		sw.Done()
		s.logf("copilot-bridge: stream failed: %s", err)
		return
	}
	s.learn(res.AvailableModels)
	sw.Model = servedModel(model, res)
	sw.Begin()
	if !sw.sent && res.Text != "" {
		// The CLI sent only a final assistant.message (no deltas): without this
		// the client would get a 200 stream with an empty answer.
		sw.Chunk(map[string]any{"content": res.Text}, "")
	}
	if len(res.ToolCalls) > 0 {
		if !toolsSent {
			sw.Chunk(map[string]any{"tool_calls": openAIToolCalls(res.ToolCalls, true)}, "")
		}
		sw.Chunk(map[string]any{}, "tool_calls")
	} else {
		sw.Chunk(map[string]any{}, "stop")
	}
	if req.IncludeUsage {
		sw.Usage(openAIUsage(res.Usage))
	}
	sw.Done()
}

// openAIToolCalls renders calls on the wire. A stream delta carries an index per
// call; a complete message does not.
func openAIToolCalls(calls []ToolCall, indexed bool) []map[string]any {
	out := make([]map[string]any, 0, len(calls))
	for i, c := range calls {
		m := map[string]any{"id": c.ID, "type": "function",
			"function": map[string]any{"name": c.Name, "arguments": c.Arguments}}
		if indexed {
			m["index"] = i
		}
		out = append(out, m)
	}
	return out
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
		"prompt_tokens_details":     map[string]any{"cached_tokens": u.CachedRead, "cache_write_tokens": u.CachedWrite},
		"completion_tokens_details": map[string]any{"reasoning_tokens": u.ReasoningTokens},
		// Not an OpenAI field; kept so the ledger's raw_usage shows what the
		// subscription actually charged (1 per turn by default).
		"copilot_premium_requests": u.PremiumRequests,
		"copilot_nano_aiu":         u.NanoAIU,
	}
}

// writeErr maps a typed failure to the right status: an auth problem is the
// client's 401 (fail fast, never park the hop as unhealthy), a plan refusal is
// 402/429, anything else is a real 502.
func (s *Server) writeErr(w http.ResponseWriter, err error) {
	var f *Failure
	if errors.As(err, &f) {
		switch f.Code {
		case "vision_unsupported", "invalid_image":
			writeRaw(w, http.StatusBadRequest, ErrorEnvelope("invalid_request_error", f.Msg, f.Code))
			return
		case "chat_tools_unsupported":
			// Refused before anything billable: the router fails over on this
			// code without parking the hop (proxy.go refusesTools).
			writeRaw(w, http.StatusBadRequest, ErrorEnvelope("invalid_request_error", f.Msg, f.Code))
			return
		case "not_logged_in":
			writeRaw(w, http.StatusUnauthorized, ErrorEnvelope("authentication_error", f.Msg, f.Code))
			return
		case "quota_exhausted":
			writeRaw(w, http.StatusTooManyRequests, ErrorEnvelope("rate_limit_error", f.Msg, f.Code))
			return
		case "queue_full":
			w.Header().Set("Retry-After", bridgeutil.QueueFullRetryAfter)
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

type sseWriter struct {
	*bridgeutil.ChatChunks
	sent bool // at least one content delta reached the client
}

// newSSEWriter streams chat chunks through the shared encoder (transport and
// framing in internal/bridgeutil). A failed write is sticky: no later event or
// [DONE] reaches the client.
func newSSEWriter(w http.ResponseWriter, transport *bridgeutil.Stream, id string, created int64, model string) *sseWriter {
	return &sseWriter{ChatChunks: bridgeutil.NewChatChunks(w, transport, id, created, model)}
}

func (s *sseWriter) delta(text string) error {
	if text == "" {
		return nil
	}
	s.Begin()
	s.sent = true
	return s.Chunk(map[string]any{"content": text}, "")
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
		case "quota_exhausted", "queue_full":
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

// Known output or usage forbids a second generation even if a refusal was
// mistakenly labelled pre-inference. Unknown usage alone never permits fallback.
func (r Result) hasWork() bool {
	u := r.Usage
	return r.Text != "" || len(r.ToolCalls) > 0 || u.PromptTokens != 0 || u.CompletionTokens != 0 || u.CachedRead != 0 || u.CachedWrite != 0 || u.ReasoningTokens != 0 || u.PremiumRequests != 0 || u.NanoAIU != 0
}
