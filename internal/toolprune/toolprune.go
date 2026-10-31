// Package toolprune implements the tool-pruning measurement (M1,
// docs/tool-pruning-design.md): what tool schemas a client ships, which tools
// its traces actually call, and what the policy WOULD remove — measured, never
// shipped. M2 adds the tools-array rewrite; this package owns extraction,
// eligibility and the per-session pair counter either way.
//
// Extraction is conservative by construction: an over-marked "used" can only
// keep a tool, and every unnameable or unparseable piece is left alone.
package toolprune

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/c4lyp5o/ezllm/internal/compress"
)

// Schema is one shipped tool-schema entry.
type Schema struct {
	// Name is the entry's tool name. '' = the entry has none in any known
	// shape (or is not an object) — never eligible, tracked as nothing.
	Name string
	// Tokens is EstimateTokens over the entry exactly as shipped, so the
	// measurement and the compression floor share one estimator.
	Tokens int64
}

// Observation is what one request body says about the action space and the
// trace so far.
type Observation struct {
	// Shipped is the tools array in wire order (legacy `functions` is
	// deliberately not included — untouched and uncounted).
	Shipped []Schema
	// Used are the tool names the visible trace has called anywhere.
	Used map[string]bool
	// ChoiceName is the tool tool_choice forces ('' = none forced).
	ChoiceName string
	// ChoiceDemands says tool_choice requires SOME tool call without naming
	// one ("required"/"any"): eligibility is disabled for the request, or a
	// request could ship zero tools while a call is demanded.
	ChoiceDemands bool
}

// StatsView is the stored aggregate slice the policy reads
// (store.ToolUsageRow maps onto it server-side).
type StatsView struct {
	ShippedSessions int64
	UsedSessions    int64
}

// Policy is the eligibility rule. Keep is operator knowledge (rtk.go's rule).
type Policy struct {
	MinUnusedSessions int
	Keep              map[string]bool
}

// EligibleName is the aggregate verdict for one tool: never used in any
// session, shipped in at least MinUnusedSessions sessions, not kept.
func (p Policy) EligibleName(name string, st StatsView) bool {
	if name == "" {
		return false
	}
	if p.Keep[name] {
		return false
	}
	if st.UsedSessions > 0 {
		return false // sticky lifetime: used once, never eligible again
	}
	return st.ShippedSessions >= int64(p.MinUnusedSessions)
}

// Eligible returns the schemas the policy would drop, in shipped order. The
// per-request tool_choice guards apply on top of EligibleName.
func (p Policy) Eligible(obs Observation, stats map[string]StatsView) []Schema {
	if obs.ChoiceDemands {
		return nil
	}
	out := make([]Schema, 0, len(obs.Shipped))
	for _, s := range obs.Shipped {
		if obs.ChoiceName != "" && s.Name == obs.ChoiceName {
			continue
		}
		if p.EligibleName(s.Name, stats[s.Name]) {
			out = append(out, s)
		}
	}
	return out
}

// ShadowTokens is the measurement: how many estimated prompt tokens the
// eligible set carries per request.
func ShadowTokens(eligible []Schema) int64 {
	var n int64
	for _, s := range eligible {
		n += s.Tokens
	}
	return n
}

// Observe decodes the tool action space and trace usage from a decoded
// request body. Root must be the request's JSON object; nil is a no-op.
func Observe(root map[string]any) Observation {
	obs := Observation{Used: map[string]bool{}}
	if root == nil {
		return obs
	}
	if tools, ok := root["tools"].([]any); ok {
		for _, t := range tools {
			m, ok := t.(map[string]any)
			if !ok {
				continue // never eligible, never counted
			}
			obs.Shipped = append(obs.Shipped, Schema{
				Name:   toolEntryName(m),
				Tokens: compress.EstimateTokens(string(mustJSON(m))),
			})
		}
	}
	obs.ChoiceName, obs.ChoiceDemands = toolChoiceOf(root)
	collectUsed(root, obs.Used)
	return obs
}

// toolEntryName reads a tool entry's name across wire shapes: chat nests it
// under "function", anthropic and responses carry it top-level. ” = the
// entry is unnameable (never eligible).
func toolEntryName(m map[string]any) string {
	if fn, ok := m["function"].(map[string]any); ok {
		if s, ok := fn["name"].(string); ok && s != "" {
			return s
		}
	}
	if s, ok := m["name"].(string); ok {
		return s
	}
	return ""
}

