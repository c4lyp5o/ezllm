package claudebridge

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Native error vocabulary — the codes the CLI reports on its assistant/result
// frames. Mapped exactly as the official plugin's NATIVE_ERROR_STATUS does, so a
// plan limit reaches the client as 429 and a login problem as 401 instead of one
// undifferentiated 500.
const (
	codeRateLimit = "rate_limit"
	codeBilling   = "billing_error"
	codeAuth      = "authentication_failed"
	codeOverload  = "overloaded"
	codeServer    = "server_error"
)

var (
	reLimit = regexp.MustCompile(`(?i)(usage limit|session limit|plan limit|rate limit|temporarily limiting)`)
	reBill  = regexp.MustCompile(`(?i)(billing|payment required|out of credits)`)
	reAuth  = regexp.MustCompile(`(?i)(logged out|log.?in|authentication|oauth)`)
	reOut   = regexp.MustCompile(`(?i)(output token maximum|max_output_tokens|prompt is too long)`)
)

// MapNativeError returns the HTTP status and Anthropic error type for a native
// failure. The native error code wins when present; otherwise the CLI's own words
// decide. Nothing is rewritten or hidden.
func MapNativeError(code, text string) (int, string) {
	switch code {
	case codeRateLimit:
		return 429, "rate_limit_error"
	case codeBilling:
		return 402, "billing_error"
	case codeAuth:
		return 401, "authentication_error"
	case codeOverload:
		return 529, "overloaded_error"
	case codeServer:
		return 503, "api_error"
	case "max_output_tokens":
		// The client's own ceiling, not a bridge or plan failure: 400 so it raises
		// max_tokens instead of retrying (mandatory-thinking models need headroom).
		return 400, "invalid_request_error"
	}
	switch {
	case reOut.MatchString(text):
		return 400, "invalid_request_error"
	case reLimit.MatchString(text):
		return 429, "rate_limit_error"
	case reBill.MatchString(text):
		return 402, "billing_error"
	case reAuth.MatchString(text):
		return 401, "authentication_error"
	}
	return 502, "api_error"
}

// ErrorEnvelope is the Anthropic wire error shape, with the machine-readable cause
// kept in `code` per the repository convention (refusals name their reason).
func ErrorEnvelope(errType, message, code string) []byte {
	if errType == "" {
		errType = "api_error"
	}
	if code == "" {
		code = strings.ToLower(strings.ReplaceAll(errType, "_error", ""))
	}
	b, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": message, "code": code},
	})
	return b
}
