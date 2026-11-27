package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// chatStub is a chat-completions upstream that records the body it got.
type chatStub struct {
	mu     sync.Mutex
	last   map[string]any
	status int
	reply  string   // a whole chat completion
	events []string // SSE data payloads, for stream:true
}

func (c *chatStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		c.mu.Lock()
		c.last = body
		c.mu.Unlock()
		if c.status >= 400 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(c.status)
			_, _ = io.WriteString(w, `{"error":{"message":"slow down","type":"rate_limit_error"}}`)
			return
		}
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, e := range c.events {
				_, _ = io.WriteString(w, "data: "+e+"\n\n")
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, c.reply)
	}
}

func (c *chatStub) got() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

func geminiDo(t *testing.T, h *harness, path, apiKey, body string) (int, http.Header, string) {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("x-goog-api-key", apiKey)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec.Code, rec.Header(), rec.Body.String()
}

const geminiToolTurn = `{
  "systemInstruction": {"parts": [{"text": "Be brief."}]},
  "contents": [
    {"role": "user", "parts": [{"text": "Weather in Paris?"}]},
    {"role": "model", "parts": [{"functionCall": {"name": "weather", "args": {"city": "Paris"}}}]},
    {"role": "user", "parts": [{"functionResponse": {"name": "weather", "response": {"temp": 21}}}]},
    {"role": "user", "parts": [{"text": "And Rome?"}]}
  ],
  "tools": [{"functionDeclarations": [{"name": "weather", "description": "Current weather",
    "parameters": {"type": "OBJECT", "properties": {"city": {"type": "STRING"}}, "required": ["city"]}}]}],
  "toolConfig": {"functionCallingConfig": {"mode": "ANY", "allowedFunctionNames": ["weather"]}},
  "generationConfig": {"temperature": 0.2, "maxOutputTokens": 256, "stopSequences": ["END"]}
}`

