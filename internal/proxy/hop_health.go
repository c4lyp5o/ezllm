package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/ratelimit"
	"github.com/c4lyp5o/ezllm/internal/settings"
)

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
	// keyID and keyHint name the parked key.
	keyID   int64
	keyHint string
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
	strikes  int
	failed   bool
	cooldown time.Duration
	// clientCancelled marks an attempt that failed because the CALLER hung up.
	// It is neither a success nor a hop failure: recording it as either one is
	// wrong (clearing a legitimate park, or parking a healthy hop for a closed
	// browser tab), so recordHop leaves the hop's state untouched.
	clientCancelled bool
	// noVerdict marks an attempt refused before any dial (the adapter knows
	// the hop cannot serve this wire). Like a cancel it says nothing about the
	// hop's health, so recordHop must neither park it nor clear its park.
	noVerdict bool
	// reason is the operator-facing cause, surfaced on /admin/health. A parked
	// hop produces no ledger row, so without this a hop could disappear from
	// routing with no recorded explanation anywhere.
	reason string
}

const (
	// defaultHopCooldown parks a hop whose failure carries neither an upstream
	// Retry-After nor an operator duration (transport errors).
	defaultHopCooldown = 5 * time.Minute
	// maxCooldownEntries bounds the hop cooldown map; see recordHop.
	maxCooldownEntries = 4096

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
//
// The KEY belongs in it too: providers meter each key separately, so a 429 on
// one of an account's keys parks only that key for that model, and the
// account's next key is the next candidate.
func hopKey(rt Route) string {
	return endpointKey(rt) + "|" + strconv.FormatInt(rt.KeyID, 10)
}

// endpointKey is hopKey without the key: one model on one account, whichever
// key dials it.
func endpointKey(rt Route) string {
	return fmt.Sprintf("%d|%s|%s", rt.Account.ID, strings.TrimRight(rt.Account.BaseURL, "/"), rt.Model)
}

// keyScopedStatus reports whether an upstream status is about the KEY rather
// than the model or the endpoint: a rate limit, a refused key. Only
// these are worth trying again on the same account's next key; a 5xx or a bad
// request would fail the same way on every key.
func keyScopedStatus(code int) bool {
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		return true
	}
	return false
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
	Namespace string `json:"namespace"`
	Model     string `json:"model"`
	// KeyID and KeyHint name the parked key: an account with several keys
	// parks them one at a time.
	KeyID       int64     `json:"key_id,omitempty"`
	KeyHint     string    `json:"key_hint,omitempty"`
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
			KeyID:       st.keyID,
			KeyHint:     st.keyHint,
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
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].KeyID < out[j].KeyID
	})
	return out
}

// ClearCooldowns ends parks early: every one, every model of a namespace, or
// one namespace and model. It is the operator's "Unpark now" after fixing the
// cause (a topped-up balance, a lifted limit). The hop is tried on its next
// request; a real failure parks it again. It returns how many were cleared.
func (d *Dispatcher) ClearCooldowns(namespace, model string) int {
	d.cooldownMu.Lock()
	n := 0
	for k, st := range d.cooldowns {
		if namespace != "" && st.namespace != namespace {
			continue
		}
		if model != "" && st.model != model {
			continue
		}
		delete(d.cooldowns, k)
		n++
	}
	d.cooldownMu.Unlock()
	if n > 0 {
		d.log.Info("cooldowns cleared", "namespace", namespace, "model", model, "count", n)
	}
	return n
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

// classifyHopPolicy decides what one hop attempt means: whether it failed,
// and how long the hop should be parked for.
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
func (d *Dispatcher) classifyHopPolicy(ctx context.Context, err error, status int, hdr http.Header, hasAlternate bool) hopOutcome {
	on429, on5xx := d.on429, d.on5xx
	if p, ok := settings.From(ctx); ok {
		on429 = settings.Duration(p.Cooldown429)
		on5xx = settings.Duration(p.Cooldown5xx)
	}
	if err != nil {
		// The CALLER hung up (closed tab, cancelled agent run, shutdown). The
		// dial fails with context.Canceled, which is a net.Error, so without
		// this check it reads as a transport fault on the hop: one strike, and
		// at the default threshold a five-minute park. Measured live: 80 such
		// rows in seven days, and eight later requests answered "all combo
		// endpoints are in five-minute cooldown".
		if ctx.Err() != nil {
			return hopOutcome{clientCancelled: true, reason: "client cancelled"}
		}
		if errors.Is(err, provider.ErrWireUnsupported) {
			return hopOutcome{noVerdict: true, reason: "wire unsupported"}
		}
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
		if on429 > 0 {
			return hopOutcome{failed: true, cooldown: on429, reason: "429 rate limited"}
		}
		return hopOutcome{failed: true, cooldown: defaultHopCooldown, reason: "429 rate limited"}
	case status >= 500 || status == http.StatusRequestTimeout || status == http.StatusTooEarly:
		reason := fmt.Sprintf("%d upstream error", status)
		if dur, ok := parseRetryAfter(hdr.Get("Retry-After")); ok {
			return hopOutcome{failed: true, cooldown: dur, reason: reason + " (Retry-After)"}
		}
		// 408/425 are transient by definition and share the 5xx duration: the
		// operator configured one "hop is unhealthy" number, not two.
		if on5xx > 0 {
			return hopOutcome{failed: true, cooldown: on5xx, reason: reason}
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
		if oc.clientCancelled || oc.noVerdict {
			return // neither a cancel nor a pre-dial refusal says anything about this hop
		}
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
	// Entries leave only when their hop succeeds, and a model that never
	// answers (a made-up ID on an account with no catalog yet) never does.
	// Past the cap, sweep the ones whose park is over and that hold no probe.
	if _, had := d.cooldowns[key]; !had && len(d.cooldowns) >= maxCooldownEntries {
		for k, v := range d.cooldowns {
			if !v.until.After(now) && !v.probeActive {
				delete(d.cooldowns, k)
			}
		}
	}
	st := d.cooldowns[key]
	st.strikes++
	threshold := d.maxStrikes
	if oc.strikes > 0 {
		threshold = oc.strikes
	}
	if threshold < 1 {
		threshold = 1
	}
	if st.strikes >= threshold {
		st.until = now.Add(oc.cooldown)
		st.strikes = 0
		st.namespace = rt.Account.Namespace
		st.model = rt.Model
		st.keyID, st.keyHint = rt.KeyID, rt.KeyHint
		st.reason = oc.reason
		// Park events are rare (threshold crossings), so this stays inside the
		// lock: it is the one line that explains an endpoint disappearing.
		d.log.Info("hop parked",
			"account", rt.Account.Namespace, "model", rt.Model, "key", rt.KeyHint,
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
