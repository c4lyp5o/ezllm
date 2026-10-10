package store

// v8 (M9) model prices + derived cost. Money was the ledger's blind spot:
// every "dollar savings" claim had to be hand-computed from a paper's pricing
// appendix. Rates live in model_prices, keyed (account, model) because the
// same model costs different money through different resellers, and versioned
// by effective_from because providers reprice — a price change is a NEW row,
// never an UPDATE, so a January query can still reconstruct January's bill.
//
// Cost is DERIVED at query time (never stored per row): if it were stamped at
// insert, seeding or correcting a price would silently fail to fix history,
// and an operator correcting one cell in the dashboard would be changing the
// past by rewriting one number. The join picks, for each call, the newest
// price row effective on or before the call's date.
//
// Pricing honesty: calls whose (account, model) has no effective price row
// count as UNPRICED, are excluded from USD, and are reported as a count. A
// cost that quietly treats the unknown as zero is worse than no cost.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Price is one versioned rate row (USD per 1M tokens, matching the column
// comments in schema.sql).
type Price struct {
	AccountID       int64   `json:"account_id"`
	ModelID         string  `json:"model_id"`
	EffectiveFrom   string  `json:"effective_from"` // YYYY-MM-DD
	Currency        string  `json:"currency"`
	PriceIn         float64 `json:"price_in"`
	PriceOut        float64 `json:"price_out"`
	PriceCacheRead  float64 `json:"price_cache_read"`
	PriceCacheWrite float64 `json:"price_cache_write"`
	Note            string  `json:"note,omitempty"`
}

// validPriceDate accepts YYYY-MM-DD (and normalizes a full RFC3339 timestamp
// to its date prefix, because clients will paste one).
func validPriceDate(s string) (string, bool) {
	if len(s) >= 10 {
		if t, err := time.Parse("2006-01-02", s[:10]); err == nil {
			return t.Format("2006-01-02"), true
		}
	}
	return "", false
}

