package store

import (
	"context"
	"fmt"
	"time"
)

// Tool-usage learning stats (docs/tool-pruning-design.md, schema v33).
// Aggregates only — never prompt content: WHAT tools a client ships and
// calls, never what the prompts say.

// ToolUsageDelta is one (client, tool) stats increment for a request.
// ShippedRequests is per request; ShippedSessions/UsedSessions move at most
// 1 per delta (the caller counts each (session, tool) pair once), and
// UsedSessions is sticky lifetime: a tool used in any session is never
// policy-eligible again.
type ToolUsageDelta struct {
	Client          string
	Tool            string
	SchemaTokens    int64
	ShippedRequests int64
	ShippedSessions int64
	UsedSessions    int64
	// LastUsedAt stamps a usage observation ('' = this delta carries none;
	// the COALESCE keeps the older stamp).
	LastUsedAt string
}

// ToolUsageRow is one (client, tool) aggregate as stored. Policy verdicts
// (eligible / prize) are computed server-side — they need the operator's
// keep list and thresholds, which are config, not ledger.
type ToolUsageRow struct {
	Client          string `json:"client"`
	Tool            string `json:"tool"`
	SchemaTokens    int64  `json:"schema_tokens"`
	ShippedRequests int64  `json:"shipped_requests"`
	ShippedSessions int64  `json:"shipped_sessions"`
	UsedSessions    int64  `json:"used_sessions"`
	FirstSeenAt     string `json:"first_seen_at"`
	LastSeenAt      string `json:"last_seen_at"`
	LastUsedAt      string `json:"last_used_at"`
}

// UpsertToolUsage applies stats deltas in one transaction, executing the
// prepared upsert once per tool (the budget-reservation precedent for a
// synchronous pre-dispatch write). The
// conflict target is the (client, tool) primary key: counts accumulate,
// schema_tokens is last-seen, first_seen_at never moves, and last_used_at
// only moves on a delta that carries a usage stamp.
func (d *DB) UpsertToolUsage(ctx context.Context, deltas []ToolUsageDelta) error {
	if len(deltas) == 0 {
		return nil
	}
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO tool_usage (client, tool, schema_tokens, shipped_requests, shipped_sessions, used_sessions, first_seen_at, last_seen_at, last_used_at)
VALUES (?,?,?,?,?,?,?,?,?)
ON CONFLICT(client, tool) DO UPDATE SET
  schema_tokens = CASE WHEN excluded.schema_tokens > 0 THEN excluded.schema_tokens ELSE tool_usage.schema_tokens END,
  shipped_requests = tool_usage.shipped_requests + excluded.shipped_requests,
  shipped_sessions = tool_usage.shipped_sessions + excluded.shipped_sessions,
  used_sessions = tool_usage.used_sessions + excluded.used_sessions,
  last_seen_at = excluded.last_seen_at,
  last_used_at = COALESCE(excluded.last_used_at, tool_usage.last_used_at)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	for _, dl := range deltas {
		var lastUsed any
		if dl.LastUsedAt != "" {
			lastUsed = dl.LastUsedAt
		}
		if _, err := stmt.ExecContext(ctx,
			dl.Client, dl.Tool, dl.SchemaTokens,
			dl.ShippedRequests, dl.ShippedSessions, dl.UsedSessions,
			now, now, lastUsed); err != nil {
			return fmt.Errorf("upsert tool usage: %w", err)
		}
	}
	return tx.Commit()
}

// ListToolUsage returns every (client, tool) aggregate, ordered for stable
// paging (client then tool).
func (d *DB) ListToolUsage(ctx context.Context) ([]ToolUsageRow, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT client, tool, schema_tokens, shipped_requests, shipped_sessions, used_sessions,
       COALESCE(first_seen_at,''), COALESCE(last_seen_at,''), COALESCE(last_used_at,'')
FROM tool_usage ORDER BY client, tool`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ToolUsageRow{}
	for rows.Next() {
		var r ToolUsageRow
		if err := rows.Scan(&r.Client, &r.Tool, &r.SchemaTokens, &r.ShippedRequests,
			&r.ShippedSessions, &r.UsedSessions, &r.FirstSeenAt, &r.LastSeenAt, &r.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ToolUsageForClient is the request-path read: one client's aggregates, keyed
// by tool for the eligibility verdict. A small indexed read per request in
// shadow mode (the budget-reservation precedent), nothing at all in off mode.
func (d *DB) ToolUsageForClient(ctx context.Context, client string) (map[string]ToolUsageRow, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT client, tool, schema_tokens, shipped_requests, shipped_sessions, used_sessions,
       COALESCE(first_seen_at,''), COALESCE(last_seen_at,''), COALESCE(last_used_at,'')
FROM tool_usage WHERE client = ?`, client)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ToolUsageRow{}
	for rows.Next() {
		var r ToolUsageRow
		if err := rows.Scan(&r.Client, &r.Tool, &r.SchemaTokens, &r.ShippedRequests,
			&r.ShippedSessions, &r.UsedSessions, &r.FirstSeenAt, &r.LastSeenAt, &r.LastUsedAt); err != nil {
			return nil, err
		}
		out[r.Tool] = r
	}
	return out, rows.Err()
}