// toolChoiceOf maps tool_choice onto (forced name, demands a call) across
// shapes: "auto"/"none" are normal policy; "required"/"any" (string or
// typed) demand SOME call; {"type":"function"|"tool", name…} forces a tool.
func toolChoiceOf(root map[string]any) (name string, demands bool) {
	switch tc := root["tool_choice"].(type) {
	case string:
		if tc == "required" || tc == "any" {
			return "", true
		}
	case map[string]any:
		typ, _ := tc["type"].(string)
		switch typ {
		case "required", "any":
			return "", true
		case "function", "tool":
			if fn, ok := tc["function"].(map[string]any); ok {
				if s, ok := fn["name"].(string); ok && s != "" {
					return s, false
				}
			}
			if s, ok := tc["name"].(string); ok && s != "" {
				return s, false
			}
		}
	}
	return "", false
}

// collectUsed walks the visible trace (messages / input) for tool call
// sites and tool results. Call sites record their name directly; results
// record only an id, resolved through the call sites seen anywhere in the
// request. Anything ambiguous marks the tool used — the safe direction.
func collectUsed(root map[string]any, used map[string]bool) {
	byID := map[string]string{} // call id -> tool name
	var resultIDs []string

	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case []any:
			for _, item := range t {
				walk(item)
			}
		case map[string]any:
			typ, _ := t["type"].(string)
			switch typ {
			case "tool_use", "custom_tool_call":
				if s, ok := t["name"].(string); ok && s != "" {
					used[s] = true
				}
				if id, ok := t["id"].(string); ok {
					if s, ok := t["name"].(string); ok {
						byID[id] = s
					}
				}
			case "function_call":
				// Responses wire: {type:"function_call", name, call_id}.
				if s, ok := t["name"].(string); ok && s != "" {
					used[s] = true
				}
				if id, ok := t["call_id"].(string); ok {
					if s, ok := t["name"].(string); ok {
						byID[id] = s
					}
				}
			case "tool_result", "custom_tool_call_output", "function_call_output":
				resultIDs = append(resultIDs, idOf(t)...)
			}
			// Chat wire: assistant messages carry tool_calls / function_call
			// as top-level keys; role:"tool" and role:"function" results
			// carry tool_call_id / name.
			if calls, ok := t["tool_calls"].([]any); ok {
				for _, c := range calls {
					cm, ok := c.(map[string]any)
					if !ok {
						continue
					}
					name := ""
					if fn, ok := cm["function"].(map[string]any); ok {
						name, _ = fn["name"].(string)
					}
					if name == "" {
						name, _ = cm["name"].(string)
					}
					if name != "" {
						used[name] = true
					}
					if id, ok := cm["id"].(string); ok && name != "" {
						byID[id] = name
					}
				}
			}
			if fc, ok := t["function_call"].(map[string]any); ok {
				if s, ok := fc["name"].(string); ok && s != "" {
					used[s] = true
				}
			}
			if role, _ := t["role"].(string); role == "tool" {
				resultIDs = append(resultIDs, idOf(t)...)
			}
			if role, _ := t["role"].(string); role == "function" {
				// Legacy function-result messages name their tool outright.
				if s, ok := t["name"].(string); ok && s != "" {
					used[s] = true
				}
			}
			for _, v := range t {
				walk(v)
			}
		}
	}
	for _, key := range []string{"messages", "input"} {
		if v, ok := root[key]; ok {
			walk(v)
		}
	}
	for _, id := range resultIDs {
		if name, ok := byID[id]; ok {
			used[name] = true
		}
	}
}

