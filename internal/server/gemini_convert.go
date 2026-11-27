package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// The Gemini surface speaks Google's generateContent shape to the client and
// runs every request through the chat inference path (routing, rules,
// compression, ledger, failover). This file converts between the two shapes.

// geminiRequest is the part of a generateContent body ezllm reads.
type geminiRequest struct {
	Contents          []geminiContent  `json:"contents"`
	SystemInstruction *geminiContent   `json:"systemInstruction"`
	Tools             []geminiTool     `json:"tools"`
	ToolConfig        *geminiToolCfg   `json:"toolConfig"`
	GenerationConfig  *geminiGenConfig `json:"generationConfig"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text             string          `json:"text,omitempty"`
	Thought          bool            `json:"thought,omitempty"`
	InlineData       *geminiBlob     `json:"inlineData,omitempty"`
	FileData         *geminiFile     `json:"fileData,omitempty"`
	FunctionCall     *geminiCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiResponse `json:"functionResponse,omitempty"`
}

type geminiBlob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiFile struct {
	MimeType string `json:"mimeType"`
	FileURI  string `json:"fileUri"`
}

type geminiCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type geminiResponse struct {
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFuncDecl `json:"functionDeclarations"`
	// Google's own tools (search, code execution, URL context) run inside
	// Google; no other provider can serve them.
	GoogleSearch          json.RawMessage `json:"googleSearch,omitempty"`
	GoogleSearchRetrieval json.RawMessage `json:"googleSearchRetrieval,omitempty"`
	CodeExecution         json.RawMessage `json:"codeExecution,omitempty"`
	URLContext            json.RawMessage `json:"urlContext,omitempty"`
}

type geminiFuncDecl struct {
	Name                 string          `json:"name"`
	Description          string          `json:"description,omitempty"`
	Parameters           json.RawMessage `json:"parameters,omitempty"`
	ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema,omitempty"`
}

type geminiToolCfg struct {
	FunctionCallingConfig *struct {
		Mode                 string   `json:"mode"`
		AllowedFunctionNames []string `json:"allowedFunctionNames"`
	} `json:"functionCallingConfig"`
}

