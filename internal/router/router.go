// Package router resolves a client's requested model into a concrete Route.
//
// M2 scope: DIRECT namespace resolution only — "<namespace>/<model>" against a
// registered account. Combos, strategies, key pools, cooldowns and caps land in
// M4; the Resolver interface is already shaped so M4 replaces the implementation
// without touching the handlers.
package router

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// ErrNotFound is returned when a model string cannot be resolved. The message
// carries the hints (which namespaces/combos DO serve it) so the 404 is useful.
type ErrNotFound struct {
	Requested string
	Hints     []string
}

func (e *ErrNotFound) Error() string {
	if len(e.Hints) == 0 {
		return fmt.Sprintf("no provider serves %q", e.Requested)
	}
	return fmt.Sprintf("no provider serves %q — available: %s", e.Requested, strings.Join(e.Hints, ", "))
}

// Catalog is the read-only view of registered accounts/keys/models that the
// resolver needs. Implemented by the store layer.
type Catalog interface {
	// AccountByNamespace returns the enabled account for a namespace.
	AccountByNamespace(ctx context.Context, ns string) (*provider.Account, error)
	// PickKey returns a decrypted key eligible for the account (M2: the first
	// enabled key; M4 adds rotation/cooldown/quota exclusion).
	PickKey(ctx context.Context, accountID int64) (store.KeyPick, error)
	// ModelExists reports whether the account's catalog contains modelID.
	ModelExists(ctx context.Context, accountID int64, modelID string) (bool, error)
	// ModelPin returns the surface pinned for a model, or "" when unpinned.
	ModelPin(ctx context.Context, accountID int64, modelID string) (string, error)
	// AllNamespaces lists namespace/model pairs for helpful 404s.
	AllNamespaces(ctx context.Context) ([]string, error)
	// AccountByID loads an account by primary key — combo hops reference
	// accounts by id, not namespace, so the combo path needs this.
	AccountByID(ctx context.Context, id int64) (*provider.Account, error)
	// ComboByName loads one combo with its ordered hops (2 indexed queries).
	ComboByName(ctx context.Context, name string) (*store.Combo, error)
	// AllComboNames lists combo names for 404 hints.
	AllComboNames(ctx context.Context) ([]string, error)
}

// ErrPinned refuses a request whose client surface is not the one this model
// is pinned to. It never reroutes: the body is in the client's shape, so
// sending it to another surface would only produce an upstream 400. The
// server maps this to 400 and names the surface the client must use.
type ErrPinned struct {
	Requested string
	Pinned    string
	Got       string
}

func (e *ErrPinned) Error() string {
	return fmt.Sprintf("model %q is pinned to the %s surface; this request used %s — call it on the %s endpoint instead",
		e.Requested, e.Pinned, e.Got, e.Pinned)
}

// surfaceName maps a wire surface to its pin name.
func surfaceName(s provider.Surface) string {
	switch s {
	case provider.SurfaceAnthropic:
		return "anthropic"
	case provider.SurfaceResponses:
		return "responses"
	default:
		return "openai"
	}
}

// checkPin enforces a model's pin against the client's surface. An unpinned
// model passes unchanged. A lookup error FAILS CLOSED: an operator who pinned
// a model away from a surface must not have that pin silently bypassed by a
// transient DB error. The error is returned so the caller answers, not skips.
func (r *Resolver) checkPin(ctx context.Context, accountID int64, model, requested string, surface provider.Surface) error {
	pin, err := r.catalog.ModelPin(ctx, accountID, model)
	if err != nil {
		return fmt.Errorf("model pin lookup for %q: %w", requested, err)
	}
	if pin == "" || pin == surfaceName(surface) {
		return nil
	}
	return &ErrPinned{Requested: requested, Pinned: pin, Got: surfaceName(surface)}
}

// ErrUnavailable marks a request the router refused for a RULE rather than a
// routing mistake: the model is outside its allowed hours, or its usage cap
// is spent. The server maps this to 429 (with the reason in the body) — a
// distinct signal from 404, which means "no such model here at all". Combos
// never surface it: an ineligible hop is simply dropped from the candidates.
type ErrUnavailable struct {
	Requested string
	Reason    string
	// cause is the original eligibility error (a *rules.Refusal when the rules
	// engine said no). It is unwrapped so the server can errors.As into the
	// structured type and recover kind/next_allowed for the 429 body, without
	// this package importing the rules package (no cycle). Reason stays the
	// flat human string for the message and for anything that only reads it.
	cause error
}

