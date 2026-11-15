package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// v35 drops the shelved tool-pruning column: a fresh database never has it, and
// an upgraded v34 database loses it while every other value on its rows stays.
func TestToolPruneColumnDroppedInV35(t *testing.T) {
	ctx := context.Background()
	fresh, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "fresh.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	if has, err := hasColumn(ctx, fresh.w, "calls", "tool_prune_shadow_tokens"); err != nil || has {
		t.Fatalf("fresh database has tool_prune_shadow_tokens (has=%v err=%v)", has, err)
	}
	fresh.Close()

	path := filepath.Join(t.TempDir(), "old.sqlite")
	db, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	db.RecordCall(Call{TS: time.Now().UTC(), Client: "keeper", Account: "ns", Model: "m", Status: 200, TokensIn: 42})
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	// Regress to v34: the column existed and carried a measurement.
	for _, q := range []string{
		`ALTER TABLE calls ADD COLUMN tool_prune_shadow_tokens INTEGER NOT NULL DEFAULT 0`,
		`UPDATE calls SET tool_prune_shadow_tokens = 777`,
		`UPDATE schema_version SET version = 34`,
	} {
		if _, err := db.w.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("reopen/migrate: %v", err)
	}
	defer migrated.Close()
	if got := migrated.SchemaVersion(); got != schemaVersion {
		t.Fatalf("schema version = %d, want %d", got, schemaVersion)
	}
	if has, err := hasColumn(ctx, migrated.w, "calls", "tool_prune_shadow_tokens"); err != nil || has {
		t.Fatalf("tool_prune_shadow_tokens survived the v35 migration (has=%v err=%v)", has, err)
	}
	rows, err := migrated.RecentCalls(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Client != "keeper" || rows[0].TokensIn != 42 {
		t.Fatalf("rows = %+v, want the pre-upgrade row intact", rows)
	}
}
