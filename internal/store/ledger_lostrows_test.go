package store

// F6 regression: a failed flush used to discard the whole batch with a single
// log line and no counter. `dropped_rows` — the number an operator watches to
// answer "am I losing rows?" — counts only the queue-full drop path, so a
// transient SQLite lock or a full disk lost rows while that number read 0.
//
// The fix adds a second atomic counter (lost_rows, via LostRows) incremented
// by the batch size on failure, plus one bounded retry before discarding.

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestFailedFlushIncrementsLostRows(t *testing.T) {
	t.Cleanup(func() { lostRows.Store(0) })
	lostRows.Store(0)

	dir := t.TempDir()
	db, err := Open(context.Background(), Options{
		Path:      filepath.Join(dir, "t.sqlite"),
		BatchSize: 4,
		BatchWait: time.Hour, // the explicit Flush barrier is what drives this test
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Kill the writer so EVERY flush fails — attempt and bounded retry alike.
	// This is the deterministic stand-in for a full disk or a stuck lock.
	if err := db.w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	beforeLost := LostRows()
	beforeDropped := DroppedCalls()
	for i := 0; i < 3; i++ {
		db.RecordCall(Call{Client: "test", Model: "m", Status: 200})
	}
	if err := db.Flush(); err != nil {
		t.Fatalf("flush barrier: %v", err)
	}

	lost := LostRows() - beforeLost
	if lost != 3 {
		t.Fatalf("lost_rows delta = %d, want 3 — a failed flush must be visible, not silent", lost)
	}

	// The queue-full path stays independent: this failure must NOT have been
	// booked as a backpressure drop. If dropped moved instead, the operator's
	// "am I losing rows?" answer still disagrees with reality.
	if dropped := DroppedCalls() - beforeDropped; dropped != 0 {
		t.Fatalf("dropped_rows moved by %d — a flush failure must be counted as lost, not dropped", dropped)
	}
}

// A healthy store must never move the counter: rows that persist are not lost.
func TestHealthyFlushLeavesLostRowsAtZero(t *testing.T) {
	t.Cleanup(func() { lostRows.Store(0) })
	lostRows.Store(0)

	dir := t.TempDir()
	db, err := Open(context.Background(), Options{
		Path:      filepath.Join(dir, "t.sqlite"),
		BatchSize: 2,
		BatchWait: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	for i := 0; i < 5; i++ {
		db.RecordCall(Call{Client: "test", Model: "m", Status: 200})
	}
	if err := db.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := LostRows(); got != 0 {
		t.Fatalf("lost_rows = %d on a healthy flush, want 0", got)
	}
}
