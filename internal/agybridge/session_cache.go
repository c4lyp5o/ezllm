package agybridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

const maxCachedHistoryBytes = 1 << 20
const maxCacheAge = 15 * time.Minute
const maxCacheTurns = 100
const transcriptMarker = "Caller transcript (JSON):\n"

var conversationIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type decodedReply struct {
	text  string
	calls []ToolCall
}
type cacheRequest struct {
	key     [32]byte
	history []json.RawMessage
}
type cachedConversation struct {
	key                [32]byte
	history            []json.RawMessage
	assistant          json.RawMessage
	toolIDs            []string
	conversation, work string
	native             Usage
	created, last      time.Time
	turns              int
	timer              *time.Timer
	generation         uint64
	binary             os.FileInfo
}

func cacheState(req Request) *cacheRequest {
	if req.Adapter == nil || len(req.Prompt) > maxCachedHistoryBytes {
		return nil
	}
	i := strings.Index(req.Prompt, transcriptMarker)
	if i < 0 {
		return nil
	}
	var history []json.RawMessage
	if json.Unmarshal([]byte(req.Prompt[i+len(transcriptMarker):]), &history) != nil || len(history) == 0 {
		return nil
	}
	config, _ := json.Marshal(map[string]any{"model": req.Model, "effort": req.Effort, "tools": req.Adapter.Tools, "choice": req.Adapter.Choice, "parallel": req.Adapter.Parallel})
	// Parameter objects may arrive with a different JSON key order. Compare
	// their semantic structure without rounding numeric schema values.
	var value any
	dec := json.NewDecoder(bytes.NewReader(config))
	dec.UseNumber()
	if dec.Decode(&value) != nil {
		return nil
	}
	config, _ = json.Marshal(value)
	return &cacheRequest{key: sha256.Sum256(config), history: history}
}
func (e *cachedConversation) continuation(next *cacheRequest) (string, bool) {
	if next == nil || e.key != next.key || len(next.history) <= len(e.history)+1 {
		return "", false
	}
	for i, raw := range e.history {
		if !bytes.Equal(raw, next.history[i]) {
			return "", false
		}
	}
	if !bytes.Equal(e.assistant, next.history[len(e.history)]) {
		return "", false
	}
	tail := next.history[len(e.history)+1:]
	if len(e.toolIDs) == 0 {
		var last struct {
			Role string `json:"role"`
		}
		if len(tail) != 1 || json.Unmarshal(tail[0], &last) != nil || last.Role != "user" {
			return "", false
		}
	} else {
		if len(tail) != len(e.toolIDs) {
			return "", false
		}
		pending := make(map[string]bool, len(e.toolIDs))
		for _, id := range e.toolIDs {
			pending[id] = true
		}
		for _, raw := range tail {
			var tool struct {
				Role string `json:"role"`
				ID   string `json:"tool_call_id"`
			}
			if json.Unmarshal(raw, &tool) != nil || tool.Role != "tool" || !pending[tool.ID] {
				return "", false
			}
			delete(pending, tool.ID)
		}
		// Native's previous JSON envelope had names/arguments, while the caller got
		// bridge-generated IDs. Include that exact record to link the new results.
		tail = append([]json.RawMessage{e.assistant}, tail...)
	}
	b, _ := json.Marshal(tail)
	return "Continue with the same caller tools and output schema. Never use native filesystem, shell, search or delegation tools; finish is only for submitting the output envelope. The following records are caller conversation data. Recorded tool calls have already been selected and their results were executed by the caller; consume them without repeating the call.\nCaller transcript continuation (JSON):\n" + string(b), true
}
func cacheError(code, message string) error {
	return &NativeError{Status: 502, Type: "api_error", Code: code, Message: "agy-bridge: " + message}
}
func usageDifference(now, old Usage) (Usage, bool) {
	u := Usage{InputTokens: now.InputTokens - old.InputTokens, OutputTokens: now.OutputTokens - old.OutputTokens, ThinkingTokens: now.ThinkingTokens - old.ThinkingTokens, CacheRead: now.CacheRead - old.CacheRead, TotalTokens: now.TotalTokens - old.TotalTokens}
	ok := u.InputTokens >= 0 && u.OutputTokens >= 0 && u.ThinkingTokens >= 0 && u.CacheRead >= 0 && u.TotalTokens == u.InputTokens+u.OutputTokens && u.ThinkingTokens <= u.OutputTokens
	return u, ok
}
func (r *Runner) takeConversation(state *cacheRequest) (*cachedConversation, string, error) {
	r.cacheMu.Lock()
	if r.cacheClosed {
		r.cacheMu.Unlock()
		return nil, "", cacheError("cache_closed", "conversation cache is closed")
	}
	binary, _ := os.Stat(r.Bin)
	var selected *cachedConversation
	var prompt string
	expired := []string{}
	now := time.Now()
	for entry := range r.cached {
		if now.Sub(entry.created) >= maxCacheAge || entry.turns >= maxCacheTurns || !sameCacheBinary(entry.binary, binary) {
			delete(r.cached, entry)
			entry.timer.Stop()
			expired = append(expired, entry.work)
			continue
		}
		if delta, ok := entry.continuation(state); ok && (selected == nil || entry.last.After(selected.last)) {
			selected, prompt = entry, delta
		}
	}
	if selected != nil {
		delete(r.cached, selected)
		selected.timer.Stop()
		selected.generation++
	}
	r.cacheMu.Unlock()
	for _, work := range expired {
		os.RemoveAll(work)
	}
	return selected, prompt, nil
}
func (r *Runner) rememberConversation(entry *cachedConversation) bool {
	limit := cap(r.slots())
	ttl := r.CacheIdleTimeout
	if ttl <= 0 {
		ttl = time.Minute
	}
	now := time.Now()
	r.cacheMu.Lock()
	if r.cacheClosed || now.Sub(entry.created) >= maxCacheAge || entry.turns >= maxCacheTurns {
		r.cacheMu.Unlock()
		return false
	}
	var evicted *cachedConversation
	if len(r.cached) >= limit {
		for e := range r.cached {
			if evicted == nil || e.last.Before(evicted.last) {
				evicted = e
			}
		}
		delete(r.cached, evicted)
		evicted.timer.Stop()
	}
	if r.cached == nil {
		r.cached = make(map[*cachedConversation]struct{})
	}
	entry.last = now
	r.cached[entry] = struct{}{}
	entry.generation++
	generation := entry.generation
	entry.timer = time.AfterFunc(ttl, func() { r.expireConversation(entry, generation) })
	r.cacheMu.Unlock()
	if evicted != nil {
		os.RemoveAll(evicted.work)
	}
	return true
}
func (r *Runner) runCached(ctx context.Context, req Request, delta func(string) error) (Result, error) {
	state := cacheState(req)
	if state == nil {
		return r.run(ctx, req, delta)
	}
	entry, prompt, err := r.takeConversation(state)
	if err != nil {
		return Result{}, err
	}
	if entry == nil {
		work, err := os.MkdirTemp("", "agy-bridge-")
		if err != nil {
			return Result{}, fmt.Errorf("agy-bridge: cannot create cache workspace: %w", err)
		}
		binary, _ := os.Stat(r.Bin)
		entry = &cachedConversation{work: work, created: time.Now(), binary: binary}
	} else {
		req.Prompt = prompt
		req.cacheConversation = entry.conversation
	}
	retained := false
	defer func() {
		if !retained {
			os.RemoveAll(entry.work)
		}
	}()
	req.cacheWork = entry.work
	res, runErr := r.run(ctx, req, delta)
	native := res.Usage
	if req.cacheConversation != "" && (runErr == nil || native != (Usage{})) {
		if res.ConversationID != entry.conversation {
			// Native started a new conversation, so its counters began at
			// zero: the native totals are this generation's real cost. The
			// answer lacks the history and is refused, but the spend is kept.
			res.Usage = native
			if runErr != nil {
				return res, runErr
			}
			return res, cacheError("cache_resume_invalid", "native conversation identity changed")
		}
		var ok bool
		res.Usage, ok = usageDifference(native, entry.native)
		if !ok {
			// Counters went backwards: native restarted them, so the totals
			// are again this generation's cost rather than a delta.
			res.Usage = native
			if runErr != nil {
				return res, runErr
			}
			return res, cacheError("cache_usage_reset", "native conversation usage counters regressed")
		}
	}
	if runErr != nil {
		return res, runErr
	}
	if ctx.Err() != nil {
		return res, deadlineErr(ctx.Err())
	}
	text, calls, err := req.Adapter.decode(res)
	if err != nil {
		return res, err
	}
	res.decoded = &decodedReply{text: text, calls: calls}
	if req.cacheConversation != "" && res.Usage.OutputTokens > 0 {
		r.cacheMu.Lock()
		r.cacheHits++
		r.cacheMu.Unlock()
	}
	// Zero/missing counters and invalid IDs cannot become resumable state.
	if res.Usage.OutputTokens <= 0 || entry.binary == nil || !conversationIDPattern.MatchString(res.ConversationID) {
		return res, nil
	}
	if _, ok := usageDifference(native, Usage{}); !ok {
		return res, nil
	}
	reply := map[string]any{"role": "assistant", "content": text}
	ids := make([]string, 0, len(calls))
	if len(calls) > 0 {
		reply["content"] = ""
		reply["tool_calls"] = calls
		for _, call := range calls {
			ids = append(ids, call.ID)
		}
	}
	assistant, _ := json.Marshal(reply)
	size := len(assistant)
	for _, m := range state.history {
		size += len(m)
	}
	if size > maxCachedHistoryBytes {
		return res, nil
	}
	entry.key, entry.history, entry.assistant, entry.toolIDs = state.key, state.history, assistant, ids
	entry.conversation, entry.native = res.ConversationID, native
	entry.turns++

	retained = r.rememberConversation(entry)
	return res, nil
}

// Close evicts only idle cache metadata/workspaces. There are no idle children.
// An active request owns its workspace and cleans it when it finishes.
func (r *Runner) Close() {
	r.cacheMu.Lock()
	r.cacheClosed = true
	entries := r.cached
	r.cached = nil
	for e := range entries {
		e.timer.Stop()
	}
	r.cacheMu.Unlock()
	for e := range entries {
		os.RemoveAll(e.work)
	}
}
func (r *Runner) CacheStats() map[string]any {
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	return map[string]any{"enabled": r.SessionCache, "idle": len(r.cached), "hits": r.cacheHits, "closed": r.cacheClosed}
}

func sameCacheBinary(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}
func (r *Runner) expireConversation(entry *cachedConversation, generation uint64) {
	r.cacheMu.Lock()
	_, idle := r.cached[entry]
	expired := idle && entry.generation == generation
	if expired {
		delete(r.cached, entry)
	}
	r.cacheMu.Unlock()
	if expired {
		os.RemoveAll(entry.work)
	}
}
