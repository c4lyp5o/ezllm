package codexbridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// A template contains configuration and one empty user item, never caller
// input or child output. Idle entries hold no process. Credits bound both
// templates and concurrent control-only cleanup, including failed deletions.
const CacheEnvKey = "EZLLM_CODEX_BRIDGE_CACHE"
const templateIdle = time.Minute
const templateAge = 15 * time.Minute

type templateCache struct {
	mu              sync.Mutex
	idle            map[[32]byte][]*templateLease
	count           int
	closed          bool
	hits            uint64
	cleanupFailures uint64
}
type templateLease struct {
	r       *Runner
	key     [32]byte
	root    string
	id      string
	created time.Time
	uses    int
	timer   *time.Timer
	good    atomic.Bool
	once    sync.Once
}

// CacheHits counts successful template borrows, without exposing any identity.
func (r *Runner) CacheHits() uint64 {
	r.cache.mu.Lock()
	defer r.cache.mu.Unlock()
	return r.cache.hits
}

func (r *Runner) templateKey(req Request) ([32]byte, bool) {
	// Canonical JSON preserves tool ordering and number precision, while ignoring
	// whitespace and object-key order. Input is intentionally absent.
	raw, err := json.Marshal(struct {
		Model, Instructions, Effort, Summary string
		Tools                                []ToolSpec
		Schema                               json.RawMessage
		Sandbox                              SandboxMode
		Binary                               string
	}{req.Model, req.Instructions, req.Effort, req.Summary, req.Tools, req.OutputSchema, r.sandboxMode(), binaryIdentity(r.Bin)})
	if err != nil || len(raw) > 1<<20 {
		return [32]byte{}, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var obj any
	if dec.Decode(&obj) != nil {
		return [32]byte{}, false
	}
	raw, err = json.Marshal(obj)
	return sha256.Sum256(raw), err == nil
}
func binaryIdentity(bin string) string {
	resolved, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return bin
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return resolved
	}
	return fmt.Sprintf("%s:%d:%d", resolved, fi.Size(), fi.ModTime().UnixNano())
}

func (r *Runner) borrowTemplate(req Request) *templateLease {
	if !r.ForkCache || r.Channel == ChannelExec {
		return nil
	}
	key, ok := r.templateKey(req)
	if !ok {
		return nil
	}
	cache := &r.cache
	cache.mu.Lock()
	if cache.closed {
		cache.mu.Unlock()
		return nil
	}
	entries := cache.idle[key]
	if len(entries) > 0 {
		lease := entries[len(entries)-1]
		cache.idle[key] = entries[:len(entries)-1]
		if len(cache.idle[key]) == 0 {
			delete(cache.idle, key)
		}
		lease.timer.Stop()
		cache.hits++
		// Each checkout gets a separate close guard; delayed old expiry callbacks
		// compare the lease pointer and cannot remove a newer checkout.
		next := &templateLease{r: r, key: key, root: lease.root, id: lease.id, created: lease.created, uses: lease.uses + 1}
		cache.mu.Unlock()
		return next
	}
	limit := cap(r.slots()) + r.maxParked()
	if cache.count >= limit {
		cache.mu.Unlock()
		return nil
	}
	cache.count++
	cache.mu.Unlock()
	root, err := os.MkdirTemp("", "codex-bridge-template-")
	if err != nil {
		cache.mu.Lock()
		cache.count--
		cache.mu.Unlock()
		return nil
	}
	return &templateLease{r: r, key: key, root: root, created: time.Now(), uses: 1}
}

// release runs only after the process has been reaped. RemoveAll never follows
// symlinks; recreating the three private subdirectories prevents scratch state
// from crossing requests while retaining the same paths for cache affinity.
func (l *templateLease) release() {
	l.once.Do(func() {
		r := l.r
		cache := &r.cache
		scrubOK := true
		entries, err := os.ReadDir(l.root)
		if err != nil {
			scrubOK = false
		}
		for _, entry := range entries {
			if os.RemoveAll(filepath.Join(l.root, entry.Name())) != nil {
				scrubOK = false
			}
		}

		cache.mu.Lock()
		idleCount := 0
		for _, entries := range cache.idle {
			idleCount += len(entries)
		}
		if idleCount < cap(r.slots()) && l.good.Load() && scrubOK && l.id != "" && !cache.closed && time.Since(l.created) < templateAge && l.uses < 100 {
			if cache.idle == nil {
				cache.idle = make(map[[32]byte][]*templateLease)
			}
			cache.idle[l.key] = append(cache.idle[l.key], l)
			l.timer = time.AfterFunc(templateIdle, func() { r.expireTemplate(l) })
			cache.mu.Unlock()
			return
		}
		cache.mu.Unlock()
		r.deleteTemplate(l)
	})
}
func (r *Runner) expireTemplate(l *templateLease) {
	cache := &r.cache
	cache.mu.Lock()
	entries := cache.idle[l.key]
	found := false
	for i, e := range entries {
		if e == l {
			entries = append(entries[:i], entries[i+1:]...)
			found = true
			break
		}
	}
	if len(entries) == 0 {
		delete(cache.idle, l.key)
	} else {
		cache.idle[l.key] = entries
	}
	cache.mu.Unlock()
	if found {
		r.deleteTemplate(l)
	}
}
func (r *Runner) shutdownTemplates() {
	cache := &r.cache
	cache.mu.Lock()
	cache.closed = true
	var entries []*templateLease
	for _, list := range cache.idle {
		entries = append(entries, list...)
	}
	cache.idle = nil
	for _, l := range entries {
		l.timer.Stop()
	}
	cache.mu.Unlock()
	for _, l := range entries {
		r.deleteTemplate(l)
	}
}

