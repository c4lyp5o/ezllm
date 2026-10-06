package rules

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// RuleSource loads all rules — satisfied by *store.DB (via a thin adapter in
// main, so this package never imports store and there is no import cycle).
// Returning the full set keeps the cache reload a single call rather than a
// per-request query.
type RuleSource interface {
	AllRules(ctx context.Context) ([]Rule, error)
}

// UsageSource reads one metered bucket — satisfied by *store.DB's UsageFor.
// Returns (0 usage, nil) for a bucket that has never been written, which the
// callers treat as "not capped yet", never as an error.
type UsageSource interface {
	UsageFor(ctx context.Context, accountID int64, modelID, bucket string) (tokens int64, err error)
}

// Reason is the machine-readable cause attached to a refusal, so the server
// can shape the 429 body (kind, next_allowed) without parsing a string.
type Reason struct {
	Kind        string    // "window" | "cap"
	Detail      string    // human sentence for the message
	NextAllowed time.Time // next window open (window refusals only)
}

// Refusal is the error the predicate returns for an ineligible model. It
// implements error and carries the structured fields the 429 body needs.
// The router and server treat it as a rule refusal (429), distinct from a
// routing mistake (404) or an upstream failure (502).
type Refusal struct {
	Requested string // "ns/model" or the combo name the client asked for
	AccountID int64
	ModelID   string
	Reason    Reason
}

func (r *Refusal) Error() string {
	if r.Reason.Detail == "" {
		return fmt.Sprintf("%q is not available right now", r.Requested)
	}
	return fmt.Sprintf("%q is not available right now: %s", r.Requested, r.Reason.Detail)
}

