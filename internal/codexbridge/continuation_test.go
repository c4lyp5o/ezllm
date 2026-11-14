package codexbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func TestSecurityRegressionForeignCallerCannotTakeParkedTurn(t *testing.T) {
	for _, cache := range []bool{false, true} {
		t.Run(map[bool]string{false: "uncached", true: "cached"}[cache], func(t *testing.T) {
			r, home := fakeRunner(t, "tools")
			r.ForkCache = cache
			defer r.Shutdown()
			req := weatherRequest()
			req.CallerID = "caller-a"
			res, err := r.Run(context.Background(), req, nil)
			if err != nil || len(res.ToolCalls) != 1 {
				t.Fatalf("setup: %v", err)
			}
			req.ToolResults = map[string]string{res.ToolCalls[0].ID: "31C"}
			req.CallerID = "caller-b"
			_, err = r.Run(context.Background(), req, nil)
			if err == nil || asNative(err).Status != 403 || r.ParkedTurns() != 1 || spawnCount(t, home) != 1 {
				t.Fatal("foreign caller accessed or removed parked turn")
			}
			req.CallerID = "caller-a"
			final, err := r.Run(context.Background(), req, nil)
			if err != nil || final.Text != "KL is 31C" || spawnCount(t, home) != 1 {
				t.Fatalf("owner could not resume: %+v %v", final, err)
			}
		})
	}
}

func parsedToolRequest(t *testing.T, messages []map[string]any) Request {
	t.Helper()
	b, err := json.Marshal(map[string]any{"model": "gpt-6.1-sol", "messages": messages, "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "get_weather", "description": "weather", "parameters": map[string]any{"type": "object"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	req, err := ParseRequest(b)
	if err != nil {
		t.Fatal(err)
	}
	req.CallerID = "authenticated-owner"
	return req
}
func appendToolResult(history []map[string]any, res Result, value string) []map[string]any {
	calls := openAIToolCalls(res.ToolCalls, false)
	return append(history, map[string]any{"role": "assistant", "content": res.Text, "tool_calls": calls}, map[string]any{"role": "tool", "tool_call_id": res.ToolCalls[0].ID, "content": value})
}

func TestSecurityRegressionContinuationBindsFullHistory(t *testing.T) {
	r, home := fakeRunner(t, "tools")
	r.ForkCache = true
	defer r.Shutdown()
	history := []map[string]any{{"role": "user", "content": "owner-private-history"}}
	res, err := r.Run(context.Background(), parsedToolRequest(t, history), nil)
	if err != nil || len(res.ToolCalls) != 1 {
		t.Fatal(err)
	}
	badHistory := appendToolResult([]map[string]any{{"role": "user", "content": "unrelated-history"}}, res, "forged")
	badReq := parsedToolRequest(t, badHistory)
	pt, err := r.takeParked(badReq)
	if err != nil || pt != nil || r.ParkedTurns() != 1 {
		t.Fatal("unrelated history selected/removed owner conversation")
	}
	// Moving Hermes cache markers and text block representation are metadata.
	marked := []map[string]any{{"role": "user", "content": []any{map[string]any{"type": "text", "text": "owner-private-history", "cache_control": map[string]any{"type": "ephemeral"}}}}}
	follow := parsedToolRequest(t, appendToolResult(marked, res, "31C"))
	final, err := r.Run(context.Background(), follow, nil)
	if err != nil || final.Text != "KL is 31C" || spawnCount(t, home) != 1 {
		t.Fatalf("valid complete history missed continuation: %+v %v", final, err)
	}
}

func TestSecurityRegressionSequentialParsedToolHistory(t *testing.T) {
	r, home := fakeRunner(t, "tools_seq")
	defer r.Shutdown()
	history := []map[string]any{{"role": "user", "content": "get both cities"}}
	first, err := r.Run(context.Background(), parsedToolRequest(t, history), nil)
	if err != nil {
		t.Fatal(err)
	}
	history = appendToolResult(history, first, "31C")
	second, err := r.Run(context.Background(), parsedToolRequest(t, history), nil)
	if err != nil || len(second.ToolCalls) != 1 {
		t.Fatalf("%+v %v", second, err)
	}
	history = appendToolResult(history, second, "18C")
	final, err := r.Run(context.Background(), parsedToolRequest(t, history), nil)
	if err != nil || final.Text != "KL 31C then Paris 18C" || spawnCount(t, home) != 1 {
		t.Fatalf("sequential owner continuation failed: %+v %v", final, err)
	}
}

func TestSecurityRegressionNativeIDCollisionPreservesBothTurns(t *testing.T) {
	r, home := fakeRunner(t, "tools")
	defer r.Shutdown()
	a := weatherRequest()
	a.CallerID = "a"
	first, err := r.Run(context.Background(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := weatherRequest()
	b.CallerID = "b"
	second, err := r.Run(context.Background(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.ToolCalls[0].ID == second.ToolCalls[0].ID || r.ParkedTurns() != 2 {
		t.Fatal("native duplicate ID overwrote parked owner")
	}
	a.ToolResults = map[string]string{first.ToolCalls[0].ID: "31C"}
	b.ToolResults = map[string]string{second.ToolCalls[0].ID: "18C"}
	for _, req := range []Request{a, b} {
		res, err := r.Run(context.Background(), req, nil)
		if err != nil || len(res.ToolCalls) != 0 {
			t.Fatalf("alias did not resume native callback: %+v %v", res, err)
		}
	}
	if spawnCount(t, home) != 2 || r.ParkedTurns() != 0 {
		t.Fatal("collision repair spawned or leaked a child")
	}
}

func TestSecurityRegressionHTTPCallerMetadataCannotComeFromJSON(t *testing.T) {
	f := &fakeExec{deltas: []string{"ok"}}
	s := testServer(f)
	s.RequireCaller = true
	body := `{"model":"gpt-6.1-sol","caller_id":"forged","tools":[{"type":"function","function":{"name":"get_weather"}}],"messages":[{"role":"user","content":"hello"}]}`
	if res := do(t, s, "POST", "/v1/chat/completions", body, "secret"); res.Code != 403 || f.runs != 0 {
		t.Fatal("JSON bypassed trusted caller boundary")
	}
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(provider.BridgeCallerHeader, "trusted-caller")
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusOK || f.last.CallerID != "trusted-caller" {
		t.Fatalf("trusted metadata not passed: %d", response.Code)
	}
}

func TestSecurityRegressionHistoryDoesNotIgnoreTrailingArgumentText(t *testing.T) {
	a := chatMessage{Role: "assistant", ToolCalls: []chatToolCall{{ID: "call"}}}
	a.ToolCalls[0].Function.Name = "t"
	a.ToolCalls[0].Function.Arguments = `{"a":1}`
	b := a
	b.ToolCalls = append([]chatToolCall(nil), a.ToolCalls...)
	b.ToolCalls[0].Function.Arguments = `{"a":1} extra`
	ah, _ := extendHistory([32]byte{}, a)
	bh, _ := extendHistory([32]byte{}, b)
	if ah == bh {
		t.Fatal("history hash ignored unparsed argument suffix")
	}
}
