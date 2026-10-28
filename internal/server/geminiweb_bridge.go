package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/c4lyp5o/ezllm/internal/geminiweb"
	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// The gemini-web bridge speaks the OpenAI wire on a loopback-internal mount
// and drives internal/geminiweb behind it. The provider adapter
// (provider/geminiweb.go) rewrites gemini-web hops here; registration's
// auth oracle is this bridge's GET /models (zero generation quota).
//
// Mount: POST /internal/geminiweb/v1/chat/completions, GET /internal/geminiweb/v1/models.
// Auth: the X-Ezllm-Gemini-Web-Account header stamped by the adapter — it
// must map to an ENABLED gemini-web account. The credential comes from the
// stamped key header (preferred, so a bad-key calibration probe exercises
// the same path as live traffic) or the account's stored key.

const geminiWebBridgePrefix = "/internal/geminiweb/v1"

type geminiWebBridge struct {
	db  *store.DB
	log *slog.Logger
	// geminiBase is the upstream origin; empty = production
	// (gemini.google.com). Tests inject a mock.
	geminiBase string

	mu      sync.Mutex
	clients map[int64]*geminiwebClientEntry
	now     func() time.Time // tests
	rotate  func(context.Context, *geminiweb.Client) (string, error)
	// genLocks serializes per-account use of a cached client. The client
	// carries mutable session/cookie state (and rotates them) and is not
	// safe for concurrent use, so two chats on one web account must not
	// run at once.
	genLocks map[int64]*sync.Mutex
}

type geminiwebClientEntry struct {
	accountID int64
	keyID     int64 // provider_keys row the key came from (for rotation persistence)
	client    *geminiweb.Client
	catalog   []geminiweb.Model
	catalogAt time.Time
	key       string    // the credential the client was built from
	rotatedAt time.Time // last rotation attempt (zero = never)
}

const (
	geminiWebCatalogTTL  = 30 * time.Minute
	geminiWebRotateEvery = 8 * time.Minute // upstream rotates ~10; stay ahead
)

// newGeminiWebBridge builds the bridge. geminiBase empty = production
// upstream; tests pass a mock origin.
func newGeminiWebBridge(db *store.DB, geminiBase string) *geminiWebBridge {
	return &geminiWebBridge{db: db, log: slog.Default(), geminiBase: geminiBase, clients: map[int64]*geminiwebClientEntry{}, genLocks: map[int64]*sync.Mutex{}, now: time.Now, rotate: func(ctx context.Context, c *geminiweb.Client) (string, error) { return c.Rotate(ctx) }}
}

// lockFor returns the per-account generation mutex, creating it on first use.
func (b *geminiWebBridge) lockFor(acctID int64) *sync.Mutex {
	b.mu.Lock()
	defer b.mu.Unlock()
	l, ok := b.genLocks[acctID]
	if !ok {
		l = &sync.Mutex{}
		b.genLocks[acctID] = l
	}
	return l
}

// register mounts the bridge routes on the server mux.
func (b *geminiWebBridge) register(mux *http.ServeMux) {
	mux.HandleFunc("POST "+geminiWebBridgePrefix+"/chat/completions", b.handleChatCompletions)
	mux.HandleFunc("GET "+geminiWebBridgePrefix+"/models", b.handleModels)
}

// clientFor resolves a live geminiweb client for an account: cached client,
// lazy Init, and periodic cookie rotation. A stamped key that differs from
// the cached one rebuilds the client (credential rotated or re-registered).
func (b *geminiWebBridge) clientFor(ctx context.Context, acctID int64, stampedKey string) (*geminiwebClientEntry, error) {
	b.mu.Lock()
	entry, ok := b.clients[acctID]
	if ok && entry.client.Initialized() && (stampedKey == "" || stampedKey == entry.key) {
		b.mu.Unlock()
		// The caller (resolve) holds the per-account lock, so reading the
		// session state above cannot overlap rotation's Init.
		return entry, nil
	}
	b.mu.Unlock()

	key, keyID := stampedKey, int64(0)
	if key == "" {
		pick, err := b.db.PickKey(ctx, acctID)
		if err != nil {
			return nil, fmt.Errorf("gemini-web bridge: pick key: %w", err)
		}
		key, keyID = pick.Plaintext, pick.ID
	}
	if stampedKey != "" {
		// A calibration key need not be stored. Only associate a row when its
		// actual credential matches, so probing cannot overwrite another key.
		keys, err := b.db.ListKeys(ctx, acctID)
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			_, plaintext, err := b.db.KeyForTest(ctx, k.ID)
			if err != nil {
				return nil, err
			}
			if plaintext == stampedKey {
				keyID = k.ID
				break
			}
		}
	}
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("gemini-web bridge: empty credential")
	}
	c, err := geminiweb.NewFromKey(key)
	if err != nil {
		return nil, err
	}
	if b.geminiBase != "" {
		c.WithBase(b.geminiBase)
	}
	if err := c.Init(ctx); err != nil {
		return nil, err
	}
	entry = &geminiwebClientEntry{accountID: acctID, keyID: keyID, client: c, key: key}
	b.mu.Lock()
	b.clients[acctID] = entry
	b.mu.Unlock()
	return entry, nil
}

