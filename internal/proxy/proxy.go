// Package proxy forwards a client request upstream and copies the response back
// byte-for-byte, tapping usage for the ledger.
//
// THE INVARIANT: for streaming responses we never buffer, never re-serialize,
// and never add latency. Bytes go upstream->client via io.Copy semantics with a
// Flush per chunk; the usage tap is a read-only observer on a tee.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/c4lyp5o/ezllm/internal/jsonbody"
	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/ratelimit"
	"github.com/c4lyp5o/ezllm/internal/store"
	"github.com/c4lyp5o/ezllm/internal/translate"
)

// maxBodyBytes caps the request body we will read into memory (we must read it
// to swap `model`). 32 MiB is generous for chat payloads and bounds abuse.
const maxBodyBytes = 32 << 20

// maxErrSnippet bounds the upstream error text stored in the ledger's err
// column. The ledger is not a request log: 512 bytes holds every provider's
// error envelope (message, type, code, request id) and stops a chatty 4xx from
// bloating the table.
const maxErrSnippet = 512

// maxStreamBytes bounds what the usage tap inspects. The response itself is
// NOT bounded (it is streamed), only the per-line scan buffer.
const maxScanToken = 4 << 20

// UsageTapper accumulates usage across an SSE stream.
//
// A single stream can carry usage in SEVERAL events, and the later ones are
// authoritative:
//
//	anthropic  message_start gives input/cache breakdown with output_tokens=0,
//	           then message_delta gives the FINAL input+output totals. Tapping
//	           only the first event records out=0 (observed live).
//	responses  usage arrives on response.completed OR, when the response is
//	           truncated by max_output_tokens, on response.incomplete. Tapping
//	           only .completed records all-zero usage (observed live).
//
// So we merge rather than take the first hit.
type UsageTapper struct {
	surface provider.Surface
	cur     *store.Usage
}

// NewUsageTapper builds a tapper for one surface.
func NewUsageTapper(surface provider.Surface) *UsageTapper {
	return &UsageTapper{surface: surface}
}

// Observe inspects one SSE data line (read-only; bytes are forwarded untouched).
func (t *UsageTapper) Observe(line []byte) {
	if len(line) < 6 || !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	if u := tapSSELine(line, t.surface); u != nil {
		t.cur = MergeUsage(t.cur, u)
	}
}

// Usage returns the merged result, or nil when the stream carried none.
func (t *UsageTapper) Usage() *store.Usage { return t.cur }

// ErrUnknownModel is returned when the requested model cannot be resolved.
var ErrUnknownModel = errors.New("proxy: unknown model")

// errAttemptBudgetSpent is the request-wide budget (addendum #3) saying no.
// It is an operator-visible condition, not an upstream failure: the ledger
// note names the budget rather than blaming a provider.
var errAttemptBudgetSpent = errors.New("proxy: request attempt budget spent")

// Route is a resolved destination: one account + one key + the model to send.
type Route struct {
	Account  provider.Account
	KeyID    int64
	KeyPlain string // decrypted; NEVER logged
	KeyHint  string
	Model    string // model id to send upstream
	Alias    string // what the client asked for (combo name or namespace/model)
	Surface  provider.Surface
	// Translated marks a hop that speaks only the OpenAI wire shape while
	// the client speaks anthropic: the request body is translated on the
	// way out and the response on the way back, while Surface stays the
	// CLIENT's surface (the ledger and the usage tap must never see the
	// translation). Set by the router from probed capability; nothing else
	// about the hop's failover/park semantics changes.
	Translated bool
	// HoldUntilFailure (block_round_robin only) marks a hop the operator
	// wants held: it keeps the block while it succeeds and releases it on a
	// hop-attributable failure (see Resolver.MarkHopBad). It affects nothing
	// else — eligibility, failover and the ledger are unchanged.
	HoldUntilFailure bool
	// BlockPos tags the DISPATCH, not the hop: this request's 1-based
	// position inside its block_round_robin block (0 = not counted). The
	// router sets the SAME value on every candidate of a dispatch, so the
	// ledger row carries it no matter which hop actually served.
	BlockPos int
	// ResponsesWire marks a hop curated onto OpenAI's Responses wire
	// (translation.responses_models) while the client speaks chat OR
	// anthropic: the body is translated onto the responses wire on the way
	// out and back into the CLIENT's shape on the way in. Same rules as
	// Translated — Surface stays the client's; error bodies pass through
	// byte-identical (all wires share the {"error":{…}} family).
	ResponsesWire bool
	// WireSurface is the wire the upstream is ACTUALLY spoken to on, when it
	// differs from the client's. It is the single source of truth for the dial
	// path, the pin target and the codec choice — Translated/ResponsesWire are
	// the same fact in legacy form (see EffectiveWire). Empty = native.
	WireSurface provider.Surface
}

// EffectiveWire is THE wire selector: what we speak upstream. Native routes
// (the overwhelming majority) leave WireSurface empty and answer with the
// client's own surface; the legacy Translated/ResponsesWire flags are honoured
// so a Route built before the field existed keeps its meaning.
func (rt Route) EffectiveWire() provider.Surface {
	if rt.WireSurface != "" {
		return rt.WireSurface
	}
	switch {
	case rt.Translated:
		return provider.SurfaceOpenAI
	case rt.ResponsesWire:
		return provider.SurfaceResponses
	}
	return rt.Surface
}

// Attempt is one candidate that did NOT serve the request, in the order the
// dispatcher abandoned it. It exists because a failed or skipped hop writes no
// ledger row of its own (one request = one row, and that row is the billing
// truth) — without the trail, the hops that "hopped" are invisible to the
// operator's recent-calls view and only exist in logs.
type Attempt struct {
	Namespace string // hop namespace
	Model     string // hop model
	Status    int    // the upstream status the hop earned (0 = never dialed)
	Note      string // short reason: classified cause, skip reason, transport error
}

// Result carries what the ledger needs after a dispatch. Attempts lists every
// candidate that did not serve, dispatch order; the serving hop is never listed.
type Result struct {
	Status        int
	Stream        bool
	TTFTms        *int64
	Totalms       *int64
	Usage         *store.Usage
	EndpointID    string
	UpstreamModel string
	Err           string
	Attempts      []Attempt
	// UpstreamAttempts is how many upstream calls this REQUEST actually made
	// — same-hop retries and failover hops counted together (addendum #3).
	// AttemptBudget is the configured request-wide budget; 0 means UNLIMITED
	// (the legacy behaviour), recorded as such so an unknown budget is never
	// mistaken for a zero-left budget.
	UpstreamAttempts int
	AttemptBudget    int
	// Committed reports whether commit already called WriteHeader on the
	// client. It exists because Status is set from the upstream in attempt
	// BEFORE anything is written: on a body-read failure commit can return an
	// error while still carrying a 200, and a caller that guards its error
	// response on Status would leave net/http to emit its default 200 with an
	// Only this flag distinguishes "has a status" from "has been
	// sent".
	Committed bool
	// StreamOutcome reports how a TRANSLATED stream ended after its bytes
	// reached the client: completed, failed (a late error frame), or
	// truncated (the source ended without a terminal event — including the
	// legacy synthesized close, which looks successful to a legacy client).
	// Empty for native streams and for a stream the client abandoned before
	// its terminal (client disconnects say nothing about the hop).
	StreamOutcome string
}

// Stream-delivery outcomes, in the vocabulary the ledger and hop-health read.
const (
	StreamOutcomeCompleted = "completed"
	StreamOutcomeFailed    = "failed"
	StreamOutcomeTruncated = "truncated"
)

// StreamCompleted reports whether the delivery outcome permits treating the
// hop as having served the request. An unknown outcome (native stream, or a
// stream cut short by the client) is not evidence AGAINST the hop, so it
// passes; a failed or truncated translation is evidence against it.
func (r Result) StreamCompleted() bool {
	return r.StreamOutcome == "" || r.StreamOutcome == StreamOutcomeCompleted
}

// Rewriter resolves the client's requested model into a Route. The router
// package implements it (M4); M2 wires a direct single-account resolver.
type Rewriter interface {
	Resolve(ctx context.Context, requestedModel string, surface provider.Surface, client string) (Route, error)
}

// Dispatcher performs the actual upstream call.
type Dispatcher struct {
	client        *http.Client
	registry      *provider.Registry
	retryAttempts int
	// attemptBudget is the request-wide upstream-attempt budget (addendum #3):
	// same-hop retries and failover hops counted together. <=0 = unlimited.
	attemptBudget int
	// dispatchDeadline bounds the selection loop only (never a live stream).
	dispatchDeadline time.Duration
	cooldownMu       sync.Mutex
	cooldowns        map[string]cooldownState
	// on429/on5xx are the operator's park durations and maxStrikes the failure
	// count needed to park, all guarded by cooldownMu. Zero durations mean
	// "fall back to defaultHopCooldown"; maxStrikes < 1 means park on the first
	// failure.
	on429      time.Duration
	on5xx      time.Duration
	maxStrikes int

	// limiter is the account-wide request budget. Nil disables rate gating
	// entirely (the pre-limiter behaviour), which is what keeps this optional
	// for tests and for callers that never set it.
	limiter *ratelimit.Limiter

	// log makes failover VISIBLE to the operator. Failover is supposed to be
	// invisible to the client, and it was invisible to everyone: a hop that was
	// skipped (parked) writes no ledger row, and a hop that failed and was
	// replaced writes none either, so a combo could quietly run on one healthy
	// hop for hours with no record anywhere of why. Never nil — defaults to
	// slog.Default().
	log *slog.Logger
}

type cooldownState struct {
	until       time.Time
	probeActive bool
	// probeSince bounds a half-open probe. The probe is released when the
	// upstream answers its HEADERS, not when its body ends, so a slow provider
	// (measured 11-14s TTFT on opencode-go) held the gate that long and every
	// other caller was told "all combo endpoints are in five-minute cooldown"
	// by a hop that was NOT parked. See acquireHop/probeHold.
	probeSince time.Time
	// reason is the operator-facing cause ("429 rate limited", "transport
	// error"). A parked hop writes no ledger row — the request never reaches an
	// upstream — so this is the only place the cause can be reported.
	reason string
	// namespace labels the entry for /admin/health; the park key is by account
	// id and base URL, which is not readable at a glance.
	namespace string
	// model is the parked hop's model, so an operator can see WHICH of an
	// account's many models is cooling rather than just the account.
	model string
	// strikes counts CONSECUTIVE failures. Parking is deferred until it reaches
	// maxStrikes so one transient blip does not remove a healthy hop for the
	// whole cooldown window; a success resets it.
	strikes int
}

