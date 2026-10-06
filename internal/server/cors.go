package server

// CORS for the /v1 inference surface only.
//
// /v1/* is meant to be reached by third-party clients — Open WebUI, playgrounds,
// LlamaIndex.Copilot and other browser-based tools — which a same-origin-only
// server silently breaks (the browser blocks the preflight, the user sees a
// network error and blames the router).
//
// This does not weaken anything: ezllm has no ambient authority. The credential
// is a bearer token the client puts in a header, never a cookie, so a foreign
// origin cannot make an authenticated call it could not already make from a
// terminal.
//
// /admin/* is deliberately EXCLUDED. The dashboard keeps the admin token in
// localStorage, and a permissive grant there would let any page the admin
// happens to visit read that token cross-origin — which is a real escalation,
// since that token is also the inference credential. Same-origin is enough for
// the dashboard, so nothing is sacrificed.
//
// Allowlist is opt-in via EZLLM_CORS_ORIGINS (comma-separated). With no
// allowlist the request Origin is echoed back concretely rather than "*", so
// credentialed requests stay valid under the CORS spec.
//
// Placement: withCORS sits OUTSIDE the mux (see Handler), because ServeMux
// answers OPTIONS with 405 whenever a method-specific pattern matches the path
// — so preflight handling has to happen before dispatch. It only ever answers
// OPTIONS for /v1/*; every other path falls through to the normal stack.

// corsPreflightHeaders lists what a client may send. x-opencode-* is included on
// purpose: session-sticky providers require those headers, and a browser aborts
// the whole call if the preflight does not recognise them.
var corsPreflightHeaders = "Authorization, Content-Type, Accept, anthropic-version, " +
	"anthropic-dangerous-direct-browser-access, x-api-key, x-opencode-session, x-opencode-request"

// corsExposedHeaders lets browser SDKs read the headers we set on responses.
const corsExposedHeaders = "X-Request-Id, X-Ezllm-Client, Retry-After"

// corsAllows reports whether origin may be granted access. An empty allowlist
// means "any origin", which is the default.
func corsAllows(allowed []string, origin string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		if a == "*" || a == origin {
			return true
		}
	}
	return false
}
