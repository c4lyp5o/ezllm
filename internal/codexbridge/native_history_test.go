package codexbridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func nativeHistoryRequest(t *testing.T) Request {
	t.Helper()
	req, err := ParseRequest([]byte(`{"model":"gpt-6.1-sol","tools":[{"type":"function","function":{"name":"terminal"}}],"messages":[{"role":"system","content":"You are Hermes."},{"role":"user","content":[{"type":"text","text":"Build it."},{"type":"image_url","image_url":{"url":"data:image/png;base64,OLD"}},{"type":"text","text":"Use this layout."}]},{"role":"assistant","content":"Checking.","tool_calls":[{"id":"old-call","type":"function","function":{"name":"terminal","arguments":"{\"command\":\"bun test\"}"}}]},{"role":"tool","tool_call_id":"old-call","content":"{\"exit_code\":1,\"error\":\"not ready\"}"},{"role":"assistant","phase":"commentary","content":"The implementation is still in progress."},{"role":"user","content":[{"type":"text","text":"Continue."},{"type":"image_url","image_url":{"url":"data:image/png;base64,NEW"}},{"type":"text","text":"Finish and verify."}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestNativeHistoryPreservesRolesPhasesAndToolRelationships(t *testing.T) {
	req := nativeHistoryRequest(t)
	items, input, err := nativeConversation(req.turns)
	if err != nil {
		t.Fatal(err)
	}
	want := []UserInput{{Type: "text", Text: "Continue."}, {Type: "image", URL: "data:image/png;base64,NEW"}, {Type: "text", Text: "Finish and verify."}}
	if !reflect.DeepEqual(input, want) {
		t.Fatalf("latest input lost ordering: %+v", input)
	}
	b, _ := json.Marshal(items)
	expected := `[{"content":[{"text":"Build it.","type":"input_text"},{"image_url":"data:image/png;base64,OLD","type":"input_image"},{"text":"Use this layout.","type":"input_text"}],"role":"user","type":"message"},{"content":[{"text":"Checking.","type":"output_text"}],"phase":"commentary","role":"assistant","type":"message"},{"arguments":"{\"command\":\"bun test\"}","call_id":"old-call","name":"terminal","type":"function_call"},{"call_id":"old-call","output":"{\"exit_code\":1,\"error\":\"not ready\"}","type":"function_call_output"},{"content":[{"text":"The implementation is still in progress.","type":"output_text"}],"phase":"commentary","role":"assistant","type":"message"}]`
	if string(b) != expected {
		t.Fatalf("native history mismatch:\n%s", b)
	}
	if strings.Contains(string(b), "Continue.") || strings.Contains(string(b), "You are Hermes.") {
		t.Fatal("latest user/system duplicated in native history")
	}
}

func TestNativeHistoryToolResultReplayDoesNotInventUserContent(t *testing.T) {
	req := nativeHistoryRequest(t)
	turns := req.turns[:3] // user, assistant call, result
	items, input, err := nativeConversation(turns)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 || items[3].(map[string]any)["type"] != "function_call_output" {
		t.Fatalf("tool output not preserved: %+v", items)
	}
	if len(input) != 1 || !strings.Contains(input[0].Text, "Continue the user's request") {
		t.Fatal("missing continuation input")
	}
}

func TestNativeHistoryPhasesAreValidatedAndBindContinuation(t *testing.T) {
	req := nativeHistoryRequest(t)
	m := req.turns[1]
	a, err := messageCanonical(m)
	if err != nil {
		t.Fatal(err)
	}
	m.Phase = "commentary"
	b, _ := messageCanonical(m)
	if string(a) != string(b) {
		t.Fatal("equivalent inferred/explicit commentary changes continuation identity")
	}
	m.Phase = "final_answer"
	b, _ = messageCanonical(m)
	if string(a) == string(b) {
		t.Fatal("different semantic phase shared continuation identity")
	}
	_, err = ParseRequest([]byte(`{"model":"m","messages":[{"role":"assistant","phase":"invalid","content":"hi"},{"role":"user","content":"continue"}]}`))
	if err == nil {
		t.Fatal("invalid phase accepted")
	}
}

func TestNativeHistoryIsInjectedIntoRequestThreadAfterFork(t *testing.T) {
	for _, cache := range []bool{false, true} {
		t.Run(fmt.Sprintf("cache=%t", cache), func(t *testing.T) {
			r, home := fakeRunner(t, "ok")
			r.NativeHistory = true
			r.ForkCache = cache
			req := nativeHistoryRequest(t)
			for i := 0; i < 2; i++ {
				if _, err := r.Run(context.Background(), req, nil); err != nil {
					t.Fatal(err)
				}
			}
			b, err := os.ReadFile(filepath.Join(home, "turn_start.json"))
			if err != nil {
				t.Fatal(err)
			}
			var start struct {
				Input []UserInput `json:"input"`
			}
			if err := json.Unmarshal(b, &start); err != nil {
				t.Fatal(err)
			}
			if len(start.Input) != 3 || start.Input[0].Text != "Continue." {
				t.Fatal("flattened history still sent as user input")
			}
			trace, err := os.Open(filepath.Join(home, "cache_rpc.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			defer trace.Close()
			scan := bufio.NewScanner(trace)
			forks, history, seed := 0, 0, 0
			for scan.Scan() {
				var rpc struct {
					Method string `json:"method"`
					Params struct {
						Items []map[string]any `json:"items"`
					} `json:"params"`
				}
				if err := json.Unmarshal(scan.Bytes(), &rpc); err != nil {
					t.Fatal(err)
				}
				if rpc.Method == "thread/fork" {
					forks++
				}
				if rpc.Method != "thread/inject_items" {
					continue
				}
				if len(rpc.Params.Items) == 1 {
					raw, _ := json.Marshal(rpc.Params.Items)
					if string(raw) != `[{"content":[{"text":"","type":"input_text"}],"role":"user","type":"message"}]` {
						t.Fatal("caller content entered template seed")
					}
					seed++
					continue
				}
				if len(rpc.Params.Items) != 5 {
					t.Fatal("missing history items")
				}
				history++
				if cache && forks != history {
					t.Fatal("caller history injected before request fork")
				}
			}
			if err := scan.Err(); err != nil {
				t.Fatal(err)
			}
			if history != 2 || (cache && (seed != 1 || r.CacheHits() != 1)) {
				t.Fatalf("history=%d seed=%d cache hits=%d", history, seed, r.CacheHits())
			}
		})
	}
}

func TestNativeHistoryInjectionFailureNeverStartsInference(t *testing.T) {
	r, home := fakeRunner(t, "inject_error")
	r.NativeHistory = true
	_, err := r.Run(context.Background(), nativeHistoryRequest(t), nil)
	if err == nil {
		t.Fatal("injection failure ignored")
	}
	if _, err := os.Stat(filepath.Join(home, "turn_start.json")); !os.IsNotExist(err) {
		t.Fatal("inference started after failed history injection")
	}
}

func TestNativeHistoryParkedContinuationDoesNotReplay(t *testing.T) {
	r, home := fakeRunner(t, "tools")
	r.NativeHistory = true
	req, err := ParseRequest([]byte(`{"model":"m","tools":[{"type":"function","function":{"name":"get_weather"}}],"messages":[{"role":"user","content":"Weather?"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Run(context.Background(), req, nil)
	if err != nil || len(res.ToolCalls) != 1 {
		t.Fatalf("first tool call: %+v %v", res, err)
	}
	body, _ := json.Marshal(map[string]any{"model": "m", "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}}, "messages": []any{map[string]any{"role": "user", "content": "Weather?"}, map[string]any{"role": "assistant", "content": res.Text, "tool_calls": openAIToolCalls(res.ToolCalls, false)}, map[string]any{"role": "tool", "tool_call_id": res.ToolCalls[0].ID, "content": "31C"}}})
	next, err := ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Run(context.Background(), next, nil)
	if err != nil || got.Text != "KL is 31C" {
		t.Fatalf("native continuation: %+v %v", got, err)
	}
	if spawnCount(t, home) != 1 {
		t.Fatal("matching tool results replayed instead of resuming")
	}
}

func TestNativeHistoryIncompleteToolGroupsUseLegacyReplayBeforeInference(t *testing.T) {
	r, home := fakeRunner(t, "ok")
	r.NativeHistory = true
	// Context compression or an interrupted tool batch may leave an orphan.
	req, err := ParseRequest([]byte(`{"model":"m","messages":[{"role":"tool","tool_call_id":"missing","content":"historical output"},{"role":"user","content":"continue"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, "turn_start.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "historical output") {
		t.Fatal("legacy fallback lost orphan result")
	}
	if _, err := os.Stat(filepath.Join(home, "cache_rpc.jsonl")); !os.IsNotExist(err) {
		t.Fatal("incomplete group was injected into native history")
	}
}

func TestNativeHistoryToolOutputRetainsImageOrder(t *testing.T) {
	req := nativeHistoryRequest(t)
	req.turns = req.turns[:3]
	req.turns[2].Content = json.RawMessage(`[{"type":"text","text":"before"},{"type":"image_url","image_url":{"url":"data:image/png;base64,TOOL"}},{"type":"text","text":"after"}]`)
	items, _, err := nativeConversation(req.turns)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(items[3])
	if string(b) != `{"call_id":"old-call","output":[{"text":"before","type":"input_text"},{"image_url":"data:image/png;base64,TOOL","type":"input_image"},{"text":"after","type":"input_text"}],"type":"function_call_output"}` {
		t.Fatalf("tool output changed: %s", b)
	}
}

func TestSecurityRegressionNativeImageOrderBindsParkedTurn(t *testing.T) {
	r, home := fakeRunner(t, "tools")
	r.NativeHistory = true
	image := map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,SAME"}}
	text := map[string]any{"type": "text", "text": "Analyze this image."}
	history := []map[string]any{{"role": "user", "content": []any{text, image}}}
	res, err := r.Run(context.Background(), parsedToolRequest(t, history), nil)
	if err != nil || len(res.ToolCalls) != 1 {
		t.Fatalf("setup: %+v %v", res, err)
	}
	changed := []map[string]any{{"role": "user", "content": []any{image, text}}}
	bad := parsedToolRequest(t, appendToolResult(changed, res, "forged"))
	pt, err := r.takeParked(bad)
	if err != nil || pt != nil || r.ParkedTurns() != 1 {
		t.Fatal("changed image order consumed a different native history")
	}
	final, err := r.Run(context.Background(), parsedToolRequest(t, appendToolResult(history, res, "31C")), nil)
	if err != nil || final.Text != "KL is 31C" || spawnCount(t, home) != 1 {
		t.Fatalf("rightful continuation failed: %+v %v", final, err)
	}
}
