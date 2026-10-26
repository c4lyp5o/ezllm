package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/c4lyp5o/ezllm/internal/settings"
)

func (d *DB) LoadSettings(ctx context.Context) (settings.State, error) {
	var s settings.State
	var raw string
	err := d.r.QueryRowContext(ctx, `SELECT revision, values_json FROM instance_settings WHERE id=1`).Scan(&s.Revision, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		s.Values = map[string]json.RawMessage{}
		return s, nil
	}
	if err != nil {
		return s, err
	}
	err = json.Unmarshal([]byte(raw), &s.Values)
	return s, err
}
func (d *DB) SaveSettings(ctx context.Context, s settings.State, revision int64) error {
	raw, err := json.Marshal(s.Values)
	if err != nil {
		return err
	}
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO instance_settings(id,revision,values_json) VALUES(1,0,'{}') ON CONFLICT DO NOTHING`)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE instance_settings SET revision=?,values_json=? WHERE id=1 AND revision=?`, s.Revision, string(raw), revision)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return settings.ErrConflict
		}
		return nil
	})
}

// InheritedRuleUsage uses ledger instants rather than counters whose bucket
// names would otherwise conflate the old and new instance timezones.
func (d *DB) InheritedRuleUsage(ctx context.Context, account int64, model, window string, now time.Time, loc *time.Location) (int64, error) {
	if err := d.ensureMeter(ctx); err != nil {
		return 0, err
	}
	start, end := BucketRange(now, window, loc)
	var used int64
	err := d.r.QueryRowContext(ctx, `SELECT COALESCE(SUM(tokens_in+tokens_out),0) FROM calls WHERE account_id=? AND model=? AND ts>=? AND ts<?`, account, model, start.UTC().Format("2006-01-02T15:04:05.000Z"), end.UTC().Format("2006-01-02T15:04:05.000Z")).Scan(&used)
	return used, err
}

func (d *DB) SetOperatorSettings(m *settings.Manager) { d.operatorSettings.Store(m) }

type JobHealth struct {
	LastSuccess string `json:"last_success,omitempty"`
	LastFailure string `json:"last_failure,omitempty"`
}

func (d *DB) JobHealth(ctx context.Context) (map[string]JobHealth, error) {
	rows, err := d.r.QueryContext(ctx, `SELECT kind,last_success,last_failure FROM instance_job_health`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]JobHealth{}
	for rows.Next() {
		var kind string
		var h JobHealth
		if err := rows.Scan(&kind, &h.LastSuccess, &h.LastFailure); err != nil {
			return nil, err
		}
		out[kind] = h
	}
	return out, rows.Err()
}
func (d *DB) SetJobHealth(ctx context.Context, kind string, h JobHealth) error {
	_, err := d.w.ExecContext(ctx, `INSERT INTO instance_job_health(kind,last_success,last_failure) VALUES(?,?,?) ON CONFLICT(kind) DO UPDATE SET last_success=excluded.last_success,last_failure=excluded.last_failure`, kind, h.LastSuccess, h.LastFailure)
	return err
}
