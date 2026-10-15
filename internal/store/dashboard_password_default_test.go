package store

// docx item 13: the login page shows the first-run STRONGPASSWORD hint only
// while the stored hash still matches the factory password — the flag is the
// whole feature, so both states must be proven, not inferred.

import (
	"context"
	"database/sql"
	"testing"
)

func TestDashboardPasswordIsDefaultTracksChange(t *testing.T) {
	db := openEventsDB(t, 4)
	defer db.Close()
	ctx := context.Background()

	if err := db.EnsureDashboardPassword(ctx); err != nil {
		t.Fatal(err)
	}
	if !db.DashboardPasswordIsDefault(ctx) {
		t.Fatalf("fresh bootstrap must still be the default password")
	}

	if err := db.ChangeDashboardPassword(ctx, DefaultDashboardPassword, "correct horse battery staple"); err != nil {
		t.Fatalf("change: %v", err)
	}
	if db.DashboardPasswordIsDefault(ctx) {
		t.Fatalf("after a real change the hash must no longer match STRONGPASSWORD")
	}
}

func TestDashboardPasswordIsDefaultFailsClosedOnMissingRow(t *testing.T) {
	db := openEventsDB(t, 4)
	defer db.Close()
	// openEventsDB bootstraps the factory row (store.Open runs
	// EnsureDashboardPassword); remove it to model a store with no
	// dashboard_auth state at all.
	if err := db.WithWriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(), `DELETE FROM dashboard_auth`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if db.DashboardPasswordIsDefault(context.Background()) {
		t.Fatalf("no dashboard_auth row must report false (fail closed)")
	}
}
