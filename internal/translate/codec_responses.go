package translate

// The OpenAI responses surface. The legacy path only ever ENCODED onto it; M2
// adds the decoder so a responses client can originate a cross-wire request.
//
// Statelessness is the hard edge: a responses request can name provider-side
// state (previous_response_id, conversation, item_reference). None of that can
// cross to another provider's wire, so it is refused with a reason rather than
// silently sent as a fresh, context-free conversation.

import (
	"encoding/json"
	"fmt"
)

var responsesRefusals = map[string]string{
	"previous_response_id": "provider-side response history cannot cross wires; send the conversation explicitly",
	"conversation":         "a provider-side conversation cannot cross wires; send the conversation explicitly",
	"background":           "background responses are not portable across wires",
	"include":              "include changes the returned payload; it has no equivalent on the other wires",
	"text":                 "text.format is not portable across wires",
}

type responsesInItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
	ID        string          `json:"id"`
	Summary   json.RawMessage `json:"summary"`
}

// DecodeResponsesRequest parses a /v1/responses body into the IR.
func DecodeResponsesRequest(body []byte) (*RequestIR, error) {
	top, err := topKeys(body)
	if err != nil {
		return nil, err
	}
	if err := classify(top, "responses", responsesSupported, responsesIgnorable, responsesRefusals); err != nil {
		return nil, err
	}

	ir := &RequestIR{}
	if raw := top["instructions"]; len(raw) > 0 {
		inst, err := stringFromRaw(raw)
		if err != nil {
			return nil, unsupported("responses.instructions", err.Error())
		}
		if inst != "" {
			ir.Turns = append(ir.Turns, Turn{Role: RoleSystem, Blocks: []Block{{Kind: BlockText, Text: inst}}})
		}
	}
	if ir.MaxOutputTokens, _, err = intFromRaw(top["max_output_tokens"]); err != nil {
		return nil, unsupported("responses.max_output_tokens", err.Error())
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
	if ir.ParallelToolCalls, err = boolFromRaw(top["parallel_tool_calls"]); err != nil {
		return nil, err
	}
	if raw := top["reasoning"]; len(raw) > 0 {
		var r map[string]json.RawMessage
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, unsupported("responses.reasoning", err.Error())
		}
		for k := range r {
			if k != "effort" {
				return nil, unsupported("responses.reasoning."+k,
					"only reasoning.effort has an equivalent on the other wires")
			}
		}
		if v, err := stringFromRaw(r["effort"]); err != nil {
			return nil, unsupported("responses.reasoning.effort", err.Error())
		} else {
			ir.ReasoningEffort = v
		}
	}

	if raw := top["input"]; len(raw) > 0 {
		// A bare string is a single user turn.
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			ir.Turns = append(ir.Turns, Turn{Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: text}}})
		} else {
			var items []responsesInItem
			if err := json.Unmarshal(raw, &items); err != nil {
				return nil, unsupported("responses.input", err.Error())
			}
			for _, it := range items {
				turns, err := responsesTurns(it)
				if err != nil {
					return nil, err
				}
				ir.Turns = append(ir.Turns, turns...)
			}
		}
	}

	if raw := top["tools"]; len(raw) > 0 {
		var tools []responsesTool
		if err := json.Unmarshal(raw, &tools); err != nil {
			return nil, unsupported("responses.tools", err.Error())
		}
		for _, t := range tools {
			if t.Type != "function" {
				return nil, unsupported("responses.tools.type",
					fmt.Sprintf("tool type %q is not a function; only function tools cross wires", t.Type))
			}
			ir.Tools = append(ir.Tools, Tool{Name: t.Name, Description: t.Description, Schema: t.Parameters})
		}
	}

	if raw := top["tool_choice"]; len(raw) > 0 {
		tc, err := responsesToolChoice(raw)
		if err != nil {
			return nil, err
		}
		ir.ToolChoice = tc
	}
	return ir, nil
}

func responsesToolChoice(raw json.RawMessage) (*ToolChoice, error) {
	var mode string
	if err := json.Unmarshal(raw, &mode); err == nil {
		switch mode {
		case ToolChoiceAuto, ToolChoiceNone, ToolChoiceRequired:
			return &ToolChoice{Mode: mode}, nil
		}
		return nil, unsupported("responses.tool_choice",
			fmt.Sprintf("%q is not a portable tool choice", mode))
	}
	var named struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &named); err != nil {
		return nil, unsupported("responses.tool_choice", err.Error())
	}
	switch named.Type {
	case "function":
		if named.Name == "" {
			return nil, unsupported("responses.tool_choice", "a named function choice without a name")
		}
		return &ToolChoice{Mode: ToolChoiceTool, Name: named.Name}, nil
	case "allowed_tools":
		return nil, unsupported("responses.tool_choice",
			"allowed_tools restricts a provider-side tool set; it cannot be expressed on the other wires")
	}
	return nil, unsupported("responses.tool_choice",
		fmt.Sprintf("choice type %q is not portable", named.Type))
}

