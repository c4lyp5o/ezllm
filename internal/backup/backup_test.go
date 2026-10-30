package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

const testMaster = "active-environment-master-key-for-tests"
const testSecret = "independent-backup-encryption-secret-for-tests"

func backupDB(t *testing.T) *store.DB {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "live.sqlite"), MasterSecret: testMaster})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	id, err := db.UpsertAccount(ctx, provider.Account{Name: "test", Namespace: "test", Kind: provider.KindOpenAICompatible, BaseURL: "https://example.invalid/v1", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = db.AddKey(ctx, id, "test", "provider-secret-test-value"); err != nil {
		t.Fatal(err)
	}
	if err = db.CompleteDashboardSetup(ctx, "test-dashboard-password"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.DashboardLogin(ctx, "test-dashboard-password"); err != nil {
		t.Fatal(err)
	}
	return db
}
func TestSettingsBackupWALVerifyRestoreAndRetention(t *testing.T) {
	db := backupDB(t)
	ctx := context.Background()
	dir := t.TempDir()
	m := New(db, dir, testSecret, testMaster)
	db.RecordCall(store.Call{Client: "test", Model: "m", TokensIn: 7})
	// Keep committing unrelated WAL rows while the online snapshot runs.
	writerCtx, stopWriter := context.WithCancel(ctx)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-writerCtx.Done():
				return
			case <-ticker.C:
				db.RecordCall(store.Call{Client: "concurrent", Model: "m", TokensIn: 1})
			}
		}
	}()
	defer func() { stopWriter(); <-writerDone }()

	for i := 0; i < 2; i++ {
		if err := m.Reserve(); err != nil {
			t.Fatal(err)
		}
		if err := m.Reserve(); err == nil {
			t.Fatal("overlapping backup accepted")
		}
		if err := m.RunReserved(ctx, 1); err != nil {
			t.Fatal(err)
		}
	}
	status := m.Status()
	if status.Running || status.LastOutcome != "success" || len(status.Backups) != 1 {
		t.Fatal(status)
	}
	restarted := New(db, dir, testSecret, testMaster)
	if restarted.Status().LastOutcome != "success" {
		t.Fatal("backup status did not survive manager restart")
	}
	archive := filepath.Join(dir, status.Backups[0].ID+".tar.gz.age")
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), testMaster) || strings.Contains(string(raw), "SQLite format") {
		t.Fatal("plaintext backup exposed")
	}
	if _, err = Verify(ctx, archive, testSecret); err != nil {
		t.Fatal(err)
	}
	if _, err = Verify(ctx, archive, "incorrect-backup-secret"); err == nil {
		t.Fatal("wrong secret accepted")
	}
	broken := filepath.Join(dir, "truncated.age")
	if err = os.WriteFile(broken, raw[:len(raw)-20], 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Verify(ctx, broken, testSecret); err == nil {
		t.Fatal("truncated archive accepted")
	}
	output := filepath.Join(t.TempDir(), "restored")
	if err = Restore(ctx, archive, testSecret, output); err != nil {
		t.Fatal(err)
	}
	if err = Restore(ctx, archive, testSecret, output); err == nil {
		t.Fatal("existing destination overwritten")
	}
	restored, err := store.Open(ctx, store.Options{Path: filepath.Join(output, "ezllm.sqlite"), MasterSecret: testMaster})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	rows, err := restored.ListDashboardSessions(ctx, "")
	if err != nil || len(rows) != 0 {
		t.Fatal("restored sessions not revoked", rows, err)
	}
	var calls int
	if err = restored.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM calls WHERE client='test' AND tokens_in=7`).Scan(&calls); err != nil || calls != 1 {
		t.Fatal("WAL ledger missing", calls, err)
	}
	// A failed or cancelled job cannot prune the successful backup.
	if err = m.Reserve(); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err = m.RunReserved(cancelled, 1); err == nil {
		t.Fatal("cancelled job succeeded")
	}
	if len(m.Status().Backups) != 1 {
		t.Fatal("failure pruned backup")
	}
}
func TestSettingsBackupWrongMasterAndUnavailableStorage(t *testing.T) {
	db := backupDB(t)
	ctx := context.Background()
	m := New(db, t.TempDir(), testSecret, "wrong-master-key")
	if err := m.Reserve(); err != nil {
		t.Fatal(err)
	}
	if err := m.RunReserved(ctx, 7); err == nil {
		t.Fatal("wrong master key verified")
	}
	if len(m.Status().Backups) != 0 {
		t.Fatal("invalid backup published")
	}
	if err := New(db, t.TempDir(), testMaster, testMaster).Check(); err == nil {
		t.Fatal("reused master secret accepted")
	}
	file := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(file, []byte("occupied"), 0600)
	if err := New(db, file, testSecret, testMaster).Check(); err == nil {
		t.Fatal("file accepted as destination")
	}
}

// A notification channel's secret is a credential: a backup whose key cannot
// decrypt it must not verify, even with no provider key to trip on.
func TestSettingsBackupVerifiesNotificationChannelSecrets(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "live.sqlite"), MasterSecret: testMaster})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err = db.CreateNotificationChannel(ctx, store.NotificationChannel{Kind: "discord", Name: "ops", Enabled: true, MinSeverity: "warn"},
		map[string]string{"webhook_url": "https://discord.com/api/webhooks/1/secret-part"}); err != nil {
		t.Fatal(err)
	}
	if err = db.CompleteDashboardSetup(ctx, "test-dashboard-password"); err != nil {
		t.Fatal(err)
	}
	bad := New(db, t.TempDir(), testSecret, "wrong-master-key")
	if err := bad.Reserve(); err != nil {
		t.Fatal(err)
	}
	if err := bad.RunReserved(ctx, 7); err == nil {
		t.Fatal("a backup that cannot decrypt the channel secret verified")
	}
	good := New(db, t.TempDir(), testSecret, testMaster)
	if err := good.Reserve(); err != nil {
		t.Fatal(err)
	}
	if err := good.RunReserved(ctx, 7); err != nil {
		t.Fatalf("a good backup failed: %v", err)
	}
}