// generateContent with tools: the chat body carries the conversation, the
// tools and the settings; the answer comes back in Gemini's shape.
func TestGeminiGenerateContentWithTools(t *testing.T) {
	stub := &chatStub{reply: `{"id":"chatcmpl-1","model":"gpt-6-luna","choices":[{"index":0,"message":{"role":"assistant","content":"Checking.",
	  "tool_calls":[{"id":"call_9","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Rome\"}"}}]},"finish_reason":"tool_calls"}],
	  "usage":{"prompt_tokens":40,"completion_tokens":12,"total_tokens":52,"prompt_tokens_details":{"cached_tokens":8}}}`}
	h := newHarness(t, stub.handler())
	code, hdr, body := geminiDo(t, h, "/v1beta/models/up/gpt-6-luna:generateContent", "tok-infer", geminiToolTurn)
	if code != http.StatusOK {
		t.Fatalf("generateContent: %d %s", code, body)
	}
	if !strings.HasPrefix(hdr.Get("Content-Type"), "application/json") {
		t.Errorf("content type = %q", hdr.Get("Content-Type"))
	}
	sent := stub.got()
	msgs, _ := sent["messages"].([]any)
	if sent["model"] != "gpt-6-luna" || len(msgs) != 5 {
		t.Fatalf("chat body = %v", sent)
	}
	roles := []string{}
	for _, m := range msgs {
		roles = append(roles, m.(map[string]any)["role"].(string))
	}
	if strings.Join(roles, ",") != "system,user,assistant,tool,user" {
		t.Errorf("roles = %v", roles)
	}
	call := msgs[2].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if msgs[3].(map[string]any)["tool_call_id"] != call["id"] {
		t.Errorf("the function response is not tied to its call: %v vs %v", msgs[3], call)
	}
	tool := sent["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	params := tool["parameters"].(map[string]any)
	if params["type"] != "object" || params["properties"].(map[string]any)["city"].(map[string]any)["type"] != "string" {
		t.Errorf("schema types not lowered: %v", params)
	}
	if choice := sent["tool_choice"].(map[string]any); choice["function"].(map[string]any)["name"] != "weather" {
		t.Errorf("tool_choice = %v", sent["tool_choice"])
	}
	if sent["max_tokens"] != float64(256) || sent["temperature"] != 0.2 {
		t.Errorf("generation config = max_tokens %v temperature %v", sent["max_tokens"], sent["temperature"])
	}

	var out struct {
		Candidates []struct {
			Content struct {
				Role  string `json:"role"`
				Parts []struct {
					Text         string `json:"text"`
					FunctionCall *struct {
						ID   string         `json:"id"`
						Name string         `json:"name"`
						Args map[string]any `json:"args"`
					} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		Usage map[string]int `json:"usageMetadata"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	c := out.Candidates[0]
	if c.Content.Role != "model" || c.Content.Parts[0].Text != "Checking." || c.Content.Parts[1].FunctionCall.Args["city"] != "Rome" || c.FinishReason != "STOP" {
		t.Fatalf("candidate = %+v", c)
	}
	if out.Usage["promptTokenCount"] != 40 || out.Usage["candidatesTokenCount"] != 12 || out.Usage["cachedContentTokenCount"] != 8 {
		t.Fatalf("usage = %v", out.Usage)
	}
}

var streamEvents = []string{
	`{"id":"c1","model":"gpt-6-luna","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`,
	`{"id":"c1","model":"gpt-6-luna","choices":[{"index":0,"delta":{"content":"lo"}}]}`,
	`{"id":"c1","model":"gpt-6-luna","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"ci"}}]}}]}`,
	`{"id":"c1","model":"gpt-6-luna","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"Oslo\"}"}}]}}]}`,
	`{"id":"c1","model":"gpt-6-luna","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	`{"id":"c1","model":"gpt-6-luna","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`,
}

// streamGenerateContent?alt=sse: text as it arrives, then one chunk with the
// whole function call, the finish reason and usage.
func TestGeminiStreamSSE(t *testing.T) {
	stub := &chatStub{events: streamEvents}
	h := newHarness(t, stub.handler())
	code, hdr, body := geminiDo(t, h, "/v1beta/models/up/gpt-6-luna:streamGenerateContent?alt=sse", "tok-infer", `{"contents":[{"parts":[{"text":"hi"}]}]}`)
	if code != http.StatusOK || !strings.HasPrefix(hdr.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream: %d %q %s", code, hdr.Get("Content-Type"), body)
	}
	if stub.got()["stream"] != true {
		t.Fatalf("the chat request was not streamed: %v", stub.got())
	}
	var chunks []map[string]any
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		if p, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			var c map[string]any
			if err := json.Unmarshal([]byte(p), &c); err != nil {
				t.Fatalf("chunk %q: %v", p, err)
			}
			chunks = append(chunks, c)
		}
	}
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 2 text + 1 final:\n%s", len(chunks), body)
	}
	text := func(c map[string]any) string {
		return c["candidates"].([]any)[0].(map[string]any)["content"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"].(string)
	}
	if text(chunks[0])+text(chunks[1]) != "Hello" {
		t.Errorf("text = %q", text(chunks[0])+text(chunks[1]))
	}
	last := chunks[2]["candidates"].([]any)[0].(map[string]any)
	fc := last["content"].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionCall"].(map[string]any)
	if fc["name"] != "weather" || fc["args"].(map[string]any)["city"] != "Oslo" || last["finishReason"] != "STOP" {
		t.Fatalf("final chunk = %v", last)
	}
	if u := chunks[2]["usageMetadata"].(map[string]any); u["totalTokenCount"] != float64(12) {
		t.Fatalf("usage = %v", u)
	}
}

// Without alt=sse the stream is one JSON array, written as it grows.
func TestGeminiStreamJSONArray(t *testing.T) {
	stub := &chatStub{events: streamEvents}
	h := newHarness(t, stub.handler())
	code, _, body := geminiDo(t, h, "/v1beta/models/up/gpt-6-luna:streamGenerateContent", "tok-infer", `{"contents":[{"parts":[{"text":"hi"}]}]}`)
	var arr []map[string]any
	if code != http.StatusOK || json.Unmarshal([]byte(body), &arr) != nil || len(arr) != 3 {
		t.Fatalf("array stream: %d %s", code, body)
	}
}

// The key in the URL is refused unless Settings allows it; the header and a
// bearer token always work.
func TestGeminiQueryKeyIsOffByDefault(t *testing.T) {
	stub := &chatStub{reply: `{"id":"c","model":"gpt-6-luna","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`}
	h := settingsHarness(t, stub.handler())
	body := `{"contents":[{"parts":[{"text":"hi"}]}]}`
	code, _, resp := geminiDo(t, h, "/v1beta/models/up/gpt-6-luna:generateContent?key=tok-infer", "", body)
	if code != http.StatusUnauthorized || !strings.Contains(resp, "x-goog-api-key") || !strings.Contains(resp, "UNAUTHENTICATED") {
		t.Fatalf("query key with the setting off: %d %s", code, resp)
	}
	if code, _, resp := geminiDo(t, h, "/v1beta/models/up/gpt-6-luna:generateContent", "tok-infer", body); code != http.StatusOK {
		t.Fatalf("header key: %d %s", code, resp)
	}
	if code, _, resp := h.do("POST", "/v1beta/models/up/gpt-6-luna:generateContent", "tok-infer", body); code != http.StatusOK {
		t.Fatalf("bearer: %d %s", code, resp)
	}
	m := h.server.settings
	if _, err := m.Patch(context.Background(), m.View().Revision, map[string]json.RawMessage{"gemini_query_key": json.RawMessage("true")}); err != nil {
		t.Fatal(err)
	}
	if code, _, resp := geminiDo(t, h, "/v1beta/models/up/gpt-6-luna:generateContent?key=tok-infer", "", body); code != http.StatusOK {
		t.Fatalf("query key with the setting on: %d %s", code, resp)
	}
	if code, _, _ := geminiDo(t, h, "/v1beta/models/up/gpt-6-luna:generateContent?key=wrong", "", body); code != http.StatusUnauthorized {
		t.Fatalf("a wrong query key: %d", code)
	}
}

// An upstream error arrives in Google's error shape with its status.
func TestGeminiErrorShape(t *testing.T) {
	h := newHarness(t, (&chatStub{status: http.StatusTooManyRequests}).handler())
	code, _, body := geminiDo(t, h, "/v1beta/models/up/gpt-6-luna:generateContent", "tok-infer", `{"contents":[{"parts":[{"text":"hi"}]}]}`)
	var e struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if code != http.StatusTooManyRequests || json.Unmarshal([]byte(body), &e) != nil || e.Error.Status != "RESOURCE_EXHAUSTED" || e.Error.Code != 429 || e.Error.Message == "" {
		t.Fatalf("error: %d %s", code, body)
	}
}

// Requests no other provider can serve are refused up front, saying why.
func TestGeminiRefusesGoogleOnlyFeatures(t *testing.T) {
	h := newHarness(t, (&chatStub{}).handler())
	for name, body := range map[string]string{
		"google search": `{"contents":[{"parts":[{"text":"hi"}]}],"tools":[{"googleSearch":{}}]}`,
		"pdf inline":    `{"contents":[{"parts":[{"inlineData":{"mimeType":"application/pdf","data":"AAAA"}}]}]}`,
		"candidates":    `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"candidateCount":2}}`,
	} {
		code, _, resp := geminiDo(t, h, "/v1beta/models/up/gpt-6-luna:generateContent", "tok-infer", body)
		if code != http.StatusBadRequest || !strings.Contains(resp, "not supported through ezllm") {
			t.Errorf("%s: %d %s", name, code, resp)
		}
	}
	if code, _, _ := geminiDo(t, h, "/v1beta/models/up/gpt-6-luna:embedContent", "tok-infer", `{}`); code != http.StatusNotFound {
		t.Errorf("unknown method: %d", code)
	}
}

// Models are listed in Google's shape, and countTokens estimates.
func TestGeminiModelsAndCountTokens(t *testing.T) {
	h := newHarness(t, (&chatStub{}).handler())
	req := httptest.NewRequest("GET", "/v1beta/models", nil)
	req.Header.Set("x-goog-api-key", "tok-infer")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"models/up/gpt-6-luna"`) || !strings.Contains(rec.Body.String(), "streamGenerateContent") {
		t.Fatalf("models: %d %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest("GET", "/v1beta/models/up/nope", nil)
	req.Header.Set("x-goog-api-key", "tok-infer")
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown model: %d", rec.Code)
	}
	code, hdr, body := geminiDo(t, h, "/v1beta/models/up/gpt-6-luna:countTokens", "tok-infer", `{"contents":[{"parts":[{"text":"How many tokens is this?"}]}]}`)
	var out struct {
		Total int `json:"totalTokens"`
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &out) != nil || out.Total <= 0 || hdr.Get(countTokensSourceHeader) != "estimate" {
		t.Fatalf("countTokens: %d %s", code, body)
	}
}
