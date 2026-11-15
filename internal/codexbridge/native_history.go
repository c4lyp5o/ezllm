package codexbridge

import (
	"encoding/json"
	"errors"
)

var errUnpairedNativeHistory = errors.New("conversation has incomplete tool-call relationships")

// nativeConversation preserves roles and function-call/result relationships on
// fresh app-server threads. A live parked continuation bypasses this conversion.
// Items belong only to the request's thread/fork, never its cache template.
func nativeConversation(turns []chatMessage) ([]any, []UserInput, error) {
	if len(turns) == 0 {
		return nil, nil, nil
	}
	history := turns
	var input []UserInput
	last := turns[len(turns)-1]
	if last.Role == "user" {
		history = turns[:len(turns)-1]
		var err error
		input, err = nativeUserInput(last.Content)
		if err != nil {
			return nil, nil, err
		}
	} else {
		input = []UserInput{{Type: "text", Text: "Continue the user's request using the tool results in the conversation above."}}
	}
	var items []any
	pending := map[string]bool{}
	for _, m := range history {
		if m.Role == "tool" {
			if !pending[m.ToolCallID] {
				return nil, nil, errUnpairedNativeHistory
			}
			delete(pending, m.ToolCallID)
			text, images, err := contentText(m.Content)
			if err != nil {
				return nil, nil, err
			}
			var output any = text
			if len(images) > 0 {
				parts, partErr := nativeUserInput(m.Content)
				if partErr != nil {
					return nil, nil, partErr
				}
				var content []any
				for _, part := range parts {
					if part.Type == "image" {
						content = append(content, map[string]any{"type": "input_image", "image_url": part.URL})
					} else {
						content = append(content, map[string]any{"type": "input_text", "text": part.Text})
					}
				}
				output = content
			}
			items = append(items, map[string]any{"type": "function_call_output", "call_id": m.ToolCallID, "output": output})
			continue
		}
		parts, err := nativeUserInput(m.Content)
		if err != nil {
			return nil, nil, err
		}
		var content []any
		for _, p := range parts {
			if p.Type == "image" {
				content = append(content, map[string]any{"type": "input_image", "image_url": p.URL})
				continue
			}
			kind := "input_text"
			if m.Role == "assistant" {
				kind = "output_text"
			}
			content = append(content, map[string]any{"type": kind, "text": p.Text})
		}
		if len(content) > 0 {
			item := map[string]any{"type": "message", "role": m.Role, "content": content}
			if m.Role == "assistant" {
				item["phase"] = assistantPhase(m)
			}
			items = append(items, item)
		}
		for _, call := range m.ToolCalls {
			if call.ID == "" || call.Function.Name == "" || pending[call.ID] {
				return nil, nil, errUnpairedNativeHistory
			}
			pending[call.ID] = true
			items = append(items, map[string]any{"type": "function_call", "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments})
		}
	}
	if len(pending) > 0 {
		return nil, nil, errUnpairedNativeHistory
	}
	return items, input, nil
}

// Preserve the order of text and images, rather than appending all images after
// a flattened transcript. Cache metadata is transport-only.
func nativeUserInput(raw json.RawMessage) ([]UserInput, error) {
	if !present(raw) {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if text == "" {
			return nil, nil
		}
		return []UserInput{{Type: "text", Text: text}}, nil
	}
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, err
	}
	var result []UserInput
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text":
			result = append(result, UserInput{Type: "text", Text: p.Text})
		case "image_url":
			if p.ImageURL == nil || p.ImageURL.URL == "" {
				return nil, Unsupported{Code: "invalid_request", Msg: "image_url part without a url"}
			}
			result = append(result, UserInput{Type: "image", URL: p.ImageURL.URL})
		default:
			return nil, Unsupported{Code: "invalid_request", Msg: "unsupported native history content type"}
		}
	}
	return result, nil
}

// Chat Completions lacks native message phases. Preserve an explicit phase;
// otherwise a tool-bearing assistant response is intermediate commentary, and
// a response without tool calls is its final answer for that client turn.
func assistantPhase(m chatMessage) string {
	if m.Phase != "" {
		return m.Phase
	}
	if len(m.ToolCalls) > 0 {
		return "commentary"
	}
	return "final_answer"
}
