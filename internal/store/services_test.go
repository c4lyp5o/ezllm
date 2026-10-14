package store

// v15 service bindings + probes.
//
// The migration test reproduces a real v14 database (drop the two v15 tables,
// stamp the version back) instead of trusting a fresh one: fresh databases get
// the tables from schema.sql directly, so a broken upgrade path would be
// invisible to them.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func boolPtr(v bool) *bool { return &v }

func TestServiceBindingRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Fresh database has the v15 tables and no bindings.
	got, err := db.ListServiceBindings(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("fresh database has %d bindings, want 0", len(got))
	}

	// Manual binding round-trips.
	if err := db.SetServiceBinding(ctx, "embeddings", "manual", "up/emb-1"); err != nil {
		t.Fatalf("set manual: %v", err)
	}
	b, err := db.ServiceBinding(ctx, "embeddings")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if b.Mode != "manual" || b.ModelTarget != "up/emb-1" {
		t.Errorf("binding = %+v, want manual up/emb-1", b)
	}
	resolved, err := db.ResolveServiceDefault(ctx, "embeddings")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved != "up/emb-1" {
		t.Errorf("resolved = %q, want up/emb-1", resolved)
	}

	// Auto must ignore and clear any leftover target.
	if err := db.SetServiceBinding(ctx, "embeddings", "auto", "up/emb-1"); err != nil {
		t.Fatalf("set auto: %v", err)
	}
	b, err = db.ServiceBinding(ctx, "embeddings")
	if err != nil {
		t.Fatalf("get auto: %v", err)
	}
	if b.Mode != "auto" || b.ModelTarget != "" {
		t.Errorf("auto binding = %+v, want mode=auto target empty", b)
	}

	// Clearing removes the row and the default.
	if err := db.ClearServiceBinding(ctx, "embeddings"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if resolved, err := db.ResolveServiceDefault(ctx, "embeddings"); err != nil || resolved != "" {
		t.Errorf("resolve after clear = %q, %v; want empty", resolved, err)
	}

	// Validation lives in the write path, not only the handler.
	if err := db.SetServiceBinding(ctx, "nope", "manual", "up/m"); err == nil {
		t.Error("unknown surface accepted")
	}
	if err := db.SetServiceBinding(ctx, "embeddings", "manual", ""); err == nil {
		t.Error("manual binding without target accepted")
	}
	if err := db.SetServiceBinding(ctx, "embeddings", "wild", "up/m"); err == nil {
		t.Error("unknown mode accepted")
	}
}

