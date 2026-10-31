package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestToolUsageFreshSchema(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "fresh.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// The claim is "migrated up to the binary's version" — the constant, not
	// a literal (the v10 lesson).
	if got := db.SchemaVersion(); got != schemaVersion {
		t.Fatalf("schema version = %d, want %d", got, schemaVersion)
	}
	if err := db.UpsertToolUsage(ctx, []ToolUsageDelta{{Client: "c", Tool: "T", SchemaTokens: 7, ShippedRequests: 1, ShippedSessions: 1}}); err != nil {
		t.Fatalf("upsert on fresh schema: %v", err)
	}
	rows, err := db.ListToolUsage(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list = %v rows, err %v", len(rows), err)
	}
}

// A v32 database (no tool_usage, no calls.tool_prune_shadow_tokens) must
// migrate up with data intact — a fresh DB never executes the upgrade
// branch, so this is the test that proves the ALTER loop.
func TestToolUsageMigrationFromV32(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.sqlite")
	db, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateAccount(ctx, AccountInput{
		Name: "Keeper", Namespace: "keeper", Kind: "openai-compatible",
		BaseURL: "https://example.invalid/v1",
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if err := db.UpsertToolUsage(ctx, []ToolUsageDelta{{Client: "c", Tool: "Keep", SchemaTokens: 3, ShippedRequests: 2, ShippedSessions: 1}}); err != nil {
		t.Fatalf("seed tool usage: %v", err)
	}
	// Regress to v32: drop the new column, drop the new table, stamp 32.
	if _, err := db.w.ExecContext(ctx, "ALTER TABLE calls DROP COLUMN tool_prune_shadow_tokens"); err != nil {
		t.Fatalf("drop column: %v", err)
	}
	if _, err := db.w.ExecContext(ctx, "DROP TABLE tool_usage"); err != nil {
		t.Fatalf("drop tool_usage: %v", err)
	}
	if _, err := db.w.ExecContext(ctx, `UPDATE schema_version SET version = 32`); err != nil {
		t.Fatalf("stamp v32: %v", err)
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
	has, err := hasColumn(ctx, migrated.w, "calls", "tool_prune_shadow_tokens")
	if err != nil || !has {
		t.Fatalf("calls.tool_prune_shadow_tokens missing after migrate (has=%v err=%v)", has, err)
	}
	// Data preservation: the pre-regression account survives the upgrade.
	accts, err := migrated.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	var kept bool
	for _, a := range accts {
		if a.Namespace == "keeper" {
			kept = true
		}
	}
	if !kept {
		t.Fatal("seed account lost in migration")
	}
}

func TestUpsertToolUsageAccumulates(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "acc.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.UpsertToolUsage(ctx, []ToolUsageDelta{{
		Client: "c", Tool: "T", SchemaTokens: 10, ShippedRequests: 1, ShippedSessions: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	// Second sighting: request counter grows, the session pair does not.
	if err := db.UpsertToolUsage(ctx, []ToolUsageDelta{{
		Client: "c", Tool: "T", SchemaTokens: 12, ShippedRequests: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	// Third: usage flip carries its stamp.
	if err := db.UpsertToolUsage(ctx, []ToolUsageDelta{{
		Client: "c", Tool: "T", ShippedRequests: 1, UsedSessions: 1, LastUsedAt: "2026-10-31T00:00:00.000Z",
	}}); err != nil {
		t.Fatal(err)
	}
	// Fourth: no usage stamp must NOT move last_used_at (COALESCE).
	if err := db.UpsertToolUsage(ctx, []ToolUsageDelta{{
		Client: "c", Tool: "T", ShippedRequests: 1,
	}}); err != nil {
		t.Fatal(err)
	}

	rows, err := db.ListToolUsage(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list = %v rows, err %v", len(rows), err)
	}
	r := rows[0]
	if r.ShippedRequests != 4 || r.ShippedSessions != 1 || r.UsedSessions != 1 {
		t.Errorf("counts = req %d sess %d used %d, want 4/1/1", r.ShippedRequests, r.ShippedSessions, r.UsedSessions)
	}
	if r.SchemaTokens != 12 {
		t.Errorf("schema_tokens = %d, want last-seen 12", r.SchemaTokens)
	}
	if r.LastUsedAt != "2026-10-31T00:00:00.000Z" {
		t.Errorf("last_used_at = %q, want the usage stamp to stick", r.LastUsedAt)
	}
	if r.FirstSeenAt == "" || r.LastSeenAt == "" {
		t.Errorf("first/last seen must be stamped, got %q/%q", r.FirstSeenAt, r.LastSeenAt)
	}
}

func TestToolUsageForClientFilters(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "flt.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.UpsertToolUsage(ctx, []ToolUsageDelta{
		{Client: "one", Tool: "A", ShippedRequests: 1, ShippedSessions: 1},
		{Client: "two", Tool: "A", ShippedRequests: 1, ShippedSessions: 1},
		{Client: "two", Tool: "B", ShippedRequests: 1, ShippedSessions: 1},
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.ToolUsageForClient(ctx, "two")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 for client two", len(rows))
	}
	if _, ok := rows["A"]; !ok {
		t.Error("rows must be keyed by tool name")
	}
}