type geminiGenConfig struct {
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"topP,omitempty"`
	MaxOutputTokens  *int64          `json:"maxOutputTokens,omitempty"`
	StopSequences    []string        `json:"stopSequences,omitempty"`
	CandidateCount   *int            `json:"candidateCount,omitempty"`
	Seed             *int64          `json:"seed,omitempty"`
	PresencePenalty  *float64        `json:"presencePenalty,omitempty"`
	FrequencyPenalty *float64        `json:"frequencyPenalty,omitempty"`
	ResponseMimeType string          `json:"responseMimeType,omitempty"`
	ResponseSchema   json.RawMessage `json:"responseSchema,omitempty"`
	ResponseJSON     json.RawMessage `json:"responseJsonSchema,omitempty"`
	ThinkingConfig   *struct {
		IncludeThoughts bool `json:"includeThoughts"`
	} `json:"thinkingConfig,omitempty"`
}

// errGeminiUnsupported marks a request ezllm cannot carry to another provider.
var errGeminiUnsupported = errors.New("unsupported")

// geminiToChat builds the chat-completions body for a generateContent
// request. Function calls get ids (Gemini's own, or call_<n>) so their
// responses can be matched; a response without an id takes the oldest open
// call of the same name.
func geminiToChat(model string, g geminiRequest, stream bool) (map[string]any, bool, error) {
	var msgs []any
	if g.SystemInstruction != nil {
		if text := geminiText(g.SystemInstruction.Parts); text != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": text})
		}
	}
	open := map[string][]string{} // function name → ids still waiting for a response
	n := 0
	for _, c := range g.Contents {
		switch c.Role {
		case "model":
			msg := map[string]any{"role": "assistant"}
			var text strings.Builder
			var calls []any
			for _, p := range c.Parts {
				switch {
				case p.FunctionCall != nil:
					id := p.FunctionCall.ID
					if id == "" {
						n++
						id = "call_" + strconv.Itoa(n)
					}
					open[p.FunctionCall.Name] = append(open[p.FunctionCall.Name], id)
					args := string(p.FunctionCall.Args)
					if args == "" || args == "null" {
						args = "{}"
					}
					calls = append(calls, map[string]any{"id": id, "type": "function",
						"function": map[string]any{"name": p.FunctionCall.Name, "arguments": args}})
				case p.Thought:
					// A model's earlier thoughts are not sent back.
				default:
					text.WriteString(p.Text)
				}
			}
			if text.Len() > 0 {
				msg["content"] = text.String()
			} else {
				msg["content"] = nil
			}
			if len(calls) > 0 {
				msg["tool_calls"] = calls
			}
			msgs = append(msgs, msg)
		case "", "user", "function", "tool":
			var parts []any
			for _, p := range c.Parts {
				switch {
				case p.FunctionResponse != nil:
					id := p.FunctionResponse.ID
					name := p.FunctionResponse.Name
					if id == "" {
						if q := open[name]; len(q) > 0 {
							id, open[name] = q[0], q[1:]
						} else {
							n++
							id = "call_" + strconv.Itoa(n)
						}
					}
					out := string(p.FunctionResponse.Response)
					if out == "" {
						out = "{}"
					}
					msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": id, "content": out})
				case p.InlineData != nil:
					if !strings.HasPrefix(p.InlineData.MimeType, "image/") {
						return nil, false, fmt.Errorf("%w: inline %s data (only images can be passed on)", errGeminiUnsupported, p.InlineData.MimeType)
					}
					parts = append(parts, map[string]any{"type": "image_url",
						"image_url": map[string]any{"url": "data:" + p.InlineData.MimeType + ";base64," + p.InlineData.Data}})
				case p.FileData != nil:
					if !strings.HasPrefix(p.FileData.MimeType, "image/") || !strings.HasPrefix(p.FileData.FileURI, "https://") {
						return nil, false, fmt.Errorf("%w: fileData (upload files to Google are not reachable from other providers; send images inline)", errGeminiUnsupported)
					}
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": p.FileData.FileURI}})
				case p.Text != "":
					parts = append(parts, map[string]any{"type": "text", "text": p.Text})
				}
			}
			switch {
			case len(parts) == 1 && parts[0].(map[string]any)["type"] == "text":
				msgs = append(msgs, map[string]any{"role": "user", "content": parts[0].(map[string]any)["text"]})
			case len(parts) > 0:
				msgs = append(msgs, map[string]any{"role": "user", "content": parts})
			}
		default:
			return nil, false, fmt.Errorf("content role %q: use user or model", c.Role)
		}
	}
	if len(msgs) == 0 {
		return nil, false, errors.New("contents is required")
	}
	body := map[string]any{"model": model, "messages": msgs}
	if stream {
		body["stream"] = true
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	var tools []any
	for _, t := range g.Tools {
		if len(t.GoogleSearch) > 0 || len(t.GoogleSearchRetrieval) > 0 || len(t.CodeExecution) > 0 || len(t.URLContext) > 0 {
			return nil, false, fmt.Errorf("%w: Google's built-in tools (search, code execution, URL context) run only on Google", errGeminiUnsupported)
		}
		for _, f := range t.FunctionDeclarations {
			params := f.ParametersJSONSchema
			if len(params) == 0 && len(f.Parameters) > 0 {
				params = lowerSchemaTypes(f.Parameters)
			}
			fn := map[string]any{"name": f.Name}
			if f.Description != "" {
				fn["description"] = f.Description
			}
			if len(params) > 0 {
				fn["parameters"] = params
			} else {
				fn["parameters"] = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if g.ToolConfig != nil && g.ToolConfig.FunctionCallingConfig != nil {
		fc := g.ToolConfig.FunctionCallingConfig
		switch strings.ToUpper(fc.Mode) {
		case "", "AUTO", "MODE_UNSPECIFIED":
		case "NONE":
			body["tool_choice"] = "none"
		case "ANY", "VALIDATED":
			if len(fc.AllowedFunctionNames) == 1 {
				body["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": fc.AllowedFunctionNames[0]}}
			} else {
				body["tool_choice"] = "required"
			}
		default:
			return nil, false, fmt.Errorf("functionCallingConfig.mode %q", fc.Mode)
		}
	}
	thoughts := false
	if gc := g.GenerationConfig; gc != nil {
		if gc.CandidateCount != nil && *gc.CandidateCount > 1 {
			return nil, false, fmt.Errorf("%w: candidateCount above 1", errGeminiUnsupported)
		}
		set := func(k string, v any, ok bool) {
			if ok {
				body[k] = v
			}
		}
		set("temperature", gc.Temperature, gc.Temperature != nil)
		set("top_p", gc.TopP, gc.TopP != nil)
		set("max_tokens", gc.MaxOutputTokens, gc.MaxOutputTokens != nil)
		set("stop", gc.StopSequences, len(gc.StopSequences) > 0)
		set("seed", gc.Seed, gc.Seed != nil)
		set("presence_penalty", gc.PresencePenalty, gc.PresencePenalty != nil)
		set("frequency_penalty", gc.FrequencyPenalty, gc.FrequencyPenalty != nil)
		schema := gc.ResponseJSON
		if len(schema) == 0 && len(gc.ResponseSchema) > 0 {
			schema = lowerSchemaTypes(gc.ResponseSchema)
		}
		switch {
		case len(schema) > 0:
			body["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "response", "schema": schema}}
		case gc.ResponseMimeType == "application/json":
			body["response_format"] = map[string]any{"type": "json_object"}
		}
		thoughts = gc.ThinkingConfig != nil && gc.ThinkingConfig.IncludeThoughts
	}
	return body, thoughts, nil
}

func geminiText(parts []geminiPart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// lowerSchemaTypes rewrites Gemini's OpenAPI-style schema types ("OBJECT",
// "STRING") into JSON Schema's lower case, everywhere in the schema.
func lowerSchemaTypes(raw json.RawMessage) json.RawMessage {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	var walk func(any) any
	walk = func(x any) any {
		switch t := x.(type) {
		case map[string]any:
			for k, val := range t {
				if k == "type" {
					if s, ok := val.(string); ok {
						t[k] = strings.ToLower(s)
						continue
					}
				}
				t[k] = walk(val)
			}
		case []any:
			for i := range t {
				t[i] = walk(t[i])
			}
		}
		return x
	}
	out, err := json.Marshal(walk(v))
	if err != nil {
		return raw
	}
	return out
}

// geminiFinish maps a chat finish_reason to Gemini's finishReason.
func geminiFinish(reason string) string {
	switch reason {
	case "length":
		return "MAX_TOKENS"
	case "content_filter":
		return "SAFETY"
	case "", "stop", "tool_calls", "function_call":
		return "STOP"
	default:
		return "OTHER"
	}
}

// geminiUsage maps chat usage to Gemini's usageMetadata.
func geminiUsage(u map[string]any) map[string]any {
	if u == nil {
		return nil
	}
	num := func(m map[string]any, k string) int64 {
		if f, ok := m[k].(float64); ok {
			return int64(f)
		}
		return 0
	}
	out := map[string]any{
		"promptTokenCount":     num(u, "prompt_tokens"),
		"candidatesTokenCount": num(u, "completion_tokens"),
		"totalTokenCount":      num(u, "total_tokens"),
	}
	if d, ok := u["prompt_tokens_details"].(map[string]any); ok && num(d, "cached_tokens") > 0 {
		out["cachedContentTokenCount"] = num(d, "cached_tokens")
	}
	if d, ok := u["completion_tokens_details"].(map[string]any); ok && num(d, "reasoning_tokens") > 0 {
		out["thoughtsTokenCount"] = num(d, "reasoning_tokens")
	}
	return out
}

// geminiArgs turns a tool call's argument string into the object Gemini
// sends; arguments that are not a JSON object travel under "arguments".
func geminiArgs(s string) json.RawMessage {
	s = strings.TrimSpace(s)
	if s == "" {
		return json.RawMessage(`{}`)
	}
	var obj map[string]any
	if json.Unmarshal([]byte(s), &obj) == nil {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(map[string]string{"arguments": s})
	return b
}

// chatToGemini converts a whole chat completion into a GenerateContentResponse.
func chatToGemini(raw []byte, thoughts bool) (map[string]any, error) {
	var resp struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content          any    `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	var parts []any
	finish := ""
	if len(resp.Choices) > 0 {
		c := resp.Choices[0]
		finish = c.FinishReason
		if thoughts && c.Message.ReasoningContent != "" {
			parts = append(parts, map[string]any{"text": c.Message.ReasoningContent, "thought": true})
		}
		if text := contentText(c.Message.Content); text != "" {
			parts = append(parts, map[string]any{"text": text})
		}
		for _, tc := range c.Message.ToolCalls {
			parts = append(parts, map[string]any{"functionCall": map[string]any{
				"id": tc.ID, "name": tc.Function.Name, "args": geminiArgs(tc.Function.Arguments)}})
		}
	}
	cand := map[string]any{"index": 0, "finishReason": geminiFinish(finish)}
	if len(parts) > 0 {
		cand["content"] = map[string]any{"role": "model", "parts": parts}
	}
	out := map[string]any{"candidates": []any{cand}, "modelVersion": resp.Model, "responseId": resp.ID}
	if u := geminiUsage(resp.Usage); u != nil {
		out["usageMetadata"] = u
	}
	return out, nil
}

