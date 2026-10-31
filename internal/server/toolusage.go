package server

import (
	"net/http"
	"sort"
	"time"

	"github.com/c4lyp5o/ezllm/internal/compress"
	"github.com/c4lyp5o/ezllm/internal/jsonbody"
	"github.com/c4lyp5o/ezllm/internal/store"
	"github.com/c4lyp5o/ezllm/internal/toolprune"
)

// Tool pruning M1 (docs/tool-pruning-design.md): measurement only. Shadow
// mode parses the action space, keeps tool_usage stats and books the
// policy-eligible potential on the ledger row; it never rewrites a body.

// toolPruneKeepSet turns the operator's keep list into the policy lookup.
func toolPruneKeepSet(keep []string) map[string]bool {
	m := make(map[string]bool, len(keep))
	for _, name := range keep {
		m[name] = true
	}
	return m
}

// toolPruneShadow returns the request's policy-eligible prunable schema
// tokens and updates the tool-usage stats (mode=shadow only). Every failure
// is a no-measurement, never a request change (DECISIONS §8): the gate is
// the mode check first, so mode=off does no work at all.
func (s *Server) toolPruneShadow(r *http.Request, client string, doc *jsonbody.BodyDoc) int64 {
	if s.toolPruneMode != "shadow" || s.db == nil || doc == nil || doc.Err() != nil || doc.Map() == nil {
		return 0
	}
	obs := toolprune.Observe(doc.Map())
	if len(obs.Shipped) == 0 {
		return 0
	}
	// Session identity mirrors the compression commit ledger: an explicit
	// X-Ezllm-Session wins, otherwise the conversation's own opening.
	session := compress.ClientSession(client, sessionOf(r))
	if session == "" {
		session = compress.ConversationKey(client, doc.Map())
	}
	// Eligibility reads the PRE-request aggregates, then this request's
	// deltas land — a decision must not grade its own homework.
	var shadow int64
	if rows, err := s.db.ToolUsageForClient(r.Context(), client); err == nil {
		stats := make(map[string]toolprune.StatsView, len(rows))
		for _, row := range rows {
			stats[row.Tool] = toolprune.StatsView{ShippedSessions: row.ShippedSessions, UsedSessions: row.UsedSessions}
		}
		shadow = toolprune.ShadowTokens(s.toolPrune.Eligible(obs, stats))
	}
	now := time.Now()
	deltas := s.toolPruneTrk.Deltas(client, session, obs, now)
	rows := make([]store.ToolUsageDelta, 0, len(deltas))
	for _, d := range deltas {
		rows = append(rows, store.ToolUsageDelta{
			Client: d.Client, Tool: d.Tool, SchemaTokens: d.SchemaTokens,
			ShippedRequests: d.ShippedRequests, ShippedSessions: d.ShippedSessions,
			UsedSessions: d.UsedSessions, LastUsedAt: d.LastUsedAt,
		})
	}
	_ = s.db.UpsertToolUsage(r.Context(), rows) // best-effort: fail open
	return shadow
}

// handleToolUsage: GET /admin/tool-usage → every (client, tool) aggregate,
// the effective policy, and the prize rollup that gates M2
// (summary.prize_tokens = Σ schema_tokens × shipped_requests over eligible
// rows — "what would pruning remove from every future request").
func (s *Server) handleToolUsage(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.ListToolUsage(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(rows))
	var prizeTotal int64
	clients := map[string]bool{}
	for _, row := range rows {
		clients[row.Client] = true
		eligible := s.toolPrune.EligibleName(row.Tool, toolprune.StatsView{
			ShippedSessions: row.ShippedSessions, UsedSessions: row.UsedSessions,
		})
		var prize int64
		if eligible {
			prize = row.SchemaTokens * row.ShippedRequests
		}
		prizeTotal += prize
		out = append(out, map[string]any{
			"client": row.Client, "tool": row.Tool,
			"schema_tokens":    row.SchemaTokens,
			"shipped_requests": row.ShippedRequests,
			"shipped_sessions": row.ShippedSessions,
			"used_sessions":    row.UsedSessions,
			"first_seen_at":    row.FirstSeenAt,
			"last_seen_at":     row.LastSeenAt,
			"last_used_at":     row.LastUsedAt,
			"eligible":         eligible,
			"prize_tokens":     prize,
		})
	}
	mode := s.toolPruneMode
	if mode == "" {
		mode = "off"
	}
	keep := make([]string, 0, len(s.toolPrune.Keep))
	for name := range s.toolPrune.Keep {
		keep = append(keep, name)
	}
	sort.Strings(keep)
	writeJSON(w, http.StatusOK, map[string]any{
		"rows": out,
		"policy": map[string]any{
			"mode":                mode,
			"min_unused_sessions": s.toolPrune.MinUnusedSessions,
			"keep":                keep,
		},
		"summary": map[string]any{
			"rows":         len(rows),
			"clients":      len(clients),
			"prize_tokens": prizeTotal,
		},
	})
}