// Engine owns the cached rule set and evaluates eligibility. Reads are lock-
// free (atomic.Value holding an immutable map) so a rule check never contends
// on a mutex in the hot path; the cache is refreshed on a ticker and
// explicitly by Reload after any admin write.
type Engine struct {
	src   RuleSource
	usage UsageSource
	now   Clock

	rules atomic.Value // map[key]*Rule, immutable once published

	// refresh interval for the background ticker. Exported-ish via field so
	// tests can shrink it; production sets it via NewEngine.
	refreshEvery time.Duration

	stopOnce sync.Once
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

type key struct {
	accountID int64
	modelID   string
}

// NewEngine builds an Engine, loads the initial rule set, and (unless
// autoRefresh is false) starts a background refresh goroutine. Production
// passes autoRefresh=true; tests pass false and call Reload manually.
func NewEngine(src RuleSource, usage UsageSource, now Clock, refreshEvery time.Duration, autoRefresh bool) (*Engine, error) {
	if now == nil {
		now = SystemClock
	}
	if refreshEvery <= 0 {
		refreshEvery = 30 * time.Second
	}
	e := &Engine{
		src:          src,
		usage:        usage,
		now:          now,
		refreshEvery: refreshEvery,
		stopCh:       make(chan struct{}),
	}
	e.rules.Store(map[key]*Rule{})
	if err := e.Reload(context.Background()); err != nil {
		return nil, err
	}
	if autoRefresh {
		e.wg.Add(1)
		go e.refreshLoop()
	}
	return e, nil
}

// Reload republishes the rule set. Called at startup and after any admin
// write to model_rules, so a change takes effect on the next request rather
// than after a TTL. Safe to call concurrently with evaluation.
func (e *Engine) Reload(ctx context.Context) error {
	list, err := e.src.AllRules(ctx)
	if err != nil {
		return fmt.Errorf("rules: reload: %w", err)
	}
	m := make(map[key]*Rule, len(list))
	for i := range list {
		r := list[i]
		if !r.Enabled {
			continue // disabled rules are as if absent
		}
		m[key{r.AccountID, r.ModelID}] = &r
	}
	e.rules.Store(m) // atomic publish — readers never see a partial map
	return nil
}

func (e *Engine) refreshLoop() {
	defer e.wg.Done()
	t := time.NewTicker(e.refreshEvery)
	defer t.Stop()
	for {
		select {
		case <-e.stopCh:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := e.Reload(ctx); err != nil {
				// Keep serving the last-good set; a failed reload must not
				// blank the rules (that would silently un-cap everything).
				_ = err
			}
			cancel()
		}
	}
}

// Close stops the background refresh goroutine.
func (e *Engine) Close() {
	e.stopOnce.Do(func() { close(e.stopCh) })
	e.wg.Wait()
}

// Get returns the rule for (account, model), or nil when there is none.
func (e *Engine) Get(accountID int64, modelID string) *Rule {
	m, _ := e.rules.Load().(map[key]*Rule)
	return m[key{accountID, modelID}]
}

// Evaluate is the predicate the router's Eligible seam calls. It answers
// "may this (account, model) be used RIGHT NOW?" and returns a *Refusal
// (which the server maps to 429) when it may not.
//
// Order matters: window check first (zero I/O), cap check second (one PK
// lookup). The cheap veto runs first so a night-only model at noon says
// "not until 22:00" rather than spending a read and possibly reporting a
// cap it would never hit.
func (e *Engine) Evaluate(ctx context.Context, accountID int64, modelID, requested string) error {
	r := e.Get(accountID, modelID)
	if r == nil || r.AllowAll() {
		return nil // no rule, or a rule that restricts nothing
	}

	now := e.now()

	// 1. Window — pure arithmetic, no I/O.
	if r.Window.Start != "" || r.Window.End != "" {
		ok, err := r.Window.InWindow(now)
		if err != nil {
			// Corrupt window: fail closed (deny), with the parse error as the
			// reason so an operator sees WHY rather than a mystery 429.
			return &Refusal{
				Requested: requested, AccountID: accountID, ModelID: modelID,
				Reason: Reason{Kind: "window", Detail: "rule is misconfigured: " + err.Error()},
			}
		}
		if !ok {
			return &Refusal{
				Requested: requested, AccountID: accountID, ModelID: modelID,
				Reason: Reason{
					Kind:        "window",
					Detail:      r.windowDetail(),
					NextAllowed: r.Window.NextAllowed(now),
				},
			}
		}
	}

	// 2. Cap — one primary-key lookup. Zero or no row = not yet capped.
	if r.CapTokens > 0 && e.usage != nil {
		bucket := r.bucketFor(now, nil)
		used, err := e.usage.UsageFor(ctx, accountID, modelID, bucket)
		if err != nil {
			// A failed read must not silently un-cap. Fail closed: refuse
			// and say the meter could not be read (operator investigates),
			// rather than letting traffic through an unknown budget.
			return &Refusal{
				Requested: requested, AccountID: accountID, ModelID: modelID,
				Reason: Reason{Kind: "cap", Detail: "usage meter unavailable: " + err.Error()},
			}
		}
		if used >= r.CapTokens {
			return &Refusal{
				Requested: requested, AccountID: accountID, ModelID: modelID,
				Reason: Reason{
					Kind:   "cap",
					Detail: fmt.Sprintf("%s %s cap reached (%d of %d tokens)", r.CapWindow, bucket, used, r.CapTokens),
				},
			}
		}
	}
	return nil
}

// Eligible adapts Evaluate to the router's Eligible signature. The requested
// string is not known at the seam (the router passes only account+model), so
// the refusal carries the model id and the server enriches it with the
// client's requested name when shaping the response.
func (e *Engine) Eligible(ctx context.Context, accountID int64, modelID string) error {
	return e.Evaluate(ctx, accountID, modelID, modelID)
}

// windowDetail renders the window as a sentence for the 429 message,
// e.g. "allowed 22:00-06:00 Asia/Kuala_Lumpur (Mon-Fri)".
func (r *Rule) windowDetail() string {
	var b strings.Builder
	if r.Window.Start != "" && r.Window.End != "" {
		fmt.Fprintf(&b, "allowed %s-%s %s", r.Window.Start, r.Window.End, r.Window.TZ)
	}
	if len(r.Window.Days) > 0 {
		fmt.Fprintf(&b, " (%s)", summarizeDays(r.Window.Days))
	}
	return b.String()
}