func (e *ErrUnavailable) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("%q is not available right now", e.Requested)
	}
	return fmt.Sprintf("%q is not available right now: %s", e.Requested, e.Reason)
}

// Unwrap exposes the underlying eligibility error so errors.As can reach it.
func (e *ErrUnavailable) Unwrap() error { return e.cause }

// Eligible reports whether a hop may be used RIGHT NOW. Returning an error
// removes the hop from the candidate list (and, for a direct route, becomes
// the 429). This is the seam the rules engine plugs into: token caps and
// allowed-hours windows live behind it, so strategies never need to know why a
// hop is unavailable — only that it is.
type Eligible func(ctx context.Context, accountID int64, modelID string) error

// Resolver turns a requested model string into a Route.
type Resolver struct {
	catalog Catalog
	// eligible filters candidates before any strategy runs. nil = everything
	// eligible (the M2 behaviour, and the right default until rules exist).
	eligible Eligible
}

// New builds a Resolver.
func New(c Catalog) *Resolver { return &Resolver{catalog: c} }

// WithEligibility installs the pre-strategy filter. The rules engine calls
// this at startup; leaving it unset keeps routing behaviour unchanged.
func (r *Resolver) WithEligibility(e Eligible) *Resolver {
	r.eligible = e
	return r
}

// eligibleHere applies the filter, treating nil as "allow".
func (r *Resolver) eligibleHere(ctx context.Context, accountID int64, modelID string) error {
	if r.eligible == nil {
		return nil
	}
	return r.eligible(ctx, accountID, modelID)
}

// Resolve implements proxy.Rewriter: the FIRST candidate. Handlers that can
// fail over ask ResolveCandidates instead.
//
// Resolution order (per plan §1.1):
//  1. bare combo name → first hop of that combo
//  2. "<namespace>/<model>" → direct route to that account+model
//  3. otherwise → 404 with hints listing what DOES serve that model
//
// There is deliberately NO default account: a bare model id is ambiguous
// (the same model id can exist on several accounts), and guessing would
// mis-attribute spend.
func (r *Resolver) Resolve(ctx context.Context, requested string, surface provider.Surface, client string) (proxy.Route, error) {
	routes, err := r.ResolveCandidates(ctx, requested, surface, client)
	if err != nil {
		return proxy.Route{}, err
	}
	return routes[0], nil
}

// ResolveCandidates returns the ordered candidate list for a request: every
// hop of a combo that is enabled AND currently eligible, or the single direct
// route for "ns/model". The caller tries them in order (see
// proxy.ForwardCandidates); an empty list is always accompanied by an error.
func (r *Resolver) ResolveCandidates(ctx context.Context, requested string, surface provider.Surface, client string) ([]proxy.Route, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return nil, errors.New("router: model is required")
	}

	ns, model, hasSlash := splitNamespace(requested)
	if !hasSlash {
		return r.resolveCombo(ctx, requested, surface)
	}

	acct, err := r.catalog.AccountByNamespace(ctx, ns)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Hint on the MODEL, not the whole "ns/model" string: the namespace
			// is what was wrong, so matching against "nope/gpt-6-luna" would
			// find nothing and leave the user with a bare 404.
			hints, _ := r.catalog.AllNamespaces(ctx)
			return nil, &ErrNotFound{Requested: requested, Hints: filterHints(hints, model)}
		}
		return nil, err
	}
	if !acct.Enabled {
		return nil, fmt.Errorf("router: account %q is disabled", ns)
	}

	// Surface capability is enforced in M4 via the probed models table; in M2 we
	// still verify the model exists in the account's catalog so a typo 404s
	// rather than burning an upstream call.
	if ok, err := r.catalog.ModelExists(ctx, acct.ID, model); err == nil && !ok {
		hints, _ := r.catalog.AllNamespaces(ctx)
		return nil, &ErrNotFound{
			Requested: requested,
			Hints:     filterHints(hints, model),
		}
	}

	// A pin is a refusal, not a skip: a direct route has no other hop to try.
	if err := r.checkPin(ctx, acct.ID, model, requested, surface); err != nil {
		return nil, err
	}

	// A direct route has nowhere to fail over to, so an ineligible model is a
	// hard stop rather than a dropped candidate.
	if err := r.eligibleHere(ctx, acct.ID, model); err != nil {
		return nil, &ErrUnavailable{Requested: requested, Reason: err.Error(), cause: err}
	}

	key, err := r.catalog.PickKey(ctx, acct.ID)
	if err != nil {
		return nil, fmt.Errorf("router: no usable key for %q: %w", ns, err)
	}

	return []proxy.Route{{
		Account:  *acct,
		KeyID:    key.ID,
		KeyPlain: key.Plaintext,
		KeyHint:  key.Hint,
		Model:    model,
		Alias:    requested,
		Surface:  surface,
	}}, nil
}

