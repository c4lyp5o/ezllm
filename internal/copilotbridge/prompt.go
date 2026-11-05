package copilotbridge

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Request is one OpenAI chat call reduced to what a headless `copilot` turn can
// carry: one prompt, one model, one stream flag.
//
// There is no system-prompt flag and no history replay on the CLI, so system
// text and prior turns are flattened into the prompt as a deterministic
// transcript (the agy rule, docs/antigravity-subscription-design.md §3).
type Request struct {
	Model        string
	Stream       bool
	IncludeUsage bool
	Prompt       string
	// SystemLen and Turns are for the operator log only: they make an
	// unexpectedly huge flattened prompt explainable after the fact.
	SystemLen int
	Turns     int
	// ToolNames are the caller's declared functions; the reply is parsed for
	// <tool_call> blocks naming one of them (tools.go). Empty = no tools.
	ToolNames map[string]bool
}

// Unsupported is a request the bridge refuses before anything is spawned.
type Unsupported struct {
	Code string
	Msg  string
}

func (u Unsupported) Error() string { return "copilot-bridge: " + u.Msg }

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	Stream         bool            `json:"stream"`
	StreamOptions  *streamOptions  `json:"stream_options"`
	Tools          json.RawMessage `json:"tools"`
	ToolChoice     json.RawMessage `json:"tool_choice"`
	Functions      json.RawMessage `json:"functions"`
	ResponseFormat *struct {
		Type string `json:"type"`
	} `json:"response_format"`
	N *int `json:"n"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []toolCallIn    `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name"`
}

// contentText accepts the two shapes a chat message's content takes: a plain
// string, or the array of typed parts a vision-capable client sends. Text parts
// are joined; an image part is refused by name rather than silently dropped,
// because dropping it would answer a question about a picture nobody saw.
func contentText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", fmt.Errorf("message content is neither a string nor a parts array")
	}
	var b strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text":
			b.WriteString(p.Text)
		default:
			return "", fmt.Errorf("content part %q is not supported by this bridge", p.Type)
		}
	}
	return b.String(), nil
}

// ParseRequest validates and flattens. Every refusal is typed so the ledger's
// [cause] marker says what actually happened.
func ParseRequest(body []byte) (Request, error) {
	var in chatRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return Request{}, Unsupported{Code: "invalid_request", Msg: "body is not a valid chat completion request"}
	}
	// Tools are EMULATED (tools.go): the CLI cannot hand a pending call back, so
	// the schemas ride in the prompt and the reply is parsed for <tool_call>
	// blocks. Only function tools exist on this wire.
	defs, err := parseTools(in.Tools)
	if err != nil {
		return Request{}, err
	}
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Function.Name] = true
	}
	choice, err := parseToolChoice(in.ToolChoice, names)
	if err != nil {
		return Request{}, err
	}
	if choice.None {
		defs, names = nil, map[string]bool{} // the caller forbade calls this turn
	}
	if len(in.Functions) > 0 && string(in.Functions) != "null" && string(in.Functions) != "[]" {
		return Request{}, Unsupported{Code: "chat_tools_unsupported",
			Msg: "legacy function calling is not supported on this Copilot hop"}
	}
	if in.N != nil && *in.N > 1 {
		return Request{}, Unsupported{Code: "invalid_request", Msg: "n > 1 is not supported on this Copilot hop"}
	}
	if in.ResponseFormat != nil && in.ResponseFormat.Type != "" && in.ResponseFormat.Type != "text" {
		return Request{}, Unsupported{Code: "protocol_unsupported",
			Msg: "response_format " + in.ResponseFormat.Type + " is not supported on this Copilot hop"}
	}

	req := Request{Model: strings.TrimSpace(in.Model), Stream: in.Stream}
	if in.StreamOptions != nil {
		req.IncludeUsage = in.StreamOptions.IncludeUsage
	}

	var sys []string
	var turns []string
	callName := map[string]string{} // tool_call id -> function name, for result turns
	for _, m := range in.Messages {
		text, err := contentText(m.Content)
		if err != nil {
			return Request{}, Unsupported{Code: "invalid_request", Msg: err.Error()}
		}
		switch strings.ToLower(strings.TrimSpace(m.Role)) {
		case "system", "developer":
			sys = append(sys, text)
		case "assistant":
			for _, tc := range m.ToolCalls {
				callName[tc.ID] = tc.Function.Name
				text += "\n" + renderCall(tc)
			}
			turns = append(turns, "Assistant: "+strings.TrimSpace(text))
		case "user":
			turns = append(turns, "User: "+text)
		case "tool":
			name := callName[m.ToolCallID]
			if name == "" {
				name = m.Name
			}
			turns = append(turns, fmt.Sprintf("Tool result for %s (%s):\n%s", name, m.ToolCallID, text))
		default:
			return Request{}, Unsupported{Code: "invalid_request", Msg: "unknown role " + m.Role}
		}
	}
	if len(turns) == 0 {
		return Request{}, Unsupported{Code: "invalid_request", Msg: "no user message"}
	}

	sysText := strings.Join(sys, "\n\n")
	if len(defs) > 0 {
		sysText = strings.TrimSpace(sysText + "\n\n" + toolPrompt(defs, choice))
		req.ToolNames = names
	}
	var b strings.Builder
	if sysText != "" {
		b.WriteString("System instructions:\n")
		b.WriteString(sysText)
		b.WriteString("\n\nConversation:\n")
	}
	b.WriteString(strings.Join(turns, "\n\n"))
	req.Prompt = b.String()
	req.SystemLen = len(strings.Join(sys, "\n\n"))
	req.Turns = len(turns)
	return req, nil
}
