package store

import (
	"context"
	"time"
)

// ledgerTSLayout is the fixed-width UTC millisecond timestamp used in `calls`.
// Because it is fixed-width and zero-padded, text comparison IS chronological
// comparison — which is why the ledger's range queries can use >= on a string.
const ledgerTSLayout = "2006-01-02T15:04:05.000Z"

// accountRate parses a stored per-account request budget.
//
// A bad stored value degrades to "unlimited" rather than failing the read. The
// admin API validates on write, so an invalid value could only arrive from a
// manual DB edit — and refusing to load the ACCOUNT over it would take a
// working provider offline for a limit that was never valid. Degrading keeps
// the hop usable and simply leaves the limiter disengaged.
func accountRate(limit int64, window string) (int64, time.Duration) {
	if limit <= 0 || window == "" {
		return 0, 0
	}
	d, err := time.ParseDuration(window)
	if err != nil || d <= 0 {
		return 0, 0
	}
	return limit, d
}

// AccountRate is one account's request budget, as read for limiter seeding.
type AccountRate struct {
	AccountID int64
	Limit     int64
	Window    time.Duration
}

// AccountsWithRateLimit returns every account that has a request budget set,
// with its window already parsed.
//
// This is the read half of the proxy limiter's boot seed. The limiter itself
// holds no durable state (a reservation on the hot path must not touch SQLite),
// so without this a restart would hand a spent credential a fresh budget —
// exactly when a burst is most likely, since a restart is followed by a
// reconnect storm.
func (d *DB) AccountsWithRateLimit(ctx context.Context) ([]AccountRate, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT id, rate_limit, COALESCE(rate_window,'')
FROM accounts
WHERE COALESCE(rate_limit,0) > 0 AND COALESCE(rate_window,'') <> ''
ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AccountRate
	for rows.Next() {
		var (
			ar     AccountRate
			window string
		)
		if err := rows.Scan(&ar.AccountID, &ar.Limit, &window); err != nil {
			return nil, err
		}
		ar.Limit, ar.Window = accountRate(ar.Limit, window)
		if ar.Limit > 0 && ar.Window > 0 {
			out = append(out, ar)
		}
	}
	return out, rows.Err()
}

// CallTimesSince returns the dispatch times of an account's calls at or after
// `since`, oldest first, for seeding the limiter's window.
//
// Bounded by the caller's lookback (one rate window), so this reads at most a
// window's worth of rows once at boot — never on the request path.
func (d *DB) CallTimesSince(ctx context.Context, accountID int64, since time.Time) ([]time.Time, error) {
	rows, err := d.r.QueryContext(ctx,
		`SELECT ts FROM calls WHERE account_id = ? AND ts >= ? ORDER BY ts`,
		accountID, since.UTC().Format(ledgerTSLayout))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []time.Time
	for rows.Next() {
		var ts string
		if err := rows.Scan(&ts); err != nil {
			return nil, err
		}
		if t, err := time.Parse(ledgerTSLayout, ts); err == nil {
			out = append(out, t)
		}
	}
	return out, rows.Err()
}
