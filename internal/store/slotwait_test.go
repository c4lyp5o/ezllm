package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSlotWaitRoundTrip(t *testing.T) {
	db, err := Open(context.Background(), Options{Path: filepath.Join(t.TempDir(), "l.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	wait := int64(4200)
	db.RecordCall(Call{TS: time.Now().UTC(), Client: "bridge", Account: "claude-sub", Model: "m", Status: 200, SlotWaitms: &wait})
	db.RecordCall(Call{TS: time.Now().UTC(), Client: "remote", Account: "ns", Model: "m", Status: 200})
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	rows, err := db.RecentCalls(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*int64{}
	for _, r := range rows {
		got[r.Client] = r.SlotWaitms
	}
	if got["bridge"] == nil || *got["bridge"] != 4200 {
		t.Fatalf("bridge slot_wait_ms = %v, want 4200", got["bridge"])
	}
	if got["remote"] != nil {
		t.Fatalf("non-bridge slot_wait_ms = %d, want NULL", *got["remote"])
	}
	if e := EventFromCall(Call{SlotWaitms: &wait}); e.SlotWaitMs == nil || *e.SlotWaitMs != 4200 {
		t.Fatalf("feed event slot wait = %v", e.SlotWaitMs)
	}
}

// v36 databases gain slot_wait_ms and turn_id; old rows stay NULL / "" (never measured).
func TestSlotWaitMigrationFromV36(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.sqlite")
	db, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	db.RecordCall(Call{TS: time.Now().UTC(), Client: "keeper", Account: "ns", Model: "m", Status: 200})
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"slot_wait_ms", "turn_id"} {
		if _, err := db.w.ExecContext(ctx, "ALTER TABLE calls DROP COLUMN "+col); err != nil {
			t.Fatalf("drop %s: %v", col, err)
		}
	}
	if _, err := db.w.ExecContext(ctx, `UPDATE schema_version SET version = 36`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("reopen/migrate: %v", err)
	}
	defer migrated.Close()
	if got := migrated.SchemaVersion(); got != schemaVersion {
		t.Fatalf("schema version = %d, want %d", got, schemaVersion)
	}
	rows, err := migrated.RecentCalls(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Client != "keeper" || rows[0].SlotWaitms != nil || rows[0].TurnID != "" {
		t.Fatalf("rows = %+v, want the old row with NULL slot wait and no turn id", rows)
	}
}
