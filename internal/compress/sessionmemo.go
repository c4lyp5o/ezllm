package compress

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"
)

// ── committed-prefix stability (WS-C, scoped) ──────────────────────────────
//
// The defect this kills: compression is deterministic per message, but the
// ratio floor judges the WHOLE pipeline. As a conversation grows, the same
// history edit's saved/pre shrinks and eventually fails the floor — so an
// accepted rewrite is REJECTED on a later request and the already-transmitted
// history reverts to its original bytes, breaking the upstream prefix cache
// (accepted once, flipped the next). The exempt-final-message window slides
// the same way: a message shipped verbatim becomes "editable" next request.
//
// The scoped fix: a per-session COMMIT LEDGER of what actually shipped. Each
// editable message is keyed by a hash of its full shape; on every request the
// ledger's form (transformed or original) ships verbatim, and only genuinely
// NEW messages go through the engines + floor. Already-shipped history is
// never re-judged, so the prefix is byte-stable for the life of the session.
//
// Properties (plan WS-C requirements, honestly scoped):
//   - bounded: per-session and global entry caps with LRU/TTL eviction
//   - TTL'd: entries expire after ttl of disuse
//   - process-local: never written to disk or the ledger; lost on restart
//     (re-derivation is deterministic, so the cost is one re-acceptance)
//   - session-scoped: no cross-session state (the key includes the session)
//   - profile-scoped: the key includes the profile name, so an operator's
//     profile CHANGE starts a fresh ledger — an explicit, visible transition,
//     not silent drift
//   - privacy: keys are hashes (no plaintext session ids), values are the
//     shipped message forms which are prompt-derived content — bounded,
//     TTL'd, memory-only, and disableable by leaving the session id absent
//     (legacy stateless behavior).

const (
	// Defaults chosen for chat-sized sessions on a single home box.
	DefaultSessionTTL        = 30 * time.Minute
	DefaultSessionMaxEntries = 512 // per session+profile
	DefaultMemoMaxEntries    = 8192
)

// SessionMemo is the commit ledger. Safe for concurrent use.
type SessionMemo struct {
	mu         sync.Mutex
	ttl        time.Duration
	maxPerKey  int
	maxEntries int
	now        func() time.Time

	// key = sha256(session | profileVersion | messageHash); ships form.
	entries map[string]*memoEntry
	// binding: session -> the profile fingerprint it was FIRST seen with (the
	// immutable version binding, addendum #4). Bounded like entries; a session
	// absent here binds on its next use.
	binding map[string]string
	lru     map[string]int64 // key -> logical clock of last use
	clock   int64
	ops     uint64
}

type memoEntry struct {
	shipped any // the exact message object that went upstream (nil = original)
	usedAt  time.Time
}

// SessionMemoOptions tunes the ledger; zero values take the defaults.
type SessionMemoOptions struct {
	TTL        time.Duration
	MaxPerKey  int
	MaxEntries int
	Now        func() time.Time
}

// NewSessionMemo builds a ledger with defaults or the given overrides.
func NewSessionMemo(opts *SessionMemoOptions) *SessionMemo {
	m := &SessionMemo{
		ttl:        DefaultSessionTTL,
		maxPerKey:  DefaultSessionMaxEntries,
		maxEntries: DefaultMemoMaxEntries,
		now:        time.Now,
		entries:    map[string]*memoEntry{},
		binding:    map[string]string{},
		lru:        map[string]int64{},
	}
	if opts != nil {
		if opts.TTL > 0 {
			m.ttl = opts.TTL
		}
		if opts.MaxPerKey > 0 {
			m.maxPerKey = opts.MaxPerKey
		}
		if opts.MaxEntries > 0 {
			m.maxEntries = opts.MaxEntries
		}
		if opts.Now != nil {
			m.now = opts.Now
		}
	}
	return m
}

