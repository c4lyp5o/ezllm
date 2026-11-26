package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// ── keys ────────────────────────────────────────────────────────────────────

// MarkKeyTest records a key test outcome (the server only calls this AFTER a
// pass on first-add; retests may record failures against a live row).
func (d *DB) MarkKeyTest(ctx context.Context, keyID int64, ok bool, detail string) error {
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
UPDATE provider_keys SET last_test_at=strftime('%Y-%m-%dT%H:%M:%fZ','now'),
       last_test_ok=?, last_test_detail=? WHERE id=?`,
			boolToInt(ok), detail, keyID)
		return err
	})
}

// ListKeys returns an account's keys (hints only — plaintext never leaves
// the store/API boundary).
func (d *DB) ListKeys(ctx context.Context, accountID int64) ([]KeySummary, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT id, label, key_hint, enabled, last_test_at, last_test_ok,
       COALESCE(last_test_detail,''), added_at
FROM provider_keys WHERE account_id=? ORDER BY id`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KeySummary{}
	for rows.Next() {
		var k KeySummary
		var lastTest sql.NullString
		var ok sql.NullBool
		if err := rows.Scan(&k.ID, &k.Label, &k.Hint, &k.Enabled, &lastTest, &ok,
			&k.LastTestDetail, &k.AddedAt); err != nil {
			return nil, err
		}
		k.LastTestAt = lastTest.String
		if ok.Valid {
			v := ok.Bool
			k.LastTestOK = &v
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// KeyForTest reads a stored key so a retest can run against the provider.
// The plaintext returns to memory only — never to a response body.
func (d *DB) KeyForTest(ctx context.Context, keyID int64) (accountID int64, plaintext string, err error) {
	row := d.r.QueryRowContext(ctx, `
SELECT account_id, key_plain FROM provider_keys WHERE id=?`, keyID)
	if err := row.Scan(&accountID, &plaintext); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, "", ErrNotFound
		}
		return 0, "", err
	}
	if d.crypto != nil && strings.HasPrefix(plaintext, cryptoPrefix) {
		plaintext, err = d.crypto.Decrypt(plaintext)
		if err != nil {
			return 0, "", err
		}
	}
	return accountID, plaintext, nil
}

// DeleteKey removes one credential.
func (d *DB) DeleteKey(ctx context.Context, accountID, keyID int64) error {
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM provider_keys WHERE id=? AND account_id=?`, keyID, accountID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// EnableKey toggles a credential without deleting it.
func (d *DB) EnableKey(ctx context.Context, accountID, keyID int64, enabled bool) error {
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE provider_keys SET enabled=? WHERE id=? AND account_id=?`, boolToInt(enabled), keyID, accountID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ── models + protocol verdicts ──────────────────────────────────────────────

// ModelEntryRow is one catalog row with its three-valued protocol state.
type ModelEntryRow struct {
	ID        string  `json:"id"`
	Display   string  `json:"display,omitempty"`
	OwnedBy   string  `json:"owned_by,omitempty"`
	Chat      *bool   `json:"chat"`
	Anthropic *bool   `json:"anthropic"`
	Responses *bool   `json:"responses"`
	TestedAt  *string `json:"tested_at,omitempty"`
	// Pin is the operator-chosen surface ("" = auto). Enforced by the router.
	Pin string `json:"pin"`
	// Source is who last wrote the verdicts: "probe", "curated", or "" for a
	// legacy row nobody has attributed yet. Curated rows survive probe sweeps.
	Source string `json:"source,omitempty"`
	// Note is the operator's reason for a curation (curated rows only).
	Note string `json:"note,omitempty"`
}

// ListModels returns an account's catalog with protocol verdicts.
func (d *DB) ListModels(ctx context.Context, accountID int64) ([]ModelEntryRow, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT model_id, COALESCE(display_name,''), COALESCE(owned_by,''),
       proto_chat, proto_anthropic, proto_responses, proto_tested_at, COALESCE(proto_pin,''),
       COALESCE(proto_source,''), COALESCE(proto_note,'')
FROM models WHERE account_id=? ORDER BY model_id`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelEntryRow{}
	for rows.Next() {
		var m ModelEntryRow
		var o, a, r sql.NullBool
		var tested sql.NullString
		if err := rows.Scan(&m.ID, &m.Display, &m.OwnedBy, &o, &a, &r, &tested, &m.Pin, &m.Source, &m.Note); err != nil {
			return nil, err
		}
		if o.Valid {
			v := o.Bool
			m.Chat = &v
		}
		if a.Valid {
			v := a.Bool
			m.Anthropic = &v
		}
		if r.Valid {
			v := r.Bool
			m.Responses = &v
		}
		if tested.Valid && tested.String != "" {
			m.TestedAt = &tested.String
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetModelProto records one probe verdict. nil → NULL (untested), which is a
// first-class state: "we could not ask" must never render as "unsupported".
// A curated row is left alone: the operator's curation outranks a probe until
// released via SetModelCurated, so a sweep can never silently undo one.
func (d *DB) SetModelProto(ctx context.Context, accountID int64, modelID string, s provider.Surface, supported *bool) error {
	return d.SetModelProtos(ctx, accountID, map[string]map[string]*bool{modelID: {string(s): supported}})
}

// protoUpsertSQL upserts one capability column for (account_id, model_id): the
// sweep may probe a model the catalog sync has not inserted yet (or vice
// versa), and a curated row is never overwritten. col comes only from
// protoColumn, never from input.
func protoUpsertSQL(col string) string {
	return fmt.Sprintf(`
INSERT INTO models (account_id, model_id, %s, proto_tested_at, proto_source)
VALUES (?,?,?,strftime('%%Y-%%m-%%dT%%H:%%M:%%fZ','now'),'probe')
ON CONFLICT(account_id, model_id) DO UPDATE SET
  %s=excluded.%s, proto_tested_at=excluded.proto_tested_at, proto_source='probe'
WHERE COALESCE(proto_source,'') <> 'curated'`, col, col, col)
}

// SetModelProtos applies a whole sweep in ONE transaction — the common case
// after a background protocol probe, and the reason the sweep does not take
// the write lock 108 times. Service surfaces keep their verdicts in
// service_probes and are skipped here (they used to fall through to the chat
// proto_chat column).
func (d *DB) SetModelProtos(ctx context.Context, accountID int64, rows map[string]map[string]*bool) error {
	if len(rows) == 0 {
		return nil
	}
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		stmts := map[string]*sql.Stmt{}
		defer func() {
			for _, s := range stmts {
				s.Close()
			}
		}()
		for modelID, perSurface := range rows {
			for surf, val := range perSurface {
				col, ok := protoColumn(provider.Surface(surf))
				if !ok {
					continue
				}
				st, ok := stmts[col]
				if !ok {
					var err error
					if st, err = tx.PrepareContext(ctx, protoUpsertSQL(col)); err != nil {
						return err
					}
					stmts[col] = st
				}
				if _, err := st.ExecContext(ctx, accountID, modelID, boolPtrToInt(val)); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// ProtoEdit is one surface's three-state edit on a curated row: Present=false
// keeps the recorded verdict, Value=nil clears it to untested, Value non-nil
// records the verdict.
type ProtoEdit struct {
	Present bool
	Value   *bool
}

// SetModelCurated records operator-curated protocol verdicts with provenance.
//
// release=false stamps proto_source='curated' with the note and clears
// proto_tested_at — nothing was probed, so no probe timestamp is fabricated.
// Until released, probe writes (SetModelProto/Protos) skip the row entirely.
// release=true hands the row back to the sweep: verdicts stay as recorded,
// source and note clear, and the next probe write applies again.
//
// ErrNotFound when the model row does not exist: curation never creates rows.
func (d *DB) SetModelCurated(ctx context.Context, accountID int64, modelID, note string, edits map[string]ProtoEdit, release bool) error {
	cols := map[string]string{"chat": "proto_chat", "anthropic": "proto_anthropic", "responses": "proto_responses"}
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var res sql.Result
		var err error
		if release {
			res, err = tx.ExecContext(ctx,
				`UPDATE models SET proto_source=NULL, proto_note=NULL
WHERE account_id=? AND model_id=?`, accountID, modelID)
		} else {
			set := []string{}
			args := []any{}
			for surface, edit := range edits {
				col, ok := cols[surface]
				if !ok {
					return fmt.Errorf("store: unknown surface %q", surface)
				}
				if !edit.Present {
					continue
				}
				set = append(set, col+"=?")
				args = append(args, boolPtrToInt(edit.Value))
			}
			set = append(set, "proto_source='curated'", "proto_note=?", "proto_tested_at=NULL")
			args = append(args, note, accountID, modelID)
			res, err = tx.ExecContext(ctx,
				`UPDATE models SET `+strings.Join(set, ", ")+` WHERE account_id=? AND model_id=?`, args...)
		}
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// InsertQuotaSnapshot stores one immutable quota read.
func (d *DB) InsertQuotaSnapshot(ctx context.Context, keyID int64, q provider.Quota, ok bool, errMsg string) error {
	kind := string(q.Kind)
	if kind == "" {
		if ok {
			kind = "none"
		} else {
			kind = "error"
		}
	}
	resets := q.RollingResetsAt
	if resets == "" {
		resets = q.WeeklyResetsAt
	}
	if resets == "" {
		resets = q.MonthlyResetsAt
	}
	raw := q.Raw
	if raw == "" {
		raw = "{}"
	}
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
INSERT INTO quota_snapshots
  (provider_key_id, kind, percent_rolling, percent_weekly, percent_monthly,
   balance, balance_unit, cost_total, cost_today, tokens_today, requests_today,
   resets_at, raw, ok, err)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			keyID, kind, q.PercentRolling, q.PercentWeekly, q.PercentMonthly,
			q.Balance, nullIfEmpty(q.BalanceUnit), q.CostTotal, q.CostToday,
			i64OrNil(q.TokensToday), i64OrNil(q.ReqsToday),
			nullIfEmpty(resets), raw, boolToInt(ok), nullIfEmpty(errMsg))
		return err
	})
}

// PruneQuotaSnapshots removes quota reads older than keep, but never a key's
// newest read: the provider card shows it however old it is. The hourly
// alert check reads quotas, so without this the table only grows.
func (d *DB) PruneQuotaSnapshots(ctx context.Context, keep time.Duration) (int64, error) {
	cutoff := time.Now().Add(-keep).UTC().Format("2006-01-02T15:04:05.000Z")
	var n int64
	err := d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
DELETE FROM quota_snapshots WHERE read_at < ? AND id NOT IN (
  SELECT (SELECT id FROM quota_snapshots q2 WHERE q2.provider_key_id = k.provider_key_id
          ORDER BY read_at DESC, id DESC LIMIT 1)
  FROM (SELECT DISTINCT provider_key_id FROM quota_snapshots) k)`, cutoff)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n, err
}

// LatestQuotas returns the newest snapshot per key (the poller's read side).
func (d *DB) LatestQuotas(ctx context.Context, keyIDs []int64) ([]map[string]any, error) {
	q := `
SELECT k.id, k.account_id, k.label, k.key_hint,
       -- LEFT JOIN: a never-probed key has all-NULL quota columns, and kind/
       -- read_at/ok scan into plain string/bool. No snapshot => '' / false.
       COALESCE(q.kind,''), q.percent_rolling, q.percent_weekly, q.percent_monthly,
       q.balance, q.balance_unit, q.cost_total, q.cost_today,
       q.tokens_today, q.requests_today,
       q.resets_at,
       COALESCE(q.read_at,''), COALESCE(q.ok,0)
FROM provider_keys k
LEFT JOIN quota_snapshots q ON q.id = (
  SELECT id FROM quota_snapshots WHERE provider_key_id = k.id
  ORDER BY read_at DESC, id DESC LIMIT 1)`
	var args []any
	if len(keyIDs) > 0 {
		ph := make([]string, len(keyIDs))
		for i, id := range keyIDs {
			ph[i] = "?"
			args = append(args, id)
		}
		q += " WHERE k.id IN (" + strings.Join(ph, ",") + ")"
	}
	q += " ORDER BY k.account_id, k.id"

	rows, err := d.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var keyID, acctID int64
		var label, hint, kind, readAt string
		var pr, pw, pm, bal, ct, cd sql.NullFloat64
		var tokToday, reqToday sql.NullInt64
		var unit, resets sql.NullString
		var ok bool
		if err := rows.Scan(&keyID, &acctID, &label, &hint, &kind, &pr, &pw, &pm,
			&bal, &unit, &ct, &cd, &tokToday, &reqToday, &resets, &readAt, &ok); err != nil {
			return nil, err
		}
		item := map[string]any{
			"key_id": keyID, "account_id": acctID, "label": label, "hint": hint,
			"kind": kind, "read_at": readAt, "ok": ok,
			"percent_rolling": nullFloat(pr), "percent_weekly": nullFloat(pw),
			"percent_monthly": nullFloat(pm), "balance": nullFloat(bal),
			"unit": nullStr(unit), "cost_total": nullFloat(ct),
			"cost_today":   nullFloat(cd),
			"tokens_today": nullInt(tokToday), "requests_today": nullInt(reqToday),
			"resets_at": nullStr(resets),
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
