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
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
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
	// Tools: the CLI runs its OWN 27-tool agent loop and cannot be handed a
	// caller's function schemas (design §10). Refuse by name — a silent drop
	// would let an agent loop spin forever waiting for tool_calls.
	if len(in.Tools) > 0 && string(in.Tools) != "null" && string(in.Tools) != "[]" {
		return Request{}, Unsupported{Code: "chat_tools_unsupported",
			Msg: "this Copilot hop runs the CLI's own agent loop and cannot accept client tool schemas"}
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
	for _, m := range in.Messages {
		text, err := contentText(m.Content)
		if err != nil {
			return Request{}, Unsupported{Code: "invalid_request", Msg: err.Error()}
		}
		switch strings.ToLower(strings.TrimSpace(m.Role)) {
		case "system", "developer":
			sys = append(sys, text)
		case "assistant":
			turns = append(turns, "Assistant: "+text)
		case "user":
			turns = append(turns, "User: "+text)
		case "tool":
			return Request{}, Unsupported{Code: "chat_tools_unsupported",
				Msg: "tool results cannot be replayed into a Copilot hop"}
		default:
			return Request{}, Unsupported{Code: "invalid_request", Msg: "unknown role " + m.Role}
		}
	}
	if len(turns) == 0 {
		return Request{}, Unsupported{Code: "invalid_request", Msg: "no user message"}
	}

	var b strings.Builder
	if len(sys) > 0 {
		b.WriteString("System instructions:\n")
		b.WriteString(strings.Join(sys, "\n\n"))
		b.WriteString("\n\nConversation:\n")
	}
	b.WriteString(strings.Join(turns, "\n\n"))
	req.Prompt = b.String()
	req.SystemLen = len(strings.Join(sys, "\n\n"))
	req.Turns = len(turns)
	return req, nil
}
