// Package bridgeutil contains protocol-independent support for CLI bridges.
package bridgeutil

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// StreamWriteStall bounds one write/flush to a client that stopped reading.
const StreamWriteStall = 60 * time.Second

// Stream writes already-encoded bytes and flushes them without interpreting
// events or deciding when headers/completion should be sent. One handler owns
// writes. Context cancellation can concurrently interrupt a blocked write.
type Stream struct {
	ctx       context.Context
	w         http.ResponseWriter
	rc        *http.ResponseController
	stall     time.Duration
	mu        sync.Mutex
	cancelled bool
	stop      func() bool
	done      chan struct{}
	err       error
}

func NewStream(ctx context.Context, w http.ResponseWriter, stall time.Duration) *Stream {
	if stall <= 0 {
		stall = StreamWriteStall
	}
	s := &Stream{ctx: ctx, w: w, rc: http.NewResponseController(w), stall: stall, done: make(chan struct{})}
	s.stop = context.AfterFunc(ctx, func() {
		defer close(s.done)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.cancelled = true
		_ = s.rc.SetWriteDeadline(time.Now())
	})
	return s
}

func (s *Stream) arm() error {
	if s.err != nil {
		return s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelled || s.ctx.Err() != nil {
		return s.ctx.Err()
	}
	err := s.rc.SetWriteDeadline(time.Now().Add(s.stall))
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}
func (s *Stream) fail(err error) error {
	if err != nil && s.ctx.Err() != nil {
		err = s.ctx.Err()
	}
	if s.err == nil {
		s.err = err
	}
	return s.err
}

// Write flushes each complete encoded event supplied by its adapter. A failed
// write is sticky: no later event or terminal marker is written after failure.
func (s *Stream) Write(p []byte) (int, error) {
	if err := s.arm(); err != nil {
		return 0, s.fail(err)
	}
	n, err := s.w.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return n, s.fail(err)
	}
	return n, s.Flush()
}
func (s *Stream) Flush() error {
	if err := s.arm(); err != nil {
		return s.fail(err)
	}
	err := s.rc.Flush()
	if errors.Is(err, http.ErrNotSupported) {
		err = nil
	}
	return s.fail(err)
}

// Close must run after the handler's final write. Join cancellation before
// clearing the deadline, so its callback cannot modify a reused connection.
func (s *Stream) Close() {
	if !s.stop() {
		<-s.done
	}
	_ = s.rc.SetWriteDeadline(time.Time{})
}