// idOf collects every call-id field a result block might carry.
func idOf(m map[string]any) []string {
	var ids []string
	for _, k := range []string{"tool_use_id", "tool_call_id", "call_id", "id"} {
		if s, ok := m[k].(string); ok && s != "" {
			ids = append(ids, s)
		}
	}
	return ids
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// ── per-session pair counting ───────────────────────────────────────────────
//
// ShippedSessions/UsedSessions must count each (session, tool) pair once, so
// the tracker remembers what a session has already counted. Process-local,
// bounded and TTL'd (the sessionmemo precedent): a restart can double-count
// one session per active conversation, which crosses min_unused_sessions at
// most one session early. Accepted and documented — visible in the counts,
// never in request behaviour.

const (
	// sessionTTL mirrors compress.SessionMemo's chat-session window.
	sessionTTL = 30 * time.Minute
	// maxTrackedSessions bounds memory; over the cap, expired sessions go
	// first and the rest are dropped oldest-first.
	maxTrackedSessions = 4096
)

// Delta is one (client, tool) stats increment for a request. The server maps
// it onto store.ToolUsageDelta (this package does not import store).
type Delta struct {
	Client          string
	Tool            string
	SchemaTokens    int64
	ShippedRequests int64
	ShippedSessions int64
	UsedSessions    int64
	LastUsedAt      string // '' = this delta carries no usage stamp
}

type sessionCounts struct {
	seen    map[string]bool // shipped-counted this session
	used    map[string]bool // used-counted this session
	expires time.Time
}

// Tracker counts (session, tool) pairs once per session. Safe for concurrent
// use.
type Tracker struct {
	mu       sync.Mutex
	sessions map[string]*sessionCounts
}

// NewTracker returns an empty tracker.
func NewTracker() *Tracker {
	return &Tracker{sessions: map[string]*sessionCounts{}}
}

// Deltas returns the stats increments this request contributes. With an
// empty session the design (DECISIONS §5) is request counts only: the pair
// counters never move, so a client with no derivable session can never make
// a tool eligible (conservative by construction).
func (t *Tracker) Deltas(client, session string, obs Observation, now time.Time) []Delta {
	if len(obs.Shipped) == 0 && len(obs.Used) == 0 {
		return nil
	}
	out := make([]Delta, 0, len(obs.Shipped)+len(obs.Used))
	add := func(name string, schemaTokens int64, shippedReq, shippedSess, usedSess int64, usedAt string) {
		out = append(out, Delta{
			Client: client, Tool: name, SchemaTokens: schemaTokens,
			ShippedRequests: shippedReq, ShippedSessions: shippedSess,
			UsedSessions: usedSess, LastUsedAt: usedAt,
		})
	}

	var sc *sessionCounts
	if session != "" {
		t.mu.Lock()
		sc = t.sessions[session]
		if sc == nil {
			t.evictLocked(now)
			sc = &sessionCounts{seen: map[string]bool{}, used: map[string]bool{}, expires: now.Add(sessionTTL)}
			t.sessions[session] = sc
		} else {
			sc.expires = now.Add(sessionTTL)
		}
		t.mu.Unlock()
	}

	// One delta per shipped name (duplicate names in one array are
	// pathological; first entry wins).
	written := map[string]bool{}
	for _, s := range obs.Shipped {
		if s.Name == "" || written[s.Name] {
			continue
		}
		written[s.Name] = true
		var shippedSess, usedSess int64
		var usedAt string
		if sc != nil {
			if !sc.seen[s.Name] {
				sc.seen[s.Name] = true
				shippedSess = 1
			}
			if obs.Used[s.Name] && !sc.used[s.Name] {
				sc.used[s.Name] = true
				usedSess = 1
				usedAt = now.UTC().Format("2006-01-02T15:04:05.000Z")
			}
		}
		add(s.Name, s.Tokens, 1, shippedSess, usedSess, usedAt)
	}
	// A tool called this request but no longer in the shipped array still
	// owes its used flip (usage is sticky and must survive a shrinking
	// catalog).
	for name := range obs.Used {
		if written[name] {
			continue
		}
		var usedSess int64
		var usedAt string
		if sc != nil && !sc.used[name] {
			sc.used[name] = true
			usedSess = 1
			usedAt = now.UTC().Format("2006-01-02T15:04:05.000Z")
		}
		add(name, 0, 0, 0, usedSess, usedAt)
	}
	return out
}

// evictLocked drops expired sessions, then the oldest, to hold the cap.
// Callers hold t.mu.
func (t *Tracker) evictLocked(now time.Time) {
	if len(t.sessions) < maxTrackedSessions {
		return
	}
	for k, sc := range t.sessions {
		if now.After(sc.expires) {
			delete(t.sessions, k)
		}
	}
	for k := range t.sessions {
		if len(t.sessions) < maxTrackedSessions {
			return
		}
		delete(t.sessions, k)
	}
}
