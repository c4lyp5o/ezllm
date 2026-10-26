package provider

import "testing"

// Every case is a body that actually reached ezllm (ledger or live probe);
// the classification must stay stable so historical LIKE queries keep working.
func TestErrorCause(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{
			"opencode 429 endpoint unavailable",
			429, `{"error":{"type":"server_error","message":"Upstream request failed: Endpoint is unavailable."}}`,
			"upstream_capacity",
		},
		{
			// opencode's edge wraps a REQUEST-shape fault in its own
			// "Upstream request failed:" envelope and types it server_error.
			// The envelope must not win: this is our body being wrong, and
			// calling it capacity hides the bug behind "upstream was busy".
			"opencode-edge missing required parameter",
			400, `{"error":{"param":"messages[347].content","type":"invalid_request_error","message":"Upstream request failed: [missing_required_parameter] Missing required parameter: 'messages[347].content'."}}`,
			"bad_request",
		},
		{
			"opencode-edge invalid params (model adapter)",
			400, `{"error":{"type":"server_error","message":"Upstream request failed: [bad_request_error] invalid params, model \"MiniMax-M3.1-Flash\" requires adapter …"}}`,
			"bad_request",
		},
		{
			"opencode-edge unsupported reasoning value",
			400, `{"error":{"param":"reasoning_effort","type":"invalid_request_error","message":"Upstream request failed: [unsupported_value] Unsupported value: 'reasoning_effort'."}}`,
			"bad_request",
		},
		{
			"opencode cloudflare browser ban",
			403, `<html><body>error code: 1010</body></html>`,
			"automation_gate",
		},
		{
			"tokenrouter request limit",
			429, `{"error":{"code":"","message":"You have reached the request limit: Maximum 5 requests within 1 minutes. (request id: x)","type":"api_error"}}`,
			"rate_limited",
		},
		{
			"gpt-6.1-sol tools+reasoning on chat",
			400, `{"error":{"message":"Function tools with reasoning_effort are not supported for gpt-6.1-sol in /v1/chat/completions. To use function tools, use /v1/responses or set reasoning_effort to 'none'.","type":"invalid_request_error"}}`,
			"chat_tools_unsupported",
		},
		{
			"opencode protocol refusal",
			400, `{"error":{"type":"ModelProtocolUnsupported","message":"Model does not support this protocol"}}`,
			"protocol_unsupported",
		},
		{
			"group-level surface block",
			403, `{"error":{"message":"This group does not allow /v1/messages dispatch","type":"permission_error"}}`,
			// "does not allow" is the capability-gap phrase; it must win over
			// the generic permission word.
			"protocol_unsupported",
		},
		{
			"openai quota exhausted",
			429, `{"error":{"code":"insufficient_quota","message":"You exceeded your current quota, please check your plan and billing details."}}`,
			"quota_exhausted",
		},
		{
			"anthropic bad key",
			401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
			"bad_credentials",
		},
		{
			"context too long",
			400, `{"error":{"message":"This model's maximum context length is 128000 tokens. However, you requested 200000 tokens.","type":"invalid_request_error"}}`,
			"context_too_long",
		},
		{"plain 500 with unknown words", 500, `{"error":{"message":"internal hiccup"}}`, "provider_error"},
		{"empty body 400", 400, ``, "bad_request"},
		{"empty body 429", 429, ``, "rate_limited"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ErrorCause(tc.status, []byte(tc.body)); got != tc.want {
				t.Errorf("ErrorCause(%d, …) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}
