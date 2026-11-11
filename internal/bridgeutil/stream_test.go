package bridgeutil

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

type blockedWriter struct {
	mu       sync.Mutex
	deadline time.Time
	notify   chan struct{}
	entered  chan struct{}
	once     sync.Once
}

func newBlockedWriter() *blockedWriter {
	return &blockedWriter{notify: make(chan struct{}, 1), entered: make(chan struct{})}
}
func (w *blockedWriter) Header() http.Header { return make(http.Header) }
func (w *blockedWriter) WriteHeader(int)     {}
func (w *blockedWriter) Flush()              {}
func (w *blockedWriter) SetWriteDeadline(d time.Time) error {
	w.mu.Lock()
	w.deadline = d
	w.mu.Unlock()
	select {
	case w.notify <- struct{}{}:
	default:
	}
	return nil
}
func (w *blockedWriter) Write([]byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	for {
		w.mu.Lock()
		d := w.deadline
		w.mu.Unlock()
		delay := time.Until(d)
		if d.IsZero() {
			delay = time.Hour
		}
		if delay <= 0 {
			return 0, os.ErrDeadlineExceeded
		}
		timer := time.NewTimer(delay)
		select {
		case <-w.notify:
			timer.Stop()
		case <-timer.C:
			return 0, os.ErrDeadlineExceeded
		}
	}
}
func TestStreamPreservesBytesAndFlushesBothWireFormats(t *testing.T) {
	for _, wire := range []string{"event: message_delta\ndata: {\"delta\":{\"text\":\"hi\"}}\n\n", "data: {\"choices\":[]}\n\ndata: [DONE]\n\n"} {
		w := httptest.NewRecorder()
		s := NewStream(context.Background(), w, time.Second)
		n, err := s.Write([]byte(wire))
		s.Close()
		if err != nil || n != len(wire) || w.Body.String() != wire || !w.Flushed {
			t.Fatalf("n=%d err=%v bytes=%q flush=%v", n, err, w.Body.String(), w.Flushed)
		}
	}
}
func TestStreamCancellationInterruptsWriteAndClearsDeadline(t *testing.T) {
	w := newBlockedWriter()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := NewStream(ctx, w, time.Hour)
	done := make(chan error, 1)
	go func() { _, err := s.Write([]byte("event")); done <- err }()
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("write not entered")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not unblock write")
	}
	s.Close()
	w.mu.Lock()
	d := w.deadline
	w.mu.Unlock()
	if !d.IsZero() {
		t.Fatal("deadline left on connection")
	}
	if _, err := s.Write([]byte("late")); !errors.Is(err, context.Canceled) {
		t.Fatalf("late write=%v", err)
	}
}
func TestStreamStallTimesOut(t *testing.T) {
	w := newBlockedWriter()
	s := NewStream(context.Background(), w, 20*time.Millisecond)
	defer s.Close()
	done := make(chan error, 1)
	go func() { _, err := s.Write([]byte("event")); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("write was not bounded")
	}
}

type failingWriter struct {
	*httptest.ResponseRecorder
	calls    int
	short    bool
	flushErr error
}

func (w *failingWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.short {
		return len(p) - 1, nil
	}
	return 0, io.ErrClosedPipe
}
func (w *failingWriter) FlushError() error { return w.flushErr }
func TestStreamFailureIsStickyAndShortWritesFail(t *testing.T) {
	for _, short := range []bool{false, true} {
		w := &failingWriter{ResponseRecorder: httptest.NewRecorder(), short: short}
		s := NewStream(context.Background(), w, time.Second)
		defer s.Close()
		_, err := s.Write([]byte("data"))
		want := io.ErrClosedPipe
		if short {
			want = io.ErrShortWrite
		}
		if !errors.Is(err, want) {
			t.Fatalf("err=%v", err)
		}
		if _, err := s.Write([]byte("[DONE]")); !errors.Is(err, want) || w.calls != 1 {
			t.Fatalf("wrote after failure: calls=%d err=%v", w.calls, err)
		}
	}
}
func TestStreamFlushFailurePropagates(t *testing.T) {
	w := &failingWriter{ResponseRecorder: httptest.NewRecorder(), flushErr: io.ErrClosedPipe}
	s := NewStream(context.Background(), w, time.Second)
	defer s.Close()
	if err := s.Flush(); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("flush=%v", err)
	}
	if _, err := s.Write([]byte("late")); !errors.Is(err, io.ErrClosedPipe) || w.calls != 0 {
		t.Fatal("write after failed flush")
	}
}
func TestStreamCancelledBeforeWriteDoesNotEmit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := httptest.NewRecorder()
	s := NewStream(ctx, w, time.Second)
	defer s.Close()
	if _, err := s.Write([]byte("event")); !errors.Is(err, context.Canceled) || w.Body.Len() != 0 {
		t.Fatal("wrote after cancellation")
	}
}

func TestStreamCloseJoinsCancellationBeforeClearingDeadline(t *testing.T) {
	for i := 0; i < 100; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		w := newBlockedWriter()
		s := NewStream(ctx, w, time.Hour)
		done := make(chan struct{})
		go func() { cancel(); close(done) }()
		s.Close()
		<-done
		w.mu.Lock()
		d := w.deadline
		w.mu.Unlock()
		if !d.IsZero() {
			t.Fatal("cancellation callback changed deadline after Close")
		}
	}
}
