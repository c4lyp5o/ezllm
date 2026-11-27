package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ── combos ──────────────────────────────────────────────────────────────────

// ComboHop is one ordered hop in a combo. Combos have NO namespace — they are
// addressed by bare name, and they do the hopping between explicit hops.
type ComboHop struct {
	AccountID int64  `json:"account_id"`
	ModelID   string `json:"model_id"`
	// Weight is kept for API compatibility only: routing never used it, so it
	// is accepted on input and always stored and reported as 1.
	Weight  int   `json:"weight"`
	Enabled *bool `json:"enabled"`
	// HoldUntilFailure (block_round_robin only): the hop keeps the block while
	// it succeeds and releases it on a hop-attributable failure. Nullable so a
	// PATCH that omits it keeps the stored value.
	HoldUntilFailure *bool `json:"hold_until_failure,omitempty"`
}

// Combo is the admin-API combo shape.
type Combo struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Strategy    string `json:"strategy"`
	StickyIdleS int    `json:"sticky_idle_s"`
	// BlockSize is how many consecutive dispatches one hop serves before the
	// rotation advances (block_round_robin). 1 = plain per-request round robin.
	BlockSize            int    `json:"block_size"`
	Enabled              bool   `json:"enabled"`
	CompressionProfileID *int64 `json:"compression_profile_id"`
	ContextSize          int64  `json:"context_size"`
	Notes                string `json:"notes,omitempty"`
	// AutoFree makes the hop list derived: every free model in the catalogs
	// (see autofree.go), optionally limited to AutoFreeAccounts. Hops written
	// by hand are replaced on the next sync, except their enabled/hold flags.
	AutoFree         bool    `json:"auto_free"`
	AutoFreeAccounts []int64 `json:"auto_free_accounts"`
	// AutoFreeInclude lists models that join the pool even without a free
	// marker in their id (promotions, account-level free tiers).
	AutoFreeInclude []FreeInclude `json:"auto_free_include"`
	CreatedAt       string        `json:"created_at"`
	Hops            []ComboHop    `json:"hops"`
}

// ValidStrategies is the single source of truth for strategy names.
//
// This list IS the validation: the schema deliberately carries no CHECK on
// `strategy`, because SQLite cannot alter a constraint, so a CHECK would make
// every new strategy a full table rebuild (as block_round_robin was).
func ValidStrategies() []string {
	return []string{"failover", "true_round_robin", "sticky_last_good", "least_used", "block_round_robin"}
}

// ValidateCombo checks the payload before writing (422 on failure).
func ValidateCombo(c Combo) error {
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("name is required")
	}
	if c.ContextSize <= 0 || c.ContextSize > 10000000 {
		return errors.New("context_size must be between 1 and 10000000 tokens")
	}
	if strings.Contains(c.Name, "/") {
		return errors.New("combo name must not contain '/' — combos have no namespace")
	}
	valid := false
	for _, s := range ValidStrategies() {
		if c.Strategy == s {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("strategy must be one of %v", ValidStrategies())
	}
	if c.StickyIdleS < 0 {
		return errors.New("sticky_idle_s must be >= 0")
	}
	// 0 is accepted as "unset" (the JSON zero value); UpsertCombo normalises it
	// to 1 before writing, which is also what the column CHECK allows.
	if c.BlockSize < 0 || c.BlockSize > 1000 {
		return errors.New("block_size must be between 1 and 1000")
	}
	for i, h := range c.Hops {
		if h.AccountID <= 0 {
			return fmt.Errorf("hops[%d]: account_id is required", i)
		}
		if strings.TrimSpace(h.ModelID) == "" {
			return fmt.Errorf("hops[%d]: model_id is required", i)
		}
	}
	return nil
}

