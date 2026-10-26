package agybridge

import (
	"encoding/json"
	"regexp"
	"strings"
)

// NativeError is a failure with the status and OpenAI error type the client
// receives. Message carries the CLI's own words, minus any URL (see mapError).
type NativeError struct {
	Status  int
	Type    string
	Code    string
	Message string
}

func (e *NativeError) Error() string { return e.Message }

var urlPattern = regexp.MustCompile(`https?://\S+`)

// mapError classifies a turn that ended in status ERROR. agy reports failures
// as prose, not codes, so this matches the phrases measured on 1.4.7 and the
// ones its docs name. A plan or quota limit must reach the router as 429 (so a
// combo skips the hop) and a login the operator must fix as 401/403.
//
// URLs are withheld: the measured eligibility error carries a single-use
// accounts.google.com sign-in continuation, which must not travel to a client
// or into the ledger. The operator gets the link by running agy on the host.
func mapError(status, msg string) *NativeError {
	text := strings.TrimSpace(urlPattern.ReplaceAllString(msg, "[link withheld: run agy on the host]"))
	if text == "" {
		text = "turn ended with status " + status
	}
	text = "agy-bridge: " + text
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "not eligible") || strings.Contains(low, "eligibility check"):
		return &NativeError{Status: 403, Type: "permission_error", Code: "account_ineligible", Message: text}
	case strings.Contains(low, "authentication required") || strings.Contains(low, "unauthenticated") ||
		strings.Contains(low, "not signed in") || strings.Contains(low, "sign in to"):
		return &NativeError{Status: 401, Type: "authentication_error", Code: "login_required", Message: text}
	case strings.Contains(low, "resource_exhausted") || strings.Contains(low, "resource exhausted") ||
		strings.Contains(low, "quota") || strings.Contains(low, "rate limit") || strings.Contains(low, "credits"):
		return &NativeError{Status: 429, Type: "rate_limit_error", Code: "rate_limit", Message: text}
	case strings.Contains(low, "unknown model") || strings.Contains(low, "model not found") ||
		strings.Contains(low, "invalid model") || strings.Contains(low, "unsupported model"):
		return &NativeError{Status: 400, Type: "invalid_request_error", Code: "unknown_model", Message: text}
	}
	return &NativeError{Status: 502, Type: "api_error", Code: "native_error", Message: text}
}

// ErrorEnvelope is the OpenAI wire error shape.
func ErrorEnvelope(errType, message, code string) []byte {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"type": errType, "message": message, "code": code}})
	return b
}
