package copilotbridge

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestModelCatalogRefresh(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	var calls atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	c := &ModelCatalog{TTL: time.Minute, Now: clock, Fetch: func(context.Context) ([]Model, error) {
		calls.Add(1)
		if fail.Load() {
			return nil, errors.New("offline")
		}
		return []Model{{ID: "auto"}, {ID: "gpt-6.2-sol"}}, nil
	}}
	fallback := []Model{{ID: "old"}}
	got, live := c.Get(context.Background(), fallback)
	if live || got[0].ID != "old" {
		t.Fatal(got, live)
	}
	fail.Store(false)
	c.Get(context.Background(), fallback)
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
	advance(2 * time.Minute)
	c.Get(context.Background(), fallback) // starts the refresh in the background
	c.idle()
	got, live = c.Get(context.Background(), fallback)
	if !live || len(got) != 2 || got[1].ID != "gpt-6.2-sol" {
		t.Fatal(got, live)
	}
	got[0].ID = "mutated"
	fail.Store(true)
	advance(2 * time.Minute)
	c.Get(context.Background(), fallback)
	c.idle()
	got, live = c.Get(context.Background(), fallback)
	if !live || got[0].ID != "auto" {
		t.Fatal(got, live)
	}
}

// A refresh that hangs (a stuck CLI) must not hold up readers: they get the
// last good catalog at once. Before the fix Get held its lock across Fetch.
func TestModelCatalogRefreshDoesNotBlockReaders(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	release := make(chan struct{})
	var calls atomic.Int32
	c := &ModelCatalog{TTL: time.Minute, Now: clock, Fetch: func(context.Context) ([]Model, error) {
		if calls.Add(1) > 1 {
			<-release
		}
		return []Model{{ID: "auto"}, {ID: "m1"}}, nil
	}}
	if _, live := c.Get(context.Background(), nil); !live {
		t.Fatal("first fetch not served")
	}
	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()
	done := make(chan []Model, 2)
	for i := 0; i < 2; i++ { // the first starts the hung refresh, the second must not queue behind it
		go func() { m, _ := c.Get(context.Background(), nil); done <- m }()
	}
	for i := 0; i < 2; i++ {
		select {
		case m := <-done:
			if len(m) != 2 || m[1].ID != "m1" {
				t.Fatalf("stale catalog = %v", m)
			}
		case <-time.After(time.Second):
			close(release)
			t.Fatal("Get waited for a hung refresh")
		}
	}
	close(release)
	c.idle()
	if calls.Load() != 2 {
		t.Fatalf("fetches = %d, want 2 (one refresh at a time)", calls.Load())
	}
}

// Warm starts the first fetch without waiting for it.
func TestModelCatalogWarmDoesNotBlock(t *testing.T) {
	release := make(chan struct{})
	c := &ModelCatalog{Fetch: func(context.Context) ([]Model, error) {
		<-release
		return []Model{{ID: "auto"}}, nil
	}}
	start := time.Now()
	c.Warm()
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("Warm blocked")
	}
	close(release)
	c.idle()
	if _, live := c.Get(context.Background(), nil); !live {
		t.Fatal("warm fetch not kept")
	}
}

func TestModelCatalogConcurrent(t *testing.T) {
	var calls atomic.Int32
	c := &ModelCatalog{Fetch: func(context.Context) ([]Model, error) { calls.Add(1); return []Model{{ID: "auto"}}, nil }}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.Get(context.Background(), nil) }()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
}
func TestServerUsesLiveCatalog(t *testing.T) {
	s := &Server{Models: []Model{{ID: "obsolete"}}, ModelCatalog: &ModelCatalog{Fetch: func(context.Context) ([]Model, error) {
		return []Model{{ID: "auto"}, {ID: "mai-code-2"}}, nil
	}}}
	if !s.knownModel("mai-code-2") || s.knownModel("obsolete") {
		t.Fatal(s.catalog())
	}
}
