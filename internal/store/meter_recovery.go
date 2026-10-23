package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ensureMeter reconciles durable missed debits before any cap can admit more
// work. The normal path is an indexed existence check; writes occur only when
// a previous flush could persist calls but not advance its counters.
func (d *DB) ensureMeter(ctx context.Context) error {
	var pending bool
	if err := d.r.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pending_usage_meter)`).Scan(&pending); err != nil {
		return err
	}
	if !pending {
		return nil
	}
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error { return d.replayMeter(ctx, tx) })
}

func (d *DB) replayMeter(ctx context.Context, tx *sql.Tx) error {
	rs, err := tx.QueryContext(ctx, `SELECT c.ts,COALESCE(c.account_id,0),c.client,c.model,c.tokens_in,c.tokens_out,p.reservation_id FROM pending_usage_meter p JOIN calls c ON c.id=p.call_id ORDER BY p.call_id`)
	if err != nil {
		return err
	}
	var rows []Call
	for rs.Next() {
		var c Call
		var ts string
		if err = rs.Scan(&ts, &c.AccountID, &c.Client, &c.Model, &c.TokensIn, &c.TokensOut, &c.ReservationID); err != nil {
			rs.Close()
			return err
		}
		c.TS, err = time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			rs.Close()
			return err
		}
		rows = append(rows, c)
	}
	err = rs.Err()
	rs.Close()
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	if err = d.meterInTx(ctx, tx, rows); err != nil {
		return fmt.Errorf("reconcile usage meter: %w", err)
	}
	for _, c := range rows {
		if c.ReservationID != 0 {
			if _, err = tx.ExecContext(ctx, `DELETE FROM client_budget_reservations WHERE id=?`, c.ReservationID); err != nil {
				return err
			}
		}
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM pending_usage_meter`)
	return err
}
