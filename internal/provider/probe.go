package provider

import (
	"net/http"
	"strings"
)

// Auth probing — the M3 key-test design.
//
// Live probing on 2026-10-06 proved two things that shape this file:
//
//  1. GET /v1/models on opencode-go is UNAUTHENTICATED. It returns 200 plus the
//     full 36-model catalog for an empty key, "not-a-key", even "sk-". Any key
//     test using it as the auth gate accepts garbage. (ssn-gpt's /models does
//     401 — auth behaviour is per-provider, so it must be discovered, never
//     assumed.)
//
//  2. The cheapest credential proof is GET /v1/usage: 401 on a bad key, 200 on
//     a good one, no session header, ~0.75s, ZERO tokens spent.
//
// So each adapter declares (a) which endpoint proves auth, and (b) whether its
// catalog endpoint is trustworthy as proof. The registration orchestrator runs
// the oracle as the gate; the catalog is recorded but never trusted on its own.

// AuthProbe is the cheapest call that fails on a bad credential for this kind.
type AuthProbe struct {
	Method string // GET / POST
	Path   string // relative to Account.BaseURL, e.g. "/usage"
	// Headers are sent verbatim (e.g. anthropic-version). Auth itself is the
	// adapter's normal path — registration calls PrepareRequest when it can, so
	// quirks like x-opencode-session stay in ONE place.
	NeedsSession bool // true → registration must generate a session id
	// Body, when non-empty, is the request body for POST oracles (Anthropic
	// has no cheap GET that authenticates).
	Body string
}

// AuthOracle returns the cheapest call that proves the credential is valid.
// The registration flow treats a 401/403 here as an invalid key (fail fast — no
// retry; on these providers 403 means bad credentials, not "slow down").
func AuthOracle(a Adapter) AuthProbe { return a.AuthOracle() }

// AuthRejectStatuser is an optional adapter interface. Adapters whose upstream
// signals a bad credential with a non-401/403 status (Gemini: 400
// INVALID_ARGUMENT) list those statuses here. Adapters that do not implement it
// keep the 401/403 default, so no other provider's behavior changes.
type AuthRejectStatuser interface {
	AuthRejectStatuses() []int
}

// CatalogIsPublic reports whether ListModels succeeds without a valid key.
// true → the catalog step is informational only and CANNOT pass a key test.
func CatalogIsPublic(a Adapter) bool { return a.CatalogIsPublic() }

// ProbeVerdict classifies one protocol-probe response. NULL (unknown) is a
// first-class result: "we could not ask" must never be recorded as
// "unsupported", or a working model would be pushed out of routing.
type ProbeVerdict int

const (
	VerdictUnknown     ProbeVerdict = iota // 429 / 5xx / timeout → proto_*=NULL
	VerdictSupported                       // 200 → proto_*=1
	VerdictUnsupported                     // protocol/format error → proto_*=0
)

// ProtocolErrorTypes are the upstream error type strings that mean "this model
// does not speak this protocol" rather than "your request was wrong".
// Verified live: /messages and /responses on opencode-go both return
// ModelProtocolUnsupported; a wrong-path probe surfaced "not supported for
// format openai".
var protocolErrorHints = []string{
	"ModelProtocolUnsupported",
	"protocol",
	"not supported for format",
	"not supported",
}

// ClassifyProbeResult maps one upstream response to a verdict.
//   - 2xx → supported
//   - 400/404 carrying a protocol-shaped error → unsupported
//   - 401/403 → handled by the caller (abort the probe: the key went bad)
//   - 429/5xx → unknown
//
// authFailure is true when the status means the credential died mid-probe; the
// caller must stop probing entirely rather than record a misleading 0.
func ClassifyProbeResult(status int, body string) (v ProbeVerdict, authFailure bool) {
	switch {
	case status >= 200 && status < 300:
		return VerdictSupported, false
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return VerdictUnknown, true
	case status == http.StatusTooManyRequests:
		return VerdictUnknown, false
	case status == http.StatusNotFound:
		// The surface endpoint itself doesn't exist → this kind can't serve it.
		return VerdictUnsupported, false
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		lower := strings.ToLower(body)
		for _, hint := range protocolErrorHints {
			if strings.Contains(lower, strings.ToLower(hint)) {
				return VerdictUnsupported, false
			}
		}
		// A 400 we don't recognise: could be a malformed probe request rather
		// than a protocol gap. Recording 0 would be a guess — record nothing.
		return VerdictUnknown, false
	default: // 5xx and anything else
		return VerdictUnknown, false
	}
}
