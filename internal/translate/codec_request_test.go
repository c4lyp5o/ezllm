package translate

// M2 — surface-neutral request IR + one decoder and one encoder per surface.
// These tests pin the portable contract BEFORE the codecs exist (they must
// fail to compile first, then fail behaviorally, then pass). The legacy
// translators are untouched; the goldens prove the new path agrees with them
// wherever the legacy path is not lossy.

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func dec(t *testing.T, decode func([]byte) (*RequestIR, error), body string) *RequestIR {
	t.Helper()
	ir, err := decode([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return ir
}

func enc(t *testing.T, encode func(*RequestIR, string) ([]byte, error), ir *RequestIR, model string) map[string]any {
	t.Helper()
	b, err := encode(ir, "up/model")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("encoded body is not JSON: %v (%s)", err, b)
	}
	return m
}

func mustJSONEq(t *testing.T, a, b []byte, label string) {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("%s: left is not JSON: %v", label, err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("%s: right is not JSON: %v", label, err)
	}
	if !reflect.DeepEqual(x, y) {
		t.Errorf("%s: bodies differ\n left: %s\nright: %s", label, a, b)
	}
}

// ── decode/encode agree with the legacy translators where legacy is lossless ─

const goldenAnthropicBody = `{
  "model":"claude-x","max_tokens":777,
  "system":"You are tidy.",
  "messages":[
    {"role":"user","content":"list files"},
    {"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"ls","input":{"path":"/a"}}]},
    {"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"a.txt"}]}
  ],
  "tools":[{"name":"ls","description":"list","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}]
}`

func TestNewPathMatchesLegacyAnthropicToChat(t *testing.T) {
	legacy, err := Request([]byte(goldenAnthropicBody), "up/model")
	if err != nil {
		t.Fatal(err)
	}
	ir := dec(t, DecodeMessagesRequest, goldenAnthropicBody)
	fresh, err := EncodeChatRequest(ir, "up/model")
	if err != nil {
		t.Fatal(err)
	}
	mustJSONEq(t, fresh, legacy, "anthropic->chat")
}

const goldenChatBody = `{
  "model":"gpt-x","max_tokens":55,"stream":true,
  "messages":[
    {"role":"system","content":"Be brief."},
    {"role":"user","content":"hi"},
    {"role":"assistant","content":null,"tool_calls":[{"id":"call_9","type":"function","function":{"name":"ls","arguments":"{\"path\":\"/\"}"}}]},
    {"role":"tool","tool_call_id":"call_9","content":"etc"}
  ],
  "tools":[{"type":"function","function":{"name":"ls","description":"list","parameters":{"type":"object"}}}],
  "tool_choice":{"type":"function","function":{"name":"ls"}}
}`

func TestNewPathMatchesLegacyChatToResponses(t *testing.T) {
	legacy, err := ChatToResponses([]byte(goldenChatBody), "up/model")
	if err != nil {
		t.Fatal(err)
	}
	ir := dec(t, DecodeChatRequest, goldenChatBody)
	fresh, err := EncodeResponsesRequest(ir, "up/model")
	if err != nil {
		t.Fatal(err)
	}
	mustJSONEq(t, fresh, legacy, "chat->responses")
}

func TestNewPathMatchesLegacyAnthropicToResponses(t *testing.T) {
	legacy, err := AnthropicToResponses([]byte(goldenAnthropicBody), "up/model")
	if err != nil {
		t.Fatal(err)
	}
	ir := dec(t, DecodeMessagesRequest, goldenAnthropicBody)
	fresh, err := EncodeResponsesRequest(ir, "up/model")
	if err != nil {
		t.Fatal(err)
	}
	mustJSONEq(t, fresh, legacy, "anthropic->responses")
}

// ── IR carries the portable subset ──────────────────────────────────────────

func TestDecodeCarriesParallelToolCallsInOrder(t *testing.T) {
	ir := dec(t, DecodeChatRequest, `{
      "model":"m",
      "messages":[
        {"role":"user","content":"do both"},
        {"role":"assistant","content":null,"tool_calls":[
          {"id":"c1","type":"function","function":{"name":"read","arguments":"{\"a\":1}"}},
          {"id":"c2","type":"function","function":{"name":"write","arguments":"{\"b\":2}"}}
        ]},
        {"role":"tool","tool_call_id":"c1","content":"r1"},
        {"role":"tool","tool_call_id":"c2","content":"r2"}
      ]}`)
	if len(ir.Turns) != 4 {
		t.Fatalf("turns = %d, want 4 (user, assistant, tool, tool)", len(ir.Turns))
	}
	asst := ir.Turns[1]
	if asst.Role != RoleAssistant || len(asst.Blocks) != 2 {
		t.Fatalf("assistant turn = %+v, want two call blocks", asst)
	}
	if asst.Blocks[0].CallID != "c1" || asst.Blocks[0].Name != "read" || asst.Blocks[0].Arguments != `{"a":1}` {
		t.Errorf("call block 0 = %+v", asst.Blocks[0])
	}
	if asst.Blocks[1].CallID != "c2" || asst.Blocks[1].Name != "write" {
		t.Errorf("call block 1 = %+v", asst.Blocks[1])
	}
	if ir.Turns[2].Role != RoleTool || ir.Turns[2].Blocks[0].CallID != "c1" || ir.Turns[2].Blocks[0].Text != "r1" {
		t.Errorf("tool turn 2 = %+v", ir.Turns[2])
	}
	if ir.Turns[3].Blocks[0].CallID != "c2" {
		t.Errorf("tool turn 3 = %+v", ir.Turns[3])
	}
}

func TestAbsentAndZeroControlsStayDistinct(t *testing.T) {
	absent := dec(t, DecodeChatRequest, `{"model":"m","messages":[{"role":"user","content":"x"}]}`)
	zero := dec(t, DecodeChatRequest, `{"model":"m","messages":[{"role":"user","content":"x"}],"temperature":0}`)
	if absent.Temperature != nil {
		t.Error("absent temperature decoded as present")
	}
	if zero.Temperature == nil || *zero.Temperature != 0 {
		t.Error("temperature 0 lost")
	}
	// The distinction must survive encoding.
	outAbsent := enc(t, EncodeChatRequest, absent, "up/model")
	outZero := enc(t, EncodeChatRequest, zero, "up/model")
	if _, has := outAbsent["temperature"]; has {
		t.Error("absent temperature encoded anyway")
	}
	if v, has := outZero["temperature"]; !has || v != 0.0 {
		t.Errorf("encoded temperature = %v, want 0", v)
	}
}

func TestMultiSystemOrderSurvivesToResponses(t *testing.T) {
	// The legacy chat->responses path drops every system message after the
	// first (documented loss). The portable path must keep them, in order.
	ir := dec(t, DecodeChatRequest, `{
      "model":"m",
      "messages":[
        {"role":"system","content":"first"},
        {"role":"user","content":"hi"},
        {"role":"system","content":"second"}
      ]}`)
	out := enc(t, EncodeResponsesRequest, ir, "up/model")
	inst, _ := out["instructions"].(string)
	if inst != "first\n\nsecond" {
		t.Errorf("instructions = %q, want both system turns in order", inst)
	}
}

func TestMaxLimitVariants(t *testing.T) {
	// chat: max_tokens carried; max_completion_tokens carried; both set and
	// disagreeing is a contradiction the IR must refuse to guess.
	ir := dec(t, DecodeChatRequest, `{"model":"m","max_completion_tokens":42,"messages":[{"role":"user","content":"x"}]}`)
	if ir.MaxOutputTokens == nil || *ir.MaxOutputTokens != 42 {
		t.Fatalf("max_completion_tokens lost: %+v", ir.MaxOutputTokens)
	}
	_, err := DecodeChatRequest([]byte(`{"model":"m","max_tokens":100,"max_completion_tokens":200,"messages":[{"role":"user","content":"x"}]}`))
	if err == nil {
		t.Fatal("contradictory limits must be refused, not guessed")
	}
}

func TestReasoningEffortCarriedWhereExpressible(t *testing.T) {
	ir := dec(t, DecodeChatRequest, `{"model":"m","reasoning_effort":"high","messages":[{"role":"user","content":"x"}]}`)
	if ir.ReasoningEffort != "high" {
		t.Fatalf("reasoning_effort lost")
	}
	out := enc(t, EncodeResponsesRequest, ir, "up/model")
	r, _ := out["reasoning"].(map[string]any)
	if r == nil || r["effort"] != "high" {
		t.Errorf("responses reasoning = %v, want effort high", out["reasoning"])
	}
	// The messages wire runs at the upstream's own effort: the field is
	// dropped (as the legacy path drops thinking), never a failed hop.
	m := enc(t, EncodeMessagesRequest, ir, "up/model")
	if _, has := m["reasoning_effort"]; has {
		t.Error("reasoning_effort leaked onto the messages wire")
	}
}

// ── refusals: the honest edge of the portable subset ────────────────────────

func TestStatefulResponsesHistoryRefused(t *testing.T) {
	for name, body := range map[string]string{
		"previous_response_id": `{"model":"m","previous_response_id":"resp_1","input":"hi"}`,
		"conversation":         `{"model":"m","conversation":"conv_1","input":"hi"}`,
		"item_reference":       `{"model":"m","input":[{"type":"item_reference","id":"it_1"}]}`,
	} {
		_, err := DecodeResponsesRequest([]byte(body))
		if err == nil {
			t.Errorf("%s: stateful history must be refused cross-wire", name)
			continue
		}
		// The reason matters: an operator must see WHY, not a generic
		// unknown-field complaint.
		if !strings.Contains(err.Error(), "cannot cross wires") && !strings.Contains(err.Error(), "provider's own history") {
			t.Errorf("%s: refusal lacks the cross-wire reason: %v", name, err)
		}
	}
}

func TestMediaContentSupported(t *testing.T) {
	chatImg := `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`
	if _, err := DecodeChatRequest([]byte(chatImg)); err != nil {
		t.Errorf("chat image content must be supported: %v", err)
	}
	msgImg := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}`
	if _, err := DecodeMessagesRequest([]byte(msgImg)); err != nil {
		t.Errorf("anthropic image content must be supported: %v", err)
	}
	respImg := `{"model":"m","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`
	if _, err := DecodeResponsesRequest([]byte(respImg)); err != nil {
		t.Errorf("responses image content must be supported: %v", err)
	}
}

func TestReasoningReplay(t *testing.T) {
	if _, err := DecodeMessagesRequest([]byte(`{"model":"m","max_tokens":8,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"x"}]}`)); err == nil {
		t.Error("anthropic thinking must be refused cross-wire")
	}
	// A replayed Responses reasoning item is the provider's own (often
	// encrypted) state: it is dropped, and the visible turns cross.
	ir := dec(t, DecodeResponsesRequest, `{"model":"m","input":[{"type":"reasoning","summary":[]},{"type":"message","role":"user","content":"x"}]}`)
	if len(ir.Turns) != 1 || ir.Turns[0].Role != RoleUser {
		t.Errorf("turns = %+v, want only the user turn", ir.Turns)
	}
}

func TestNonFunctionToolsRefused(t *testing.T) {
	if _, err := DecodeChatRequest([]byte(`{"model":"m","messages":[],"tools":[{"type":"web_search","web_search":{}}]}`)); err == nil {
		t.Error("chat non-function tool must be refused")
	}
	if _, err := DecodeResponsesRequest([]byte(`{"model":"m","input":[],"tools":[{"type":"web_search_preview"}]}`)); err == nil {
		t.Error("responses non-function tool must be refused")
	}
}

// An unknown extension is dropped, not refused (the legacy converters do the
// same); the named refusals still refuse.
func TestUnknownTopLevelFieldDropped(t *testing.T) {
	ir := dec(t, DecodeChatRequest, `{"model":"m","messages":[{"role":"user","content":"x"}],"flibberty":1}`)
	if out := enc(t, EncodeChatRequest, ir, "up/model"); out["flibberty"] != nil {
		t.Error("an unknown extension crossed wires")
	}
	if _, err := DecodeChatRequest([]byte(`{"model":"m","messages":[],"functions":[{"name":"f"}]}`)); err == nil {
		t.Error("legacy functions must still be refused")
	}
}

func TestIgnorableFieldsDropSilently(t *testing.T) {
	ir := dec(t, DecodeChatRequest, `{"model":"m","messages":[{"role":"user","content":"x"}],"user":"u1","metadata":{"a":1},"seed":7}`)
	out := enc(t, EncodeChatRequest, ir, "up/model")
	for _, k := range []string{"user", "metadata", "seed"} {
		if _, has := out[k]; has {
			t.Errorf("%s should have been dropped as ignorable", k)
		}
	}
}

func TestToolChoiceVariantsMap(t *testing.T) {
	// chat required -> messages any; chat named -> messages tool.
	ir := dec(t, DecodeChatRequest, `{"model":"m","max_tokens":8,"messages":[],"tool_choice":"required"}`)
	out := enc(t, EncodeMessagesRequest, ir, "up/model")
	if tc, _ := out["tool_choice"].(map[string]any); tc == nil || tc["type"] != "any" {
		t.Errorf("messages tool_choice = %v, want type any", out["tool_choice"])
	}
	ir = dec(t, DecodeChatRequest, `{"model":"m","max_tokens":8,"messages":[],"tool_choice":{"type":"function","function":{"name":"ls"}}}`)
	out = enc(t, EncodeMessagesRequest, ir, "up/model")
	if tc, _ := out["tool_choice"].(map[string]any); tc == nil || tc["type"] != "tool" || tc["name"] != "ls" {
		t.Errorf("messages tool_choice = %v, want named tool ls", out["tool_choice"])
	}
	// responses allowed_tools is not portable.
	if _, err := DecodeResponsesRequest([]byte(`{"model":"m","input":[],"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"ls"}]}}`)); err == nil {
		t.Error("allowed_tools tool_choice must be refused (cannot be expressed honestly)")
	}
}

// ── encoding into the anthropic wire ────────────────────────────────────────

func TestEncodeMessagesCoalescesSystemAndToolResults(t *testing.T) {
	ir := dec(t, DecodeChatRequest, `{
      "model":"m","max_tokens":99,
      "messages":[
        {"role":"system","content":"s1"},
        {"role":"developer","content":"s2"},
        {"role":"user","content":"go"},
        {"role":"assistant","content":null,"tool_calls":[
          {"id":"c1","type":"function","function":{"name":"read","arguments":"{\"a\":1}"}},
          {"id":"c2","type":"function","function":{"name":"write","arguments":"{}"}}
        ]},
        {"role":"tool","tool_call_id":"c1","content":"r1"},
        {"role":"tool","tool_call_id":"c2","content":"r2"}
      ]}`)
	out := enc(t, EncodeMessagesRequest, ir, "up/model")
	sys, _ := out["system"].([]any)
	if len(sys) != 2 || sys[0].(map[string]any)["text"] != "s1" || sys[1].(map[string]any)["text"] != "s2" {
		t.Fatalf("system = %v, want [s1 s2] in order", out["system"])
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3 (user, assistant, one tool-result user message)", len(msgs))
	}
	last := msgs[2].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("tool results must land in a user message, got %v", last["role"])
	}
	blocks, _ := last["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("tool-result message has %d blocks, want 2 (both results, one message)", len(blocks))
	}
	b0 := blocks[0].(map[string]any)
	if b0["type"] != "tool_result" || b0["tool_use_id"] != "c1" || b0["content"] != "r1" {
		t.Errorf("tool_result block 0 = %v", b0)
	}
}

