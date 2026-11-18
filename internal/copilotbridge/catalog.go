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

// ModelCatalog serializes refreshes and keeps the last successful catalog.
// Failed refreshes are retried after TTL, not on every incoming request.
type ModelCatalog struct {
	mu     sync.Mutex
	Fetch  func(context.Context) ([]Model, error)
	TTL    time.Duration
	Now    func() time.Time
	models []Model
	next   time.Time
	live   bool
}

func (c *ModelCatalog) Get(ctx context.Context, fallback []Model) ([]Model, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	if !now.Before(c.next) {
		ttl := c.TTL
		if ttl <= 0 {
			ttl = time.Minute
		}
		c.next = now.Add(ttl)
		if models, err := c.Fetch(ctx); err == nil && len(models) > 0 {
			c.models = append([]Model(nil), models...)
			c.live = true
		}
	}
	if !c.live {
		return append([]Model(nil), fallback...), false
	}
	return append([]Model(nil), c.models...), true
}
