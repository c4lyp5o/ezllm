package claudebridge

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func frame(t *testing.T, typ string, event map[string]any) Frame {
	t.Helper()
	if event == nil {
		f, err := ParseFrame([]byte(`{"type":"` + typ + `"}`))
		if err != nil {
			t.Fatalf("ParseFrame: %v", err)
		}
		return f
	}
	raw, _ := json.Marshal(map[string]any{"type": "stream_event", "event": event})
	f, err := ParseFrame(raw)
	if err != nil {
		t.Fatalf("ParseFrame: %v", err)
	}
	return f
}

// Native's stream events ARE Anthropic events; the bridge must forward them byte
// for byte rather than re-encoding text through a second wire.
func TestStreamWriterForwardsNativeEventsVerbatim(t *testing.T) {
	var buf bytes.Buffer
	sw := &StreamWriter{W: &buf}
	start := map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_01ABC", "model": "claude-haiku-5-5",
		"usage": map[string]any{"input_tokens": 2, "output_tokens": 0,
			"cache_read_input_tokens": 8609, "cache_creation_input_tokens": 11650},
	}}
	events := []map[string]any{
		start,
		{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}},
		{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "OK"}},
		{"type": "content_block_stop", "index": 0},
		{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"},
			"usage": map[string]any{"output_tokens": 4}},
		{"type": "message_stop"},
	}
	for _, ev := range events {
		if err := sw.Handle(frame(t, "stream_event", ev)); err != nil {
			t.Fatalf("Handle: %v", err)
		}
	}
	out := buf.String()
	for _, want := range []string{"event: message_start", "event: content_block_delta", "event: message_stop",
		"cache_read_input_tokens", `"text":"OK"`, "msg_01ABC"} {
		if !strings.Contains(out, want) {
			t.Errorf("stream output missing %q\n%s", want, out)
		}
	}
	if err := sw.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if strings.Count(buf.String(), "event: message_stop") != 1 {
		t.Errorf("a completed stream must not gain a second message_stop:\n%s", buf.String())
	}
	if sw.usage.OutputTokens != 4 || sw.usage.CacheReadInputTokens != 8609 {
		t.Errorf("usage not captured: %+v", sw.usage)
	}
}

