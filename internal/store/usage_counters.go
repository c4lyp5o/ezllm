package store

// M5 rules engine — the O(1) usage meter.
//
// Two properties make this safe on the hot path:
//
//  1. Reads are a single primary-key lookup. There is no SUM() over `calls`
//     anywhere near a request — an aggregate per inference would put SQLite
//     work on every call and scale with the ledger's size, not its rate.
//  2. Rotation is implicit. A window that ends simply writes a new bucket key,
//     so there is no reset job and no "forgot to reset the counter" bug class.
//     Old buckets age out by being ignored (and, optionally, pruned).
//
// Increment happens inside insertCalls' existing transaction (see ledger.go),
// in the same commit as the calls rows themselves — so the meter can never
// disagree with the ledger about whether a call counted.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CountedTokens is THE documented quantity a cap metering counts: prompt plus
// completion. Deliberately excludes cached_read — a cache hit is not what a
// vendor charges full price for, so counting it would disagree with their
// invoice while confusing the user. Changing this constant changes what every
// existing cap means, so it stays a named constant rather than an inline sum.
const CountedTokens = "tokens_in + tokens_out"

// UsageCounter is one metered bucket. Tokens is what the cap compares
// against; Calls is kept for the dashboard ("1,204 calls in this window").
type UsageCounter struct {
	AccountID int64  `json:"account_id"`
	ModelID   string `json:"model_id"`
	Bucket    string `json:"bucket"`
	Tokens    int64  `json:"tokens"`
	Calls     int64  `json:"calls"`
	UpdatedAt string `json:"updated_at"`
}

// CountBucket returns the meter bucket key for `t` under the given window.
//
// The window is evaluated in `loc`, not in UTC and not in the server's local
// zone: a rule selling a US night discount must flip at New York midnight,
// not Kuala Lumpur's. Malaysia has no DST (fixed UTC+8), so our own default
// tz is stable, but a vendor's is not.
//
// Buckets are calendar-aligned:
//
//	5h      → 2026-10-06T2   (block = hour/5: 0=00-04, 1=05-09, 2=10-14,
//	                           3=15-19, 4=20-23 — block 4 is short because a
//	                           day has 24 hours, not 25; accepted caveat)
//	daily   → 2026-10-06
//	weekly  → 2026-W41       (ISO week)
//	monthly → 2026-10
//
// Calendar blocks were chosen over rolling windows on purpose: a rolling 5h
// would need a range scan over `calls` (forbidden here) or sub-bucket
// granularity. The cost is that headroom can be small right after a reset.
func CountBucket(t time.Time, window string, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	lt := t.In(loc)
	switch window {
	case "5h":
		return fmt.Sprintf("%s-T%d", lt.Format("2006-01-02"), lt.Hour()/5)
	case "daily":
		return lt.Format("2006-01-02")
	case "weekly":
		y, w := lt.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", y, w)
	default: // "monthly" and anything unset
		return lt.Format("2006-01")
	}
}

// UsageFor reads one bucket. Missing bucket = 0 usage (never an error: a model
// that has never been called is simply not over its cap).
func (d *DB) UsageFor(ctx context.Context, accountID int64, modelID, bucket string) (UsageCounter, error) {
	var c UsageCounter
	err := d.r.QueryRowContext(ctx,
		`SELECT account_id, model_id, bucket, tokens, calls, updated_at
		   FROM usage_counters WHERE account_id = ? AND model_id = ? AND bucket = ?`,
		accountID, modelID, bucket).Scan(&c.AccountID, &c.ModelID, &c.Bucket, &c.Tokens, &c.Calls, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		// Never called in this window = zero usage, not an error. Gating a
		// cap on "the row exists" would refuse every model's first request.
		return UsageCounter{AccountID: accountID, ModelID: modelID, Bucket: bucket}, nil
	}
	if err != nil {
		return UsageCounter{}, fmt.Errorf("usage for %s in bucket %s: %w", modelID, bucket, err)
	}
	return c, nil
}

// counterGroup is a batch of calls folded into one increment — N rows in a
// ledger flush usually collapse to far fewer counter upserts.
type counterGroup struct {
	accountID int64
	modelID   string
	bucket    string
	tokens    int64
	calls     int64
}

