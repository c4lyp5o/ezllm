package translate

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

type respOut struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	Role         string `json:"role"`
	Model        string `json:"model"`
	StopReason   string `json:"stop_reason"`
	StopSequence *string
	Content      []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func translateResp(t *testing.T, in, model string) (respOut, []byte) {
	t.Helper()
	out, err := Response([]byte(in), model)
	if err != nil {
		t.Fatalf("Response(%s) failed: %v", in, err)
	}
	var got respOut
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("translated body is not valid JSON: %v\n%s", err, out)
	}
	return got, out
}

func TestResponseTranslationGolden(t *testing.T) {
	got, err := Response(fixture(t, "openai_response.json"), "claude-3-5-sonnet-20240620")
	if err != nil {
		t.Fatalf("Response failed: %v", err)
	}
	want := fixture(t, "anthropic_response_golden.json")
	if !bytes.Equal(got, want) {
		t.Errorf("golden mismatch\nwant: %s\ngot:  %s", want, got)
	}
}

func TestResponseDeterminism(t *testing.T) {
	in := fixture(t, "openai_response.json")
	first, err := Response(in, "claude-3-5-sonnet-20240620")
	if err != nil {
		t.Fatalf("first Response: %v", err)
	}
	second, err := Response(in, "claude-3-5-sonnet-20240620")
	if err != nil {
		t.Fatalf("second Response: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("non-deterministic output\nfirst:  %s\nsecond: %s", first, second)
	}
	if orig := fixture(t, "openai_response.json"); !bytes.Equal(in, orig) {
		t.Error("Response mutated its input slice")
	}
}

func TestResponseTable(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		check func(t *testing.T, got respOut, raw []byte)
	}{
		{
			name: "empty choices yields empty content and end_turn",
			in:   `{"id":"x","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":0}}`,
			check: func(t *testing.T, got respOut, raw []byte) {
				if got.StopReason != "end_turn" {
					t.Errorf("stop_reason = %q", got.StopReason)
				}
				if got.Content == nil || len(got.Content) != 0 {
					t.Errorf("content = %v, want []", got.Content)
				}
				if !strings.Contains(string(raw), `"content":[]`) {
					t.Errorf("content must serialize as []: %s", raw)
				}
			},
		},
		{
			name: "refusal finish_reason",
			in:   `{"id":"x","choices":[{"message":{"role":"assistant","content":""},"finish_reason":"content_filter"}],"usage":{}}`,
			check: func(t *testing.T, got respOut, _ []byte) {
				if got.StopReason != "refusal" {
					t.Errorf("stop_reason = %q, want refusal", got.StopReason)
				}
			},
		},
		{
			name: "length maps to max_tokens, unknown maps to end_turn",
			in:   `{"id":"x","choices":[{"message":{"role":"assistant","content":"cut"},"finish_reason":"length"}],"usage":{}}`,
			check: func(t *testing.T, got respOut, _ []byte) {
				if got.StopReason != "max_tokens" {
					t.Errorf("stop_reason = %q", got.StopReason)
				}
			},
		},
		{
			name: "missing usage decodes as zeros",
			in:   `{"id":"x","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`,
			check: func(t *testing.T, got respOut, _ []byte) {
				if got.Usage.InputTokens != 0 || got.Usage.OutputTokens != 0 {
					t.Errorf("usage = %+v, want zeros", got.Usage)
				}
			},
		},
		{
			name: "tool_calls only, empty arguments become an object",
			in:   `{"id":"x","choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"t","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`,
			check: func(t *testing.T, got respOut, _ []byte) {
				if len(got.Content) != 1 || got.Content[0].Type != "tool_use" {
					t.Fatalf("content = %+v", got.Content)
				}
				if string(got.Content[0].Input) != "{}" {
					t.Errorf("input = %s, want {}", got.Content[0].Input)
				}
				if got.StopReason != "tool_use" {
					t.Errorf("stop_reason = %q", got.StopReason)
				}
				if got.Usage.InputTokens != 3 || got.Usage.OutputTokens != 4 {
					t.Errorf("usage = %+v", got.Usage)
				}
			},
		},
		{
			name: "malformed tool arguments degrade to the raw string",
			in:   `{"id":"x","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"t","function":{"name":"f","arguments":"not json"}}]},"finish_reason":"tool_calls"}],"usage":{}}`,
			check: func(t *testing.T, got respOut, _ []byte) {
				if string(got.Content[0].Input) != `"not json"` {
					t.Errorf("input = %s", got.Content[0].Input)
				}
			},
		},
		{
			name: "id already prefixed is not double-prefixed",
			in:   `{"id":"msg_abc","choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],"usage":{}}`,
			check: func(t *testing.T, got respOut, _ []byte) {
				if got.ID != "msg_abc" {
					t.Errorf("id = %q", got.ID)
				}
			},
		},
		{
			name: "envelope fields are always present",
			in:   `{"id":"x","choices":[],"usage":{}}`,
			check: func(t *testing.T, got respOut, raw []byte) {
				if got.Type != "message" || got.Role != "assistant" || got.Model != "client-model" {
					t.Errorf("envelope = %+v", got)
				}
				if !strings.Contains(string(raw), `"stop_sequence":null`) {
					t.Errorf("stop_sequence must be null, not omitted: %s", raw)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, raw := translateResp(t, tc.in, "client-model")
			tc.check(t, got, raw)
		})
	}
}
