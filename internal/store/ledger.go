package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// Surface identifies which wire protocol served a call.
type Surface string

const (
	SurfaceOpenAI     Surface = "openai"      // /v1/chat/completions
	SurfaceAnthropic  Surface = "anthropic"   // /v1/messages
	SurfaceResponses  Surface = "responses"   // /v1/responses
	SurfaceEmbeddings Surface = "embeddings"  // /v1/embeddings
	SurfaceImagesGen  Surface = "images_gen"  // /v1/images/generations
	SurfaceAudioTTS   Surface = "audio_tts"   // /v1/audio/speech
	SurfaceAudioSTT   Surface = "audio_stt"   // /v1/audio/transcriptions
	SurfaceImagesEdit Surface = "images_edit" // /v1/images/edits
	SurfaceModeration Surface = "moderations" // /v1/moderations
	SurfaceRerank     Surface = "rerank"      // /v1/rerank
	SurfaceVideosGen  Surface = "videos_gen"  // /v1/videos/generations
	SurfaceMusicGen   Surface = "music_gen"   // /v1/music/generations
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
	// IP is the caller address (X-Forwarded-For / X-Real-IP / remote) captured
	// at request time, for traffic mapping. Empty when unavailable.
	IP string
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
	// CompressionVersion is the immutable profile fingerprint behind this
	// row's compression decision (addendum #5).
	CompressionVersion string
	// CompressionStages is the per-engine attribution JSON (addendum #7):
	// every stage that ran, with before/after. Reconciles with TokensSaved
	// via the documented rule on compress.StageStat.
	CompressionStages string
	// ShadowTokensSaved is the sampled POTENTIAL reduction (addendum #7) —
	// tuning data that never contributed to what shipped.
	ShadowTokensSaved  int64
	CompressionApplied bool
	// CompressionRulesFired is caveman attribution: the number of prose rules
	// that rewrote something. Always 0 for session_dedup/rtk/headroom/lite.
	CompressionRulesFired int
	ContextTokensPre      int64
	ContextTokensSaved    int64
	PromptTokensPre       int64
	TokensSaved           int64
	// TokensSavedNotional is what compression WOULD have saved on a request it
	// ultimately shipped unchanged (ratio-floor reject / fail-open). Kept apart
	// from TokensSaved so the dashboard headline never counts bytes we did not
	// actually cut. 0 when TokensSaved was real.
	TokensSavedNotional int64
	CompressionMs       *int64

	// v8 (M9) research capture — the request SHAPE, which raw_usage (a response)
	// cannot reconstruct. See schema.sql calls.
	PrefixSHA string // stable conversation prefix hash ('' = not derivable)
	MsgCount  int    // messages posted upstream
	ToolCount int    // tools in the action space
	SessionID string // operator grouping key from X-Ezllm-Session
	ReqBytes  int    // exact body bytes posted upstream

	// ReservationID is the client-budget reservation this row's usage
	// replaces (not persisted). The flush deletes it in the transaction that
	// counts the usage, so the budget is never both unreserved and uncounted.
	ReservationID int64

	// BlockPos is block_round_robin's per-dispatch tag: the 1-based position
	// of THIS call inside its block (the "N requests finished" evidence).
	// 0 = not counted (direct routes, non-block strategies).
	BlockPos int

	// Translated marks a hop whose WIRE differed from the client's surface, so
	// the bodies were translated in one or both directions: the
	// anthropic->openai-chat leg (Route.Translated) and a hop curated onto the
	// Responses wire (Route.ResponsesWire). The tap reads the client-shaped
	// body the codec synthesized, which carries the cache split in the
	// client's own convention — see schema.sql calls.translated.
	Translated bool

	Err string
}

// Usage holds the normalized counters plus the verbatim JSON.
type Usage struct {
	In, Out, CachedRead, CachedWrite, Reasoning int64
	Raw                                         map[string]any
}

// NormalizeUsage maps a provider's raw usage object onto our surface-independent
// counters, reading an anthropic-shape input_tokens as exclusive of cache (the
// meaning of usage_convention=auto). See NormalizeUsageWith.
func NormalizeUsage(surface Surface, raw map[string]any) Usage {
	return NormalizeUsageWith(surface, raw, provider.UsageConventionAuto)
}

