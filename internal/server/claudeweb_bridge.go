package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/c4lyp5o/ezllm/internal/claudeweb"
	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// The claude-web bridge: loopback-internal OpenAI-wire routes the
// claude-web adapter rewrites hops to. Behind it, internal/claudeweb drives
// claude.ai with the Chrome TLS profile (mount: docs/claude-web-design.md).
//
// Mount: POST /internal/claudeweb/v1/chat/completions, GET /internal/claudeweb/v1/models.
// Auth: the stamped account header maps to an ENABLED claude-web account;
// the credential is the stamped key (or the account's stored key).
//
// Conversation scope (documented): ONE conversation per account-client. The
// upstream state machine chains conversation_id + parent uuids inside that
// conversation; multi-conversation routing is out of M14 scope.

const claudeWebBridgePrefix = "/internal/claudeweb/v1"

type claudeWebBridge struct {
	db *store.DB
	// newClient builds a session client (injected for tests: a plain-HTTP
	// fetcher against a mock).
	newClient func(ctx context.Context, acct *provider.Account, key string) (*claudeweb.Client, error)
	fetcher   claudeweb.Fetcher // nil = the production uTLS fetcher

	mu      sync.Mutex
	clients map[int64]*claudeweb.Client
}

func newClaudeWebBridge(db *store.DB, fetcher claudeweb.Fetcher) *claudeWebBridge {
	b := &claudeWebBridge{db: db, fetcher: fetcher, clients: map[int64]*claudeweb.Client{}}
	if fetcher == nil {
		b.newClient = b.clientFor
	} else {
		b.newClient = func(ctx context.Context, acct *provider.Account, key string) (*claudeweb.Client, error) {
			cookie, err := claudeweb.NormalizeSessionCookie(key)
			if err != nil {
				return nil, err
			}
			return claudeweb.New(fetcher, cookie, ""), nil
		}
	}
	return b
}

// clientFor is the production client builder: normalizes the stored
// sessionKey and caches one client per account (one conversation each).
func (b *claudeWebBridge) clientFor(_ context.Context, acct *provider.Account, key string) (*claudeweb.Client, error) {
	cookie, err := claudeweb.NormalizeSessionCookie(key)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if c, ok := b.clients[acct.ID]; ok {
		return c, nil
	}
	c := claudeweb.New(claudeweb.NewTLSFetcher(), cookie, "")
	b.clients[acct.ID] = c
	return c, nil
}

func (b *claudeWebBridge) register(mux *http.ServeMux) {
	mux.HandleFunc("POST "+claudeWebBridgePrefix+"/chat/completions", b.handleChatCompletions)
	mux.HandleFunc("GET "+claudeWebBridgePrefix+"/models", b.handleModels)
}

// resolve maps the stamped headers to an enabled claude-web account + client.
func (b *claudeWebBridge) resolve(r *http.Request) (*claudeweb.Client, error) {
	h := r.Header.Get(provider.ClaudeWebAccountHeader)
	if h == "" {
		return nil, errors.New("missing account header")
	}
	var acctID int64
	if _, err := fmt.Sscan(h, &acctID); err != nil || acctID <= 0 {
		return nil, errors.New("bad account header")
	}
	acct, err := b.db.AccountByID(r.Context(), acctID)
	if err != nil || acct == nil || acct.Kind != provider.KindClaudeWeb || !acct.Enabled {
		return nil, errors.New("not an enabled claude-web account")
	}
	key := r.Header.Get(provider.ClaudeWebKeyHeader)
	if key == "" {
		pick, err := b.db.PickKey(r.Context(), acctID)
		if err != nil {
			return nil, fmt.Errorf("claude-web bridge: pick key: %w", err)
		}
		key = pick.Plaintext
	}
	return b.newClient(r.Context(), acct, key)
}