// resolveCombo turns a bare combo name into ordered candidates.
//
// Every hop must clear FOUR gates before it becomes a candidate: the hop is
// enabled, its account exists and is enabled, the model is in that account's
// catalog, and the eligibility filter (rules engine) allows it right now. A
// hop failing any gate is dropped silently — that is what lets one combo hold
// both a day model and a night model without the caller caring which is live.
//
// If nothing survives, the error names the FIRST hop's failure rather than
// "no candidates", because "combo 'x' has no usable hops: account 'y' is
// disabled" is actionable and the generic message is not.
func (r *Resolver) resolveCombo(ctx context.Context, name string, surface provider.Surface) ([]proxy.Route, error) {
	combo, err := r.catalog.ComboByName(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Hint on combos AND namespaces: a bare name is ambiguous between
			// the two, so the 404 must show both kinds of answer.
			//
			// Combo names are NOT substring-filtered (namespaces still are).
			// The commonest typo is a transposition — "dialy" for "daily" —
			// and filterHints drops every name that does not CONTAIN the
			// misspelling, so the correct answer would be filtered away from
			// exactly the user who needs it. Combos are a short curated list;
			// showing all of them is the useful behaviour.
			combos, _ := r.catalog.AllComboNames(ctx)
			ns, _ := r.catalog.AllNamespaces(ctx)
			hints := append(combos, filterHints(ns, name)...)
			return nil, &ErrNotFound{Requested: name, Hints: hints}
		}
		return nil, err
	}
	if !combo.Enabled {
		return nil, fmt.Errorf("router: combo %q is disabled", name)
	}
	if len(combo.Hops) == 0 {
		return nil, fmt.Errorf("router: combo %q has no hops", name)
	}

	var (
		routes []proxy.Route
		first  error // first hop's rejection, kept as the failure to report
	)
	for _, h := range combo.Hops {
		if h.Enabled != nil && !*h.Enabled {
			continue
		}
		acct, err := r.catalog.AccountByID(ctx, h.AccountID)
		if err != nil {
			if first == nil {
				first = fmt.Errorf("hop account %d/%s: %w", h.AccountID, h.ModelID, err)
			}
			continue
		}
		if !acct.Enabled {
			if first == nil {
				first = fmt.Errorf("hop %s/%s: account %q is disabled", acct.Namespace, h.ModelID, acct.Namespace)
			}
			continue
		}
		if ok, err := r.catalog.ModelExists(ctx, acct.ID, h.ModelID); err != nil || !ok {
			if first == nil {
				first = fmt.Errorf("hop %s/%s: not in that account's catalog", acct.Namespace, h.ModelID)
			}
			continue
		}
		// A pinned hop on the wrong surface is a refusal for the whole request,
		// not a skip: silently moving to another hop would change protocol.
		if err := r.checkPin(ctx, acct.ID, h.ModelID, name, surface); err != nil {
			return nil, err
		}
		// The rules engine's veto. In a combo this is a skip, never an error:
		// that is the entire day/night mechanism. But when EVERY hop ends up
		// skipped by rules this must surface as ErrUnavailable (429 "try
		// later"), exactly like resolveDirect's hard stop — a raw refusal
		// would fall through errorStatus to 502 and read as a gateway fault.
		if err := r.eligibleHere(ctx, acct.ID, h.ModelID); err != nil {
			if first == nil {
				first = &ErrUnavailable{
					Requested: name,
					Reason:    fmt.Sprintf("hop %s/%s: %s", acct.Namespace, h.ModelID, err.Error()),
					cause:     err,
				}
			}
			continue
		}
		key, err := r.catalog.PickKey(ctx, acct.ID)
		if err != nil {
			if first == nil {
				first = fmt.Errorf("hop %s/%s: no usable key: %w", acct.Namespace, h.ModelID, err)
			}
			continue
		}
		routes = append(routes, proxy.Route{
			Account:  *acct,
			KeyID:    key.ID,
			KeyPlain: key.Plaintext,
			KeyHint:  key.Hint,
			Model:    h.ModelID,
			Alias:    name,
			Surface:  surface,
		})
	}

	if len(routes) == 0 {
		if first != nil {
			// A rules veto arrives pre-typed and pre-worded ("X is not
			// available right now: hop …"); re-wrapping it would print the
			// combo name twice and bury the cause under boilerplate.
			var ru *ErrUnavailable
			if errors.As(first, &ru) {
				return nil, first
			}
			return nil, fmt.Errorf("router: combo %q has no usable hops: %w", name, first)
		}
		return nil, fmt.Errorf("router: combo %q has no usable hops", name)
	}
	return orderRoutes(combo, routes), nil
}

