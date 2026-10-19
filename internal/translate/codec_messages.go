package translate

// The anthropic messages surface. Decoding lifts `system` into IR turns and
// splits a user turn's blocks into text runs and tool-result turns IN SOURCE
// ORDER (a tool_result must not jump ahead of the text that preceded it).
// Encoding is the inverse: system turns become the top-level system array, and
// CONSECUTIVE tool turns become ONE user message with several tool_result
// blocks — which is the shape the API expects after a multi-tool assistant
// turn.

import (
	"encoding/json"
	"fmt"
)

// messagesRefusals names the anthropic fields that exist, are meaningful, and
// still cannot be carried across wires — each with the reason the operator
// needs to understand the refusal.
var messagesRefusals = map[string]string{
	"thinking": "reasoning replay cannot be translated across wires",
	"top_k":    "the chat and responses wires have no portable top_k",
}

type messagesInMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type messagesInBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	Source    json.RawMessage `json:"source"`
}

// DecodeMessagesRequest parses an anthropic /v1/messages body into the IR.
func DecodeMessagesRequest(body []byte) (*RequestIR, error) {
	top, err := topKeys(body)
	if err != nil {
		return nil, err
	}
	if err := classify(top, "messages", messagesSupported, messagesIgnorable, messagesRefusals); err != nil {
		return nil, err
	}

	ir := &RequestIR{}
	if raw := top["system"]; len(raw) > 0 {
		sys, err := textContent(raw, "messages.system")
		if err != nil {
			return nil, err
		}
		if sys != "" {
			ir.Turns = append(ir.Turns, Turn{
				Role:   RoleSystem,
				Blocks: []Block{{Kind: BlockText, Text: sys}},
			})
		}
	}
	if ir.MaxOutputTokens, _, err = intFromRaw(top["max_tokens"]); err != nil {
		return nil, unsupported("messages.max_tokens", err.Error())
	}
	if ir.Temperature, err = floatFromRaw(top["temperature"]); err != nil {
		return nil, err
	}
	if ir.TopP, err = floatFromRaw(top["top_p"]); err != nil {
		return nil, err
	}
	if ir.Stream, err = boolValue(top["stream"]); err != nil {
		return nil, err
	}
	if raw := top["stop_sequences"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &ir.Stop); err != nil {
			return nil, unsupported("messages.stop_sequences", err.Error())
		}
	}

	if raw := top["messages"]; len(raw) > 0 {
		var msgs []messagesInMessage
		if err := json.Unmarshal(raw, &msgs); err != nil {
			return nil, unsupported("messages.messages", err.Error())
		}
		for _, m := range msgs {
			turns, err := messagesTurns(m)
			if err != nil {
				return nil, err
			}
			ir.Turns = append(ir.Turns, turns...)
		}
	}

	if raw := top["tools"]; len(raw) > 0 {
		var tools []AnthropicTool
		if err := json.Unmarshal(raw, &tools); err != nil {
			return nil, unsupported("messages.tools", err.Error())
		}
		for _, t := range tools {
			ir.Tools = append(ir.Tools, Tool{Name: t.Name, Description: t.Description, Schema: t.InputSchema})
		}
	}

	if raw := top["tool_choice"]; len(raw) > 0 {
		var tc AnthropicToolChoice
		if err := json.Unmarshal(raw, &tc); err != nil {
			return nil, unsupported("messages.tool_choice", err.Error())
		}
		out, err := messagesToolChoice(tc)
		if err != nil {
			return nil, err
		}
		ir.ToolChoice = out
	}
	return ir, nil
}

func messagesToolChoice(tc AnthropicToolChoice) (*ToolChoice, error) {
	switch tc.Type {
	case "auto", ToolChoiceNone:
		return &ToolChoice{Mode: tc.Type}, nil
	case "any":
		return &ToolChoice{Mode: ToolChoiceRequired}, nil
	case "tool":
		if tc.Name == "" {
			return nil, unsupported("messages.tool_choice", "a named choice without a name")
		}
		return &ToolChoice{Mode: ToolChoiceTool, Name: tc.Name}, nil
	}
	return nil, unsupported("messages.tool_choice",
		fmt.Sprintf("%q is not a portable tool choice", tc.Type))
}

