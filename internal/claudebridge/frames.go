package claudebridge

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
)

// Request is the subset of an Anthropic Messages request this hop honours. Anything
// else arrives as Unsupported and is refused before a process is spawned (M1 is
// text-only; see docs/claude-subscription-design.md).
type Request struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	System    json.RawMessage `json:"system,omitempty"`
	Messages  []Message       `json:"messages"`
	Stream    bool            `json:"stream,omitempty"`
	Stop      []string        `json:"stop_sequences,omitempty"`
}

// Message is one conversation turn. Content stays raw: string or block array both
// pass through to native untouched.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// Unsupported names a request field the bridge cannot honour yet. It is a typed
// refusal, not a silent drop: a client that asked for tools must not get a
// tool-less answer and a 200.
type Unsupported struct{ Field string }

func (u Unsupported) Error() string {
	return "claude-bridge: " + u.Field + " is not supported on this hop (M1 is text-only)"
}

// ParseRequest decodes and validates a /v1/messages body.
func ParseRequest(body []byte) (Request, error) {
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		return Request{}, fmt.Errorf("claude-bridge: invalid request body: %w", err)
	}
	if req.Model == "" {
		return Request{}, fmt.Errorf("claude-bridge: model is required")
	}
	if len(req.Messages) == 0 {
		return Request{}, fmt.Errorf("claude-bridge: messages must not be empty")
	}
	// Raw-map pass to see fields the typed struct ignores.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err == nil {
		for _, field := range []string{"tools", "tool_choice", "parallel_tool_calls"} {
			if _, ok := raw[field]; ok {
				return Request{}, Unsupported{Field: field}
			}
		}
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "user" {
		return Request{}, fmt.Errorf("claude-bridge: history must end in a user message (assistant prefill is unsupported)")
	}
	var content any
	if err := json.Unmarshal(last.Content, &content); err != nil {
		return Request{}, fmt.Errorf("claude-bridge: last message content is not valid JSON: %w", err)
	}
	switch c := content.(type) {
	case nil:
		return Request{}, fmt.Errorf("claude-bridge: last user message must not be empty")
	case string:
		if strings.TrimSpace(c) == "" {
			return Request{}, fmt.Errorf("claude-bridge: last user message must not be empty")
		}
	case []any:
		if len(c) == 0 {
			return Request{}, fmt.Errorf("claude-bridge: last user message must not be empty")
		}
	}
	return req, nil
}

// SystemText renders the system field (string or text blocks) for --system-prompt-file.
// Native owns the system slot; it is deliberately kept out of the extra body.
func SystemText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("claude-bridge: system must be a string or text blocks")
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// BuildExtraBody renders the upstream body handed to native through
// CLAUDE_CODE_EXTRA_BODY. Native merges it into the one request it issues, so this
// body is what the subscription actually sees.
//
// Deliberate omissions, measured on CLI 2.1.299 and mirrored from the official
// plugin's request_body(): no `system` (native owns it via --system-prompt-file), no
// message ids (the strict schema rejects them: "messages.1.id: Extra inputs are not
// permitted"), no sampling controls (subscription routes reject temperature/top_p).
func BuildExtraBody(req Request) (string, error) {
	msgs := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		var content any
		if err := json.Unmarshal(m.Content, &content); err != nil {
			return "", fmt.Errorf("claude-bridge: %s message content is not valid JSON: %w", m.Role, err)
		}
		msgs = append(msgs, map[string]any{"role": m.Role, "content": content})
	}
	body := map[string]any{
		"model":    req.Model,
		"messages": msgs,
		"stream":   req.Stream,
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if len(req.Stop) > 0 {
		body["stop_sequences"] = req.Stop
	}
	out, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("claude-bridge: cannot encode upstream body: %w", err)
	}
	return string(out), nil
}

// BuildFrames renders the conversation as native stream-json input frames. Frames
// only drive native's turn machinery — the upstream request itself comes from
// BuildExtraBody — but every non-final user frame carries shouldQuery:false so
// native replays it with a zero-turn acknowledgement instead of generating (and
// charging the plan) once per frame. Measured on 2.1.299: without that marker each
// user frame produced its own upstream request.
func BuildFrames(req Request) ([]map[string]any, error) {
	frames := make([]map[string]any, 0, len(req.Messages))
	for i, m := range req.Messages {
		switch m.Role {
		case "user", "assistant":
		default:
			return nil, fmt.Errorf("claude-bridge: unsupported message role %q", m.Role)
		}
		var content any
		if err := json.Unmarshal(m.Content, &content); err != nil {
			return nil, fmt.Errorf("claude-bridge: %s message content is not valid JSON: %w", m.Role, err)
		}
		frame := map[string]any{
			"type":    m.Role,
			"message": map[string]any{"role": m.Role, "content": content},
		}
		if m.Role == "user" && i < len(req.Messages)-1 {
			frame["shouldQuery"] = false
		}
		frames = append(frames, frame)
	}
	return frames, nil
}

// ConversationSessionID is stable for the life of one conversation: derived from
// what the conversation starts with (system plus the first message), which every
// request replays unchanged. A fresh id per call would move native's per-session
// scratchpad marker and break the cached prefix on every turn — the same reason the
// official plugin derives its id this way.
func ConversationSessionID(system string, req Request) string {
	first := any(nil)
	if len(req.Messages) > 0 {
		var content any
		_ = json.Unmarshal(req.Messages[0].Content, &content)
		first = map[string]any{"role": req.Messages[0].Role, "content": content}
	}
	payload, _ := json.Marshal([]any{system, first})
	sum := sha256.Sum256(payload)
	// UUIDv4 shape (version/variant bits set) — native wants a uuid string.
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