// ruleWindowsFor resolves the metering window + timezone for every
// (account, model) present in this batch. It is a single indexed query over
// model_rules (UNIQUE(account_id, model_id)), issued once per ledger flush —
// never once per request. Models with no rule are simply absent from the map,
// and a missing model is not metered (nothing caps it).
func (d *DB) ruleWindowsFor(ctx context.Context, rows []Call) (map[modelKey]windowSpec, error) {
	type pair struct {
		accountID int64
		modelID   string
	}
	seen := map[pair]bool{}
	var pairs []pair
	for _, c := range rows {
		if c.AccountID == 0 || c.Model == "" {
			continue
		}
		k := pair{c.AccountID, c.Model}
		if !seen[k] {
			seen[k] = true
			pairs = append(pairs, k)
		}
	}
	out := make(map[modelKey]windowSpec, len(pairs))
	if len(pairs) == 0 {
		return out, nil
	}
	// Chunked IN queries keep us well under SQLite's variable limit even for
	// a large batch (BatchSize is 32 by default, but a barrier can flush more).
	const chunk = 400
	for start := 0; start < len(pairs); start += chunk {
		end := start + chunk
		if end > len(pairs) {
			end = len(pairs)
		}
		part := pairs[start:end]
		ph := make([]string, len(part))
		args := make([]any, 0, len(part)*2)
		for i, p := range part {
			ph[i] = "(?,?)"
			args = append(args, p.accountID, p.modelID)
		}
		q := `SELECT account_id, model_id, cap_window, win_tz FROM model_rules
              WHERE (account_id, model_id) IN (` + joinComma(ph) + `)`
		rws, err := d.r.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("load rule windows: %w", err)
		}
		for rws.Next() {
			var mk modelKey
			var w windowSpec
			if err := rws.Scan(&mk.accountID, &mk.modelID, &w.window, &w.tz); err != nil {
				rws.Close()
				return nil, fmt.Errorf("scan rule window: %w", err)
			}
			out[mk] = w
		}
		if err := rws.Err(); err != nil {
			rws.Close()
			return nil, err
		}
		rws.Close()
	}
	return out, nil
}

func joinComma(ss []string) string {
	var b strings.Builder
	for i, s := range ss {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s)
	}
	return b.String()
}

// modelKey identifies a metered (account, model) pair — the same key the
// router's Eligible seam passes, so a rule lookup needs no translation.
type modelKey struct {
	accountID int64
	modelID   string
}

// windowSpec is a model's metering window: which cap_window buckets it uses
// and which timezone those buckets are aligned to.
type windowSpec struct {
	window string // 5h|daily|weekly|monthly
	tz     string // IANA name or offset
}

// loc resolves the spec's timezone, defaulting to UTC if it is empty or
// unparseable (a corrupt tz must not crash the ledger flush; UTC is the safe
// neutral because buckets only need to be *consistent*, and a wrong-but-
// stable boundary is recoverable while a panic is not).
func (w windowSpec) loc() *time.Location {
	if w.tz == "" {
		return time.UTC
	}
	if l, err := time.LoadLocation(w.tz); err == nil {
		return l
	}
	return time.UTC
}

// groupForCounter folds ledger rows into one increment per
// (account, model, bucket).
//
// Rows with no AccountID (pre-M5 history, or an identity we could not
// resolve) are skipped: a meter credited from an uncertain identity could
// merge two accounts' budgets, and refusing to meter is always safe — a
// missing increment under-counts (soft cap drifts high), never over-counts.
func groupForCounter(rows []Call, windows map[modelKey]windowSpec) []counterGroup {
	if len(rows) == 0 {
		return nil
	}
	type key struct {
		accountID int64
		modelID   string
		bucket    string
	}
	order := []key{}
	agg := map[key]*counterGroup{}
	for _, c := range rows {
		if c.AccountID == 0 || c.Model == "" {
			continue
		}
		w, ok := windows[modelKey{c.AccountID, c.Model}]
		if !ok {
			continue // no rule → not metered (nothing to cap)
		}
		bucket := CountBucket(c.TS, w.window, w.loc())
		k := key{c.AccountID, c.Model, bucket}
		g, ok2 := agg[k]
		if !ok2 {
			g = &counterGroup{accountID: k.accountID, modelID: k.modelID, bucket: k.bucket}
			agg[k] = g
			order = append(order, k)
		}
		g.tokens += c.TokensIn + c.TokensOut
		g.calls++
	}
	out := make([]counterGroup, 0, len(order))
	for _, k := range order {
		out = append(out, *agg[k])
	}
	return out
}

// bumpUsageCounters performs the increments. Must be called WITHIN an open
// transaction on tx so the meter commits atomically with the calls rows —
// a meter that advances without its calls (or vice versa) is worse than no
// meter, because it silently drifts from the truth.
func bumpUsageCounters(ctx context.Context, tx *sql.Tx, groups []counterGroup) error {
	if len(groups) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO usage_counters (account_id, model_id, bucket, tokens, calls, updated_at)
VALUES (?,?,?,?,?, strftime('%Y-%m-%dT%H:%M:%fZ','now'))
ON CONFLICT(account_id, model_id, bucket) DO UPDATE SET
  tokens = tokens + excluded.tokens,
  calls  = calls  + excluded.calls,
  updated_at = excluded.updated_at`)
	if err != nil {
		return fmt.Errorf("prepare usage counter: %w", err)
	}
	defer stmt.Close()
	for _, g := range groups {
		if _, err := stmt.ExecContext(ctx, g.accountID, g.modelID, g.bucket, g.tokens, g.calls); err != nil {
			return fmt.Errorf("bump usage counter: %w", err)
		}
	}
	return nil
}
