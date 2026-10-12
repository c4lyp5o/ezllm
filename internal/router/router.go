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
	"log/slog"
	"math/rand/v2"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/store"
	"github.com/c4lyp5o/ezllm/internal/translate"
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

// checkPin enforces a model's pin against the supplied dispatch surface.
// Native/direct callers pass the client's surface; translated combo hops pass
// their OpenAI wire. An unpinned model passes unchanged. A lookup error FAILS CLOSED: an operator who pinned
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

// ErrWireUnsupported reports a DIRECT route with no wire left to dial: the
// client's surface is PROVEN impossible for the model and nothing entitles it
// to another wire. A combo never surfaces this — a hop in that state is simply
// dropped — but a direct route ("ns/model") has nowhere else to go, and
// dialing a wire the probe already disproved only buys a pointless upstream
// call. The server answers 400: the client asked for a surface this model
// cannot serve, which is a request problem, not a gateway fault.
type ErrWireUnsupported struct {
	Requested string
	// Wire is the surface that was probed impossible.
	Wire provider.Surface
	// Reason names the failing gate in the operator's words.
	Reason string
}

func (e *ErrWireUnsupported) Error() string {
	msg := fmt.Sprintf("%q cannot serve the %s wire", e.Requested, surfaceName(e.Wire))
	if e.Reason != "" {
		return msg + ": " + e.Reason
	}
	return msg
}

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
	// responsesModels is the operator-curated list of hops that must be dialed
	// on the Responses wire for chat AND anthropic clients. Each entry is a
	// REGEX matched against the full "<ns>/<model>" string (both ends
	// anchored), so a plain id still matches only itself and a family pattern
	// like `stationine/gpt-6\\.1-sol` covers the variants. Empty = inert.
	responsesModels []*regexp.Regexp
	// universalEnabled turns on the three NEW cross-wire cells
	// (chat→messages, responses→chat, responses→messages) and the fixed
	// fallback ordering, and lets an explicit pin name the wire. It is
	// ANDed with translationEnabled: the universal matrix cannot resurrect a
	// translation layer the operator switched off.
	universalEnabled bool
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

// WithUniversal enables the universal wire matrix (config
// translation.universal_enabled, default off). Unset keeps every routing
// decision exactly as it is today: the single anthropic→openai alternative
// plus the curated responses-wire override.
func (r *Resolver) WithUniversal(enabled bool) *Resolver {
	r.universalEnabled = enabled
	return r
}

// universalOrder is the fixed fallback preference for a proven-impossible
// native wire. A Messages client (Claude Code) falls back to chat first —
// the most widely served shape — then Responses; a chat client prefers
// Responses (tools+reasoning parity) then Messages; a Responses client
// prefers chat then Messages. Only proven-capable destinations are used.
func universalOrder(client provider.Surface) []provider.Surface {
	switch client {
	case provider.SurfaceAnthropic:
		return []provider.Surface{provider.SurfaceOpenAI, provider.SurfaceResponses}
	case provider.SurfaceOpenAI:
		return []provider.Surface{provider.SurfaceResponses, provider.SurfaceAnthropic}
	case provider.SurfaceResponses:
		return []provider.Surface{provider.SurfaceOpenAI, provider.SurfaceAnthropic}
	}
	return nil
}

// pinSurface parses a models.proto_pin value. The stored names ARE the
// provider.Surface values, so the pin vocabulary cannot drift from the wires.
func pinSurface(pin string) (provider.Surface, bool) {
	switch provider.Surface(pin) {
	case provider.SurfaceOpenAI, provider.SurfaceAnthropic, provider.SurfaceResponses:
		return provider.Surface(pin), true
	}
	return "", false
}

// WithResponsesModels installs the curated patterns whose matching hops ride
// the Responses wire for chat and anthropic clients (see
// docs/responses-wire-design.md). Entries are regexes matched fully against
// "<ns>/<model>"; an entry that does not compile is dropped — config.Load
// validates the same list at boot and refuses to start on it, so this skip is
// defensive only (tests may hand the resolver raw strings).
func (r *Resolver) WithResponsesModels(models []string) *Resolver {
	patterns := make([]*regexp.Regexp, 0, len(models))
	for _, v := range models {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		re, err := regexp.Compile(`\A(?:` + v + `)\z`)
		if err != nil {
			continue // unreachable via config (validated at boot); see above
		}
		patterns = append(patterns, re)
	}
	r.responsesModels = patterns
	return r
}