// messagesTurns expands one anthropic message into IR turns, preserving block
// order: text runs become user/assistant turns, each tool_result becomes its
// own tool turn.
func messagesTurns(m messagesInMessage) ([]Turn, error) {
	// A plain string body is the common case.
	var text string
	if err := json.Unmarshal(m.Content, &text); err == nil && len(m.Content) > 0 {
		return []Turn{{Role: m.Role, Blocks: []Block{{Kind: BlockText, Text: text}}}}, nil
	}

	var blocks []messagesInBlock
	if len(m.Content) > 0 {
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			return nil, unsupported("messages.content", err.Error())
		}
	}

	var (
		turns   []Turn
		pending []Block // accumulated text, flushed when a result interrupts
	)
	flush := func() {
		if len(pending) == 0 {
			return
		}
		turns = append(turns, Turn{Role: m.Role, Blocks: pending})
		pending = nil
	}

	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				pending = append(pending, Block{Kind: BlockText, Text: b.Text})
			}
		case "tool_result":
			flush()
			result, err := textContent(b.Content, "messages.tool_result.content")
			if err != nil {
				return nil, err
			}
			turns = append(turns, Turn{Role: RoleTool, Blocks: []Block{{
				Kind: BlockResult, CallID: b.ToolUseID, Text: result,
			}}})
		case "tool_use":
			pending = append(pending, Block{
				Kind:      BlockCall,
				CallID:    b.ID,
				Name:      b.Name,
				Arguments: argumentsFromInput(b.Input, b.ID),
			})
		case "image":
			url, err := anthropicImageSourceURL(b.Source)
			if err != nil {
				return nil, err
			}
			pending = append(pending, Block{Kind: BlockImage, ImageURL: url})
		case "thinking", "redacted_thinking":
			return nil, unsupported("messages.content."+b.Type,
				"reasoning replay cannot be translated across wires")
		case "":
			return nil, unsupported("messages.content", "a content block with no type")
		default:
			return nil, unsupported("messages.content."+b.Type,
				"only text, tool_use and tool_result blocks are portable")
		}
	}
	flush()
	return turns, nil
}

// anthropicImageSourceURL resolves an Anthropic image block's source into a URL.
func anthropicImageSourceURL(raw json.RawMessage) (string, error) {
	var src struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	}
	if err := json.Unmarshal(raw, &src); err != nil {
		return "", unsupported("messages.content.image.source", "unreadable image source")
	}
	switch src.Type {
	case "base64":
		if src.Data == "" {
			return "", unsupported("messages.content.image.source", "empty base64 data")
		}
		mt := src.MediaType
		if mt == "" {
			mt = "image/png"
		}
		return "data:" + mt + ";base64," + src.Data, nil
	case "url":
		if src.URL == "" {
			return "", unsupported("messages.content.image.source", "empty url")
		}
		return src.URL, nil
	default:
		return "", unsupported("messages.content.image.source",
			fmt.Sprintf("unsupported source type %q", src.Type))
	}
}
func argumentsFromInput(raw json.RawMessage, callID string) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		if _, err := argumentsObject(string(raw), callID); err != nil {
			return "{}"
		}
	}
	if _, isObj := v.(map[string]any); !isObj {
		return "{}"
	}
	return marshalObjectArgs(v)
}

// ── encoder ─────────────────────────────────────────────────────────────────

type messagesOutRequest struct {
	Model         string               `json:"model"`
	MaxTokens     int                  `json:"max_tokens"`
	System        []messagesOutBlock   `json:"system,omitempty"`
	Messages      []messagesOutMessage `json:"messages"`
	StopSequences []string             `json:"stop_sequences,omitempty"`
	Temperature   *float64             `json:"temperature,omitempty"`
	TopP          *float64             `json:"top_p,omitempty"`
	Stream        bool                 `json:"stream,omitempty"`
	Tools         []AnthropicTool      `json:"tools,omitempty"`
	ToolChoice    *AnthropicToolChoice `json:"tool_choice,omitempty"`
}

type messagesOutMessage struct {
	Role    string             `json:"role"`
	Content []messagesOutBlock `json:"content"`
}

type messagesOutBlock struct {
	Type      string  `json:"type"`
	Text      string  `json:"text,omitempty"`
	ID        string  `json:"id,omitempty"`
	Name      string  `json:"name,omitempty"`
	Input     any     `json:"input,omitempty"`
	ToolUseID string  `json:"tool_use_id,omitempty"`
	Content   *string `json:"content,omitempty"`
	Source    any     `json:"source,omitempty"`
}

