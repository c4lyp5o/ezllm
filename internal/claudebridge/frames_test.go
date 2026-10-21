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

// A Hermes agent loop ends most requests on a user turn made only of
// tool_result blocks; that is a real query, not an empty message.
const toolHistory = `{"model":"claude-haiku-5-5","max_tokens":1024,` +
	`"tools":[{"name":"get_weather","input_schema":{"type":"object"}}],"messages":[` +
	`{"role":"user","content":"Weather in Paris?"},` +
	`{"role":"assistant","content":[{"type":"text","text":"Checking."},` +
	`{"type":"tool_use","id":"toolu_01SPIKEPARIS","name":"get_weather","input":{"city":"Paris"}}]},` +
	`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01SPIKEPARIS","content":"17C, light rain"}]}]}`

func TestParseRequestAcceptsToolResultTurn(t *testing.T) {
	req, err := ParseRequest([]byte(toolHistory))
	if err != nil {
		t.Fatalf("tool_result-only final turn must be accepted: %v", err)
	}
	frames, err := BuildFrames(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 3 {
		t.Fatalf("frames = %d, want 3", len(frames))
	}
	if frames[0]["shouldQuery"] != false {
		t.Error("replayed user frame must carry shouldQuery:false")
	}
	if _, ok := frames[2]["shouldQuery"]; ok {
		t.Error("the final tool_result turn is the trigger and must query")
	}
	// Measured on 2.1.306: native accepts tool_use/tool_result blocks in its
	// frames verbatim, so they are not flattened or rewritten.
	raw, _ := json.Marshal(frames)
	for _, want := range []string{`"type":"tool_use"`, `"tool_use_id":"toolu_01SPIKEPARIS"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("frames lost %s: %s", want, raw)
		}
	}
	if b := body(t, req); !strings.Contains(b, `"tool_use_id":"toolu_01SPIKEPARIS"`) {
		t.Errorf("extra body lost the tool_result: %s", b)
	}
}

// Every request of one tool conversation replays the same opening, so the
// session id (and native's cached prefix) holds across tool turns.
func TestSessionIDStableAcrossToolTurns(t *testing.T) {
	first, err := ParseRequest([]byte(`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":"Weather in Paris?"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	later, err := ParseRequest([]byte(toolHistory))
	if err != nil {
		t.Fatal(err)
	}
	if a, b := ConversationSessionID("sys", first), ConversationSessionID("sys", later); a != b {
		t.Errorf("session id moved across tool turns: %s vs %s", a, b)
	}
}

// Every tool_choice form reaches upstream as sent; measured on 2.1.306, native
// honours auto, any, a named tool and none.
func TestToolChoiceFormsPassThroughUnchanged(t *testing.T) {
	for _, choice := range []string{
		`{"type":"auto"}`,
		`{"type":"any"}`,
		`{"type":"tool","name":"get_weather"}`,
		`{"type":"none"}`,
		`{"type":"auto","disable_parallel_tool_use":true}`,
	} {
		req, err := ParseRequest([]byte(`{"model":"claude-haiku-5-5","tool_choice":` + choice +
			`,"tools":[{"name":"get_weather","input_schema":{"type":"object"}}],` +
			`"messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatalf("%s: %v", choice, err)
		}
		var got struct {
			ToolChoice json.RawMessage `json:"tool_choice"`
		}
		if err := json.Unmarshal([]byte(body(t, req)), &got); err != nil {
			t.Fatal(err)
		}
		if string(got.ToolChoice) != choice {
			t.Errorf("tool_choice = %s, want %s", got.ToolChoice, choice)
		}
	}
}

func TestParseRequestRefusesServerTools(t *testing.T) {
	_, err := ParseRequest([]byte(`{"model":"claude-haiku-5-5","tools":[` +
		`{"name":"get_weather","input_schema":{"type":"object"}},` +
		`{"type":"web_search_20250305","name":"web_search"}],` +
		`"messages":[{"role":"user","content":"hi"}]}`))
	u, ok := err.(Unsupported)
	if !ok {
		t.Fatalf("want Unsupported, got %T: %v", err, err)
	}
	if u.Field != "tools[1].type=web_search_20250305" {
		t.Errorf("Field = %q", u.Field)
	}
	if msg := u.Error(); strings.Contains(msg, "M1") || !strings.Contains(msg, "server tools execute upstream") {
		t.Errorf("refusal must name its reason, without stale milestone wording: %q", msg)
	}
}

func TestParseRequestAcceptsCustomTypedTool(t *testing.T) {
	if _, err := ParseRequest([]byte(`{"model":"claude-haiku-5-5","tools":[` +
		`{"type":"custom","name":"get_weather","input_schema":{"type":"object"}}],` +
		`"messages":[{"role":"user","content":"hi"}]}`)); err != nil {
		t.Fatalf("type custom is a client tool: %v", err)
	}
}

// Hermes marks its system prompt and the last three messages; native adds two
// markers of its own and upstream allows four. Without the trim, measured on
// 2.1.306: "A maximum of 4 blocks with cache_control may be provided. Found 6."
func TestExtraBodyKeepsOnlyTheNewestTwoCacheMarkers(t *testing.T) {
	mark := `,"cache_control":{"type":"ephemeral"}`
	req, err := ParseRequest([]byte(`{"model":"claude-haiku-5-5",` +
		`"tools":[{"name":"t","input_schema":{"type":"object"}` + mark + `}],"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"u0"` + mark + `}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"a0"` + mark + `}]},` +
		`{"role":"user","content":[{"type":"text","text":"u1"` + mark + `}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"a1"}]},` +
		`{"role":"user","content":[{"type":"text","text":"u2"` + mark + `}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Tools    []map[string]any `json:"tools"`
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body(t, req)), &got); err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, m := range got.Messages {
		for _, b := range m.Content {
			if b["cache_control"] != nil {
				kept = append(kept, b["text"].(string))
			}
		}
	}
	if strings.Join(kept, ",") != "u1,u2" {
		t.Errorf("message markers kept on %v, want u1,u2 (the newest two)", kept)
	}
	if got.Tools[0]["cache_control"] != nil {
		t.Error("tool marker survived past the two-marker budget")
	}
}

func TestExtraBodyKeepsToolMarkerWithinBudget(t *testing.T) {
	req, err := ParseRequest([]byte(`{"model":"claude-haiku-5-5",` +
		`"tools":[{"name":"t","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if b := body(t, req); strings.Count(b, "cache_control") != 2 {
		t.Errorf("two markers fit the budget and must both survive: %s", b)
	}
}
