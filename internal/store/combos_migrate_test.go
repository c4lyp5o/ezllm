package store

// v9 combos rebuild: adding a strategy value to the old CHECK constraint is
// impossible in SQLite without rebuilding the table, so the migration rebuilds
// `combos` — new block_size column AND no CHECK — preserving every row id.
//
// The fixture takes a real schema from Open() and then swaps ONLY the combos
// table back to its pre-v9 shape, so nothing else about the database is
// hand-modelled and a failure here can only be about the rebuild. Dropping
// combos while combo_hops references it needs foreign_keys off, exactly as the
// migration itself does.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// oldCombosDDL is the pre-v9 shape, verbatim from a deployed database: the
// strategy CHECK, no block_size, and no context_size (added later by an ALTER,
// so the rebuild has to cope with its absence).
const oldCombosDDL = `
CREATE TABLE combos (
  id          INTEGER PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  enabled     INTEGER NOT NULL DEFAULT 1,
  strategy    TEXT NOT NULL DEFAULT 'failover'
              CHECK (strategy IN ('failover','true_round_robin','strict_round_robin','sticky_last_good','least_used')),
  sticky_idle_s INTEGER NOT NULL DEFAULT 1800,
  compression_profile_id INTEGER REFERENCES compression_profiles(id),
  rr_cursor   INTEGER NOT NULL DEFAULT 0,
  rr_epoch    INTEGER NOT NULL DEFAULT 0,
  notes       TEXT,
  created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
)`

// comboID is deliberately not 1: preserving a non-default primary key is what
// keeps combo_hops.combo_id pointing at the right combo after the rebuild.
const comboID = 7

func TestMigrateV8ToV9RebuildsCombos(t *testing.T) {
	path := filepath.Join(t.TempDir(), "combos.sqlite")
	ctx := context.Background()

	db, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.w.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatalf("fk off: %v", err)
	}
	// Downgrade the combos table to the deployed pre-v9 shape, then seed a combo
	// and its hops into it.
	if _, err := db.w.ExecContext(ctx, `DROP TABLE combos`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := db.w.ExecContext(ctx, oldCombosDDL); err != nil {
		t.Fatalf("recreate old shape: %v", err)
	}
	if _, err := db.w.ExecContext(ctx,
		`INSERT INTO combos (id, name, strategy, sticky_idle_s, notes, created_at)
		 VALUES (?, 'daily', 'sticky_last_good', 30, 'keeper', '2026-10-01T00:00:00.000Z')`, comboID); err != nil {
		t.Fatalf("seed combo: %v", err)
	}
	if _, err := db.w.ExecContext(ctx,
		`INSERT INTO combo_hops (combo_id, position, account_id, model_id, weight, enabled)
		 VALUES (?, 0, 3, 'gpt-6-luna', 1, 1)`, comboID); err != nil {
		t.Fatalf("seed hop: %v", err)
	}
	if _, err := db.w.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
		t.Fatalf("fk on: %v", err)
	}
	if _, err := db.w.ExecContext(ctx, `UPDATE schema_version SET version = 8`); err != nil {
		t.Fatalf("stamp v8: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen → cur(8) < schemaVersion(9) → the rebuild must run.
	migrated, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("reopen/migrate: %v", err)
	}
	defer migrated.Close()

	if got := migrated.SchemaVersion(); got != 9 {
		t.Fatalf("schema version = %d, want 9", got)
	}
	if has, err := hasColumn(ctx, migrated.w, "combos", "block_size"); err != nil || !has {
		t.Fatalf("combos.block_size missing after rebuild (has=%v err=%v)", has, err)
	}

	// The combo survived with its identity intact — combo_hops.combo_id depends
	// on that, which is why the copy names its columns instead of SELECT *.
	var (
		id       int64
		strategy string
		sticky   int
		context  int64
		notes    string
	)
	if err := migrated.r.QueryRowContext(ctx,
		`SELECT id, strategy, sticky_idle_s, context_size, COALESCE(notes,'') FROM combos WHERE name='daily'`).
		Scan(&id, &strategy, &sticky, &context, &notes); err != nil {
		t.Fatalf("combo lost by the rebuild: %v", err)
	}
	if id != comboID || strategy != "sticky_last_good" || sticky != 30 || notes != "keeper" {
		t.Errorf("combo row changed: id=%d strategy=%q sticky=%d notes=%q", id, strategy, sticky, notes)
	}
	if context != 200000 {
		t.Errorf("context_size = %d, want the 200000 default for a table that predates the column", context)
	}
	// Existing combos must become per-request rotation, not something surprising.
	var blockSize int
	if err := migrated.r.QueryRowContext(ctx, `SELECT block_size FROM combos WHERE id=?`, comboID).Scan(&blockSize); err != nil {
		t.Fatalf("block_size read: %v", err)
	}
	if blockSize != 1 {
		t.Errorf("block_size = %d, want 1", blockSize)
	}

	// combo_hops must still resolve — the rebuild's whole risk is the drop/rename.
	var orphans int
	if err := migrated.r.QueryRowContext(ctx, `
SELECT COUNT(*) FROM combo_hops h LEFT JOIN combos c ON c.id = h.combo_id WHERE c.id IS NULL`).
		Scan(&orphans); err != nil {
		t.Fatalf("orphan check: %v", err)
	}
	if orphans != 0 {
		t.Errorf("%d combo_hops rows lost their combo in the rebuild", orphans)
	}

	// THE point of the rebuild: the strategy the old CHECK rejected is now
	// storable, because validation lives in Go.
	if _, err := migrated.w.ExecContext(ctx,
		`UPDATE combos SET strategy='block_round_robin' WHERE id=?`, comboID); err != nil {
		t.Fatalf("old CHECK still enforced after rebuild: %v", err)
	}
	// The Go validator is now the only gate, and it still names the valid set.
	if err := ValidateCombo(Combo{Name: "x", Strategy: "nonsense", ContextSize: 200000, BlockSize: 1}); err == nil {
		t.Error("ValidateCombo accepted an unknown strategy")
	} else if !strings.Contains(err.Error(), "strategy must be one of") {
		t.Errorf("validation error = %q, want it to name the valid strategies", err)
	}
	// And block_round_robin is one of them.
	if err := ValidateCombo(Combo{Name: "x", Strategy: "block_round_robin", ContextSize: 200000, BlockSize: 5}); err != nil {
		t.Errorf("ValidateCombo rejected block_round_robin: %v", err)
	}

	// Idempotent: the guard is the column, and a rebuild is not repeatable
	// blindly (the second pass would find no combos_v9 to work with).
	if err := migrated.rebuildCombosV9(ctx); err != nil {
		t.Fatalf("second rebuild should be a no-op, got %v", err)
	}
}

func TestValidateComboBlockSize(t *testing.T) {
	base := Combo{Name: "c", Strategy: "block_round_robin", ContextSize: 200000}
	for _, tc := range []struct {
		size    int
		wantErr bool
	}{
		{0, false}, // unset; UpsertCombo normalises to 1
		{1, false},
		{5, false},
		{1000, false},
		{-1, true},
		{1001, true},
	} {
		c := base
		c.BlockSize = tc.size
		err := ValidateCombo(c)
		if tc.wantErr && err == nil {
			t.Errorf("block_size %d accepted, want rejected", tc.size)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("block_size %d rejected: %v", tc.size, err)
		}
	}
}