func responsesTurns(it responsesInItem) ([]Turn, error) {
	switch it.Type {
	case "", "message":
		text, err := textContent(it.Content, "responses.input.content")
		if err != nil {
			return nil, err
		}
		role := it.Role
		if role == "" {
			role = RoleUser
		}
		switch role {
		case RoleUser, RoleAssistant, RoleSystem, RoleDeveloper:
		default:
			return nil, unsupported("responses.input.role",
				fmt.Sprintf("role %q is not portable", role))
		}
		turn := Turn{Role: role}
		if len(it.Content) > 0 {
			turn.Blocks = []Block{{Kind: BlockText, Text: text}}
		}
		return []Turn{turn}, nil

	case "function_call":
		return []Turn{{Role: RoleAssistant, Blocks: []Block{{
			Kind: BlockCall, CallID: it.CallID, Name: it.Name, Arguments: it.Arguments,
		}}}}, nil

	case "function_call_output":
		out, err := textContent(it.Output, "responses.input.output")
		if err != nil {
			return nil, err
		}
		return []Turn{{Role: RoleTool, Blocks: []Block{{
			Kind: BlockResult, CallID: it.CallID, Text: out,
		}}}}, nil

	case "reasoning":
		return nil, unsupported("responses.input.reasoning",
			"reasoning replay cannot be translated across wires")

	case "item_reference":
		return nil, unsupported("responses.input.item_reference",
			"item references need the provider's own history, which cannot cross wires")
	}
	return nil, unsupported("responses.input."+it.Type,
		"only message, function_call and function_call_output items are portable")
}

// ── encoder ─────────────────────────────────────────────────────────────────

// EncodeResponsesRequest renders the IR onto the responses wire. It reuses the
// legacy request types so the shape stays field-for-field identical to
// ChatToResponses/AnthropicToResponses.
func EncodeResponsesRequest(ir *RequestIR, upstreamModel string) ([]byte, error) {
	out := responsesWireRequest{
		Model:             upstreamModel,
		Temperature:       ir.Temperature,
		TopP:              ir.TopP,
		Stream:            ir.Stream,
		ParallelToolCalls: ir.ParallelToolCalls,
	}
	if ir.MaxOutputTokens != nil {
		out.MaxOutputTokens = *ir.MaxOutputTokens
	}
	if ir.ReasoningEffort != "" {
		out.Reasoning = &responsesReasoning{Effort: ir.ReasoningEffort}
	}

	// System and developer turns coalesce into `instructions`, in order. The
	// legacy path kept only the FIRST system message; the portable path keeps
	// every one, because dropping instructions changes model behaviour.
	var instructions []string
	for _, t := range ir.Turns {
		if t.Role != RoleSystem && t.Role != RoleDeveloper {
			continue
		}
		if text := t.Text(); text != "" {
			instructions = append(instructions, text)
		}
	}
	if len(instructions) > 0 {
		joined := joinParagraphs(instructions)
		out.Instructions = &joined
	}

	for _, t := range ir.Turns {
		switch t.Role {
		case RoleSystem, RoleDeveloper:
			continue // lifted into instructions
		case RoleTool:
			for _, b := range t.Blocks {
				if b.Kind != BlockResult {
					continue
				}
				out.Input = append(out.Input, responsesInputItem{
					kind: itemFunctionOutput, callID: b.CallID, output: b.Text,
				})
			}
		case RoleAssistant:
			if text := t.Text(); text != "" {
				out.Input = append(out.Input, newResponsesTextItem(RoleAssistant, text))
			}
			for _, b := range t.Blocks {
				if b.Kind != BlockCall {
					continue
				}
				out.Input = append(out.Input, responsesInputItem{
					kind: itemFunctionCall, callID: b.CallID, name: b.Name, arguments: b.Arguments,
				})
			}
		default:
			if len(t.Blocks) == 0 {
				continue // nothing to say (the legacy encoder did the same)
			}
			out.Input = append(out.Input, newResponsesTextItem(t.Role, t.Text()))
		}
	}

	for _, t := range ir.Tools {
		out.Tools = append(out.Tools, responsesTool{
			Type: "function", Name: t.Name, Description: t.Description, Parameters: t.Schema,
		})
	}
	if ir.ToolChoice != nil {
		switch ir.ToolChoice.Mode {
		case ToolChoiceTool:
			out.ToolChoice = responsesNamedToolChoice{Type: "function", Name: ir.ToolChoice.Name}
		default:
			out.ToolChoice = ir.ToolChoice.Mode
		}
	}
	return json.Marshal(out)
}

// joinParagraphs keeps instruction boundaries readable and deterministic.
func joinParagraphs(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "\n\n"
		}
		out += p
	}
	return out
}