// hopOutcome classifies one hop attempt: whether it failed, and how long the
// hop should be parked as a result (0 = leave it available).
//
// failed and cooldown are separate because they answer different questions. A
// 429 on a combo's ONLY hop is a failure with nothing to park for — cooling it
// would strand the request, so the hop stays available and the client gets the
// upstream's real answer instead of a synthetic refusal.
type hopOutcome struct {
	failed   bool
	cooldown time.Duration
	// reason is the operator-facing cause, surfaced on /admin/health. A parked
	// hop produces no ledger row, so without this a hop could disappear from
	// routing with no recorded explanation anywhere.
	reason string
}

const (
	// defaultHopCooldown parks a hop whose failure carries neither an upstream
	// Retry-After nor an operator duration (transport errors).
	defaultHopCooldown = 5 * time.Minute

	// maxHonouredRetryAfter bounds an upstream-supplied Retry-After. Providers
	// may legitimately ask for an hour; honouring that blindly would remove the
	// hop for the rest of the session. A value beyond this is treated as absent
	// so the operator's configured duration applies instead.
	maxHonouredRetryAfter = 15 * time.Minute

	// probeHold bounds how long one half-open probe may keep an EXPIRED park
	// closed to everyone else. The intent of the probe election is to stop a
	// thundering herd from stampeding an endpoint whose window has just rolled
	// over — not to serialize all traffic behind the one caller that happens to
	// probe first. Without a bound, a probe against a slow provider blocked the
	// endpoint for the whole upstream round-trip and callers were refused with
	// "all combo endpoints are in five-minute cooldown" by a hop that was not
	// parked: an expired park must not look parked.
	probeHold = 30 * time.Second
)

// hopKey identifies one parkable hop: account + endpoint + MODEL.
//
// The model belongs in the key because a 429 is nearly always a per-model
// condition — opencode-go meters each model separately, every router tiers per
// model — while an account-only key meant ONE throttled model removed every model
// on that account from every combo that referenced it. Measured live 2026-10-11:
// a single unavailable opencode-go model ("Endpoint is unavailable", HTTP 429)
// silently took all 38 of that account's models out of routing, 60s at a time,
// which is how a free model that had served 235 calls in an afternoon shrank to a
// few per hour. An endpoint-wide fault (transport error, 5xx) now parks each
// model as it is tried, costing at most one extra failed attempt per model — what
// the half-open probe would have done anyway.
func hopKey(rt Route) string {
	return fmt.Sprintf("%d|%s|%s", rt.Account.ID, strings.TrimRight(rt.Account.BaseURL, "/"), rt.Model)
}

// acquireHop grants one request the right to try an endpoint. A cooled endpoint
// has exactly one half-open probe after the five-minute window; concurrent
// callers skip it until that probe succeeds, fails, or exceeds probeHold.
func (d *Dispatcher) acquireHop(rt Route, now time.Time) (allowed, probe bool) {
	key := hopKey(rt)
	d.cooldownMu.Lock()
	defer d.cooldownMu.Unlock()
	if d.cooldowns == nil {
		return true, false
	}
	state, exists := d.cooldowns[key]
	if !exists {
		return true, false
	}
	// Deny only while the park is live, or while a FRESH probe is deciding the
	// endpoint's fate. Once the window has expired and any probe is older than
	// probeHold, the hop is eligible again: reporting a healthy endpoint as
	// parked is worse than one extra request against a bad one, and the strike
	// counter re-parks it on the next real failure.
	if state.until.After(now) || (state.probeActive && now.Sub(state.probeSince) < probeHold) {
		return false, false
	}
	state.probeActive = true
	state.probeSince = now
	d.cooldowns[key] = state
	return true, true
}

// Cooldown is one parked endpoint as an operator should see it: which account is
// cooling, until when, why, and whether a recovery probe is in flight.
type Cooldown struct {
	Namespace   string    `json:"namespace"`
	Model       string    `json:"model"`
	Until       time.Time `json:"until"`
	Strikes     int       `json:"strikes"`
	ProbeActive bool      `json:"probe_active"`
	Reason      string    `json:"reason"`
}

// skipReason explains, in one operator-readable line, why acquireHop refused a
// hop. Read WITHOUT the caller holding the lock so the hot path never writes to
// a log sink while holding cooldownMu.
func (d *Dispatcher) skipReason(rt Route, now time.Time) string {
	key := hopKey(rt)
	d.cooldownMu.Lock()
	defer d.cooldownMu.Unlock()
	st, ok := d.cooldowns[key]
	if !ok {
		return "unknown"
	}
	if st.until.After(now) {
		return fmt.Sprintf("parked until %s (%s)", st.until.UTC().Format(time.RFC3339), st.reason)
	}
	if st.probeActive && now.Sub(st.probeSince) < probeHold {
		return "half-open probe in flight"
	}
	return "unknown"
}

// Cooldowns snapshots the parks that are currently keeping traffic off an
// endpoint (still cooling, or probing). A parked hop is INVISIBLE in the ledger
// — the request never reaches an upstream, so no row is written — which made a
// silently-skipped account look exactly like a broken one. This is the operator's
// only view of it, so it reports cause as well as fact.
func (d *Dispatcher) Cooldowns() []Cooldown {
	now := time.Now()
	d.cooldownMu.Lock()
	defer d.cooldownMu.Unlock()
	out := make([]Cooldown, 0, len(d.cooldowns))
	for _, st := range d.cooldowns {
		if !st.until.After(now) && !st.probeActive {
			continue // expired and idle: no longer holding traffic back
		}
		out = append(out, Cooldown{
			Namespace:   st.namespace,
			Model:       st.model,
			Until:       st.until,
			Strikes:     st.strikes,
			ProbeActive: st.probeActive,
			Reason:      st.reason,
		})
	}
	// Stable order: the dashboard and its tests both read this list, and a map
	// walk would reshuffle it on every call.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Model < out[j].Model
	})
	return out
}

func (d *Dispatcher) releaseProbe(rt Route) {
	key := hopKey(rt)
	d.cooldownMu.Lock()
	defer d.cooldownMu.Unlock()
	if state, ok := d.cooldowns[key]; ok && state.probeActive {
		state.probeActive = false
		d.cooldowns[key] = state
	}
}

// classifyHop decides what one hop attempt means: whether it failed, and how
// long the hop should be parked for.
//
// Precedence for the park duration: an explicit upstream Retry-After wins (the
// provider alone knows when its own window resets), then the operator's
// cooldown.on_429/on_5xx, then the built-in default.
//
// hasAlternate is what keeps cooling honest. Parking a hop is an optimisation:
// it lets the NEXT call skip straight to a hop that still has budget instead of
// re-discovering the same 429. When this hop is the only candidate, that
// optimisation has nothing to optimise — the choice is "try again and get a
// real answer once the window rolls over" versus "synthesise a refusal from a
// stale local counter". The first is strictly more useful, so a sole hop is
// never parked for a rate limit. A hop that is genuinely broken (5xx,
// transport error) is still parked: those failures mean "do not send traffic
// here", which a single hop cannot escape anyway.
func (d *Dispatcher) classifyHop(err error, status int, hdr http.Header, hasAlternate bool) hopOutcome {
	if err != nil {
		if !retryableUpstreamError(err) {
			return hopOutcome{} // our own fault (bad kind, bad request shape): not the hop's
		}
		return hopOutcome{failed: true, cooldown: defaultHopCooldown, reason: "transport error"}
	}
	switch {
	case status == http.StatusTooManyRequests:
		if !hasAlternate {
			// Sole hop: fail, but do not park — a real attempt is better than a
			// synthetic refusal once the window rolls.
			return hopOutcome{failed: true, reason: "429 rate limited (sole hop, not parked)"}
		}
		if dur, ok := parseRetryAfter(hdr.Get("Retry-After")); ok {
			return hopOutcome{failed: true, cooldown: dur, reason: "429 rate limited (Retry-After)"}
		}
		if d.on429 > 0 {
			return hopOutcome{failed: true, cooldown: d.on429, reason: "429 rate limited"}
		}
		return hopOutcome{failed: true, cooldown: defaultHopCooldown, reason: "429 rate limited"}
	case status >= 500 || status == http.StatusRequestTimeout || status == http.StatusTooEarly:
		reason := fmt.Sprintf("%d upstream error", status)
		if dur, ok := parseRetryAfter(hdr.Get("Retry-After")); ok {
			return hopOutcome{failed: true, cooldown: dur, reason: reason + " (Retry-After)"}
		}
		// 408/425 are transient by definition and share the 5xx duration: the
		// operator configured one "hop is unhealthy" number, not two.
		if d.on5xx > 0 && status >= 500 {
			return hopOutcome{failed: true, cooldown: d.on5xx, reason: reason}
		}
		return hopOutcome{failed: true, cooldown: defaultHopCooldown, reason: reason}
	}
	return hopOutcome{} // 2xx, or a 4xx that describes the request rather than the hop
}

// parseRetryAfter parses the two spellings RFC 9110 allows for Retry-After:
// delta-seconds and an HTTP-date. Anything absent, non-positive, unparseable,
// or longer than maxHonouredRetryAfter reports false, so a bogus or hostile
// header can neither pin a hop for a session nor be silently clamped into a
// duration the upstream never asked for.
func parseRetryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0, false
		}
		d := time.Duration(secs) * time.Second
		if d > maxHonouredRetryAfter {
			return 0, false
		}
		return d, true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d <= 0 || d > maxHonouredRetryAfter {
			return 0, false
		}
		return d, true
	}
	return 0, false
}