// handleModels is the auth oracle: org discovery first (zero quota), then
// the bootstrap catalog. Dead cookie -> 401.
func (b *claudeWebBridge) handleModels(w http.ResponseWriter, r *http.Request) {
	c, err := b.resolve(r)
	if err != nil {
		b.writeErr(w, http.StatusUnauthorized, "authentication_error", err.Error())
		return
	}
	if _, err := c.OrganizationID(r.Context()); err != nil {
		b.writeErr(w, claudeWebStatus(err), "authentication_error", err.Error())
		return
	}
	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	list := struct {
		Object string       `json:"object"`
		Data   []modelEntry `json:"data"`
	}{Object: "list"}
	for _, id := range provider.ClaudeWebDefaultModels {
		list.Data = append(list.Data, modelEntry{ID: id, Object: "model", Created: 0, OwnedBy: "anthropic"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(list)
}

// handleChatCompletions translates one OpenAI chat turn into a claude-web
// completion (SSE or JSON out).
func (b *claudeWebBridge) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	c, err := b.resolve(r)
	if err != nil {
		b.writeErr(w, http.StatusUnauthorized, "authentication_error", err.Error())
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		b.writeErr(w, http.StatusBadRequest, "invalid_request_error", "cannot read body")
		return
	}
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		Stream         bool `json:"stream"`
		Tools          any  `json:"tools"`
		ResponseFormat any  `json:"response_format"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		b.writeErr(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}
	if req.Model == "" {
		b.writeErr(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	// Typed refusal: the web protocol has no structured output (design scope
	// fences). Tools DO exist on this wire (Anthropic-native) and pass
	// through unchanged.
	if req.ResponseFormat != nil {
		b.writeErr(w, http.StatusBadRequest, "unsupported_feature",
			"claude-web does not support structured output (response_format)")
		return
	}
	// No model gate: the web protocol has no discovery and ids pass through
	// (design doc). The bootstrap catalog seeds ROUTING; what the account can
	// actually serve is revealed by the upstream answer.
	opts := claudeweb.GenerateOptions{Model: req.Model, Prompt: claudeWebFlatten(req.Messages)}
	if tools, ok := req.Tools.([]any); ok {
		opts.Tools = tools
	}
	chatID := "chatcmpl-cw-" + claudeweb.NewUUID()[:13]

	if !req.Stream {
		res, err := c.Generate(r.Context(), opts, nil)
		if err != nil {
			b.writeErr(w, claudeWebStatus(err), "upstream_error", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(claudeWebChatResponse(chatID, req.Model, res.Text))
		return
	}

	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	emit := func(role, content, finish string) {
		chunk := map[string]any{
			"id": chatID, "object": "chat.completion.chunk",
			"created": time.Now().Unix(), "model": req.Model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"role": role, "content": content},
				"finish_reason": finish,
			}},
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", data)
		if fl != nil {
			fl.Flush()
		}
	}
	emit("assistant", "", "")
	_, err = c.Generate(r.Context(), opts, func(d string) { emit("", d, "") })
	if err != nil {
		data, _ := json.Marshal(map[string]any{"error": map[string]any{
			"message": err.Error(), "type": "upstream_error",
		}})
		fmt.Fprintf(w, "data: %s\n\n", data)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
		return
	}
	emit("", "", "stop")
	fmt.Fprint(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

func (b *claudeWebBridge) writeErr(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": errType, "code": status},
	})
}

// claudeWebStatus maps the typed client failures to HTTP: expired 401
// (classifies bad_credentials), challenge 403 with its own type, rate 429,
// cut/unavailable 502.
func claudeWebStatus(err error) int {
	switch {
	case errors.Is(err, claudeweb.ErrSessionExpired):
		return http.StatusUnauthorized
	case errors.Is(err, claudeweb.ErrCloudflareChallenge), errors.Is(err, claudeweb.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, claudeweb.ErrRateLimited):
		return http.StatusTooManyRequests
	default:
		return http.StatusBadGateway
	}
}

func claudeWebFlatten(msgs []struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}) string {
	var parts []string
	for _, m := range msgs {
		if t := claudeWebContentText(m.Content); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n")
}

func claudeWebContentText(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, p := range v {
			if part, ok := p.(map[string]any); ok {
				if t, ok := part["text"].(string); ok {
					sb.WriteString(t)
					sb.WriteString("\n")
				}
			}
		}
		return strings.TrimSpace(sb.String())
	}
	return ""
}

func claudeWebChatResponse(id, model, text string) map[string]any {
	return map[string]any{
		"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{
			"index": 0, "message": map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
	}
}
