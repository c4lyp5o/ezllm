package store

// Operator-curated protocol verdicts: round-trip with provenance, probe
// protection, catalog-sync survival, and the v10→v11 migration.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func present(b bool) ProtoEdit { return ProtoEdit{Present: true, Value: &b} }

func entryByID(t *testing.T, db *DB, acct int64, model string) ModelEntryRow {
	t.Helper()
	entries, err := db.ListModels(context.Background(), acct)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range entries {
		if m.ID == model {
			return m
		}
	}
	t.Fatalf("model %q not in catalog", model)
	return ModelEntryRow{}
}

// A curation round-trips: verdicts land, source+note stick, and proto_tested_at
// stays NULL — nothing was probed, so no probe timestamp is fabricated.
// Absent edits keep their recorded values.
func TestModelCuratedRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openAt(t, filepath.Join(t.TempDir(), "proto-curated.sqlite"))
	defer db.Close()
	acct := seedPinAccount(t, db)

	// Probe first so the row carries a real probe timestamp the curation must
	// clear: a curated row never claims "last probed at".
	if err := db.SetModelProto(ctx, acct, "m1", provider.SurfaceOpenAI, ptrTrue()); err != nil {
		t.Fatal(err)
	}
	if m := entryByID(t, db, acct, "m1"); m.TestedAt == nil || m.Source != "probe" {
		t.Fatalf("precondition: probe should stamp tested_at + source=probe, got %+v", m)
	}

	if err := db.SetModelCurated(ctx, acct, "m1", "edge stonewalls anthropic probes", map[string]ProtoEdit{
		"openai": present(true), "anthropic": present(false),
	}, false); err != nil {
		t.Fatal(err)
	}
	m := entryByID(t, db, acct, "m1")
	if m.Source != "curated" || m.Note != "edge stonewalls anthropic probes" {
		t.Fatalf("provenance = %q/%q, want curated + note", m.Source, m.Note)
	}
	if m.OpenAI == nil || !*m.OpenAI || m.Anthropic == nil || *m.Anthropic {
		t.Fatalf("verdicts = %v/%v, want true/false", m.OpenAI, m.Anthropic)
	}
	if m.TestedAt != nil {
		t.Fatalf("curated row carries tested_at %q, want nil", *m.TestedAt)
	}

	if err := db.SetModelCurated(ctx, acct, "m1", "responses probed ok by hand", map[string]ProtoEdit{
		"responses": present(true),
	}, false); err != nil {
		t.Fatal(err)
	}
	m = entryByID(t, db, acct, "m1")
	if m.OpenAI == nil || !*m.OpenAI || m.Anthropic == nil || *m.Anthropic {
		t.Fatalf("absent verdicts changed: %v/%v", m.OpenAI, m.Anthropic)
	}
	if m.Responses == nil || !*m.Responses {
		t.Fatalf("responses = %v, want true", m.Responses)
	}

	// Curation never creates rows — an unknown model is a not-found, matching
	// the pin route's typo protection.
	if err := db.SetModelCurated(ctx, acct, "nope", "x", map[string]ProtoEdit{"openai": present(true)}, false); err != ErrNotFound {
		t.Fatalf("unknown model err = %v, want ErrNotFound", err)
	}
}

// A curated row outranks probes until released: neither the single probe write
// nor the sweep may overwrite it. Release hands the row back — verdicts stay,
// provenance clears, and the next probe write applies again.
func TestModelCuratedProbeProtectionAndRelease(t *testing.T) {
	ctx := context.Background()
	db := openAt(t, filepath.Join(t.TempDir(), "proto-protect.sqlite"))
	defer db.Close()
	acct := seedPinAccount(t, db)
	yes := true
	if err := db.SetModelCurated(ctx, acct, "m1", "curated openai-only", map[string]ProtoEdit{
		"openai": present(true), "anthropic": present(false),
	}, false); err != nil {
		t.Fatal(err)
	}

	if err := db.SetModelProto(ctx, acct, "m1", provider.SurfaceAnthropic, &yes); err != nil {
		t.Fatal(err)
	}
	if err := db.SetModelProtos(ctx, acct, map[string]map[string]*bool{
		"m1": {"openai": ptrTrue(), "anthropic": &yes},
	}); err != nil {
		t.Fatal(err)
	}
	m := entryByID(t, db, acct, "m1")
	if m.Source != "curated" || m.Anthropic == nil || *m.Anthropic {
		t.Fatalf("probe clobbered a curated row: source=%q anthropic=%v", m.Source, m.Anthropic)
	}

	if err := db.SetModelCurated(ctx, acct, "m1", "", nil, true); err != nil {
		t.Fatal(err)
	}
	m = entryByID(t, db, acct, "m1")
	if m.Source != "" || m.Note != "" {
		t.Fatalf("after release provenance = %q/%q, want empty", m.Source, m.Note)
	}
	if m.OpenAI == nil || !*m.OpenAI {
		t.Fatalf("openai after release = %v, want the recorded true", m.OpenAI)
	}
	if err := db.SetModelProto(ctx, acct, "m1", provider.SurfaceAnthropic, &yes); err != nil {
		t.Fatal(err)
	}
	m = entryByID(t, db, acct, "m1")
	if m.Anthropic == nil || !*m.Anthropic || m.Source != "probe" {
		t.Fatalf("probe write after release: anthropic=%v source=%q", m.Anthropic, m.Source)
	}
}

// A curated verdict survives a catalog re-sync, like pin and probe verdicts.
func TestModelCuratedSurvivesResync(t *testing.T) {
	ctx := context.Background()
	db := openAt(t, filepath.Join(t.TempDir(), "proto-resync.sqlite"))
	defer db.Close()
	acct := seedPinAccount(t, db)
	if err := db.SetModelCurated(ctx, acct, "m1", "curated", map[string]ProtoEdit{"openai": present(true)}, false); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertModels(ctx, acct, []provider.ModelInfo{{ID: "m1", Display: "M One"}}); err != nil {
		t.Fatal(err)
	}
	m := entryByID(t, db, acct, "m1")
	if m.Source != "curated" || m.Note != "curated" || m.OpenAI == nil || !*m.OpenAI {
		t.Fatalf("curation lost on resync: %+v", m)
	}
}

// A v10 database (no proto_source/proto_note) upgrades to v11 in place,
// keeping its rows.
func TestMigrateV10AddsProtoSourceKeepsRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v10.sqlite")
	seed := openAt(t, path)
	acct := seedPinAccount(t, seed)
	seed.Close()

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"proto_source", "proto_note"} {
		if _, err := raw.Exec(`ALTER TABLE models DROP COLUMN ` + col); err != nil {
			t.Fatalf("drop %s: %v", col, err)
		}
	}
	if _, err := raw.Exec(`UPDATE schema_version SET version = 10`); err != nil {
		t.Fatalf("rewind version: %v", err)
	}
	raw.Close()

	db := openAt(t, path)
	defer db.Close()
	if got := db.SchemaVersion(); got != schemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", got, schemaVersion)
	}
	for _, col := range []string{"proto_source", "proto_note"} {
		ok, err := hasColumn(context.Background(), db.Writer(), "models", col)
		if err != nil || !ok {
			t.Fatalf("%s column present = %v err=%v, want true", col, ok, err)
		}
	}
	var n int
	if err := db.Reader().QueryRow(`SELECT COUNT(*) FROM models WHERE account_id=?`, acct).Scan(&n); err != nil || n != 1 {
		t.Fatalf("model rows after upgrade = %d err=%v, want 1", n, err)
	}
}
