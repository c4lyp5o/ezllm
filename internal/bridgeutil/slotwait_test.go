package bridgeutil

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// A request that queued behind a held slot reports the wait in the response
// header, and the header is set before the status goes out.
func TestTrackSlotWaitReportsTheQueueTime(t *testing.T) {
	s := NewSlots(1, 4)
	hold, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(60*time.Millisecond, hold)

	rec := httptest.NewRecorder()
	w, r := TrackSlotWait(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	release, err := s.Acquire(r.Context())
	if err != nil {
		t.Fatal(err)
	}
	release()
	_, _ = w.Write([]byte("ok")) // implicit 200 must carry the header too

	ms, err := strconv.ParseInt(rec.Header().Get(SlotWaitHeader), 10, 64)
	if err != nil {
		t.Fatalf("header %q: %v", rec.Header().Get(SlotWaitHeader), err)
	}
	if ms < 50 || ms > 5000 {
		t.Fatalf("slot wait = %d ms, want about 60", ms)
	}
}

// Routes that never take a slot (models, status) send no header, so the
// ledger keeps NULL instead of a false 0.
func TestTrackSlotWaitOmitsTheHeaderWithoutAcquire(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := TrackSlotWait(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	w.WriteHeader(http.StatusOK)
	if v, ok := rec.Header()[SlotWaitHeader]; ok {
		t.Fatalf("header present without Acquire: %v", v)
	}
}

// A refusal still reports the (near-zero) time spent deciding: the ledger row
// for a queue_full 429 shows the bridge answered, not that it never ran.
func TestTrackSlotWaitCoversARefusal(t *testing.T) {
	s := NewSlots(1, 1)
	hold, _ := s.Acquire(context.Background())
	defer hold()
	waiting := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { close(waiting); _, _ = s.Acquire(ctx) }()
	<-waiting
	for s.Queued() == 0 {
		time.Sleep(time.Millisecond)
	}
	rec := httptest.NewRecorder()
	w, r := TrackSlotWait(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if _, err := s.Acquire(r.Context()); err != ErrQueueFull {
		t.Fatalf("err = %v, want ErrQueueFull", err)
	}
	w.WriteHeader(http.StatusTooManyRequests)
	if rec.Header().Get(SlotWaitHeader) == "" {
		t.Fatal("refusal carried no slot-wait header")
	}
}

// The wrapper must not hide Flush or SetWriteDeadline from Stream, which
// reaches them through http.ResponseController.
func TestTrackSlotWaitKeepsFlush(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := TrackSlotWait(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if err := http.NewResponseController(w).Flush(); err != nil {
		t.Fatalf("Flush through the wrapper: %v", err)
	}
	if !rec.Flushed {
		t.Fatal("recorder was not flushed")
	}
}
