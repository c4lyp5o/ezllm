package translate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ResponsesToAnthropic translates a non-streaming OpenAI Responses API
// response body into an Anthropic /v1/messages response body, for the client
// that spoke anthropic while its hop was dialed on the responses wire.
//
// Output items map in order: message/output_text → text blocks,
// function_call → tool_use (arguments parsed into the input object), reasoning
// dropped (Q4). stop_reason: incomplete → max_tokens, tool calls present →
// tool_use, else end_turn. Contract: docs/responses-wire-design.md.
func ResponsesToAnthropic(body []byte, clientModel string) ([]byte, error) {
	var up responsesWireResponse
	if err := json.Unmarshal(body, &up); err != nil {
		return nil, fmt.Errorf("unmarshal responses body: %w", err)
	}

	res := AnthropicResponse{
		Type:         "message",
		Role:         "assistant",
		Content:      []AnthropicBlock{},
		Model:        clientModel,
		StopSequence: nil,
		Usage: AnthropicUsage{
			InputTokens:  up.Usage.InputTokens,
			OutputTokens: up.Usage.OutputTokens,
		},
	}
	id := up.ID
	if !strings.HasPrefix(id, "msg_") {
		id = "msg_" + id
	}
	res.ID = id

	toolCalls := 0
	for _, item := range up.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Type == "output_text" && c.Text != "" {
					res.Content = append(res.Content, AnthropicBlock{Type: "text", Text: c.Text})
				}
			}
		case "function_call":
			toolCalls++
			res.Content = append(res.Content, AnthropicBlock{
				Type:  "tool_use",
				ID:    item.CallID,
				Name:  item.Name,
				Input: parseToolArguments(item.Arguments),
			})
		case "reasoning":
			// dropped — locked decision Q4
		}
	}

	switch {
	case up.Status == "incomplete":
		res.StopReason = "max_tokens"
	case toolCalls > 0:
		res.StopReason = "tool_use"
	default:
		res.StopReason = "end_turn"
	}
	return json.Marshal(res)
}
