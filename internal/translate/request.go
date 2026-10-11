package translate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Request translates an Anthropic /v1/messages request body into an
// OpenAI chat-completions request body.
//
// Determinism is a contract, not a nicety: ordering is fixed by the
// struct definitions (never a map re-marshal), tool arguments are
// produced by json.Marshal of a parsed value (Go sorts map keys), and
// the input slice is never mutated. That is what keeps an upstream's
// automatic prefix cache working across a translated hop.
//
// Dropped by design (locked decisions): metadata, thinking,
// cache_control, anthropic_version — we ride the upstream's automatic
// prefix cache instead of synthesizing Anthropic cache markers.
func Request(body []byte, upstreamModel string) ([]byte, error) {
	var req AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("unmarshal anthropic request: %w", err)
	}

	out := OpenAIRequest{
		Model:       upstreamModel,
		MaxTokens:   req.MaxTokens,
		Stop:        req.StopSequences,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      req.Stream,
		ToolChoice:  translateToolChoice(req.ToolChoice),
	}
	if req.Stream {
		// Required for an honest ledger on a streamed call.
		out.StreamOptions = &OpenAIStreamOptions{IncludeUsage: true}
	}

	if req.System != nil {
		if sysText := flattenText(req.System); sysText != "" {
			out.Messages = append(out.Messages, OpenAIMessage{Role: "system", Content: sysText})
		}
	}

	for _, m := range req.Messages {
		switch m.Role {
		case "user":
			out.Messages = append(out.Messages, translateUserMessage(m.Content)...)
		case "assistant":
			out.Messages = append(out.Messages, translateAssistantMessage(m.Content))
		}
	}

	for _, t := range req.Tools {
		out.Tools = append(out.Tools, OpenAITool{
			Type: "function",
			Function: OpenAIFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}

	return json.Marshal(out)
}

// translateUserMessage handles the three shapes an Anthropic user turn
// can take: a plain string, text blocks, or tool_result blocks (the
// dominant Claude Code payload). A turn that mixes tool_result blocks
// with text becomes tool messages followed by a user message — OpenAI
// requires tool messages to directly follow the assistant turn that
// requested them, so the remaining text cannot be folded in.
func translateUserMessage(content any) []OpenAIMessage {
	if s, ok := content.(string); ok {
		return []OpenAIMessage{{Role: "user", Content: s}}
	}
	blocks, ok := content.([]any)
	if !ok {
		return nil
	}

	var msgs []OpenAIMessage
	var rest []any
	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if block["type"] == "tool_result" {
			msgs = append(msgs, OpenAIMessage{
				Role:       "tool",
				ToolCallID: blockString(block, "tool_use_id"),
				Content:    flattenText(block["content"]),
			})
			continue
		}
		rest = append(rest, b)
	}
	if text := flattenText(rest); text != "" {
		msgs = append(msgs, OpenAIMessage{Role: "user", Content: text})
	}
	return msgs
}

func translateAssistantMessage(content any) OpenAIMessage {
	msg := OpenAIMessage{Role: "assistant"}
	if s, ok := content.(string); ok {
		msg.Content = s
		return msg
	}
	blocks, ok := content.([]any)
	if !ok {
		return msg
	}

	var textParts []string
	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch block["type"] {
		case "text":
			if t, ok := block["text"].(string); ok {
				textParts = append(textParts, t)
			}
		case "tool_use":
			// `arguments` must be a STRING of compact JSON; json.Marshal
			// of the parsed input is deterministic (sorted map keys).
			args, _ := json.Marshal(block["input"])
			msg.ToolCalls = append(msg.ToolCalls, OpenAIToolCall{
				ID:   blockString(block, "id"),
				Type: "function",
				Function: OpenAIFunction{
					Name:      blockString(block, "name"),
					Arguments: string(args),
				},
			})
		}
	}
	if len(textParts) > 0 {
		msg.Content = strings.Join(textParts, "\n")
	}
	return msg
}

// flattenText renders any Anthropic content value as plain text:
// a string passes through, text blocks are joined with a newline,
// tool_result blocks recurse into their own content.
func flattenText(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	blocks, ok := content.([]any)
	if !ok {
		return ""
	}
	var texts []string
	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch block["type"] {
		case "text":
			if t, ok := block["text"].(string); ok {
				texts = append(texts, t)
			}
		case "tool_result":
			if t := flattenText(block["content"]); t != "" {
				texts = append(texts, t)
			}
		}
	}
	return strings.Join(texts, "\n")
}

// translateToolChoice maps Anthropic's tool_choice onto OpenAI's.
// Absent stays absent (OpenAI's own default is auto) so the emitted
// body carries only what the client actually asked for.
func translateToolChoice(tc *AnthropicToolChoice) any {
	if tc == nil {
		return nil
	}
	switch tc.Type {
	case "any":
		return "required"
	case "tool":
		return OpenAIToolChoice{
			Type:     "function",
			Function: OpenAIFunction{Name: tc.Name},
		}
	default: // "auto", "none"
		return tc.Type
	}
}

// blockString reads a string field without panicking on a missing or
// non-string value — Claude Code payloads vary and must never crash a
// hop.
func blockString(block map[string]any, key string) string {
	s, _ := block[key].(string)
	return s
}