// SetPrice upserts one rate row. Negative rates are refused: they would make
// savings calculators produce fictional positive "savings" out of thin air.
func (d *DB) SetPrice(ctx context.Context, p Price) error {
	if p.AccountID == 0 {
		return fmt.Errorf("store: price needs account_id")
	}
	if strings.TrimSpace(p.ModelID) == "" {
		return fmt.Errorf("store: price needs model_id")
	}
	day, ok := validPriceDate(p.EffectiveFrom)
	if !ok {
		return fmt.Errorf("store: price effective_from must be YYYY-MM-DD, got %q", p.EffectiveFrom)
	}
	p.EffectiveFrom = day
	if p.Currency == "" {
		p.Currency = "USD"
	}
	for name, v := range map[string]float64{"price_in": p.PriceIn, "price_out": p.PriceOut, "price_cache_read": p.PriceCacheRead, "price_cache_write": p.PriceCacheWrite} {
		if v < 0 {
			return fmt.Errorf("store: price %s is negative (%v)", name, v)
		}
	}
	// WithWriteTx takes the writer mutex: d.w is a single connection and a
	// raw ExecContext would race with the ledger's own writes.
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
INSERT INTO model_prices (account_id, model_id, effective_from, currency,
  price_in, price_out, price_cache_read, price_cache_write, note, updated_at)
VALUES (?,?,?,?,?,?,?,?,?, strftime('%Y-%m-%dT%H:%M:%fZ','now'))
ON CONFLICT(account_id, model_id, effective_from) DO UPDATE SET
  currency=excluded.currency, price_in=excluded.price_in, price_out=excluded.price_out,
  price_cache_read=excluded.price_cache_read, price_cache_write=excluded.price_cache_write,
  note=excluded.note, updated_at=excluded.updated_at`,
			p.AccountID, p.ModelID, p.EffectiveFrom, p.Currency,
			p.PriceIn, p.PriceOut, p.PriceCacheRead, p.PriceCacheWrite, p.Note)
		if err != nil {
			return fmt.Errorf("store: set price: %w", err)
		}
		return nil
	})
}

// ListPrices returns every rate row, newest-effective first per model.
func (d *DB) ListPrices(ctx context.Context) ([]Price, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT account_id, model_id, effective_from, currency,
       price_in, price_out, price_cache_read, price_cache_write, COALESCE(note,'')
FROM model_prices ORDER BY model_id, account_id, effective_from DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Price{}
	for rows.Next() {
		var p Price
		if err := rows.Scan(&p.AccountID, &p.ModelID, &p.EffectiveFrom, &p.Currency,
			&p.PriceIn, &p.PriceOut, &p.PriceCacheRead, &p.PriceCacheWrite, &p.Note); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePrice removes one exact rate version (by account, model, effective date).
func (d *DB) DeletePrice(ctx context.Context, accountID int64, modelID, effectiveFrom string) error {
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM model_prices WHERE account_id=? AND model_id=? AND effective_from=?`,
			accountID, modelID, effectiveFrom)
		if err != nil {
			return fmt.Errorf("store: delete price: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
}

// CostRow is one (day, account, model) bucket of the derived cost report.
type CostRow struct {
	Day           string  `json:"day"`
	AccountID     int64   `json:"account_id"`
	Account       string  `json:"account"`
	Model         string  `json:"model"`
	Calls         int64   `json:"calls"`
	UnpricedCalls int64   `json:"unpriced_calls"` // NO price row at that date — see header note
	TokensIn      int64   `json:"tokens_in"`      // full-price input (excludes cached, per NormalizeUsage)
	TokensOut     int64   `json:"tokens_out"`
	CachedRead    int64   `json:"tokens_cached_read"`
	CachedWrite   int64   `json:"tokens_cached_write"`
	USD           float64 `json:"usd"` // priced calls only
}

// priceFor resolves the effective rate for one call: the newest row on or
// before the call's date. Correlated subqueries keep this readable; the table
// is tiny (tens of rows), so the plan is not a concern at ledger sizes here.
const priceJoin = `
WITH priced AS (
  SELECT c.id, c.ts, COALESCE(c.account_id,0) AS account_id, c.account, c.model,
         c.tokens_in, c.tokens_out, c.tokens_cached_read, c.tokens_cached_write,
         (SELECT p.price_in          FROM model_prices p
            WHERE p.account_id = c.account_id AND p.model_id = c.model
              AND p.effective_from <= date(c.ts)
            ORDER BY p.effective_from DESC LIMIT 1) AS pin,
         (SELECT p.price_out         FROM model_prices p
            WHERE p.account_id = c.account_id AND p.model_id = c.model
              AND p.effective_from <= date(c.ts)
            ORDER BY p.effective_from DESC LIMIT 1) AS pout,
         (SELECT p.price_cache_read  FROM model_prices p
            WHERE p.account_id = c.account_id AND p.model_id = c.model
              AND p.effective_from <= date(c.ts)
            ORDER BY p.effective_from DESC LIMIT 1) AS pcr,
         (SELECT p.price_cache_write FROM model_prices p
            WHERE p.account_id = c.account_id AND p.model_id = c.model
              AND p.effective_from <= date(c.ts)
            ORDER BY p.effective_from DESC LIMIT 1) AS pcw
  FROM calls c
  WHERE c.ts >= ? AND c.ts < ?
)`

// CostReport derives spend per (day, account, model) for [from,to).
func (d *DB) CostReport(ctx context.Context, from, to time.Time) ([]CostRow, error) {
	q := priceJoin + `
SELECT date(ts) AS day, account_id, account, model,
       COUNT(*)                                            AS calls,
       SUM(CASE WHEN pin IS NULL THEN 1 ELSE 0 END)        AS unpriced,
       COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0),
       COALESCE(SUM(tokens_cached_read),0), COALESCE(SUM(tokens_cached_write),0),
       COALESCE(SUM(CASE WHEN pin IS NOT NULL THEN
             tokens_in*pin + tokens_out*pout
           + tokens_cached_read*pcr + tokens_cached_write*pcw ELSE 0 END),0)/1e6 AS usd
FROM priced
GROUP BY day, account_id, account, model
ORDER BY day DESC, usd DESC`
	rows, err := d.r.QueryContext(ctx, q,
		from.UTC().Format("2006-01-02T15:04:05.000Z"), to.UTC().Format("2006-01-02T15:04:05.000Z"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CostRow{}
	for rows.Next() {
		var r CostRow
		var usd float64
		if err := rows.Scan(&r.Day, &r.AccountID, &r.Account, &r.Model, &r.Calls, &r.UnpricedCalls,
			&r.TokensIn, &r.TokensOut, &r.CachedRead, &r.CachedWrite, &usd); err != nil {
			return nil, err
		}
		r.USD = usd
		out = append(out, r)
	}
	return out, rows.Err()
}
