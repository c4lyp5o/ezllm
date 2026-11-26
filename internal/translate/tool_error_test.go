package translate

import (
	"io"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

const toolErrorBody = `{"model":"m","max_tokens":64,"messages":[
 {"role":"user","content":"run it"},
 {"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"bash","input":{"cmd":"false"}}]},
 {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"content":""}]}]}`

// A failed tool with no output must not read as a success with empty output,
// on any translator that takes an Anthropic request.
func TestToolErrorSurvivesTranslation(t *testing.T) {
	legacy, err := Request([]byte(toolErrorBody), "up")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(legacy), "[tool error]") {
		t.Errorf("legacy chat translation dropped is_error: %s", legacy)
	}
	if !strings.Contains(string(legacy), `"content":null`) {
		t.Errorf("tool-call-only assistant turn should send content null: %s", legacy)
	}
	ir, err := DecodeMessagesRequest([]byte(toolErrorBody))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, turn := range ir.Turns {
		for _, b := range turn.Blocks {
			if b.Kind == BlockResult && strings.Contains(b.Text, "[tool error]") {
				found = true
			}
		}
	}
	if !found {
		t.Error("universal decoder dropped is_error")
	}
	resp, err := AnthropicToResponses([]byte(toolErrorBody), "up")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resp), "[tool error]") {
		t.Errorf("responses translation dropped is_error: %s", resp)
	}
}

// An Anthropic image reaches a Responses hop as input_image inside a user
// message; it used to be dropped.
func TestAnthropicImageReachesResponses(t *testing.T) {
	body := `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"what is this"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}`
	out, err := AnthropicToResponses([]byte(body), "up")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}`) {
		t.Fatalf("image missing or wrongly shaped: %s", out)
	}
}

// A chat stream with text, then a tool call, then more text translates to an
// Anthropic stream; the trailing text used to abort it.
func TestChatStreamTextAfterToolCall(t *testing.T) {
	in := `data: {"id":"c1","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"Let me check."}}]}

data: {"id":"c1","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}}]}

data: {"id":"c1","created":1,"model":"m","choices":[{"index":0,"delta":{"content":" Done."}}]}

data: {"id":"c1","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	r := NewStreamReader(strings.NewReader(in), provider.SurfaceChat, provider.SurfaceAnthropic, "daily", CodecOptions{})
	out, _ := io.ReadAll(r)
	s := string(out)
	if strings.Contains(s, "interleaved") || !strings.Contains(s, "Done.") || !strings.Contains(s, "lookup") {
		t.Fatalf("stream = %s", s)
	}
}

// A data URI's type ends at ';' or the comma, and plain (non-base64) data is
// encoded before it is labelled base64.
func TestAnthropicImageSourceFromDataURI(t *testing.T) {
	src := anthropicImageSourceFromURL("data:image/jpeg,%FF%D8").(map[string]any)
	if src["media_type"] != "image/jpeg" || src["data"] != "/9g=" {
		t.Fatalf("source = %v", src)
	}
	b64 := anthropicImageSourceFromURL("data:image/webp;base64,AAAA").(map[string]any)
	if b64["media_type"] != "image/webp" || b64["data"] != "AAAA" {
		t.Fatalf("base64 source = %v", b64)
	}
}