// ListCombos returns all combos with their ordered hops.
func (d *DB) ListCombos(ctx context.Context) ([]Combo, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT id, name, strategy, sticky_idle_s, enabled, compression_profile_id, context_size,
       block_size, COALESCE(notes,''), created_at, auto_free, auto_free_accounts, auto_free_include
FROM combos ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Combo{}
	byID := map[int64]int{}
	for rows.Next() {
		var c Combo
		var cpid sql.NullInt64
		var autoAccts, autoIncl string
		if err := rows.Scan(&c.ID, &c.Name, &c.Strategy, &c.StickyIdleS, &c.Enabled,
			&cpid, &c.ContextSize, &c.BlockSize, &c.Notes, &c.CreatedAt, &c.AutoFree, &autoAccts, &autoIncl); err != nil {
			return nil, err
		}
		c.AutoFreeAccounts = parseAccountIDs(autoAccts)
		c.AutoFreeInclude = parseFreeIncludes(autoIncl)
		if c.ContextSize == 0 {
			c.ContextSize = 200000
		}
		if cpid.Valid {
			v := cpid.Int64
			c.CompressionProfileID = &v
		}
		c.Hops = []ComboHop{}
		byID[c.ID] = len(out)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	hrows, err := d.r.QueryContext(ctx, `
SELECT combo_id, account_id, model_id, weight, enabled, hold_until_failure
FROM combo_hops ORDER BY combo_id, position`)
	if err != nil {
		return nil, err
	}
	defer hrows.Close()
	for hrows.Next() {
		var comboID int64
		var h ComboHop
		var hold int
		if err := hrows.Scan(&comboID, &h.AccountID, &h.ModelID, &h.Weight, &h.Enabled, &hold); err != nil {
			return nil, err
		}
		h.Weight = 1 // rows saved before weights were removed may hold more
		if idx, ok := byID[comboID]; ok {
			if hold != 0 {
				held := true
				h.HoldUntilFailure = &held
			}
			out[idx].Hops = append(out[idx].Hops, h)
		}
	}
	return out, hrows.Err()
}

// UpsertCombo creates or replaces a combo by name (idempotent POST per the
// API contract). Hops replace the whole ordered list; positions = array order.
func (d *DB) UpsertCombo(ctx context.Context, c Combo) (int64, error) {
	if c.ContextSize == 0 {
		c.ContextSize = 200000
	}
	if c.BlockSize == 0 {
		c.BlockSize = 1
	}
	if err := ValidateCombo(c); err != nil {
		return 0, err
	}
	var id int64
	err := d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var cpid any
		if c.CompressionProfileID != nil {
			cpid = *c.CompressionProfileID
		}
		_, err := tx.ExecContext(ctx, `
INSERT INTO combos (name, strategy, sticky_idle_s, enabled, compression_profile_id, context_size, block_size, notes,
                    auto_free, auto_free_accounts, auto_free_include)
VALUES (?,?,?,?,COALESCE(?, (SELECT id FROM compression_profiles WHERE 0)),?,?,?,?,?,?)
ON CONFLICT(name) DO UPDATE SET
  strategy=excluded.strategy, sticky_idle_s=excluded.sticky_idle_s,
  enabled=excluded.enabled, compression_profile_id=excluded.compression_profile_id,
  context_size=excluded.context_size, block_size=excluded.block_size, notes=excluded.notes,
  auto_free=excluded.auto_free, auto_free_accounts=excluded.auto_free_accounts,
  auto_free_include=excluded.auto_free_include`,
			c.Name, c.Strategy, c.StickyIdleS, boolToInt(c.Enabled), cpid, c.ContextSize, c.BlockSize, c.Notes,
			boolToInt(c.AutoFree), formatAccountIDs(c.AutoFreeAccounts), formatFreeIncludes(c.AutoFreeInclude))
		if err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT id FROM combos WHERE name=?`, c.Name).Scan(&id); err != nil {
			return err
		}
		if err := d.replaceHops(ctx, tx, id, c.Hops); err != nil {
			return err
		}
		return d.syncAutoFree(ctx, tx, id)
	})
	return id, err
}

// UpdateCombo saves combo id, renaming it when c.Name differs. UpsertCombo keys
// on the name, so an edit that renamed a combo created a second one and left
// the old one in place. A name another combo holds is a conflict (409).
func (d *DB) UpdateCombo(ctx context.Context, id int64, c Combo) error {
	if c.ContextSize == 0 {
		c.ContextSize = 200000
	}
	if c.BlockSize == 0 {
		c.BlockSize = 1
	}
	if err := ValidateCombo(c); err != nil {
		return err
	}
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var other int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM combos WHERE name=? AND id<>?`, c.Name, id).Scan(&other)
		if err == nil {
			return &ErrConflict{What: "a combo named " + c.Name + " already exists"}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var cpid any
		if c.CompressionProfileID != nil {
			cpid = *c.CompressionProfileID
		}
		res, err := tx.ExecContext(ctx, `
UPDATE combos SET name=?, strategy=?, sticky_idle_s=?, enabled=?, compression_profile_id=?,
  context_size=?, block_size=?, notes=?, auto_free=?, auto_free_accounts=?, auto_free_include=?
WHERE id=?`,
			c.Name, c.Strategy, c.StickyIdleS, boolToInt(c.Enabled), cpid, c.ContextSize, c.BlockSize, c.Notes,
			boolToInt(c.AutoFree), formatAccountIDs(c.AutoFreeAccounts), formatFreeIncludes(c.AutoFreeInclude), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if err := d.replaceHops(ctx, tx, id, c.Hops); err != nil {
			return err
		}
		return d.syncAutoFree(ctx, tx, id)
	})
}

