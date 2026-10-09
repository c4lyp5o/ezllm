package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func ptrTrue() *bool { b := true; return &b }

// seedPinAccount creates one account with one model row and returns its id.
func seedPinAccount(t *testing.T, db *DB) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := db.CreateAccount(ctx, AccountInput{
		Name: "pin-test", Namespace: "pinns", Kind: "openai-compatible",
		BaseURL: "https://example.invalid/v1", Enabled: ptrTrue(),
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if err := db.UpsertModels(ctx, id, []provider.ModelInfo{{ID: "m1"}}); err != nil {
		t.Fatalf("UpsertModels: %v", err)
	}
	return id
}

// Fresh databases carry the column and a pin round-trips through set/read/clear.
func TestModelPinRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openAt(t, filepath.Join(t.TempDir(), "pin.sqlite"))
	defer db.Close()
	acct := seedPinAccount(t, db)

	if got := db.SchemaVersion(); got != schemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", got, schemaVersion)
	}
	if pin, err := db.ModelPin(ctx, acct, "m1"); err != nil || pin != "" {
		t.Fatalf("fresh model pin = %q err=%v, want empty", pin, err)
	}
	if err := db.SetModelPin(ctx, acct, "m1", "responses"); err != nil {
		t.Fatalf("SetModelPin: %v", err)
	}
	if pin, _ := db.ModelPin(ctx, acct, "m1"); pin != "responses" {
		t.Fatalf("pin = %q, want responses", pin)
	}
	if err := db.SetModelPin(ctx, acct, "m1", ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if pin, _ := db.ModelPin(ctx, acct, "m1"); pin != "" {
		t.Fatalf("after clear pin = %q, want empty", pin)
	}
}

// Setting a pin on a model the account does not have is a not-found, not a
// silent no-op that would leave the operator thinking it took effect.
func TestSetModelPinUnknownModelIsNotFound(t *testing.T) {
	db := openAt(t, filepath.Join(t.TempDir(), "pin2.sqlite"))
	defer db.Close()
	acct := seedPinAccount(t, db)
	if err := db.SetModelPin(context.Background(), acct, "nope", "openai"); err != ErrNotFound {
		t.Fatalf("unknown model: err = %v, want ErrNotFound", err)
	}
}

// A pin survives a catalog re-sync: UpsertModels only owns display_name/owned_by.
func TestModelPinSurvivesResync(t *testing.T) {
	ctx := context.Background()
	db := openAt(t, filepath.Join(t.TempDir(), "pin3.sqlite"))
	defer db.Close()
	acct := seedPinAccount(t, db)
	if err := db.SetModelPin(ctx, acct, "m1", "anthropic"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertModels(ctx, acct, []provider.ModelInfo{{ID: "m1"}}); err != nil {
		t.Fatal(err)
	}
	if pin, _ := db.ModelPin(ctx, acct, "m1"); pin != "anthropic" {
		t.Fatalf("pin after resync = %q, want anthropic", pin)
	}
}

// A v6 database (no proto_pin) upgrades to v7 in place, keeping its rows.
func TestMigrateV6AddsProtoPinKeepsRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v6.sqlite")
	// Build a fresh DB, then drop proto_pin and rewind the version to 6 to
	// reproduce the pre-v7 shape the migration must upgrade.
	seed := openAt(t, path)
	acct := seedPinAccount(t, seed)
	seed.Close()

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`ALTER TABLE models DROP COLUMN proto_pin`); err != nil {
		t.Fatalf("drop proto_pin: %v", err)
	}
	if _, err := raw.Exec(`UPDATE schema_version SET version = 6`); err != nil {
		t.Fatalf("rewind version: %v", err)
	}
	raw.Close()

	db := openAt(t, path)
	defer db.Close()
	if got := db.SchemaVersion(); got != 7 {
		t.Fatalf("SchemaVersion = %d, want 7", got)
	}
	ok, err := hasColumn(context.Background(), db.Writer(), "models", "proto_pin")
	if err != nil || !ok {
		t.Fatalf("proto_pin column present = %v err=%v, want true", ok, err)
	}
	var n int
	if err := db.Reader().QueryRow(`SELECT COUNT(*) FROM models WHERE account_id=?`, acct).Scan(&n); err != nil || n != 1 {
		t.Fatalf("model rows after upgrade = %d err=%v, want 1", n, err)
	}
}
