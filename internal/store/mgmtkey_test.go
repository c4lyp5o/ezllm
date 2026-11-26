package store

import (
	"context"
	"testing"
)

// A stored management key that cannot be decrypted is dropped, never handed
// to a quota call as the raw ciphertext.
func TestManagementKeyFailsClosed(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	id, err := d.CreateAccount(ctx, AccountInput{Name: "acme", Namespace: "acme", Kind: "openai-compatible", BaseURL: "https://api.example.com/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.w.ExecContext(ctx, `UPDATE accounts SET management_key_plain=? WHERE id=?`, cryptoPrefix+"damaged", id); err != nil {
		t.Fatal(err)
	}
	a, err := d.AccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if a.ManagementKey != "" {
		t.Fatalf("management key = %q, want it dropped", a.ManagementKey)
	}
	b, err := d.AccountByNamespace(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if b.ManagementKey != "" {
		t.Fatalf("by namespace: management key = %q, want it dropped", b.ManagementKey)
	}
}