// NormalizeUsageWith maps a raw usage object onto the four billed buckets —
// In (full-price input), CachedRead, CachedWrite, Out — so that every input
// token lands in exactly ONE of them: cost is in·p_in + read·p_read +
// write·p_write + out·p_out, and a token counted in two buckets is billed
// twice. Verified against live ledger rows (2026-10-20):
//
//	openai     prompt_tokens INCLUDES the cache read and write  -> in = prompt - read - write
//	responses  input_tokens INCLUDES the cache read and write   -> in = input - read - write
//	anthropic  input_tokens EXCLUDES them (Anthropic itself, and usage_convention
//	           auto/anthropic); a declared "total" account INCLUDES them
//
// A part is subtracted only when the total can contain it, so nothing goes
// negative; on a declared-total Messages account, a total smaller than its
// cache read is read as exclusive (D3 of usage-convention-design). Reasoning
// is already inside the output count and is recorded for display only.
func NormalizeUsageWith(surface Surface, raw map[string]any, conv provider.UsageConvention) Usage {
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
	// first returns the first non-zero counter among (map, key) pairs.
	first := func(pairs ...any) int64 {
		for i := 0; i+1 < len(pairs); i += 2 {
			if m, _ := pairs[i].(map[string]any); m != nil {
				if v := num(m, pairs[i+1].(string)); v != 0 {
					return v
				}
			}
		}
		return 0
	}
	// inclusive splits a total that contains the cache parts it can hold. A
	// total smaller than its own cache read is broken data; it clamps to 0
	// (under-report, never overcharge).
	inclusive := func(total int64) int64 {
		switch {
		case total >= u.CachedRead+u.CachedWrite:
			return total - u.CachedRead - u.CachedWrite
		case total >= u.CachedRead:
			return total - u.CachedRead // the write cannot be inside it
		}
		return 0
	}

	switch surface {
	case SurfaceAnthropic:
		in := num(raw, "input_tokens")
		u.Out = num(raw, "output_tokens")
		u.CachedRead = num(raw, "cache_read_input_tokens")
		u.CachedWrite = num(raw, "cache_creation_input_tokens")
		u.Reasoning = num(sub(raw, "output_tokens_details"), "thinking_tokens")
		u.In = in
		if conv == provider.UsageConventionTotal && in >= u.CachedRead {
			u.In = inclusive(in)
		}

	case SurfaceResponses:
		det := sub(raw, "input_tokens_details")
		u.CachedRead = num(det, "cached_tokens")
		u.CachedWrite = num(det, "cache_write_tokens")
		u.In = inclusive(num(raw, "input_tokens"))
		u.Out = num(raw, "output_tokens")
		u.Reasoning = num(sub(raw, "output_tokens_details"), "reasoning_tokens")

	case SurfaceOpenAI:
		fallthrough
	default:
		prompt := num(raw, "prompt_tokens")
		if prompt == 0 {
			// some gateways use input_tokens even on the OpenAI surface
			prompt = num(raw, "input_tokens")
		}
		det := sub(raw, "prompt_tokens_details")
		// The details object is the standard place; DeepSeek's own
		// prompt_cache_hit_tokens and a top-level cached_tokens are the
		// fallbacks some gateways send instead.
		u.CachedRead = first(det, "cached_tokens", raw, "prompt_cache_hit_tokens", raw, "cached_tokens")
		u.CachedWrite = first(det, "cache_write_tokens", det, "cache_creation_input_tokens", raw, "cache_creation_input_tokens")
		u.In = inclusive(prompt)
		u.Out = num(raw, "completion_tokens")
		if u.Out == 0 {
			u.Out = num(raw, "output_tokens")
		}
		u.Reasoning = num(sub(raw, "completion_tokens_details"), "reasoning_tokens")
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
// latency to a token stream (invariant #5). A row arriving after Close is
// counted separately (DroppedAfterClose).
func (d *DB) RecordCall(c Call) {
	if c.TS.IsZero() {
		c.TS = time.Now().UTC()
	}
	cp := c
	d.recMu.RLock()
	defer d.recMu.RUnlock()
	if d.closed {
		// The ledger loop has exited: queueing would lose the row silently.
		// Its reservation stays to expire: the pools are closing.
		droppedAfterClose.Add(1)
		return
	}
	select {
	case d.callCh <- ledgerItem{call: &cp}:
	default:
		droppedCalls.Add(1)
		// No row will release it, so release here — off this goroutine,
		// which must never wait on the database.
		if cp.ReservationID != 0 {
			go d.ReleaseClientBudget(context.Background(), cp.ReservationID)
		}
	}
}

// droppedCalls counts ledger rows shed under backpressure (via /admin/health).
var droppedCalls atomic.Int64

// DroppedCalls reports how many ledger rows were shed (should stay 0).
func DroppedCalls() int64 { return droppedCalls.Load() }

// droppedAfterClose counts rows recorded after Close, when no ledger loop is
// left to persist them. Kept apart from backpressure drops: it means a
// producer outlived shutdown, not that the database was slow.
var droppedAfterClose atomic.Int64

// DroppedAfterClose reports how many ledger rows arrived after Close.
func DroppedAfterClose() int64 { return droppedAfterClose.Load() }

// lostRows counts rows a FAILED FLUSH discarded. It is separate from
// droppedCalls on purpose: dropping counts the queue-full path, while a
// transient SQLite lock or a full disk used to discard the whole batch with a
// single log line and no counter — so the number an operator watches to answer
// "am I losing rows?" read 0 while rows were being lost.
var lostRows atomic.Int64

// LostRows reports how many rows failed flushes discarded (should stay 0).
func LostRows() int64 { return lostRows.Load() }

// unmeteredRows counts rows that reached the ledger but not the usage meter:
// the meter is best-effort inside the insert, so its failure rolls back only
// the counter bumps — and model caps and client budgets under-count by those
// rows. Without this number that gap was one Warn line and nothing to watch.
var unmeteredRows atomic.Int64

// UnmeteredRows reports how many committed rows were not metered (should stay 0).
func UnmeteredRows() int64 { return unmeteredRows.Load() }

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
		err := d.insertCalls(ctx, buf)
		if err != nil {
			// One bounded retry before giving up: a transient SQLite lock
			// (a concurrent writer, a checkpoint) clears on its own, and a
			// ledger row is evidence, not boilerplate. Most failures end
			// here, which is what keeps the counter below honest.
			retryCtx, retryCancel := context.WithTimeout(context.Background(), 10*time.Second)
			err = d.insertCalls(retryCtx, buf)
			retryCancel()
		}
		if err != nil {
			// The batch is lost either way — but now it is COUNTED. The old
			// code logged one line and incremented nothing, so the operator's
			// "am I losing rows?" number stayed at zero while rows vanished.
			lostRows.Add(int64(len(buf)))
			slog.Error("ledger flush failed after retry; rows discarded",
				"rows", len(buf), "err", err, "lost_total", lostRows.Load())
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
  ts, account_id, client, ip, surface, alias, account, provider_key_id, key_hint, model,
  status, stream, ttft_ms, total_ms,
  tokens_in, tokens_out, tokens_cached_read, tokens_cached_write, reasoning_tokens, raw_usage,
  endpoint_id, upstream_model,
  compression_profile, compression_applied, prompt_tokens_pre, tokens_saved, compression_ms,
  compression_rules_fired, context_tokens_pre, context_tokens_saved,
  compression_saved_notional, prefix_sha, msg_count, tool_count, session_id, req_bytes, block_pos, translated, err,
  compression_version, compression_stages, shadow_tokens_saved
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
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
			c.Client, c.IP, string(c.Surface), c.Alias, c.Account, keyID, c.KeyHint, c.Model,
			c.Status, boolInt(c.Stream), c.TTFTms, c.Totalms,
			c.TokensIn, c.TokensOut, c.TokensCachedRead, c.TokensCachedWrite, c.ReasoningTokens, c.RawUsage,
			c.EndpointID, c.UpstreamModel,
			c.CompressionProfile, boolInt(c.CompressionApplied), c.PromptTokensPre, c.TokensSaved, c.CompressionMs,
			c.CompressionRulesFired, c.ContextTokensPre, c.ContextTokensSaved,
			c.TokensSavedNotional, c.PrefixSHA, c.MsgCount, c.ToolCount, c.SessionID, c.ReqBytes, c.BlockPos, boolInt(c.Translated), c.Err,
			c.CompressionVersion, c.CompressionStages, c.ShadowTokensSaved,
		)
		if err != nil {
			return fmt.Errorf("insert call: %w", err)
		}
	}

	// Budget reservations end in the SAME transaction that counts their
	// rows' usage. Outside the meter savepoint: a reservation must not
	// outlive its committed row even when metering fails.
	for _, c := range rows {
		if c.ReservationID != 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM client_budget_reservations WHERE id = ?`, c.ReservationID); err != nil {
				return err
			}
		}
	}

	// M5: advance the usage meter in the SAME transaction as the calls rows so
	// the meter and the ledger can never disagree about what counted. Metering
	// is best-effort here: a failed bump must not fail the insert (losing a
	// dashboard row is worse than a briefly stale counter, and counters are
	// rebuildable from `calls` if they ever drift).
	//
	// Best-effort must still be all-or-nothing: the meter runs inside a
	// savepoint, so a failure partway through (model counters bumped, client
	// counters not, or half the groups) rolls back every bump of this batch
	// instead of committing a meter that disagrees with itself.
	if _, err := tx.ExecContext(ctx, `SAVEPOINT meter`); err != nil {
		return err
	}
	metered := true
	if err := d.meterInTx(ctx, tx, rows); err != nil {
		metered = false
		slog.Warn("usage meter bump failed; batch not metered", "rows", len(rows), "err", err)
		if _, rerr := tx.ExecContext(ctx, `ROLLBACK TO meter`); rerr != nil {
			return rerr
		}
	}
	if _, err := tx.ExecContext(ctx, `RELEASE meter`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Counted only once the rows are durable: a commit that fails is retried
	// by the ledger loop, and counting before it would count those rows twice.
	if !metered {
		unmeteredRows.Add(int64(len(rows)))
	}
	return nil
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
	if err := bumpUsageCounters(ctx, tx, groupForCounter(rows, windows)); err != nil {
		return err
	}
	return d.meterClientsInTx(ctx, tx, rows)
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
	Key         string `json:"k"`
	Calls       int64  `json:"calls"`
	TokensIn    int64  `json:"tin"`
	TokensOut   int64  `json:"tout"`
	CachedRead  int64  `json:"cread"`
	CachedWrite int64  `json:"cwrite"`
	Reasoning   int64  `json:"reasoning"`
	TokensSaved int64  `json:"saved"`
	// AppliedSaved is the AUTHORITATIVE savings aggregate: SUM over rows
	// where compression_applied=1 only. TokensSaved sums the raw column,
	// which sweeps in legacy rows written before the applied/notional split
	// (~21k tokens sitting on declined rows) — kept as evidence, never as a
	// headline. Every UI total must read AppliedSaved.
	AppliedSaved int64 `json:"applied_saved"`
	ContextPre   int64 `json:"context_pre"`
	ContextSaved int64 `json:"context_saved"`
	Errors       int64 `json:"errors"`
	P50TTFTms    int64 `json:"p50_ttft_ms"`
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
       COALESCE(SUM(CASE WHEN compression_applied=1 THEN tokens_saved ELSE 0 END),0) AS applied_saved,
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
		if err := rows.Scan(&r.Key, &r.Calls, &r.TokensIn, &r.TokensOut, &r.CachedRead, &r.CachedWrite, &r.Reasoning, &r.TokensSaved, &r.AppliedSaved, &r.ContextPre, &r.ContextSaved, &r.Errors); err != nil {
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
		// UTC by construction: rows are stamped UTC and this slices the first
		// 10 chars off the stamp, so the dashboard's day boundary is midnight
		// UTC. Usage-rule caps evaluate their bucket in the RULE's
		// *time.Location (rules.Window.loc), so with a non-UTC rule the
		// dashboard's day view and the "daily … cap reached" refusal can
		// disagree about which day a call belongs to. Documented in
		// docs/API.md under GET /admin/usage; deliberately NOT silently
		// changed — making it tz-aware is a behaviour change for every
		// existing dashboard.
		return "substr(ts,1,10)", nil
	default:
		return "", fmt.Errorf("store: unknown group_by %q", groupBy)
	}
}

// fillP50 computes the median TTFT per group.
//
// It used to SELECT every matching (k, ttft_ms) row into a Go map on EVERY
// /admin/usage request — cost scaling with ledger size instead of request
// rate, against the codebase's own rule: "Do not sum paginated rows for
// dashboard totals; calculate aggregates over the full filtered set in SQL."
// The window functions below partition and order inside SQLite and return
// exactly ONE row per group, so what crosses into Go is bounded by the number
// of groups and the work is a single range scan.
//
// Median convention (documented because it was silent for years): the UPPER of
// the two middle values for an even count — rn = n/2 + 1 reproduces the
// historical Go `vs[len(vs)/2]` exactly. This number is directional guidance
// on a dashboard, not a statistical claim, so the convention is pinned rather
// than "improved" to the true mean of the middles: silently changing it would
// move every even-sized group's number in the UI with no way to tell.
func (d *DB) fillP50(ctx context.Context, col string, from, to time.Time, rows []UsageRow) error {
	if len(rows) == 0 {
		return nil
	}
	q := fmt.Sprintf(`SELECT k, ttft_ms FROM (
  SELECT %s AS k, ttft_ms,
         ROW_NUMBER() OVER (PARTITION BY %s ORDER BY ttft_ms) AS rn,
         COUNT(*)     OVER (PARTITION BY %s)                  AS n
  FROM calls
  WHERE ts >= ? AND ts < ? AND ttft_ms IS NOT NULL
)
WHERE rn = n / 2 + 1
ORDER BY k, ttft_ms`, col, col, col)
	rs, err := d.r.QueryContext(ctx, q, from.UTC().Format("2006-01-02T15:04:05.000Z"), to.UTC().Format("2006-01-02T15:04:05.000Z"))
	if err != nil {
		return err
	}
	defer rs.Close()
	med := make(map[string]int64, len(rows))
	for rs.Next() {
		var k string
		var v int64
		if err := rs.Scan(&k, &v); err != nil {
			return err
		}
		med[k] = v
	}
	if err := rs.Err(); err != nil {
		return err
	}
	for i := range rows {
		if v, ok := med[rows[i].Key]; ok {
			rows[i].P50TTFTms = v
		}
	}
	return nil
}

// TotalP50TTFTms computes the median TTFT across the WHOLE window — the
// totals row of the usage report. Summing per-group medians would be
// arithmetically meaningless, so the totals card read P50TTFTms straight off
// the summed struct and showed 0 "not measured yet" for its whole life even
// though thousands of rows carried a measured TTFT. Same window-function
// shape and the same upper-of-two-middles convention as fillP50 — one row
// scanned back, no per-row data crossing into Go.
func (d *DB) TotalP50TTFTms(ctx context.Context, from, to time.Time) (int64, error) {
	var v sql.NullInt64
	err := d.r.QueryRowContext(ctx, `SELECT ttft_ms FROM (
  SELECT ttft_ms,
         ROW_NUMBER() OVER (ORDER BY ttft_ms) AS rn,
         COUNT(*)     OVER ()                  AS n
  FROM calls
  WHERE ts >= ? AND ts < ? AND ttft_ms IS NOT NULL
)
WHERE rn = n / 2 + 1`,
		from.UTC().Format("2006-01-02T15:04:05.000Z"),
		to.UTC().Format("2006-01-02T15:04:05.000Z")).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil // no measured TTFT in the window — 0, honestly
	}
	if err != nil {
		return 0, err
	}
	if !v.Valid {
		return 0, nil
	}
	return v.Int64, nil
}

// UsageDayReportTZ aggregates the ledger by LOCAL DAY in loc, computing the day
// buckets in Go rather than in SQL.
//
// Why this one path is the exception to "aggregate in SQL": SQLite has no IANA
// zone table, so a day bucket cannot be expressed in pure SQL for a zone that
// observes DST. The tempting strftime('%Y-%m-%d', ts, '+N hours') trick is
// correct for a FIXED-offset zone and silently wrong by up to an hour either
// side of a DST transition — precisely the silent-wrong-answer class F9 exists
// to remove. Converting per row in Go is correct for every zone.
//
// The bucketing is CountBucket(ts, "daily", loc): the SAME function the usage
// meter writes its counters with. That is the whole point — one implementation
// of "which day is this", so the dashboard's day view and the rule's
// "daily … cap reached" refusal cannot drift apart by a zone offset.
//
// Every other group_by (and the no-tz default) still aggregates in SQL; this
// path is opt-in via tz, and the window is capped by the caller.
func (d *DB) UsageDayReportTZ(ctx context.Context, from, to time.Time, loc *time.Location) ([]UsageRow, error) {
	if loc == nil {
		loc = time.UTC
	}
	rs, err := d.r.QueryContext(ctx, `
SELECT ts, ttft_ms,
       COALESCE(tokens_in,0), COALESCE(tokens_out,0),
       COALESCE(tokens_cached_read,0), COALESCE(tokens_cached_write,0),
       COALESCE(reasoning_tokens,0), COALESCE(tokens_saved,0),
       COALESCE(context_tokens_pre,0), COALESCE(context_tokens_saved,0),
       status, COALESCE(compression_applied,0)
FROM calls
WHERE ts >= ? AND ts < ?`, from.UTC().Format("2006-01-02T15:04:05.000Z"), to.UTC().Format("2006-01-02T15:04:05.000Z"))
	if err != nil {
		return nil, err
	}
	defer rs.Close()

	type acc struct {
		row   UsageRow
		ttfts []int64
	}
	byDay := map[string]*acc{}
	for rs.Next() {
		var ts string
		var ttft sql.NullInt64
		var tin, tout, cread, cwrite, reasoning, saved, cpre, csaved int64
		var status, applied int
		if err := rs.Scan(&ts, &ttft, &tin, &tout, &cread, &cwrite, &reasoning, &saved, &cpre, &csaved, &status, &applied); err != nil {
			return nil, err
		}
		t, err := time.Parse("2006-01-02T15:04:05.000Z", ts)
		if err != nil {
			continue // a corrupt stamp must not abort the whole report
		}
		day := CountBucket(t, "daily", loc) // the meter's own bucketing
		a := byDay[day]
		if a == nil {
			a = &acc{row: UsageRow{Key: day}}
			byDay[day] = a
		}
		a.row.Calls++
		a.row.TokensIn += tin
		a.row.TokensOut += tout
		a.row.CachedRead += cread
		a.row.CachedWrite += cwrite
		a.row.Reasoning += reasoning
		a.row.TokensSaved += saved
		if applied == 1 {
			a.row.AppliedSaved += saved // the SQL path's applied_saved, row for row
		}
		a.row.ContextPre += cpre
		a.row.ContextSaved += csaved
		if status >= 400 {
			a.row.Errors++
		}
		if ttft.Valid {
			a.ttfts = append(a.ttfts, ttft.Int64)
		}
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}

	out := make([]UsageRow, 0, len(byDay))
	for _, a := range byDay {
		if len(a.ttfts) > 0 {
			slices.Sort(a.ttfts)
			// Same convention as fillP50: the UPPER of the two middle values
			// for an even count. Documented there; kept identical here so the
			// tz path and the SQL path report the same statistic.
			a.row.P50TTFTms = a.ttfts[len(a.ttfts)/2]
		}
		out = append(out, a.row)
	}
	slices.SortFunc(out, func(x, y UsageRow) int { return strings.Compare(x.Key, y.Key) })
	return out, nil
}

// RecentCalls returns the newest N ledger rows (drilldown / debugging).
func (d *DB) RecentCalls(ctx context.Context, limit int) ([]Call, error) {
	if limit <= 0 || limit > 1000 {
		limit = 50
	}
	rows, err := d.r.QueryContext(ctx, `
SELECT ts, client, COALESCE(ip,''), surface, alias, account, COALESCE(provider_key_id,0), key_hint, model,
       status, stream, ttft_ms, total_ms,
       tokens_in, tokens_out, tokens_cached_read, tokens_cached_write, reasoning_tokens,
       COALESCE(raw_usage,''), COALESCE(endpoint_id,''), COALESCE(upstream_model,''),
       compression_profile, compression_applied, prompt_tokens_pre, tokens_saved, compression_ms,
       compression_rules_fired, context_tokens_pre, context_tokens_saved,
       COALESCE(compression_saved_notional,0), COALESCE(prefix_sha,''), msg_count, tool_count,
       COALESCE(session_id,''), req_bytes, COALESCE(block_pos,0), COALESCE(translated,0), COALESCE(err,''),
       COALESCE(compression_version,''), COALESCE(compression_stages,''), COALESCE(shadow_tokens_saved,0)
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
		var translated int
		if err := rows.Scan(&ts, &c.Client, &c.IP, &c.Surface, &c.Alias, &c.Account, &c.KeyID, &c.KeyHint, &c.Model,
			&c.Status, &c.Stream, &ttft, &total,
			&c.TokensIn, &c.TokensOut, &c.TokensCachedRead, &c.TokensCachedWrite, &c.ReasoningTokens,
			&c.RawUsage, &c.EndpointID, &c.UpstreamModel,
			&c.CompressionProfile, &applied, &c.PromptTokensPre, &c.TokensSaved, &cms,
			&c.CompressionRulesFired, &c.ContextTokensPre, &c.ContextTokensSaved,
			&c.TokensSavedNotional, &c.PrefixSHA, &c.MsgCount, &c.ToolCount,
			&c.SessionID, &c.ReqBytes, &c.BlockPos, &translated, &c.Err, &c.CompressionVersion,
			&c.CompressionStages, &c.ShadowTokensSaved); err != nil {
			return nil, err
		}
		if t, err := time.Parse("2006-01-02T15:04:05.000Z", ts); err == nil {
			c.TS = t
		}
		c.TTFTms, c.Totalms = ttftPtr(ttft), ttftPtr(total)
		c.CompressionApplied = applied == 1
		c.Translated = translated == 1
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