// needsResponsesWire reports whether a hop is curated onto the Responses wire
// for the given client surface. Chat and anthropic clients both translate
// (the operator's list is the proof the hop only behaves on /v1/responses —
// same list, same master knob); responses clients pass through unchanged.
func (r *Resolver) needsResponsesWire(clientSurface provider.Surface, namespace, model string) bool {
	if clientSurface == provider.SurfaceResponses || len(r.responsesModels) == 0 {
		return false
	}
	full := namespace + "/" + model
	for _, re := range r.responsesModels {
		if re.MatchString(full) {
			return true
		}
	}
	return false
}

// messagesConventionReason names the one non-capability blocker of a Messages
// destination: the operator's usage_convention fact (schema v13) is unstated,
// so a cross-wire conversion onto that wire cannot normalize input tokens.
const messagesConventionReason = "messages destination needs an explicit usage_convention on the account (auto is unstated)"

// wireDecision is the outcome of wire-first selection for one candidate: the
// wire to dial plus the legacy flags that carry the same fact, or a typed
// refusal.
type wireDecision struct {
	wire          provider.Surface
	responsesWire bool
	translated    bool
	// refuse means the candidate cannot be dialed on ANY wire. A combo drops
	// the hop; a direct route (no hop to fall back to) answers the client.
	refuse *ErrWireUnsupported
	// refusePin carries a pin violation on the ACTUAL wire. Both paths fail
	// hard on it — a pin is a refusal, never a skip (see checkPin).
	refusePin error
}

