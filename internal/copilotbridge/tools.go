package copilotbridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Client tools. The model calls them natively over the tool channel (channel.go),
// which reports the batch instead of running it. Tool results come back on the
// caller's next request and are replayed into the transcript as text.

const maxTools = 128

type toolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type toolCallIn struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ToolCall is one call the model made, in the shape OpenAI returns.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string // a JSON string, as OpenAI requires
}

// toolChoice reduces OpenAI's tool_choice to what the channel can honour.
type toolChoice struct {
	None     bool   // "none": the model must not call tools
	Required bool   // "required": at least one call
	Name     string // a specific function
}

func parseToolChoice(raw json.RawMessage, names map[string]bool) (toolChoice, error) {
	var tc toolChoice
	if len(raw) == 0 || string(raw) == "null" {
		return tc, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "none":
			tc.None = true
		case "required":
			tc.Required = true
		case "auto", "":
		default:
			return tc, Unsupported{Code: "invalid_request", Msg: "unknown tool_choice " + s}
		}
		return tc, nil
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Type != "function" || obj.Function.Name == "" {
		return tc, Unsupported{Code: "invalid_request", Msg: "tool_choice must be none, auto, required or a function"}
	}
	if !names[obj.Function.Name] {
		return tc, Unsupported{Code: "invalid_request", Msg: "tool_choice names a function that is not in tools: " + obj.Function.Name}
	}
	tc.Name = obj.Function.Name
	return tc, nil
}

// parseTools validates the caller's tools. Only function tools exist on the
// OpenAI chat wire; anything else is refused by name.
func parseTools(raw json.RawMessage) ([]toolDef, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" || s == "[]" {
		return nil, nil
	}
	var defs []toolDef
	if err := json.Unmarshal(raw, &defs); err != nil {
		return nil, Unsupported{Code: "invalid_request", Msg: "tools is not an array of function tools"}
	}
	if len(defs) > maxTools {
		return nil, Unsupported{Code: "invalid_request", Msg: fmt.Sprintf("more than %d tools", maxTools)}
	}
	seen := map[string]bool{}
	for _, d := range defs {
		if d.Type != "function" {
			return nil, Unsupported{Code: "chat_tools_unsupported", Msg: "tool type " + d.Type + " is not supported on this Copilot hop (function tools only)"}
		}
		if d.Function.Name == "" || seen[d.Function.Name] {
			return nil, Unsupported{Code: "invalid_request", Msg: "every tool needs a unique function name"}
		}
		seen[d.Function.Name] = true
	}
	return defs, nil
}

// renderCall writes a past assistant tool call into the transcript.
func renderCall(tc toolCallIn) string {
	return fmt.Sprintf("Called %s (%s) with %s", tc.Function.Name, tc.ID, normaliseArgs(json.RawMessage(tc.Function.Arguments)))
}

// normaliseArgs returns the arguments as the JSON STRING OpenAI carries. A model
// that already stringified its arguments is unwrapped once.
func normaliseArgs(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && json.Valid([]byte(s)) {
		return s
	}
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		// Not JSON (a truncated history entry): keep it visible as a string.
		quoted, _ := json.Marshal(string(raw))
		return string(quoted)
	}
	return buf.String()
}
