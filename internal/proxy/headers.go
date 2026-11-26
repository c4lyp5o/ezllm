package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/c4lyp5o/ezllm/internal/bridgeutil"
	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/settings"
	"github.com/c4lyp5o/ezllm/internal/store"
)

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
	// The in-process web bridges' admission secret. A client cannot know it,
	// but a value it sends is still never repeated to any upstream.
	"X-Ezllm-Bridge-Secret":     true,
	provider.BridgeCallerHeader: true,
	// The client's address and the browser's page, as a reverse proxy or the
	// browser added them: they describe the hop to ezllm. Forwarded, they
	// told a provider the caller's real or internal IP and the dashboard URL,
	// and a stale X-Forwarded-For can trip a provider's proxy-spoofing block.
	"X-Forwarded-For":   true,
	"X-Forwarded-Host":  true,
	"X-Forwarded-Proto": true,
	"X-Forwarded-Port":  true,
	"X-Real-Ip":         true,
	"Forwarded":         true,
	"Cf-Connecting-Ip":  true,
	"True-Client-Ip":    true,
	"Referer":           true,
	"Sec-Fetch-Site":    true,
	"Sec-Fetch-Mode":    true,
	"Sec-Fetch-Dest":    true,
	"Sec-Fetch-User":    true,
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
		canonical := http.CanonicalHeaderKey(k)
		if hopByHop[canonical] || canonical == "Set-Cookie" || canonical == "Set-Cookie2" || canonical == "Clear-Site-Data" ||
			canonical == bridgeutil.SlotWaitHeader {
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
// streamErrCause decides what the ledger's err column says about a finished
// stream, and it is a precedence rule, not a last-writer-wins assignment.
//
// A cause recorded earlier in the request is the precise one — "stream
// truncated: unexpected EOF" names what actually happened to the bytes — and an
// error envelope in the body must never replace it with a generic snippet. What
// the envelope DOES add is the upstream's own classification of why, so when one
// is present its cause word is appended once: "… [rate_limited]". That keeps
// both halves of the diagnosis in a single TEXT column.
//
// With no cause yet, the snippet is the whole story: a failover-committed SSE
// error response (a 429 whose reason rides in the first data payload) and a 200
// stream carrying an error object both land here with nothing else to say.
func streamErrCause(existing string, status int, first []byte) string {
	if existing == "" {
		if sseHasErrorEnvelope(first) || status >= 400 {
			return upstreamErrorSnippet(status, first)
		}
		return ""
	}
	if sseHasErrorEnvelope(first) {
		// The full snippet, not just its cause word: the provider's own sentence
		// ("this model is not included in your free usage") is the most precise
		// thing either half carries, and a later reader needs it verbatim.
		if snippet := upstreamErrorSnippet(status, first); !strings.Contains(existing, snippet) {
			return existing + " · " + snippet
		}
	}
	return existing
}

// isSSEMetadata reports an SSE comment or a field other than data.
func isSSEMetadata(line []byte) bool {
	field, _, _ := bytes.Cut(line, []byte(":"))
	switch string(field) {
	case "": // a blank line or a ":" comment
		return true
	case "event", "id", "retry":
		return true
	}
	return false
}

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
	// Same rendering as the ledger's err column, so a log line and a row read
	// identically.
	return upstreamErrorSnippet(resp.StatusCode, peekBody(resp, maxErrPeek))
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

// usageSurface picks the usage-parsing shape for a surface: Anthropic and
// Responses have their own; every other surface (chat and the OpenAI-shaped
// service endpoints) reads as OpenAI. It is NOT the ledger surface — the
// server's store2surface maps every service surface to its own value.
func usageSurface(s provider.Surface) store.Surface {
	switch s {
	case provider.SurfaceAnthropic:
		return store.SurfaceAnthropic
	case provider.SurfaceResponses:
		return store.SurfaceResponses
	default:
		return store.SurfaceChat
	}
}

func (d *Dispatcher) requestPolicy(ctx context.Context) settings.Values {
	if p, ok := settings.From(ctx); ok {
		return p
	}
	p := settings.Defaults()
	p.Retries = d.retryAttempts
	p.AttemptBudget = d.attemptBudget
	if d.dispatchDeadline > 0 {
		p.DispatchDeadline = d.dispatchDeadline.String()
	}
	return p
}

// modelPath makes a model ID safe to place in a URL path. Its slashes stay
// (Cloudflare's @cf/vendor/model and Gemini's models/name use them), but each
// segment is escaped and a "." or ".." segment is encoded, so an ID such as
// "../x?y" cannot climb the provider's path or add a query while the
// account's key is attached.
func modelPath(model string) string {
	segs := strings.Split(model, "/")
	for i, s := range segs {
		switch s {
		case ".", "..":
			segs[i] = strings.ReplaceAll(s, ".", "%2E")
		default:
			segs[i] = url.PathEscape(s)
		}
	}
	return strings.Join(segs, "/")
}
