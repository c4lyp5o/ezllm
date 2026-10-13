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
	Path   string // relative to Account.BaseURL, e.g. "/usage"; if it starts with "http" it is treated as an absolute URL
	// Headers are sent verbatim (e.g. anthropic-version). Auth itself is the
	// adapter's normal path — registration calls PrepareRequest when it can, so
	// quirks like x-opencode-session stay in ONE place.
	NeedsSession bool // true → registration must generate a session id
	// Body, when non-empty, is the request body for POST oracles (Anthropic
	// has no cheap GET that authenticates).
	Body string
	// CalibrationKey: if set, this key is used for the deliberate-bad-key
	// calibration call instead of the generic fakeKey(). Some providers (e.g.
	// Cloudflare) require a well-formed token prefix (cfut_ + 40 hex) to reach
	// the 401 rejection path; a syntactically-invalid key gets a 400 format
	// error which the calibration would misclassify as "inconclusive".
	CalibrationKey string
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

// HostedAuthOracle is an optional adapter interface for adapters whose cheapest
// credential check depends on the ACCOUNT (host-scoped paths, absolute URLs).
// If implemented, registration uses this instead of the parameterless
// AuthOracle() to get the oracle with full account context.
type HostedAuthOracle interface {
	AuthOracleFor(acct Account) AuthProbe
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
	// Live 2026-10-11: space-stationine refuses a whole group with
	// 403 {"error":{"message":"This group does not allow /v1/messages dispatch",
	// "type":"permission_error"}} — a capability gap with none of the words above.
	"does not allow",
}

// bodyHasProtocolHint reports whether an upstream error body names a protocol
// capability gap rather than a credential or request problem. Judged on the
// provider's own words, so a hint never has to guess a vendor's phrasing.
func bodyHasProtocolHint(body string) bool {
	lower := strings.ToLower(body)
	for _, hint := range protocolErrorHints {
		if strings.Contains(lower, strings.ToLower(hint)) {
			return true
		}
	}
	return false
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
	case status == http.StatusUnauthorized:
		return VerdictUnknown, true
	case status == http.StatusForbidden:
		// A 403 is normally a dead credential, but a POLICY refusal that names
		// the protocol is a capability gap, not a key problem. Judging by the
		// provider's own words keeps a sweep from aborting (and recording
		// nothing) on a provider whose group simply blocks one surface.
		if bodyHasProtocolHint(body) {
			return VerdictUnsupported, false
		}
		return VerdictUnknown, true
	case status == http.StatusTooManyRequests:
		return VerdictUnknown, false
	case status == http.StatusNotFound:
		// The surface endpoint itself doesn't exist → this kind can't serve it.
		return VerdictUnsupported, false
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		if bodyHasProtocolHint(body) {
			return VerdictUnsupported, false
		}
		// A 400 we don't recognise: could be a malformed probe request rather
		// than a protocol gap. Recording 0 would be a guess — record nothing.
		return VerdictUnknown, false
	default: // 5xx and anything else
		return VerdictUnknown, false
	}
}
