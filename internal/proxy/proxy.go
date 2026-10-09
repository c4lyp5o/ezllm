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
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// maxBodyBytes caps the request body we will read into memory (we must read it
// to swap `model`). 32 MiB is generous for chat payloads and bounds abuse.
const maxBodyBytes = 32 << 20

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

// Route is a resolved destination: one account + one key + the model to send.
type Route struct {
	Account  provider.Account
	KeyID    int64
	KeyPlain string // decrypted; NEVER logged
	KeyHint  string
	Model    string // model id to send upstream
	Alias    string // what the client asked for (combo name or namespace/model)
	Surface  provider.Surface
}

// Result carries what the ledger needs after a dispatch.
type Result struct {
	Status        int
	Stream        bool
	TTFTms        *int64
	Totalms       *int64
	Usage         *store.Usage
	EndpointID    string
	UpstreamModel string
	Err           string
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
	cooldownMu    sync.Mutex
	cooldowns     map[string]cooldownState
}

type cooldownState struct {
	until       time.Time
	probeActive bool
}

const deadHopCooldown = 5 * time.Minute

func hopKey(rt Route) string {
	return fmt.Sprintf("%d|%s", rt.Account.ID, strings.TrimRight(rt.Account.BaseURL, "/"))
}

// acquireHop grants one request the right to try an endpoint. A cooled endpoint
// has exactly one half-open probe after the five-minute window; concurrent
// callers skip it until that probe succeeds or fails.
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
	if state.until.After(now) || state.probeActive {
		return false, false
	}
	state.probeActive = true
	d.cooldowns[key] = state
	return true, true
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

func cooldownFailure(err error, status int) bool {
	if err != nil {
		return retryableUpstreamError(err)
	}
	return status == http.StatusRequestTimeout || status == http.StatusTooEarly || status >= http.StatusInternalServerError
}

func (d *Dispatcher) recordHop(rt Route, failed bool, now time.Time) {
	key := hopKey(rt)
	d.cooldownMu.Lock()
	defer d.cooldownMu.Unlock()
	if failed {
		if d.cooldowns == nil {
			d.cooldowns = make(map[string]cooldownState)
		}
		d.cooldowns[key] = cooldownState{until: now.Add(deadHopCooldown)}
	} else if d.cooldowns != nil {
		delete(d.cooldowns, key)
	}
}