// recordHop applies the result of one hop attempt to its cooldown state.
//
// A success clears the hop outright — both the park and its strike count. A
// failure banks a strike and only parks once the threshold is reached, so a
// single transient blip does not remove a healthy hop for the whole cooldown
// window. A failure with nothing to park for (a sole hop that 429'd) banks
// nothing: there is no alternate to protect, so a strike could never be acted
// on.
func (d *Dispatcher) recordHop(rt Route, oc hopOutcome, now time.Time) {
	key := hopKey(rt)
	d.cooldownMu.Lock()
	defer d.cooldownMu.Unlock()

	if !oc.failed {
		if d.cooldowns != nil {
			delete(d.cooldowns, key)
		}
		return
	}
	if oc.cooldown <= 0 {
		return
	}
	if d.cooldowns == nil {
		d.cooldowns = make(map[string]cooldownState)
	}
	st := d.cooldowns[key]
	st.strikes++
	threshold := d.maxStrikes
	if threshold < 1 {
		threshold = 1
	}
	if st.strikes >= threshold {
		st.until = now.Add(oc.cooldown)
		st.strikes = 0
		st.namespace = rt.Account.Namespace
		st.model = rt.Model
		st.reason = oc.reason
		// Park events are rare (threshold crossings), so this stays inside the
		// lock: it is the one line that explains an endpoint disappearing.
		d.log.Info("hop parked",
			"account", rt.Account.Namespace, "model", rt.Model,
			"until", st.until.UTC().Format(time.RFC3339), "reason", oc.reason)
	}
	d.cooldowns[key] = st
}

// SetLimiter attaches the account-wide request-budget gate. Nil (the default)
// disables rate gating.
func (d *Dispatcher) SetLimiter(l *ratelimit.Limiter) {
	d.limiter = l
}

// reserveRate takes one request from the hop's account budget.
//
// Called immediately before dispatch, never during resolution: the router
// visits every hop to build the candidate list, so reserving there would charge
// requests to hops that are never called.
func (d *Dispatcher) reserveRate(rt Route) (bool, time.Time) {
	if d.limiter == nil {
		return true, time.Time{}
	}
	return d.limiter.Allow(rt.Account.ID, rt.Account.RateLimit, rt.Account.RateWindow)
}

// RateLimited reports that every candidate hop was out of its configured
// request budget. This is a REFUSAL, not a gateway fault: the client should wait
// until NextAllowed rather than treat it as an outage, which is why the server
// answers 429 and not 502.
type RateLimited struct {
	Account     string
	Limit       int64
	Window      time.Duration
	NextAllowed time.Time
}

func (e *RateLimited) Error() string {
	msg := fmt.Sprintf("account %q is at its request budget (%d per %s)", e.Account, e.Limit, e.Window)
	if !e.NextAllowed.IsZero() {
		msg += "; next request allowed at " + e.NextAllowed.Format(time.RFC3339)
	}
	return msg
}

// ConfigureCooldown applies the operator's cooldown settings. A zero or negative
// value leaves the built-in default in place, so a partial or hand-edited config
// can never silently disable cooling.
func (d *Dispatcher) ConfigureCooldown(on429, on5xx time.Duration, maxStrikes int) {
	d.cooldownMu.Lock()
	defer d.cooldownMu.Unlock()
	if on429 > 0 {
		d.on429 = on429
	}
	if on5xx > 0 {
		d.on5xx = on5xx
	}
	if maxStrikes > 0 {
		d.maxStrikes = maxStrikes
	}
}

// NewDispatcher builds a Dispatcher. timeout governs connection/response-header
// time only; streaming bodies are unbounded by design.
func NewDispatcher(client *http.Client, registry *provider.Registry) *Dispatcher {
	return &Dispatcher{
		client:        client,
		registry:      registry,
		retryAttempts: 3,
		cooldowns:     make(map[string]cooldownState),
		on429:         defaultHopCooldown,
		on5xx:         defaultHopCooldown,
		// Park on the first failure unless the operator says otherwise. A
		// threshold is only worth having once the operator has stated it; the
		// conservative default keeps a failing hop out of rotation immediately.
		maxStrikes: 1,
		log:        slog.Default(),
	}
}

// SetLogger installs the operator-facing logger used for failover diagnostics.
// Nil restores the default so a caller can never silence the dispatcher by
// accident.
func (d *Dispatcher) SetLogger(l *slog.Logger) {
	if l == nil {
		l = slog.Default()
	}
	d.log = l
}

// SetAttemptBudget bounds the TOTAL upstream attempts one request may make
// across same-hop retries AND failover hops (addendum #3). <=0 means
// unlimited — the legacy behaviour, recorded explicitly as unlimited so an
// unknown budget is never mistaken for a zero-left budget.
func (d *Dispatcher) SetAttemptBudget(n int) {
	d.attemptBudget = n
}

// SetDispatchDeadline bounds the SELECTION loop only: the time spent choosing
// and waiting between attempts. The winning attempt keeps its own upstream
// timeout (streaming responses have no natural deadline — config documents
// that), so this never truncates a live stream.
func (d *Dispatcher) SetDispatchDeadline(deadline time.Duration) {
	d.dispatchDeadline = deadline
}

// attemptLedger is the request-wide attempt budget: one instance per
// ForwardCandidatesDoc call, shared by every hop's retry loop.
type attemptLedger struct {
	limit int // <=0 unlimited
	used  int
}

// consume records one upstream attempt. ok=false means the budget is spent.
func (l *attemptLedger) consume() bool {
	if l.limit > 0 && l.used >= l.limit {
		return false
	}
	l.used++
	return true
}

func (l *attemptLedger) exhausted() bool {
	return l.limit > 0 && l.used >= l.limit
}

// pastDeadline reports whether the selection deadline has passed.
func pastDeadline(sel time.Time) bool {
	return !sel.IsZero() && !time.Now().Before(sel)
}

// budgetLabel renders the budget for ledger notes: unlimited is a word.
func budgetLabel(limit int) string {
	if limit <= 0 {
		return "unlimited"
	}
	return strconv.Itoa(limit)
}

func (d *Dispatcher) SetRetries(retries int) {
	if retries < 0 {
		retries = 0
	}
	if retries > 9 {
		retries = 9
	}
	d.retryAttempts = retries
}

// SwapModel rewrites the `model` field of a JSON request body, preserving every
// other field (unknown provider-specific fields such as extra_body, thinking,
// enable_thinking, tools, reasoning, previous_response_id): the new model is
// spliced into the client's own bytes, or the body is round-tripped through a
// map where a splice cannot normalize it (see jsonbody.SpliceModel).
func SwapModel(body []byte, newModel string) ([]byte, string, error) {
	if len(body) == 0 {
		return nil, "", errors.New("proxy: empty body")
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // avoid float64 mangling of large ints
	if err := dec.Decode(&m); err != nil {
		return nil, "", fmt.Errorf("proxy: body is not a JSON object: %w", err)
	}
	// `null`, `[]`, `"str"` etc. decode without error into a nil/non-object map;
	// assigning into a nil map panics, so reject explicitly.
	if m == nil {
		return nil, "", errors.New("proxy: body is not a JSON object")
	}
	old, _ := m["model"].(string)
	// The same splice-or-re-marshal choice jsonbody.WithModel makes: both
	// paths must emit identical bytes (docs/single-parse-dispatch-design.md).
	if out, ok := jsonbody.SpliceModel(body, newModel); ok {
		return out, old, nil
	}
	m["model"] = newModel
	out, err := json.Marshal(m)
	if err != nil {
		return nil, old, fmt.Errorf("proxy: re-marshal: %w", err)
	}
	return out, old, nil
}

// IsStream reports whether the body asks for streaming (without mutating it).
func IsStream(body []byte) bool {
	var m struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}
	return m.Stream
}

// isMultipartBody reports whether the body looks like a multipart/form-data
// payload (audio STT). Multipart bodies start with a boundary marker "--".
func isMultipartBody(body []byte) bool {
	return bytes.HasPrefix(body, []byte("--"))
}

// extractMultipartBoundary reads the boundary from the first line of a
// multipart body ("--<boundary>\r\n").
func extractMultipartBoundary(body []byte) string {
	eol := bytes.IndexAny(body, "\r\n")
	if eol < 0 {
		return ""
	}
	return strings.TrimPrefix(string(body[:eol]), "--")
}

// swapModelMultipart rebuilds a multipart/form-data body with the "model" form
// field replaced by newModel. Every other part (the audio file, language,
// prompt, etc.) is copied through byte-for-byte. The boundary is extracted
// from the body's own first line, so no external Content-Type is needed.
func swapModelMultipart(body []byte, newModel string) ([]byte, error) {
	if bytes.IndexAny(body, "\r\n") < 0 {
		return nil, errors.New("proxy: multipart body has no boundary line")
	}
	boundary := extractMultipartBoundary(body)
	if boundary == "" {
		return nil, errors.New("proxy: multipart body has empty boundary")
	}
	// Use mime/multipart to read the parts, then rebuild with the model swapped.
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	var out bytes.Buffer
	mw := multipart.NewWriter(&out)
	if err := mw.SetBoundary(boundary); err != nil {
		return nil, fmt.Errorf("proxy: multipart boundary: %w", err)
	}
	var swapped bool
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("proxy: read multipart part: %w", err)
		}
		value, err := io.ReadAll(part)
		if err != nil {
			return nil, fmt.Errorf("proxy: read multipart part body: %w", err)
		}
		fieldName := part.FormName()
		// Swap the model field; pass everything else through untouched.
		if fieldName == "model" {
			value = []byte(newModel)
			swapped = true
		}
		// Preserve the original part header (filename, content-type).
		h := textproto.MIMEHeader(part.Header)
		w, err := mw.CreatePart(h)
		if err != nil {
			return nil, fmt.Errorf("proxy: create multipart part: %w", err)
		}
		if _, err := w.Write(value); err != nil {
			return nil, fmt.Errorf("proxy: write multipart part: %w", err)
		}
	}
	if !swapped {
		// The request carried no model field at all: the handler resolved a
		// service default, so APPEND the model field this hop must carry
		// instead of refusing. A model-less multipart with no default is
		// refused before dispatch — injection only happens for a resolved one.
		if err := mw.WriteField("model", newModel); err != nil {
			return nil, fmt.Errorf("proxy: write multipart model field: %w", err)
		}
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("proxy: close multipart writer: %w", err)
	}
	return out.Bytes(), nil
}

