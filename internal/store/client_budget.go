package store

// Per-client-token budgets: the meter behind the per-key cap. Independent of
// model rules (which meter an account+model); a client token can route to any
// number of accounts/models and still has one budget.
//
// Counted quantity is the same as every other cap: tokens_in + tokens_out
// (CountedTokens). Buckets use CountBucket, so the dashboard's "5h/daily/
// weekly/monthly" labels mean the same thing here as on a model rule.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ClientBudget is the answer to "may this token make another call?".
type ClientBudget struct {
	Limit  int64  `json:"limit"`  // 0 = unlimited
	Used   int64  `json:"used"`   // counted tokens in the current bucket
	Window string `json:"window"` // 5h|daily|weekly|monthly
	Bucket string `json:"bucket"`
}

// ClientBudgetStatus reads the token's cap and its current bucket usage.
// ErrNotFound means the token row disappeared (revoked) — callers treat it as
// unauthorized, never as "unlimited".
func (d *DB) ClientBudgetStatus(ctx context.Context, name string) (ClientBudget, error) {
	var limit int64
	var window string
	err := d.r.QueryRowContext(ctx,
		`SELECT cap_tokens, COALESCE(NULLIF(cap_window,''), 'monthly')
		   FROM client_tokens WHERE name = ? AND enabled = 1`, name).
		Scan(&limit, &window)
	if errors.Is(err, sql.ErrNoRows) {
		return ClientBudget{}, ErrNotFound
	}
	if err != nil {
		return ClientBudget{}, fmt.Errorf("client budget: %w", err)
	}
	if limit <= 0 {
		return ClientBudget{Limit: 0, Window: window}, nil
	}
	now := time.Now()
	bucket := CountBucket(now, window, time.Local)
	var used int64
	err = d.r.QueryRowContext(ctx,
		`SELECT COALESCE(tokens,0) FROM client_usage_counters WHERE client = ? AND bucket = ?`,
		name, bucket).Scan(&used)
	if errors.Is(err, sql.ErrNoRows) {
		// A cap set after the key already spent tokens has no counter yet.
		// Backfill from the ledger once, then the normal meter takes over.
		used, err = d.backfillClientBucket(ctx, name, window, bucket, now)
	}
	if err != nil {
		return ClientBudget{}, fmt.Errorf("client budget usage: %w", err)
	}
	return ClientBudget{Limit: limit, Used: used, Window: window, Bucket: bucket}, nil
}

// backfillClientBucket computes the current bucket from `calls` and seeds the
// counter. This runs only when the counter row is missing (new cap on an
// existing key, or a migrated database), so the hot path stays a PK lookup.
func (d *DB) backfillClientBucket(ctx context.Context, name, window, bucket string, now time.Time) (int64, error) {
	start, end := BucketRange(now, window, time.Local)
	var used, calls int64
	err := d.r.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(tokens_in + tokens_out),0), COUNT(*)
		   FROM calls WHERE client = ? AND ts >= ? AND ts < ?`,
		name, start.UTC().Format("2006-01-02T15:04:05.000Z"),
		end.UTC().Format("2006-01-02T15:04:05.000Z")).Scan(&used, &calls)
	if err != nil {
		return 0, err
	}
	if calls > 0 {
		if err := d.WithWriteTx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `
