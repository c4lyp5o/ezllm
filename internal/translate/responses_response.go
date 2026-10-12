package translate

import (
	"encoding/json"
	"fmt"
)

// ResponsesToChat translates a non-streaming OpenAI Responses API response
// body into an OpenAI chat-completions response body, for a chat client whose
// hop was dialed on the responses wire.
//
// Reasoning items are dropped silently (a chat body has no field for them);
// the envelope always carries a valid finish_reason; usage is remapped 1:1.
// Contract tables: docs/responses-wire-design.md.
func ResponsesToChat(body []byte, clientModel string) ([]byte, error) {
	var up responsesWireResponse
	if err := json.Unmarshal(body, &up); err != nil {
		return nil, fmt.Errorf("unmarshal responses body: %w", err)
	}

	msg := chatWireOutMessage{Role: "assistant"}
	for _, item := range up.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Type == "output_text" && c.Text != "" {
					if msg.Content != "" {
						msg.Content += "\n"
					}
					msg.Content += c.Text
				}
			}
		case "function_call":
			msg.ToolCalls = append(msg.ToolCalls, chatWireOutToolCall{
				ID:   item.CallID,
				Type: "function",
				Function: chatWireOutFunction{
					Name:      item.Name,
					Arguments: item.Arguments,
				},
			})
		}
		// "reasoning" and anything unknown: dropped by design.
	}

	finish := "stop"
	switch {
	case up.Status == "incomplete":
		finish = "length"
	case len(msg.ToolCalls) > 0:
		finish = "tool_calls"
	}

	out := chatWireOutCompletion{
		ID:      up.ID,
		Object:  "chat.completion",
		Created: up.CreatedAt,
		Model:   clientModel,
		Choices: []chatWireOutChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: finish,
		}},
		Usage: chatWireOutUsage{
			PromptTokens:     up.Usage.InputTokens,
			CompletionTokens: up.Usage.OutputTokens,
		},
	}
	return json.Marshal(out)
}

// ── responses wire decode types ───────────────────────────────────────────

type responsesWireResponse struct {
	ID         string               `json:"id"`
	CreatedAt  int64                `json:"created_at"`
	Model      string               `json:"model"`
	Status     string               `json:"status"`
	Incomplete *responsesIncomplete `json:"incomplete_details,omitempty"`
	Output     []responsesOutItem   `json:"output"`
	Usage      responsesOutUsage    `json:"usage"`
	Error      *responsesWireError  `json:"error,omitempty"`
}

type responsesIncomplete struct {
	Reason string `json:"reason"`
}

type responsesOutItem struct {
	Type      string                `json:"type"`
	Role      string                `json:"role,omitempty"`
	Content   []responsesOutContent `json:"content,omitempty"`
	CallID    string                `json:"call_id,omitempty"`
	Name      string                `json:"name,omitempty"`
	Arguments string                `json:"arguments,omitempty"`
}

type responsesOutContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type responsesOutUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type responsesWireError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code,omitempty"`
}

// ── chat wire output types (fixed field order) ────────────────────────────

type chatWireOutCompletion struct {
	ID      string              `json:"id"`
	Object  string              `json:"object"`
	Created int64               `json:"created"`
	Model   string              `json:"model"`
	Choices []chatWireOutChoice `json:"choices"`
	Usage   chatWireOutUsage    `json:"usage"`
}

type chatWireOutChoice struct {
	Index        int                `json:"index"`
	Message      chatWireOutMessage `json:"message"`
	FinishReason string             `json:"finish_reason"`
}

type chatWireOutMessage struct {
	Role      string                `json:"role"`
	Content   string                `json:"content,omitempty"`
	ToolCalls []chatWireOutToolCall `json:"tool_calls,omitempty"`
}

type chatWireOutToolCall struct {
	ID       string              `json:"id"`
	Type     string              `json:"type"`
	Function chatWireOutFunction `json:"function"`
}

type chatWireOutFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatWireOutUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}
