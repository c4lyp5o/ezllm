package store

import (
	"context"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// UpdateKeyPlain is the gemini-web rotator's persistence path: the rotated
// __Secure-1PSIDTS pair replaces the stored credential IN PLACE (same row,
// refreshed hint), and PickKey must then observe the NEW plaintext.
func TestUpdateKeyPlainRotatedCookie(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	id, err := db.UpsertAccount(ctx, provider.Account{
		Name: "gem", Namespace: "gem", Kind: provider.KindGeminiWeb,
		BaseURL: "internal://geminiweb", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	keyID, _, err := db.AddKey(ctx, id, "primary", "__Secure-1PSID=old\n__Secure-1PSIDTS=oldts")
	if err != nil {
		t.Fatal(err)
	}
	rotated := "__Secure-1PSID=old\n__Secure-1PSIDTS=freshts"
	if err := db.UpdateKeyPlain(ctx, keyID, rotated); err != nil {
		t.Fatal(err)
	}
	pick, err := db.PickKey(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if pick.Plaintext != rotated {
		t.Errorf("PickKey after rotation = %q, want the rotated pair", pick.Plaintext)
	}
	if pick.ID != keyID {
		t.Errorf("PickKey id = %d, want %d (the same row, updated in place)", pick.ID, keyID)
	}
	keys, err := db.ListKeys(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("ListKeys = %+v (rotation must not create rows)", keys)
	}
}
