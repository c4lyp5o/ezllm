// Package proxy forwards a client request upstream and copies the response back
// byte-for-byte, tapping usage for the ledger.
//
// THE INVARIANT: for streaming responses we never buffer, never re-serialize,
// and never add latency. Bytes go upstream->client via io.Copy semantics with a
// Flush per chunk; the usage tap is a read-only observer on a tee.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/c4lyp5o/ezllm/internal/bridgeutil"
	"github.com/c4lyp5o/ezllm/internal/jsonbody"
	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/ratelimit"
	"github.com/c4lyp5o/ezllm/internal/settings"
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

// maxScanToken bounds the line the usage tap inspects. The response itself is
// NOT bounded (it is streamed): a longer line is forwarded whole, untapped.
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
// So we merge rather than take the first hit — the RAW usage objects, key by
// key, and normalize once at the end. Normalizing each event alone and then
// mixing counters could pair one event's input with another's cache split.
type UsageTapper struct {
	surface   provider.Surface
	conv      provider.UsageConvention
	raw       map[string]any
	event     []byte
	skipEvent bool
	terminal  bool
	// responseID is the Responses wire's response id (response.created), so
	// a later request naming it as previous_response_id joins the same turn.
	responseID string
}

// NewUsageTapperFor builds a tapper that normalizes with the account's
// declared usage convention (it changes only the Messages wire's reading).
func NewUsageTapperFor(surface provider.Surface, conv provider.UsageConvention) *UsageTapper {
	return &UsageTapper{surface: surface, conv: conv}
}

// Observe inspects a physical SSE line without changing forwarded bytes.
// Single-line JSON stays on the fast path; incomplete JSON is assembled only
// up to maxScanToken. An oversized event is ignored until its blank separator.
func (t *UsageTapper) Observe(line []byte) {
	line = bytes.TrimRight(line, "\r\n")
	if len(line) == 0 {
		t.event = nil
		t.skipEvent = false
		return
	}
	if t.skipEvent || !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	data := bytes.TrimPrefix(line, []byte("data:"))
	if t.surface == provider.SurfaceChat && bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		t.terminal = true
		return
	}
	if len(t.event) == 0 && len(data) <= maxScanToken && json.Valid(data) {
		t.observePayload(line)
		return
	}
	if len(t.event)+len(data)+1 > maxScanToken {
		t.event = nil
		t.skipEvent = true
		return
	}
	t.event = append(t.event, data...)
	t.event = append(t.event, '\n')
	if json.Valid(t.event) {
		t.observePayload(append([]byte("data:"), t.event...))
		t.event = nil
	}
}

func (t *UsageTapper) observePayload(line []byte) {
	if t.surface != provider.SurfaceChat {
		var event struct {
			Type     string `json:"type"`
			Response struct {
				ID string `json:"id"`
			} `json:"response"`
		}
		if json.Unmarshal(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))), &event) == nil {
			switch t.surface {
			case provider.SurfaceAnthropic:
				t.terminal = t.terminal || event.Type == "message_stop"
			case provider.SurfaceResponses:
				t.terminal = t.terminal || event.Type == "response.completed" || event.Type == "response.incomplete" || event.Type == "response.failed"
				if t.responseID == "" && event.Response.ID != "" {
					t.responseID = event.Response.ID
				}
			}
		}
	}
	if u := sseUsageRaw(line, t.surface); u != nil {
		if t.raw == nil {
			t.raw = map[string]any{}
		}
		mergeUsageRaw(t.raw, u)
	}
}

// Usage returns the merged result, or nil when the stream carried none.
func (t *UsageTapper) Usage() *store.Usage {
	if t.raw == nil {
		return nil
	}
	n := store.NormalizeUsageWith(usageSurface(t.surface), t.raw, t.conv)
	return &n
}

