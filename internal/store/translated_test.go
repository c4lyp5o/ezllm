package store

// Ledger attribution for wire-translated hops (schema v12): the marker
// round-trips through every projection (insert → RecentCalls → CallEvent), and
// the v11 → v12 migration adds the column in place with existing rows intact.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// A translated row survives the insert → RecentCalls round trip; a native row
// beside it must stay untranslated. The round trip is the proof — any column/
// placeholder/arg mismatch in the INSERT fails here rather than in prod.
func TestCallTranslatedRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rt.sqlite")
	db := openAt(t, path)
	defer db.Close()

	base := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	db.RecordCall(Call{TS: base, Client: "claude-code", Surface: SurfaceAnthropic,
		Account: "groq", Model: "deepseek-flash", Status: 200, Translated: true,
		TokensIn: 1000, TokensOut: 5})
	db.RecordCall(Call{TS: base.Add(time.Second), Client: "hermes", Surface: SurfaceAnthropic,
		Account: "vercel", Model: "qwen3.8", Status: 200, Translated: false})
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}

	rows, err := db.RecentCalls(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// RecentCalls is newest-first: index 0 is the native row.
	native, translated := rows[0], rows[1]
	if translated.Translated != true {
		t.Error("translated marker lost on the write path")
	}
	if translated.TokensIn != 1000 {
		t.Errorf("tokens_in = %d, want 1000 (the vendor's inclusive total)", translated.TokensIn)
	}
	if native.Translated {
		t.Error("a native hop must not be marked translated")
	}

	// The SSE feed must carry the same marker as the snapshot projection.
	ev := EventFromCall(translated)
	if !ev.Translated {
		t.Error("CallEvent lost the translated marker (snapshot/live parity)")
	}
	if EventFromCall(native).Translated {
		t.Error("CallEvent must not mark a native hop translated")
	}
}

// A database stamped at v11 (no translated column) upgrades to v12 in place,
// keeping its rows and attributing NOTHING retroactively: old rows default 0.
func TestMigrateV11AddsTranslatedKeepsRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v11.sqlite")
	seed := openAt(t, path)
	seed.RecordCall(Call{TS: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC),
		Client: "hermes", Surface: SurfaceAnthropic, Account: "vercel",
		Model: "qwen3.8", Status: 200})
	if err := seed.Flush(); err != nil {
		t.Fatal(err)
	}
	seed.Close()

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`ALTER TABLE calls DROP COLUMN translated`); err != nil {
		t.Fatalf("drop translated: %v", err)
	}
	if _, err := raw.Exec(`UPDATE schema_version SET version = 11`); err != nil {
		t.Fatalf("rewind version: %v", err)
	}
	raw.Close()

	db := openAt(t, path)
	defer db.Close()

	// The claim is "migrated up to the binary's version", never a literal.
	if got := db.SchemaVersion(); got != schemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", got, schemaVersion)
	}
	ok, err := hasColumn(context.Background(), db.Writer(), "calls", "translated")
	if err != nil || !ok {
		t.Fatalf("translated column present = %v err=%v, want true", ok, err)
	}
	rows, err := db.RecentCalls(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows after upgrade = %d, want 1 (data preserved)", len(rows))
	}
	if rows[0].Translated {
		t.Error("pre-v12 rows must not be attributed translated (no invented provenance)")
	}
}