func TestServiceProbeVerdictsDriveAuto(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := db.CreateAccount(ctx, AccountInput{
		Name: "Up", Namespace: "up", Kind: "openai-compatible",
		BaseURL: "https://example.invalid/v1",
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	other, err := db.CreateAccount(ctx, AccountInput{
		Name: "Down", Namespace: "down", Kind: "openai-compatible",
		BaseURL: "https://example.invalid/v1",
	})
	if err != nil {
		t.Fatalf("create second account: %v", err)
	}
	if err := db.UpsertModels(ctx, id, []provider.ModelInfo{{ID: "emb-1", Display: "emb"}}); err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if err := db.UpsertModels(ctx, other, []provider.ModelInfo{{ID: "emb-2", Display: "emb2"}}); err != nil {
		t.Fatalf("catalog 2: %v", err)
	}

	// Auto with zero tested models resolves to nothing — it must never guess.
	if err := db.SetServiceBinding(ctx, "embeddings", "auto", ""); err != nil {
		t.Fatalf("set auto: %v", err)
	}
	if resolved, err := db.ResolveServiceDefault(ctx, "embeddings"); err != nil || resolved != "" {
		t.Fatalf("auto with no probes = %q, %v; want empty", resolved, err)
	}

	// A FAILED verdict must not feed auto.
	if err := db.UpsertServiceProbe(ctx, "embeddings", other, "emb-2", false, "500 boom"); err != nil {
		t.Fatalf("probe fail: %v", err)
	}
	if resolved, err := db.ResolveServiceDefault(ctx, "embeddings"); err != nil || resolved != "" {
		t.Fatalf("auto with only failed probes = %q, %v; want empty", resolved, err)
	}

	// One tested-ok model: auto picks it.
	if err := db.UpsertServiceProbe(ctx, "embeddings", id, "emb-1", true, ""); err != nil {
		t.Fatalf("probe ok: %v", err)
	}
	resolved, err := db.ResolveServiceDefault(ctx, "embeddings")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved != "up/emb-1" {
		t.Fatalf("auto = %q, want up/emb-1", resolved)
	}

	// A SECOND tested-ok model must win as the most recent (auto = most
	// recently tested-ok). Upsert twice with a distinguishable order.
	if err := db.UpsertServiceProbe(ctx, "embeddings", other, "emb-2", true, ""); err != nil {
		t.Fatalf("probe ok 2: %v", err)
	}
	resolved, err = db.ResolveServiceDefault(ctx, "embeddings")
	if err != nil {
		t.Fatalf("resolve 2: %v", err)
	}
	if resolved != "down/emb-2" {
		t.Fatalf("auto = %q, want the most recent verdict down/emb-2", resolved)
	}

	// Disabling the account must take its model out of auto rotation.
	if err := db.UpdateAccount(ctx, other, AccountInput{Enabled: boolPtr(false)}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	resolved, err = db.ResolveServiceDefault(ctx, "embeddings")
	if err != nil {
		t.Fatalf("resolve disabled: %v", err)
	}
	if resolved != "up/emb-1" {
		t.Fatalf("auto = %q, want up/emb-1 (disabled account skipped)", resolved)
	}
}

func TestServiceBindingSurvivesResync(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := db.CreateAccount(ctx, AccountInput{
		Name: "Up", Namespace: "up", Kind: "openai-compatible",
		BaseURL: "https://example.invalid/v1",
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := db.UpsertModels(ctx, id, []provider.ModelInfo{{ID: "emb-1", Display: "emb"}}); err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if err := db.UpsertServiceProbe(ctx, "embeddings", id, "emb-1", true, ""); err != nil {
		t.Fatalf("probe: %v", err)
	}
	// A whole-catalog resync replaces models rows; the verdict lives in its
	// own table and must not be collateral damage.
	if err := db.UpsertModels(ctx, id, []provider.ModelInfo{{ID: "emb-1", Display: "emb"}, {ID: "emb-9", Display: "new"}}); err != nil {
		t.Fatalf("resync: %v", err)
	}
	p, err := db.ServiceProbe(ctx, "embeddings", id, "emb-1")
	if err != nil {
		t.Fatalf("probe lost on resync: %v", err)
	}
	if !p.OK {
		t.Errorf("probe verdict reset by resync: %+v", p)
	}
}

func TestMigrateV14ToV15AddsServiceTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v15.sqlite")
	ctx := context.Background()

	db, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// An account + binding that must survive the migration untouched.
	if _, err := db.CreateAccount(ctx, AccountInput{
		Name: "Keeper", Namespace: "keeper", Kind: "openai-compatible",
		BaseURL: "https://example.invalid/v1",
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if err := db.SetServiceBinding(ctx, "embeddings", "manual", "keeper/emb-1"); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	// Reproduce a real v14 database: no v15 tables, version 14.
	for _, table := range []string{"service_bindings", "service_probes"} {
		if _, err := db.w.ExecContext(ctx, "DROP TABLE "+table); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
	}
	if _, err := db.w.ExecContext(ctx, `UPDATE schema_version SET version = 14`); err != nil {
		t.Fatalf("stamp v14: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen → cur(14) < schemaVersion → the upgrade branch must re-apply
	// schema.sql and create the v15 tables.
	migrated, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("reopen/migrate: %v", err)
	}
	defer migrated.Close()

	if got := migrated.SchemaVersion(); got != schemaVersion {
		t.Fatalf("schema version = %d, want %d", got, schemaVersion)
	}
	// The tables exist and are usable.
	probes, err := migrated.ListServiceProbes(ctx, "embeddings")
	if err != nil {
		t.Fatalf("service_probes unusable after migrate: %v", err)
	}
	if probes == nil {
		t.Fatal("ListServiceProbes returned nil slice — must be []T{} per the dashboard contract")
	}
	// Pre-migration data survived (the binding row was dropped WITH its table
	// here on purpose — what must survive is the ACCOUNT the binding names).
	if _, err := migrated.AccountByNamespace(ctx, "keeper"); err != nil {
		t.Fatalf("existing account lost in migration: %v", err)
	}
	// The migrated DB accepts writes to the new tables.
	if err := migrated.SetServiceBinding(ctx, "embeddings", "manual", "keeper/emb-1"); err != nil {
		t.Fatalf("write after migrate: %v", err)
	}
	if b, err := migrated.ServiceBinding(ctx, "embeddings"); err != nil || b.ModelTarget != "keeper/emb-1" {
		t.Fatalf("binding after migrate = %+v, %v", b, err)
	}
}
