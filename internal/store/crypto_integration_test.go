package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func testAccount(t *testing.T, db *DB) int64 {
	t.Helper()
	id, err := db.UpsertAccount(context.Background(), provider.Account{
		Name:      "Test",
		Namespace: "test",
		Kind:      provider.KindOpenAICompatible,
		BaseURL:   "https://example.invalid/v1",
		Enabled:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAddKeyEncryptsAtRestAndPickKeyDecrypts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.sqlite")
	ctx := context.Background()

	db, err := Open(ctx, Options{Path: path, MasterSecret: "test-master"})
	if err != nil {
		t.Fatal(err)
	}
	id := testAccount(t, db)
	keyID, _, err := db.AddKey(ctx, id, "primary", "sk-test-secret-value")
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := db.r.QueryRowContext(ctx, `SELECT key_plain FROM provider_keys WHERE id=?`, keyID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, cryptoPrefix) {
		t.Fatalf("key_plain was not encrypted: %q", raw)
	}
	pick, err := db.PickKey(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if pick.Plaintext != "sk-test-secret-value" {
		t.Fatalf("decrypted key = %q", pick.Plaintext)
	}
	_ = db.Close()

	// Same master key on a fresh open: still readable.
	db2, err := Open(ctx, Options{Path: path, MasterSecret: "test-master"})
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	pick, err = db2.PickKey(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if pick.Plaintext != "sk-test-secret-value" {
		t.Fatalf("reopened decrypted key = %q", pick.Plaintext)
	}
}

func TestOpenEncryptsLegacyPlaintextRows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.sqlite")
	ctx := context.Background()

	// First boot without a master key: keys are stored in plaintext (legacy).
	db, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	id := testAccount(t, db)
	keyID, _, err := db.AddKey(ctx, id, "legacy", "sk-legacy-plain")
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	// Next boot with a master key: the migration must rewrite it in place.
	db2, err := Open(ctx, Options{Path: path, MasterSecret: "test-master"})
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	var raw string
	if err := db2.r.QueryRowContext(ctx, `SELECT key_plain FROM provider_keys WHERE id=?`, keyID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, cryptoPrefix) {
		t.Fatalf("legacy key was not encrypted: %q", raw)
	}
	pick, err := db2.PickKey(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if pick.Plaintext != "sk-legacy-plain" {
		t.Fatalf("legacy key decrypted to %q", pick.Plaintext)
	}
}

func TestPickKeyFailsWithWrongMasterKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.sqlite")
	ctx := context.Background()

	db, err := Open(ctx, Options{Path: path, MasterSecret: "right"})
	if err != nil {
		t.Fatal(err)
	}
	id := testAccount(t, db)
	if _, _, err := db.AddKey(ctx, id, "primary", "sk-test-secret-value"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	db2, err := Open(ctx, Options{Path: path, MasterSecret: "wrong"})
	if err != nil {
		// A wrong key may fail during the migration pass instead; either way
		// the ciphertext must never be returned as plaintext.
		return
	}
	defer db2.Close()
	if _, err := db2.PickKey(ctx, id); err == nil {
		t.Fatal("PickKey succeeded with the wrong master key")
	}
}