// mergeUsageRaw folds a later usage object into an earlier one: nested
// objects merge, and a later value wins unless it is a zero (or null)
// standing in for a number the earlier event did report — Anthropic's
// message_delta repeats input_tokens but may zero what it does not carry.
// The output count is the exception: message_start's is a placeholder (1)
// and the later event's is the final cumulative count, zero included.
func mergeUsageRaw(dst, src map[string]any) {
	for k, v := range src {
		// A later zero replaces only message_start's placeholder (0 or 1),
		// never a real count an earlier event reported.
		if k == "output_tokens" && v != nil {
			if old, had := dst[k].(float64); had && old > 1 && isZeroUsage(v) {
				continue
			}
			dst[k] = v
			continue
		}
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				mergeUsageRaw(dm, sm)
				continue
			}
			cp := map[string]any{}
			mergeUsageRaw(cp, sm)
			dst[k] = cp
			continue
		}
		if old, had := dst[k]; had && isZeroUsage(v) && !isZeroUsage(old) {
			continue
		}
		dst[k] = v
	}
}

func isZeroUsage(v any) bool {
	switch n := v.(type) {
	case nil:
		return true
	case float64:
		return n == 0
	}
	return false
}

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
	// CLIENT's surface; accounting observes the effective upstream wire. Set by the router from probed capability; nothing else
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
	// Reasoning is what THIS model accepts for `reasoning_effort`, learned from
	// its own refusal. The zero value means nothing is known, and prepareHopBody
	// then sends the client's request byte-identical.
	Reasoning provider.ReasoningRule
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
		return provider.SurfaceChat
	case rt.ResponsesWire:
		return provider.SurfaceResponses
	}
	return rt.Surface
}

// Attempt is one candidate that did NOT serve the request, in the order the
// dispatcher abandoned it. It exists because a failed or skipped hop writes no
// delivery row of its own; observed usage is carried separately in
// DiscardedUsage. Without the trail, unmetered/skipped hops are invisible to the
// operator's recent-calls view and only exist in logs.
type Attempt struct {
	Namespace string // hop namespace
	Model     string // hop model
	Status    int    // the upstream status the hop earned (0 = never dialed)
	Note      string // short reason: classified cause, skip reason, transport error
}

// Result carries what the ledger needs after a dispatch. Attempts lists every
// candidate that did not serve, dispatch order; the serving hop is never listed.
// LearnedReasoning is one fact a refusal taught us, tagged with the hop that
// taught it: the vocabulary is per (account, model), never global.
type LearnedReasoning struct {
	AccountID int64
	// Namespace names the account for the console line; the store keys on ID.
	Namespace string
	Model     string
	Rule      provider.ReasoningRule
}

// Result reports the outcome of one request.
type Result struct {
	Status  int
	Stream  bool
	TTFTms  *int64
	Totalms *int64
	// SlotWaitms is how long a subscription bridge queued the request for a
	// CLI slot (part of Totalms); nil for every other upstream.
	SlotWaitms *int64
	// ResponseID is the native Responses wire's response id (empty for other
	// wires and translated hops); the server chains stateful turns with it.
	ResponseID    string
	Usage         *store.Usage
	EndpointID    string
	UpstreamModel string
	Err           string
	Attempts      []Attempt
	// DiscardedUsage contains source-accounted attempts that did not deliver.
	DiscardedUsage []store.Call
	// LearnedReasoning is what an upstream refusal taught us about a model's
	// `reasoning_effort` vocabulary. The proxy has no store, so it reports the
	// fact and the server persists it (store.LearnModelReasoning); nil means
	// nothing was learned on this request.
	LearnedReasoning *LearnedReasoning
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
	callerBridge  atomic.Pointer[callerBridge]
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
	spent []store.Call
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
	// client-facing keeps the client surface. Accounting reads the dial wire. One
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
			url = base + "/run/" + modelPath(rt.Model)
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
	d.stampCaller(ctx, upReq, rt.KeyPlain)
	// Google AI Studio native audio uses x-goog-api-key, not Bearer.
	if isGeminiNativeAudio(rt, rt.Surface) {
		upReq.Header.Set("x-goog-api-key", rt.KeyPlain)
		upReq.Header.Del("Authorization")
	}
	// Content-Length: NewRequestWithContext set it from body; an adapter that
	// rewrites the body (Copilot drops system messages) sets its own. Never
	// re-stamp len(body) here — a shorter rewritten body would then fail the
	// client's length check.
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
			// geminiAudioResponse closes the body itself once it has READ it;
			// the read-failure path returns before that point, and this
			// response is discarded (nil is returned to the caller), so the
			// body would be abandoned unclosed. Close is idempotent on the
			// paths that already closed it.
			_ = resp.Body.Close()
			return nil, Result{Stream: stream, Totalms: msPtr(time.Since(start)), Err: err.Error()}, time.Time{}, err
		}
	}

	res := Result{
		Status:        resp.StatusCode,
		Stream:        stream,
		EndpointID:    resp.Header.Get("x-opencode-endpoint-id"),
		UpstreamModel: resp.Header.Get("x-opencode-upstream-model-id"),
		SlotWaitms:    bridgeSlotWait(upReq.URL, resp.Header),
	}
	return resp, res, start, nil
}

