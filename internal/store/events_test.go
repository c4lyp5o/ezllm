package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openEventsDB(t *testing.T, batchSize int) *DB {
	t.Helper()
	db, err := Open(context.Background(), Options{
		Path:      filepath.Join(t.TempDir(), "e.sqlite"),
		MasterKey: "0123456789abcdef0123456789abcdef",
		BatchSize: batchSize,
		BatchWait: time.Hour, // flush on size or explicit Flush(), so batches are deterministic
	})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestSubscribeLedgerReceivesCommittedBatches(t *testing.T) {
	db := openEventsDB(t, 2)
	defer db.Close()

	ch, cancel := db.SubscribeLedger(8)
	defer cancel()

	db.RecordCall(Call{AccountID: 1, Account: "a", Model: "m1", Status: 200})
	db.RecordCall(Call{AccountID: 1, Account: "a", Model: "m2", Status: 200})
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}

	select {
	case batch := <-ch:
		if len(batch) != 2 {
			t.Fatalf("batch = %d rows, want 2", len(batch))
		}
		for _, c := range batch {
			if c.Model == "" {
				t.Fatal("published row lost its fields")
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no batch published after commit")
	}
}

func TestSubscribeLedgerUnsubscribeStopsDelivery(t *testing.T) {
	db := openEventsDB(t, 1)
	defer db.Close()

	ch, cancel := db.SubscribeLedger(4)
	db.RecordCall(Call{Model: "x", Status: 200})
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	<-ch // it was live first
	cancel()

	db.RecordCall(Call{Model: "y", Status: 200})
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	select {
	case b, open := <-ch:
		// The channel is closed by cancel(); a receive then yields (nil, false).
		// Only a successful OPEN receive means a post-cancel delivery slipped through.
		if open {
			t.Fatalf("received after cancel: %+v", b)
		}
	case <-time.After(150 * time.Millisecond):
	}
}

func TestPublishDropsForSlowSubscriber(t *testing.T) {
	db := openEventsDB(t, 1)
	defer db.Close()

	// buffer 1, never read: commits must drop for this subscriber, not block.
	ch, cancel := db.SubscribeLedger(1)
	defer cancel()
	for i := 0; i < 30; i++ {
		recDone := make(chan struct{})
		go func() { db.RecordCall(Call{Model: "m", Status: 200}); close(recDone) }()
		select {
		case <-recDone:
		case <-time.After(time.Second):
			t.Fatalf("RecordCall blocked at row %d — publish is not drop-on-full", i)
		}
		// drain occasionally so the loop keeps up; the unread backlog proves drops
		if i%10 == 9 {
			<-ch
		}
	}
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
}
