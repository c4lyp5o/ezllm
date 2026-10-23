package server

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// Audit C6 (background half): detached probes and refresh loops ran on their
// own context.Background() timeouts, so shutdown neither cancelled nor waited
// for them before closing the store. StopBackground must do both.
func TestStopBackgroundCancelsAndWaits(t *testing.T) {
	s := New(Options{})
	started := make(chan struct{})
	var finished atomic.Bool
	s.goBackground(func(ctx context.Context) {
		close(started)
		<-ctx.Done()                      // a probe blocked on its upstream
		time.Sleep(20 * time.Millisecond) // its wrap-up before the store write
		finished.Store(true)
	})
	<-started
	if !s.StopBackground(2 * time.Second) {
		t.Fatal("StopBackground timed out on a job that honours cancellation")
	}
	if !finished.Load() {
		t.Fatal("StopBackground returned before the job finished")
	}
	ran := false
	s.goBackground(func(context.Context) { ran = true })
	time.Sleep(20 * time.Millisecond)
	if ran {
		t.Fatal("a job started after StopBackground")
	}
}

// The real wiring: a combo auto-probe stuck on a hanging upstream ends at
// StopBackground. Before, the probe ran on its own context.Background()
// timeouts and kept the store busy past shutdown.
func TestStopBackgroundEndsAStuckAutoProbe(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		select { // an upstream that never answers
		case <-r.Context().Done():
		case <-release:
		}
	}, func(o *Options) { o.ComboAutoProbe = true })
	t.Cleanup(func() { close(release) }) // before the harness closes the upstream
	acct := seededAccountID(t, h)
	if err := h.db.UpsertModels(context.Background(), acct, []provider.ModelInfo{{ID: "fresh"}}); err != nil {
		t.Fatal(err)
	}
	code, resp := jsonDo(t, h, "POST", "/admin/combos", "tok-admin", map[string]any{
		"name": "stuck", "strategy": "failover", "enabled": true,
		"hops": []map[string]any{{"account_id": acct, "model_id": "fresh"}},
	})
	if code != http.StatusCreated {
		t.Fatalf("create combo: %d %v", code, resp)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("auto-probe never reached the upstream")
	}
	if !h.server.StopBackground(3 * time.Second) {
		t.Fatal("StopBackground timed out: the stuck probe ignored shutdown")
	}
}