// EncodeMessagesRequest renders the IR as an anthropic /v1/messages body.
func EncodeMessagesRequest(ir *RequestIR, upstreamModel string) ([]byte, error) {
	if ir.MaxOutputTokens == nil {
		return nil, unsupported("max_tokens",
			"the messages wire requires an explicit output limit and the source request had none")
	}
	if ir.ReasoningEffort != "" {
		return nil, unsupported("reasoning_effort",
			"the messages wire has no portable reasoning-effort control")
	}

	out := messagesOutRequest{
		Model:         upstreamModel,
		MaxTokens:     *ir.MaxOutputTokens,
		StopSequences: ir.Stop,
		Temperature:   ir.Temperature,
		TopP:          ir.TopP,
		Stream:        ir.Stream,
	}

	// System and developer turns coalesce into the top-level system array:
	// anthropic has no system role inside `messages`. ORDER is preserved.
	for _, t := range ir.Turns {
		if t.Role != RoleSystem && t.Role != RoleDeveloper {
			continue
		}
		if text := t.Text(); text != "" {
			out.System = append(out.System, messagesOutBlock{Type: "text", Text: text})
		}
	}

	for i := 0; i < len(ir.Turns); i++ {
		t := ir.Turns[i]
		switch t.Role {
		case RoleSystem, RoleDeveloper:
			continue // lifted above

		case RoleTool:
			// One user message carries every consecutive tool result: that is
			// the shape the API expects after a multi-call assistant turn.
			msg := messagesOutMessage{Role: RoleUser}
			for ; i < len(ir.Turns) && ir.Turns[i].Role == RoleTool; i++ {
				for _, b := range ir.Turns[i].Blocks {
					if b.Kind != BlockResult {
						continue
					}
					text := b.Text
					msg.Content = append(msg.Content, messagesOutBlock{
						Type: "tool_result", ToolUseID: b.CallID, Content: &text,
					})
				}
			}
			i-- // the outer loop increments
			out.Messages = append(out.Messages, msg)

		case RoleAssistant:
			msg := messagesOutMessage{Role: RoleAssistant}
			for _, b := range t.Blocks {
				switch b.Kind {
				case BlockText:
					if b.Text != "" {
						msg.Content = append(msg.Content, messagesOutBlock{Type: "text", Text: b.Text})
					}
				case BlockCall:
					input, err := argumentsObject(b.Arguments, b.CallID)
					if err != nil {
						return nil, err
					}
					msg.Content = append(msg.Content, messagesOutBlock{
						Type: "tool_use", ID: b.CallID, Name: b.Name, Input: input,
					})
				case BlockImage:
					msg.Content = append(msg.Content, messagesOutBlock{
						Type:   "image",
						Source: anthropicImageSourceFromURL(b.ImageURL),
					})
				}
			}
			out.Messages = append(out.Messages, msg)

		default:
			msg := messagesOutMessage{Role: t.Role}
			for _, b := range t.Blocks {
				if b.Kind == BlockText && b.Text != "" {
					msg.Content = append(msg.Content, messagesOutBlock{Type: "text", Text: b.Text})
				}
				if b.Kind == BlockImage {
					msg.Content = append(msg.Content, messagesOutBlock{
						Type:   "image",
						Source: anthropicImageSourceFromURL(b.ImageURL),
					})
				}
			}
			out.Messages = append(out.Messages, msg)
		}
	}

	for _, t := range ir.Tools {
		out.Tools = append(out.Tools, AnthropicTool{
			Name: t.Name, Description: t.Description, InputSchema: t.Schema,
		})
	}
	if ir.ToolChoice != nil {
		switch ir.ToolChoice.Mode {
		case ToolChoiceRequired:
			out.ToolChoice = &AnthropicToolChoice{Type: "any"}
		case ToolChoiceTool:
			out.ToolChoice = &AnthropicToolChoice{Type: "tool", Name: ir.ToolChoice.Name}
		default:
			out.ToolChoice = &AnthropicToolChoice{Type: ir.ToolChoice.Mode}
		}
	}
	return json.Marshal(out)
}
