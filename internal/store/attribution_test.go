package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// Addendum #7: per-engine attribution + sampled shadow measurement ride the
// ledger row, and the v16 -> v17 migration adds both columns to an existing
// database.
func TestMigrateV16ToV17AddsAttributionColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v17.sqlite")
	ctx := context.Background()

	db, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.RecordCall(Call{
		TS: time.Now().UTC(), Client: "c", Account: "a", Model: "m", Status: 200,
		TokensIn: 100, TokensSaved: 33, CompressionApplied: true, CompressionProfile: "p1",
	})
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	// Reproduce a v16 database: no attribution columns, version 16.
	for _, ddl := range []string{
		`ALTER TABLE calls DROP COLUMN compression_stages`,
		`ALTER TABLE calls DROP COLUMN shadow_tokens_saved`,
	} {
		if _, err := db.w.ExecContext(ctx, ddl); err != nil {
			t.Fatalf("%s (SQLite >= 3.35 required): %v", ddl, err)
		}
	}
	if _, err := db.w.ExecContext(ctx, `UPDATE schema_version SET version = 16`); err != nil {
		t.Fatal(err)
	}
	// A pre-#7 row must read back as "no attribution, no shadow" after it.
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
	rows, err := migrated.RecentCalls(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].CompressionStages != "" || rows[0].ShadowTokensSaved != 0 {
		t.Errorf("a pre-#7 row must read empty/0: %q/%d",
			rows[0].CompressionStages, rows[0].ShadowTokensSaved)
	}

	// A row written AFTER the migration round-trips the attribution JSON and
	// the shadow number.
	migrated.RecordCall(Call{
		TS: time.Now().UTC(), Client: "c", Account: "a", Model: "m", Status: 200,
		TokensIn: 100, TokensSaved: 20, CompressionApplied: true, CompressionProfile: "p1",
		CompressionStages: `[{"engine":"lite","saved":12}]`,
		ShadowTokensSaved: 45,
	})
	if err := migrated.Flush(); err != nil {
		t.Fatal(err)
	}
	rows, err = migrated.RecentCalls(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	var gotStages string
	var gotShadow int64
	for _, r := range rows {
		if r.CompressionStages != "" {
			gotStages, gotShadow = r.CompressionStages, r.ShadowTokensSaved
		}
	}
	if gotStages != `[{"engine":"lite","saved":12}]` || gotShadow != 45 {
		t.Errorf("round-trip = %q/%d, want the attribution JSON and 45", gotStages, gotShadow)
	}
}
