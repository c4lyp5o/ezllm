package copilotbridge

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Request is one OpenAI chat call reduced to what a `copilot` turn can carry: a
// system text, a prompt, the caller's tools, one model, one stream flag.
//
// Neither CLI path can replay history as real turns, so prior turns are flattened
// into a deterministic transcript (the agy rule,
// docs/antigravity-subscription-design.md §3). `-p` also has no system-prompt flag,
// so its Prompt carries the system text too.
type Request struct {
	Model        string
	Stream       bool
	IncludeUsage bool
	// Prompt is system + transcript, for the `copilot -p` fallback.
	Prompt string
	// System and Transcript are the two halves separately, for the channel, which
	// has a real system message.
	System     string
	Transcript string
	// SystemLen and Turns are for the operator log only: they make an
	// unexpectedly huge flattened prompt explainable after the fact.
	SystemLen int
	Turns     int
	// Tools are the caller's functions (channel.go).
	Tools  []toolDef
	Choice toolChoice
}

// Unsupported is a request the bridge refuses before anything is spawned.
type Unsupported struct {
	Code string
	Msg  string
}

func (u Unsupported) Error() string { return "copilot-bridge: " + u.Msg }

type chatRequest struct {
	Model               string          `json:"model"`
	Messages            []chatMessage   `json:"messages"`
	Stream              bool            `json:"stream"`
	StreamOptions       *streamOptions  `json:"stream_options"`
	Tools               json.RawMessage `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	Functions           json.RawMessage `json:"functions"`
	MaxTokens           json.RawMessage `json:"max_tokens"`
	MaxCompletionTokens json.RawMessage `json:"max_completion_tokens"`
	ResponseFormat      *struct {
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
	// Only function tools exist on this wire (tools.go).
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
		defs = nil // the caller forbade calls this turn
	}
	if choice.Required || choice.Name != "" {
		return Request{}, Unsupported{Code: "chat_tools_unsupported", Msg: "forced tool choices are not supported on this Copilot hop"}
	}
	for _, limit := range []json.RawMessage{in.MaxTokens, in.MaxCompletionTokens} {
		if len(limit) > 0 && strings.TrimSpace(string(limit)) != "null" {
			return Request{}, Unsupported{Code: "chat_parameter_unsupported", Msg: "output token limits are not supported on this Copilot hop"}
		}
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
	req.System = sysText
	req.Transcript = strings.Join(turns, "\n\n")
	req.Tools, req.Choice = defs, choice
	var b strings.Builder
	if sysText != "" {
		b.WriteString("System instructions:\n")
		b.WriteString(sysText)
		b.WriteString("\n\nConversation:\n")
	}
	b.WriteString(req.Transcript)
	req.Prompt = b.String()
	req.SystemLen = len(sysText)
	req.Turns = len(turns)
	return req, nil
}