// Deletion is a bounded control-only CLI invocation, never a model request.
// On failure the credit remains occupied, preventing unbounded orphan growth.
func (r *Runner) deleteTemplate(l *templateLease) {
	go func() {
		ok := l.id == ""
		defer func() {
			if os.RemoveAll(l.root) != nil {
				ok = false
			}
			r.templateDeleted(ok)
		}()
		if !ok {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			select {
			case r.slots() <- struct{}{}:
				defer func() { <-r.slots() }()
			case <-ctx.Done():
				return
			}
			c, err := r.spawn(ctx, spawnOpts{experimental: true}, nil, declineRequests)
			if err == nil {
				// A fresh process has no subscription to this persisted parent.
				_, err = c.conn.call(ctx, "thread/delete", map[string]any{"threadId": l.id})
				c.close()
			}
			ok = err == nil
		}
	}()
}
func (r *Runner) templateDeleted(ok bool) {
	r.cache.mu.Lock()
	defer r.cache.mu.Unlock()
	if ok {
		r.cache.count--
	} else {
		r.cache.cleanupFailures++
	}
}

func (r *Runner) forkTemplate(ctx context.Context, c *child, l *templateLease, params map[string]any) (json.RawMessage, error) {
	if l.id == "" {
		params["ephemeral"] = false
		raw, err := c.conn.call(ctx, "thread/start", params)
		if err != nil {
			return nil, err
		}
		var root struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		if json.Unmarshal(raw, &root) != nil || root.Thread.ID == "" {
			return nil, fmt.Errorf("missing template thread id")
		}
		l.id = root.Thread.ID
		_, err = c.conn.call(ctx, "thread/inject_items", map[string]any{"threadId": l.id, "items": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": ""}}}}})
		if err != nil {
			return nil, err
		}
	} else {
		// Tools are inherited from the saved parent. Resending dynamicTools is not
		// supported on resume. No caller history is ever supplied to the parent.
		resume := map[string]any{"threadId": l.id, "cwd": c.cwd, "model": params["model"], "sandbox": params["sandbox"], "approvalPolicy": params["approvalPolicy"], "baseInstructions": params["baseInstructions"]}
		if _, err := c.conn.call(ctx, "thread/resume", resume); err != nil {
			return nil, err
		}
	}
	return c.conn.call(ctx, "thread/fork", map[string]any{"threadId": l.id, "ephemeral": true, "experimentalPredictionMode": true, "excludeTurns": true})
}

// ForkCacheEnabled keeps the experimental protocol opt-in explicit.
func ForkCacheEnabled(getenv func(string) string) bool {
	if getenv == nil {
		getenv = os.Getenv
	}
	return strings.EqualFold(strings.TrimSpace(getenv(CacheEnvKey)), "on")
}

// RunCachePreflight qualifies only cache-enabled installations. Older CLI
// schemas remain sufficient when this mode is off.
func RunCachePreflight(ctx context.Context, bin, home string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "codex-cache-preflight-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	schema := filepath.Join(dir, "schema")
	cmd := exec.CommandContext(ctx, bin, "app-server", "generate-json-schema", "--experimental", "--out", schema)
	cmd.Dir = dir
	cmd.Env = childEnv(dir, home)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("codex cache: experimental schema unavailable: %w (%s)", err, firstLine(string(out)))
	}
	required := map[string][]string{
		"ClientRequest.json":        {"thread/fork", "thread/resume", "thread/inject_items", "thread/delete"},
		"v2/ThreadForkParams.json":  {"experimentalPredictionMode", "ephemeral", "excludeTurns"},
		"v2/ThreadStartParams.json": {"dynamicTools", "ephemeral"},
	}
	for path, names := range required {
		raw, err := os.ReadFile(filepath.Join(schema, path))
		if err != nil {
			return fmt.Errorf("codex cache: missing %s: %w", path, err)
		}
		if missing := missingNames(string(raw), names); len(missing) > 0 {
			return fmt.Errorf("codex cache: missing %s in %s", strings.Join(missing, ", "), path)
		}
	}
	return nil
}

// waitTemplateCleanup gives graceful shutdown a bounded control-only drain.
func (r *Runner) waitTemplateCleanup() {
	deadline := time.NewTimer(21 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		r.cache.mu.Lock()
		done := r.cache.count == int(r.cache.cleanupFailures)
		r.cache.mu.Unlock()
		if done {
			return
		}
		select {
		case <-deadline.C:
			return
		case <-tick.C:
		}
	}
}

func validTemplateUsage(u Usage) bool {
	return u.InputTokens > 0 && u.CachedTokens >= 0 && u.CachedTokens <= u.InputTokens && u.OutputTokens >= 0 && u.ReasoningTokens >= 0 && u.ReasoningTokens <= u.OutputTokens
}

// CacheStats contains counts only, for the loopback /status endpoint.
type CacheStats struct {
	Enabled         bool   `json:"enabled"`
	Hits            uint64 `json:"hits"`
	Idle            int    `json:"idle"`
	Credits         int    `json:"credits"`
	CleanupFailures uint64 `json:"cleanup_failures"`
}

func (r *Runner) CacheStats() CacheStats {
	r.cache.mu.Lock()
	defer r.cache.mu.Unlock()
	idle := 0
	for _, entries := range r.cache.idle {
		idle += len(entries)
	}
	return CacheStats{r.ForkCache, r.cache.hits, idle, r.cache.count, r.cache.cleanupFailures}
}
