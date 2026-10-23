package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestAccountingMeterRecoverySurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "recovery.sqlite")
	db, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	aid := seedAccountForRule(t, db, "recovery")
	// Pin the rule's window timezone: an empty WinTZ defaults to
	// Asia/Kuala_Lumpur, so the meter buckets "daily" in KL while this test
	// reads the bucket back in UTC. West of UTC that is the same day and the
	// test passed by luck; east of it (this box, UTC+8) the assertion looked
	// for yesterday's bucket and read zero. The meter was right; only the
	// test's assumption was wrong.
	if _, err = db.UpsertModelRule(ctx, ModelRule{AccountID: aid, ModelID: "m", CapWindow: "daily", WinTZ: "UTC", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	id, err := db.AddClientToken(ctx, "recovery", "token", "infer")
	if err != nil {
		t.Fatal(err)
	}
	cap := int64(100)
	if err = db.UpdateToken(ctx, id, nil, nil, &cap, "daily", nil, nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	call := Call{TS: now, Client: "recovery", AccountID: aid, Model: "m", TokensOut: 1}
	if err = db.insertCalls(ctx, []Call{call}); err != nil {
		t.Fatal(err)
	}
	reservation, ok, err := db.ReserveClientBudget(ctx, "recovery", 99)
	if err != nil || !ok {
		t.Fatalf("reserve: %t %v", ok, err)
	}
	if _, err = db.w.Exec(`CREATE TRIGGER audit_meter_abort BEFORE INSERT ON client_usage_counters WHEN NEW.tokens >= 99 BEGIN SELECT RAISE(ABORT,'meter fault'); END`); err != nil {
		t.Fatal(err)
	}
	call.TokensOut = 99
	call.ReservationID = reservation
	if err = db.insertCalls(ctx, []Call{call}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ClientBudgetStatus(ctx, "recovery"); err == nil {
		t.Fatal("uncounted budget was readable during meter failure")
	}
	if _, _, err = db.ReserveClientBudget(ctx, "recovery", 1); err == nil {
		t.Fatal("reservation did not fail closed")
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.w.Exec(`DROP TRIGGER audit_meter_abort`); err != nil {
		t.Fatal(err)
	}
	b, err := db.ClientBudgetStatus(ctx, "recovery")
	if err != nil || b.Used != 100 {
		t.Fatalf("recovered budget=%+v err=%v", b, err)
	}
	c, err := db.UsageFor(ctx, aid, "m", CountBucket(now, "daily", time.UTC))
	if err != nil || c.Tokens != 100 {
		t.Fatalf("recovered model=%+v err=%v", c, err)
	}
	if _, ok, err = db.ReserveClientBudget(ctx, "recovery", 1); err != nil || ok {
		t.Fatalf("spent cap readmitted: %t %v", ok, err)
	}
	// A second recovery/read and a normal flush must never replay the debit twice.
	if err = db.insertCalls(ctx, []Call{{TS: now, Client: "recovery", AccountID: aid, Model: "m", TokensOut: 2}}); err != nil {
		t.Fatal(err)
	}
	b, err = db.ClientBudgetStatus(ctx, "recovery")
	if err != nil || b.Used != 102 {
		t.Fatalf("double replay: %+v %v", b, err)
	}
	var pending, reserved int
	if err = db.r.QueryRow(`SELECT COUNT(*) FROM pending_usage_meter`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if err = db.r.QueryRow(`SELECT COUNT(*) FROM client_budget_reservations`).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if pending != 0 || reserved != 0 {
		t.Fatalf("pending=%d reserved=%d", pending, reserved)
	}
}

func TestAccountingMeterMigrationFrom27(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "migration.sqlite")
	db, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.insertCalls(ctx, []Call{{TS: time.Now().UTC(), Model: "preserved", TokensIn: 7}}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.w.Exec(`DROP TABLE pending_usage_meter`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.w.Exec(`DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES(27)`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if db.SchemaVersion() != 28 {
		t.Fatalf("schema=%d", db.SchemaVersion())
	}
	var count int
	if err = db.r.QueryRow(`SELECT COUNT(*) FROM calls WHERE model='preserved' AND tokens_in=7`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("migration lost ledger: %d %v", count, err)
	}
	if err = db.r.QueryRow(`SELECT COUNT(*) FROM pending_usage_meter`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("pending=%d %v", count, err)
	}
}
