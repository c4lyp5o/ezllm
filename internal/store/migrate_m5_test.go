package store

// M5 migration regression: a pre-M5 (v1) database must upgrade to v2 without
// error and keep its ledger.
//
// This test exists because the bug it guards was INVISIBLE to every other
// test: fresh databases get calls.account_id straight from schema.sql, so the
// migrate branch — the one real deployments take — never ran. Booting against
// the live v1 database aborted with "no such column: account_id": schema.sql
// creates idx_calls_acct_model ON calls(account_id, ...), CREATE TABLE IF NOT
// EXISTS is a no-op on the existing v1 table (which lacks the column), and the
// ALTER that adds it ran afterwards. The v1 fixture below is built by hand to
// reproduce exactly that shape.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// v1SchemaSQL reproduces the pre-M5 shape a real deployment has: the calls
// table WITHOUT account_id, its v1 index, and the version-1 stamp.
const v1SchemaSQL = `
CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')));
CREATE TABLE calls (
  id INTEGER PRIMARY KEY,
  ts TEXT NOT NULL,
  client TEXT, surface TEXT, alias TEXT, account TEXT,
  provider_key_id INTEGER, key_hint TEXT, model TEXT,
  status INTEGER, stream INTEGER, ttft_ms INTEGER, total_ms INTEGER,
  tokens_in INTEGER DEFAULT 0, tokens_out INTEGER DEFAULT 0,
  tokens_cached_read INTEGER DEFAULT 0, tokens_cached_write INTEGER DEFAULT 0,
  reasoning_tokens INTEGER DEFAULT 0,
  raw_usage TEXT, endpoint_id INTEGER, upstream_model TEXT,
  compression_profile TEXT, compression_applied INTEGER,
  prompt_tokens_pre INTEGER, tokens_saved INTEGER, compression_ms INTEGER,
  compression_rules_fired INTEGER DEFAULT 0,
  context_tokens_pre INTEGER DEFAULT 0, context_tokens_saved INTEGER DEFAULT 0,
  err TEXT
);
CREATE INDEX idx_calls_ts ON calls(ts DESC);
INSERT INTO schema_version(version) VALUES (1);
`

func openAt(t *testing.T, path string) *DB {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	return db
}

// buildV1DB writes a pre-M5 database (raw driver, no Open → no migration)
// with n ledger rows, and closes it.
func buildV1DB(t *testing.T, path string, n int) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(v1SchemaSQL); err != nil {
		t.Fatalf("build v1 fixture: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := raw.Exec(
			`INSERT INTO calls (ts, client, surface, model, status, tokens_in, tokens_out)
			 VALUES ('2026-10-05T10:00:00.000Z','hermes','openai','some-model',200,100,50)`); err != nil {
			t.Fatalf("seed v1 row: %v", err)
		}
	}
}

func TestMigrateV1AddsAccountIDAndKeepsRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v1.sqlite")
	buildV1DB(t, path, 5)

	// Open: migrate() must take the v1->v2 path and succeed.
	db := openAt(t, path)
	defer db.Close()

	if got := db.SchemaVersion(); got != schemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", got, schemaVersion)
	}
	var contextCols int
	if err := db.Reader().QueryRow(`SELECT COUNT(*) FROM pragma_table_info('calls') WHERE name IN ('context_tokens_pre','context_tokens_saved')`).Scan(&contextCols); err != nil {
		t.Fatal(err)
	}
	if contextCols != 2 {
		t.Fatalf("context token columns = %d, want 2", contextCols)
	}

	// account_id column now exists (the M5 metering key)
	ok, err := hasColumn(ctx, db.Writer(), "calls", "account_id")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("calls.account_id missing after migration")
	}

	// the index that aborted boot now exists
	var idxCount int
	if err := db.Writer().QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_calls_acct_model'`).
		Scan(&idxCount); err != nil {
		t.Fatal(err)
	}
	if idxCount != 1 {
		t.Errorf("idx_calls_acct_model = %d, want 1", idxCount)
	}

	// the new M5 tables exist
	for _, tbl := range []string{"model_rules", "usage_counters"} {
		exists, err := hasTable(ctx, db.Writer(), tbl)
		if err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Errorf("table %s missing after migration", tbl)
		}
	}

	// ledger rows survived
	var n int
	if err := db.Writer().QueryRow(`SELECT COUNT(*) FROM calls`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("calls rows = %d, want 5 (migration must not destroy data)", n)
	}
}

// TestMigrateV1ReopenIsIdempotent: opening the upgraded database again must not
// re-ALTER, error, or churn the version stamp.
func TestMigrateV1ReopenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.sqlite")
	buildV1DB(t, path, 0)

	for i := 0; i < 3; i++ {
		db := openAt(t, path)
		if got := db.SchemaVersion(); got != schemaVersion {
			t.Errorf("open #%d: SchemaVersion = %d, want %d", i, got, schemaVersion)
		}
		db.Close()
	}
}
