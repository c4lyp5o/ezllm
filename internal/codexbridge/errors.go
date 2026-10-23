package codexbridge

import (
	"encoding/json"
	"strings"
)

// NativeError is a failure with the status and OpenAI error type the client
// receives. Message carries the CLI's own words; nothing is rewritten.
type NativeError struct {
	Status  int
	Type    string
	Code    string
	Message string
}

func (e *NativeError) Error() string { return e.Message }

// turnError is the app-server's TurnError: a message plus codexErrorInfo,
// either a bare string ("usageLimitExceeded") or a one-key object
// ({"httpConnectionFailed":{"httpStatusCode":502}}).
type turnError struct {
	Message           string          `json:"message"`
	CodexErrorInfo    json.RawMessage `json:"codexErrorInfo"`
	AdditionalDetails string          `json:"additionalDetails"`
}

// errorKind returns codexErrorInfo's discriminator.
func (t turnError) errorKind() string {
	var s string
	if err := json.Unmarshal(t.CodexErrorInfo, &s); err == nil {
		return s
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(t.CodexErrorInfo, &obj); err == nil {
		for k := range obj {
			return k
		}
	}
	return ""
}

// MapTurnError classifies a failed turn. The codexErrorInfo enum is from the
// 0.163.0 app-server schema; a plan or window limit must reach the router as
// 429 (so a combo skips the hop) and a dead login as 401.
func MapTurnError(t turnError) *NativeError {
	msg := strings.TrimSpace(t.Message)
	if d := strings.TrimSpace(t.AdditionalDetails); d != "" {
		msg += " — " + d
	}
	if msg == "" {
		msg = "turn failed without a message"
	}
	msg = "codex-bridge: " + msg
	kind := t.errorKind()
	switch kind {
	case "usageLimitExceeded", "rateLimitExceeded", "sessionBudgetExceeded":
		return &NativeError{Status: 429, Type: "rate_limit_error", Code: "rate_limit", Message: msg}
	case "unauthorized":
		return &NativeError{Status: 401, Type: "authentication_error", Code: "unauthorized", Message: msg}
	case "contextWindowExceeded":
		return &NativeError{Status: 400, Type: "invalid_request_error", Code: "context_length_exceeded", Message: msg}
	case "badRequest":
		return &NativeError{Status: 400, Type: "invalid_request_error", Code: "invalid_request", Message: msg}
	case "cyberPolicy", "misalignmentPolicyViolation":
		return &NativeError{Status: 400, Type: "invalid_request_error", Code: "content_policy", Message: msg}
	case "serverOverloaded", "flexUnavailable":
		return &NativeError{Status: 503, Type: "api_error", Code: "overloaded", Message: msg}
	}
	return &NativeError{Status: 502, Type: "api_error", Code: "native_error", Message: msg}
}

// ErrorEnvelope is the OpenAI wire error shape.
func ErrorEnvelope(errType, message, code string) []byte {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"type": errType, "message": message, "code": code}})
	return b
}
