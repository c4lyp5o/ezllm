package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func seedSecrets(t *testing.T, path, secret string) int64 {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: path, MasterSecret: secret})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id, err := db.CreateAccount(ctx, AccountInput{Name: "Acme", Namespace: "acme", Kind: "openai-compatible",
		BaseURL: "https://api.tokenrouter.com/v1", ManagementKey: "sk-mgmt-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.AddKey(ctx, id, "main", "sk-provider-secret"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateNotificationChannel(ctx, NotificationChannel{Kind: "webhook", Name: "hook", Enabled: true, MinSeverity: "info", Config: map[string]string{}},
		map[string]string{"url": "https://example.com/hook-secret"}); err != nil {
		t.Fatal(err)
	}
	return id
}

// Every secret opens under the new key after a rotation, and not under the old.
func TestRotateMasterKeyRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ezllm.sqlite")
	id := seedSecrets(t, path, "old-master-secret")
	res, err := RotateMasterKey(context.Background(), path, "old-master-secret", "new-master-secret")
	if err != nil {
		t.Fatal(err)
	}
	if res.ProviderKeys != 1 || res.ManagementKeys != 1 || res.ChannelSecrets != 1 {
		t.Fatalf("rotated %+v, want one of each", res)
	}
	ctx := context.Background()
	if _, err := Open(ctx, Options{Path: path, MasterSecret: "old-master-secret"}); err == nil {
		t.Fatal("the old key still opens the rotated database")
	}
	db, err := Open(ctx, Options{Path: path, MasterSecret: "new-master-secret"})
	if err != nil {
		t.Fatalf("the new key does not open the database: %v", err)
	}
	defer db.Close()
	acct, err := db.AccountByID(ctx, id)
	if err != nil || acct.ManagementKey != "sk-mgmt-secret" {
		t.Fatalf("management key = %q, %v", acct.ManagementKey, err)
	}
	pick, err := db.PickKey(ctx, id)
	if err != nil || pick.Plaintext != "sk-provider-secret" {
		t.Fatalf("provider key = %q, %v", pick.Plaintext, err)
	}
	chans, err := db.ListNotificationChannels(ctx)
	if err != nil || len(chans) != 1 || chans[0].secrets["url"] != "https://example.com/hook-secret" {
		t.Fatalf("channel secrets: %+v %v", chans, err)
	}
}

// One value the old key cannot open stops the rotation, and nothing changes.
func TestRotateMasterKeyIsAllOrNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ezllm.sqlite")
	seedSecrets(t, path, "old-master-secret")
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: path, MasterSecret: "old-master-secret"})
	if err != nil {
		t.Fatal(err)
	}
	other, _ := NewFieldCrypto("some-other-key")
	alien, _ := other.Encrypt("written under another key")
	if _, err := db.w.ExecContext(ctx, `UPDATE notification_channels SET secrets=?`, alien); err != nil {
		t.Fatal(err)
	}
	var before string
	_ = db.r.QueryRowContext(ctx, `SELECT key_plain FROM provider_keys`).Scan(&before)
	db.Close()

	if _, err := RotateMasterKey(ctx, path, "old-master-secret", "new-master-secret"); err == nil || !strings.Contains(err.Error(), "notification_channels") {
		t.Fatalf("err = %v, want a refusal naming the bad row", err)
	}
	db, err = Open(ctx, Options{Path: path, MasterSecret: "old-master-secret"})
	if err != nil {
		t.Fatalf("the old key no longer opens the database after a failed rotation: %v", err)
	}
	defer db.Close()
	var after string
	_ = db.r.QueryRowContext(ctx, `SELECT key_plain FROM provider_keys`).Scan(&after)
	if after != before {
		t.Fatal("a failed rotation changed the provider key")
	}
}

func TestRotateMasterKeyRefusesTheSameKey(t *testing.T) {
	if _, err := RotateMasterKey(context.Background(), filepath.Join(t.TempDir(), "x.sqlite"), "same", "same"); err == nil {
		t.Fatal("rotating to the same key must be refused")
	}
}
