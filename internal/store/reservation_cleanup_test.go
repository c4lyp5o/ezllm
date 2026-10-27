package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestDroppedCallCleanupIsBoundedAndReleases(t *testing.T) {
	before := droppedCalls.Load()
	defer droppedCalls.Store(before)
	// A stalled consumer must not create a goroutine per dropped call.
	blocked := &DB{callCh: make(chan ledgerItem, 1), reservationCleanup: make(chan int64, 2)}
	blocked.callCh <- ledgerItem{}
	for i := int64(1); i <= 1000; i++ {
		blocked.RecordCall(Call{ReservationID: i})
	}
	if got := len(blocked.reservationCleanup); got != 2 {
		t.Fatalf("cleanup queue length = %d", got)
	}

	db := openAt(t, filepath.Join(t.TempDir(), "cleanup.sqlite"))
	defer db.Close()
	ctx := context.Background()
	result, err := db.w.ExecContext(ctx, `INSERT INTO client_budget_reservations (client, bucket, tokens, created_at, expires_at) VALUES ('test', 'x', 1, '2026-01-01', '2099-01-01')`)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	db.reservationCleanup <- id
	deadline := time.Now().Add(2 * time.Second)
	for {
		var count int
		if err := db.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM client_budget_reservations WHERE id=?`, id).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("queued reservation was not released")
		}
		time.Sleep(time.Millisecond)
	}
}
