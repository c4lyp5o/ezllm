package bridgeutil

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"
)

func waitQueued(t *testing.T, s *Slots, n int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for s.Queued() != n {
		if time.Now().After(deadline) {
			t.Fatalf("queued = %d, want %d", s.Queued(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSlotsBoundTheQueueAndCount(t *testing.T) {
	s := NewSlots(1, 1)
	release, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		r, err := s.Acquire(context.Background())
		if err == nil {
			r()
		}
		got <- err
	}()
	waitQueued(t, s, 1)
	if _, err := s.Acquire(context.Background()); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("third caller err = %v, want ErrQueueFull", err)
	}
	release()
	release() // idempotent: must not free a slot twice
	if err := <-got; err != nil {
		t.Fatalf("queued caller err = %v", err)
	}
	st := s.Stats()
	want := map[string]any{"in_flight": int64(0), "queued": int64(0), "max_queue": 1, "max_concurrent": 1, "served": int64(2), "rejected_queue_full": int64(1)}
	for k, v := range want {
		if st[k] != v {
			t.Errorf("%s = %v, want %v", k, st[k], v)
		}
	}
}

func TestSlotsQueuedCallerLeavesOnCancel(t *testing.T) {
	s := NewSlots(1, 4)
	release, _ := s.Acquire(context.Background())
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() { _, err := s.Acquire(ctx); got <- err }()
	waitQueued(t, s, 1)
	cancel()
	if err := <-got; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if s.Queued() != 0 {
		t.Fatalf("queued = %d after cancel", s.Queued())
	}
}

func TestSlotsDefaultsAndHold(t *testing.T) {
	s := NewSlots(0, 0)
	if s.Cap() != 1 || s.Stats()["max_queue"] != DefaultMaxQueue {
		t.Fatalf("defaults = %v", s.Stats())
	}
	// Hold waits for a slot regardless of the queue bound and counts nothing.
	release, _ := s.Acquire(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.Hold(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Hold on a busy slot err = %v", err)
	}
	release()
	held, err := s.Hold(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	held()
	if st := s.Stats(); st["served"] != int64(1) || st["rejected_queue_full"] != int64(0) {
		t.Fatalf("Hold touched the counters: %v", st)
	}
}

func TestAuthorized(t *testing.T) {
	for _, tc := range []struct {
		token, bearer, xkey string
		allowX, want        bool
	}{
		{"t", "t", "", false, true},
		{"t", "x", "", false, false},
		{"t", "", "t", false, false}, // x-api-key ignored unless allowed
		{"t", "", "t", true, true},
		{"t", "t", "wrong", true, false}, // a present x-api-key is the credential
		{"", "", "", false, false},       // no configured token refuses all
		{"", "", "", true, false},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		if tc.bearer != "" {
			r.Header.Set("Authorization", "Bearer "+tc.bearer)
		}
		if tc.xkey != "" {
			r.Header.Set("x-api-key", tc.xkey)
		}
		if got := Authorized(r, tc.token, tc.allowX); got != tc.want {
			t.Errorf("%+v: got %v", tc, got)
		}
	}
}
