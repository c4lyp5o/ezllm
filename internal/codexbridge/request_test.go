package codexbridge

import (
	"errors"
	"strings"
	"testing"
)

func TestParseRequestSingleTurnPassesThrough(t *testing.T) {
	req, err := ParseRequest([]byte(`{"model":"gpt-6.1-sol","stream":true,"stream_options":{"include_usage":true},
		"reasoning_effort":"high","messages":[{"role":"system","content":"Be terse."},{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "gpt-6.1-sol" || !req.Stream || !req.IncludeUsage || req.Effort != "high" || req.Instructions != "Be terse." {
		t.Fatalf("req = %+v", req)
	}
	if len(req.Input) != 1 || req.Input[0] != (UserInput{Type: "text", Text: "hi"}) {
		t.Fatalf("input = %+v, want the user message unchanged", req.Input)
	}
}

func TestParseRequestWithoutSystemUsesTheMinimalInstructions(t *testing.T) {
	req, err := ParseRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil || req.Instructions != DefaultInstructions {
		t.Fatalf("instructions = %q err = %v", req.Instructions, err)
	}
}

// History travels as a flattened transcript (§4.2), deterministic so the same
// history yields the same cacheable prefix.
func TestParseRequestFlattensHistory(t *testing.T) {
	body := `{"model":"m","messages":[
		{"role":"user","content":"My codename is BLUE-HERON-7."},
		{"role":"assistant","content":"Noted."},
		{"role":"user","content":[{"type":"text","text":"What is my codename?"}]}]}`
	a, err := ParseRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := ParseRequest([]byte(body))
	text := a.Input[0].Text
	for _, want := range []string{`<turn role="user">`, "BLUE-HERON-7", `<turn role="assistant">`, "Noted.", "Reply to the latest user message:", "What is my codename?"} {
		if !strings.Contains(text, want) {
			t.Errorf("transcript lacks %q:\n%s", want, text)
		}
	}
	if strings.Index(text, "BLUE-HERON-7") > strings.Index(text, "What is my codename?") {
		t.Error("earlier turns must come before the latest message")
	}
	if text != b.Input[0].Text {
		t.Error("the same history produced two different transcripts")
	}
}

func TestParseRequestCarriesImages(t *testing.T) {
	req, err := ParseRequest([]byte(`{"model":"m","messages":[{"role":"user","content":[
		{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Input) != 2 || req.Input[1] != (UserInput{Type: "image", URL: "data:image/png;base64,AAAA"}) {
		t.Fatalf("input = %+v", req.Input)
	}
}

func TestParseRequestRefusals(t *testing.T) {
	for name, tc := range map[string]struct{ body, code string }{
		"functions":    {`{"model":"m","functions":[{"name":"f"}],"messages":[{"role":"user","content":"x"}]}`, "invalid_request"},
		"web tool":     {`{"model":"m","tools":[{"type":"web_search"}],"messages":[{"role":"user","content":"x"}]}`, "invalid_request"},
		"bad choice":   {`{"model":"m","tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":42,"messages":[{"role":"user","content":"x"}]}`, "invalid_request"},
		"tool no id":   {`{"model":"m","messages":[{"role":"user","content":"x"},{"role":"assistant","tool_calls":[{"id":"c1","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","content":"r"}]}`, "invalid_request"},
		"no model":     {`{"messages":[{"role":"user","content":"x"}]}`, "invalid_request"},
		"n=2":          {`{"model":"m","n":2,"messages":[{"role":"user","content":"x"}]}`, "invalid_request"},
		"empty":        {`{"model":"m","messages":[]}`, "invalid_request"},
		"last is asst": {`{"model":"m","messages":[{"role":"user","content":"x"},{"role":"assistant","content":"y"}]}`, "invalid_request"},
		"bad role":     {`{"model":"m","messages":[{"role":"wizard","content":"x"}]}`, "invalid_request"},
		"audio part":   {`{"model":"m","messages":[{"role":"user","content":[{"type":"input_audio"}]}]}`, "invalid_request"},
		"not json":     {`{`, "invalid_request"},
	} {
		_, err := ParseRequest([]byte(tc.body))
		var u Unsupported
		if !errors.As(err, &u) || u.Code != tc.code {
			t.Errorf("%s: err = %v, want code %s", name, err, tc.code)
		}
	}
}

func TestNullToolsAreNotTools(t *testing.T) {
	if _, err := ParseRequest([]byte(`{"model":"m","tools":null,"tool_choice":null,"messages":[{"role":"user","content":"x"}]}`)); err != nil {
		t.Fatalf("explicit nulls must not count as tools: %v", err)
	}
}

func TestEffortNormalization(t *testing.T) {
	for in, want := range map[string]string{"minimal": "low", "none": "low", "HIGH": "high", "": "", "ultra": "ultra"} {
		if got := normalizeEffort(in); got != want {
			t.Errorf("normalizeEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMapTurnError(t *testing.T) {
	for raw, want := range map[string]int{
		`"usageLimitExceeded"`:                          429,
		`"rateLimitExceeded"`:                           429,
		`"unauthorized"`:                                401,
		`"contextWindowExceeded"`:                       400,
		`"serverOverloaded"`:                            503,
		`{"httpConnectionFailed":{"httpStatusCode":0}}`: 502,
		`"other"`: 502,
		`null`:    502,
	} {
		ne := MapTurnError(turnError{Message: "boom", CodexErrorInfo: []byte(raw)})
		if ne.Status != want || !strings.Contains(ne.Message, "boom") {
			t.Errorf("%s → %d %q, want %d carrying the message", raw, ne.Status, ne.Message, want)
		}
	}
}

func TestParseRequestMapsToolsAndChoice(t *testing.T) {
	req, err := ParseRequest([]byte(`{"model":"m","tool_choice":{"type":"function","function":{"name":"get_weather"}},"parallel_tool_calls":false,
		"tools":[{"type":"function","function":{"name":"get_weather","description":"weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}},
		         {"type":"function","function":{"name":"now"}}],
		"messages":[{"role":"user","content":"weather?"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 2 || req.Tools[0].Name != "get_weather" || !strings.Contains(string(req.Tools[0].Parameters), "city") ||
		string(req.Tools[1].Parameters) != `{"type":"object","properties":{}}` {
		t.Fatalf("tools = %+v", req.Tools)
	}
	if !strings.Contains(req.Instructions, "You must call the tool get_weather") || !strings.Contains(req.Instructions, "at most one tool") {
		t.Fatalf("instructions lack the tool_choice/parallel guidance: %q", req.Instructions)
	}
	none, _ := ParseRequest([]byte(`{"model":"m","tool_choice":"none","tools":[{"type":"function","function":{"name":"f"}}],"messages":[{"role":"user","content":"x"}]}`))
	if len(none.Tools) != 0 {
		t.Fatal("tool_choice none must declare no tools")
	}
}

// A request ending in tool results is a continuation: the results are keyed by
// tool_call_id, and the transcript (the stateless fallback) carries the calls.
func TestParseRequestContinuation(t *testing.T) {
	req, err := ParseRequest([]byte(`{"model":"m","tools":[{"type":"function","function":{"name":"get_weather"}}],"messages":[
		{"role":"user","content":"weather in KL and Paris?"},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"call-a","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"KL\"}"}},
			{"id":"call-b","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},
		{"role":"tool","tool_call_id":"call-a","content":"31C"},
		{"role":"tool","tool_call_id":"call-b","content":"18C"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.ToolResults) != 2 || req.ToolResults["call-a"] != "31C" || req.ToolResults["call-b"] != "18C" {
		t.Fatalf("tool results = %v", req.ToolResults)
	}
	text := req.Input[0].Text
	for _, want := range []string{"called tool get_weather (call-a)", `role="tool result call-b"`, "18C", "Continue from there."} {
		if !strings.Contains(text, want) {
			t.Errorf("replay transcript lacks %q:\n%s", want, text)
		}
	}
}
