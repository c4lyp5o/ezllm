package store

import (
	"context"
	"testing"
)

func TestDashboardPasswordBootstrapAndChange(t *testing.T) {
	db := openEventsDB(t, 4)
	defer db.Close()
	ctx := context.Background()

	// Open initializes the default dashboard password for every caller, not
	// only the command binary.
	token, err := db.DashboardLogin(ctx, DefaultDashboardPassword)
	if err != nil {
		t.Fatalf("login with bootstrap password: %v", err)
	}
	if !db.DashboardSession(ctx, token) {
		t.Fatal("newly issued dashboard session should be valid")
	}

	var storedHash string
	if err := db.Reader().QueryRowContext(ctx, `SELECT password_hash FROM dashboard_auth WHERE id=1`).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if storedHash == DefaultDashboardPassword {
		t.Fatal("dashboard password must not be stored plaintext")
	}

	const replacement = "new-testing-password-2026"
	if err := db.ChangeDashboardPassword(ctx, "wrong-current-password", replacement); err != ErrInvalidDashboardPassword {
		t.Fatalf("change with wrong current password = %v, want ErrInvalidDashboardPassword", err)
	}
	if err := db.ChangeDashboardPassword(ctx, DefaultDashboardPassword, replacement); err != nil {
		t.Fatalf("change password: %v", err)
	}
	if db.DashboardSession(ctx, token) {
		t.Fatal("password change must revoke existing dashboard sessions")
	}
	if _, err := db.DashboardLogin(ctx, DefaultDashboardPassword); err != ErrInvalidDashboardPassword {
		t.Fatalf("old password login error = %v, want ErrInvalidDashboardPassword", err)
	}
	newToken, err := db.DashboardLogin(ctx, replacement)
	if err != nil {
		t.Fatalf("login with replacement password: %v", err)
	}
	if !db.DashboardSession(ctx, newToken) {
		t.Fatal("replacement password should issue a valid dashboard session")
	}
}

func TestProviderKeysUsePlaintextColumnOnly(t *testing.T) {
	db := openEventsDB(t, 4)
	defer db.Close()
	ctx := context.Background()

	plainExists, err := hasColumn(ctx, db.Writer(), "provider_keys", "key_plain")
	if err != nil {
		t.Fatal(err)
	}
	if !plainExists {
		t.Fatal("fresh schema must include provider_keys.key_plain")
	}
	cipherExists, err := hasColumn(ctx, db.Writer(), "provider_keys", "key_ct")
	if err != nil {
		t.Fatal(err)
	}
	if cipherExists {
		t.Fatal("fresh schema must not include obsolete provider_keys.key_ct")
	}
}
