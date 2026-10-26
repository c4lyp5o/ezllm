package agybridge

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Request is one OpenAI Chat Completions call, reduced to what one headless
// `agy -p` turn takes: a single prompt. agy has no system-prompt flag and no
// history replay, so system text and prior turns travel inside the prompt as a
// deterministic transcript (docs/antigravity-subscription-design.md §3).
type Request struct {
	Model        string
	Stream       bool
	IncludeUsage bool
	Effort       string
	Prompt       string
}

// Unsupported is a request the bridge refuses before anything is spawned.
type Unsupported struct {
	Code string
	Msg  string
}

func (u Unsupported) Error() string { return "agy-bridge: " + u.Msg }

type chatRequest struct {
	Model           string          `json:"model"`
	Messages        []chatMessage   `json:"messages"`
	Stream          bool            `json:"stream"`
	StreamOptions   *streamOptions  `json:"stream_options"`
	ReasoningEffort string          `json:"reasoning_effort"`
	N               *int            `json:"n"`
	Tools           json.RawMessage `json:"tools"`
	Functions       json.RawMessage `json:"functions"`
	ResponseFormat  *struct {
		Type string `json:"type"`
	} `json:"response_format"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func present(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null" && s != "[]"
}

// ParseRequest validates a Chat Completions body and builds the prompt. Client
// tools and images are refused, not dropped: the headless stream-json input
// takes text blocks only, and agy's own tools are the agent's, not the
// client's.
func ParseRequest(body []byte) (Request, error) {
	var cr chatRequest
	if err := json.Unmarshal(body, &cr); err != nil {
		return Request{}, Unsupported{Code: "invalid_request", Msg: "request body is not valid JSON: " + err.Error()}
	}
	if strings.TrimSpace(cr.Model) == "" {
		return Request{}, Unsupported{Code: "invalid_request", Msg: "model is required"}
	}
	if present(cr.Tools) || present(cr.Functions) {
		return Request{}, Unsupported{Code: "chat_tools_unsupported", Msg: "client tools are not supported by the Antigravity bridge"}
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
		text, err := contentText(m.Content)
		if err != nil {
			return Request{}, err
		}
		switch m.Role {
		case "system", "developer":
			if text != "" {
				system = append(system, text)
			}
		case "user", "assistant":
			turns = append(turns, chatMessage{Role: m.Role, Content: mustRaw(text)})
		case "tool":
			return Request{}, Unsupported{Code: "chat_tools_unsupported", Msg: "tool messages are not supported by the Antigravity bridge"}
		default:
			return Request{}, Unsupported{Code: "invalid_request", Msg: fmt.Sprintf("unknown message role %q", m.Role)}
		}
	}
	if len(turns) == 0 || turns[len(turns)-1].Role != "user" {
		return Request{}, Unsupported{Code: "invalid_request", Msg: "the last message must be from the user"}
	}
	if rf := cr.ResponseFormat; rf != nil {
		switch rf.Type {
		case "", "text":
		case "json_object":
			system = append(system, "Respond with a single valid JSON object and nothing else.")
		default:
			return Request{}, Unsupported{Code: "invalid_request", Msg: fmt.Sprintf("response_format type %q is not supported", rf.Type)}
		}
	}
	req.Prompt = buildPrompt(system, turns)
	return req, nil
}

// buildPrompt renders instructions and turns as one prompt. A lone user
// message with no instructions passes through unchanged; otherwise the framing
// is deterministic so the same history yields the same prefix (prompt caching
// is prefix-based).
func buildPrompt(system []string, turns []chatMessage) string {
	latest := rawText(turns[len(turns)-1].Content)
	if len(system) == 0 && len(turns) == 1 {
		return latest
	}
	var b strings.Builder
	if len(system) > 0 {
		b.WriteString("<instructions>\n")
		b.WriteString(strings.TrimSpace(strings.Join(system, "\n\n")))
		b.WriteString("\n</instructions>\n\n")
	}
	if history := turns[:len(turns)-1]; len(history) > 0 {
		b.WriteString("Earlier turns of this conversation, oldest first:\n\n")
		for _, m := range history {
			fmt.Fprintf(&b, "<turn role=%q>\n%s\n</turn>\n", m.Role, strings.TrimSpace(rawText(m.Content)))
		}
		b.WriteString("\nReply to the latest user message:\n\n")
	}
	b.WriteString(latest)
	return b.String()
}

// normalizeEffort maps OpenAI's effort words onto agy's --effort values
// (low|medium|high|xhigh|max); the model decides what it accepts.
func normalizeEffort(e string) string {
	switch e = strings.ToLower(strings.TrimSpace(e)); e {
	case "minimal", "none":
		return "low"
	}
	return e
}

func contentText(raw json.RawMessage) (string, error) {
	if !present(raw) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", Unsupported{Code: "invalid_request", Msg: "message content must be a string or an array of content parts"}
	}
	var texts []string
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text":
			texts = append(texts, p.Text)
		default:
			return "", Unsupported{Code: "invalid_request", Msg: fmt.Sprintf("content part type %q is not supported: the Antigravity bridge takes text only", p.Type)}
		}
	}
	return strings.Join(texts, "\n"), nil
}

func mustRaw(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

func rawText(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}
