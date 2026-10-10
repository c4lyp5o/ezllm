package store

// v8 (M9) ledger capture: the research columns must round-trip through the
// async batch writer, and a pre-v8 database must upgrade without losing rows.
// Both are behaviours a wrong implementation passes silently — a column added
// to schema.sql but not to the INSERT/scan, or a migration that runs after the
// index creation, only shows up here.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestResearchColumnsRoundTrip(t *testing.T) {
	db := openTest(t)
	db.RecordCall(Call{
		Surface: SurfaceOpenAI, Account: "a", AccountID: 0, Model: "m", Status: 200,
		TokensIn: 5000, TokensCachedRead: 4000,
		PrefixSHA: "deadbeefdeadbeef",
		MsgCount:  7,
		ToolCount: 3,
		SessionID: "sess-42",
		ReqBytes:  12345,
	})
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	rows, err := db.RecentCalls(context.Background(), 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("err=%v rows=%d", err, len(rows))
	}
	c := rows[0]
	if c.PrefixSHA != "deadbeefdeadbeef" {
		t.Errorf("prefix_sha = %q, want the value written", c.PrefixSHA)
	}
	if c.MsgCount != 7 || c.ToolCount != 3 {
		t.Errorf("msg/tool counts = %d/%d, want 7/3", c.MsgCount, c.ToolCount)
	}
	if c.SessionID != "sess-42" {
		t.Errorf("session_id = %q, want sess-42", c.SessionID)
	}
	if c.ReqBytes != 12345 {
		t.Errorf("req_bytes = %d, want 12345", c.ReqBytes)
	}
}

func TestSavedRealVsNotionalRoundTrip(t *testing.T) {
	db := openTest(t)
	// A real saving: applied, tokens actually shipped shorter.
	db.RecordCall(Call{Surface: SurfaceOpenAI, Account: "a", Model: "m", Status: 200,
		CompressionProfile: "p", CompressionApplied: true, PromptTokensPre: 1000,
		TokensSaved: 400, TokensSavedNotional: 0})
	// A floor reject: nothing shipped, but the potential is recorded separately.
	db.RecordCall(Call{Surface: SurfaceOpenAI, Account: "a", Model: "m", Status: 200,
		CompressionProfile: "p", CompressionApplied: false, PromptTokensPre: 2000,
		TokensSaved: 0, TokensSavedNotional: 60})
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	rows, _ := db.RecentCalls(context.Background(), 2)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	var applied, rejected Call
	for _, r := range rows {
		if r.CompressionApplied {
			applied = r
		} else {
			rejected = r
		}
	}
	if applied.TokensSaved != 400 || applied.TokensSavedNotional != 0 {
		t.Errorf("applied row: saved=%d notional=%d, want 400/0", applied.TokensSaved, applied.TokensSavedNotional)
	}
	if rejected.TokensSaved != 0 || rejected.TokensSavedNotional != 60 {
		t.Errorf("rejected row: saved=%d notional=%d, want 0/60", rejected.TokensSaved, rejected.TokensSavedNotional)
	}
}

// TestMigrateV7AddsResearchColumnsKeepsRows rewinds a live v8 database to v7
// (dropping the new columns is what sql.Open's migration then re-adds) and
// proves existing rows survive and the columns exist. Guarding the ORDER is the
// point: the research indexes are created over prefix_sha/session_id, so if the
// ALTER ran after schema.sql re-apply, boot would abort on a pre-v8 file.
func TestMigrateV7AddsResearchColumnsKeepsRows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.sqlite")

	db := openAt(t, path)
	// calls.account_id has no FK (it is the denormalized metering key), so a
	// row records fine without a seeded account — this test is about the calls
	// table surviving the upgrade.
	db.RecordCall(Call{Surface: SurfaceOpenAI, Account: "a", Model: "m", Status: 200, TokensIn: 42, PrefixSHA: "abc"})
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	before := rowCount(t, db, "calls")
	db.Close()

	// Rewind to v7 by dropping every v8 calls column + version stamp.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// Drop the v8 indexes BEFORE the columns: SQLite refuses to drop a column
	// an index depends on. (Real upgrades never hit this — v7 databases simply
	// lack both, and the migration adds columns before re-applying schema.sql,
	// which creates the indexes.)
	for _, idx := range []string{"idx_calls_prefix", "idx_calls_session"} {
		if _, err := raw.Exec(`DROP INDEX IF EXISTS ` + idx); err != nil {
			t.Fatalf("drop %s: %v", idx, err)
		}
	}
	for _, col := range []string{"prefix_sha", "session_id", "msg_count", "tool_count", "req_bytes", "compression_saved_notional"} {
		if _, err := raw.Exec("ALTER TABLE calls DROP COLUMN " + col); err != nil {
			t.Fatalf("drop %s: %v", col, err)
		}
	}
	if _, err := raw.Exec(`UPDATE schema_version SET version = 7`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	// Reopen: migration must add the columns back without touching the row.
	db2 := openAt(t, path)
	defer db2.Close()
	if got := db2.SchemaVersion(); got != schemaVersion {
		t.Fatalf("version = %d, want %d", got, schemaVersion)
	}
	for _, col := range []string{"prefix_sha", "session_id", "msg_count", "tool_count", "req_bytes", "compression_saved_notional"} {
		ok, err := hasColumn(context.Background(), db2.Writer(), "calls", col)
		if err != nil || !ok {
			t.Errorf("calls.%s present = %v err = %v, want true", col, ok, err)
		}
	}
	if after := rowCount(t, db2, "calls"); after != before {
		t.Errorf("calls rows after upgrade = %d, want %d (upgrade must not lose history)", after, before)
	}
	// The dropped column's value is gone (we dropped the column), but the row
	// survived with its other fields intact.
	var in int64
	if err := db2.Reader().QueryRow(`SELECT tokens_in FROM calls ORDER BY id DESC LIMIT 1`).Scan(&in); err != nil || in != 42 {
		t.Errorf("tokens_in = %d err = %v, want 42", in, err)
	}
}

func rowCount(t *testing.T, db *DB, table string) int {
	t.Helper()
	var n int
	if err := db.Reader().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