// maybeRotate keeps the short-lived PSIDTS cookie fresh and persists the
// rotated pair. Best-effort by design: a failed rotation leaves the client
// usable (upstream keeps accepting the previous cookie for a while) and the
// next tick retries.
func (b *geminiWebBridge) maybeRotate(ctx context.Context, entry *geminiwebClientEntry) {
	b.mu.Lock()
	if b.now().Sub(entry.rotatedAt) < geminiWebRotateEvery {
		b.mu.Unlock()
		return
	}
	entry.rotatedAt = b.now()
	keyID := entry.keyID
	oldKey := entry.key
	b.mu.Unlock()

	newKey, err := b.rotate(ctx, entry.client)
	if err != nil {
		// Best-effort: leave the client usable with its previous cookie.
		b.mu.Lock()
		entry.rotatedAt = time.Time{}
		b.mu.Unlock()
		return
	}
	// Re-scrape the access token against the rotated cookie. The SNlM0e
	// access token is tied to the cookie session and must be refreshed
	// after a rotation; otherwise the cached client keeps a stale token.
	if err := entry.client.Init(ctx); err != nil {
		// Revert to the pre-rotation session: rebuild the client from the
		// old key so the stale (but still valid) cookies keep working.
		c, perr := geminiweb.NewFromKey(oldKey)
		if perr == nil {
			if b.geminiBase != "" {
				c.WithBase(b.geminiBase)
			}
			if ierr := c.Init(ctx); ierr == nil {
				b.mu.Lock()
				entry.client = c
				entry.key = oldKey
				entry.rotatedAt = time.Time{}
				b.mu.Unlock()
				return
			}
		}
		b.mu.Lock()
		entry.key = oldKey
		entry.rotatedAt = time.Time{}
		b.mu.Unlock()
		return
	}
	if keyID != 0 {
		if err := b.db.UpdateKeyPlain(ctx, keyID, newKey); err != nil {
			b.log.Warn("gemini-web cookie rotation could not be saved; retrying next interval", "account_id", entry.accountID)
			// Keep the old cache key and leave rotatedAt as set above, so the
			// retry waits a full interval instead of hitting the upstream
			// rotate endpoint on every request while the DB write fails.
			return
		}
	}
	b.mu.Lock()
	entry.key = newKey
	b.mu.Unlock()
}

// models returns the account's catalog, cached for the TTL.
func (b *geminiWebBridge) models(ctx context.Context, entry *geminiwebClientEntry) ([]geminiweb.Model, error) {
	b.mu.Lock()
	if len(entry.catalog) > 0 && b.now().Sub(entry.catalogAt) < geminiWebCatalogTTL {
		out := entry.catalog
		b.mu.Unlock()
		return out, nil
	}
	b.mu.Unlock()

	st, err := entry.client.GetUserStatus(ctx)
	if err != nil {
		return nil, err
	}
	if st.Status.Code != 1000 {
		return nil, fmt.Errorf("gemini-web bridge: account status %d (%s)", st.Status.Code, st.Status.Name)
	}
	b.mu.Lock()
	entry.catalog = st.Models
	entry.catalogAt = b.now()
	b.mu.Unlock()
	return st.Models, nil
}

// handleModels is the auth oracle + catalog: 200 with the OpenAI-shaped
// model list on a live cookie, 401 on a dead one.
func (b *geminiWebBridge) handleModels(w http.ResponseWriter, r *http.Request) {
	entry, unlock, err := b.resolve(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "authentication_error", err.Error())
		return
	}
	defer unlock()
	b.maybeRotate(r.Context(), entry)
	models, err := b.models(r.Context(), entry)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "authentication_error", err.Error())
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
	for _, m := range models {
		id := m.Name
		if id == "" {
			id = m.ID
		}
		list.Data = append(list.Data, modelEntry{ID: id, Object: "model", Created: 0, OwnedBy: "google"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(list)
}

// chatReq is the subset of the OpenAI chat body the bridge reads.
type chatReq struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	} `json:"messages"`
	Stream         bool `json:"stream"`
	Tools          any  `json:"tools"`
	ResponseFormat any  `json:"response_format"`
}

