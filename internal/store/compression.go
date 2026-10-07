package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ── compression profiles (M5: full CRUD; the combo editor reads the same row) ──

const profileSelect = `SELECT id, name, enabled, stages, exempt_last_turn,
       min_compress_ratio, fail_open, auto_trigger_tokens,
       COALESCE(notes,''), created_at FROM compression_profiles`

func scanProfile(rows *sql.Rows) (*CompressionProfile, error) {
	var p CompressionProfile
	var stages string
	if err := rows.Scan(&p.ID, &p.Name, &p.Enabled, &stages, &p.ExemptLastTurn,
		&p.MinCompressRatio, &p.FailOpen, &p.AutoTriggerTokens, &p.Notes, &p.CreatedAt); err != nil {
		return nil, err
	}
	p.Stages = json.RawMessage(stages)
	return &p, nil
}

// GetCompressionProfile returns one profile by id.
func (d *DB) GetCompressionProfile(ctx context.Context, id int64) (*CompressionProfile, error) {
	rows, err := d.r.QueryContext(ctx, profileSelect+` WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, ErrNotFound
	}
	return scanProfile(rows)
}

// CompressionProfileByName resolves the request-header lookup path.
func (d *DB) CompressionProfileByName(ctx context.Context, name string) (*CompressionProfile, error) {
	rows, err := d.r.QueryContext(ctx, profileSelect+` WHERE name = ?`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, ErrNotFound
	}
	return scanProfile(rows)
}

// UpsertCompressionProfile inserts (ID==0) or updates an existing row.
// Stages must already be validated JSON (server layer validates the pipeline).
func (d *DB) UpsertCompressionProfile(ctx context.Context, p CompressionProfile) (int64, error) {
	if strings.TrimSpace(p.Name) == "" {
		return 0, errors.New("store: profile name is required")
	}
	stages := string(p.Stages)
	if stages == "" {
		stages = "[]"
	}
	if !json.Valid([]byte(stages)) {
		return 0, errors.New("store: stages must be JSON")
	}
	var id int64
	err := d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if p.ID == 0 {
			res, err := tx.ExecContext(ctx, `
INSERT INTO compression_profiles
  (name, enabled, stages, exempt_last_turn, min_compress_ratio, fail_open, auto_trigger_tokens, notes)
VALUES (?,?,?,?,?,?,?,?)`,
				p.Name, p.Enabled, stages, p.ExemptLastTurn, p.MinCompressRatio,
				p.FailOpen, p.AutoTriggerTokens, p.Notes)
			if err != nil {
				return err
			}
			id, err = res.LastInsertId()
			return err
		}
		_, err := tx.ExecContext(ctx, `
UPDATE compression_profiles SET
  name=?, enabled=?, stages=?, exempt_last_turn=?, min_compress_ratio=?,
  fail_open=?, auto_trigger_tokens=?, notes=?
WHERE id=?`,
			p.Name, p.Enabled, stages, p.ExemptLastTurn, p.MinCompressRatio,
			p.FailOpen, p.AutoTriggerTokens, p.Notes, p.ID)
		if err != nil {
			return err
		}
		id = p.ID
		return nil
	})
	return id, err
}

// DeleteCompressionProfile refuses while any combo still points at it —
// silently nulling a combo's pipeline would change routing behavior without
// anyone asking for it.
func (d *DB) DeleteCompressionProfile(ctx context.Context, id int64) error {
	var refs int
	if err := d.r.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM combos WHERE compression_profile_id = ?`, id).Scan(&refs); err != nil {
		return err
	}
	if refs > 0 {
		return fmt.Errorf("store: profile is referenced by %d combo(s); detach first", refs)
	}
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM compression_profiles WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}
