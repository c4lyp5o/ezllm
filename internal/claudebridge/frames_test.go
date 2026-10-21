package claudebridge

import (
	"encoding/json"
	"strings"
	"testing"
)

func body(t *testing.T, req Request) string {
	t.Helper()
	s, err := BuildExtraBody(req)
	if err != nil {
		t.Fatalf("BuildExtraBody: %v", err)
	}
	return s
}

// M2: tools are honoured, not refused — they travel to upstream unchanged.
func TestParseRequestCarriesToolsIntoExtraBody(t *testing.T) {
	tools := `[{"name":"get_weather","description":"Get weather.","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]`
	choice := `{"type":"tool","name":"get_weather"}`
	req, err := ParseRequest([]byte(`{"model":"claude-haiku-5-5","max_tokens":16,"tools":` + tools +
		`,"tool_choice":` + choice + `,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("tool-bearing request must be accepted: %v", err)
	}
	var got struct {
		Tools      json.RawMessage `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if err := json.Unmarshal([]byte(body(t, req)), &got); err != nil {
		t.Fatal(err)
	}
	if string(got.Tools) != tools {
		t.Errorf("tools = %s, want the client's definitions verbatim %s", got.Tools, tools)
	}
	if string(got.ToolChoice) != choice {
		t.Errorf("tool_choice = %s, want %s", got.ToolChoice, choice)
	}
}

func TestExtraBodyOmitsToolsWhenAbsent(t *testing.T) {
	req, err := ParseRequest([]byte(`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if b := body(t, req); strings.Contains(b, "tools") || strings.Contains(b, "tool_choice") {
		t.Errorf("text-only body grew tool fields: %s", b)
	}
}

// parallel_tool_calls is an OpenAI field; on the Messages wire it is
// tool_choice.disable_parallel_tool_use. Arriving here it is refused, not dropped.
func TestParseRequestRefusesForeignParallelToolCalls(t *testing.T) {
	_, err := ParseRequest([]byte(`{"model":"claude-haiku-5-5","max_tokens":16,"parallel_tool_calls":false,` +
		`"messages":[{"role":"user","content":"hi"}]}`))
	u, ok := err.(Unsupported)
	if !ok {
		t.Fatalf("want Unsupported, got %T: %v", err, err)
	}
	if u.Field != "parallel_tool_calls" {
		t.Errorf("Field = %q, want parallel_tool_calls", u.Field)
	}
}

func TestParseRequestRequiresUserLast(t *testing.T) {
	for _, raw := range []string{
		`{"model":"m","messages":[{"role":"assistant","content":"prefill"}]}`,
		`{"model":"m","messages":[{"role":"user","content":"   "}]}`,
		`{"model":"","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","messages":[]}`,
	} {
		if _, err := ParseRequest([]byte(raw)); err == nil {
			t.Errorf("ParseRequest(%s) = nil error, want a refusal", raw)
		}
	}
}

// The extra body is what the subscription actually sees. It must carry the
// messages and nothing native rejects or owns.
func TestBuildExtraBodyOmitsSystemIDsAndSampling(t *testing.T) {
	req := Request{
		Model:     "claude-haiku-5-5",
		MaxTokens: 64,
		System:    json.RawMessage(`"be terse"`),
		Messages: []Message{
			{Role: "user", Content: json.RawMessage(`"hello"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"hi"}]`)},
			{Role: "user", Content: json.RawMessage(`"again"`)},
		},
		Stream: true,
		Stop:   []string{"STOP"},
	}
	out := body(t, req)
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("extra body is not JSON: %v", err)
	}
	for _, banned := range []string{"system", "temperature", "top_p", "id"} {
		if _, ok := got[banned]; ok {
			t.Errorf("extra body must not carry %q (native owns it or the strict schema rejects it): %s", banned, out)
		}
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3", len(msgs))
	}
	if m, _ := msgs[1].(map[string]any); m["role"] != "assistant" {
		t.Errorf("messages[1].role = %v, want assistant", m["role"])
	}
	if got["stream"] != true {
		t.Errorf("stream = %v, want true", got["stream"])
	}
	if got["max_tokens"] != float64(64) {
		t.Errorf("max_tokens = %v, want 64", got["max_tokens"])
	}
	if ss, _ := got["stop_sequences"].([]any); len(ss) != 1 || ss[0] != "STOP" {
		t.Errorf("stop_sequences = %v, want [STOP]", got["stop_sequences"])
	}
}

// Replayed user frames must not generate. Measured: without shouldQuery:false
// every user frame produced its own upstream request and its own plan draw.
func TestBuildFramesMarksHistoryUserFrames(t *testing.T) {
	req := Request{Model: "claude-haiku-5-5", Messages: []Message{
		{Role: "user", Content: json.RawMessage(`"one"`)},
		{Role: "assistant", Content: json.RawMessage(`"ok"`)},
		{Role: "user", Content: json.RawMessage(`"two"`)},
	}}
	frames, err := BuildFrames(req)
	if err != nil {
		t.Fatalf("BuildFrames: %v", err)
	}
	if len(frames) != 3 {
		t.Fatalf("frames = %d, want 3", len(frames))
	}
	if q, ok := frames[0]["shouldQuery"].(bool); !ok || q {
		t.Errorf("frames[0] shouldQuery = %v, want explicit false (replay, no generation)", frames[0]["shouldQuery"])
	}
	if q, ok := frames[2]["shouldQuery"]; ok {
		t.Errorf("frames[2] (the trigger) shouldQuery = %v, want absent", q)
	}
	if frames[1]["type"] != "assistant" {
		t.Errorf("frames[1].type = %v, want assistant", frames[1]["type"])
	}
}

func TestConversationSessionIDStablePerConversation(t *testing.T) {
	mk := func(first string) Request {
		return Request{Model: "m", Messages: []Message{
			{Role: "user", Content: json.RawMessage(`"` + first + `"`)},
			{Role: "user", Content: json.RawMessage(`"tail"`)},
		}}
	}
	a := ConversationSessionID("sys", mk("hello"))
	b := ConversationSessionID("sys", mk("hello"))
	if a != b {
		t.Fatalf("session id must be stable across turns of one conversation: %s != %s", a, b)
	}
	if c := ConversationSessionID("sys", mk("different")); c == a {
		t.Error("a different conversation must not collide on the same session id")
	}
	if len(a) != 36 || strings.Count(a, "-") != 4 {
		t.Errorf("session id %q is not uuid-shaped", a)
	}
}