INSERT INTO client_usage_counters (client, bucket, tokens, calls, updated_at)
VALUES (?,?,?,?,strftime('%Y-%m-%dT%H:%M:%fZ','now'))
ON CONFLICT(client, bucket) DO NOTHING`, name, bucket, used, calls)
			return err
		}); err != nil {
			return 0, err
		}
	}
	return used, nil
}

// BucketRange returns the half-open [start,end) of the bucket containing t.
// Kept next to CountBucket so the two definitions of a window cannot drift.
func BucketRange(t time.Time, window string, loc *time.Location) (time.Time, time.Time) {
	if loc == nil {
		loc = time.UTC
	}
	lt := t.In(loc)
	switch window {
	case "5h":
		hour := (lt.Hour() / 5) * 5
		start := time.Date(lt.Year(), lt.Month(), lt.Day(), hour, 0, 0, 0, loc)
		end := start.Add(5 * time.Hour)
		// Block 4 is 20:00–24:00, not 20:00–01:00: CountBucket's block index
		// restarts at midnight, so the range must not spill into the next day.
		if dayEnd := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1); end.After(dayEnd) {
			end = dayEnd
		}
		return start, end
	case "daily":
		start := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc)
		return start, start.AddDate(0, 0, 1)
	case "weekly":
		offset := (int(lt.Weekday()) + 6) % 7 // Monday = 0
		start := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -offset)
		return start, start.AddDate(0, 0, 7)
	default: // monthly
		start := time.Date(lt.Year(), lt.Month(), 1, 0, 0, 0, 0, loc)
		return start, start.AddDate(0, 1, 0)
	}
}

func (d *DB) clientWindowsFor(ctx context.Context, rows []Call) (map[string]windowSpec, error) {
	seen := map[string]bool{}
	var names []string
	for _, c := range rows {
		if c.Client == "" || seen[c.Client] {
			continue
		}
		seen[c.Client] = true
		names = append(names, c.Client)
	}
	out := map[string]windowSpec{}
	if len(names) == 0 {
		return out, nil
	}
	const chunk = 400
	for start := 0; start < len(names); start += chunk {
		end := start + chunk
		if end > len(names) {
			end = len(names)
		}
		part := names[start:end]
		ph := make([]string, len(part))
		args := make([]any, 0, len(part))
		for i, n := range part {
			ph[i] = "?"
			args = append(args, n)
		}
		q := `SELECT name, COALESCE(NULLIF(cap_window,''), 'monthly')
		        FROM client_tokens WHERE cap_tokens > 0 AND name IN (` + joinComma(ph) + `)`
		rs, err := d.r.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("load client windows: %w", err)
		}
		for rs.Next() {
			var name, window string
			if err := rs.Scan(&name, &window); err != nil {
				rs.Close()
				return nil, fmt.Errorf("scan client window: %w", err)
			}
			out[name] = windowSpec{window: window}
		}
		if err := rs.Err(); err != nil {
			rs.Close()
			return nil, err
		}
		rs.Close()
	}
	return out, nil
}

type clientCounterGroup struct {
	client string
	bucket string
	tokens int64
	calls  int64
}

func groupForClientCounter(rows []Call, windows map[string]windowSpec) []clientCounterGroup {
	type key struct{ client, bucket string }
	byKey := map[key]*clientCounterGroup{}
	var order []key
	for _, c := range rows {
		if c.Client == "" {
			continue
		}
		w, ok := windows[c.Client]
		if !ok {
			continue
		}
		bucket := CountBucket(c.TS, w.window, time.Local)
		k := key{c.Client, bucket}
		g := byKey[k]
		if g == nil {
			g = &clientCounterGroup{client: c.Client, bucket: bucket}
			byKey[k] = g
			order = append(order, k)
		}
		g.tokens += c.TokensIn + c.TokensOut
		g.calls++
	}
	out := make([]clientCounterGroup, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	return out
}

func bumpClientCounters(ctx context.Context, tx *sql.Tx, groups []clientCounterGroup) error {
	for _, g := range groups {
		_, err := tx.ExecContext(ctx, `
INSERT INTO client_usage_counters (client, bucket, tokens, calls, updated_at)
VALUES (?,?,?,?,strftime('%Y-%m-%dT%H:%M:%fZ','now'))
ON CONFLICT(client, bucket) DO UPDATE SET
  tokens = tokens + excluded.tokens,
  calls  = calls  + excluded.calls,
  updated_at = excluded.updated_at`,
			g.client, g.bucket, g.tokens, g.calls)
		if err != nil {
			return fmt.Errorf("bump client counter %s/%s: %w", g.client, g.bucket, err)
		}
	}
	return nil
}

// meterClientsInTx folds the batch into per-token buckets. Only tokens with a
// cap are metered; an unlimited token would write a counter nobody reads.
func (d *DB) meterClientsInTx(ctx context.Context, tx *sql.Tx, rows []Call) error {
	if len(rows) == 0 {
		return nil
	}
	windows, err := d.clientWindowsFor(ctx, rows)
	if err != nil {
		return err
	}
	return bumpClientCounters(ctx, tx, groupForClientCounter(rows, windows))
}
