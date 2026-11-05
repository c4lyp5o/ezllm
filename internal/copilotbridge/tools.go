package copilotbridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Client tools, EMULATED. The Copilot CLI runs its own agent loop and has no
// hook that hands a pending tool call back to the caller (design §10), so this
// hop cannot do native function calling. It does the next best thing: the
// caller's tool schemas are rendered into the prompt, the model is told to
// answer with <tool_call> blocks, and those blocks are parsed back into OpenAI
// tool_calls. Tool results the caller sends on its next request are replayed
// into the transcript. The model decides whether to comply; a reply that does
// not parse is returned as plain text, never invented into a call.

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

// toolChoice reduces OpenAI's tool_choice to what the prompt can say.
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

// toolPrompt is the instruction block appended to the system text.
func toolPrompt(defs []toolDef, tc toolChoice) string {
	var b strings.Builder
	b.WriteString("You can call functions. The available functions are listed below as JSON.\n")
	b.WriteString("To call a function, reply with a <tool_call> block and NOTHING else in that block:\n")
	b.WriteString(`<tool_call>{"name": "<function name>", "arguments": {<arguments object>}}</tool_call>` + "\n")
	b.WriteString("You may emit several <tool_call> blocks in one reply to call several functions. ")
	b.WriteString("When no function is needed, answer normally in plain text. ")
	b.WriteString("These functions are your ONLY tools: do not try to read files, run commands or browse. ")
	b.WriteString("After you call a function, the next turn contains its result as \"Tool result\"; use it to continue.\n")
	switch {
	case tc.Name != "":
		b.WriteString("You MUST call the function " + tc.Name + " in this reply.\n")
	case tc.Required:
		b.WriteString("You MUST call at least one function in this reply.\n")
	}
	b.WriteString("\nFunctions:\n")
	for _, d := range defs {
		entry := map[string]any{"name": d.Function.Name}
		if d.Function.Description != "" {
			entry["description"] = d.Function.Description
		}
		if len(d.Function.Parameters) > 0 && string(d.Function.Parameters) != "null" {
			entry["parameters"] = d.Function.Parameters
		}
		line, _ := json.Marshal(entry)
		b.Write(line)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderCall writes a past assistant tool call back in the same format the
// model is told to emit, so the transcript reads consistently.
func renderCall(tc toolCallIn) string {
	args := json.RawMessage(tc.Function.Arguments)
	if !json.Valid(args) {
		args, _ = json.Marshal(tc.Function.Arguments)
	}
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage("{}")
	}
	b, _ := json.Marshal(map[string]any{"name": tc.Function.Name, "arguments": args})
	return "<tool_call>" + string(b) + "</tool_call>"
}

var toolCallBlock = regexp.MustCompile(`(?s)<tool_call>(.*?)</tool_call>`)

// ExtractToolCalls splits a model reply into plain text and the tool calls it
// made. A block counts only if it parses and names a declared function; any
// other block is left in the text untouched.
func ExtractToolCalls(text string, allowed map[string]bool, newID func() string) (string, []ToolCall) {
	if len(allowed) == 0 || !strings.Contains(text, "<tool_call>") {
		return text, nil
	}
	var calls []ToolCall
	clean := toolCallBlock.ReplaceAllStringFunc(text, func(block string) string {
		inner := strings.TrimSpace(toolCallBlock.FindStringSubmatch(block)[1])
		inner = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(inner, "```json"), "```"), "```"))
		var c struct {
			Name       string          `json:"name"`
			Arguments  json.RawMessage `json:"arguments"`
			Parameters json.RawMessage `json:"parameters"`
		}
		if json.Unmarshal([]byte(inner), &c) != nil || !allowed[c.Name] {
			return block
		}
		args := c.Arguments
		if len(args) == 0 {
			args = c.Parameters
		}
		calls = append(calls, ToolCall{ID: newID(), Name: c.Name, Arguments: normaliseArgs(args)})
		return ""
	})
	return strings.TrimSpace(clean), calls
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
		return "{}"
	}
	return buf.String()
}
