package agybridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const lookupTools = `"tools":[{"type":"function","function":{"name":"lookup","description":"Look up a value on the caller","parameters":{"type":"object","properties":{"key":{"type":"string"}},"required":["key"],"additionalProperties":false}}}]`
const lookupRequest = `{"model":"gemini-3.8-flash-low",` + lookupTools + `,"messages":[{"role":"user","content":"look up x"}]}`
const lookupEnvelope = `{"content":null,"tool_calls":[{"name":"lookup","arguments":{"key":"x"}}]}`

func TestExperimentalToolsOptInAndTranscript(t *testing.T) {
	if _, err := ParseRequest([]byte(lookupRequest)); err == nil {
		t.Fatal("default accepted tools")
	}
	req, err := parseRequest([]byte(lookupRequest), true)
	if err != nil {
		t.Fatal(err)
	}
	if req.Adapter == nil || !strings.Contains(req.Prompt, "Never execute your built-in tools") || !strings.Contains(req.Prompt, `"name":"lookup"`) {
		t.Fatalf("missing adapter context: %+v", req)
	}
	body := `{"model":"m",` + lookupTools + `,"messages":[{"role":"user","content":"x"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_old","type":"function","function":{"name":"lookup","arguments":"{\"key\":\"x\"}"}}]},{"role":"tool","tool_call_id":"call_old","content":"caller-result-unique"}]}`
	req, err = parseRequest([]byte(body), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"tool_call_id":"call_old"`, `"id":"call_old"`, "caller-result-unique"} {
		if !strings.Contains(req.Prompt, want) {
			t.Fatalf("transcript lost %s", want)
		}
	}
	// Opt-in alone must leave ordinary chat byte-identical.
	plain := []byte(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`)
	a, _ := ParseRequest(plain)
	b, _ := parseRequest(plain, true)
	if a.Prompt != b.Prompt || b.Adapter != nil {
		t.Fatal("ordinary chat changed")
	}
}

func TestExperimentalToolsRejectMalformedRequests(t *testing.T) {
	for name, body := range map[string]string{
		"duplicate names":   `{"model":"m","tools":[{"type":"function","function":{"name":"f"}},{"type":"function","function":{"name":"f"}}],"messages":[{"role":"user","content":"x"}]}`,
		"wrong schema":      `{"model":"m","tools":[{"type":"function","function":{"name":"f","parameters":{"type":"array"}}}],"messages":[{"role":"user","content":"x"}]}`,
		"unknown choice":    strings.Replace(lookupRequest, `"messages":`, `"tool_choice":{"type":"function","function":{"name":"other"}},"messages":`, 1),
		"required no tools": `{"model":"m","tool_choice":"required","messages":[{"role":"user","content":"x"}]}`,
		"orphan result":     `{"model":"m","messages":[{"role":"tool","tool_call_id":"missing","content":"x"}]}`,
		"missing result":    `{"model":"m",` + lookupTools + `,"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"user","content":"x"}]}`,
		"legacy functions":  `{"model":"m","functions":[{"name":"f"}],"messages":[{"role":"user","content":"x"}]}`,
		"image":             strings.Replace(lookupRequest, `"look up x"`, `[{"type":"image_url","image_url":{"url":"data:"}}]`, 1),
		"format conflict":   strings.Replace(lookupRequest, `"messages":`, `"response_format":{"type":"json_object"},"messages":`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseRequest([]byte(body), true); err == nil {
				t.Fatal("accepted malformed request")
			}
		})
	}
}

