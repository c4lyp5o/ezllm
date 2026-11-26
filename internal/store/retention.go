package store

import (
	"context"
	"database/sql"
	"time"
)

// MinRetentionDays is the shortest ledger retention allowed. Every budget,
// spend cap and rule window (5h up to a calendar month) reads raw calls, so
// the ledger always keeps a little over a month of them.
const MinRetentionDays = 32

// maxRollupDaysPerRun bounds one run: a first run on a years-old ledger rolls
// a few hundred days and the rest follow on the next run.
const maxRollupDaysPerRun = 400

// RollUpLedger sums every call before `before` into calls_daily (one row per
// UTC day, account, key, client, alias, surface and model) and then deletes
// those calls. It works one UTC day per transaction, oldest first, so the
// write lock is held briefly and a stop part way leaves whole days done.
// `before` is rounded down to midnight UTC: only whole days are rolled.
//
// A call with a missed meter debit still pending is kept until the debit is
// reconciled; rolling it would drop the debit with the row.
func (d *DB) RollUpLedger(ctx context.Context, before time.Time) (days int, rows int64, err error) {
	cutoff := before.UTC().Truncate(24 * time.Hour).Format("2006-01-02T15:04:05.000Z")
	for days < maxRollupDaysPerRun {
		var oldest sql.NullString
		if err := d.r.QueryRowContext(ctx, `
SELECT MIN(ts) FROM calls
WHERE ts < ? AND id NOT IN (SELECT call_id FROM pending_usage_meter)`, cutoff).Scan(&oldest); err != nil {
			return days, rows, err
		}
		if !oldest.Valid || len(oldest.String) < 10 {
			return days, rows, nil
		}
		day := oldest.String[:10]
		start := day + "T00:00:00.000Z"
		t, perr := time.Parse("2006-01-02", day)
		if perr != nil {
			return days, rows, perr
		}
		end := t.AddDate(0, 0, 1).Format("2006-01-02T15:04:05.000Z")
		if end > cutoff {
			end = cutoff
		}
		var n int64
		err := d.WithWriteTx(ctx, func(tx *sql.Tx) error {
			const eligible = `ts >= ? AND ts < ? AND id NOT IN (SELECT call_id FROM pending_usage_meter)`
			if _, err := tx.ExecContext(ctx, `
INSERT INTO calls_daily (day, account_id, account, provider_key_id, key_hint, client, alias, surface, model,
  calls, errors, tokens_in, tokens_out, tokens_cached_read, tokens_cached_write, reasoning_tokens,
  tokens_saved, applied_saved, context_tokens_pre, context_tokens_saved)
SELECT substr(ts, 1, 10), COALESCE(account_id, 0), account, COALESCE(provider_key_id, 0), key_hint, client, alias, surface, model,
  COUNT(*), SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END),
  SUM(tokens_in), SUM(tokens_out), SUM(tokens_cached_read), SUM(tokens_cached_write), SUM(reasoning_tokens),
  SUM(tokens_saved), SUM(CASE WHEN compression_applied = 1 THEN tokens_saved ELSE 0 END),
  SUM(context_tokens_pre), SUM(context_tokens_saved)
FROM calls WHERE `+eligible+`
GROUP BY 1, 2, 3, 4, 5, 6, 7, 8, 9
ON CONFLICT(day, account_id, account, provider_key_id, key_hint, client, alias, surface, model) DO UPDATE SET
  calls = calls + excluded.calls, errors = errors + excluded.errors,
  tokens_in = tokens_in + excluded.tokens_in, tokens_out = tokens_out + excluded.tokens_out,
  tokens_cached_read = tokens_cached_read + excluded.tokens_cached_read,
  tokens_cached_write = tokens_cached_write + excluded.tokens_cached_write,
  reasoning_tokens = reasoning_tokens + excluded.reasoning_tokens,
  tokens_saved = tokens_saved + excluded.tokens_saved, applied_saved = applied_saved + excluded.applied_saved,
  context_tokens_pre = context_tokens_pre + excluded.context_tokens_pre,
  context_tokens_saved = context_tokens_saved + excluded.context_tokens_saved`, start, end); err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, `DELETE FROM calls WHERE `+eligible, start, end)
			if err != nil {
				return err
			}
			n, _ = res.RowsAffected()
			return nil
		})
		if err != nil {
			return days, rows, err
		}
		days++
		rows += n
	}
	return days, rows, nil
}
