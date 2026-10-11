package translate

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// reqOut is a decode target for translated request bodies; assertions
// read fields rather than raw bytes except in the golden test.
type reqOut struct {
	Model    string `json:"model"`
	Messages []struct {
		Role       string `json:"role"`
		Content    string `json:"content"`
		ToolCallID string `json:"tool_call_id"`
		ToolCalls  []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"messages"`
	MaxTokens     int      `json:"max_tokens"`
	Stop          []string `json:"stop"`
	Stream        bool     `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	} `json:"tools"`
	ToolChoice json.RawMessage `json:"tool_choice"`
}

func translateReq(t *testing.T, in, model string) (reqOut, []byte) {
	t.Helper()
	out, err := Request([]byte(in), model)
	if err != nil {
		t.Fatalf("Request(%s) failed: %v", in, err)
	}
	var got reqOut
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("translated body is not valid JSON: %v\n%s", err, out)
	}
	return got, out
}

func TestRequestTranslationGolden(t *testing.T) {
	got, err := Request(fixture(t, "anthropic_request.json"), "gpt-4o")
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	want := fixture(t, "openai_request_golden.json")
	if !bytes.Equal(got, want) {
		t.Errorf("golden mismatch\nwant: %s\ngot:  %s", want, got)
	}
}

// TestRequestDeterminism pins the prefix-cache invariant: identical
// input must produce byte-identical output, and the input is never
// mutated. Any nondeterminism breaks upstream automatic prefix caching.
func TestRequestDeterminism(t *testing.T) {
	in := fixture(t, "anthropic_request.json")
	first, err := Request(in, "gpt-4o")
	if err != nil {
		t.Fatalf("first Request: %v", err)
	}
	second, err := Request(in, "gpt-4o")
	if err != nil {
		t.Fatalf("second Request: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("non-deterministic output\nfirst:  %s\nsecond: %s", first, second)
	}
	if orig := fixture(t, "anthropic_request.json"); !bytes.Equal(in, orig) {
		t.Error("Request mutated its input slice")
	}
}

func TestRequestUnknownFieldsIgnored(t *testing.T) {
	in := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],
		"top_k":40,"weird_future_field":{"nested":true},"anthropic_beta":["x"]}`
	got, out := translateReq(t, in, "upstream-model")
	if got.Model != "upstream-model" {
		t.Errorf("model = %q, want upstream-model", got.Model)
	}
	for _, banned := range []string{"top_k", "weird_future_field", "anthropic_beta"} {
		if strings.Contains(string(out), banned) {
			t.Errorf("unknown field %q leaked into translated body: %s", banned, out)
		}
	}
}

func TestRequestTable(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		check func(t *testing.T, got reqOut, raw []byte)
	}{
		{
			name: "system as string becomes first system message",
			in:   `{"model":"m","system":"Be terse.","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`,
			check: func(t *testing.T, got reqOut, _ []byte) {
				if len(got.Messages) != 2 || got.Messages[0].Role != "system" || got.Messages[0].Content != "Be terse." {
					t.Errorf("messages = %+v", got.Messages)
				}
				if got.Messages[1].Role != "user" {
					t.Errorf("user message not preserved after system: %+v", got.Messages)
				}
			},
		},
		{
			name: "absent system synthesizes nothing",
			in:   `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`,
			check: func(t *testing.T, got reqOut, _ []byte) {
				if len(got.Messages) != 1 || got.Messages[0].Role != "user" {
					t.Errorf("messages = %+v", got.Messages)
				}
			},
		},
		{
			name: "multi text blocks join with newline",
			in:   `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}`,
			check: func(t *testing.T, got reqOut, _ []byte) {
				if got.Messages[0].Content != "a\nb" {
					t.Errorf("content = %q, want %q", got.Messages[0].Content, "a\nb")
				}
			},
		},
		{
			name: "tool_result string becomes tool message",
			in:   `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_9","content":"done"}]}]}`,
			check: func(t *testing.T, got reqOut, _ []byte) {
				m := got.Messages[0]
				if m.Role != "tool" || m.ToolCallID != "call_9" || m.Content != "done" {
					t.Errorf("message = %+v", m)
				}
			},
		},
		{
			name: "tool_result block content flattens to text",
			in:   `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":[{"type":"text","text":"line1"},{"type":"text","text":"line2"}]}]}]}`,
			check: func(t *testing.T, got reqOut, _ []byte) {
				if got.Messages[0].Content != "line1\nline2" {
					t.Errorf("content = %q", got.Messages[0].Content)
				}
			},
		},
		{
			name: "mixed tool_result and text keeps both, tool message first",
			in:   `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"out"},{"type":"text","text":"also read this"}]}]}`,
			check: func(t *testing.T, got reqOut, _ []byte) {
				if len(got.Messages) != 2 {
					t.Fatalf("want tool message + user message, got %+v", got.Messages)
				}
				if got.Messages[0].Role != "tool" || got.Messages[0].Content != "out" {
					t.Errorf("first message = %+v, want tool", got.Messages[0])
				}
				if got.Messages[1].Role != "user" || got.Messages[1].Content != "also read this" {
					t.Errorf("second message = %+v, want user text", got.Messages[1])
				}
			},
		},
		{
			name: "assistant tool_use becomes tool_calls with string arguments",
			in:   `{"model":"m","max_tokens":10,"messages":[{"role":"assistant","content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"tu1","name":"get_weather","input":{"location":"NYC","days":2}}]}]}`,
			check: func(t *testing.T, got reqOut, _ []byte) {
				m := got.Messages[0]
				if m.Content != "checking" || len(m.ToolCalls) != 1 {
					t.Fatalf("message = %+v", m)
				}
				tc := m.ToolCalls[0]
				if tc.ID != "tu1" || tc.Type != "function" || tc.Function.Name != "get_weather" {
					t.Errorf("tool call = %+v", tc)
				}
				var parsed map[string]any
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &parsed); err != nil {
					t.Fatalf("arguments is not a JSON string: %q (%v)", tc.Function.Arguments, err)
				}
				if parsed["location"] != "NYC" {
					t.Errorf("arguments = %q", tc.Function.Arguments)
				}
			},
		},
		{
			name: "tool_choice shapes",
			in:   `{"model":"m","max_tokens":10,"messages":[],"tool_choice":{"type":"any"}}`,
			check: func(t *testing.T, got reqOut, _ []byte) {
				if string(got.ToolChoice) != `"required"` {
					t.Errorf("any -> %s, want \"required\"", got.ToolChoice)
				}
			},
		},
		{
			name: "tool_choice named tool",
			in:   `{"model":"m","max_tokens":10,"messages":[],"tool_choice":{"type":"tool","name":"get_weather"}}`,
			check: func(t *testing.T, got reqOut, _ []byte) {
				var tc struct {
					Type     string `json:"type"`
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				}
				if err := json.Unmarshal(got.ToolChoice, &tc); err != nil {
					t.Fatalf("tool_choice invalid: %v", err)
				}
				if tc.Type != "function" || tc.Function.Name != "get_weather" {
					t.Errorf("tool_choice = %s", got.ToolChoice)
				}
			},
		},
		{
			name: "tool_choice none passes through",
			in:   `{"model":"m","max_tokens":10,"messages":[],"tool_choice":{"type":"none"}}`,
			check: func(t *testing.T, got reqOut, _ []byte) {
				if string(got.ToolChoice) != `"none"` {
					t.Errorf("none -> %s", got.ToolChoice)
				}
			},
		},
		{
			name: "absent tool_choice stays absent",
			in:   `{"model":"m","max_tokens":10,"messages":[]}`,
			check: func(t *testing.T, got reqOut, _ []byte) {
				if len(got.ToolChoice) != 0 {
					t.Errorf("tool_choice synthesized: %s", got.ToolChoice)
				}
			},
		},
		{
			name: "stop_sequences map to stop, tools map verbatim",
			in:   `{"model":"m","max_tokens":10,"messages":[],"stop_sequences":["A","B"],"tools":[{"name":"f","description":"d","input_schema":{"type":"object","properties":{"x":{"type":"string"}}}}]}`,
			check: func(t *testing.T, got reqOut, _ []byte) {
				if len(got.Stop) != 2 || got.Stop[0] != "A" {
					t.Errorf("stop = %v", got.Stop)
				}
				if len(got.Tools) != 1 || got.Tools[0].Type != "function" || got.Tools[0].Function.Name != "f" {
					t.Fatalf("tools = %+v", got.Tools)
				}
				var schema map[string]any
				if err := json.Unmarshal(got.Tools[0].Function.Parameters, &schema); err != nil {
					t.Fatalf("parameters not verbatim JSON: %v", err)
				}
				if schema["type"] != "object" {
					t.Errorf("parameters = %s", got.Tools[0].Function.Parameters)
				}
			},
		},
		{
			name: "stream true requests include_usage; stream false does not",
			in:   `{"model":"m","max_tokens":10,"messages":[],"stream":true}`,
			check: func(t *testing.T, got reqOut, _ []byte) {
				if !got.Stream || got.StreamOptions == nil || !got.StreamOptions.IncludeUsage {
					t.Errorf("stream=%v stream_options=%+v", got.Stream, got.StreamOptions)
				}
			},
		},
		{
			name: "non-stream body carries no stream_options",
			in:   `{"model":"m","max_tokens":10,"messages":[]}`,
			check: func(t *testing.T, got reqOut, _ []byte) {
				if got.StreamOptions != nil {
					t.Errorf("stream_options = %+v, want nil", got.StreamOptions)
				}
			},
		},
		{
			name: "thinking, metadata and cache_control are dropped",
			in:   `{"model":"m","max_tokens":10,"messages":[],"thinking":{"type":"enabled","budget_tokens":1024},"metadata":{"user_id":"u"},"cache_control":{"type":"ephemeral"}}`,
			check: func(t *testing.T, got reqOut, raw []byte) {
				for _, banned := range []string{"thinking", "budget_tokens", "metadata", "cache_control"} {
					if strings.Contains(string(raw), banned) {
						t.Errorf("dropped field %q leaked: %s", banned, raw)
					}
				}
			},
		},
		{
			name: "missing tool_use_id and tool_use name do not panic",
			in:   `{"model":"m","max_tokens":10,"messages":[{"role":"assistant","content":[{"type":"tool_use","input":{"x":1}}]},{"role":"user","content":[{"type":"tool_result","content":"orphan"}]}]}`,
			check: func(t *testing.T, got reqOut, _ []byte) {
				if len(got.Messages) != 2 {
					t.Fatalf("messages = %+v", got.Messages)
				}
				if got.Messages[0].ToolCalls[0].ID != "" || got.Messages[1].ToolCallID != "" {
					t.Errorf("expected empty ids, got %+v", got.Messages)
				}
			},
		},
		{
			name: "max_tokens and model are threaded",
			in:   `{"model":"claude-3-5-sonnet","max_tokens":4096,"messages":[]}`,
			check: func(t *testing.T, got reqOut, _ []byte) {
				if got.MaxTokens != 4096 {
					t.Errorf("max_tokens = %d", got.MaxTokens)
				}
				if got.Model != "upstream-model" {
					t.Errorf("model = %q, want the hop's upstream model", got.Model)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, raw := translateReq(t, tc.in, "upstream-model")
			tc.check(t, got, raw)
		})
	}
}
