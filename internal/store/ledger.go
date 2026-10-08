package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"
)

// Surface identifies which wire protocol served a call.
type Surface string

const (
	SurfaceOpenAI    Surface = "openai"    // /v1/chat/completions
	SurfaceAnthropic Surface = "anthropic" // /v1/messages
	SurfaceResponses Surface = "responses" // /v1/responses
)

// Call is one ledger row. Token counters are NORMALIZED (surface-independent):
//
//	tokens_in           cold (non-cached) input tokens
//	tokens_out          output/completion tokens
//	tokens_cached_read  input served from cache
//	tokens_cached_write input written to cache
//
// This matters because the three surfaces disagree: OpenAI includes cached
// tokens inside prompt_tokens, Anthropic EXCLUDES cache_read from input_tokens,
// and Responses reports its own details object. Use NormalizeUsage rather than
// copying fields by hand.
type Call struct {
	TS time.Time
	// AccountID is the M5 metering key. The denormalized Account NAME below
	// survives key deletion (and can be re-pointed at a different account), so
	// a cap keyed by name would merge two accounts' budgets — meter by ID.
	AccountID int64 // 0 when unknown (pre-M5 rows, or unresolved identity)
	Client    string
	Surface   Surface
	Alias     string // combo name, or "namespace/model" for a direct route
	Account   string // denormalized: survives key/account deletion
	KeyID     int64  // 0 when unknown
	KeyHint   string // denormalized
	Model     string // model actually sent upstream
	Status    int
	Stream    bool
	TTFTms    *int64
	Totalms   *int64

	TokensIn          int64
	TokensOut         int64
	TokensCachedRead  int64
	TokensCachedWrite int64
	ReasoningTokens   int64
	RawUsage          string // verbatim usage JSON (audit / re-derive)

	EndpointID    string // x-opencode-endpoint-id
	UpstreamModel string // x-opencode-upstream-model-id

	CompressionProfile string
	CompressionApplied bool
	// CompressionRulesFired is caveman attribution: the number of prose rules
	// that rewrote something. Always 0 for session_dedup/rtk/headroom/lite.
	CompressionRulesFired int
	ContextTokensPre      int64
	ContextTokensSaved    int64
	PromptTokensPre       int64
	TokensSaved           int64
	CompressionMs         *int64

	Err string
}

// Usage holds the normalized counters plus the verbatim JSON.
type Usage struct {
	In, Out, CachedRead, CachedWrite, Reasoning int64
	Raw                                         map[string]any
}

// NormalizeUsage maps a provider's raw usage object onto our surface-independent
// counters. Verified against live probes 2026-10-05:
//
//	openai     prompt_tokens INCLUDES cached_tokens  -> in = prompt - cached
//	anthropic  input_tokens EXCLUDES cache_read      -> in = input_tokens
//	responses  input_tokens INCLUDES cached_tokens   -> in = input - cached
//
// Unknown shapes fall back to the most common OpenAI-ish field names and leave
// cache counters at zero rather than guessing wrong.
func NormalizeUsage(surface Surface, raw map[string]any) Usage {
	u := Usage{Raw: raw}
	if raw == nil {
		return u
	}
	num := func(m map[string]any, k string) int64 {
		if v, ok := m[k]; ok {
			return toInt64(v)
		}
		return 0
	}
	sub := func(m map[string]any, k string) map[string]any {
		if v, ok := m[k].(map[string]any); ok {
			return v
		}
		return nil
	}

	switch surface {
	case SurfaceAnthropic:
		// input_tokens already excludes cache_read / cache_creation.
		u.In = num(raw, "input_tokens")
		u.Out = num(raw, "output_tokens")
		u.CachedRead = num(raw, "cache_read_input_tokens")
		u.CachedWrite = num(raw, "cache_creation_input_tokens")
		// Anthropic has no reasoning field in usage; server_tool_use ignored.

	case SurfaceResponses:
		in := num(raw, "input_tokens")
		det := sub(raw, "input_tokens_details")
		if det != nil {
			u.CachedRead = num(det, "cached_tokens")
			u.CachedWrite = num(det, "cache_write_tokens")
		}
		u.In = in - u.CachedRead // cached is INCLUDED in input_tokens
		if u.In < 0 {
			u.In = 0
		}
		u.Out = num(raw, "output_tokens")
		if odet := sub(raw, "output_tokens_details"); odet != nil {
			u.Reasoning = num(odet, "reasoning_tokens")
		}

	case SurfaceOpenAI:
		fallthrough
	default:
		prompt := num(raw, "prompt_tokens")
		if prompt == 0 {
			// some gateways use input_tokens even on the OpenAI surface
			prompt = num(raw, "input_tokens")
		}
		if det := sub(raw, "prompt_tokens_details"); det != nil {
			u.CachedRead = num(det, "cached_tokens")
		}
		u.In = prompt - u.CachedRead // cached is INCLUDED in prompt_tokens
		if u.In < 0 {
			u.In = 0
		}
		u.Out = num(raw, "completion_tokens")
		if u.Out == 0 {
			u.Out = num(raw, "output_tokens")
		}
		if odet := sub(raw, "completion_tokens_details"); odet != nil {
			u.Reasoning = num(odet, "reasoning_tokens")
		}
	}
	return u
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case float32:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	case int32:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		return 0
	}
}