// handleChatCompletions translates one OpenAI chat turn into a gemini-web
// generation and answers OpenAI wire (SSE when stream=true).
func (b *geminiWebBridge) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	// Serialize generation per account: the cached client owns mutable
	// session/cookie state and is not safe for concurrent use.
	entry, unlock, err := b.resolve(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "authentication_error", err.Error())
		return
	}
	defer unlock()
	b.maybeRotate(r.Context(), entry)
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "cannot read body")
		return
	}
	var req chatReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}
	if req.Model == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	// Typed refusals — the web protocol has no tool calling, no structured
	// output, no image input (docs/gemini-web-design.md scope fences).
	if req.Tools != nil {
		writeErr(w, http.StatusBadRequest, "unsupported_feature",
			"gemini-web does not support tool calling; use a gemini-openai account for tools")
		return
	}
	if req.ResponseFormat != nil {
		writeErr(w, http.StatusBadRequest, "unsupported_feature",
			"gemini-web does not support structured output (response_format)")
		return
	}
	model, err := b.resolveModel(r.Context(), entry, req.Model)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "model_not_found", err.Error())
		return
	}

	chatID := "chatcmpl-gw-" + geminiweb.NewUUID()[:13]
	opts := geminiweb.GenerateOptions{Model: &model, Prompt: flattenMessages(req.Messages)}

	if !req.Stream {
		res, err := entry.client.Generate(r.Context(), opts, func(geminiweb.Delta) {})
		if err != nil {
			writeErr(w, mapUpstreamStatus(err), "upstream_error", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openAIChatResponse(chatID, model.Name, res.Text))
		return
	}

	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	emit := func(role, content, finish string) {
		chunk := map[string]any{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model.Name,
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         map[string]any{"role": role, "content": content},
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
	_, err = entry.client.Generate(r.Context(), opts, func(d geminiweb.Delta) {
		if d.Text != "" {
			emit("", d.Text, "")
		}
	})
	if err != nil {
		// Mid-stream failure: emit an OpenAI-shaped error event and stop.
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

// resolveModel matches a requested id against the catalog: exact name, exact
// raw id, or alias (all derived server-side; nothing hardcoded).
func (b *geminiWebBridge) resolveModel(ctx context.Context, entry *geminiwebClientEntry, requested string) (geminiweb.Model, error) {
	models, err := b.models(ctx, entry)
	if err != nil {
		return geminiweb.Model{}, err
	}
	for _, m := range models {
		if m.Name == requested || m.ID == requested {
			return m, nil
		}
	}
	for _, m := range models {
		for _, a := range m.Aliases {
			if a == requested {
				return m, nil
			}
		}
	}
	return geminiweb.Model{}, fmt.Errorf("model %q not in this account's catalog (%d models)", requested, len(models))
}

// resolve maps the stamped headers to an enabled gemini-web account and
// returns its client with the per-account lock HELD; the caller defers the
// returned unlock. The lock covers the cache lookup too: clientFor reads the
// client's session state, which rotation rewrites under this same lock.
func (b *geminiWebBridge) resolve(r *http.Request) (*geminiwebClientEntry, func(), error) {
	acct, err := bridgeAccount(r, b.db, provider.KindGeminiWeb, provider.GeminiWebAccountHeader)
	if err != nil {
		return nil, nil, err
	}
	lock := b.lockFor(acct.ID)
	lock.Lock()
	entry, err := b.clientFor(r.Context(), acct.ID, r.Header.Get(provider.GeminiWebKeyHeader))
	if err != nil {
		lock.Unlock()
		return nil, nil, err
	}
	return entry, lock.Unlock, nil
}

// mapUpstreamStatus maps a geminiweb error to an HTTP status: auth failures
// are 401 (classified bad_credentials by provider.ErrorCause), rate limits
// 429, everything else 502.
func mapUpstreamStatus(err error) int {
	var ue geminiweb.ErrUpstream
	if errors.As(err, &ue) {
		switch ue.Code {
		case 1037:
			return http.StatusTooManyRequests
		case 1013:
			return http.StatusServiceUnavailable
		case 1060:
			return http.StatusForbidden
		}
		return http.StatusBadGateway
	}
	if errors.Is(err, geminiweb.ErrNoAccessToken) {
		return http.StatusUnauthorized
	}
	return http.StatusBadGateway
}

// flattenMessages joins OpenAI messages into one prompt (the web protocol
// has no message array — a documented simplification, see the design doc).
// System messages are dropped: they carry the client's persona/memory
// (SOUL.md/MEMORY.md/USER.md) which must never reach a web provider.
func flattenMessages(msgs []struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}) string {
	var parts []string
	for _, m := range msgs {
		if m.Role == "system" {
			continue // stealth: never forward persona/memory to web providers
		}
		if text := contentToText(m.Content); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

func contentToText(c any) string {
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

func openAIChatResponse(id, model, text string) map[string]any {
	return map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
	}
}