// The Messages wire requires an output limit; a client that named none gets
// the default, and a client's own limit always wins.
func TestEncodeMessagesSuppliesMaxTokens(t *testing.T) {
	ir := dec(t, DecodeChatRequest, `{"model":"m","messages":[{"role":"user","content":"x"}]}`)
	if m := enc(t, EncodeMessagesRequest, ir, "up/model"); m["max_tokens"] != float64(defaultMessagesMaxTokens) {
		t.Errorf("max_tokens = %v, want the default", m["max_tokens"])
	}
	ir = dec(t, DecodeChatRequest, `{"model":"m","max_tokens":77,"messages":[{"role":"user","content":"x"}]}`)
	if m := enc(t, EncodeMessagesRequest, ir, "up/model"); m["max_tokens"] != float64(77) {
		t.Errorf("max_tokens = %v, want the client's 77", m["max_tokens"])
	}
}

func TestEncodeMessagesRefusesNonObjectArguments(t *testing.T) {
	ir := &RequestIR{
		MaxOutputTokens: intPtr(8),
		Turns: []Turn{
			{Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "x"}}},
			{Role: RoleAssistant, Blocks: []Block{{Kind: BlockCall, CallID: "c1", Name: "read", Arguments: `["not","an","object"]`}}},
		},
	}
	if _, err := EncodeMessagesRequest(ir, "up/model"); err == nil {
		t.Error("array-shaped tool arguments must be refused on the messages wire, not sent as {}")
	}
}

// ── determinism ────────────────────────────────────────────────────────────

func TestEncodersAreDeterministic(t *testing.T) {
	for _, tc := range []struct {
		decode func([]byte) (*RequestIR, error)
		encode func(*RequestIR, string) ([]byte, error)
		body   string
	}{
		{DecodeChatRequest, EncodeChatRequest, goldenChatBody},
		{DecodeMessagesRequest, EncodeMessagesRequest, goldenAnthropicBody},
		{DecodeMessagesRequest, EncodeResponsesRequest, goldenAnthropicBody},
		{DecodeChatRequest, EncodeResponsesRequest, goldenChatBody},
	} {
		ir := dec(t, tc.decode, tc.body)
		a, err := tc.encode(ir, "up/model")
		if err != nil {
			t.Fatal(err)
		}
		b, err := tc.encode(ir, "up/model")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Error("encoding is not deterministic — upstream prefix caches will break")
		}
	}
}

func intPtr(i int) *int { return &i }
