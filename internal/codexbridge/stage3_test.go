package codexbridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseRequestStructuredOutputAndSummary(t *testing.T) {
	req, err := ParseRequest([]byte(`{"model":"m","reasoning":{"summary":"detailed","effort":"high"},
		"response_format":{"type":"json_schema","json_schema":{"name":"city","schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}}},
		"messages":[{"role":"user","content":"capital of Malaysia"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Summary != "detailed" || req.Effort != "high" || !strings.Contains(string(req.OutputSchema), `"required":["city"]`) {
		t.Fatalf("req = %+v", req)
	}
	plain, _ := ParseRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"x"}]}`))
	if plain.Summary != "auto" || plain.OutputSchema != nil {
		t.Fatalf("defaults: summary=%q schema=%s", plain.Summary, plain.OutputSchema)
	}
	jsonMode, _ := ParseRequest([]byte(`{"model":"m","response_format":{"type":"json_object"},"messages":[{"role":"user","content":"x"}]}`))
	if !strings.Contains(jsonMode.Instructions, "single valid JSON object") || jsonMode.OutputSchema != nil {
		t.Fatalf("json_object: %+v", jsonMode)
	}
	for _, body := range []string{
		`{"model":"m","response_format":{"type":"json_schema"},"messages":[{"role":"user","content":"x"}]}`,
		`{"model":"m","response_format":{"type":"xml"},"messages":[{"role":"user","content":"x"}]}`,
		`{"model":"m","reasoning":{"summary":"verbose"},"messages":[{"role":"user","content":"x"}]}`,
	} {
		var u Unsupported
		if _, err := ParseRequest([]byte(body)); !errors.As(err, &u) || u.Code != "invalid_request" {
			t.Errorf("%s → %v, want invalid_request", body, err)
		}
	}
}

func TestRunStreamsReasoningSummariesAndPassesTheSchema(t *testing.T) {
	r, home := fakeRunner(t, "reasoning")
	req := simpleRequest()
	req.Summary = "detailed"
	req.OutputSchema = json.RawMessage(`{"type":"object"}`)
	var kinds []string
	res, err := r.Run(context.Background(), req, func(d Delta) error {
		if d.Reasoning != "" {
			kinds = append(kinds, "r")
		} else {
			kinds = append(kinds, "c")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reasoning != "**Counting**\n\n333+199-2*66" || res.Text != "400" || strings.Join(kinds, "") != "rrc" {
		t.Fatalf("reasoning=%q text=%q order=%v", res.Reasoning, res.Text, kinds)
	}
	var ts map[string]any
	b, _ := os.ReadFile(filepath.Join(home, "turn_start.json"))
	_ = json.Unmarshal(b, &ts)
	if ts["summary"] != "detailed" || ts["outputSchema"] == nil {
		t.Fatalf("turn/start params = %v", ts)
	}
}

func TestExecChannelCarriesSchemaSummaryAndReasoning(t *testing.T) {
	r, home := fakeRunner(t, "ok")
	r.Channel = ChannelExec
	req := simpleRequest()
	req.Summary = "auto"
	req.OutputSchema = json.RawMessage(`{"type":"object"}`)
	res, err := r.Run(context.Background(), req, nil)
	if err != nil || res.Reasoning != "**Greeting** in French" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	args, _ := readExec(t, home)
	if !slices.Contains(args, `model_reasoning_summary="auto"`) || !slices.Contains(args, "--output-schema") {
		t.Fatalf("exec argv = %v", args)
	}
}

func TestReasoningContentInBothShapes(t *testing.T) {
	f := &fakeExec{reasoning: []string{"**Counting**"}, deltas: []string{"400"}}
	s := testServer(f)
	rec := do(t, s, "POST", "/v1/chat/completions", hiBody, "secret")
	if !strings.Contains(rec.Body.String(), `"reasoning_content":"**Counting**"`) {
		t.Fatalf("non-stream lacks reasoning_content: %s", rec.Body)
	}
	rec = do(t, s, "POST", "/v1/chat/completions", `{"model":"gpt-6.1-sol","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "secret")
	body := rec.Body.String()
	ri, ci := strings.Index(body, `"reasoning_content":"**Counting**"`), strings.Index(body, `"content":"400"`)
	if ri < 0 || ci < 0 || ri > ci {
		t.Fatalf("stream must carry reasoning_content before content: %s", body)
	}
}
