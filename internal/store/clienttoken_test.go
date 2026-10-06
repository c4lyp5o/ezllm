package store

import (
	"context"
	"strings"
	"testing"
)

// Boot seeding calls UpsertClientToken for every config entry, every boot. The
// schema has token_hash UNIQUE alongside name, so an upsert keyed only on name
// breaks when the incoming plaintext already belongs to another row: SQLite
// raises "UNIQUE constraint failed: client_tokens.token_hash" BEFORE the
// ON CONFLICT(name) clause can apply, and main() aborts startup. This happened
// for real on restart.
func TestUpsertClientTokenSurvivesHashConflict(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	if err := db.UpsertClientToken(ctx, "hermes", "tok-alpha", "infer,admin"); err != nil {
		t.Fatal(err)
	}
	// Same plaintext, different config name — the boot case that used to fail.
	if err := db.UpsertClientToken(ctx, "claude-code", "tok-alpha", "infer"); err != nil {
		t.Fatalf("second name reusing a token must not abort boot: %v", err)
	}
	// The original name's row must have moved aside rather than duplicated.
	var n int
	if err := db.Reader().QueryRow(
		`SELECT COUNT(*) FROM client_tokens WHERE token_hash=?`, HashToken("tok-alpha")).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("rows holding the same hash = %d, want 1 (names must not duplicate)", n)
	}
	// The hash now resolves to whichever name owns it — the conflict handler
	// re-pointed it, so it must be exactly one of ours, never both.
	name, _, err := db.ClientByToken(ctx, "tok-alpha")
	if err != nil {
		t.Fatalf("ClientByToken: %v", err)
	}
	if name != "hermes" && name != "claude-code" {
		t.Errorf("owner = %q, want one of the seeded names", name)
	}

	// Re-seed with an unchanged token: the common path, must stay idempotent.
	if err := db.UpsertClientToken(ctx, "claude-code", "tok-alpha", "infer"); err != nil {
		t.Fatalf("re-seed must be idempotent: %v", err)
	}
	if err := db.UpsertClientToken(ctx, "hermes", "tok-alpha", "infer,admin"); err != nil {
		t.Fatalf("re-seed must be idempotent: %v", err)
	}

	// Rotating one name to a NEW value keeps both tokens working.
	if err := db.UpsertClientToken(ctx, "hermes", "tok-beta", "infer,admin"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ClientByToken(ctx, "tok-beta"); err != nil {
		t.Errorf("rotated token must resolve: %v", err)
	}
	// Claiming a token that another name currently holds moves it, and the
	// displaced row is deleted — it is unreachable by definition, since its own
	// name can no longer resolve it. This is the deliberate cost of sharing one
	// token across two config entries, and the test pins it so a future change
	// cannot silently resurrect a duplicate.
	if err := db.UpsertClientToken(ctx, "solo", "tok-alpha", "infer"); err != nil {
		t.Fatalf("claim another name's token must not fail: %v", err)
	}
	owner, _, err := db.ClientByToken(ctx, "tok-alpha")
	if err != nil {
		t.Fatalf("tok-alpha must still resolve: %v", err)
	}
	if owner != "solo" {
		t.Errorf("owner = %q, want solo", owner)
	}
	var still int
	if err := db.Reader().QueryRow(
		`SELECT COUNT(*) FROM client_tokens WHERE token_hash=?`, HashToken("tok-alpha")).Scan(&still); err != nil {
		t.Fatal(err)
	}
	if still != 1 {
		t.Errorf("rows holding tok-alpha = %d, want exactly 1", still)
	}
}

// Regression guard: the old failure mode was a raw SQLite constraint error, which
// main() wrapped as "client token %q: ...". Nothing may still bubble that up.
func TestUpsertClientTokenNoConstraintLeak(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	if err := db.UpsertClientToken(ctx, "a", "shared-token", "infer"); err != nil {
		t.Fatal(err)
	}
	err := db.UpsertClientToken(ctx, "b", "shared-token", "infer")
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("constraint error escaped the upsert: %v", err)
	}
}
