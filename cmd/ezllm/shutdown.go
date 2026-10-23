package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// requestLifetime owns every request handler so shutdown can end them before
// the store closes. A timed-out http.Server.Shutdown has NOT finished its
// active handlers; closing the ledger under them loses their rows while the
// callers still see 200s.
type requestLifetime struct {
	ctx      context.Context
	cancel   context.CancelFunc
	inflight sync.WaitGroup
}

func newRequestLifetime() *requestLifetime {
	ctx, cancel := context.WithCancel(context.Background())
	return &requestLifetime{ctx: ctx, cancel: cancel}
}

// baseContext is the server's BaseContext: every request context derives from
// the lifetime, so cancel reaches handlers blocked on an upstream.
func (l *requestLifetime) baseContext(net.Listener) context.Context { return l.ctx }

// track counts a handler as in flight until it returns.
func (l *requestLifetime) track(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.inflight.Add(1)
		defer l.inflight.Done()
		h.ServeHTTP(w, r)
	})
}

// drain stops srv in two phases: a graceful Shutdown within drain, then, if
// handlers are still running, cancel their contexts, close the connections,
// and wait up to abort for the handlers to return (and record their rows).
// Callers close the store only after drain returns.
func (l *requestLifetime) drain(srv *http.Server, drain, abort time.Duration, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), drain)
	defer cancel()
	err := srv.Shutdown(ctx)
	if err == nil {
		return
	}
	log.Error("http shutdown: drain budget exceeded; cancelling in-flight requests", "err", err)
	l.cancel()
	_ = srv.Close()
	done := make(chan struct{})
	go func() { l.inflight.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(abort):
		log.Error("http shutdown: handlers still running after cancel; their ledger rows are counted as dropped", "waited", abort)
	}
}
