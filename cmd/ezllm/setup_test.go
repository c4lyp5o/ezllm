package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

func openSetupDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), store.Options{
		Path: filepath.Join(t.TempDir(), "s.sqlite"), BatchSize: 4, BatchWait: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

var quietLog = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestPrepareDashboardSetupMintsCodeOnFirstBoot(t *testing.T) {
	t.Setenv(envDashboardPassword, "")
	t.Setenv(envDashboardPasswordFile, "")
	db := openSetupDB(t)
	code, err := prepareDashboardSetup(context.Background(), db, quietLog)
	if err != nil || len(code) != 9 {
		t.Fatalf("first boot without env = %q, %v; want a XXXX-XXXX code", code, err)
	}
	if first, _ := db.DashboardFirstBoot(context.Background()); !first {
		t.Fatal("minting a code must not complete setup")
	}
}

func TestPrepareDashboardSetupFromEnvSkipsSetup(t *testing.T) {
	t.Setenv(envDashboardPassword, "env-supplied-password")
	db := openSetupDB(t)
	code, err := prepareDashboardSetup(context.Background(), db, quietLog)
	if err != nil || code != "" {
		t.Fatalf("env password = %q, %v; want no code", code, err)
	}
	if _, err := db.DashboardLogin(context.Background(), "env-supplied-password"); err != nil {
		t.Fatalf("env password must log in: %v", err)
	}

	// A later boot with a different env value never overwrites it.
	t.Setenv(envDashboardPassword, "different-env-password")
	if code, err := prepareDashboardSetup(context.Background(), db, quietLog); err != nil || code != "" {
		t.Fatalf("second boot = %q, %v", code, err)
	}
	if _, err := db.DashboardLogin(context.Background(), "env-supplied-password"); err != nil {
		t.Fatalf("original password must survive a changed env value: %v", err)
	}
}

func TestPrepareDashboardSetupFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(path, []byte("file-supplied-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envDashboardPassword, "")
	t.Setenv(envDashboardPasswordFile, path)
	db := openSetupDB(t)
	if code, err := prepareDashboardSetup(context.Background(), db, quietLog); err != nil || code != "" {
		t.Fatalf("file password = %q, %v", code, err)
	}
	if _, err := db.DashboardLogin(context.Background(), "file-supplied-password"); err != nil {
		t.Fatalf("file password (newline trimmed) must log in: %v", err)
	}
}

func TestPrepareDashboardSetupRejectsShortEnvPassword(t *testing.T) {
	t.Setenv(envDashboardPassword, "short")
	db := openSetupDB(t)
	if _, err := prepareDashboardSetup(context.Background(), db, quietLog); err == nil {
		t.Fatal("a too-short env password must fail the boot, not be silently ignored")
	}
}