// isCloudflare reports whether the hop dials Cloudflare Workers AI, whose
// image and audio models take native bodies at native paths.
func isCloudflare(rt Route) bool {
	return strings.Contains(rt.Account.BaseURL, "api.cloudflare.com")
}

// prepareHopBody rewrites the client's body for one hop: a byte-stable model
// swap on the native path, or a full wire translation when the upstream wire
// differs from the client's. The WIRE comes from EffectiveWire — the same
// value attempt dials and commit unwraps — so a hop can never be dialed on one
// wire while its body is encoded for another. Translation REPLACES the swap:
// it rewrites the whole wire shape, model included, so the paths never compose.
func prepareHopBody(body []byte, rt Route, doc *jsonbody.BodyDoc) ([]byte, error) {
	// Google AI Studio native audio (TTS/STT) needs a generateContent
	// translation, not the OpenAI envelope. It runs before the multipart
	// branch because the STT input IS multipart and must be unwrapped.
	if isGeminiNativeAudio(rt, rt.Surface) {
		return geminiAudioRequest(body, rt.Surface)
	}
	// Cloudflare MeloTTS takes {"prompt": text}, not the OpenAI
	// {"model","input","voice"} envelope.
	if rt.Surface == provider.SurfaceAudioTTS && isCloudflare(rt) {
		var in struct {
			Input string `json:"input"`
			Text  string `json:"text"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, fmt.Errorf("proxy: cloudflare tts body: %w", err)
		}
		text := in.Input
		if text == "" {
			text = in.Text
		}
		return json.Marshal(map[string]any{"prompt": text})
	}
	// Cloudflare flux-2 models require a multipart body (prompt as a form
	// field) rather than the JSON envelope. Build it here so the proxy's
	// content-type detection picks up the boundary.
	if rt.Surface == provider.SurfaceImagesGen && isCloudflare(rt) && strings.Contains(rt.Model, "flux-2") {
		var in struct {
			Prompt string `json:"prompt"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, fmt.Errorf("proxy: cloudflare image body: %w", err)
		}
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		if err := mw.WriteField("prompt", in.Prompt); err != nil {
			return nil, err
		}
		if err := mw.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	// Cloudflare Workers AI image models (non-flux-2) are native: the model is
	// in the URL and the JSON body accepts ONLY prompt-like fields.
	if rt.Surface == provider.SurfaceImagesGen && isCloudflare(rt) {
		var in struct {
			Prompt string `json:"prompt"`
			Seed   any    `json:"seed"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, fmt.Errorf("proxy: cloudflare image body: %w", err)
		}
		out := map[string]any{"prompt": in.Prompt}
		if in.Seed != nil {
			out["seed"] = in.Seed
		}
		return json.Marshal(out)
	}
	// Multipart (audio STT): the model lives in a form field, not a JSON key,
	// and SwapModel is JSON-only. Rebuild the multipart body with the model
	// field swapped; everything else (the audio file part) passes through
	// byte-for-byte.
	if isMultipartBody(body) {
		return swapModelMultipart(body, rt.Model)
	}
	wire := rt.EffectiveWire()
	if wire != rt.Surface {
		// The three NEW cells ride the strict surface-neutral codecs; an
		// unsupported request feature is a typed refusal BEFORE any upstream
		// call (zero spend, no blind failover).
		if translate.IsUniversalCell(rt.Surface, wire) {
			return translate.RequestUniversal(body, rt.Surface, wire, rt.Model)
		}
		// Legacy cells keep their characterized converters.
		switch {
		case wire == provider.SurfaceOpenAI:
			return translate.Request(body, rt.Model)
		case wire == provider.SurfaceResponses && rt.Surface == provider.SurfaceAnthropic:
			return translate.AnthropicToResponses(body, rt.Model)
		case wire == provider.SurfaceResponses && rt.Surface == provider.SurfaceOpenAI:
			return translate.ChatToResponses(body, rt.Model)
		}
		// A cross-wire pair with no converter must never fall through to the
		// model swap: that would send the client's own shape to a wire that
		// cannot read it.
		return nil, fmt.Errorf("proxy: no request converter for %s client on the %s wire", rt.Surface, wire)
	}
	// Native: same shape on both ends, only the model id is swapped. When the
	// caller already decoded the body once (single-parse dispatch), mutate
	// that shared map instead of decoding again; a nil doc falls back to the
	// legacy self-contained decode. Both paths emit byte-identical bodies.
	if doc != nil {
		return doc.WithModel(rt.Model)
	}
	swapped, _, err := SwapModel(body, rt.Model)
	return swapped, err
}

// universalCodecOptions builds the strict codec options for a universal cell.
// When the UPSTREAM speaks Messages, the account's explicit usage convention
// is required: the upstream response's own input_tokens meaning is the
// operator-declared fact (schema v13), never a guess. The router already
// refuses the cell without one; a hand-built route gets the same refusal here.
func universalCodecOptions(rt Route) (translate.CodecOptions, error) {
	opts := translate.CodecOptions{}
	if rt.EffectiveWire() == provider.SurfaceAnthropic {
		conv, err := translate.UsageConventionOf(rt.Account.UsageConvention)
		if err != nil {
			return opts, err
		}
		opts.MessagesUsage = conv
	}
	return opts, nil
}

// streamOutcomeFrom maps a translated reader's terminal state onto the
// delivery outcome hop-health reads. An undrained reader (the client went
// away mid-stream) reports unknowable rather than failed: a client
// disconnecting says nothing about the hop.
func streamOutcomeFrom(sr *translate.StreamReader) string {
	if sr == nil {
		return ""
	}
	if sr.Truncated() {
		return StreamOutcomeTruncated
	}
	switch sr.Outcome() {
	case translate.FinishFailed:
		return StreamOutcomeFailed
	case "":
		return ""
	default:
		return StreamOutcomeCompleted
	}
}

// attempt runs the upstream call and returns the response WITHOUT touching w.
//
// Split out of Forward so failover can inspect the status before anything is
// committed to the client. This is the whole trick: w.Header() is only a map
// until WriteHeader fires, and WriteHeader is what makes the status
// irrevocable. Keeping the two phases apart is what lets us try hop 2 after
// hop 1 answers 429 — see ForwardCandidates.
func (d *Dispatcher) attempt(ctx context.Context, r *http.Request, rt Route, body []byte, doc *jsonbody.BodyDoc) (*http.Response, Result, time.Time, error) {
	ad, err := d.registry.Get(rt.Account.Kind)
	if err != nil {
		return nil, Result{}, time.Time{}, err
	}
	// A translated hop dials the upstream on ITS wire while everything
	// client-facing (ledger, usage tap) keeps the client surface. One
	// selector: the same value prepareHopBody encodes for and commit unwraps.
	dialSurface := rt.EffectiveWire()
	url := provider.UpstreamURL(rt.Account.BaseURL, rt.Account.Kind, provider.SurfacePath(dialSurface))
	// Google AI Studio native audio (TTS/STT): generateContent, not OpenAI audio.
	if isGeminiNativeAudio(rt, rt.Surface) {
		url = geminiAudioURL(rt)
	}
	// Cloudflare Workers AI implements neither the OpenAI /images/generations
	// path nor the audio/* paths: image and TTS/STT models are served at the
	// native /ai/run/<model> endpoint.
	switch dialSurface {
	case provider.SurfaceImagesGen, provider.SurfaceAudioTTS, provider.SurfaceAudioSTT:
		if isCloudflare(rt) {
			base := strings.TrimSuffix(strings.TrimSuffix(rt.Account.BaseURL, "/"), "/v1")
			url = base + "/run/" + rt.Model
		}
	}

	upReq, err := http.NewRequestWithContext(ctx, r.Method, url, bytes.NewReader(body))
	if err != nil {
		return nil, Result{}, time.Time{}, err
	}
	// Copy safe client headers, then let the adapter apply auth + strip its own.
	copyClientHeaders(upReq, r, ad.StripHeaders())
	if err := ad.PrepareRequest(ctx, upReq, rt.Account, rt.KeyPlain, dialSurface); err != nil {
		return nil, Result{}, time.Time{}, err
	}
	// Google AI Studio native audio uses x-goog-api-key, not Bearer.
	if isGeminiNativeAudio(rt, rt.Surface) {
		upReq.Header.Set("x-goog-api-key", rt.KeyPlain)
		upReq.Header.Del("Authorization")
	}
	// Content-Length must match the rewritten body.
	upReq.ContentLength = int64(len(body))
	// Preserve multipart Content-Type with boundary; default to JSON.
	contentType := "application/json"
	if isMultipartBody(body) {
		if boundary := extractMultipartBoundary(body); boundary != "" {
			contentType = "multipart/form-data; boundary=" + boundary
		}
	}
	upReq.Header.Set("Content-Type", contentType)

	// Single-parse dispatch: the client's streaming flag was decoded once per
	// REQUEST (jsonbody), and every hop body — swapped or translated —
	// preserves it, so the request-level flag answers for every attempt
	// without re-deriving it per hop. A nil doc (legacy callers) falls back to
	// the old per-attempt parse.
	var stream bool
	if doc != nil {
		stream = doc.Stream()
	} else {
		stream = IsStream(body)
	}

	start := time.Now()
	resp, err := d.client.Do(upReq)
	if err != nil {
		return nil, Result{Stream: stream, Totalms: msPtr(time.Since(start)), Err: err.Error()}, time.Time{}, err
	}
	if isGeminiNativeAudio(rt, rt.Surface) {
		if err := geminiAudioResponse(resp, dialSurface); err != nil {
			return nil, Result{Stream: stream, Totalms: msPtr(time.Since(start)), Err: err.Error()}, time.Time{}, err
		}
	}

	res := Result{
		Status:        resp.StatusCode,
		Stream:        stream,
		EndpointID:    resp.Header.Get("x-opencode-endpoint-id"),
		UpstreamModel: resp.Header.Get("x-opencode-upstream-model-id"),
	}
	return resp, res, start, nil
}

// gatewayFault answers the client with a 502 of ezllm's own making (body
// already shaped for the client) and records why. It commits the status, so
// nothing else may be written for this request afterwards.
func gatewayFault(w http.ResponseWriter, res *Result, start time.Time, errNote string, body []byte) {
	res.Status = http.StatusBadGateway
	res.Err = errNote
	w.Header().Set("Content-Type", "application/json")
	res.Committed = true
	w.WriteHeader(http.StatusBadGateway)
	_, _ = w.Write(body)
	res.Totalms = msPtr(time.Since(start))
}

// crossWire is how one translated hop re-encodes its upstream's answer for the
// client. Every cell shares the same codecs (translate.NewStreamReader,
// ResponseUniversal); the cells differ only in codec policy and in how an
// upstream ERROR body and a translation failure are shaped, which this keeps
// side by side so a change to one cell is visibly a change to that cell.
type crossWire struct {
	wire      provider.Surface
	opts      translate.CodecOptions
	optsErr   error                   // universal cell without a usable account fact
	reshape   func(raw []byte) []byte // upstream error body → client shape; nil = pass through
	errPrefix string                  // ledger err prefix for a failed translation
	failBody  func(raw []byte) []byte // body of the 502 for a failed translation
}

// crossWireFor returns the hop's translation policy, or false for a native hop.
func crossWireFor(rt Route) (crossWire, bool) {
	wire := rt.EffectiveWire()
	if wire == rt.Surface {
		return crossWire{}, false
	}
	if translate.IsUniversalCell(rt.Surface, wire) {
		// The three new cells: strict codecs, errors re-shaped into the
		// client's family, failures in the client's own error shape.
		opts, err := universalCodecOptions(rt)
		return crossWire{
			wire: wire, opts: opts, optsErr: err,
			reshape:   func(raw []byte) []byte { return translate.ReorientError(raw, rt.Surface) },
			errPrefix: "translate response: ",
			failBody: func([]byte) []byte {
				return translate.EncodeError(translate.ErrorIR{Type: "api_error", Message: "upstream response could not be translated"}, rt.Surface)
			},
		}, true
	}
	legacy := translate.CodecOptions{Legacy: true}
	if wire == provider.SurfaceOpenAI {
		// Legacy anthropic client on the chat wire: errors and failures are
		// both rendered as an anthropic error envelope.
		return crossWire{
			wire: wire, opts: legacy,
			reshape:   translate.Error,
			errPrefix: "translate response: ",
			failBody:  translate.Error,
		}, true
	}
	// Legacy responses-wire hop (chat or anthropic client). Error bodies pass
	// through byte-identical: the upstream's own words reach the client and
	// the ledger err column untouched.
	return crossWire{
		wire: wire, opts: legacy,
		errPrefix: "translate responses body: ",
		failBody: func([]byte) []byte {
			return []byte(`{"error":{"type":"upstream_error","message":"responses body could not be translated","code":502}}`)
		},
	}, true
}

// commit streams one upstream response to the client and closes resp.Body.
// Nothing here may be called twice for one request — after the first
// WriteHeader the status can no longer change.
func (d *Dispatcher) commit(ctx context.Context, w http.ResponseWriter, resp *http.Response, rt Route, res Result, start time.Time) (Result, error) {
	defer resp.Body.Close()

	// Propagate response headers that matter, minus hop-by-hop and identity.
	copyResponseHeaders(w, resp)
	cw, cross := crossWireFor(rt)

	isSSE := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
	if res.Stream && isSSE {
		// A translated stream wraps the body BEFORE any byte is copied: the
		// client sees its own event shape synthesized from the upstream's, and
		// streamCopy's usage tap then reads client-shaped events — which
		// matches rt.Surface exactly. Close still reaches the ORIGINAL body (a
		// NopCloser would leak the upstream connection).
		//
		// translated carries the reader whose terminal state becomes the
		// delivery outcome; nil for a native stream.
		var translated *translate.StreamReader
		if cross {
			if cw.optsErr != nil {
				// A cell that cannot be converted must not stream
				// foreign-shaped bytes to the client.
				gatewayFault(w, &res, start, "translate stream: "+cw.optsErr.Error(),
					translate.EncodeError(translate.ErrorIR{Type: "api_error", Message: cw.optsErr.Error()}, rt.Surface))
				return res, nil
			}
			translated = translate.NewStreamReader(resp.Body, cw.wire, rt.Surface, rt.Alias, cw.opts)
			resp.Body = struct {
				io.Reader
				io.Closer
			}{translated, resp.Body}
		}
		// streamCopy writes the header before its first byte, so the status
		// is committed the moment it returns — regardless of a copy error.
		res.Committed = true
		ttft, usage, copyErr, first := d.streamCopy(ctx, w, resp, rt.Surface, start)
		res.TTFTms = msAtLeastOne(ttft)
		if usage != nil {
			res.Usage = usage
		}
		if copyErr != nil {
			// The client has already received a 200 + partial stream, so we
			// cannot change the status. Record the truncation instead: a
			// half-delivered stream must never look like a clean success in
			// the ledger.
			res.Err = "stream truncated: " + copyErr.Error()
		}
		// Mirror the non-stream branch's err-column rule. The stream branch
		// had no equivalent, so a failover-committed SSE error response — a
		// 429 whose reason rides in the first data payload — landed in the
		// ledger with a blank err, the one field that diagnoses an incident
		// after the fact. Some providers also answer 200 SSE carrying an
		// error object inside the stream, which is equally worth recording.
		if res.Err == "" && (res.Status >= 400 || sseHasErrorEnvelope(first)) {
			res.Err = upstreamErrorSnippet(res.Status, first)
		}
		// The delivery outcome: a late error frame or a source that ended
		// without a terminal event must not reach hop-health as success, even
		// though the HTTP status said 200 and the client got bytes. Legacy
		// clients still receive the historic synthesized close; the ledger
		// and MarkHopGood read this field instead.
		if translated != nil {
			res.StreamOutcome = streamOutcomeFrom(translated)
			if res.Err == "" && !res.StreamCompleted() {
				// The client already received a 200 and bytes, so the status
				// cannot change; the ledger records the outcome instead.
				res.Err = "translated stream " + res.StreamOutcome
			}
		}
	} else {
		// Non-streamed: read the body once, tap usage, then write it out.
		// The extra byte detects a body that outgrew maxBodyBytes: a
		// LimitReader alone stops silently at the cap, so a truncated JSON
		// payload would be written with a 200 and a Content-Length that no
		// longer matches the body. Reading one byte MORE than the cap turns
		// that into an explicit 502.
		lr := io.LimitReader(resp.Body, maxBodyBytes+1)
		raw, err := io.ReadAll(lr)
		if err != nil {
			res.Err = err.Error()
			return res, err
		}
		if int64(len(raw)) > maxBodyBytes {
			// The upstream kept sending past 32 MiB. Writing the truncated
			// body would hand the client corrupt JSON, so refuse instead —
			// and never raise the cap: this body is buffered in memory.
			gatewayFault(w, &res, start, fmt.Sprintf("upstream response exceeded %d bytes", maxBodyBytes),
				[]byte(`{"error":{"type":"upstream_response_truncated","message":"upstream response exceeded 32 MiB","code":502}}`))
			return res, nil
		}
		// A 200 with a completely empty body is not a valid provider response
		// for any of our three surfaces (all return JSON). It means the
		// upstream died after headers were written. Record it so a truncated
		// call never looks like a clean success in the ledger, and tell the
		// client with a 502 rather than an empty 200.
		if resp.StatusCode == http.StatusOK && len(bytes.TrimSpace(raw)) == 0 {
			gatewayFault(w, &res, start, "upstream returned an empty body",
				[]byte(`{"error":{"type":"upstream_error","message":"upstream returned an empty body","code":502}}`))
			return res, nil
		}
		// A translated hop re-encodes every byte the client sees into its own
		// shape. The err-column snippet is captured from the upstream's OWN
		// words before any re-shaping, and usage is tapped on the translated
		// body — the tap is surface-aware and rt.Surface is the client's.
		if cross {
			if res.Status >= 400 {
				if cw.reshape != nil {
					if res.Err == "" {
						res.Err = upstreamErrorSnippet(res.Status, raw)
					}
					raw = cw.reshape(raw)
				}
			} else {
				terr := cw.optsErr
				if terr == nil {
					var translated []byte
					if translated, terr = translate.ResponseUniversal(raw, cw.wire, rt.Surface, rt.Alias, cw.opts); terr == nil {
						raw = translated
					}
				}
				if terr != nil {
					// A body we cannot translate is a gateway fault, and the
					// client must never receive the upstream's foreign shape.
					gatewayFault(w, &res, start, cw.errPrefix+terr.Error(), cw.failBody(raw))
					return res, nil
				}
			}
		}
		if u := tapJSONUsage(raw, rt.Surface); u != nil {
			res.Usage = u
		}
		// Record WHY an error status happened. Before this, the err column was
		// populated only for transport and stream failures, so a provider 429
		// left a fully blank row — status 429, err "". The upstream's own
		// sentence ("Maximum 5 requests within 1 minutes") is the single most
		// useful field for diagnosing an incident after the fact, and without
		// this it is streamed to the client and then gone forever.
		if res.Status >= 400 && res.Err == "" {
			res.Err = upstreamErrorSnippet(res.Status, raw)
		}
		res.Committed = true
		w.WriteHeader(resp.StatusCode)
		if _, werr := w.Write(raw); werr != nil {
			res.Err = "client write: " + werr.Error()
		}
	}
	res.Totalms = msPtr(time.Since(start))
	return res, nil
}

// Forward sends the (already model-swapped) body upstream and copies the
// response to w. It returns the ledger Result. Single-candidate form: nothing
// to fail over to, so attempt and commit happen back to back.
func (d *Dispatcher) Forward(ctx context.Context, w http.ResponseWriter, r *http.Request, rt Route, body []byte) (Result, error) {
	led := &attemptLedger{limit: d.attemptBudget}
	var sel time.Time
	if d.dispatchDeadline > 0 {
		sel = time.Now().Add(d.dispatchDeadline)
	}
	resp, res, start, err := d.retryAttempt(ctx, r, rt, body, nil, led, sel)
	res.UpstreamAttempts, res.AttemptBudget = led.used, d.attemptBudget
	if err != nil {
		return res, err
	}
	return d.commit(ctx, w, resp, rt, res, start)
}

func (d *Dispatcher) retryAttempt(ctx context.Context, r *http.Request, rt Route, body []byte, doc *jsonbody.BodyDoc, led *attemptLedger, sel time.Time) (*http.Response, Result, time.Time, error) {
	maxAttempts := d.retryAttempts + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	var lastResp *http.Response
	var lastResult Result
	var lastStart time.Time
	for attempt := 0; attempt < maxAttempts; attempt++ {
		// The request-wide budget binds across hops: a hop whose turn comes
		// after the budget is spent is never dialed at all.
		if led != nil && (led.exhausted() || pastDeadline(sel)) {
			return lastResp, lastResult, lastStart, errAttemptBudgetSpent
		}
		if led != nil {
			led.consume()
		}
		resp, result, start, err := d.attempt(ctx, r, rt, body, doc)
		lastResp, lastResult, lastStart = resp, result, start
		// A non-retryable answer is FINAL, whatever its status — including 429,
		// which is a failover signal rather than a worth-another-try one. Every
		// 2xx and every non-retryable 4xx leaves through here, so the retry
		// decision lives in exactly one predicate.
		if err == nil && !retryableUpstreamStatus(result.Status) {
			return resp, result, start, nil
		}
		if attempt+1 == maxAttempts || ctx.Err() != nil || (err != nil && !retryableUpstreamError(err)) {
			return resp, result, start, err
		}
		if resp != nil {
			drainClose(resp)
		}
		delay := time.Duration(1<<attempt) * time.Second
		if delay > 8*time.Second {
			delay = 8 * time.Second
		}
		// Never sleep toward a failover the selection deadline forbids.
		if !sel.IsZero() {
			if left := time.Until(sel); left < delay {
				delay = left
			}
		}
		if delay <= 0 {
			return lastResp, lastResult, lastStart, errAttemptBudgetSpent
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, lastResult, lastStart, ctx.Err()
		case <-timer.C:
		}
	}
	// Provably unreachable: the loop's final iteration always satisfies
	// attempt+1 == maxAttempts and returns above. Go cannot see that
	// invariant, so a terminating statement is required — keep it in sync with
	// the guard, since a change there would make this the live fall-out path.
	return lastResp, lastResult, lastStart, errors.New("proxy: retry budget exhausted")
}

// retryableUpstreamStatus reports whether an upstream answer is worth retrying
// on the SAME hop.
//
// 429 is deliberately absent. A rate limit is not a transient glitch: the
// upstream has just told us this credential has no budget left in the current
// window, so an immediate retry spends MORE of the very budget being reported
// and guarantees the next answer is another 429. Retrying a rate-limited key is
// what turns "one call over budget" into "the whole minute gone".
//
// 429 is still a FAILOVER signal — see failoverStatus, which includes it, so a
// different hop with budget can serve the request. Retry and failover are
// different questions and must not share a predicate.
func retryableUpstreamStatus(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusTooEarly || code >= 500
}

func retryableUpstreamError(err error) bool {
	var netErr net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr)
}

// failoverStatus reports whether an upstream answer means "that hop couldn't
// serve it, another one might" rather than "the request itself is wrong".
//
// 400/413/422 are deliberately excluded: they describe the BODY, which we are
// about to replay byte-identical to the next hop — retrying would burn every
// key in the combo to get the same rejection. 401/403/404/429 and 5xx are
// hop-specific (bad key, model absent on that account, quota, outage) and are
// exactly what failover exists to survive.
func failoverStatus(code int) bool {
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return code >= 500
}

// drainClose releases an abandoned response so its connection can be reused.
func drainClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
	_ = resp.Body.Close()
}

// ForwardCandidates tries routes in order and commits the first one that the
// client may see. This is combo failover.
//
// The invariant that makes it safe: attempt() never touches w, so until
// commit() calls WriteHeader the status is still changeable and no byte has
// reached the client. A hop abandoned here is therefore invisible — the client
// sees only the hop that ultimately commits. Once commit runs we stop, because
// a partially streamed response cannot be handed to a second upstream.
//
// Failover-able responses stay buffered until a later candidate succeeds; if
// every eligible route fails, the last real upstream error is committed. Routes
// still cooling down are never called merely to manufacture a final response.
//
// doc is the optional single-parse decode of body (docs/single-parse-dispatch-
// design.md): when non-nil, every hop's model swap reuses that one decode.
// Legacy callers pass nil and get the unchanged per-hop decode path.
func (d *Dispatcher) ForwardCandidates(ctx context.Context, w http.ResponseWriter, r *http.Request, routes []Route, body []byte) (Route, Result, error) {
	return d.ForwardCandidatesDoc(ctx, w, r, routes, body, nil)
}

// ForwardCandidatesDoc is ForwardCandidates with the request body's shared
// decode threaded through. See ForwardCandidates for the failover contract.
func (d *Dispatcher) ForwardCandidatesDoc(ctx context.Context, w http.ResponseWriter, r *http.Request, routes []Route, body []byte, doc *jsonbody.BodyDoc) (Route, Result, error) {
	if len(routes) == 0 {
		return Route{}, Result{}, errors.New("proxy: no candidate routes")
	}
	// HOP COMPATIBILITY GROUPS (addendum #2): the combo/strategy layer
	// declares three kinds of relationship between hops, each handled by a
	// different skip/failover rule:
	//
	//   REPLICA: same model, different accounts — interchangeable. Failover
	//   picks the next replica; a 429 on one never retried on the same.
	//
	//   CROSS-MODEL FALLBACK: different model — the combo declares the
	//   preference order. A 429 falls through to the next model.
	//
	//   WIRE TRANSLATION: WireSurface != "" — same logical model, different
	//   wire protocol (e.g. OpenAI → Anthropic). The combo declares which
	//   surface the upstream speaks; the Route carries the translation.
	//
	// CAPABILITY FILTERING (addendum #6): each hop is checked, in order,
	// for: (1) cooldown, (2) context-size fitness, (3) rate budget, (4)
	// attempt budget, then dispatched. A hop that fails any check is
	// skipped with an honest note — the attempt trail carries it to the
	// served row's ledger so the operator sees the full story.
	//
	// NEVER TRUNCATE: this dispatcher never truncates a request's history
	// to fit a hop's context window. If the body is too large, the hop is
	// skipped — the combo fails over to one that fits, or returns 503.
	// Truncation would silently corrupt the conversation; a skip is honest.
	// Try candidates in the resolver's strategy order, skipping cooled endpoints.
	var fallbackRoute *Route
	var fallbackResp *http.Response
	var fallbackResult Result
	var fallbackStart time.Time
	var fallbackTransportErr error
	var rateRefusal *RateLimited
	// The failover trail: every candidate abandoned before the one that
	// served, in dispatch order. Attached to whatever Result goes back so the
	// ledger row can show the hops that "hopped".
	var attempts []Attempt
	// Addendum #3: ONE request-wide budget across same-hop retries AND
	// failover hops, and a selection deadline that governs only the choosing
	// (never the winning attempt's stream).
	led := &attemptLedger{limit: d.attemptBudget}
	var sel time.Time
	if d.dispatchDeadline > 0 {
		sel = time.Now().Add(d.dispatchDeadline)
	}
	budgetNote := func(ns, model string) Attempt {
		return Attempt{
			Namespace: ns, Model: model,
			Note: fmt.Sprintf("request attempt budget spent (%s/%d used)", budgetLabel(d.attemptBudget), led.used),
		}
	}
	for i := 0; i < len(routes); i++ {
		// The budget is checked BEFORE any eligibility work: an unaffordable
		// hop is never dialed, never probed, never cooled.
		if led.exhausted() || pastDeadline(sel) {
			attempts = append(attempts, budgetNote(routes[i].Account.Namespace, routes[i].Model))
			for j := i + 1; j < len(routes); j++ {
				attempts = append(attempts, budgetNote(routes[j].Account.Namespace, routes[j].Model))
			}
			break
		}
		rt := routes[i]
		// Context-size filter (addendum #6): a request whose estimated token
		// count exceeds the hop's declared context window is filtered BEFORE
		// dispatch — no paid upstream call, no wasted round-trip. The estimate
		// is len(body)/4, a standard conservative heuristic that overestimates
		// slightly, which is exactly what we want for filtering. 0 = unknown
		// or unlimited — never filtered.
		if rt.Account.MaxContextTokens > 0 {
			reqEst := int64(len(body)) / 4
			if reqEst > rt.Account.MaxContextTokens {
				d.log.Info("hop skipped: context too small",
					"account", rt.Account.Namespace, "model", rt.Model,
					"estimated_tokens", reqEst, "max_context", rt.Account.MaxContextTokens)
				attempts = append(attempts, Attempt{
					Namespace: rt.Account.Namespace, Model: rt.Model,
					Note: fmt.Sprintf("context too small: estimated %d > max %d", reqEst, rt.Account.MaxContextTokens),
				})
				continue
			}
		}
		allowed, probe := d.acquireHop(rt, time.Now())
		if !allowed {
			// Failover is invisible to the CLIENT on purpose; it must not be
			// invisible to the OPERATOR. A skipped hop writes no ledger row (the
			// request never leaves the box), so this log line is the only place
			// the reason exists — and the attempt trail carries it to the
			// served row's ledger err.
			why := d.skipReason(rt, time.Now())
			d.log.Info("hop skipped",
				"account", rt.Account.Namespace, "model", rt.Model,
				"alias", rt.Alias, "why", why)
			attempts = append(attempts, Attempt{
				Namespace: rt.Account.Namespace, Model: rt.Model, Note: why,
			})
			continue
		}
		// Account-wide request budget, reserved HERE rather than at resolution:
		// the router visits every hop to build this list, so reserving there
		// would charge requests to hops that are never dispatched.
		//
		// Out of budget behaves exactly like cooling — the hop is skipped and
		// the combo fails over. That is what makes a rate limit survivable
		// instead of fatal: the provider's ceiling stops us wasting a request
		// learning what we already know, while a hop with budget still serves.
		if ok, next := d.reserveRate(rt); !ok {
			if probe {
				d.releaseProbe(rt)
			}
			attempts = append(attempts, Attempt{
				Namespace: rt.Account.Namespace, Model: rt.Model,
				Note: fmt.Sprintf("account budget %d per %s exhausted", rt.Account.RateLimit, rt.Account.RateWindow),
			})
			// Keep the EARLIEST release time across hops: that is the first
			// moment the request could succeed anywhere, which is what the
			// client needs to wait for.
			if rateRefusal == nil || next.Before(rateRefusal.NextAllowed) {
				rateRefusal = &RateLimited{
					Account:     rt.Account.Namespace,
					Limit:       rt.Account.RateLimit,
					Window:      rt.Account.RateWindow,
					NextAllowed: next,
				}
			}
			continue
		}
		// If all later candidates are cooling down, hold this eligible route as
		// the final response fallback. This preserves an actual upstream error
		// without making an extra request that circumvents cooldown.
		// A sole eligible endpoint is attempted normally below.
		swapped, err := prepareHopBody(body, rt, doc)
		if err != nil {
			if probe {
				d.releaseProbe(rt)
			}
			return Route{}, Result{}, err
		}
		resp, res, start, err := d.retryAttempt(ctx, r, rt, swapped, doc, led, sel)
		res.UpstreamAttempts, res.AttemptBudget = led.used, d.attemptBudget
		// Park the hop only when there is somewhere else to go. len(routes) is
		// the full candidate list the router resolved; a single entry means a
		// cooled hop has no alternate, and classifyHop declines to park a rate
		// limit in that case.
		var hdr http.Header
		if resp != nil {
			hdr = resp.Header
		}
		// A budget/deadline refusal says nothing about the hop by itself: when
		// no attempt on this hop got an answer, record nothing (neither a
		// strike nor a clear). When an earlier attempt did answer (a 5xx that
		// was being retried), that answer is the hop's outcome.
		var oc hopOutcome
		switch {
		case errors.Is(err, errAttemptBudgetSpent) && res.Status == 0:
		case errors.Is(err, errAttemptBudgetSpent):
			oc = d.classifyHop(nil, res.Status, hdr, len(routes) > 1)
			d.recordHop(rt, oc, time.Now())
		default:
			oc = d.classifyHop(err, res.Status, hdr, len(routes) > 1)
			d.recordHop(rt, oc, time.Now())
		}
		if probe {
			d.releaseProbe(rt)
		}
		if err != nil {
			// The hop's outcome was recorded above; a second record here once
			// cleared the park a transport error had just set.
			note := "transport: " + err.Error()
			if errors.Is(err, errAttemptBudgetSpent) {
				note = err.Error()
				d.log.Info("hop not dialed", // Info, not Warn: nothing failed
					"account", rt.Account.Namespace, "model", rt.Model,
					"alias", rt.Alias, "why", err.Error())
			} else {
				d.log.Warn("hop failed, failing over",
					"account", rt.Account.Namespace, "model", rt.Model,
					"alias", rt.Alias, "err", err.Error())
			}
			attempts = append(attempts, Attempt{
				Namespace: rt.Account.Namespace, Model: rt.Model,
				Note: note,
			})
			if fallbackResp != nil {
				drainClose(fallbackResp)
				fallbackResp = nil
			}
			fallbackTransportErr = err
			continue
		}
		if failoverStatus(res.Status) || oc.failed {
			// The upstream's own words are the whole diagnosis, and for a failed
			// hop they reach nobody: no ledger row is written (this hop will not
			// commit) and the response goes to the client only if every later hop
			// also fails. So read a bounded prefix of the error body here and
			// restore it afterwards — the response is still committable as the
			// final fallback, and it must stay byte-identical.
			words := peekUpstreamError(resp)
			d.log.Warn("hop failed, failing over",
				"account", rt.Account.Namespace, "model", rt.Model,
				"alias", rt.Alias, "status", res.Status,
				"reason", oc.reason, "upstream", words)
			attempts = append(attempts, Attempt{
				Namespace: rt.Account.Namespace, Model: rt.Model, Status: res.Status,
				// peekUpstreamError already renders the ledger's err format
				// (`upstream 429 [cause]: words`) — use it verbatim, never
				// re-wrap it (that double-printed the prefix once).
				Note: words,
			})
		}
		if failoverStatus(res.Status) {
			if fallbackResp != nil {
				drainClose(fallbackResp)
			}
			rtCopy, resCopy, startCopy := rt, res, start
			fallbackRoute, fallbackResp, fallbackResult, fallbackStart = &rtCopy, resp, resCopy, startCopy
			continue
		}
		if fallbackResp != nil {
			drainClose(fallbackResp)
		}
		res2, err2 := d.commit(ctx, w, resp, rt, res, start)
		// The committed route travels back with the result: the ledger must
		// credit the hop that ACTUALLY served the call. Reporting routes[0]
		// would attribute a failed hop's spend — and, worse, would make a
		// per-account cap meter the wrong budget entirely.
		res2.Attempts = attempts
		return rt, res2, err2
	}

	// Preserve the final upstream error response for compatibility, but never
	// issue another request to an endpoint that's cooling down. A stored response
	// is safe to commit: it was obtained by a prior, permitted attempt.
	if fallbackResp != nil {
		d.log.Warn("every hop failed; committing the last upstream error",
			"status", fallbackResult.Status,
			"account", fallbackRoute.Account.Namespace, "model", fallbackRoute.Model,
			"alias", fallbackRoute.Alias)
		result, err := d.commit(ctx, w, fallbackResp, *fallbackRoute, fallbackResult, fallbackStart)
		result.Attempts = attempts
		return *fallbackRoute, result, err
	}
	if fallbackTransportErr != nil {
		return Route{}, Result{Attempts: attempts, UpstreamAttempts: led.used, AttemptBudget: d.attemptBudget}, fallbackTransportErr
	}
	// Nothing was attempted at all because every hop was out of request budget.
	// Report the refusal with the earliest release time so the client can wait
	// instead of blind-retrying — and note that no upstream request was wasted
	// discovering this.
	if rateRefusal != nil {
		return Route{}, Result{Attempts: attempts, UpstreamAttempts: led.used, AttemptBudget: d.attemptBudget}, rateRefusal
	}
	return Route{}, Result{Attempts: attempts, UpstreamAttempts: led.used, AttemptBudget: d.attemptBudget}, errors.New("proxy: all combo endpoints are in five-minute cooldown")
}

// streamCopy copies SSE upstream->client with a Flush per chunk, observing
// usage events without altering a single byte. Returns (ttft, usage, err) where
// err is non-nil when the stream was truncated — the caller records it, since
// the 200 status has already been sent and cannot be changed.
//
// `start` is the DISPATCH time (when the upstream request was posted), not the
// moment streaming began. TTFT means "time to first token": the provider's
// prefill happens before it emits any byte, so measuring from inside this
// function — after headers already arrived — reports a meaningless 1-3ms and
// hides exactly the cache/prefill latency the number exists to reveal.
//
// The fourth return is the first non-terminal SSE data payload. A streamed
// error response carries its reason in there, and the ledger's err column is
// blank for streams unless the caller is handed it.
func (d *Dispatcher) streamCopy(ctx context.Context, w http.ResponseWriter, resp *http.Response, surface provider.Surface, start time.Time) (time.Duration, *store.Usage, error, []byte) {
	var ttft time.Duration
	var firstByte bool
	var firstPayload []byte

	flusher, _ := w.(http.Flusher)
	// Headers already copied; write status now so the client starts receiving.
	w.WriteHeader(resp.StatusCode)
	if flusher != nil {
		flusher.Flush()
	}

	br := bufio.NewReaderSize(resp.Body, 64*1024)
	sc := bufio.NewScanner(br)
	sc.Buffer(make([]byte, 0, 64*1024), maxScanToken)

	// We must forward EXACT bytes. Scanner strips the trailing newline, so we
	// re-emit "\n" per line — SSE lines are newline-terminated, which makes it
	// lossless for well-formed streams. Anything unusual (bare \r) is handled
	// by falling back to a raw copy if the scanner errors.
	// Read-only usage tap. Merges across events: Anthropic's message_start
	// carries the input/cache breakdown while message_delta carries the final
	// output totals, so taking only the first hit would record out=0.
	tapper := NewUsageTapper(surface)
	for sc.Scan() {
		line := sc.Bytes()
		if !firstByte {
			ttft = time.Since(start)
			firstByte = true
		}
		// Remember the first real payload so the caller can quote it in the
		// ledger. Comments (: keepalive) and the [DONE] sentinel are skipped:
		// neither can carry an error envelope.
		if firstPayload == nil {
			if p := bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("data:"))); len(p) > 0 && !bytes.Equal(p, []byte("[DONE]")) {
				firstPayload = append([]byte(nil), p...)
			}
		}
		if _, err := w.Write(line); err != nil {
			return ttft, tapper.Usage(), fmt.Errorf("client write: %w", err), firstPayload
		}
		if _, err := w.Write([]byte("\n")); err != nil {
			return ttft, tapper.Usage(), fmt.Errorf("client write: %w", err), firstPayload
		}
		if flusher != nil {
			flusher.Flush()
		}
		tapper.Observe(line)
	}
	if err := sc.Err(); err != nil {
		// Scanner hit an over-long line or a read error: drain the rest raw so
		// the client still gets a complete (if untapped) stream.
		if _, cerr := io.Copy(w, br); cerr != nil {
			if flusher != nil {
				flusher.Flush()
			}
			if !firstByte {
				ttft = time.Since(start)
			}
			if !errors.Is(err, bufio.ErrTooLong) {
				writeStreamErrorFrame(w, surface, "upstream connection lost")
				if flusher != nil {
					flusher.Flush()
				}
			}
			return ttft, tapper.Usage(), fmt.Errorf("upstream read: %w (scanner: %v)", cerr, err), firstPayload
		}
		if flusher != nil {
			flusher.Flush()
		}
		if !firstByte {
			ttft = time.Since(start)
		}
		// An over-long line is survivable (bytes still forwarded); a genuine
		// upstream read error means the stream was truncated.
		if errors.Is(err, bufio.ErrTooLong) {
			return ttft, tapper.Usage(), nil, firstPayload
		}
		// The upstream died mid-stream. The 200 is already committed, so the
		// only honest signal left is a terminal SSE error frame in the
		// client's dialect — otherwise the client is left with a silent EOF.
		writeStreamErrorFrame(w, surface, "upstream connection lost")
		if flusher != nil {
			flusher.Flush()
		}
		return ttft, tapper.Usage(), fmt.Errorf("upstream read: %w", err), firstPayload
	}
	if !firstByte {
		ttft = time.Since(start)
	}
	return ttft, tapper.Usage(), nil, firstPayload
}

// writeStreamErrorFrame emits a terminal SSE error event in the client's
// surface dialect. Called only after a committed 200 when the upstream dies
// mid-stream, so the client gets an explicit error instead of a silent EOF.
func writeStreamErrorFrame(w io.Writer, surface provider.Surface, msg string) {
	m, _ := json.Marshal(msg)
	switch surface {
	case provider.SurfaceAnthropic:
		fmt.Fprintf(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":%s}}\n\n", m)
	case provider.SurfaceResponses:
		fmt.Fprintf(w, "event: error\ndata: {\"type\":\"error\",\"code\":\"server_error\",\"message\":%s}\n\n", m)
	default: // openai
		fmt.Fprintf(w, "data: {\"error\":{\"type\":\"server_error\",\"message\":%s}}\n\n", m)
	}
}

// tapSSELine extracts usage from one SSE data line for the given surface.
//
//	openai     : final chunk carries "usage"
//	anthropic  : message_start + message_delta both carry usage (delta has the
//	             authoritative output_tokens), so we merge
//	responses  : response.completed carries the full response incl. usage
func tapSSELine(line []byte, surface provider.Surface) *store.Usage {
	payload := bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("data:")))
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return nil
	}
	var ev map[string]any
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil
	}
	switch surface {
	case provider.SurfaceAnthropic:
		typ, _ := ev["type"].(string)
		switch typ {
		case "message_start":
			if msg, ok := ev["message"].(map[string]any); ok {
				if u, ok := msg["usage"].(map[string]any); ok {
					n := store.NormalizeUsage(store.SurfaceAnthropic, u)
					return &n
				}
			}
		case "message_delta":
			if u, ok := ev["usage"].(map[string]any); ok {
				n := store.NormalizeUsage(store.SurfaceAnthropic, u)
				return &n
			}
		}
	case provider.SurfaceResponses:
		typ, _ := ev["type"].(string)
		// Usage rides on the TERMINAL event — which is response.completed on
		// success but response.incomplete when max_output_tokens truncated the
		// response (observed live). Also accept response.failed rather than
		// silently recording zero usage for a truncated stream.
		switch typ {
		case "response.completed", "response.incomplete", "response.failed":
			if r, ok := ev["response"].(map[string]any); ok {
				if u, ok := r["usage"].(map[string]any); ok {
					n := store.NormalizeUsage(store.SurfaceResponses, u)
					return &n
				}
			}
		}
		if u, ok := ev["usage"].(map[string]any); ok {
			n := store.NormalizeUsage(store.SurfaceResponses, u)
			return &n
		}
	default: // openai
		if u, ok := ev["usage"].(map[string]any); ok {
			n := store.NormalizeUsage(store.SurfaceOpenAI, u)
			return &n
		}
	}
	return nil
}

// tapJSONUsage extracts usage from a complete (non-streamed) JSON body.
func tapJSONUsage(body []byte, surface provider.Surface) *store.Usage {
	var ev map[string]any
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil
	}
	if u, ok := ev["usage"].(map[string]any); ok {
		n := store.NormalizeUsage(store2surface(surface), u)
		return &n
	}
	return nil
}

// MergeUsage combines an earlier (message_start) and later (message_delta)
// Anthropic usage object: the delta carries authoritative output totals, the
// start carries the input/cache breakdown.
func MergeUsage(base, delta *store.Usage) *store.Usage {
	if base == nil {
		return delta
	}
	if delta == nil {
		return base
	}
	out := *base
	if delta.Out > 0 {
		out.Out = delta.Out
	}
	if delta.CachedRead > 0 {
		out.CachedRead = delta.CachedRead
	}
	if delta.CachedWrite > 0 {
		out.CachedWrite = delta.CachedWrite
	}
	if delta.Reasoning > 0 {
		out.Reasoning = delta.Reasoning
	}
	if delta.In > 0 {
		out.In = delta.In
	}
	out.Raw = delta.Raw
	return &out
}

// --- header plumbing ---

var hopByHop = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
	"Proxy-Authorization": true, "Te": true, "Trailer": true,
	"Transfer-Encoding": true, "Upgrade": true,
	// identity/length are recomputed by net/http for the client side
	"Content-Length": true,
}

// stripAlways are request headers that must never reach upstream: they describe
// the client's connection to US, not its intent toward the provider.
var stripAlways = map[string]bool{
	"Authorization":   true, // adapter re-applies the provider's own credential
	"X-Api-Key":       true,
	"Cookie":          true,
	"Host":            true,
	"Content-Length":  true,
	"Accept-Encoding": true, // let the upstream decide; we must see plain bytes
	// Browsers attach Origin to every same-origin POST; forwarding it made
	// Anthropic-shaped upstreams read the proxied call as a CORS/direct-browser
	// request and answer 401 ("dangerous-direct-browser-access" gate). The
	// header describes the browser->dashboard hop and must die here.
	"Origin": true,
	// X-Ezllm-Session is the operator's ledger grouping key (M9). It describes
	// the client->gateway hop, not the gateway->provider hop, so it must not
	// reach the upstream.
	"X-Ezllm-Session": true,
	// x-ezllm-compression is the same class: it tells THIS gateway which
	// compression profile to run, so it is an operator-side ledger header with
	// no meaning upstream. It was a known leak carried for scope reasons once;
	// a provider that started reading it would see a client implementation
	// detail, so it dies here too.
	"X-Ezllm-Compression": true,
}

// copyClientHeaders copies src's headers onto dst minus stripAlways and the
// adapter's extraStrip, both matched case-insensitively. It runs per attempt,
// so it builds nothing: CanonicalHeaderKey returns an already-canonical key
// without allocating, and extraStrip is a handful of names compared in place.
func copyClientHeaders(dst, src *http.Request, extraStrip []string) {
	for k, vs := range src.Header {
		if stripAlways[http.CanonicalHeaderKey(k)] || containsFold(extraStrip, k) {
			continue
		}
		for _, v := range vs {
			dst.Header.Add(k, v)
		}
	}
}

func containsFold(names []string, k string) bool {
	for _, n := range names {
		if strings.EqualFold(n, k) {
			return true
		}
	}
	return false
}

func copyResponseHeaders(w http.ResponseWriter, resp *http.Response) {
	for k, vs := range resp.Header {
		if hopByHop[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
}

// --- small utils ---

// sseHasErrorEnvelope reports whether an SSE payload is an error object.
// A streamed failure is NOT always signalled by the HTTP status: some
// providers answer 200 with the error inside the stream's first data payload.
// Detecting it is what lets the ledger record WHY a "successful" stream was
// actually a failure.
func sseHasErrorEnvelope(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	var ev map[string]any
	if err := json.Unmarshal(payload, &ev); err != nil {
		return false
	}
	e, ok := ev["error"]
	return ok && e != nil
}

// maxErrPeek bounds how much of a FAILED response body is read for diagnostics.
// Error envelopes are tiny; 2 KiB costs nothing and still holds an HTML error
// page's first line.
const maxErrPeek = 2048

// peekUpstreamError reads a bounded prefix of a failed response's body and puts
// it back, so failover can report the provider's own words without stealing the
// body from a later commit. Only non-2xx responses are touched, and the restored
// reader repays every byte read, so the response stays committable byte-for-byte
// as the final fallback.
func peekUpstreamError(resp *http.Response) string {
	if resp == nil || resp.Body == nil || resp.StatusCode < 400 {
		return ""
	}
	head := make([]byte, maxErrPeek)
	n, _ := io.ReadFull(resp.Body, head) // a short body yields ErrUnexpectedEOF; n holds what there was
	if n > 0 {
		resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(head[:n]), resp.Body))
	}
	// Same rendering as the ledger's err column, so a log line and a row read
	// identically.
	return upstreamErrorSnippet(resp.StatusCode, head[:n])
}

// upstreamErrorSnippet renders a bounded, single-line summary of an upstream
// error body for the ledger.
//
// Bounded because the ledger is not a request log, and single-line because the
// dashboard renders err inline — a raw multi-line provider body would wreck the
// table. The status is prefixed so a row is self-describing even if the body is
// empty or non-JSON (HTML error pages happen).
//
// The semantic cause from provider.ErrorCause leads in brackets —
// `err LIKE '%[rate_limited]%'` is a diagnosis — and the upstream's own
// words follow verbatim.
func upstreamErrorSnippet(status int, raw []byte) string {
	cause := provider.ErrorCause(status, raw)
	s := strings.Join(strings.Fields(string(raw)), " ")
	if s == "" {
		return fmt.Sprintf("upstream %d [%s]: (empty body)", status, cause)
	}
	if len(s) > maxErrSnippet {
		// Cut on a rune boundary so a multi-byte character is never split into
		// invalid UTF-8 — the column is TEXT and the dashboard will render it.
		cut := maxErrSnippet
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return fmt.Sprintf("upstream %d [%s]: %s", status, cause, s)
}

func msPtr(d time.Duration) *int64 {
	v := d.Milliseconds()
	return &v
}

// msAtLeastOne renders a duration in whole milliseconds with a floor of 1.
//
// TTFT against a fast upstream is routinely sub-millisecond, and Milliseconds()
// truncates that to 0 — which the dashboard cannot distinguish from "never
// measured". Clamping to 1 keeps the value honest: it means "under 1ms", and
// the ttft<total streaming invariant stays meaningful.
func msAtLeastOne(d time.Duration) *int64 {
	v := d.Milliseconds()
	if v < 1 {
		v = 1
	}
	return &v
}

// store2surface maps the wire surface onto the ledger's surface enum.
func store2surface(s provider.Surface) store.Surface {
	switch s {
	case provider.SurfaceAnthropic:
		return store.SurfaceAnthropic
	case provider.SurfaceResponses:
		return store.SurfaceResponses
	default:
		return store.SurfaceOpenAI
	}
}

// StatusText renders an upstream error body for logging without leaking keys.
func StatusText(status int, body []byte) string {
	return "HTTP " + strconv.Itoa(status) + ": " + truncate(string(body), 200)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
