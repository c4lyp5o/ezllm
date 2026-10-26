package translate

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func TestResponseCodecStrictEnvelope(t *testing.T) {
	for _, tc := range []struct {
		surface provider.Surface
		body    string
	}{
		{provider.SurfaceChat, `{}`}, {provider.SurfaceAnthropic, `{}`}, {provider.SurfaceResponses, `{}`},
		{provider.SurfaceChat, `{"id":"c","choices":[]}`},
		{provider.SurfaceChat, `{"id":"c","choices":[{"message":{"role":"user","content":"x"},"finish_reason":"stop"}]}`},
		{provider.SurfaceResponses, `{"id":"r","status":"in_progress","output":[]}`},
		{provider.SurfaceResponses, `{"id":"r","status":"completed","output":null}`},
		{provider.SurfaceAnthropic, `{"id":"m","type":"message","role":"user","content":[],"stop_reason":"end_turn"}`},
	} {
		_, err := DecodeResponse([]byte(tc.body), tc.surface, CodecOptions{MessagesUsage: UsageInclusive})
		var invalid *InvalidUpstreamResponse
		if !errors.As(err, &invalid) {
			t.Errorf("%s %s -> %v want invalid envelope", tc.surface, tc.body, err)
		}
	}
}
func TestResponseCodecMatrixAndIdentity(t *testing.T) {
	for _, source := range []provider.Surface{provider.SurfaceChat, provider.SurfaceAnthropic, provider.SurfaceResponses} {
		var body string
		switch source {
		case provider.SurfaceChat:
			body = `{"id":"c","created":123,"choices":[{"index":0,"message":{"role":"assistant","content":"hello","tool_calls":[{"id":"call_B","type":"function","function":{"name":"ls","arguments":"{\"path\":\"src\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":7}}`
		case provider.SurfaceAnthropic:
			body = `{"id":"msg_c","type":"message","role":"assistant","content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"call_B","name":"ls","input":{"path":"src"}}],"stop_reason":"tool_use","usage":{"input_tokens":12,"output_tokens":7}}`
		case provider.SurfaceResponses:
			body = `{"id":"resp_c","created_at":123,"status":"completed","output":[{"id":"msg_item","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]},{"id":"fc_A","type":"function_call","call_id":"call_B","name":"ls","arguments":"{\"path\":\"src\"}"}],"usage":{"input_tokens":12,"output_tokens":7}}`
		}
		opts := CodecOptions{MessagesUsage: UsageInclusive}
		ir, err := DecodeResponse([]byte(body), source, opts)
		if err != nil {
			t.Fatal(err)
		}
		if ir.Finish != FinishTools || len(ir.Blocks) != 2 || ir.Blocks[1].CallID != "call_B" {
			t.Fatalf("identity %s %+v", source, ir)
		}
		for _, target := range []provider.Surface{provider.SurfaceChat, provider.SurfaceAnthropic, provider.SurfaceResponses} {
			out, err := EncodeResponse(ir, target, "client-alias", opts)
			if err != nil {
				t.Fatal(err)
			}
			fresh, err := DecodeResponse(out, target, opts)
			if err != nil {
				t.Fatalf("roundtrip %s -> %s: %v (%s)", source, target, err, out)
			}
			if !reflect.DeepEqual(ir.Blocks, fresh.Blocks) || ir.Finish != fresh.Finish || *fresh.Usage.Input != 12 || *fresh.Usage.Output != 7 {
				t.Fatalf("roundtrip lost meaning %s -> %s: %s", source, target, out)
			}
			if !bytes.Contains(out, []byte(`"model":"client-alias"`)) {
				t.Fatalf("alias lost %s", out)
			}
		}
	}
}
func TestResponseCodecFailedIsNotSuccess(t *testing.T) {
	ir, err := DecodeResponse([]byte(`{"id":"r","status":"failed","output":[],"error":{"type":"server_error","message":"boom"},"usage":{"input_tokens":4,"output_tokens":5}}`), provider.SurfaceResponses, CodecOptions{})
	if err != nil || ir.Finish != FinishFailed || ir.Error == nil {
		t.Fatalf("failed %+v %v", ir, err)
	}
	for _, target := range []provider.Surface{provider.SurfaceChat, provider.SurfaceAnthropic, provider.SurfaceResponses} {
		b, err := EncodeResponse(ir, target, "client", CodecOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(b, []byte("boom")) || bytes.Contains(b, []byte(`"finish_reason":"stop"`)) {
			t.Fatalf("failed became success %s", b)
		}
		if target == provider.SurfaceResponses && !bytes.Contains(b, []byte(`"status":"failed"`)) {
			t.Fatal(string(b))
		}
	}
}
func TestResponseCodecStrictObjectArguments(t *testing.T) {
	for _, args := range []string{`not-json`, `null`, `[1]`, `"text"`} {
		ir := &ResponseIR{ID: "r", Finish: FinishTools, Blocks: []Block{{Kind: BlockCall, CallID: "c", Name: "f", Arguments: args}}}
		if _, err := EncodeResponse(ir, provider.SurfaceAnthropic, "client", CodecOptions{}); err == nil {
			t.Errorf("invalid args accepted %s", args)
		}
		if _, err := EncodeResponse(ir, provider.SurfaceAnthropic, "client", CodecOptions{Legacy: true}); err != nil {
			t.Errorf("legacy policy tightened %v", err)
		}
	}
}
func TestResponseCodecUsagePresenceAndConvention(t *testing.T) {
	ir, err := DecodeResponse([]byte(`{"id":"c","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`), provider.SurfaceChat, CodecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []provider.Surface{provider.SurfaceChat, provider.SurfaceAnthropic} {
		b, e := EncodeResponse(ir, s, "client", CodecOptions{})
		if e != nil || bytes.Contains(b, []byte(`"usage"`)) {
			t.Fatalf("fabricated usage %s %v", b, e)
		}
	}
	for _, tc := range []struct {
		raw  string
		c    UsageConvention
		want int
	}{
		{`{"input_tokens":3,"cache_read_input_tokens":5,"cache_creation_input_tokens":2}`, UsageExclusive, 10},
		{`{"input_tokens":10,"cache_read_input_tokens":5,"cache_creation_input_tokens":2}`, UsageInclusive, 10},
		{`{"input_tokens":3,"cache_read_input_tokens":5,"cache_creation_input_tokens":2}`, UsageInclusive, 10},
		{`{"input_tokens":3,"cache_creation":{"ephemeral_5m_input_tokens":2,"ephemeral_1h_input_tokens":4}}`, UsageExclusive, 9},
		{`{"input_tokens":3,"cache_creation_input_tokens":6,"cache_creation":{"ephemeral_5m_input_tokens":2,"ephemeral_1h_input_tokens":4}}`, UsageExclusive, 9},
	} {
		u, err := decodeUsage([]byte(tc.raw), provider.SurfaceAnthropic, CodecOptions{MessagesUsage: tc.c})
		if err != nil || u.Input == nil || *u.Input != tc.want {
			t.Fatalf("normalize %s %s -> %+v %v", tc.c, tc.raw, u, err)
		}
	}
	if _, err := decodeUsage([]byte(`{"input_tokens":1}`), provider.SurfaceAnthropic, CodecOptions{}); err == nil {
		t.Fatal("auto convention guessed")
	}
}
func TestResponseCodecLegacyGoldensAndCausality(t *testing.T) {
	for _, tc := range []struct {
		source, target       provider.Surface
		input, golden, model string
	}{
		{provider.SurfaceChat, provider.SurfaceAnthropic, "openai_response.json", "anthropic_response_golden.json", "claude-3-5-sonnet-20240620"},
		{provider.SurfaceResponses, provider.SurfaceChat, "responses_response.json", "chat_response_golden.json", "daily"},
	} {
		b := fixture(t, tc.input)
		ir, err := DecodeResponse(b, tc.source, CodecOptions{Legacy: true})
		if err != nil {
			t.Fatal(err)
		}
		out, err := EncodeResponse(ir, tc.target, tc.model, CodecOptions{Legacy: true})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out, fixture(t, tc.golden)) {
			t.Fatalf("golden %s\n%s\n%s", tc.input, fixture(t, tc.golden), out)
		}
		var top map[string]any
		if err := json.Unmarshal(b, &top); err != nil {
			t.Fatal(err)
		}
		top["id"] = "mutated_real_input"
		changed, _ := json.Marshal(top)
		fresh, err := DecodeResponse(changed, tc.source, CodecOptions{Legacy: true})
		if err != nil {
			t.Fatal(err)
		}
		alt, err := EncodeResponse(fresh, tc.target, tc.model, CodecOptions{Legacy: true})
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(out, alt) || !bytes.Contains(alt, []byte("mutated_real_input")) {
			t.Fatal("input mutation had no causal effect")
		}
	}
}
func TestResponseCodecErrorEnvelopes(t *testing.T) {
	for _, s := range []provider.Surface{provider.SurfaceChat, provider.SurfaceResponses, provider.SurfaceAnthropic} {
		b := EncodeError(ErrorIR{Type: "rate_limit_error", Message: "upstream's own words", Code: "x"}, s)
		var top map[string]any
		if err := json.Unmarshal(b, &top); err != nil {
			t.Fatal(err)
		}
		if s == provider.SurfaceAnthropic && top["type"] != "error" {
			t.Fatal(string(b))
		}
		if !strings.Contains(string(b), "upstream's own words") {
			t.Fatal(string(b))
		}
	}
}
