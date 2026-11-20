package copilotbridge

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ListModels asks the authenticated CLI catalog without creating a session or
// sending a prompt. No model inference is performed.
func (r *Runner) ListModels(ctx context.Context) ([]Model, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var result struct {
		Models []Model `json:"models"`
	}
	err := r.withChannel(ctx, func(c *rpcConn) error {
		if err := c.ping(); err != nil {
			return err
		}
		return c.call("models.list", map[string]any{}, &result)
	})
	if err != nil {
		return nil, err
	}
	out := []Model{{ID: "auto", Object: "model", OwnedBy: "copilot-cli"}}
	seen := map[string]bool{"auto": true}
	for _, m := range result.Models {
		if !modelIDPattern.MatchString(m.ID) {
			return nil, fmt.Errorf("copilot catalog: invalid model id")
		}
		if !seen[m.ID] {
			seen[m.ID] = true
			out = append(out, Model{ID: m.ID, Object: "model", OwnedBy: "copilot-cli"})
		}
	}
	if len(out) == 1 {
		return nil, fmt.Errorf("copilot catalog: empty model list")
	}
	return out, nil
}

// Catalog refresh timing. A refresh spawns a CLI process outside the bridge's
// slots, so it runs rarely: every catalogTTL after a success, catalogRetry
// after a failure.
const (
	catalogTTL   = 10 * time.Minute
	catalogRetry = time.Minute
	// catalogFirstWait bounds how long a caller waits for the very first fetch
	// (the boot warm-up normally finishes it before traffic arrives). Later
	// refreshes never make a caller wait.
	catalogFirstWait = 5 * time.Second
)

// ModelCatalog keeps the last successful live catalog and refreshes it in the
// background. Get never holds the lock across a fetch and never waits for a
// refresh once a first fetch has finished: a slow or hung CLI costs callers
// nothing but a stale list. At most one fetch runs at a time.
type ModelCatalog struct {
	mu    sync.Mutex
	Fetch func(context.Context) ([]Model, error)
	TTL   time.Duration // refresh interval after a success; 0 = catalogTTL
	Now   func() time.Time

	models   []Model
	next     time.Time
	live     bool
	loaded   bool          // a fetch has finished at least once
	fetching chan struct{} // closed when the running fetch ends; nil when idle
}

// Get returns the live catalog when one has been fetched, else fallback, and
// the live flag. A due refresh starts in the background.
func (c *ModelCatalog) Get(ctx context.Context, fallback []Model) ([]Model, bool) {
	c.mu.Lock()
	c.startLocked()
	wait := c.fetching
	if c.loaded {
		wait = nil
	}
	c.mu.Unlock()
	if wait != nil { // first fetch only, shared by every early caller
		t := time.NewTimer(catalogFirstWait)
		select {
		case <-wait:
		case <-ctx.Done():
		case <-t.C:
		}
		t.Stop()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.live {
		return append([]Model(nil), fallback...), false
	}
	return append([]Model(nil), c.models...), true
}

// startLocked starts a background fetch when one is due and none is running.
func (c *ModelCatalog) startLocked() {
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	if c.fetching == nil && !now.Before(c.next) {
		c.fetching = make(chan struct{})
		go c.refresh(c.fetching)
	}
}

// refresh runs one fetch outside the lock. Fetch bounds its own time
// (ListModels uses probeTimeout).
func (c *ModelCatalog) refresh(done chan struct{}) {
	models, err := c.Fetch(context.Background())
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	ttl := c.TTL
	if ttl <= 0 {
		ttl = catalogTTL
	}
	if err == nil && len(models) > 0 {
		c.models = append([]Model(nil), models...)
		c.live = true
		c.next = now.Add(ttl)
	} else {
		c.next = now.Add(min(ttl, catalogRetry))
	}
	c.loaded = true
	c.fetching = nil
	close(done)
}

// idle waits for a running fetch to finish (tests and the boot warm-up).
func (c *ModelCatalog) idle() {
	c.mu.Lock()
	d := c.fetching
	c.mu.Unlock()
	if d != nil {
		<-d
	}
}

// Warm starts the first fetch without waiting, so it usually finishes before
// traffic arrives.
func (c *ModelCatalog) Warm() {
	c.mu.Lock()
	c.startLocked()
	c.mu.Unlock()
}
