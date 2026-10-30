package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

const channelMaster = "notification-channel-test-master"

func openCrypto(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), Options{Path: filepath.Join(t.TempDir(), "c.sqlite"), MasterSecret: channelMaster})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestNotificationChannelSecretsStayEncrypted(t *testing.T) {
	db := openCrypto(t)
	ctx := context.Background()
	const hook = "https://discord.com/api/webhooks/123/very-secret-token"
	id, err := db.CreateNotificationChannel(ctx, NotificationChannel{Kind: "discord", Name: "ops room", Enabled: true, MinSeverity: "warn"},
		map[string]string{"webhook_url": hook, "unused": "  "})
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := db.r.QueryRowContext(ctx, `SELECT secrets FROM notification_channels WHERE id=?`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, cryptoPrefix) || strings.Contains(raw, "very-secret") {
		t.Fatalf("secrets at rest = %q, want enc:v1 ciphertext", raw)
	}
	c, err := db.NotificationChannel(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if c.Secrets()["webhook_url"] != hook || strings.Join(c.SecretsSet, ",") != "webhook_url" {
		t.Fatalf("decrypted %v set %v", c.Secrets(), c.SecretsSet)
	}
	b, _ := json.Marshal(c)
	if strings.Contains(string(b), "very-secret") || !strings.Contains(string(b), `"secrets_set":["webhook_url"]`) {
		t.Fatalf("JSON carries a secret or hides which are set: %s", b)
	}
}

func TestNotificationChannelLifecycle(t *testing.T) {
	db := openCrypto(t)
	ctx := context.Background()
	id, err := db.CreateNotificationChannel(ctx, NotificationChannel{Kind: "ntfy", Name: "phone", Enabled: true, MinSeverity: "warn",
		Config: map[string]string{"topic": "ezllm-ops"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordChannelDelivery(ctx, id, "the channel answered 502"); err != nil {
		t.Fatal(err)
	}
	c, _ := db.NotificationChannel(ctx, id)
	if c.LastStatus != "failed" || c.LastError != "the channel answered 502" || c.LastSentAt == nil || len(c.SecretsSet) != 0 {
		t.Fatalf("after a failed delivery: %+v", c)
	}
	c.Name, c.MinSeverity, c.Config = "phone 2", "error", map[string]string{"topic": "ezllm-ops-2"}
	if err := db.UpdateNotificationChannel(ctx, c, map[string]string{"token": "tk_1"}); err != nil {
		t.Fatal(err)
	}
	c, _ = db.NotificationChannel(ctx, id)
	if c.Name != "phone 2" || c.MinSeverity != "error" || c.Config["topic"] != "ezllm-ops-2" || c.Secrets()["token"] != "tk_1" ||
		c.LastStatus != "" || c.LastSentAt != nil {
		t.Fatalf("after an edit (status must reset: it described the old destination): %+v", c)
	}
	if err := db.DeleteNotificationChannel(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.NotificationChannel(ctx, id); err != ErrNotFound {
		t.Fatalf("deleted channel: %v", err)
	}
	if err := db.DeleteNotificationChannel(ctx, id); err != ErrNotFound {
		t.Fatalf("second delete: %v", err)
	}
	if err := db.UpdateNotificationChannel(ctx, NotificationChannel{ID: 999, Name: "x", MinSeverity: "warn"}, nil); err != ErrNotFound {
		t.Fatalf("update of a missing channel: %v", err)
	}
}

func TestNotificationChannelCap(t *testing.T) {
	db := openCrypto(t)
	ctx := context.Background()
	for i := 0; i < MaxNotificationChannels; i++ {
		if _, err := db.CreateNotificationChannel(ctx, NotificationChannel{Kind: "ntfy", Name: "n", MinSeverity: "warn", Config: map[string]string{"topic": "t"}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.CreateNotificationChannel(ctx, NotificationChannel{Kind: "ntfy", Name: "n", MinSeverity: "warn"}, nil); err == nil || !strings.Contains(err.Error(), "at most 10") {
		t.Fatalf("eleventh channel: %v", err)
	}
}

func TestNotifyOutcome(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	in := NotificationInput{Kind: NoteCatalogRefreshFailed, Severity: "warn", Title: "refresh failed", DedupKey: "catalog:7"}
	for i, want := range []NotifyResult{NotifyCreated, NotifyFolded, NotifyFolded} {
		got, err := db.NotifyOutcome(ctx, in)
		if err != nil || got != want {
			t.Fatalf("event %d: %v %v, want %v", i, got, err, want)
		}
	}
	if err := db.MarkNotificationsRead(ctx, nil, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.NotifyOutcome(ctx, in); got != NotifyCreated {
		t.Fatalf("after reading, a repeat is a new row: got %v", got)
	}
	if got, _ := db.NotifyOutcome(ctx, NotificationInput{Kind: "x", Title: "no dedup key"}); got != NotifyCreated {
		t.Fatalf("no dedup key: got %v", got)
	}
}

// A v31 database gains the table on upgrade and keeps its notifications.
func TestNotificationChannelsMigrationFromV31(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v31.sqlite")
	db, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Notify(ctx, NotificationInput{Kind: "x", Title: "kept"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Writer().Exec(`DROP TABLE notification_channels; DELETE FROM schema_version WHERE version>31; INSERT OR IGNORE INTO schema_version(version) VALUES(31)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if db.SchemaVersion() != 32 {
		t.Fatalf("schema %d, want 32", db.SchemaVersion())
	}
	if list, err := db.ListNotificationChannels(ctx); err != nil || len(list) != 0 {
		t.Fatalf("channels after upgrade: %v %v", list, err)
	}
	if rows, _, err := db.ListNotifications(ctx, 10); err != nil || len(rows) != 1 {
		t.Fatalf("notifications after upgrade: %v %v", rows, err)
	}
}