// RawJSON renders a usage map as compact JSON for the audit column.
func RawJSON(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

// ledgerItem is one queue entry: either a Call, or a flush barrier carrying an
// ack channel. Routing flushes THROUGH callCh (rather than a second channel)
// makes ordering a channel guarantee: a flush can only be processed after every
// row enqueued before it, so Flush() returning means those rows are persisted.
type ledgerItem struct {
	call *Call
	ack  chan error // non-nil => flush barrier
}

// RecordCall enqueues a ledger row. It NEVER blocks the caller: if the queue is
// full the row is dropped and counted, because a slow database must not add
// latency to a token stream (invariant #5).
func (d *DB) RecordCall(c Call) {
	if c.TS.IsZero() {
		c.TS = time.Now().UTC()
	}
	cp := c
	select {
	case d.callCh <- ledgerItem{call: &cp}:
	default:
		droppedCalls.Add(1)
	}
}

// droppedCalls counts ledger rows shed under backpressure (via /admin/health).
var droppedCalls atomic.Int64

// DroppedCalls reports how many ledger rows were shed (should stay 0).
func DroppedCalls() int64 { return droppedCalls.Load() }

// ledgerLoop batches inserts: flush every BatchSize rows or BatchWait, whichever
// comes first, and on shutdown or an explicit barrier.
func (d *DB) ledgerLoop(batchSize int, batchWait time.Duration) {
	defer d.wg.Done()
	buf := make([]Call, 0, batchSize)
	ticker := time.NewTicker(batchWait)
	defer ticker.Stop()

	flush := func() {
		if len(buf) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := d.insertCalls(ctx, buf); err != nil {
			slog.Error("ledger flush failed", "rows", len(buf), "err", err)
		} else {
			// Copy under the loop's lock (buf is reused after this tick), then
			// hand the snapshot to live consumers.
			d.publish(slices.Clone(buf))
		}
		buf = buf[:0]
	}

	for {
		select {
		case item := <-d.callCh:
			if item.ack != nil {
				// Flush barrier: everything queued before it is in buf by
				// construction (single consumer, FIFO channel).
				flush()
				item.ack <- nil
				continue
			}
			if item.call != nil {
				buf = append(buf, *item.call)
				if len(buf) >= batchSize {
					flush()
				}
			}
		case <-ticker.C:
			flush()
		case <-d.closeCh:
			// Drain anything already queued, then final flush.
		drainLoop:
			for {
				select {
				case item := <-d.callCh:
					if item.call != nil {
						buf = append(buf, *item.call)
					}
					if item.ack != nil {
						flush()
						item.ack <- nil
					}
				default:
					break drainLoop
				}
			}
			flush()
			return
		}
	}
}

// Flush forces pending ledger rows to disk (used by tests and /admin/health).
//
// Deterministic by construction: the barrier travels through the SAME FIFO
// channel as the rows, so when Flush returns, every row enqueued before it is
// persisted. (An earlier version used a second channel, which let select pick
// the flush while rows were still queued — a real flake.)
func (d *DB) Flush() error {
	ack := make(chan error, 1)
	select {
	case d.callCh <- ledgerItem{ack: ack}:
	case <-time.After(10 * time.Second):
		return fmt.Errorf("store: flush enqueue timed out")
	}
	select {
	case err := <-ack:
		return err
	case <-time.After(15 * time.Second):
		return fmt.Errorf("store: flush timed out")
	}
}

func (d *DB) insertCalls(ctx context.Context, rows []Call) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO calls (
  ts, account_id, client, surface, alias, account, provider_key_id, key_hint, model,
  status, stream, ttft_ms, total_ms,
  tokens_in, tokens_out, tokens_cached_read, tokens_cached_write, reasoning_tokens, raw_usage,
  endpoint_id, upstream_model,
  compression_profile, compression_applied, prompt_tokens_pre, tokens_saved, compression_ms,
  compression_rules_fired, context_tokens_pre, context_tokens_saved, err
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, c := range rows {
		var keyID any
		if c.KeyID != 0 {
			keyID = c.KeyID
		}
		var acctID any
		if c.AccountID != 0 {
			acctID = c.AccountID
		}
		_, err := stmt.ExecContext(ctx,
			c.TS.UTC().Format("2006-01-02T15:04:05.000Z"),
			acctID,
			c.Client, string(c.Surface), c.Alias, c.Account, keyID, c.KeyHint, c.Model,
			c.Status, boolInt(c.Stream), c.TTFTms, c.Totalms,
			c.TokensIn, c.TokensOut, c.TokensCachedRead, c.TokensCachedWrite, c.ReasoningTokens, c.RawUsage,
			c.EndpointID, c.UpstreamModel,
			c.CompressionProfile, boolInt(c.CompressionApplied), c.PromptTokensPre, c.TokensSaved, c.CompressionMs,
			c.CompressionRulesFired, c.ContextTokensPre, c.ContextTokensSaved, c.Err,
		)
		if err != nil {
			return fmt.Errorf("insert call: %w", err)
		}
	}

	// M5: advance the usage meter in the SAME transaction as the calls rows so
	// the meter and the ledger can never disagree about what counted. Metering
	// is best-effort here: a failed bump must not fail the insert (losing a
	// dashboard row is worse than a briefly stale counter, and counters are
	// rebuildable from `calls` if they ever drift).
	if err := d.meterInTx(ctx, tx, rows); err != nil {
		slog.Warn("usage meter bump failed", "err", err)
	}
	return tx.Commit()
}

// meterInTx resolves each model's meter window (from its rule) then folds the
// batch into per-bucket increments. A model with no rule is not metered —
// nothing caps it, so a bucket for it would be a wasted write on every call.
func (d *DB) meterInTx(ctx context.Context, tx *sql.Tx, rows []Call) error {
	if len(rows) == 0 {
		return nil
	}
	windows, err := d.ruleWindowsFor(ctx, rows)
	if err != nil {
		return err
	}
	return bumpUsageCounters(ctx, tx, groupForCounter(rows, windows))
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// UsageRow is one aggregate group returned by UsageReport.
// JSON names are the CONTRACT (docs/API.md §overview) and must match what the
// dashboard reads — a tag rename silently blanks the stat cards, because the
// frontend's types are hand-written from that doc. TestUsageRowJSONKeys pins it.
type UsageRow struct {
	Key          string `json:"k"`
	Calls        int64  `json:"calls"`
	TokensIn     int64  `json:"tin"`
	TokensOut    int64  `json:"tout"`
	CachedRead   int64  `json:"cread"`
	CachedWrite  int64  `json:"cwrite"`
	Reasoning    int64  `json:"reasoning"`
	TokensSaved  int64  `json:"saved"`
	ContextPre   int64  `json:"context_pre"`
	ContextSaved int64  `json:"context_saved"`
	Errors       int64  `json:"errors"`
	P50TTFTms    int64  `json:"p50_ttft_ms"`
}

// UsageReport aggregates the ledger. group_by: account|key|alias|client|model|surface|day.
// Filtering is by [from,to) on ts. This is the dashboard's only data source —
// no client-side aggregation of raw rows.
func (d *DB) UsageReport(ctx context.Context, from, to time.Time, groupBy string) ([]UsageRow, error) {
	col, err := usageGroupColumn(groupBy)
	if err != nil {
		return nil, err
	}
	q := fmt.Sprintf(`
SELECT %s AS k,
       COUNT(*)                                   AS calls,
       COALESCE(SUM(tokens_in),0)                 AS tin,
       COALESCE(SUM(tokens_out),0)                AS tout,
       COALESCE(SUM(tokens_cached_read),0)        AS cread,
       COALESCE(SUM(tokens_cached_write),0)       AS cwrite,
       COALESCE(SUM(reasoning_tokens),0)          AS reasoning,
       COALESCE(SUM(tokens_saved),0)              AS saved,
       COALESCE(SUM(context_tokens_pre),0)        AS context_pre,
       COALESCE(SUM(context_tokens_saved),0)      AS context_saved,
       COALESCE(SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END),0) AS errors
FROM calls
WHERE ts >= ? AND ts < ?
GROUP BY k
ORDER BY calls DESC`, col)

	rows, err := d.r.QueryContext(ctx, q, from.UTC().Format("2006-01-02T15:04:05.000Z"), to.UTC().Format("2006-01-02T15:04:05.000Z"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []UsageRow{}
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.Key, &r.Calls, &r.TokensIn, &r.TokensOut, &r.CachedRead, &r.CachedWrite, &r.Reasoning, &r.TokensSaved, &r.ContextPre, &r.ContextSaved, &r.Errors); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// p50 TTFT per group: separate query (SQLite has no percentile_cont).
	if err := d.fillP50(ctx, col, from, to, out); err != nil {
		return nil, err
	}
	return out, nil
}

func usageGroupColumn(groupBy string) (string, error) {
	switch groupBy {
	case "", "account":
		return "account", nil
	case "key":
		return "COALESCE(key_hint,'(none)')", nil
	case "alias":
		return "COALESCE(NULLIF(alias,''),'(direct)')", nil
	case "client":
		return "COALESCE(NULLIF(client,''),'(none)')", nil
	case "model":
		return "model", nil
	case "surface":
		return "surface", nil
	case "day":
		return "substr(ts,1,10)", nil
	default:
		return "", fmt.Errorf("store: unknown group_by %q", groupBy)
	}
}

// fillP50 computes median TTFT per group in Go (SQLite lacks percentile_cont).
func (d *DB) fillP50(ctx context.Context, col string, from, to time.Time, rows []UsageRow) error {
	if len(rows) == 0 {
		return nil
	}
	q := fmt.Sprintf(`SELECT %s AS k, ttft_ms FROM calls
WHERE ts >= ? AND ts < ? AND ttft_ms IS NOT NULL ORDER BY k, ttft_ms`, col)
	rs, err := d.r.QueryContext(ctx, q, from.UTC().Format("2006-01-02T15:04:05.000Z"), to.UTC().Format("2006-01-02T15:04:05.000Z"))
	if err != nil {
		return err
	}
	defer rs.Close()
	byKey := make(map[string][]int64)
	for rs.Next() {
		var k string
		var v int64
		if err := rs.Scan(&k, &v); err != nil {
			return err
		}
		byKey[k] = append(byKey[k], v)
	}
	if err := rs.Err(); err != nil {
		return err
	}
	for i := range rows {
		if vs := byKey[rows[i].Key]; len(vs) > 0 {
			rows[i].P50TTFTms = vs[len(vs)/2] // already ORDER BY ttft_ms
		}
	}
	return nil
}

// RecentCalls returns the newest N ledger rows (drilldown / debugging).
func (d *DB) RecentCalls(ctx context.Context, limit int) ([]Call, error) {
	if limit <= 0 || limit > 1000 {
		limit = 50
	}
	rows, err := d.r.QueryContext(ctx, `
SELECT ts, client, surface, alias, account, COALESCE(provider_key_id,0), key_hint, model,
       status, stream, ttft_ms, total_ms,
       tokens_in, tokens_out, tokens_cached_read, tokens_cached_write, reasoning_tokens,
       COALESCE(raw_usage,''), COALESCE(endpoint_id,''), COALESCE(upstream_model,''),
       compression_profile, compression_applied, prompt_tokens_pre, tokens_saved, compression_ms,
       compression_rules_fired, context_tokens_pre, context_tokens_saved, COALESCE(err,'')
FROM calls ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Call{}
	for rows.Next() {
		var c Call
		var ts string
		var ttft, total, cms sql.NullInt64
		var applied int
		if err := rows.Scan(&ts, &c.Client, &c.Surface, &c.Alias, &c.Account, &c.KeyID, &c.KeyHint, &c.Model,
			&c.Status, &c.Stream, &ttft, &total,
			&c.TokensIn, &c.TokensOut, &c.TokensCachedRead, &c.TokensCachedWrite, &c.ReasoningTokens,
			&c.RawUsage, &c.EndpointID, &c.UpstreamModel,
			&c.CompressionProfile, &applied, &c.PromptTokensPre, &c.TokensSaved, &cms,
			&c.CompressionRulesFired, &c.ContextTokensPre, &c.ContextTokensSaved, &c.Err); err != nil {
			return nil, err
		}
		if t, err := time.Parse("2006-01-02T15:04:05.000Z", ts); err == nil {
			c.TS = t
		}
		c.TTFTms, c.Totalms = ttftPtr(ttft), ttftPtr(total)
		c.CompressionApplied = applied == 1
		c.CompressionMs = ttftPtr(cms)
		out = append(out, c)
	}
	return out, rows.Err()
}

func ttftPtr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}
