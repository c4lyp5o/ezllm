package claudebridge

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/translate"
)

// The bug report's path: Hermes speaks OpenAI chat to ezllm, ezllm translates to
// Messages for the claude-sub account, and the bridge must accept what arrives.
// The body is Hermes's shape — tools on every turn, parallel_tool_calls=false,
// a prior tool call and its result, and its system-plus-last-three cache markers.
const hermesChat = `{
 "model":"claude-sub/claude-sonnet-5-5","stream":true,"parallel_tool_calls":false,
 "messages":[
  {"role":"system","content":[{"type":"text","text":"You are Max.","cache_control":{"type":"ephemeral"}}]},
  {"role":"user","content":[{"type":"text","text":"list files","cache_control":{"type":"ephemeral"}}]},
  {"role":"assistant","content":"","tool_calls":[{"id":"functions.terminal:0","type":"function","function":{"name":"terminal","arguments":"{\"command\":\"ls\"}"}}]},
  {"role":"tool","tool_call_id":"functions.terminal:0","content":"a.txt","cache_control":{"type":"ephemeral"}},
  {"role":"user","content":[{"type":"text","text":"thanks","cache_control":{"type":"ephemeral"}}]}
 ],
 "tools":[{"type":"function","function":{"name":"terminal","description":"run","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}}]}`

func TestHermesChatBodyCrossesIntoTheBridge(t *testing.T) {
	messages, err := translate.RequestUniversal([]byte(hermesChat), provider.SurfaceOpenAI, provider.SurfaceAnthropic, "claude-sonnet-5-5")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	req, err := ParseRequest(messages)
	if err != nil {
		t.Fatalf("the bridge refused a translated Hermes turn (the reported 400): %v\n%s", err, messages)
	}
	if !strings.Contains(string(req.Tools), `"terminal"`) {
		t.Errorf("tools lost in translation: %s", req.Tools)
	}
	extra := body(t, req)
	if n := strings.Count(extra, "cache_control"); n > clientCacheMarkers {
		t.Errorf("extra body carries %d cache markers, want <= %d (upstream allows 4, native spends 2)", n, clientCacheMarkers)
	}
	var got struct {
		ToolChoice map[string]any `json:"tool_choice"`
	}
	if err := json.Unmarshal([]byte(extra), &got); err != nil {
		t.Fatal(err)
	}
	if got.ToolChoice["disable_parallel_tool_use"] != true {
		t.Errorf("tool_choice = %v, want parallel_tool_calls=false carried as disable_parallel_tool_use", got.ToolChoice)
	}
	if _, err := BuildFrames(req); err != nil {
		t.Fatalf("frames: %v", err)
	}
}