// ProfileFingerprint is the immutable VERSION identity of an effective
// profile: everything that changes the output bytes. Editing stages, the
// exemption, the floor, or the auto-trigger yields a NEW fingerprint, so an
// ACTIVE session keeps its binding (and its committed bytes) and only new
// sessions see the change — a profile edit never silently rewrites a live
// session (addendum #4). The profile NAME alone is not a version: renaming is
// an explicit operator act and the fingerprint still moves with the content.
func ProfileFingerprint(p *Profile) string {
	if p == nil {
		return ""
	}
	h := sha256.New()
	fmt.Fprintf(h, "name:%s", p.Name)
	for _, st := range p.Stages {
		fmt.Fprintf(h, "\nstage:%s", st.Engine)
		keys := make([]string, 0, len(st.Options))
		for k := range st.Options {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(h, "\nopt:%s=%v", k, st.Options[k])
		}
	}
	fmt.Fprintf(h, "\nexempt:%v\nfloor:%v\nauto:%v\nfailopen:%v\nenabled:%v",
		p.ExemptLastTurn, p.MinCompressRatio, p.AutoTriggerTokens, p.FailOpen, p.Enabled)
	if p.FreshToolOutput {
		// Only when on, so every profile fingerprinted before the option
		// existed keeps its version (live sessions are not re-versioned).
		fmt.Fprintf(h, "\nfresh:compress")
	}
	return "v" + hex.EncodeToString(h.Sum(nil)[:8])
}

// Bind returns the profile fingerprint a session is BOUND to. The first use
// of a session binds the CURRENT fingerprint; later calls return the binding
// even after the effective profile changes — an active session is never
// silently re-versioned. An empty session binds nothing (legacy path).
func (m *SessionMemo) Bind(session, currentFingerprint string) string {
	if m == nil || session == "" || currentFingerprint == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.binding[session]; ok {
		return v
	}
	m.binding[session] = currentFingerprint
	m.ops++
	if m.ops%1024 == 0 {
		m.evictBindingsLocked()
	}
	return currentFingerprint
}

// BoundFingerprint reports a session's binding without creating one.
func (m *SessionMemo) BoundFingerprint(session string) (string, bool) {
	if m == nil {
		return "", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.binding[session]
	return v, ok
}

// evictBindingsLocked keeps the session->version map bounded: dropping a
// binding simply makes that session re-bind (adopting the CURRENT profile
// version) on its next use — same treatment as a restart.
func (m *SessionMemo) evictBindingsLocked() {
	if len(m.binding) <= m.maxEntries {
		return
	}
	drop := len(m.binding) - m.maxEntries/2
	for k := range m.binding {
		delete(m.binding, k)
		if drop--; drop <= 0 {
			break
		}
	}
}

// memoKey derives the ledger key: sha256(session | profile | messageHash).
// The session id never appears in the clear inside the map.
func memoKey(session, profile string, msgHash string, ord int) string {
	h := sha256.New()
	h.Write([]byte(session))
	h.Write([]byte{0})
	h.Write([]byte(profile))
	h.Write([]byte{0})
	h.Write([]byte(msgHash))
	if ord > 0 { // occurrence 0 keys exactly as before ordinals existed
		h.Write([]byte{0})
		h.Write([]byte(strconv.Itoa(ord)))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// messageHash is the content-address of a message's full shape: any change
// (role, content, tool ids, position-independent fields) is a new message.
// A nil/undecodable message hashes as absent and is simply never memoized.
func messageHash(m any) string {
	sum := sha256.Sum256(mustMarshal(m))
	return hex.EncodeToString(sum[:])
}

// Get returns the committed form for (session, profile, message) if present
// and unexpired. ok=false means "no commitment — treat as new work".
func (m *SessionMemo) Get(session, profile string, msg any) (shipped any, ok bool) {
	return m.GetAt(session, profile, msg, 0)
}

// memoDropped is the committed form of a message that shipped as NOTHING
// (session_dedup removed a repeat). Replaying it omits the message again.
type memoDropped struct{}

// GetAt is Get for the ord-th identical occurrence of msg in the conversation
// (0 = first). Decisions that depend on position — dedup keeps the first
// copy and drops later ones — need the occurrence in the key, or committing
// "dropped" for a repeat would also drop the original. In an append-only
// conversation an earlier message's occurrence number never changes, so the
// key is as stable as the content.
func (m *SessionMemo) GetAt(session, profile string, msg any, ord int) (shipped any, ok bool) {
	if m == nil || session == "" || profile == "" {
		return nil, false
	}
	h := messageHash(msg)
	if h == "" {
		return nil, false
	}
	key := memoKey(session, profile, h, ord)
	m.mu.Lock()
	defer m.mu.Unlock()
	e, exists := m.entries[key]
	if !exists {
		return nil, false
	}
	if m.now().Sub(e.usedAt) > m.ttl {
		delete(m.entries, key)
		delete(m.lru, key)
		return nil, false
	}
	m.clock++
	m.lru[key] = m.clock
	e.usedAt = m.now()
	return e.shipped, true
}

// Put records the shipped form for (session, profile, message).
func (m *SessionMemo) Put(session, profile string, msg any, shipped any) {
	m.PutAt(session, profile, msg, 0, shipped)
}

// PutAt is Put for the ord-th identical occurrence of msg (see GetAt).
func (m *SessionMemo) PutAt(session, profile string, msg any, ord int, shipped any) {
	if m == nil || session == "" || profile == "" || shipped == nil {
		return
	}
	h := messageHash(msg)
	if h == "" {
		return
	}
	key := memoKey(session, profile, h, ord)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[key] = &memoEntry{shipped: shipped, usedAt: m.now()}
	m.clock++
	m.lru[key] = m.clock
	m.ops++
	if m.ops%1024 == 0 || len(m.entries) > m.maxEntries {
		m.evictLocked()
	}
}

// evictLocked drops expired entries and, if still over the global cap, the
// least-recently-used ones. Caller holds the lock.
func (m *SessionMemo) evictLocked() {
	now := m.now()
	for k, e := range m.entries {
		if now.Sub(e.usedAt) > m.ttl {
			delete(m.entries, k)
			delete(m.lru, k)
		}
	}
	if len(m.entries) <= m.maxEntries {
		return
	}
	type kv struct {
		key string
		clk int64
	}
	all := make([]kv, 0, len(m.lru))
	for k, c := range m.lru {
		all = append(all, kv{k, c})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].clk < all[j].clk })
	for i := 0; i < len(all) && len(m.entries) > m.maxEntries; i++ {
		delete(m.entries, all[i].key)
		delete(m.lru, all[i].key)
	}
}

// Len reports the entry count (tests/ops visibility).
func (m *SessionMemo) Len() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}

