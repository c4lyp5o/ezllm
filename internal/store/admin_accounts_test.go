package store

import (
	"context"
	"encoding/json"
	"testing"
)

func TestListAccountsEmptyKeysSerializeAsArray(t *testing.T) {
	db := openEventsDB(t, 4)
	defer db.Close()
	ctx := context.Background()
	_, err := db.CreateAccount(ctx, AccountInput{
		Name: "fresh", Namespace: "fresh", Kind: "openai-compatible",
		BaseURL: "https://provider.example/v1", QuotaMode: "probe", CapWindow: "monthly",
	})
	if err != nil {
		t.Fatal(err)
	}

	accounts, err := db.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Fatalf("got %d accounts, want 1", len(accounts))
	}
	if accounts[0].Keys == nil {
		t.Fatal("fresh account keys slice is nil")
	}
	got, err := json.Marshal(accounts[0])
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatal(err)
	}
	keys, ok := payload["keys"].([]any)
	if !ok || len(keys) != 0 {
		t.Fatalf("serialized keys = %#v, want empty array", payload["keys"])
	}
}
