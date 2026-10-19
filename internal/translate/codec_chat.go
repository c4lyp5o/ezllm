package translate

// The chat-completions surface: decoder (client body -> IR) and encoder
// (IR -> upstream body). Encoder presence rules match the legacy translator's
// output for every field the legacy path carries, so the two can be compared
// field by field (see codec_request_test.go).

import (
	"encoding/json"
	"fmt"
)

// DecodeChatRequest parses a chat-completions request body into the IR.
func DecodeChatRequest(body []byte) (*RequestIR, error) {
	top, err := topKeys(body)
	if err != nil {
		return nil, err
	}
	if err := classify(top, "chat", chatSupported, chatIgnorable, nil); err != nil {
		return nil, err
	}

	ir := &RequestIR{}
	if ir.Stream, err = boolValue(top["stream"]); err != nil {
		return nil, err
	}
	if ir.Temperature, err = floatFromRaw(top["temperature"]); err != nil {
		return nil, err
	}
	if ir.TopP, err = floatFromRaw(top["top_p"]); err != nil {
		return nil, err
	}
	if ir.ParallelToolCalls, err = boolFromRaw(top["parallel_tool_calls"]); err != nil {
		return nil, err
	}
	if ir.ReasoningEffort, err = stringFromRaw(top["reasoning_effort"]); err != nil {
		return nil, err
	}
	if ir.Stop, err = stopFromRaw(top["stop"]); err != nil {
		return nil, err
	}

	// Both limit spellings exist in the wild. Carrying both would let one wire
	// silently win, so agreement is required and disagreement is refused.
	maxTokens, hasMax, err := intFromRaw(top["max_tokens"])
	if err != nil {
		return nil, unsupported("chat.max_tokens", err.Error())
	}
	maxCompletion, hasMaxCompletion, err := intFromRaw(top["max_completion_tokens"])
	if err != nil {
		return nil, unsupported("chat.max_completion_tokens", err.Error())
	}
	switch {
	case hasMax && hasMaxCompletion && *maxTokens != *maxCompletion:
		return nil, unsupported("chat.max_tokens",
			"max_tokens and max_completion_tokens disagree; refusing to guess which limit applies")
	case hasMax:
		ir.MaxOutputTokens = maxTokens
	case hasMaxCompletion:
		ir.MaxOutputTokens = maxCompletion
	}

	if raw := top["messages"]; len(raw) > 0 {
		var msgs []chatWireMessage
		if err := json.Unmarshal(raw, &msgs); err != nil {
			return nil, unsupported("chat.messages", err.Error())
		}
		for _, m := range msgs {
			turn, err := chatTurn(m)
			if err != nil {
				return nil, err
			}
			ir.Turns = append(ir.Turns, turn)
		}
	}

	if raw := top["tools"]; len(raw) > 0 {
		var tools []chatWireTool
		if err := json.Unmarshal(raw, &tools); err != nil {
			return nil, unsupported("chat.tools", err.Error())
		}
		for _, t := range tools {
			if t.Type != "" && t.Type != "function" {
				return nil, unsupported("chat.tools.type",
					fmt.Sprintf("tool type %q is not a function; only function tools cross wires", t.Type))
			}
			ir.Tools = append(ir.Tools, Tool{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Schema:      t.Function.Parameters,
			})
		}
	}

	if raw := top["tool_choice"]; len(raw) > 0 {
		tc, err := chatToolChoice(raw)
		if err != nil {
			return nil, err
		}
		ir.ToolChoice = tc
	}
	return ir, nil
}

func boolValue(raw json.RawMessage) (bool, error) {
	b, err := boolFromRaw(raw)
	if err != nil {
		return false, err
	}
	if b == nil {
		return false, nil
	}
	return *b, nil
}

// chatContentBlocks parses a chat message's content into IR blocks, carrying
// both text and image parts. A plain string becomes a single text block.
func chatContentBlocks(raw json.RawMessage, what string) ([]Block, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, nil
		}
		return []Block{{Kind: BlockText, Text: s}}, nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, unsupported(what, "content must be a string or an array of parts")
	}
	var blocks []Block
	for _, p := range parts {
		var part struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL any    `json:"image_url"`
		}
		if err := json.Unmarshal(p, &part); err != nil {
			return nil, unsupported(what, "unreadable content part")
		}
		switch part.Type {
		case "text", "input_text", "output_text":
			if part.Text != "" {
				blocks = append(blocks, Block{Kind: BlockText, Text: part.Text})
			}
		case "":
			// Some compatible upstreams send {text: "..."} with no type.
			if part.Text != "" {
				blocks = append(blocks, Block{Kind: BlockText, Text: part.Text})
			}
		case "image_url":
			url := ""
			switch v := part.ImageURL.(type) {
			case string:
				url = v
			case map[string]any:
				if u, ok := v["url"].(string); ok {
					url = u
				}
			}
			if url == "" {
				// try re-decode as object with url
				var alt struct {
					ImageURL struct {
						URL string `json:"url"`
					} `json:"image_url"`
				}
				if json.Unmarshal(p, &alt) == nil {
					url = alt.ImageURL.URL
				}
			}
			if url == "" {
				return nil, unsupported(what+".image_url", "missing image url")
			}
			blocks = append(blocks, Block{Kind: BlockImage, ImageURL: url})
		default:
			return nil, unsupported(what+"."+part.Type,
				"only text and image content is portable across the three wires")
		}
	}
	return blocks, nil
}

