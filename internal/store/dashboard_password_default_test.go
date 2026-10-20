package store

// First-boot setup (v26): no factory password exists. A fresh store is in
// first_boot until CompleteDashboardSetup runs once; a database still on the
// legacy published password is sent back through setup on open.

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestDashboardFreshStoreIsFirstBoot(t *testing.T) {
	db := openEventsDB(t, 4)
	defer db.Close()
	ctx := context.Background()

	first, err := db.DashboardFirstBoot(ctx)
	if err != nil || !first {
		t.Fatalf("fresh store first_boot = %v (%v), want true", first, err)
	}
	// Nothing logs in before setup — not even the old factory password.
	for _, pw := range []string{legacyDefaultPassword, "", "anything-at-all-123"} {
		if _, err := db.DashboardLogin(ctx, pw); !errors.Is(err, ErrDashboardSetupRequired) {
			t.Fatalf("login(%q) before setup = %v, want ErrDashboardSetupRequired", pw, err)
		}
	}
}

func TestDashboardSetupIsOneShot(t *testing.T) {
	db := openEventsDB(t, 4)
	defer db.Close()
	ctx := context.Background()

	if err := db.CompleteDashboardSetup(ctx, "short"); !errors.Is(err, ErrDashboardPasswordTooShort) {
		t.Fatalf("short password = %v, want ErrDashboardPasswordTooShort", err)
	}
	if first, _ := db.DashboardFirstBoot(ctx); !first {
		t.Fatal("a refused setup must leave first_boot set")
	}
	if err := db.CompleteDashboardSetup(ctx, "first-real-password"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if first, _ := db.DashboardFirstBoot(ctx); first {
		t.Fatal("setup must flip first_boot")
	}
	if err := db.CompleteDashboardSetup(ctx, "attacker-password-123"); !errors.Is(err, ErrDashboardSetupDone) {
		t.Fatalf("second setup = %v, want ErrDashboardSetupDone", err)
	}
	if _, err := db.DashboardLogin(ctx, "attacker-password-123"); !errors.Is(err, ErrInvalidDashboardPassword) {
		t.Fatalf("second setup's password must not log in: %v", err)
	}
	if _, err := db.DashboardLogin(ctx, "first-real-password"); err != nil {
		t.Fatalf("setup password must log in: %v", err)
	}
}

func TestDashboardLegacyDefaultPasswordReturnsToSetup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	ctx := context.Background()
	open := func() *DB {
		db, err := Open(ctx, Options{Path: path, BatchSize: 4, BatchWait: 20 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		return db
	}

	// Model a pre-v26 install: the factory hash, first_boot=0, a live session.
	db := open()
	hash, err := bcrypt.GenerateFromPassword([]byte(legacyDefaultPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE dashboard_auth SET password_hash=?, first_boot=0 WHERE id=1`, string(hash)); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO dashboard_sessions (token_hash, expires_at) VALUES ('legacy', '2999-01-01T00:00:00Z')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db = open()
	defer db.Close()
	if first, _ := db.DashboardFirstBoot(ctx); !first {
		t.Fatal("a store still on the published factory password must return to first-boot setup")
	}
	var n int
	if err := db.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM dashboard_sessions`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("legacy sessions = %d (%v), want revoked", n, err)
	}
	if _, err := db.DashboardLogin(ctx, legacyDefaultPassword); !errors.Is(err, ErrDashboardSetupRequired) {
		t.Fatalf("legacy password login = %v, want ErrDashboardSetupRequired", err)
	}
}

func TestDashboardRealPasswordSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kept.sqlite")
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: path, BatchSize: 4, BatchWait: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteDashboardSetup(ctx, "operator-chosen-pass"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db, err = Open(ctx, Options{Path: path, BatchSize: 4, BatchWait: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if first, _ := db.DashboardFirstBoot(ctx); first {
		t.Fatal("a real password must not send the store back to setup")
	}
	if _, err := db.DashboardLogin(ctx, "operator-chosen-pass"); err != nil {
		t.Fatalf("login after reopen: %v", err)
	}
}