// NewDispatcher builds a Dispatcher. timeout governs connection/response-header
// time only; streaming bodies are unbounded by design.
func NewDispatcher(client *http.Client, registry *provider.Registry) *Dispatcher {
	return &Dispatcher{client: client, registry: registry, retryAttempts: 3, cooldowns: make(map[string]cooldownState)}
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
// other field byte-for-byte in meaning (unknown provider-specific fields such
// as extra_body, thinking, enable_thinking, tools, reasoning,
// previous_response_id all survive because we round-trip through a map).
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

// attempt runs the upstream call and returns the response WITHOUT touching w.
//
// Split out of Forward so failover can inspect the status before anything is
// committed to the client. This is the whole trick: w.Header() is only a map
// until WriteHeader fires, and WriteHeader is what makes the status
// irrevocable. Keeping the two phases apart is what lets us try hop 2 after
// hop 1 answers 429 — see ForwardCandidates.
func (d *Dispatcher) attempt(ctx context.Context, r *http.Request, rt Route, body []byte) (*http.Response, Result, time.Time, error) {
	ad, err := d.registry.Get(rt.Account.Kind)
	if err != nil {
		return nil, Result{}, time.Time{}, err
	}
	url := provider.UpstreamURL(rt.Account.BaseURL, rt.Account.Kind, provider.SurfacePath(rt.Surface))

	upReq, err := http.NewRequestWithContext(ctx, r.Method, url, bytes.NewReader(body))
	if err != nil {
		return nil, Result{}, time.Time{}, err
	}
	// Copy safe client headers, then let the adapter apply auth + strip its own.
	copyClientHeaders(upReq, r, ad.StripHeaders())
	if err := ad.PrepareRequest(ctx, upReq, rt.Account, rt.KeyPlain, rt.Surface); err != nil {
		return nil, Result{}, time.Time{}, err
	}
	// Content-Length must match the rewritten body.
	upReq.ContentLength = int64(len(body))
	upReq.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := d.client.Do(upReq)
	if err != nil {
		return nil, Result{Stream: IsStream(body), Totalms: msPtr(time.Since(start)), Err: err.Error()}, time.Time{}, err
	}

	res := Result{
		Status:        resp.StatusCode,
		Stream:        IsStream(body),
		EndpointID:    resp.Header.Get("x-opencode-endpoint-id"),
		UpstreamModel: resp.Header.Get("x-opencode-upstream-model-id"),
	}
	return resp, res, start, nil
}

// commit streams one upstream response to the client and closes resp.Body.
// Nothing here may be called twice for one request — after the first
// WriteHeader the status can no longer change.
func (d *Dispatcher) commit(ctx context.Context, w http.ResponseWriter, resp *http.Response, rt Route, res Result, start time.Time) (Result, error) {
	defer resp.Body.Close()

	// Propagate response headers that matter, minus hop-by-hop and identity.
	copyResponseHeaders(w, resp)

	isSSE := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
	if res.Stream && isSSE {
		ttft, usage, copyErr := d.streamCopy(ctx, w, resp, rt.Surface)
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
	} else {
		// Non-streamed: read the body once, tap usage, then write it out.
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		if err != nil {
			res.Err = err.Error()
			return res, err
		}
		// A 200 with a completely empty body is not a valid provider response
		// for any of our three surfaces (all return JSON). It means the
		// upstream died after headers were written. Record it so a truncated
		// call never looks like a clean success in the ledger, and tell the
		// client with a 502 rather than an empty 200.
		if resp.StatusCode == http.StatusOK && len(bytes.TrimSpace(raw)) == 0 {
			res.Status = http.StatusBadGateway
			res.Err = "upstream returned an empty body"
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"type":"upstream_error","message":"upstream returned an empty body","code":502}}`))
			res.Totalms = msPtr(time.Since(start))
			return res, nil
		}
		if u := tapJSONUsage(raw, rt.Surface); u != nil {
			res.Usage = u
		}
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
	resp, res, start, err := d.retryAttempt(ctx, r, rt, body)
	if err != nil {
		return res, err
	}
	return d.commit(ctx, w, resp, rt, res, start)
}

func (d *Dispatcher) retryAttempt(ctx context.Context, r *http.Request, rt Route, body []byte) (*http.Response, Result, time.Time, error) {
	maxAttempts := d.retryAttempts + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	var lastResp *http.Response
	var lastResult Result
	var lastStart time.Time
	for attempt := 0; attempt < maxAttempts; attempt++ {
		resp, result, start, err := d.attempt(ctx, r, rt, body)
		lastResp, lastResult, lastStart = resp, result, start
		if err == nil && (result.Status < 500 && result.Status != http.StatusTooManyRequests) {
			return resp, result, start, nil
		}
		if attempt+1 == maxAttempts || ctx.Err() != nil || (err == nil && !retryableUpstreamStatus(result.Status)) || (err != nil && !retryableUpstreamError(err)) {
			return resp, result, start, err
		}
		if resp != nil {
			drainClose(resp)
		}
		delay := time.Duration(1<<attempt) * time.Second
		if delay > 8*time.Second {
			delay = 8 * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, lastResult, lastStart, ctx.Err()
		case <-timer.C:
		}
	}
	return lastResp, lastResult, lastStart, errors.New("proxy: retry budget exhausted")
}

func retryableUpstreamStatus(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusRequestTimeout || code == http.StatusTooEarly || code >= 500
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
func (d *Dispatcher) ForwardCandidates(ctx context.Context, w http.ResponseWriter, r *http.Request, routes []Route, body []byte) (Route, Result, error) {
	if len(routes) == 0 {
		return Route{}, Result{}, errors.New("proxy: no candidate routes")
	}
	// Try candidates in the resolver's strategy order, skipping cooled endpoints.
	var fallbackRoute *Route
	var fallbackResp *http.Response
	var fallbackResult Result
	var fallbackStart time.Time
	var fallbackTransportErr error
	for i := 0; i < len(routes); i++ {
		rt := routes[i]
		allowed, probe := d.acquireHop(rt, time.Now())
		if !allowed {
			continue
		}
		// If all later candidates are cooling down, hold this eligible route as
		// the final response fallback. This preserves an actual upstream error
		// without making an extra request that circumvents cooldown.
		if i == len(routes)-1 && len(routes) == 1 {
			// A sole eligible endpoint is attempted normally below.
		}
		swapped, _, err := SwapModel(body, rt.Model)
		if err != nil {
			if probe {
				d.releaseProbe(rt)
			}
			return Route{}, Result{}, err
		}
		resp, res, start, err := d.retryAttempt(ctx, r, rt, swapped)
		d.recordHop(rt, cooldownFailure(err, res.Status), time.Now())
		if probe {
			d.releaseProbe(rt)
		}
		if err != nil {
			if fallbackResp != nil {
				drainClose(fallbackResp)
				fallbackResp = nil
			}
			fallbackTransportErr = err
			continue
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
		return rt, res2, err2
	}

	// Preserve the final upstream error response for compatibility, but never
	// issue another request to an endpoint that's cooling down. A stored response
	// is safe to commit: it was obtained by a prior, permitted attempt.
	if fallbackResp != nil {
		result, err := d.commit(ctx, w, fallbackResp, *fallbackRoute, fallbackResult, fallbackStart)
		return *fallbackRoute, result, err
	}
	if fallbackTransportErr != nil {
		return Route{}, Result{}, fallbackTransportErr
	}
	return Route{}, Result{}, errors.New("proxy: all combo endpoints are in five-minute cooldown")
}

// streamCopy copies SSE upstream->client with a Flush per chunk, observing
// usage events without altering a single byte. Returns (ttft, usage, err) where
// err is non-nil when the stream was truncated — the caller records it, since
// the 200 status has already been sent and cannot be changed.
func (d *Dispatcher) streamCopy(ctx context.Context, w http.ResponseWriter, resp *http.Response, surface provider.Surface) (time.Duration, *store.Usage, error) {
	start := time.Now()
	var ttft time.Duration
	var firstByte bool

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
	// re-emit "\n" per line — SSE lines are newline-terminated, which makes this
	// lossless for well-formed streams. Anything unusual (bare \r) is handled by
	// falling back to a raw copy if the scanner errors.
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
		if _, err := w.Write(line); err != nil {
			return ttft, tapper.Usage(), fmt.Errorf("client write: %w", err)
		}
		if _, err := w.Write([]byte("\n")); err != nil {
			return ttft, tapper.Usage(), fmt.Errorf("client write: %w", err)
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
			return ttft, tapper.Usage(), fmt.Errorf("upstream read: %w (scanner: %v)", cerr, err)
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
			return ttft, tapper.Usage(), nil
		}
		return ttft, tapper.Usage(), fmt.Errorf("upstream read: %w", err)
	}
	if !firstByte {
		ttft = time.Since(start)
	}
	return ttft, tapper.Usage(), nil
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
}

func copyClientHeaders(dst, src *http.Request, extraStrip []string) {
	strip := make(map[string]bool, len(stripAlways)+len(extraStrip))
	for k := range stripAlways {
		strip[strings.ToLower(k)] = true
	}
	for _, k := range extraStrip {
		strip[strings.ToLower(k)] = true
	}
	for k, vs := range src.Header {
		if strip[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			dst.Header.Add(k, v)
		}
	}
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
