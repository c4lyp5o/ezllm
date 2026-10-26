package provider

import "strings"

// ErrorCause classifies an upstream error into a stable semantic cause from
// the provider's own words, falling back to the HTTP status. It exists so a
// ledger query can answer "what is breaking my calls" without a human
// reading vendor prose: `err LIKE '%[rate_limited]%'` is a diagnosis.
//
// Every phrase below is drawn from a REAL body observed in the ledger or in
// live probes (see the ezllm skill) — never invented. New providers teach
// this table, they don't get a new mechanism.
//
// The cause is embedded in the ledger err column as `[<cause>]` by the
// proxy's upstreamErrorSnippet; the upstream's own words always follow it.
func ErrorCause(status int, body []byte) string {
	s := strings.ToLower(string(body))

	// ── phrase table: specific before generic ──
	switch {
	case strings.Contains(s, "error code: 1010") || strings.Contains(s, "browser signature"):
		return "automation_gate" // Cloudflare ban on library-style UAs (opencode)
	case strings.Contains(s, "endpoint is unavailable") || strings.Contains(s, "upstream request failed"):
		return "upstream_capacity" // opencode's edge reporting its own backend
	case strings.Contains(s, "reasoning_effort are not supported") || strings.Contains(s, "use /v1/responses"):
		return "chat_tools_unsupported" // tokenrouter gpt-6.1-sol class: tools+reasoning on chat
	case strings.Contains(s, "modelprotocolunsupported") || strings.Contains(s, "not supported for format") ||
		strings.Contains(s, "unsupported_api_for_model") ||
		strings.Contains(s, "does not allow"):
		return "protocol_unsupported" // surface capability gap (opencode-go, Copilot /responses, group-403s)
	case strings.Contains(s, "insufficient_quota") || strings.Contains(s, "credit_balance_exhausted") ||
		strings.Contains(s, "billing") || strings.Contains(s, "quota"):
		return "quota_exhausted"
	case strings.Contains(s, "you have reached the request limit") || strings.Contains(s, "rate limit") ||
		strings.Contains(s, "rate_limit") || strings.Contains(s, "too many requests") ||
		strings.Contains(s, "requests per"):
		return "rate_limited"
	case strings.Contains(s, "context length") || strings.Contains(s, "maximum context") ||
		strings.Contains(s, "prompt is too long"):
		return "context_too_long"
	case strings.Contains(s, "invalid_api_key") || strings.Contains(s, "invalid x-api-key") ||
		strings.Contains(s, "authentication_error") || strings.Contains(s, "incorrect api key") ||
		strings.Contains(s, "invalid api key"):
		return "bad_credentials"
	case strings.Contains(s, "permission"):
		return "permission_denied"
	case strings.Contains(s, "content_policy") || strings.Contains(s, "content filter") ||
		strings.Contains(s, "content_filter"):
		return "content_filter"
	case strings.Contains(s, "overloaded"):
		return "overloaded"
	}

	// ── status table when the body says nothing we know ──
	switch {
	case status == 400 || status == 422:
		return "bad_request"
	case status == 401:
		return "bad_credentials"
	case status == 403:
		return "permission_denied"
	case status == 404:
		return "not_found"
	case status == 408:
		return "timeout"
	case status == 413:
		return "too_large"
	case status == 429:
		return "rate_limited"
	case status >= 500:
		return "provider_error"
	default:
		return "http_status_error"
	}
}
