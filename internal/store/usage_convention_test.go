package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// usage_convention is the ONE schema fact this milestone adds: how the upstream
// Messages wire counts input_tokens. 'auto' means unknown, which a cross-wire
// Messages destination must refuse rather than guess (M5 consumes the guard).

func TestUsageConventionDefaultsToAuto(t *testing.T) {
	ctx := context.Background()
	db := openEventsDB(t, 4)
	defer db.Close()

	id, err := db.CreateAccount(ctx, AccountInput{
		Name: "Up", Namespace: "up", Kind: "openai-compatible",
		BaseURL: "https://example.invalid/v1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	acc, err := db.AccountByID(ctx, id)
	if err != nil {
		t.Fatalf("AccountByID: %v", err)
	}
	if acc.UsageConvention != provider.UsageConventionAuto {
		t.Fatalf("runtime usage_convention = %q, want %q", acc.UsageConvention, provider.UsageConventionAuto)
	}
	sum, err := db.GetAccount(ctx, id)
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if sum.UsageConvention != provider.UsageConventionAuto {
		t.Fatalf("admin usage_convention = %q, want %q", sum.UsageConvention, provider.UsageConventionAuto)
	}
}

func TestMigrateV12ToV13AddsUsageConventionPreservingAccounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v13.sqlite")
	ctx := context.Background()

	db, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	id, err := db.CreateAccount(ctx, AccountInput{
		Name: "Keeper", Namespace: "keeper", Kind: "openai-compatible",
		BaseURL: "https://example.invalid/v1", Notes: "survives",
	})
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}
	// Reproduce a v12 database: no usage_convention column, version 12.
	if _, err := db.w.ExecContext(ctx, "ALTER TABLE accounts DROP COLUMN usage_convention"); err != nil {
		t.Fatalf("drop column (SQLite >= 3.35 required): %v", err)
	}
	if _, err := db.w.ExecContext(ctx, `UPDATE schema_version SET version = 12`); err != nil {
		t.Fatalf("stamp v12: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	migrated, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("reopen/migrate: %v", err)
	}
	defer migrated.Close()
	if got := migrated.SchemaVersion(); got != schemaVersion {
		t.Fatalf("schema version = %d, want %d", got, schemaVersion)
	}
	acc, err := migrated.AccountByID(ctx, id)
	if err != nil {
		t.Fatalf("account lost in migration: %v", err)
	}
	if acc.Name != "Keeper" || acc.Namespace != "keeper" {
		t.Fatalf("account data changed: %+v", acc)
	}
	if acc.UsageConvention != provider.UsageConventionAuto {
		t.Fatalf("migrated usage_convention = %q, want auto", acc.UsageConvention)
	}
	// The upgraded database must accept the new value too.
	if err := migrated.UpdateAccount(ctx, id, AccountInput{UsageConvention: "total"}); err != nil {
		t.Fatalf("post-migration write: %v", err)
	}
}

func TestUpdateAccountUsageConventionPartialPatchSemantics(t *testing.T) {
	ctx := context.Background()
	db := openEventsDB(t, 4)
	defer db.Close()
	id, err := db.CreateAccount(ctx, AccountInput{
		Name: "Up", Namespace: "up", Kind: "openai-compatible",
		BaseURL: "https://example.invalid/v1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := db.UpdateAccount(ctx, id, AccountInput{UsageConvention: "anthropic"}); err != nil {
		t.Fatalf("set anthropic: %v", err)
	}
	// A patch that does not mention the field must not clear it.
	if err := db.UpdateAccount(ctx, id, AccountInput{Notes: "unrelated"}); err != nil {
		t.Fatalf("unrelated patch: %v", err)
	}
	acc, err := db.AccountByID(ctx, id)
	if err != nil {
		t.Fatalf("AccountByID: %v", err)
	}
	if acc.UsageConvention != "anthropic" {
		t.Fatalf("usage_convention after unrelated patch = %q, want anthropic", acc.UsageConvention)
	}
	if acc.Notes != "unrelated" {
		t.Fatalf("notes = %q, want unrelated", acc.Notes)
	}
	// And an explicit new value must land.
	if err := db.UpdateAccount(ctx, id, AccountInput{UsageConvention: "total"}); err != nil {
		t.Fatalf("set total: %v", err)
	}
	if acc, _ = db.AccountByID(ctx, id); acc.UsageConvention != "total" {
		t.Fatalf("usage_convention = %q, want total", acc.UsageConvention)
	}
	// 'auto' is the documented reset value.
	if err := db.UpdateAccount(ctx, id, AccountInput{UsageConvention: "auto"}); err != nil {
		t.Fatalf("reset auto: %v", err)
	}
	if acc, _ = db.AccountByID(ctx, id); acc.UsageConvention != "auto" {
		t.Fatalf("usage_convention = %q, want auto", acc.UsageConvention)
	}
}

func TestUpdateAccountRejectsUnknownUsageConvention(t *testing.T) {
	ctx := context.Background()
	db := openEventsDB(t, 4)
	defer db.Close()
	id, err := db.CreateAccount(ctx, AccountInput{
		Name: "Up", Namespace: "up", Kind: "openai-compatible",
		BaseURL: "https://example.invalid/v1", UsageConvention: "total",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, bad := range []string{"exclusive", "inclusive", "ANTHROPIC", " total", "auto ", "0"} {
		err := db.UpdateAccount(ctx, id, AccountInput{UsageConvention: bad})
		if err == nil || !strings.Contains(err.Error(), "usage_convention") {
			t.Fatalf("UpdateAccount(%q) error = %v, want a usage_convention refusal", bad, err)
		}
	}
	// The refused value must not have been written.
	acc, err := db.AccountByID(ctx, id)
	if err != nil {
		t.Fatalf("AccountByID: %v", err)
	}
	if acc.UsageConvention != "total" {
		t.Fatalf("usage_convention = %q, want total (write refused)", acc.UsageConvention)
	}
	if err := ValidateAccountInput(AccountInput{
		Name: "A", Namespace: "a", Kind: "openai-compatible",
		BaseURL: "https://x.invalid/v1", UsageConvention: "exclusive",
	}); err == nil || !strings.Contains(err.Error(), "usage_convention") {
		t.Fatalf("ValidateAccountInput error = %v, want a usage_convention refusal", err)
	}
}

func TestUpsertAccountPreservesUsageConvention(t *testing.T) {
	ctx := context.Background()
	db := openEventsDB(t, 4)
	defer db.Close()
	id, err := db.CreateAccount(ctx, AccountInput{
		Name: "Up", Namespace: "up", Kind: "openai-compatible",
		BaseURL: "https://example.invalid/v1", UsageConvention: "total",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// The registration/catalog path rewrites account rows around a refresh; it
	// must never reset the operator's usage fact.
	if _, err := db.UpsertAccount(ctx, provider.Account{
		Name: "up", Namespace: "up", Kind: provider.KindOpenAICompatible,
		BaseURL: "https://example.invalid/v1", Enabled: true,
	}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	acc, err := db.AccountByID(ctx, id)
	if err != nil {
		t.Fatalf("AccountByID: %v", err)
	}
	if acc.UsageConvention != "total" {
		t.Fatalf("usage_convention after catalog upsert = %q, want total", acc.UsageConvention)
	}
}

func TestUsageConventionColumnIsExplicitNotInferred(t *testing.T) {
	ctx := context.Background()
	db := openEventsDB(t, 4)
	defer db.Close()
	// The account KIND must never stand in for the fact: an account on an
	// anthropic-compatible upstream still starts at 'auto' (unknown), because
	// adapters and wiring are not evidence of vendor accounting.
	id, err := db.CreateAccount(ctx, AccountInput{
		Name: "Anth", Namespace: "anth", Kind: string(provider.KindAnthropicCompat),
		BaseURL: "https://example.invalid",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	acc, err := db.AccountByID(ctx, id)
	if err != nil {
		t.Fatalf("AccountByID: %v", err)
	}
	if acc.UsageConvention != provider.UsageConventionAuto {
		t.Fatalf("usage_convention = %q, want auto (never inferred from kind)", acc.UsageConvention)
	}
}