func TestAdapterValidatesOutputAndChoice(t *testing.T) {
	req, err := parseRequest([]byte(lookupRequest), true)
	if err != nil {
		t.Fatal(err)
	}
	text, calls, err := req.Adapter.decode(Result{Text: lookupEnvelope})
	if err != nil || text != "" || len(calls) != 1 || calls[0].Function.Name != "lookup" || calls[0].Function.Arguments != `{"key":"x"}` || calls[0].ID == "" {
		t.Fatalf("text=%q calls=%+v err=%v", text, calls, err)
	}
	for _, raw := range []string{
		"prose " + lookupEnvelope, "```json\n" + lookupEnvelope + "\n```",
		strings.Replace(lookupEnvelope, `"lookup"`, `"undeclared"`, 1),
		strings.Replace(lookupEnvelope, `{"key":"x"}`, `"encoded"`, 1),
		strings.Replace(lookupEnvelope, `"content":null`, `"content":"already executed"`, 1),
		`{"content":"x"}`, `{"content":"x","tool_calls":null}`,
		`{"content":null,"tool_calls":[]}`, `{"content":"x","tool_calls":[],"extra":1}`,
		`{"content":"` + strings.Repeat("x", maxToolEnvelopeBytes) + `","tool_calls":[]}`,
	} {
		if _, _, err := req.Adapter.decode(Result{Text: raw}); err == nil {
			t.Fatalf("accepted invalid output %.80q", raw)
		}
	}
	req.Adapter.Choice = "required"
	if _, _, err := req.Adapter.decode(Result{Text: `{"content":"answer","tool_calls":[]}`}); err == nil {
		t.Fatal("required accepted answer")
	}
	req.Adapter.Choice = "none"
	if _, _, err := req.Adapter.decode(Result{Text: lookupEnvelope}); err == nil {
		t.Fatal("none accepted call")
	}
	text, calls, err = req.Adapter.decode(Result{Text: "ignored", StructuredOutput: json.RawMessage(`{"content":"answer","tool_calls":[]}`)})
	if err != nil || text != "answer" || len(calls) != 0 {
		t.Fatalf("structured_output: %q %+v %v", text, calls, err)
	}
	req.Adapter.Choice = "auto"
	req.Adapter.Parallel = false
	if _, _, err := req.Adapter.decode(Result{Text: `{"content":null,"tool_calls":[{"name":"lookup","arguments":{}},{"name":"lookup","arguments":{}}]}`}); err == nil {
		t.Fatal("parallel false accepted multiple calls")
	}
}

func TestAdapterHTTPBothSurfacesAndNoEnvelopeLeak(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, valid := range []bool{false, true} {
			raw := lookupEnvelope
			if !valid {
				raw = "not an envelope"
			}
			ex := &fakeExec{res: Result{Text: raw, Usage: Usage{InputTokens: 100, OutputTokens: 20}}}
			s := httptest.NewServer((&Server{Runner: ex, Token: "tok", Models: ParseModels(measuredModels), ExperimentalTools: true}).Handler())
			body := strings.TrimSuffix(lookupRequest, "}") + `,"stream":` + strconv.FormatBool(stream) + `,"stream_options":{"include_usage":true}}`
			resp := post(t, s.URL, "tok", body)
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			s.Close()
			if ex.calls != 1 {
				t.Fatal("wrong runner calls")
			}
			if !valid {
				if resp.StatusCode != 502 || strings.Contains(string(data), "data: ") {
					t.Fatalf("invalid output stream=%v: %d %s", stream, resp.StatusCode, data)
				}
				continue
			}
			if resp.StatusCode != 200 || !strings.Contains(string(data), `"finish_reason":"tool_calls"`) || !strings.Contains(string(data), `"name":"lookup"`) || strings.Contains(string(data), `"content":"po`) {
				t.Fatalf("stream=%v: %d %s", stream, resp.StatusCode, data)
			}
			assertCallerToolWire(t, data, stream)
		}
	}
}

func TestAdapterRunnerSchemaFileAndTerminalStructuredOutput(t *testing.T) {
	structured := `{"content":null,"tool_calls":[{"name":"lookup","arguments":{"key":"x"}}]}`
	result, _ := json.Marshal(map[string]any{"event": "result", "result": map[string]any{"status": "SUCCESS", "response": "should never stream", "structured_output": json.RawMessage(structured)}})
	bin, dir := fakeAgy(t, `{"event":"init","init":{"permission_mode":"request-review"}}`+"\n"+string(result), 0)
	req, err := parseRequest([]byte(lookupRequest), true)
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Bin: bin, Home: t.TempDir(), Timeout: 10 * time.Second}
	streamed := false
	res, err := r.Run(context.Background(), req, func(string) error { streamed = true; return nil })
	if err != nil || streamed || string(res.StructuredOutput) != structured {
		t.Fatalf("streamed=%v result=%+v err=%v", streamed, res, err)
	}
	args := strings.Split(read(t, dir+"/argv"), "\n")
	found := false
	for i, arg := range args {
		if arg == "--json-schema" {
			found = true
			if i+1 >= len(args) || filepath.Base(args[i+1]) != "caller-tools-schema.json" {
				t.Fatal("schema not passed by file")
			}
			if _, err := os.Stat(args[i+1]); !os.IsNotExist(err) {
				t.Fatal("workspace/schema not removed")
			}
		}
	}
	if !found {
		t.Fatal("schema flag missing")
	}
}