// orderRoutes applies the combo's strategy. The candidate list is already
// filtered, so strategies only ever choose among hops that WILL work right
// now — none of them re-implements eligibility.
//
// Weights are handled by expansion (a weight-3 hop appears three times) rather
// than by a weighted cursor: it keeps the hot path free of arithmetic state,
// and the lists are tiny.
func orderRoutes(combo *store.Combo, routes []proxy.Route) []proxy.Route {
	switch combo.Strategy {
	case "true_round_robin", "strict_round_robin":
		// Both names do the same thing: rotate by one every request. The plan
		// document described strict as persisted-and-epoch-safe and true as
		// weighted; neither is implemented, so rather than keep two names for
		// one behaviour the distinction is documented as absent. Use
		// block_round_robin when cache reuse matters.
		return rotate(NextRR(combo.ID)%uint64(len(routes)), routes)
	case "block_round_robin":
		return blockOrder(combo, routes)
	case "least_used":
		// Caller supplies hop order already; least-used needs usage data the
		// router does not hold, so keep position order until M4's ledger hook
		// lands. Ordering wrongly here would be worse than not ordering.
		return routes
	case "sticky_last_good":
		return stickyOrder(combo, routes)
	default: // "failover"
		return routes
	}
}

// blockCursors is the per-combo block_round_robin cursor: which hop owns the
// current block, and how many dispatches it has left. Process-local like
// rrCounters — losing it on restart only means a block restarts.
//
// Identity, not an index: the eligible candidate list changes as hops cool down
// or come back, and an index would silently re-point the block at a different
// hop when it does. The entry carries its own mutex because the read-check-
// decrement is a read-modify-write that concurrent requests share.
var blockCursors sync.Map // int64 comboID -> *blockEntry

type blockEntry struct {
	mu        sync.Mutex
	accountID int64
	model     string
	remaining int
}

// blockOrder implements block_round_robin: one hop serves BlockSize consecutive
// dispatches, then the rotation advances to the next hop in position order.
//
// Why blocks rather than per-request rotation: within an agent turn the tool
// loop fires several calls seconds apart with a growing prefix, and each call
// reads the previous prefix out of that ACCOUNT's prompt cache. Striping every
// request across hops destroys that — the call landing on hop B carries a prefix
// B has never seen, so B pays a full cold read, and A loses the delta that went
// to B. Holding a hop for a run of calls keeps its cached prefix warm while
// still splitting load evenly over time.
//
// BlockSize 1 degenerates to plain per-request round robin. Costs one cold read
// per block boundary (`block_size - 1` warm calls per block), so a block that
// splits a burst mid-way is the failure mode to size against.
func blockOrder(combo *store.Combo, routes []proxy.Route) []proxy.Route {
	if len(routes) < 2 {
		return routes
	}
	size := combo.BlockSize
	if size < 1 {
		size = 1
	}

	v, _ := blockCursors.LoadOrStore(combo.ID, &blockEntry{})
	e := v.(*blockEntry)
	e.mu.Lock()
	defer e.mu.Unlock()

	target := -1
	for i, rt := range routes {
		if rt.Account.ID == e.accountID && rt.Model == e.model {
			target = i
			break
		}
	}

	if target < 0 || e.remaining <= 0 {
		// Start the next block. Walk forward in position order (the combo's
		// declared preference) rather than jumping; with no usable previous
		// target — a cold start, or the hop left the eligible list — begin at
		// the top of the list.
		next := 0
		if target >= 0 {
			next = (target + 1) % len(routes)
		}
		e.accountID, e.model = routes[next].Account.ID, routes[next].Model
		e.remaining = size
		target = next
	}
	e.remaining--
	return rotate(uint64(target), routes)
}

