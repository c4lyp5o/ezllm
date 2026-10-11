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
	"math/rand/v2"
	"strings"
	"sync"
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
	// ModelSupportsSurface returns the PROBED protocol capability for one
	// (account, model, surface). nil means UNTESTED, which the router reads as
	// allowed — the upstream stays authoritative.
	ModelSupportsSurface(ctx context.Context, accountID int64, modelID string, surface provider.Surface) (*bool, error)
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
	// translationEnabled gates the anthropic→openai hop translation. Off
	// by default; even ON it only ever serves hops whose probed capability
	// PROVES the OpenAI shape (see translate.Eligible).
	translationEnabled bool
}

// New builds a Resolver.
func New(c Catalog) *Resolver { return &Resolver{catalog: c} }

// WithEligibility installs the pre-strategy filter. The rules engine calls
// this at startup; leaving it unset keeps routing behaviour unchanged.
func (r *Resolver) WithEligibility(e Eligible) *Resolver {
	r.eligible = e
	return r
}

// WithTranslation enables anthropic→openai hop translation (opt-in from
// config/env). Unset keeps routing behaviour unchanged.
func (r *Resolver) WithTranslation(enabled bool) *Resolver {
	r.translationEnabled = enabled
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
	//
	// A lookup error FAILS CLOSED: the same reasoning as checkPin above — a
	// transient store error must not disable the existence check and let a
	// typo'd model burn an upstream call on every attempt until the DB recovers.
	exists, err := r.catalog.ModelExists(ctx, acct.ID, model)
	if err != nil {
		return nil, fmt.Errorf("router: model lookup for %q: %w", requested, err)
	}
	if !exists {
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
			// Copy into a fresh slice: AllComboNames returns a slice the
			// catalog owns, and appending into it can write into that
			// backing array when it has spare capacity — safe today, a
			// cross-request data race tomorrow. Order preserved: combos
			// first, then the filtered namespaces.
			hints := make([]string, 0, len(combos)+len(ns))
			hints = append(hints, combos...)
			hints = append(hints, filterHints(ns, name)...)
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
				first = fmt.Errorf("hop %s/%s: account %q is disabled", acct.Namespace, h.ModelID, acct.Name)
			}
			continue
		}
		if ok, err := r.catalog.ModelExists(ctx, acct.ID, h.ModelID); err != nil || !ok {
			if first == nil {
				first = fmt.Errorf("hop %s/%s: not in that account's catalog", acct.Namespace, h.ModelID)
			}
			continue
		}
		// Capability gate: a model PROBED unable to serve this surface is
		// dropped instead of dialled. This is the hop-level half of the rule
		// "ModelProtocolUnsupported → next hop" (see the ezllm skill): without
		// it, one incompatible member of an otherwise healthy combo answers the
		// client with the upstream's 400/403 on every rotation onto it.
		//
		// UNTESTED (nil) stays eligible, and a lookup ERROR fails OPEN — both
		// deliberate, and the exact opposite of ModelExists/checkPin above. A
		// capability flag is a hint learned from a probe, so a stale nil or a
		// transient store hiccup must never strand a hop that actually works.
		if ok, err := r.catalog.ModelSupportsSurface(ctx, acct.ID, h.ModelID, surface); err == nil && ok != nil && !*ok {
			if first == nil {
				first = fmt.Errorf("hop %s/%s: probed unable to serve the %s surface",
					acct.Namespace, h.ModelID, surfaceName(surface))
			}
			continue
		}
		// A pin on a hop that has already been probed incapable of this surface
		// is irrelevant: that hop is excluded before routing. For a remaining
		// candidate, a mismatched pin is still an operator-directed hard refusal.
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
	case "true_round_robin":
		return randomOrder(routes)
	case "strict_round_robin":
		return strictOrder(combo, routes)
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
// current block, and how many dispatches it has left. In-process on purpose: a
// DB write per inference would put SQLite on the hot path for no benefit, and
// losing it on restart only means one block starts over.
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

func rotate(start uint64, routes []proxy.Route) []proxy.Route {
	if start == 0 || len(routes) < 2 {
		return routes
	}
	out := make([]proxy.Route, 0, len(routes))
	out = append(out, routes[start:]...)
	return append(out, routes[:start]...)
}

