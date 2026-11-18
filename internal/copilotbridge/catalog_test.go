package copilotbridge

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestModelCatalogRefresh(t *testing.T) {
	now := time.Now()
	calls := 0
	fail := true
	c := &ModelCatalog{Now: func() time.Time { return now }, Fetch: func(context.Context) ([]Model, error) {
		calls++
		if fail {
			return nil, errors.New("offline")
		}
		return []Model{{ID: "auto"}, {ID: "gpt-6.2-sol"}}, nil
	}}
	fallback := []Model{{ID: "old"}}
	got, live := c.Get(context.Background(), fallback)
	if live || got[0].ID != "old" {
		t.Fatal(got, live)
	}
	fail = false
	c.Get(context.Background(), fallback)
	if calls != 1 {
		t.Fatal(calls)
	}
	now = now.Add(2 * time.Minute)
	got, live = c.Get(context.Background(), fallback)
	if !live || len(got) != 2 || got[1].ID != "gpt-6.2-sol" {
		t.Fatal(got, live)
	}
	got[0].ID = "mutated"
	fail = true
	now = now.Add(2 * time.Minute)
	got, live = c.Get(context.Background(), fallback)
	if !live || got[0].ID != "auto" {
		t.Fatal(got, live)
	}
}
func TestModelCatalogConcurrent(t *testing.T) {
	calls := 0
	c := &ModelCatalog{Fetch: func(context.Context) ([]Model, error) { calls++; return []Model{{ID: "auto"}}, nil }}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.Get(context.Background(), nil) }()
	}
	wg.Wait()
	if calls != 1 {
		t.Fatal(calls)
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