// rrCounters is the per-combo round-robin cursor. In-process on purpose: a
// DB write per inference would put SQLite on the hot path for no benefit —
// losing a cursor on restart only means one combo starts at hop 0 again.
var rrCounters sync.Map // int64 comboID -> *uint64

func rotate(start uint64, routes []proxy.Route) []proxy.Route {
	if start == 0 || len(routes) < 2 {
		return routes
	}
	out := make([]proxy.Route, 0, len(routes))
	out = append(out, routes[start:]...)
	return append(out, routes[:start]...)
}

// stickyLast remembers which hop last answered, so sticky_last_good keeps
// using it until sticky_idle_s elapses. Process-local like rrCounters.
var stickyLast sync.Map // int64 comboID -> stickyEntry

type stickyEntry struct {
	route proxy.Route
	at    time.Time
}

// stickyOrder puts the last-good hop first, while the combo is still within its
// idle window AND the hop is still in the candidate list.
//
// The idle check is the whole point of the strategy and was MISSING: the entry's
// timestamp was written and never read, so sticky_last_good pinned the first hop
// that ever answered — through restarts of nothing, indefinitely — and
// sticky_idle_s had no effect whatsoever. Release after the idle gap is what
// lets the next session start on a different account while a running session
// keeps its warm cache.
//
// sticky_idle_s of 0 means "never release on idle" (stick until the hop fails
// or leaves the list); a positive value is the number of idle seconds to hold.
// After a restart (no memory) or an idle gap it falls back to position order —
// correct, just not sticky.
func stickyOrder(combo *store.Combo, routes []proxy.Route) []proxy.Route {
	v, ok := stickyLast.Load(combo.ID)
	if !ok {
		return routes
	}
	e := v.(stickyEntry)
	if combo.StickyIdleS > 0 && time.Since(e.at) > time.Duration(combo.StickyIdleS)*time.Second {
		stickyLast.Delete(combo.ID)
		return routes
	}
	for i, rt := range routes {
		if rt.Account.ID == e.route.Account.ID && rt.Model == e.route.Model {
			if i == 0 {
				return routes
			}
			out := make([]proxy.Route, 0, len(routes))
			out = append(out, routes[i])
			return append(out, append(routes[:i], routes[i+1:]...)...)
		}
	}
	return routes
}

// MarkHopGood records a hop as the one that answered, for sticky_last_good.
func (r *Resolver) MarkHopGood(comboID int64, rt proxy.Route) {
	stickyLast.Store(comboID, stickyEntry{route: rt, at: time.Now()})
}

// NextRR advances and returns the round-robin cursor for a combo.
func NextRR(comboID int64) uint64 {
	v, _ := rrCounters.LoadOrStore(comboID, new(uint64))
	return atomic.AddUint64(v.(*uint64), 1) - 1
}

// splitNamespace splits "ns/model" into its parts. A model id may itself contain
// dots but not slashes, so we split on the FIRST slash only.
func splitNamespace(s string) (ns, model string, ok bool) {
	i := strings.Index(s, "/")
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// filterHints keeps only hints that mention the requested model, so the 404
// suggests real alternatives instead of dumping the whole catalog.
func filterHints(hints []string, model string) []string {
	if model == "" {
		return hints
	}
	var out []string
	for _, h := range hints {
		if strings.Contains(h, model) {
			out = append(out, h)
		}
	}
	if len(out) == 0 {
		return nil
	}
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}