// A native process that dies mid-stream must still leave the client with a
// well-formed terminal pair, not a stream that simply stops.
func TestFinishSynthesizesTerminalPairWhenNativeDiesEarly(t *testing.T) {
	var buf bytes.Buffer
	sw := &StreamWriter{W: &buf}
	_ = sw.Handle(frame(t, "stream_event", map[string]any{
		"type": "message_start", "message": map[string]any{"id": "msg_X", "model": "claude-haiku-5-5"}}))
	_ = sw.Handle(frame(t, "stream_event", map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": "partial"}}))
	if err := sw.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "event: message_delta") || !strings.Contains(out, "event: message_stop") {
		t.Fatalf("Finish must synthesize the terminal pair:\n%s", out)
	}
	if !strings.Contains(out, "partial") {
		t.Errorf("emitted text lost:\n%s", out)
	}
}

func TestMessageAssemblesContentAndUsage(t *testing.T) {
	sw := &StreamWriter{}
	_ = sw.Handle(frame(t, "stream_event", map[string]any{
		"type": "message_start", "message": map[string]any{"id": "msg_Y", "model": "claude-haiku-5-5"}}))
	_ = sw.Handle(frame(t, "stream_event", map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": "Hello"}}))
	_ = sw.Handle(frame(t, "stream_event", map[string]any{
		"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"},
		"usage": map[string]any{"input_tokens": 170, "output_tokens": 69}}))
	msg := sw.Message("claude-opus-5-5")
	raw, _ := json.Marshal(msg)
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	if got["id"] != "msg_Y" || got["role"] != "assistant" {
		t.Errorf("message identity wrong: %s", raw)
	}
	if got["model"] != "claude-haiku-5-5" {
		t.Errorf("model must come from native's message_start, got %v", got["model"])
	}
	if got["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v", got["stop_reason"])
	}
	usage, _ := got["usage"].(map[string]any)
	if usage["input_tokens"] != float64(170) || usage["output_tokens"] != float64(69) {
		t.Errorf("usage = %v", usage)
	}
	if content, _ := got["content"].([]any); len(content) != 1 {
		t.Errorf("content = %v", got["content"])
	}
}

func TestMapNativeError(t *testing.T) {
	cases := []struct {
		code, text string
		status     int
		errType    string
	}{
		{"rate_limit", "whatever", 429, "rate_limit_error"},
		{"billing_error", "", 402, "billing_error"},
		{"authentication_failed", "", 401, "authentication_error"},
		{"overloaded", "", 529, "overloaded_error"},
		{"server_error", "", 503, "api_error"},
		{"", "API Error: Server is temporarily limiting requests (not your usage limit)", 429, "rate_limit_error"},
		{"", "You've hit your session limit", 429, "rate_limit_error"},
		{"", "please log in again", 401, "authentication_error"},
		// The client's own ceiling: a 400 it can fix, never a retryable 5xx.
		{"max_output_tokens", "API Error: Claude's response exceeded the 32 output token maximum", 400, "invalid_request_error"},
		{"", "something exploded", 502, "api_error"},
	}
	for _, c := range cases {
		status, errType := MapNativeError(c.code, c.text)
		if status != c.status || errType != c.errType {
			t.Errorf("MapNativeError(%q, %q) = %d/%s, want %d/%s", c.code, c.text, status, errType, c.status, c.errType)
		}
	}
}

func TestErrorEnvelopeNamesItsReason(t *testing.T) {
	var got map[string]any
	if err := json.Unmarshal(ErrorEnvelope("invalid_request_error", "msg", "chat_tools_unsupported"), &got); err != nil {
		t.Fatalf("envelope is not JSON: %v", err)
	}
	if got["type"] != "error" {
		t.Errorf("outer type = %v", got["type"])
	}
	inner, _ := got["error"].(map[string]any)
	if inner["code"] != "chat_tools_unsupported" || inner["type"] != "invalid_request_error" {
		t.Errorf("inner = %v", inner)
	}
}

// The CLI gets an allowlisted environment: what it needs to start and reach
// the network, plus the operator's own CLI tuning — never ezllm's secrets or
// any credential that would replace the subscription login.
func TestChildEnvIsAnAllowlist(t *testing.T) {
	for k, v := range map[string]string{
		"PATH": "/usr/bin", "USER": "ezllm", "LOGNAME": "ezllm", "LC_ALL": "C.UTF-8",
		"HTTPS_PROXY": "http://proxy:3128", "SSL_CERT_FILE": "/etc/ssl/cert.pem",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "DISABLE_TELEMETRY": "1",
		"EZLLM_MASTER_KEY": "master-secret", "EZLLM_CLAUDE_BRIDGE_TOKEN_FILE": "/data/t",
		"GITHUB_TOKEN": "ghp_x", "AWS_SECRET_ACCESS_KEY": "aws",
		"ANTHROPIC_API_KEY": "sk-ant", "ANTHROPIC_BASE_URL": "http://evil",
		"CLAUDE_CODE_USE_BEDROCK": "1", "CLAUDE_CONFIG_DIR": "/elsewhere",
	} {
		t.Setenv(k, v)
	}
	r := &Runner{ConfigDir: "/claude-sub"}
	got := map[string]string{}
	for _, kv := range r.childEnv("/tmp/run", 0) {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	for _, k := range []string{"PATH", "USER", "LOGNAME", "LC_ALL", "HTTPS_PROXY", "SSL_CERT_FILE",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "DISABLE_TELEMETRY"} {
		if _, ok := got[k]; !ok {
			t.Errorf("%s was not passed to the CLI", k)
		}
	}
	for _, k := range []string{"EZLLM_MASTER_KEY", "EZLLM_CLAUDE_BRIDGE_TOKEN_FILE", "GITHUB_TOKEN",
		"AWS_SECRET_ACCESS_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_USE_BEDROCK"} {
		if v, ok := got[k]; ok {
			t.Errorf("%s=%q leaked into the CLI environment", k, v)
		}
	}
	if got["CLAUDE_CONFIG_DIR"] != "/claude-sub" || got["HOME"] != "/tmp/run" {
		t.Errorf("forced vars wrong: CLAUDE_CONFIG_DIR=%q HOME=%q", got["CLAUDE_CONFIG_DIR"], got["HOME"])
	}
}