// selectWire decides, for one candidate, the wire the upstream is spoken to on
// — BEFORE capability and pins can be consulted out of order:
//
//  1. An explicit translation.responses_models override WINS. It is operator
//     intent naming a wire; a capability gate that predates it must not quietly
//     reroute the hop onto the chat wire or drop it (both of which the
//     pre-selector order did).
//  2. Native proven-true OR untested stays native. NULL is "never probed", not
//     "impossible", so an untested hop keeps dialing the client's own wire.
//  3. Only a PROVEN-impossible native path opens an automatic alternative —
//     today exactly one: the anthropic→openai hop, and only when translation
//     is enabled AND the openai shape is proven. New cells join here in M5.
//  4. Nothing left → refused with the probe's own words.
//
// The pin is checked against the DECIDED wire inside this function, so a hop
// cannot pass a pin that guards a different wire than the one it dials.
func (r *Resolver) selectWire(ctx context.Context, acct *provider.Account, model, requested string, clientSurface provider.Surface) wireDecision {
	// 1. Curated responses-wire override.
	if r.needsResponsesWire(clientSurface, acct.Namespace, model) {
		rcap, rerr := r.catalog.ModelSupportsSurface(ctx, acct.ID, model, provider.SurfaceResponses)
		if rerr == nil && rcap != nil && !*rcap {
			// The override names responses and the probe disproves it:
			// dialing anyway spends a request on a wire that cannot answer.
			// (A lookup error or nil stays eligible — fail-open, like every
			// capability hint.)
			return wireDecision{refuse: &ErrWireUnsupported{
				Requested: requested, Wire: provider.SurfaceResponses,
				Reason: "probed unable to serve the responses surface",
			}}
		}
		if err := r.checkPin(ctx, acct.ID, model, requested, provider.SurfaceResponses); err != nil {
			return wireDecision{refusePin: err}
		}
		return wireDecision{wire: provider.SurfaceResponses, responsesWire: true}
	}

	// 1b. Universal: an explicit PIN names the wire. A pin is operator intent
	// (like the curated override), so it is honored BEFORE the native rule —
	// but only when the probe has not disproved the pinned wire, and a
	// Messages destination additionally needs the account's explicit usage
	// convention. universal_enabled is the SINGLE switch for the matrix: it
	// does not require the legacy translation.enabled gate (operator decision,
	// 2026-10-13).
	if r.universalEnabled {
		if pin, perr := r.catalog.ModelPin(ctx, acct.ID, model); perr == nil && pin != "" && pin != surfaceName(clientSurface) {
			wire, ok := pinSurface(pin)
			if !ok {
				return wireDecision{refuse: &ErrWireUnsupported{Requested: requested, Wire: clientSurface,
					Reason: fmt.Sprintf("model is pinned to an unknown wire %q", pin)}}
			}
			capability, cerr := r.catalog.ModelSupportsSurface(ctx, acct.ID, model, wire)
			if cerr != nil {
				capability = nil
			}
			if capability != nil && !*capability {
				return wireDecision{refuse: &ErrWireUnsupported{Requested: requested, Wire: wire,
					Reason: "model is pinned to a wire the probe disproved"}}
			}
			if wire == provider.SurfaceAnthropic && !acct.UsageConvention.Known() {
				return wireDecision{refuse: &ErrWireUnsupported{Requested: requested, Wire: wire,
					Reason: messagesConventionReason}}
			}
			return wireDecision{wire: wire, translated: true}
		}
	}

	// 2. Native when possible or unproven-impossible.
	native, err := r.catalog.ModelSupportsSurface(ctx, acct.ID, model, clientSurface)
	if err != nil {
		native = nil // lookup error = untested, fail-open
	}
	if native == nil || *native {
		if err := r.checkPin(ctx, acct.ID, model, requested, clientSurface); err != nil {
			return wireDecision{refusePin: err}
		}
		return wireDecision{wire: clientSurface}
	}

	// 3. Native is PROVEN impossible: the automatic alternatives.
	//
	// Universal: walk the fixed preference chain. A destination is used only
	// when it is PROVEN capable (NULL or a lookup error is not evidence), and
	// a Messages destination additionally requires the account's explicit
	// usage convention — an unstated convention would make the input-token
	// meaning of the upstream's own responses a guess.
	if r.universalEnabled {
		blockedByConvention := false
		for _, dest := range universalOrder(clientSurface) {
			capability, cerr := r.catalog.ModelSupportsSurface(ctx, acct.ID, model, dest)
			if cerr != nil {
				capability = nil
			}
			if capability == nil || !*capability {
				continue
			}
			if dest == provider.SurfaceAnthropic && !acct.UsageConvention.Known() {
				blockedByConvention = true
				continue
			}
			if err := r.checkPin(ctx, acct.ID, model, requested, dest); err != nil {
				return wireDecision{refusePin: err}
			}
			return wireDecision{wire: dest, translated: true}
		}
		if blockedByConvention {
			return wireDecision{refuse: &ErrWireUnsupported{Requested: requested, Wire: clientSurface,
				Reason: messagesConventionReason}}
		}
	}

	// Legacy (universal off): the single anthropic→openai alternative.
	openai, oerr := r.catalog.ModelSupportsSurface(ctx, acct.ID, model, provider.SurfaceOpenAI)
	if oerr != nil {
		openai = nil
	}
	if translate.Eligible(clientSurface, native, openai, r.translationEnabled) {
		if err := r.checkPin(ctx, acct.ID, model, requested, provider.SurfaceOpenAI); err != nil {
			return wireDecision{refusePin: err}
		}
		return wireDecision{wire: provider.SurfaceOpenAI, translated: true}
	}

	// 4. No wire left.
	return wireDecision{refuse: &ErrWireUnsupported{
		Requested: requested, Wire: clientSurface,
		Reason: "probed unable to serve the " + surfaceName(clientSurface) + " surface and not proven on another wire",
	}}
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

	// Wire-first selection (shared with combos): the curated responses
	// override, the capability facts and the PIN are all decided against the
	// wire this route will actually dial. A proven-impossible wire is refused
	// here rather than dialed: a direct route has no other hop to try, so
	// burning a request that cannot succeed only wastes the operator's quota.
	dec := r.selectWire(ctx, acct, model, requested, surface)
	if dec.refusePin != nil {
		return nil, dec.refusePin
	}
	if dec.refuse != nil {
		return nil, dec.refuse
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

	rt := proxy.Route{
		Account:  *acct,
		KeyID:    key.ID,
		KeyPlain: key.Plaintext,
		KeyHint:  key.Hint,
		Model:    model,
		Alias:    requested,
		Surface:  surface,
		// The curated responses-wire override applies to direct routes too:
		// a chat client calling the model by its namespaced id gets the same
		// wire fix as a combo hop.
		Translated:    dec.translated,
		ResponsesWire: dec.responsesWire,
	}
	if dec.wire != surface {
		rt.WireSurface = dec.wire
	}
	return []proxy.Route{rt}, nil
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

		first error // first hop's rejection, kept as the failure to report
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
		// Wire-first selection: the curated responses override, the capability
		// facts and the pin are decided TOGETHER, against the wire this hop
		// will actually be dialed on. Doing it in this order is what stops a
		// curated responses hop from being rerouted onto the chat wire (or
		// dropped) by a capability gate that never saw the override.
		dec := r.selectWire(ctx, acct, h.ModelID, name, surface)
		if dec.refusePin != nil {
			// A pin is a refusal, not a skip: the operator's constraint fails
			// the whole combo, exactly as it did before.
			return nil, dec.refusePin
		}
		if dec.refuse != nil {
			// A hop with no wire left is dropped like any other unusable hop;
			// the reason is kept for the "no usable hops" message.
			if first == nil {
				first = fmt.Errorf("hop %s/%s: %w", acct.Namespace, h.ModelID, dec.refuse)
			}
			continue
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
		route := proxy.Route{
			Account:  *acct,
			KeyID:    key.ID,
			KeyPlain: key.Plaintext,
			KeyHint:  key.Hint,
			Model:    h.ModelID,
			Alias:    name,
			Surface:  surface,
			// The route keeps the CLIENT's surface (ledger/usage tap); only
			// the wire shape flips, at dispatch time.
			Translated:    dec.translated,
			ResponsesWire: dec.responsesWire,
		}
		if dec.wire != surface {
			route.WireSurface = dec.wire
		}
		// Every candidate is a working member of the strategy: a hop whose
		// wire we translate is ordered, held good and rotated on exactly the
		// same terms as a native one (operator decision 2026-10-13 — a
		// successful translated hop IS a success, and a failure is a failure;
		// the strategies must not be able to see a difference).
		routes = append(routes, route)
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
	candidates, blockPos := orderRoutes(combo, routes)
	// BlockPos rides on EVERY candidate: it tags the DISPATCH (this request's
	// position inside its block), not a hop. Whichever route actually serves,
	// the ledger row carries the same tag — the "N requests finished" evidence.
	for i := range candidates {
		candidates[i].BlockPos = blockPos
	}
	return candidates, nil
}

// orderRoutes applies the combo's strategy. The candidate list is already
// filtered, so strategies only ever choose among hops that WILL work right
// now — none of them re-implements eligibility.
//
// Weights are handled by expansion (a weight-3 hop appears three times) rather
// than by a weighted cursor: it keeps the hot path free of arithmetic state,
// and the lists are tiny.
func orderRoutes(combo *store.Combo, routes []proxy.Route) ([]proxy.Route, int) {
	switch combo.Strategy {
	case "true_round_robin":
		return randomOrder(routes), 0
	case "strict_round_robin":
		return strictOrder(combo, routes), 0
	case "block_round_robin":
		return blockOrder(combo, routes)
	case "least_used":
		// Caller supplies hop order already; least-used needs usage data the
		// router does not hold, so keep position order until M4's ledger hook
		// lands. Ordering wrongly here would be worse than not ordering.
		return routes, 0
	case "sticky_last_good":
		return stickyOrder(combo, routes), 0
	default: // "failover"
		return routes, 0
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

// BlockCursor is one combo's live block_round_robin state, shaped for the
// dashboard's recent-calls table (the block cursor renders under the model
// of every row that alias served).
type BlockCursor struct {
	Combo     string `json:"combo"` // the ledger alias for combo-routed calls
	Hop       string `json:"hop"`   // namespace/model owning the current block
	Remaining int    `json:"remaining"`
	BlockSize int    `json:"block_size"`
	// Reason is WHY this block started: "cold start" (first dispatch, or after
	// a restart), "block complete" (the previous block ran its N), or "block
	// hop left candidates" (the owner dropped out of the eligible list).
	// It was logged once per advance and nowhere else — so a block that
	// advanced early looked identical, in the dashboard, to one that rotated
	// on schedule. Empty for a seeded (no live block yet) cursor.
	Reason string `json:"reason,omitempty"`
}

// BlockCursors snapshots the live block_round_robin cursors. Entries that
// never advanced (single-hop combos, block strategy unused) are skipped —
// they have nothing to observe.
func (r *Resolver) BlockCursors() []BlockCursor {
	out := []BlockCursor{}
	blockCursors.Range(func(k, v any) bool {
		e, ok := v.(*blockEntry)
		if !ok || e == nil {
			return true
		}
		e.mu.Lock()
		if e.combo != "" && e.ns != "" {
			out = append(out, BlockCursor{
				Combo: e.combo, Hop: e.ns + "/" + e.model,
				Remaining: e.remaining, BlockSize: e.size,
				Reason: e.reason,
			})
		}
		e.mu.Unlock()
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Combo < out[j].Combo })
	return out
}

// TestRoute resolves a single hop to a dialable Route with NO eligibility
// filtering, capability gating, or pin checks: the combo-test endpoint probes
// every hop exactly as configured, because showing WHY the router currently
// skips a hop is the point of the test. Resolution failures (account gone,
// no usable key) are the test's OUTPUT, not a handler error.
func (r *Resolver) TestRoute(ctx context.Context, accountID int64, model, comboName string, surface provider.Surface) (proxy.Route, error) {
	acct, err := r.catalog.AccountByID(ctx, accountID)
	if err != nil {
		return proxy.Route{}, err
	}
	key, err := r.catalog.PickKey(ctx, acct.ID)
	if err != nil {
		return proxy.Route{}, err
	}
	return proxy.Route{
		Account: *acct, KeyID: key.ID, KeyPlain: key.Plaintext, KeyHint: key.Hint,
		Model: model, Alias: comboName, Surface: surface,
	}, nil
}

type blockEntry struct {
	mu        sync.Mutex
	combo     string // combo NAME (the ledger alias) — set on first use
	accountID int64
	ns        string // namespace of the hop owning the block
	model     string
	remaining int
	size      int // effective block size for this block
	// reason is the last advance's reason (the same word the "block advance"
	// log line carries), retained so the dashboard can show WHY this hop
	// owns the block, not just that it does.
	reason string
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
//
// Every dispatch is COUNTED, including stretches where fewer than two hops are
// eligible: N must mean N dispatches everywhere, or the per-row block tag lies
// about how much of the block was spent. With a single candidate the rotation
// "advances" onto the same hop and the advance log says so — that is the
// operator's evidence that a long visible run was eligibility, not arithmetic.
// The second return is this dispatch's 1-based position inside its block.
func blockOrder(combo *store.Combo, routes []proxy.Route) ([]proxy.Route, int) {
	// len(routes)==0 cannot happen (the caller checked), but never index into
	// an empty list regardless.
	if len(routes) == 0 {
		return routes, 0
	}
	size := combo.BlockSize
	if size < 1 {
		size = 1
	}

	v, _ := blockCursors.LoadOrStore(combo.ID, &blockEntry{})
	e := v.(*blockEntry)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.combo == "" {
		e.combo = combo.Name
	}

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
		reason := "cold start"
		if e.accountID != 0 && target < 0 {
			reason = "block hop left candidates"
		}
		next := 0
		if target >= 0 {
			next = (target + 1) % len(routes)
			reason = "block complete"
		}
		e.accountID, e.model = routes[next].Account.ID, routes[next].Model
		e.ns = routes[next].Account.Namespace
		e.remaining = size
		e.size = size
		e.reason = reason
		target = next
		// The cursor is in-process state with no other visibility: a block that
		// advances early (eligibility flap) or never advances (a sticky
		// healthy hop) looks identical from the ledger. One line per ADVANCE
		// — never per request — makes it observable without noise.
		slog.Info("block advance",
			"combo", combo.Name,
			"hop", routes[next].Account.Namespace+"/"+routes[next].Model,
			"reason", reason,
			"block_size", size)
	}
	e.remaining--
	// Position AFTER the decrement: 1..size. A dispatch that spends the last
	// slot of its block reports pos == size, and the NEXT dispatch advances.
	return rotate(uint64(target), routes), size - e.remaining
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
