package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// Addendum #5: every accepted compression transformation is attributable to
// the immutable ruleset that made it — calls.compression_version. This also
// proves the v15 -> v16 migration adds the column to an existing database.
func TestMigrateV15ToV16AddsCompressionVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v16.sqlite")
	ctx := context.Background()

	db, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// A call row that must survive the migration with its savings intact.
	db.RecordCall(Call{
		TS: time.Now().UTC(), Client: "c", Account: "a", Model: "m", Status: 200,
		TokensIn: 100, TokensSaved: 33, CompressionApplied: true, CompressionProfile: "p1",
	})
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	// Reproduce a v15 database: no compression_version column, version 15.
	if _, err := db.w.ExecContext(ctx, `ALTER TABLE calls DROP COLUMN compression_version`); err != nil {
		t.Fatalf("drop compression_version (SQLite >= 3.35 required): %v", err)
	}
	if _, err := db.w.ExecContext(ctx, `UPDATE schema_version SET version = 15`); err != nil {
		t.Fatalf("stamp v15: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen → cur(15) < schemaVersion → the upgrade branch must run.
	migrated, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("reopen/migrate: %v", err)
	}
	defer migrated.Close()
	if got := migrated.SchemaVersion(); got != schemaVersion {
		t.Fatalf("schema version = %d, want %d", got, schemaVersion)
	}

	// A row written AFTER the migration carries the version and round-trips.
	migrated.RecordCall(Call{
		TS: time.Now().UTC(), Client: "c", Account: "a", Model: "m", Status: 200,
		TokensIn: 100, TokensSaved: 20, CompressionApplied: true, CompressionProfile: "p1",
		CompressionVersion: "v1a2b3c4d",
	})
	if err := migrated.Flush(); err != nil {
		t.Fatal(err)
	}
	rows, err := migrated.RecentCalls(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range rows {
		if r.CompressionVersion == "v1a2b3c4d" {
			found = true
		}
	}
	if !found {
		t.Errorf("the version did not round-trip through the migrated schema: %+v", rows)
	}
}

// A v16 database with a row written by the OLD binary (NULL version) reads
// back as an empty string — an unknown version, never a fabricated one.
func TestCompressionVersionNullIsUnknown(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "t.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.w.ExecContext(ctx,
		`INSERT INTO calls (ts, client, surface, account, model, status, tokens_in) VALUES (?,?,?,?,?,?,?)`,
		time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), "c", "chat", "a", "m", 200, 10); err != nil {
		t.Fatal(err)
	}
	rows, err := db.RecentCalls(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	if rows[0].CompressionVersion != "" {
		t.Errorf("a pre-#5 row must read '', got %q", rows[0].CompressionVersion)
	}
}