// Clear drops everything (tests; an operator-visible reset is a restart).
func (m *SessionMemo) Clear() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = map[string]*memoEntry{}
	m.binding = map[string]string{}
	m.lru = map[string]int64{}
}

// ConversationKey derives the memo session for a request that names none, so
// committed-prefix stability holds for every client — not only those that
// send X-Ezllm-Session. It hashes only what exists from a conversation's FIRST
// request and never changes after it: the client, the leading instructions
// (top-level system / Responses instructions, and any leading system or
// developer messages), and the first message after them. Anything later would
// move the key as the conversation grows and defeat the memo.
//
// Two conversations that open identically share a key, which is safe: memo
// entries are keyed by message content and occurrence, so they share only
// identical answers. A client whose history is rewritten (a compaction, a
// fork) opens a new key, which is exactly a new conversation.
//
// "" when the body has no conversation list (nothing to stabilize).
func ConversationKey(client string, root map[string]any) string {
	list, ok := root["messages"].([]any)
	if !ok {
		switch in := root["input"].(type) {
		case []any:
			list = in
		case string:
			list = []any{in}
		}
	}
	if len(list) == 0 {
		return ""
	}
	h := sha256.New()
	write := func(v any) {
		h.Write(mustMarshal(v))
		h.Write([]byte{0})
	}
	h.Write([]byte(client))
	h.Write([]byte{0})
	write(root["system"])
	write(root["instructions"])
	for _, m := range list {
		write(m)
		if mm, ok := m.(map[string]any); ok {
			if r, _ := mm["role"].(string); r == "system" || r == "developer" {
				continue // still the instruction envelope
			}
		}
		break // the first real message closes the key
	}
	return "auto:" + hex.EncodeToString(h.Sum(nil))[:32]
}
