package codexbridge

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Request is one OpenAI Chat Completions call, reduced to what a Codex turn
// takes. Prior turns travel as a flattened transcript (operator decision,
// docs/codex-subscription-design.md §4.2): the structured `history` field on
// thread/resume is documented for Codex Cloud only and is not used.
type Request struct {
	Model        string
	Stream       bool
	IncludeUsage bool
	Effort       string
	Instructions string
	Input        []UserInput

	// Tools are the client's function tools (stage 2): declared to Codex as
	// dynamicTools, called by the model from code-mode, answered by the client.
	Tools []ToolSpec
	// ToolResults are the trailing tool messages, by tool_call_id: present
	// when this request continues a turn that stopped on tool calls.
	ToolResults map[string]string
}

// ToolSpec is one client function tool.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolCall is one call the model made, returned to the client as tool_calls.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// UserInput is one app-server turn/start input item.
type UserInput struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	URL  string `json:"url,omitempty"`
}

// Unsupported is a request the bridge refuses before anything is spawned.
type Unsupported struct {
	Code string
	Msg  string
}

func (u Unsupported) Error() string { return "codex-bridge: " + u.Msg }

// DefaultInstructions replace Codex's agentic coding prompt when the client
// sends no system message: measured, Codex's own instructions cost ~5k tokens
// and steer toward tools that are switched off here (§2.4).
const DefaultInstructions = "You are a helpful assistant."