// contentText reads a chat message content that is a string or a list of
// text parts.
func contentText(c any) string {
	switch t := c.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, p := range t {
			if m, ok := p.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return ""
}

// geminiError rewrites an OpenAI-shaped error body into Google's shape,
// keeping the HTTP status.
func geminiError(status int, raw []byte) map[string]any {
	msg := strings.TrimSpace(string(raw))
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
		msg = e.Error.Message
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	return map[string]any{"error": map[string]any{"code": status, "message": msg, "status": geminiStatus(status)}}
}

func geminiStatus(code int) string {
	switch code {
	case http.StatusBadRequest:
		return "INVALID_ARGUMENT"
	case http.StatusUnauthorized:
		return "UNAUTHENTICATED"
	case http.StatusForbidden:
		return "PERMISSION_DENIED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusRequestEntityTooLarge:
		return "INVALID_ARGUMENT"
	case http.StatusTooManyRequests:
		return "RESOURCE_EXHAUSTED"
	case http.StatusServiceUnavailable:
		return "UNAVAILABLE"
	case http.StatusGatewayTimeout:
		return "DEADLINE_EXCEEDED"
	}
	if code >= 500 {
		return "INTERNAL"
	}
	return "FAILED_PRECONDITION"
}

// geminiStream turns chat SSE chunks into Gemini chunks. Text is passed on as
// it arrives; a function call is held until its arguments are complete,
// because Gemini sends each call whole.
type geminiStream struct {
	thoughts bool
	id       string
	model    string
	finish   string
	usage    map[string]any
	calls    map[int]*pendingCall
}

type pendingCall struct {
	id, name string
	args     strings.Builder
}

// chunk reads one chat SSE data payload and returns the Gemini chunks to send now.
func (g *geminiStream) chunk(data []byte) []map[string]any {
	var c struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any  `json:"usage"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &c) != nil {
		return nil
	}
	if len(c.Error) > 0 {
		return []map[string]any{{"error": c.Error}}
	}
	if c.ID != "" {
		g.id = c.ID
	}
	if c.Model != "" {
		g.model = c.Model
	}
	if c.Usage != nil {
		g.usage = c.Usage
	}
	var out []map[string]any
	for _, ch := range c.Choices {
		var parts []any
		if g.thoughts && ch.Delta.ReasoningContent != "" {
			parts = append(parts, map[string]any{"text": ch.Delta.ReasoningContent, "thought": true})
		}
		if ch.Delta.Content != "" {
			parts = append(parts, map[string]any{"text": ch.Delta.Content})
		}
		for _, tc := range ch.Delta.ToolCalls {
			if g.calls == nil {
				g.calls = map[int]*pendingCall{}
			}
			p := g.calls[tc.Index]
			if p == nil {
				p = &pendingCall{}
				g.calls[tc.Index] = p
			}
			if tc.ID != "" {
				p.id = tc.ID
			}
			if tc.Function.Name != "" {
				p.name = tc.Function.Name
			}
			p.args.WriteString(tc.Function.Arguments)
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			g.finish = *ch.FinishReason
		}
		if len(parts) > 0 {
			out = append(out, g.wrap(map[string]any{"index": 0, "content": map[string]any{"role": "model", "parts": parts}}))
		}
	}
	return out
}

// final is the last chunk: held function calls, the finish reason, usage.
func (g *geminiStream) final() map[string]any {
	cand := map[string]any{"index": 0, "finishReason": geminiFinish(g.finish)}
	if len(g.calls) > 0 {
		idx := make([]int, 0, len(g.calls))
		for i := range g.calls {
			idx = append(idx, i)
		}
		sort.Ints(idx)
		parts := make([]any, 0, len(idx))
		for _, i := range idx {
			p := g.calls[i]
			parts = append(parts, map[string]any{"functionCall": map[string]any{"id": p.id, "name": p.name, "args": geminiArgs(p.args.String())}})
		}
		cand["content"] = map[string]any{"role": "model", "parts": parts}
	}
	out := g.wrap(cand)
	if u := geminiUsage(g.usage); u != nil {
		out["usageMetadata"] = u
	}
	return out
}

func (g *geminiStream) wrap(cand map[string]any) map[string]any {
	return map[string]any{"candidates": []any{cand}, "modelVersion": g.model, "responseId": g.id}
}