func chatTurn(m chatWireMessage) (Turn, error) {
	switch m.Role {
	case RoleSystem, RoleDeveloper, RoleUser:
		blocks, err := chatContentBlocks(m.Content, "chat.content")
		if err != nil {
			return Turn{}, err
		}
		return Turn{Role: m.Role, Blocks: blocks}, nil

	case RoleAssistant:
		blocks, err := chatContentBlocks(m.Content, "chat.content")
		if err != nil {
			return Turn{}, err
		}
		turn := Turn{Role: RoleAssistant, Blocks: blocks}
		for _, tc := range m.ToolCalls {
			if tc.Type != "" && tc.Type != "function" {
				return Turn{}, unsupported("chat.tool_calls.type",
					fmt.Sprintf("tool call type %q is not a function", tc.Type))
			}
			turn.Blocks = append(turn.Blocks, Block{
				Kind:      BlockCall,
				CallID:    tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
		return turn, nil

	case RoleTool:
		text, err := textContent(m.Content, "chat.tool_content")
		if err != nil {
			return Turn{}, err
		}
		return Turn{Role: RoleTool, Blocks: []Block{{
			Kind: BlockResult, CallID: m.ToolCallID, Text: text,
		}}}, nil
	}
	return Turn{}, unsupported("chat.messages.role",
		fmt.Sprintf("role %q is not portable across the three wires", m.Role))
}

func chatToolChoice(raw json.RawMessage) (*ToolChoice, error) {
	var mode string
	if err := json.Unmarshal(raw, &mode); err == nil {
		switch mode {
		case ToolChoiceAuto, ToolChoiceNone, ToolChoiceRequired:
			return &ToolChoice{Mode: mode}, nil
		}
		return nil, unsupported("chat.tool_choice",
			fmt.Sprintf("%q is not a portable tool choice", mode))
	}
	var named struct {
		Type     string `json:"type"`
		Function *struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &named); err != nil {
		return nil, unsupported("chat.tool_choice", err.Error())
	}
	if named.Type != "function" || named.Function == nil || named.Function.Name == "" {
		return nil, unsupported("chat.tool_choice",
			"only a named function choice is portable")
	}
	return &ToolChoice{Mode: ToolChoiceTool, Name: named.Function.Name}, nil
}

// ── encoder ─────────────────────────────────────────────────────────────────

// chatOutRequest mirrors the legacy translator's field set (and its presence
// rules) with one deliberate difference: an ABSENT output limit stays absent
// instead of being sent as 0.
type chatOutRequest struct {
	Model             string               `json:"model"`
	Messages          []chatOutMessage     `json:"messages"`
	MaxTokens         *int                 `json:"max_tokens,omitempty"`
	Stop              []string             `json:"stop,omitempty"`
	Temperature       *float64             `json:"temperature,omitempty"`
	TopP              *float64             `json:"top_p,omitempty"`
	Stream            bool                 `json:"stream,omitempty"`
	StreamOptions     *OpenAIStreamOptions `json:"stream_options,omitempty"`
	Tools             []OpenAITool         `json:"tools,omitempty"`
	ToolChoice        any                  `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool                `json:"parallel_tool_calls,omitempty"`
	ReasoningEffort   string               `json:"reasoning_effort,omitempty"`
}

type chatOutMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []OpenAIToolCall `json:"tool_calls,omitempty"`
}

// EncodeChatRequest renders the IR as a chat-completions body.
func EncodeChatRequest(ir *RequestIR, upstreamModel string) ([]byte, error) {
	out := chatOutRequest{
		Model:             upstreamModel,
		MaxTokens:         ir.MaxOutputTokens,
		Stop:              ir.Stop,
		Temperature:       ir.Temperature,
		TopP:              ir.TopP,
		Stream:            ir.Stream,
		ParallelToolCalls: ir.ParallelToolCalls,
		ReasoningEffort:   ir.ReasoningEffort,
	}
	if ir.Stream {
		// Needed for an honest ledger on a streamed call: without it the
		// upstream may omit usage entirely.
		out.StreamOptions = &OpenAIStreamOptions{IncludeUsage: true}
	}
	for _, t := range ir.Turns {
		switch t.Role {
		case RoleTool:
			for _, b := range t.Blocks {
				if b.Kind != BlockResult {
					continue
				}
				out.Messages = append(out.Messages, chatOutMessage{
					Role: RoleTool, ToolCallID: b.CallID, Content: b.Text,
				})
			}
		case RoleAssistant:
			msg := chatOutMessage{Role: RoleAssistant, Content: chatContentFromBlocks(t.Blocks)}
			for _, b := range t.Blocks {
				if b.Kind != BlockCall {
					continue
				}
				msg.ToolCalls = append(msg.ToolCalls, OpenAIToolCall{
					ID:   b.CallID,
					Type: "function",
					Function: OpenAIFunction{
						Name:      b.Name,
						Arguments: b.Arguments,
					},
				})
			}
			out.Messages = append(out.Messages, msg)
		default:
			out.Messages = append(out.Messages, chatOutMessage{Role: t.Role, Content: chatContentFromBlocks(t.Blocks)})
		}
	}
	for _, t := range ir.Tools {
		out.Tools = append(out.Tools, OpenAITool{
			Type: "function",
			Function: OpenAIFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Schema,
			},
		})
	}
	out.ToolChoice = encodeChatToolChoice(ir.ToolChoice)
	return json.Marshal(out)
}

func encodeChatToolChoice(tc *ToolChoice) any {
	if tc == nil {
		return nil
	}
	switch tc.Mode {
	case ToolChoiceTool:
		return OpenAIToolChoice{
			Type:     "function",
			Function: OpenAIFunction{Name: tc.Name},
		}
	default:
		return tc.Mode
	}
}