// bridgeSlotWait reads how long a subscription bridge held the request in its
// slot queue (bridgeutil.SlotWaitHeader). Only a loopback upstream is trusted:
// the bridges are in-process, and a remote provider's header must not write
// into the ledger.
func bridgeSlotWait(u *url.URL, h http.Header) *int64 {
	v := h.Get(bridgeutil.SlotWaitHeader)
	if v == "" || u == nil {
		return nil
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return nil
	}
	return &n
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
	if wire == provider.SurfaceChat {
		// Legacy anthropic client on the chat wire: errors and failures are
		// both rendered as an anthropic error envelope.
		return crossWire{
			wire: wire, opts: legacy,
			reshape:   translate.Error,
			errPrefix: "translate response: ",
			failBody:  translate.Error,
		}, true
	}
	// Legacy responses-wire hop. A chat client shares the upstream's
	// {"error":{…}} family, so its error bodies pass through byte-identical.
	cw := crossWire{
		wire: wire, opts: legacy,
		errPrefix: "translate responses body: ",
		failBody: func([]byte) []byte {
			return []byte(`{"error":{"type":"upstream_error","message":"responses body could not be translated","code":502}}`)
		},
	}
	if rt.Surface == provider.SurfaceAnthropic {
		// An anthropic client does not: it gets the Messages error envelope,
		// exactly as on the chat wire above, whichever wire its hop rides.
		cw.reshape = translate.Error
		cw.failBody = func([]byte) []byte {
			return translate.EncodeError(translate.ErrorIR{Type: "upstream_error", Message: "responses body could not be translated"}, provider.SurfaceAnthropic)
		}
	}
	return cw, true
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
	// An error status takes the buffered path even when the upstream labels it
	// a stream: some gateways answer 429 or 400 as text/event-stream, and on
	// a translated hop the stream reader would re-shape an error body as if it
	// were answer chunks, skipping the error re-shaping below.
	if res.Stream && isSSE && resp.StatusCode < 400 {
		// A translated stream wraps the body BEFORE any byte is copied: the
		// client sees its own event shape synthesized from the upstream's, and
		// the independent source tap retains the upstream accounting before any
		// lossy client projection. Close still reaches the ORIGINAL body (a
		// NopCloser would leak the upstream connection).
		//
		// translated carries the reader whose terminal state becomes the
		// delivery outcome; nil for a native stream.
		var translated *translate.StreamReader
		var sourceTap *UsageTapper
		if cross {
			if cw.optsErr != nil {
				// A cell that cannot be converted must not stream
				// foreign-shaped bytes to the client.
				gatewayFault(w, &res, start, "translate stream: "+cw.optsErr.Error(),
					translate.EncodeError(translate.ErrorIR{Type: "api_error", Message: cw.optsErr.Error()}, rt.Surface))
				return res, nil
			}
			sourceTap = NewUsageTapperFor(cw.wire, rt.Account.UsageConvention)
			resp.Body = &sourceUsageReader{ReadCloser: resp.Body, tap: sourceTap}
			translated = translate.NewStreamReader(resp.Body, cw.wire, rt.Surface, rt.Alias, cw.opts)
			resp.Body = struct {
				io.Reader
				io.Closer
			}{translated, resp.Body}
		}
		// streamCopyTracked writes the header before its first byte, so the status
		// is committed the moment it returns — regardless of a copy error.
		res.Committed = true
		ttft, tap, first, copyErr := d.streamCopyTap(ctx, w, resp, rt.Surface, tapConvention(rt, cross), start, !cross && resp.StatusCode < 400)
		res.TTFTms = msAtLeastOne(ttft)
		if sourceTap != nil {
			res.Usage = sourceTap.Usage()
		} else if usage := tap.Usage(); usage != nil {
			res.Usage = usage
		}
		if !cross {
			res.ResponseID = tap.responseID
		}
		if copyErr != nil {
			// The client has already received a 200 + partial stream, so we
			// cannot change the status. Record the truncation instead: a
			// half-delivered stream must never look like a clean success in
			// the ledger.
			res.Err = "stream truncated: " + copyErr.Error()
			// The OUTCOME, not just the prose. StreamOutcome used to be set
			// only for translated streams, so a NATIVE stream that died
			// mid-body left it empty — StreamCompleted() then answered true
			// and hop health treated a cut answer as a delivered one: no
			// MarkHopBad, no strike, no park, and a hop-health rate that
			// could never see the failure. Measured live: 16 mid-stream
			// resets on one hop in 8 days, none of which struck it.
			res.StreamOutcome = StreamOutcomeTruncated
		}
		// Mirror the non-stream branch's err-column rule. The stream branch
		// had no equivalent, so a failover-committed SSE error response — a
		// 429 whose reason rides in the first data payload — landed in the
		// ledger with a blank err, the one field that diagnoses an incident
		// after the fact. Some providers also answer 200 SSE carrying an
		// error object inside the stream, which is equally worth recording.
		res.Err = streamErrCause(res.Err, res.Status, first)
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
	} else if resp.StatusCode < 400 && !cross && isBinaryMedia(resp.Header.Get("Content-Type")) {
		// Audio, video and other binary answers pass straight through as they
		// arrive: speech starts playing before generation ends, and a large
		// file is never held whole in memory. They carry no JSON usage to tap
		// and nothing to translate.
		streamBinary(w, resp, &res)
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
		// Accounting belongs to the source wire, even if client conversion fails.
		res.Usage = tapJSONUsage(raw, rt.EffectiveWire(), rt.Account.UsageConvention)
		if !cross && rt.Surface == provider.SurfaceResponses && res.Status < 400 {
			res.ResponseID = responsesBodyID(raw)
		}
		// A translated hop re-encodes every byte the client sees into its own
		// shape. The err-column snippet is captured from the upstream's OWN
		// words before any re-shaping. The codec produces the translated
		// body only for delivery; usage was already captured on the source wire.
		if cross {
			if res.Status >= 400 {
				if cw.reshape != nil {
					if res.Err == "" {
						res.Err = upstreamErrorSnippet(res.Status, raw)
					}
					raw = cw.reshape(raw)
					if isSSE {
						w.Header().Set("Content-Type", "application/json")
					}
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

// isBinaryMedia reports a response body that is media, not JSON: it can be
// passed through as it arrives.
func isBinaryMedia(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	return strings.HasPrefix(ct, "audio/") || strings.HasPrefix(ct, "video/") ||
		strings.HasPrefix(ct, "image/") || ct == "application/octet-stream"
}

// binaryChunk is the pass-through read size for binary media.
const binaryChunk = 32 << 10

// streamBinary copies a successful media body to the client chunk by chunk,
// flushing each one. The status is committed before the first byte, so a
// failure after that is recorded in the ledger instead of changing the status.
// TTFT stays unset: the dashboard's TTFT medians are for text answers.
func streamBinary(w http.ResponseWriter, resp *http.Response, res *Result) {
	res.Committed = true
	w.WriteHeader(resp.StatusCode)
	fl, _ := w.(http.Flusher)
	buf := make([]byte, binaryChunk)
	var total int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			total += int64(n)
			if _, werr := w.Write(buf[:n]); werr != nil {
				res.Err = "client write: " + werr.Error()
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			// The client already has a 200 and part of the file.
			res.Err = "stream truncated: " + rerr.Error()
			res.StreamOutcome = StreamOutcomeTruncated
			return
		}
	}
	if total == 0 && resp.StatusCode == http.StatusOK {
		res.Err = "upstream returned an empty body"
		res.StreamOutcome = StreamOutcomeTruncated
	}
}

// Forward sends the (already model-swapped) body upstream and copies the
// response to w. It returns the ledger Result. Single-candidate form: nothing
// to fail over to, so attempt and commit happen back to back.
func (d *Dispatcher) Forward(ctx context.Context, w http.ResponseWriter, r *http.Request, rt Route, body []byte) (result Result, resultErr error) {
	policy := d.requestPolicy(ctx)
	deadline := settings.Duration(policy.DispatchDeadline)
	led := &attemptLedger{limit: policy.AttemptBudget}
	defer func() { result.DiscardedUsage = led.spent }()
	var sel time.Time
	if deadline > 0 {
		sel = time.Now().Add(deadline)
	}
	resp, res, start, err := d.retryAttempt(ctx, r, rt, body, nil, led, sel)
	res.UpstreamAttempts, res.AttemptBudget = led.used, policy.AttemptBudget
	if err != nil {
		return res, err
	}
	return d.commit(ctx, w, resp, rt, res, start)
}

func (d *Dispatcher) retryAttempt(ctx context.Context, r *http.Request, rt Route, body []byte, doc *jsonbody.BodyDoc, led *attemptLedger, sel time.Time) (*http.Response, Result, time.Time, error) {
	maxAttempts := d.requestPolicy(ctx).Retries + 1
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
		if led != nil && led.exhausted() {
			discardAttempt(resp, rt, result, start, led)
			return nil, result, start, errAttemptBudgetSpent
		}
		if resp != nil {
			discardAttempt(resp, rt, result, start, led)
			lastResp = nil // the consumed response can no longer be a final fallback
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
func (d *Dispatcher) ForwardCandidatesDoc(ctx context.Context, w http.ResponseWriter, r *http.Request, routes []Route, body []byte, doc *jsonbody.BodyDoc) (served Route, result Result, resultErr error) {
	policy := d.requestPolicy(ctx)
	deadline := settings.Duration(policy.DispatchDeadline)
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
	// refusal is the first typed translation refusal: the answer when every
	// hop was skipped and none was even cooling for a better reason.
	var refusal error
	// contextSkips counts hops skipped because the request is larger than
	// their declared context window, so the final error can say so instead of
	// blaming a cooldown that never happened.
	contextSkips := 0
	// failedEndpoints holds, per endpointKey, why that model already failed on
	// one of the account's keys for a reason no other key would change (a 5xx,
	// a transport error, a bad request). Its sibling keys are skipped instead
	// of dialed: they would fail the same way and spend the attempt budget.
	var failedEndpoints map[string]string
	markEndpoint := func(rt Route, why string) {
		if failedEndpoints == nil {
			failedEndpoints = map[string]string{}
		}
		failedEndpoints[endpointKey(rt)] = why
	}
	// Addendum #3: ONE request-wide budget across same-hop retries AND
	// failover hops, and a selection deadline that governs only the choosing
	// (never the winning attempt's stream).
	led := &attemptLedger{limit: policy.AttemptBudget}
	defer func() { result.DiscardedUsage = led.spent }()
	// A parked fallback response is owned by this function until it is either
	// committed or discarded. Every discard below clears the pointer, so this
	// defer only ever fires on an EARLY return — the path where a later hop's
	// body cannot be prepared and the loop leaves before reaching the code that
	// would have closed it. Without it that return abandoned the parked body
	// and its upstream connection.
	discardFallback := func() {
		if fallbackResp != nil {
			discardAttempt(fallbackResp, *fallbackRoute, fallbackResult, fallbackStart, led)
			fallbackResp = nil
		}
	}
	defer discardFallback()
	var sel time.Time
	if deadline > 0 {
		sel = time.Now().Add(deadline)
	}
	budgetNote := func(ns, model string) Attempt {
		return Attempt{
			Namespace: ns, Model: model,
			Note: fmt.Sprintf("request attempt budget spent (%s/%d used)", budgetLabel(policy.AttemptBudget), led.used),
		}
	}
	for i := 0; i < len(routes); i++ {
		// The caller has left: no later hop is dialed, probed or charged
		// against its account's request budget for a response nobody reads.
		if ctx.Err() != nil {
			// A cancellation outranks a PARKED fallback response. When the last
			// hop answered a failover status, its response is held here to be
			// committed if nothing else serves; committing it after the caller
			// left presents a hop error as the reason the call ended and returns
			// a nil error to the server, so the cancellation never reaches the
			// ledger. Whether that happened used to depend on which side won the
			// race: a hop whose TRANSPORT noticed the cancel set
			// fallbackTransportErr and surfaced it, while one that had already
			// written a 503 parked it and was committed as a success. Discard
			// the parked body and let the error path answer — the defer would
			// close it, but the pointer is cleared here so the defer is a no-op.
			discardFallback()
			if fallbackTransportErr == nil {
				fallbackTransportErr = ctx.Err()
			}
			break
		}
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
		if why, failed := failedEndpoints[endpointKey(rt)]; failed {
			attempts = append(attempts, Attempt{
				Namespace: rt.Account.Namespace, Model: rt.Model,
				Note: "not tried on another key: " + why,
			})
			continue
		}
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
				contextSkips++
				continue
			}
		}
		// The hop's body is prepared before any hop state is touched. A hop
		// whose wire cannot carry this request (a typed translation refusal)
		// is skipped like a cooling hop — no dial, no probe slot, no rate
		// budget — and the combo moves on. The refusal is the answer only
		// when no later hop serves.
		swapped, effort, err := prepareHopBody(body, rt, doc)
		if err != nil {
			var ufe *translate.UnsupportedFieldError
			if !errors.As(err, &ufe) {
				return Route{}, Result{}, err
			}
			d.log.Info("hop skipped: request not portable to its wire",
				"account", rt.Account.Namespace, "model", rt.Model,
				"alias", rt.Alias, "why", err.Error())
			attempts = append(attempts, Attempt{
				Namespace: rt.Account.Namespace, Model: rt.Model, Note: err.Error(),
			})
			if refusal == nil {
				refusal = err
			}
			continue
		}
		// The clamp REWRITES what the client asked for, so it is never silent:
		// a client that sets "none" and gets "minimal" must be able to find out
		// why from the console. Info, because it only fires on a request that
		// would otherwise have been refused — and the client's own level is
		// still on the request, so this names the hop that forced the change.
		if effort != nil {
			if effort.Dropped {
				d.log.Info("reasoning effort dropped for this hop",
					"account", rt.Account.Namespace, "model", rt.Model,
					"asked", effort.From, "why", "reasoning is mandatory for this model")
			} else {
				d.log.Info("reasoning effort clamped for this hop",
					"account", rt.Account.Namespace, "model", rt.Model,
					"asked", effort.From, "sent", effort.To)
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
		resp, res, start, err := d.retryAttempt(ctx, r, rt, swapped, doc, led, sel)
		res.UpstreamAttempts, res.AttemptBudget = led.used, policy.AttemptBudget
		// A refusal that names its own `reasoning_effort` vocabulary is worth
		// keeping: the next request to this hop gets clamped instead of refused.
		// Read here — before the failover/commit split — so a SOLE hop that
		// answers 400 teaches us too, not only one that failed over. Only 4xx:
		// a 200 body may be a stream, and peeking it would break the delivery.
		if err == nil && resp != nil && res.Status >= 400 && res.Status < 500 && result.LearnedReasoning == nil {
			if rule := provider.ParseReasoningRule(res.Status, peekBody(resp, maxErrPeek)); rule.Known {
				result.LearnedReasoning = &LearnedReasoning{
					AccountID: rt.Account.ID, Namespace: rt.Account.Namespace,
					Model: rt.Model, Rule: rule,
				}
			}
		}
		// Park the hop only when there is somewhere else to go. len(routes) is
		// the full candidate list the router resolved; a single entry means a
		// cooled hop has no alternate, and classifyHopPolicy declines to park a
		// rate limit in that case.
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
			oc = d.classifyHopPolicy(ctx, nil, res.Status, hdr, len(routes) > 1)
			if p, ok := settings.From(ctx); ok {
				oc.strikes = p.CooldownStrikes
			}
			d.recordHop(rt, oc, time.Now())
		default:
			oc = d.classifyHopPolicy(ctx, err, res.Status, hdr, len(routes) > 1)
			if err == nil && refusesWire(resp) {
				oc = hopOutcome{failed: true, reason: "protocol unsupported"}
				if len(routes) > 1 {
					oc.cooldown = defaultHopCooldown
				}
			}
			// A history refusal is a hop fact too, but unlike a wire refusal it
			// says NOTHING about the hop's health: the same model serves a
			// conversation it started. So it fails over with no cooldown at all
			// — the attempt trail and the cause carry the explanation.
			if err == nil && !oc.failed && refusesHistory(resp) {
				oc = hopOutcome{failed: true, reason: "history has no reasoning this model requires"}
			}
			// A tools refusal is a hop fact in the same way: fail over, no cooldown.
			if err == nil && !oc.failed && refusesTools(resp) {
				oc = hopOutcome{failed: true, reason: "hop cannot honor requested tools or parameters"}
			}
			// A 403 that names model access is a fact about the hop, so park it
			// exactly as a 429 would be: same model-scoped key, same TTL, same
			// half-open probe. hasAlternate is required for the same reason as
			// the 429 path — a sole hop is better retried than refused from the
			// in-process park.
			if err == nil && !oc.failed && len(routes) > 1 && deniesModelAccess(resp) {
				oc = hopOutcome{failed: true, cooldown: defaultHopCooldown, reason: "model access denied"}
			}
			if p, ok := settings.From(ctx); ok {
				oc.strikes = p.CooldownStrikes
			}
			d.recordHop(rt, oc, time.Now())
		}
		if probe {
			d.releaseProbe(rt)
		}
		if err != nil {
			// The hop's outcome was recorded above; a second record here once
			// cleared the park a transport error had just set.
			note := "transport: " + err.Error()
			switch {
			case errors.Is(err, errAttemptBudgetSpent):
				note = err.Error()
				d.log.Info("hop not dialed", // Info, not Warn: nothing failed
					"account", rt.Account.Namespace, "model", rt.Model,
					"alias", rt.Alias, "why", err.Error())
			case errors.Is(err, provider.ErrWireUnsupported):
				// Not an outage and not a bad request: this hop cannot serve
				// the wire this model needs. Say so in the trail instead of
				// dressing it as a transport fault.
				note = err.Error()
				d.log.Info("hop cannot serve this wire, failing over",
					"account", rt.Account.Namespace, "model", rt.Model,
					"alias", rt.Alias, "why", err.Error())
			case oc.clientCancelled:
				// The caller left. Nothing was learnt about this hop, so the
				// trail must not read as a hop fault — and the log is Info,
				// because a client hanging up is routine, not an incident.
				note = "client cancelled: " + err.Error()
				d.log.Info("client cancelled, no hop charged",
					"account", rt.Account.Namespace, "model", rt.Model,
					"alias", rt.Alias, "why", err.Error())
			default:
				d.log.Warn("hop failed, failing over",
					"account", rt.Account.Namespace, "model", rt.Model,
					"alias", rt.Alias, "err", err.Error())
			}
			attempts = append(attempts, Attempt{
				Namespace: rt.Account.Namespace, Model: rt.Model,
				Note: note,
			})
			if !oc.clientCancelled && !errors.Is(err, errAttemptBudgetSpent) {
				markEndpoint(rt, note)
			}
			discardFallback()
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
		if failoverStatus(res.Status) || oc.failed {
			if !keyScopedStatus(res.Status) {
				markEndpoint(rt, fmt.Sprintf("upstream %d on this model", res.Status))
			}
			discardFallback()
			rtCopy, resCopy, startCopy := rt, res, start
			fallbackRoute, fallbackResp, fallbackResult, fallbackStart = &rtCopy, resp, resCopy, startCopy
			continue
		}
		discardFallback()
		res2, err2 := d.commit(ctx, w, resp, rt, res, start)
		// The committed route travels back with the result: the ledger must
		// credit the hop that ACTUALLY served the call. Reporting routes[0]
		// would attribute a failed hop's spend — and, worse, would make a
		// per-account cap meter the wrong budget entirely.
		res2.Attempts = attempts
		// commit builds its own Result; the fact learned above rides with it.
		res2.LearnedReasoning = result.LearnedReasoning
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
		// commit owns the body from here (it closes it), so clear the parked
		// pointer first: the deferred cleaner must not close it a second time.
		toCommit := fallbackResp
		fallbackResp = nil
		res, err := d.commit(ctx, w, toCommit, *fallbackRoute, fallbackResult, fallbackStart)
		res.Attempts = attempts
		// commit builds its own Result, as on the in-loop path: a hop that
		// taught us a vocabulary and then failed over must still report it.
		res.LearnedReasoning = result.LearnedReasoning
		return *fallbackRoute, res, err
	}
	if fallbackTransportErr != nil {
		return Route{}, Result{Attempts: attempts, UpstreamAttempts: led.used, AttemptBudget: policy.AttemptBudget, LearnedReasoning: result.LearnedReasoning}, fallbackTransportErr
	}
	// Nothing was attempted at all because every hop was out of request budget.
	// Report the refusal with the earliest release time so the client can wait
	// instead of blind-retrying — and note that no upstream request was wasted
	// discovering this.
	if rateRefusal != nil {
		return Route{}, Result{Attempts: attempts, UpstreamAttempts: led.used, AttemptBudget: policy.AttemptBudget, LearnedReasoning: result.LearnedReasoning}, rateRefusal
	}
	if refusal != nil {
		return Route{}, Result{Attempts: attempts, UpstreamAttempts: led.used, AttemptBudget: policy.AttemptBudget, LearnedReasoning: result.LearnedReasoning}, refusal
	}
	final := errors.New("proxy: all combo endpoints are in five-minute cooldown")
	switch {
	case contextSkips == len(routes):
		final = errors.New("proxy: the request is larger than every endpoint's context window (max_context_tokens)")
	case contextSkips > 0:
		final = errors.New("proxy: no endpoint can take the request: each is cooling down or has a context window smaller than it")
	}
	return Route{}, Result{Attempts: attempts, UpstreamAttempts: led.used, AttemptBudget: policy.AttemptBudget, LearnedReasoning: result.LearnedReasoning}, final
}