// lastGood remembers which hop last ANSWERED a request, per combo NAME.
//
// Keyed by name, not id, because the only writer is the success path in the
// server, which knows the alias the client asked for and not the combo's row id.
// Combo names are unique. In-process, like every other cursor here — losing it
// on restart costs one un-sticky (or one off-by-one rotation) request.
//
// TWO strategies read this, because "which hop last answered" answers both of
// their questions:
//   - sticky_last_good  : keep using it (until sticky_idle_s of quiet).
//   - strict_round_robin: start the NEXT request after it.
//
// The value is deliberately {accountID, model, at} and NOT the whole
// proxy.Route. Both readers use exactly those two fields (plus the timestamp),
// while a full Route carries KeyPlain — the DECRYPTED upstream credential — so
// storing it would pin a plaintext provider API key in a process-global map
// with no eviction, duplicating it outside the request scope that needed it.
var lastGood sync.Map // string comboName -> lastGoodEntry

type lastGoodEntry struct {
	accountID int64
	model     string
	at        time.Time
}

// MarkHopGood records the hop that actually served a request. Called by the
// server on a delivered 2xx for a combo-routed request.
//
// This had NO CALLER at all until now, which made sticky_last_good inert: the
// map it reads was never written, so the strategy silently fell back to
// position order (hop 1) on every request. A dead WRITE is as bad as a dead
// knob, and it fails the same way — quietly, with the strategy appearing to run.
func (r *Resolver) MarkHopGood(comboName string, rt proxy.Route) {
	if comboName == "" {
		return
	}
	lastGood.Store(comboName, lastGoodEntry{
		accountID: rt.Account.ID,
		model:     rt.Model,
		at:        time.Now(),
	})
}

// stickyOrder puts the last-good hop first, while the combo is still within its
// idle window AND the hop is still in the candidate list.
//
// sticky_idle_s of 0 means "never release on idle" (stick until the hop fails
// or leaves the list); a positive value is the number of idle seconds to hold.
// Without a remembered hop, or past the idle window, it falls back to position
// order — correct, just not sticky.
func stickyOrder(combo *store.Combo, routes []proxy.Route) []proxy.Route {
	v, ok := lastGood.Load(combo.Name)
	if !ok {
		return routes
	}
	e := v.(lastGoodEntry)
	if combo.StickyIdleS > 0 && time.Since(e.at) > time.Duration(combo.StickyIdleS)*time.Second {
		lastGood.Delete(combo.Name)
		return routes
	}
	for i, rt := range routes {
		if rt.Account.ID == e.accountID && rt.Model == e.model {
			if i == 0 {
				return routes
			}
			return rotate(uint64(i), routes)
		}
	}
	return routes
}

// strictOrder implements strict_round_robin: the rotation advances only when a
// request SUCCEEDS, and it advances from the hop that actually served.
//
// The difference from plain per-request rotation is entirely in what happens on
// failure. Plain rotation moves the cursor on every ATTEMPT, so a hop that just
// failed is skipped next time even though it may be perfectly healthy. Here the
// cursor moves only on a delivered response and moves to the hop that answered,
// so a failing hop deprioritises itself without being blacklisted: A 429s, B
// serves, the next request starts at C — and A is tried again when the rotation
// comes back round to it. Failover inside the request is unchanged; only the
// next request's starting point differs.
//
// With no remembered hop (a cold start) it is position order, which for the
// first request is indistinguishable from starting at the top of the list.
func strictOrder(combo *store.Combo, routes []proxy.Route) []proxy.Route {
	if len(routes) < 2 {
		return routes
	}
	v, ok := lastGood.Load(combo.Name)
	if !ok {
		return routes
	}
	e := v.(lastGoodEntry)
	for i, rt := range routes {
		if rt.Account.ID == e.accountID && rt.Model == e.model {
			return rotate(uint64((i+1)%len(routes)), routes)
		}
	}
	// The hop that answered is not eligible right now (cooling, out of budget,
	// dropped from the catalog). Position order is the honest fallback.
	return routes
}

// randomOrder implements true_round_robin: a genuine dice roll over the eligible
// hops, for every request.
//
// It is NOT a round robin despite the name, and it is the only strategy with no
// cursor at all — which is its one real advantage: nothing to reset on restart
// and no shared state to synchronise. The costs are worth stating: it is even
// only in expectation, so it will pick the same hop several times in a row, and
// like any per-request rotation it discards prefix-cache warmth. Prefer
// block_round_robin when cache reuse matters.
func randomOrder(routes []proxy.Route) []proxy.Route {
	if len(routes) < 2 {
		return routes
	}
	return rotate(uint64(rand.IntN(len(routes))), routes)
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
