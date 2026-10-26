package provider

import "testing"

// A thinking model that demands the reasoning of every earlier assistant turn
// cannot serve a conversation another model started. That is a fact about the
// HOP and the HISTORY, not about the body we sent, so it needs its own cause —
// under bad_request the operator sees "my request is malformed" and never learns
// that a mixed-family combo is what broke the run.
//
// Both bodies are real (ledger, 2026-10-24..25), and both were classified
// bad_request before this cause existed.
func TestErrorCauseReasoningEchoRequired(t *testing.T) {
	cases := []struct{ name, body string }{
		{
			"deepseek thinking-mode echo",
			`{"error":{"message":"The reasoning_content in the thinking mode must be passed back to the API.","type":"invalid_request_error"}}`,
		},
		{
			"missing reasoning for an earlier turn",
			`{"error":{"message":"{\"error\":{\"message\":\"Missing required parameter: 'messages[2].reasoning_content'. This model needs the reasoning of every earlier assistant turn.\"}}","type":"bad_request"}}`,
		},
	}
	for _, c := range cases {
		if got := ErrorCause(400, []byte(c.body)); got != "reasoning_echo_required" {
			t.Errorf("%s: ErrorCause = %q, want reasoning_echo_required", c.name, got)
		}
	}
}
