package translate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ChatToResponses translates an OpenAI chat-completions request body into an
// OpenAI Responses API request body.
//
// Determinism is the contract (see docs/responses-wire-design.md): ordered
// structs with fixed field order, the input slice is never mutated, tool
// arguments pass through verbatim as strings. Unknown fields are dropped
// silently — forward compatibility, same as the anthropic→openai path.
//
// Dropped by design: `stop` (no responses equivalent) and `stream_options`
// (include_usage does not exist on responses — usage arrives in
// response.completed and the SSE reader emits the chat usage chunk itself).
func ChatToResponses(body []byte, upstreamModel string) ([]byte, error) {
	var req chatWireRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("unmarshal chat request: %w", err)
	}

	out := responsesWireRequest{
		Model:           upstreamModel,
		MaxOutputTokens: req.MaxTokens,
		Temperature:     req.Temperature,
		TopP:            req.TopP,
		Stream:          req.Stream,
	}
	if req.ReasoningEffort != "" {
		out.Reasoning = &responsesReasoning{Effort: req.ReasoningEffort}
	}
	if req.ParallelToolCalls != nil {
		out.ParallelToolCalls = req.ParallelToolCalls
	}

	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			// Only the FIRST system message becomes instructions; later ones
			// are dropped (documented — chat bodies from coding agents carry
			// exactly one system message).
			if out.Instructions == nil {
				if s := chatContentText(m.Content); s != "" || m.Content != nil {
					s := s
					out.Instructions = &s
				}
			}
		case "user":
			if text := chatContentText(m.Content); text != "" || m.Content != nil {
				out.Input = append(out.Input, newResponsesTextItem("user", text))
			}
		case "assistant":
			if text := chatContentText(m.Content); text != "" {
				out.Input = append(out.Input, newResponsesTextItem("assistant", text))
			}
			for _, tc := range m.ToolCalls {
				out.Input = append(out.Input, responsesInputItem{
					kind:      itemFunctionCall,
					callID:    tc.ID,
					name:      tc.Function.Name,
					arguments: tc.Function.Arguments,
				})
			}
		case "tool":
			out.Input = append(out.Input, responsesInputItem{
				kind:   itemFunctionOutput,
				callID: m.ToolCallID,
				output: chatContentText(m.Content),
			})
		}
	}

	for _, t := range req.Tools {
		out.Tools = append(out.Tools, responsesTool{
			Type:        "function",
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}

	out.ToolChoice = translateChatToolChoice(req.ToolChoice)

	return json.Marshal(out)
}

// translateChatToolChoice maps chat tool_choice onto the responses shape.
// Chat allows a bare string ("auto"/"none"/"required") or the named object;
// absent stays absent, and the named form flattens one level.
func translateChatToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var tc chatWireToolChoice
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil
	}
	if tc.Type == "function" && tc.Function != nil {
		return responsesNamedToolChoice{Type: "function", Name: tc.Function.Name}
	}
	return tc.Type
}

// chatContentText renders a chat message content value as plain text:
// a JSON string passes through, an array of blocks flattens its text blocks,
// JSON null is the empty string.
func chatContentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var texts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// ── chat wire decode types ─────────────────────────────────────────────────

type chatWireRequest struct {
	Model             string            `json:"model"`
	Messages          []chatWireMessage `json:"messages"`
	MaxTokens         int               `json:"max_tokens"`
	Temperature       *float64          `json:"temperature,omitempty"`
	TopP              *float64          `json:"top_p,omitempty"`
	Stream            bool              `json:"stream"`
	Tools             []chatWireTool    `json:"tools,omitempty"`
	ToolChoice        json.RawMessage   `json:"tool_choice,omitempty"`
	ReasoningEffort   string            `json:"reasoning_effort,omitempty"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
}

type chatWireMessage struct {
	Role       string             `json:"role"`
	Content    json.RawMessage    `json:"content"`
	ToolCallID string             `json:"tool_call_id,omitempty"`
	ToolCalls  []chatWireToolCall `json:"tool_calls,omitempty"`
}

type chatWireToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function chatWireFunction `json:"function"`
}

type chatWireTool struct {
	Type     string           `json:"type"`
	Function chatWireFunction `json:"function"`
}

type chatWireFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Arguments   string          `json:"arguments,omitempty"`
}

type chatWireToolChoice struct {
	Type     string           `json:"type"`
	Function *chatWirePointer `json:"function,omitempty"`
}

type chatWirePointer struct {
	Name string `json:"name"`
}

// ── responses wire output types (fixed field order) ───────────────────────

type responsesWireRequest struct {
	Model             string               `json:"model"`
	Input             []responsesInputItem `json:"input"`
	Instructions      *string              `json:"instructions,omitempty"`
	Tools             []responsesTool      `json:"tools,omitempty"`
	ToolChoice        any                  `json:"tool_choice,omitempty"`
	MaxOutputTokens   int                  `json:"max_output_tokens,omitempty"`
	Temperature       *float64             `json:"temperature,omitempty"`
	TopP              *float64             `json:"top_p,omitempty"`
	Stream            bool                 `json:"stream,omitempty"`
	Reasoning         *responsesReasoning  `json:"reasoning,omitempty"`
	ParallelToolCalls *bool                `json:"parallel_tool_calls,omitempty"`
}

type responsesReasoning struct {
	Effort string `json:"effort"`
}

type responsesNamedToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// input item kinds — heterogeneous array, so each kind owns its MarshalJSON.
const (
	itemText           = 1 // {role, content}
	itemFunctionCall   = 2 // {type:"function_call", call_id, name, arguments}
	itemFunctionOutput = 3 // {type:"function_call_output", call_id, output}
	itemImage          = 4 // {type:"input_image", image_url}
)

type responsesInputItem struct {
	kind      int
	role      string
	content   string
	callID    string
	name      string
	arguments string
	output    string
	imageURL  string
}

func newResponsesTextItem(role, content string) responsesInputItem {
	return responsesInputItem{kind: itemText, role: role, content: content}
}

func newResponsesImageItem(url string) responsesInputItem {
	return responsesInputItem{kind: itemImage, imageURL: url}
}

func (i responsesInputItem) MarshalJSON() ([]byte, error) {
	switch i.kind {
	case itemText:
		return json.Marshal(struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{i.role, i.content})
	case itemFunctionCall:
		return json.Marshal(struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{"function_call", i.callID, i.name, i.arguments})
	case itemFunctionOutput:
		return json.Marshal(struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
			Output string `json:"output"`
		}{"function_call_output", i.callID, i.output})
	case itemImage:
		return json.Marshal(struct {
			Type     string `json:"type"`
			ImageURL string `json:"image_url"`
		}{"input_image", i.imageURL})
	}
	return nil, fmt.Errorf("translate: unknown input item kind %d", i.kind)
}