// Two real turns, no native filesystem execution: the caller provides a fake
// lookup result. This proves the CLI can select a tool and consume its result.
func TestLiveExperimentalToolRoundTrip(t *testing.T) {
	bin, home := liveCLI(t)
	model := os.Getenv("EZLLM_AGY_MODEL")
	if model == "" {
		model = "gemini-3.8-flash-low"
	}
	runner := &Runner{Bin: bin, Home: home, Timeout: 120 * time.Second}
	body := strings.Replace(lookupRequest, `"gemini-3.8-flash-low"`, strconv.Quote(model), 1)
	body = strings.Replace(body, `"messages":`, `"tool_choice":"auto","parallel_tool_calls":false,"messages":`, 1)
	req, err := parseRequest([]byte(body), true)
	if err != nil {
		t.Fatal(err)
	}
	res, err := runner.Run(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, calls, err := req.Adapter.decode(res)
	if err != nil || len(calls) != 1 {
		t.Fatalf("first turn: calls=%+v err=%v structured=%s text=%s", calls, err, res.StructuredOutput, res.Text)
	}
	if res.ToolSteps != 0 {
		t.Fatalf("attempted %d native tools", res.ToolSteps)
	}
	t.Logf("call=%s args=%s usage=%+v", calls[0].Function.Name, calls[0].Function.Arguments, res.Usage)
	nonce := "caller_nonce_" + randomHex(8)
	var original map[string]json.RawMessage
	_ = json.Unmarshal([]byte(lookupRequest), &original)
	follow, _ := json.Marshal(map[string]any{"model": model, "tools": original["tools"], "tool_choice": "none", "messages": []any{
		map[string]any{"role": "user", "content": "Look up key x using the caller tool. After the tool result, repeat its exact value as your final answer."},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": calls},
		map[string]any{"role": "tool", "tool_call_id": calls[0].ID, "content": nonce},
	}})
	req, err = parseRequest(follow, true)
	if err != nil {
		t.Fatal(err)
	}
	res, err = runner.Run(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	text, calls, err := req.Adapter.decode(res)
	if err != nil || len(calls) != 0 || !strings.Contains(text, nonce) || res.ToolSteps != 0 {
		t.Fatalf("follow-up: text=%q calls=%+v err=%v native=%d structured=%s raw=%s", text, calls, err, res.ToolSteps, res.StructuredOutput, res.Text)
	}
	t.Logf("caller result consumed; usage=%+v", res.Usage)
}

func TestAdapterRefusesNativeToolsAndSupportsNestedSteps(t *testing.T) {
	req, err := parseRequest([]byte(lookupRequest), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{
		`{"event":"step_update","step_type":"tool","state":"DONE"}`,
		`{"event":"step_update","step_update":{"step_type":"tool","state":"DONE"}}`,
		`{"event":"step_update","step_update":{"step_type":"tool","state":"ACTIVE"}}`,
	} {
		bin, _ := fakeAgy(t, `{"event":"init","init":{"permission_mode":"request-review"}}`+"\n"+step+"\n"+`{"event":"result","result":{"status":"SUCCESS","response":"should not reach client"}}`, 0)
		r := &Runner{Bin: bin, Home: t.TempDir(), Timeout: 10 * time.Second}
		_, err := r.Run(context.Background(), req, nil)
		if err == nil || asNative(err).Code != "invalid_tool_output" {
			t.Fatalf("native tool accepted: %v", err)
		}
	}
	// Ordinary chat also understands the current nested event shape.
	bin, _ := fakeAgy(t, `{"event":"init","init":{"permission_mode":"request-review"}}
{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"nested"}}
{"event":"result","result":{"status":"SUCCESS","response":"nested"}}`, 0)
	r := &Runner{Bin: bin, Home: t.TempDir(), Timeout: 10 * time.Second}
	text := ""
	_, err = r.Run(context.Background(), Request{Model: "m", Prompt: "x"}, func(s string) error { text += s; return nil })
	if err != nil || text != "nested" {
		t.Fatalf("nested text=%q err=%v", text, err)
	}
}

func TestAdapterHTTPDisabledDoesNotSpawn(t *testing.T) {
	ex := &fakeExec{}
	srv := newTestServer(ex)
	defer srv.Close()
	resp := post(t, srv.URL, "tok", lookupRequest)
	defer resp.Body.Close()
	if resp.StatusCode != 400 || ex.calls != 0 {
		t.Fatalf("status=%d calls=%d", resp.StatusCode, ex.calls)
	}
}

func TestAdapterNamedChoiceAndSchemaReferences(t *testing.T) {
	req, err := parseRequest([]byte(strings.Replace(lookupRequest, `"messages":`, `"tool_choice":{"type":"function","function":{"name":"lookup"}},"messages":`, 1)), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, calls, err := req.Adapter.decode(Result{Text: lookupEnvelope}); err != nil || len(calls) != 1 {
		t.Fatalf("named: %+v %v", calls, err)
	}
	if _, _, err := req.Adapter.decode(Result{Text: `{"content":"no call","tool_calls":[]}`}); err == nil {
		t.Fatal("named accepted final answer")
	}
	ref := strings.Replace(lookupRequest, `"key":{"type":"string"}`, `"key":{"$ref":"#/$defs/key"}`, 1)
	if _, err := parseRequest([]byte(ref), true); err == nil {
		t.Fatal("embedded reference silently accepted")
	}
}

func TestPreflightDetectsOptionalStructuredOutput(t *testing.T) {
	for _, supported := range []bool{false, true} {
		help := strings.Join(RequiredFlags, " ") + " "
		if supported {
			help += "--json-schema "
		}
		// The fake returns the flag surface plus a model row for all commands.
		bin, _ := fakeAgy(t, help+"\n"+measuredModels, 0)
		home := t.TempDir()
		path := TokenPath(home)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		pre, err := RunPreflight(context.Background(), bin, home)
		if err != nil || pre.StructuredOutput != supported {
			t.Fatalf("supported=%v pre=%+v err=%v", supported, pre, err)
		}
	}
}

// Check the actual wire fields a harness uses to dispatch and correlate calls.
func assertCallerToolWire(t *testing.T, data []byte, stream bool) {
	t.Helper()
	check := func(raw json.RawMessage) {
		t.Helper()
		var calls []ToolCall
		if json.Unmarshal(raw, &calls) != nil || len(calls) != 1 || !strings.HasPrefix(calls[0].ID, "call_") || calls[0].Type != "function" || calls[0].Function.Name != "lookup" || calls[0].Function.Arguments != `{"key":"x"}` {
			t.Fatalf("invalid caller wire calls: %s", raw)
		}
	}
	if !stream {
		var response struct {
			Choices []struct {
				Message struct {
					Content   any
					ToolCalls json.RawMessage `json:"tool_calls"`
				}
				Finish string `json:"finish_reason"`
			}
			Usage struct {
				Input  int `json:"prompt_tokens"`
				Output int `json:"completion_tokens"`
			}
		}
		if json.Unmarshal(data, &response) != nil || len(response.Choices) != 1 || response.Choices[0].Message.Content != nil || response.Choices[0].Finish != "tool_calls" || response.Usage.Input != 100 || response.Usage.Output != 20 {
			t.Fatalf("invalid completion: %s", data)
		}
		check(response.Choices[0].Message.ToolCalls)
		return
	}
	callChunks, finishEvents, usageEvents := 0, 0, 0
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var event struct {
			Choices []struct {
				Delta struct {
					Content   string
					ToolCalls json.RawMessage `json:"tool_calls"`
				}
				Finish string `json:"finish_reason"`
			}
			Usage *struct {
				Input  int `json:"prompt_tokens"`
				Output int `json:"completion_tokens"`
			}
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil {
			t.Fatal("invalid SSE JSON")
		}
		for _, c := range event.Choices {
			if c.Delta.Content != "" {
				t.Fatal("envelope leaked as content")
			}
			if present(c.Delta.ToolCalls) {
				check(c.Delta.ToolCalls)
				var indexed []struct {
					Index *int `json:"index"`
				}
				_ = json.Unmarshal(c.Delta.ToolCalls, &indexed)
				if len(indexed) != 1 || indexed[0].Index == nil || *indexed[0].Index != 0 {
					t.Fatal("missing tool-call stream index")
				}
				callChunks++
			}
			if c.Finish == "tool_calls" {
				finishEvents++
			}
		}
		if event.Usage != nil && event.Usage.Input == 100 && event.Usage.Output == 20 {
			usageEvents++
		}
	}
	if callChunks != 1 || finishEvents != 1 || usageEvents != 1 || !strings.HasSuffix(string(data), "data: [DONE]\n\n") {
		t.Fatalf("incomplete tool stream: %s", data)
	}
}

func TestAdapterHTTPFinalAnswer(t *testing.T) {
	for _, stream := range []bool{false, true} {
		ex := &fakeExec{res: Result{StructuredOutput: json.RawMessage(`{"content":"caller result consumed","tool_calls":[]}`)}}
		srv := httptest.NewServer((&Server{Runner: ex, Token: "tok", Models: ParseModels(measuredModels), ExperimentalTools: true}).Handler())
		body := strings.TrimSuffix(lookupRequest, "}") + `,"stream":` + strconv.FormatBool(stream) + `}`
		resp := post(t, srv.URL, "tok", body)
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		srv.Close()
		if err != nil || resp.StatusCode != 200 || !strings.Contains(string(data), `"content":"caller result consumed"`) || !strings.Contains(string(data), `"finish_reason":"stop"`) || strings.Contains(string(data), `"tool_calls"`) {
			t.Fatalf("stream=%v: %d %s read=%v", stream, resp.StatusCode, data, err)
		}
	}
}

// agy rejects even otherwise valid schemas unless the root explicitly declares
// type object. In particular auto choice needs this alongside its anyOf.
func TestAdapterSchemasMeetCLIRootRequirement(t *testing.T) {
	for _, choice := range []string{`"auto"`, `"none"`, `"required"`, `{"type":"function","function":{"name":"lookup"}}`} {
		body := strings.Replace(lookupRequest, `"messages":`, `"tool_choice":`+choice+`,"messages":`, 1)
		req, err := parseRequest([]byte(body), true)
		if err != nil {
			t.Fatal(err)
		}
		var schema map[string]any
		if json.Unmarshal(req.Adapter.Schema, &schema) != nil || schema["type"] != "object" || schema["properties"] == nil {
			t.Fatalf("choice %s: CLI-incompatible schema %s", choice, req.Adapter.Schema)
		}
	}
}

func TestAdapterAllowsOnlyStructuredOutputFinish(t *testing.T) {
	req, err := parseRequest([]byte(lookupRequest), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"finish", "run_command", "write_to_file", "finish_command"} {
		step, _ := json.Marshal(map[string]any{"event": "step_update", "step_update": map[string]any{"step_type": "tool", "tool_name": tool, "state": "ACTIVE"}})
		result, _ := json.Marshal(map[string]any{"event": "result", "result": map[string]any{"status": "SUCCESS", "structured_output": json.RawMessage(lookupEnvelope)}})
		bin, _ := fakeAgy(t, `{"event":"init","init":{"permission_mode":"request-review"}}`+"\n"+string(step)+"\n"+string(result), 0)
		r := &Runner{Bin: bin, Home: t.TempDir(), Timeout: 10 * time.Second}
		res, err := r.Run(context.Background(), req, nil)
		if tool != "finish" {
			if err == nil || asNative(err).Code != "invalid_tool_output" {
				t.Fatalf("native %s accepted: %v", tool, err)
			}
			continue
		}
		if err != nil || res.ToolSteps != 0 {
			t.Fatalf("finish rejected: %+v %v", res, err)
		}
		if _, calls, err := req.Adapter.decode(res); err != nil || len(calls) != 1 {
			t.Fatalf("finish output: %+v %v", calls, err)
		}
	}
}

func TestDefaultTextRequestsKeepChoiceHintsCompatible(t *testing.T) {
	for _, choice := range []string{"auto", "none"} {
		body := `{"model":"m","tools":[],"tool_choice":"` + choice + `","messages":[{"role":"user","content":"plain chat"}]}`
		req, err := ParseRequest([]byte(body))
		if err != nil || req.Adapter != nil || req.Prompt != "plain chat" {
			t.Fatalf("choice=%s prompt=%q err=%v", choice, req.Prompt, err)
		}
	}
}