type chatRequest struct {
	Model           string          `json:"model"`
	Messages        []chatMessage   `json:"messages"`
	Stream          bool            `json:"stream"`
	StreamOptions   *streamOptions  `json:"stream_options"`
	ReasoningEffort string          `json:"reasoning_effort"`
	N               *int            `json:"n"`
	Tools           json.RawMessage `json:"tools"`
	ToolChoice      json.RawMessage `json:"tool_choice"`
	Functions       json.RawMessage `json:"functions"`
	ParallelTools   *bool           `json:"parallel_tool_calls"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name"`
	ToolCalls  []chatToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type contentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

// present reports a JSON field that was sent with a value (not absent, not null).
func present(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null"
}

// ParseRequest validates a Chat Completions body and builds the turn.
func ParseRequest(body []byte) (Request, error) {
	var cr chatRequest
	if err := json.Unmarshal(body, &cr); err != nil {
		return Request{}, Unsupported{Code: "invalid_request", Msg: "request body is not valid JSON: " + err.Error()}
	}
	if strings.TrimSpace(cr.Model) == "" {
		return Request{}, Unsupported{Code: "invalid_request", Msg: "model is required"}
	}
	if present(cr.Functions) {
		return Request{}, Unsupported{Code: "invalid_request", Msg: "the legacy functions field is not supported; send tools"}
	}
	if cr.N != nil && *cr.N != 1 {
		return Request{}, Unsupported{Code: "invalid_request", Msg: "n must be 1: the subscription generates one choice per turn"}
	}
	if len(cr.Messages) == 0 {
		return Request{}, Unsupported{Code: "invalid_request", Msg: "messages must not be empty"}
	}

	req := Request{Model: cr.Model, Stream: cr.Stream, Effort: normalizeEffort(cr.ReasoningEffort)}
	if cr.StreamOptions != nil {
		req.IncludeUsage = cr.StreamOptions.IncludeUsage
	}

	var system []string
	var turns []chatMessage
	for _, m := range cr.Messages {
		switch m.Role {
		case "system", "developer":
			text, _, err := contentText(m.Content)
			if err != nil {
				return Request{}, err
			}
			if text != "" {
				system = append(system, text)
			}
		case "user", "assistant", "tool":
			turns = append(turns, m)
		default:
			return Request{}, Unsupported{Code: "invalid_request", Msg: fmt.Sprintf("unknown message role %q", m.Role)}
		}
	}
	if len(turns) == 0 || (turns[len(turns)-1].Role != "user" && turns[len(turns)-1].Role != "tool") {
		return Request{}, Unsupported{Code: "invalid_request", Msg: "the last message must be from the user or a tool result"}
	}
	req.Instructions = strings.Join(system, "\n\n")
	if req.Instructions == "" {
		req.Instructions = DefaultInstructions
	}
	if err := parseTools(&req, cr); err != nil {
		return Request{}, err
	}
	if turns[len(turns)-1].Role == "tool" {
		req.ToolResults = map[string]string{}
		for i := len(turns) - 1; i >= 0 && turns[i].Role == "tool"; i-- {
			text, _, err := contentText(turns[i].Content)
			if err != nil {
				return Request{}, err
			}
			if turns[i].ToolCallID == "" {
				return Request{}, Unsupported{Code: "invalid_request", Msg: "a tool message needs tool_call_id"}
			}
			req.ToolResults[turns[i].ToolCallID] = text
		}
	}

	text, images, err := buildTranscript(turns)
	if err != nil {
		return Request{}, err
	}
	req.Input = append(req.Input, UserInput{Type: "text", Text: text})
	for _, url := range images {
		req.Input = append(req.Input, UserInput{Type: "image", URL: url})
	}
	return req, nil
}

// parseTools maps OpenAI function tools and tool_choice. Codex has no forced
// tool choice, so "required" and a named function become an instruction; "none"
// declares no tools at all.
func parseTools(req *Request, cr chatRequest) error {
	if !present(cr.Tools) {
		return nil
	}
	var tools []chatTool
	if err := json.Unmarshal(cr.Tools, &tools); err != nil {
		return Unsupported{Code: "invalid_request", Msg: "tools must be an array of function tools"}
	}
	choice := "auto"
	if present(cr.ToolChoice) {
		var s string
		var named struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		switch {
		case json.Unmarshal(cr.ToolChoice, &s) == nil:
			choice = s
		case json.Unmarshal(cr.ToolChoice, &named) == nil && named.Function.Name != "":
			choice = "function:" + named.Function.Name
		default:
			return Unsupported{Code: "invalid_request", Msg: "tool_choice must be auto, none, required or {type:function,function:{name}}"}
		}
	}
	if choice == "none" {
		return nil
	}
	for _, t := range tools {
		if t.Type != "function" || t.Function.Name == "" {
			return Unsupported{Code: "invalid_request", Msg: fmt.Sprintf("tool type %q is not supported: only function tools pass through", t.Type)}
		}
		params := t.Function.Parameters
		if !present(params) {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		req.Tools = append(req.Tools, ToolSpec{Name: t.Function.Name, Description: t.Function.Description, Parameters: params})
	}
	switch {
	case choice == "required":
		req.Instructions += "\n\nYou must call at least one of the provided tools before you answer."
	case strings.HasPrefix(choice, "function:"):
		req.Instructions += "\n\nYou must call the tool " + strings.TrimPrefix(choice, "function:") + " before you answer."
	case choice != "auto":
		return Unsupported{Code: "invalid_request", Msg: fmt.Sprintf("tool_choice %q is not supported", choice)}
	}
	if cr.ParallelTools != nil && !*cr.ParallelTools {
		req.Instructions += "\n\nCall at most one tool at a time."
	}
	return nil
}

// normalizeEffort maps OpenAI's effort words onto Codex's. Codex has no
// "minimal" or "none"; its floor is "low". Everything else passes through and
// the model's own list decides (the CLI rejects an effort it does not offer).
func normalizeEffort(e string) string {
	switch e = strings.ToLower(strings.TrimSpace(e)); e {
	case "minimal", "none":
		return "low"
	}
	return e
}

// buildTranscript renders the conversation as the turn's text. A single user
// message passes through unchanged; otherwise earlier turns are framed as a
// transcript ahead of the latest message. The framing is deterministic so the
// same history yields the same prefix (prompt caching is prefix-based).
func buildTranscript(turns []chatMessage) (string, []string, error) {
	var images []string
	last := turns[len(turns)-1]
	latest, imgs, err := contentText(last.Content)
	if err != nil {
		return "", nil, err
	}
	if len(turns) == 1 {
		return latest, imgs, nil
	}
	history := turns[:len(turns)-1]
	closing := "\nReply to the latest user message:\n\n" + latest
	if last.Role == "tool" {
		// A stateless continuation (§4.6 fallback): the calls and their results
		// are all in the transcript; the model picks up from there.
		history, imgs = turns, nil
		closing = "\nThe results of your tool calls are above. Continue from there."
	}
	var b strings.Builder
	b.WriteString("Earlier turns of this conversation, oldest first:\n\n")
	for _, m := range history {
		text, mi, err := contentText(m.Content)
		if err != nil {
			return "", nil, err
		}
		for _, url := range mi {
			images = append(images, url)
			text += fmt.Sprintf("\n[image %d attached below]", len(images))
		}
		role := m.Role
		if m.Role == "tool" {
			role = "tool result"
			if m.ToolCallID != "" {
				role += " " + m.ToolCallID
			}
		}
		for _, tc := range m.ToolCalls {
			text += fmt.Sprintf("\n[called tool %s (%s) with %s]", tc.Function.Name, tc.ID, tc.Function.Arguments)
		}
		fmt.Fprintf(&b, "<turn role=%q>\n%s\n</turn>\n", role, strings.TrimSpace(text))
	}
	b.WriteString(closing)
	return b.String(), append(images, imgs...), nil
}

// contentText flattens a message's content (a string or an array of parts) to
// text, returning image URLs separately.
func contentText(raw json.RawMessage) (string, []string, error) {
	if !present(raw) {
		return "", nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil, nil
	}
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, Unsupported{Code: "invalid_request", Msg: "message content must be a string or an array of content parts"}
	}
	var texts, images []string
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text":
			texts = append(texts, p.Text)
		case "image_url":
			if p.ImageURL == nil || p.ImageURL.URL == "" {
				return "", nil, Unsupported{Code: "invalid_request", Msg: "image_url part without a url"}
			}
			images = append(images, p.ImageURL.URL)
		default:
			return "", nil, Unsupported{Code: "invalid_request", Msg: fmt.Sprintf("content part type %q is not supported", p.Type)}
		}
	}
	return strings.Join(texts, "\n"), images, nil
}
