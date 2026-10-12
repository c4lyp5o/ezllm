package translate

import (
	"encoding/json"
	"fmt"
)

// AnthropicToResponses translates an Anthropic /v1/messages request body into
// an OpenAI Responses API request body: the third leg of the translation
// layer, for a hop the operator put on the responses wire
// (translation.responses_models) while the client speaks anthropic — Claude
// Code against a model whose chat path refuses tools+reasoning and which has
// no /v1/messages at all (tokenrouter gpt-6.1-sol, opencode-go's luna family).
//
// Determinism is the same contract as the other two translators: fixed struct
// order, tool arguments passed through as strings, the input slice never
// mutated — byte-stable bodies are what keeps an upstream prefix cache warm.
// Contract table: docs/responses-wire-design.md (anthropic leg).
//
// Dropped by design (locked decisions, same as the anthropic→chat leg):
// stop_sequences, metadata, thinking / redacted_thinking (Q4), cache_control,
// and image blocks (the responses input_image wants a URL we do not have).
func AnthropicToResponses(body []byte, upstreamModel string) ([]byte, error) {
	var req AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("unmarshal anthropic request: %w", err)
	}

	out := responsesWireRequest{
		Model:           upstreamModel,
		MaxOutputTokens: req.MaxTokens,
		Temperature:     req.Temperature,
		TopP:            req.TopP,
		Stream:          req.Stream,
	}
	if req.System != nil {
		if sys := flattenText(req.System); sys != "" {
			out.Instructions = &sys
		}
	}

	for _, m := range req.Messages {
		switch m.Role {
		case "user":
			out.Input = append(out.Input, anthropicUserItems(m.Content)...)
		case "assistant":
			out.Input = append(out.Input, anthropicAssistantItems(m.Content)...)
		}
	}

	for _, t := range req.Tools {
		out.Tools = append(out.Tools, responsesTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		})
	}
	out.ToolChoice = translateAnthropicToolChoice(req.ToolChoice)

	return json.Marshal(out)
}

// anthropicUserItems keeps the block ORDER: consecutive text blocks coalesce
// into one user item, each tool_result becomes its own function_call_output
// (the responses API pairs it with its call by call_id, wherever it lands).
func anthropicUserItems(content any) []responsesInputItem {
	if s, ok := content.(string); ok {
		return []responsesInputItem{newResponsesTextItem("user", s)}
	}
	blocks, ok := content.([]any)
	if !ok {
		return nil
	}
	var out []responsesInputItem
	var text string
	flush := func() {
		if text != "" {
			out = append(out, newResponsesTextItem("user", text))
			text = ""
		}
	}
	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch block["type"] {
		case "tool_result":
			flush()
			out = append(out, responsesInputItem{
				kind:   itemFunctionOutput,
				callID: blockString(block, "tool_use_id"),
				output: flattenText(block["content"]),
			})
		case "text":
			if t, ok := block["text"].(string); ok {
				if text != "" {
					text += "\n"
				}
				text += t
			}
		case "thinking", "redacted_thinking":
			// dropped — locked decision Q4
		case "image":
			// dropped — responses input_image needs a URL we do not have
		}
	}
	flush()
	return out
}

// anthropicAssistantItems: text → one assistant item; each tool_use → a
// function_call item with its input marshalled verbatim (json.Marshal of the
// parsed value, Go sorts map keys — deterministic like every other translator).
func anthropicAssistantItems(content any) []responsesInputItem {
	if s, ok := content.(string); ok {
		return []responsesInputItem{newResponsesTextItem("assistant", s)}
	}
	blocks, ok := content.([]any)
	if !ok {
		return nil
	}
	var out []responsesInputItem
	var text string
	flush := func() {
		if text != "" {
			out = append(out, newResponsesTextItem("assistant", text))
			text = ""
		}
	}
	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch block["type"] {
		case "text":
			if t, ok := block["text"].(string); ok {
				if text != "" {
					text += "\n"
				}
				text += t
			}
		case "tool_use":
			flush()
			args := block["input"]
			if args == nil {
				args = map[string]any{}
			}
			marshalled, err := json.Marshal(args)
			if err != nil {
				marshalled = []byte("{}")
			}
			out = append(out, responsesInputItem{
				kind:      itemFunctionCall,
				callID:    blockString(block, "id"),
				name:      blockString(block, "name"),
				arguments: string(marshalled),
			})
		case "thinking", "redacted_thinking":
			// dropped — locked decision Q4
		}
	}
	flush()
	return out
}

// translateAnthropicToolChoice maps {type: auto|any|tool, name} onto the
// responses vocabulary (auto / required / named-function). Absent stays
// absent; an unknown type falls back to "auto" rather than 400ing upstream.
func translateAnthropicToolChoice(tc *AnthropicToolChoice) any {
	if tc == nil {
		return nil
	}
	switch tc.Type {
	case "any":
		return "required"
	case "tool":
		return responsesNamedToolChoice{Type: "function", Name: tc.Name}
	default:
		return "auto"
	}
}
