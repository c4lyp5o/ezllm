package server

import (
	"context"
	"sync"
	"time"
)

// background owns the server's detached work — catalog and price refresh
// loops, protocol sweeps, auto-probes — so shutdown can end it before the
// store closes. Each job derives its contexts from ctx instead of
// context.Background(), so StopBackground reaches a probe mid-flight.
type background struct {
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex // orders go against stop: no job starts after stop
	closed bool
	wg     sync.WaitGroup
}

func newBackground() *background {
	ctx, cancel := context.WithCancel(context.Background())
	return &background{ctx: ctx, cancel: cancel}
}

// goBackground runs fn on a tracked goroutine with the server's background
// context. After StopBackground it runs nothing.
func (s *Server) goBackground(fn func(ctx context.Context)) {
	b := s.bg
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.wg.Add(1)
	b.mu.Unlock()
	go func() {
		defer b.wg.Done()
		fn(b.ctx)
	}()
}

// StopBackground cancels every background job and waits up to d for them to
// return. False means some were still running at the deadline (their store
// writes may then fail against a closing database).
func (s *Server) StopBackground(d time.Duration) bool {
	b := s.bg
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.cancel()
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// either returns a context done when a or b is; stop releases the link.
func either(a, b context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(a)
	unlink := context.AfterFunc(b, cancel)
	return ctx, func() { unlink(); cancel() }
}
