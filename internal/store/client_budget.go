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
	"path"
	"strings"
	"time"
)

// ClientAllowsModel is the per-key model allowlist. Empty = every model.
// Patterns are comma-separated exact ids or globs (`deepseek/*`, `gpt-5*`).
func (d *DB) ClientAllowsModel(ctx context.Context, name, model string) (bool, error) {
	var patterns string
	err := d.r.QueryRowContext(ctx,
		`SELECT allowed_models FROM client_tokens WHERE name = ? AND enabled = 1`, name).Scan(&patterns)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("client allowlist: %w", err)
	}
	patterns = strings.TrimSpace(patterns)
	if patterns == "" {
		return true, nil
	}
	for _, raw := range strings.Split(patterns, ",") {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		if p == "*" || p == model {
			return true, nil
		}
		if ok, _ := path.Match(p, model); ok {
			return true, nil
		}
		// path.Match's `*` does not cross `/`; a namespace prefix is the
		// common case (`deepseek/*`) and should match the whole model id.
		if strings.HasSuffix(p, "*") && strings.HasPrefix(model, strings.TrimSuffix(p, "*")) {
			return true, nil
		}
	}
	return false, nil
}

// ReserveClientBudget admits a request by reserving its max output tokens
// against the token's current bucket. The row is deleted when the handler
// returns; rows older than 15 minutes are ignored at once and pruned at most
// once per reservationSweepEvery, so a crashed process cannot pin a budget
// forever.
func (d *DB) ReserveClientBudget(ctx context.Context, client string, tokens int64) (id int64, ok bool, err error) {
	if tokens <= 0 {
		return 0, true, nil
	}
	var limit int64
	var window string
	err = d.r.QueryRowContext(ctx,
		`SELECT cap_tokens, COALESCE(NULLIF(cap_window,''), 'monthly')
		   FROM client_tokens WHERE name = ? AND enabled = 1`, client).
		Scan(&limit, &window)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, ErrNotFound
	}
	if err != nil {
		return 0, false, fmt.Errorf("client reservation: %w", err)
	}
	if limit <= 0 {
		return 0, true, nil
	}
	if err := d.ensureMeter(ctx); err != nil {
		return 0, false, err
	}
	now := time.Now()
	bucket := CountBucket(now, window, time.Local)
	err = d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		// Pruning is housekeeping only — the reserved sum below already ignores
		// expired rows — so it runs at most once per reservationSweepEvery
		// instead of inside every capped request's write.
		if d.reservationSweepDue(now) {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM client_budget_reservations WHERE expires_at < ?`,
				now.UTC().Format(time.RFC3339)); err != nil {
				return err
			}
		}
		var used int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(tokens,0) FROM client_usage_counters WHERE client = ? AND bucket = ?`,
			client, bucket).Scan(&used); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var reserved int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(tokens),0) FROM client_budget_reservations WHERE client = ? AND bucket = ? AND expires_at >= ?`,
			client, bucket, now.UTC().Format(time.RFC3339)).Scan(&reserved); err != nil {
			return err
		}
		if used+reserved+tokens > limit {
			return nil
		}
		res, err := tx.ExecContext(ctx, `
INSERT INTO client_budget_reservations (client, bucket, tokens, created_at, expires_at)
VALUES (?,?,?,?,?)`,
			client, bucket, tokens,
			now.UTC().Format(time.RFC3339),
			now.Add(15*time.Minute).UTC().Format(time.RFC3339))
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		ok = true
		return nil
	})
	return id, ok, err
}

// reservationSweepEvery bounds how often expired reservations are pruned.
const reservationSweepEvery = time.Minute

// reservationSweepDue claims the next prune slot; exactly one caller per
// interval gets true.
func (d *DB) reservationSweepDue(now time.Time) bool {
	last := d.lastReservationSweep.Load()
	if now.UnixNano()-last < int64(reservationSweepEvery) {
		return false
	}
	return d.lastReservationSweep.CompareAndSwap(last, now.UnixNano())
}

// ReleaseClientBudget drops an in-flight reservation.
func (d *DB) ReleaseClientBudget(ctx context.Context, id int64) {
	if id == 0 {
		return
	}
	_ = d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM client_budget_reservations WHERE id = ?`, id)
		return err
	})
}

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
	if err := d.ensureMeter(ctx); err != nil {
		return ClientBudget{}, err
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
		        FROM client_tokens WHERE name IN (` + joinComma(ph) + `)`
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

// meterClientsInTx folds the batch into per-token buckets. It meters every
// token (not only capped ones) so a cap enabled later starts from real usage
// instead of a counter that stopped at the first uncapped call.
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