// ReplaceHops rewrites a combo's ordered hop list (POST /admin/combos/{id}/hops).
func (d *DB) ReplaceHops(ctx context.Context, comboID int64, hops []ComboHop) error {
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM combos WHERE id=?`, comboID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return ErrNotFound
		}
		if err := d.replaceHops(ctx, tx, comboID, hops); err != nil {
			return err
		}
		return d.syncAutoFree(ctx, tx, comboID)
	})
}

func (d *DB) replaceHops(ctx context.Context, tx *sql.Tx, comboID int64, hops []ComboHop) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM combo_hops WHERE combo_id=?`, comboID); err != nil {
		return err
	}
	for i, h := range hops {
		// A hop pointing at a missing account would just vanish at runtime —
		// reject it at write time instead ("store: " prefix => 422 in writeStoreErr).
		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM accounts WHERE id=?`, h.AccountID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return fmt.Errorf("store: hops[%d]: account_id %d does not exist", i, h.AccountID)
		}
		enabled := true
		if h.Enabled != nil {
			enabled = *h.Enabled
		}
		const weight = 1 // see ComboHop.Weight: accepted, never stored as given
		held := h.HoldUntilFailure != nil && *h.HoldUntilFailure
		if _, err := tx.ExecContext(ctx, `
INSERT INTO combo_hops (combo_id, position, account_id, model_id, weight, enabled, hold_until_failure)
VALUES (?,?,?,?,?,?,?)`, comboID, i, h.AccountID, h.ModelID, weight, boolToInt(enabled), boolToInt(held)); err != nil {
			return err
		}
	}
	return nil
}

// DeleteCombo removes a combo (hops cascade).
func (d *DB) DeleteCombo(ctx context.Context, id int64) error {
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM combos WHERE id=?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// AllComboNames lists enabled combo names — used for 404 hints, so a typo'd
// combo name tells the user what combos DO exist.
func (d *DB) AllComboNames(ctx context.Context) ([]string, error) {
	rows, err := d.r.QueryContext(ctx, `SELECT name FROM combos WHERE enabled = 1 ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ComboByName resolves a bare combo name (the router's combo entry point).
//
// This runs on the INFERENCE hot path — one query for the combo plus one for
// its hops, both indexed — rather than ListCombos, which pulls every combo and
// every hop in the database just to return one of them.
func (d *DB) ComboByName(ctx context.Context, name string) (*Combo, error) {
	row := d.r.QueryRowContext(ctx, `
SELECT id, name, strategy, sticky_idle_s, enabled, compression_profile_id, context_size,
       block_size, COALESCE(notes,''), created_at
FROM combos WHERE name = ?`, name)
	var c Combo
	var cpid sql.NullInt64
	if err := row.Scan(&c.ID, &c.Name, &c.Strategy, &c.StickyIdleS, &c.Enabled,
		&cpid, &c.ContextSize, &c.BlockSize, &c.Notes, &c.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if cpid.Valid {
		v := cpid.Int64
		c.CompressionProfileID = &v
	}
	c.Hops = []ComboHop{}

	hrows, err := d.r.QueryContext(ctx, `
SELECT account_id, model_id, weight, enabled, hold_until_failure
FROM combo_hops WHERE combo_id = ? ORDER BY position`, c.ID)
	if err != nil {
		return nil, err
	}
	defer hrows.Close()
	for hrows.Next() {
		var h ComboHop
		var hold int
		if err := hrows.Scan(&h.AccountID, &h.ModelID, &h.Weight, &h.Enabled, &hold); err != nil {
			return nil, err
		}
		h.Weight = 1 // rows saved before weights were removed may hold more
		if hold != 0 {
			held := true
			h.HoldUntilFailure = &held
		}
		c.Hops = append(c.Hops, h)
	}
	return &c, hrows.Err()
}
