package translate

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// decodeUsage reads counters with presence. Cache splits are observed, not
// emitted into translated client usage (the inclusive ledger exception).
func decodeUsage(raw json.RawMessage, surface provider.Surface, opts CodecOptions) (UsageIR, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return UsageIR{}, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return UsageIR{}, invalidResponse("usage is not an object")
	}
	read := func(name string) (*int, error) {
		r := m[name]
		if len(r) == 0 || string(r) == "null" {
			return nil, nil
		}
		var v int
		if err := json.Unmarshal(r, &v); err != nil || (!opts.Legacy && v < 0) {
			return nil, invalidResponse("invalid usage counter " + name)
		}
		return &v, nil
	}
	u := UsageIR{}
	var err error
	switch surface {
	case provider.SurfaceChat:
		u.Input, err = read("prompt_tokens")
		if err != nil {
			return u, err
		}
		u.Output, err = read("completion_tokens")
	case provider.SurfaceAnthropic:
		u.Input, err = read("input_tokens")
		if err != nil {
			return u, err
		}
		u.Output, err = read("output_tokens")
		if err != nil {
			return u, err
		}
		u.CacheRead, err = read("cache_read_input_tokens")
		if err != nil {
			return u, err
		}
		u.CacheWrite, err = read("cache_creation_input_tokens")
		if err != nil {
			return u, err
		}
		// Nested TTL breakdown is a fallback, never added to the aggregate twice.
		if u.CacheWrite == nil && len(m["cache_creation"]) > 0 {
			var nested map[string]json.RawMessage
			if err := json.Unmarshal(m["cache_creation"], &nested); err != nil {
				return u, invalidResponse("invalid cache_creation breakdown")
			}
			total := 0
			present := false
			for _, k := range []string{"ephemeral_5m_input_tokens", "ephemeral_1h_input_tokens"} {
				if r := nested[k]; len(r) > 0 {
					var n int
					if err := json.Unmarshal(r, &n); err != nil || (!opts.Legacy && n < 0) || n > math.MaxInt-total {
						return u, invalidResponse("invalid cache_creation counter")
					}
					total += n
					present = true
				}
			}
			if present {
				u.CacheWrite = &total
			}
		}
		if !opts.Legacy {
			return NormalizeMessagesUsage(u, opts.MessagesUsage)
		}
	case provider.SurfaceResponses:
		u.Input, err = read("input_tokens")
		if err != nil {
			return u, err
		}
		u.Output, err = read("output_tokens")
	default:
		return u, invalidResponse("unknown usage surface")
	}
	if err != nil {
		return u, err
	}
	// Optional observed details remain in IR; client projections suppress splits.
	for key, dest := range map[string]**int{"prompt_tokens_details": &u.CacheRead, "input_tokens_details": &u.CacheRead, "completion_tokens_details": &u.Reasoning, "output_tokens_details": &u.Reasoning} {
		if r := m[key]; len(r) > 0 && string(r) != "null" {
			var details map[string]json.RawMessage
			if err := json.Unmarshal(r, &details); err != nil {
				return u, invalidResponse("invalid usage details")
			}
			name := "cached_tokens"
			if strings.Contains(key, "completion") || strings.Contains(key, "output") {
				name = "reasoning_tokens"
			}
			if r := details[name]; len(r) > 0 {
				var n int
				if err := json.Unmarshal(r, &n); err != nil || (!opts.Legacy && n < 0) {
					return u, invalidResponse("invalid usage detail counter")
				}
				*dest = &n
			}
		}
	}
	return u, nil
}

// NormalizeMessagesUsage yields full input. Unknown conventions cannot be
// guessed, even when the current snapshot happens not to mention a cache.
func NormalizeMessagesUsage(u UsageIR, convention UsageConvention) (UsageIR, error) {
	if convention != UsageExclusive && convention != UsageInclusive {
		return u, invalidResponse("messages usage requires an explicit convention")
	}
	if u.Input == nil {
		return u, nil
	}
	total := *u.Input
	if total < 0 {
		return u, invalidResponse("negative input usage")
	}
	exclusive := convention == UsageExclusive || u.CacheRead != nil && total < *u.CacheRead
	if exclusive {
		for _, p := range []*int{u.CacheRead, u.CacheWrite} {
			if p != nil {
				if *p < 0 || *p > math.MaxInt-total {
					return u, invalidResponse("input usage overflow")
				}
				total += *p
			}
		}
	}
	u.Input = &total
	return u, nil
}

func DecodeResponse(body []byte, surface provider.Surface, opts CodecOptions) (*ResponseIR, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, invalidResponse("response is not a JSON object")
	}
	if top == nil && opts.Legacy {
		// "null" decoded into zero values and produced an empty envelope in
		// the historic readers; strict callers refuse it.
		top = map[string]json.RawMessage{}
	} else if top == nil {
		return nil, invalidResponse("response is not a JSON object")
	}
	ir := &ResponseIR{Finish: FinishCompleted}
	if r := top["error"]; len(r) > 0 && string(r) != "null" {
		var e ErrorIR
		if err := json.Unmarshal(r, &e); err != nil {
			if !opts.Legacy {
				return nil, invalidResponse("invalid error envelope")
			}
			// Historic readers ignored a malformed error field and produced
			// the normal envelope instead.
		} else {
			ir.Error = &e
			ir.Finish = FinishFailed
			if !opts.Legacy && surface != provider.SurfaceResponses {
				return ir, nil
			}
		}
	}
	var err error
	switch surface {
	case provider.SurfaceChat:
		var up struct {
			ID      string `json:"id"`
			Created int64  `json:"created"`
			Choices []struct {
				Index   int `json:"index"`
				Message struct {
					Role    string           `json:"role"`
					Content *string          `json:"content"`
					Refusal *string          `json:"refusal"`
					Calls   []OpenAIToolCall `json:"tool_calls"`
				} `json:"message"`
				Finish string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(body, &up); err != nil {
			return nil, invalidResponse(err.Error())
		}
		if !opts.Legacy && (up.ID == "" || len(up.Choices) != 1 || up.Choices[0].Index != 0 || up.Choices[0].Message.Role != "assistant" || up.Choices[0].Finish == "") {
			return nil, invalidResponse("invalid chat completion envelope")
		}
		ir.ID, ir.Created = up.ID, up.Created
		if len(up.Choices) > 0 {
			c := up.Choices[0]
			if !opts.Legacy && !validChatFinish(c.Finish) {
				return nil, invalidResponse("unknown chat finish_reason")
			}
			ir.Finish = chatFinish(c.Finish)
			if c.Message.Content != nil && *c.Message.Content != "" {
				ir.Blocks = append(ir.Blocks, Block{Kind: BlockText, Text: *c.Message.Content})
			}
			if c.Message.Refusal != nil && *c.Message.Refusal != "" && !opts.Legacy {
				ir.Blocks = append(ir.Blocks, Block{Kind: BlockText, Text: *c.Message.Refusal})
				ir.Finish = FinishRefusal
			}
			for _, call := range c.Message.Calls {
				if !opts.Legacy && (call.ID == "" || call.Function.Name == "" || call.Type != "function") {
					return nil, invalidResponse("invalid chat function call")
				}
				ir.Blocks = append(ir.Blocks, Block{Kind: BlockCall, CallID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
			}
		}
	case provider.SurfaceResponses:
		var up responsesWireResponse
		if err := json.Unmarshal(body, &up); err != nil {
			return nil, invalidResponse(err.Error())
		}
		if !opts.Legacy && (up.ID == "" || top["output"] == nil || string(top["output"]) == "null" || (up.Status != "completed" && up.Status != "incomplete" && up.Status != "failed")) {
			return nil, invalidResponse("invalid responses envelope or nonterminal status")
		}
		ir.ID, ir.Created = up.ID, up.CreatedAt
		for _, it := range up.Output {
			switch it.Type {
			case "message":
				if !opts.Legacy && it.Role != "assistant" {
					return nil, invalidResponse("invalid responses message role")
				}
				for _, c := range it.Content {
					if c.Type == "output_text" && c.Text != "" {
						ir.Blocks = append(ir.Blocks, Block{Kind: BlockText, Text: c.Text})
					} else if c.Type != "output_text" && !opts.Legacy {
						return nil, invalidResponse("unsupported responses content " + c.Type)
					}
				}
			case "function_call":
				if !opts.Legacy && (it.CallID == "" || it.Name == "") {
					return nil, invalidResponse("invalid responses function call")
				}
				ir.Blocks = append(ir.Blocks, Block{Kind: BlockCall, CallID: it.CallID, Name: it.Name, Arguments: it.Arguments})
			case "reasoning": // observed reasoning output is deliberately not replayed
			default:
				if !opts.Legacy {
					return nil, invalidResponse("unsupported responses output " + it.Type)
				}
			}
		}
		ir.Finish = FinishCompleted
		for _, b := range ir.Blocks {
			if b.Kind == BlockCall {
				ir.Finish = FinishTools
			}
		}
		if up.Status == "incomplete" {
			ir.Finish = FinishLength
			if !opts.Legacy && up.Incomplete != nil && up.Incomplete.Reason == "content_filter" {
				ir.Finish = FinishRefusal
			}
		}
		if up.Status == "failed" && !opts.Legacy {
			ir.Finish = FinishFailed
			if ir.Error == nil {
				ir.Error = &ErrorIR{Type: "api_error", Message: "upstream response failed"}
			}
		}
	case provider.SurfaceAnthropic:
		var up struct {
			ID       string            `json:"id"`
			Type     string            `json:"type"`
			Role     string            `json:"role"`
			Stop     string            `json:"stop_reason"`
			Sequence *string           `json:"stop_sequence"`
			Content  []messagesInBlock `json:"content"`
		}
		if err := json.Unmarshal(body, &up); err != nil {
			return nil, invalidResponse(err.Error())
		}
		if up.ID == "" || up.Type != "message" || up.Role != "assistant" || up.Stop == "" || top["content"] == nil {
			return nil, invalidResponse("invalid messages envelope")
		}
		if !opts.Legacy && !validMessagesFinish(up.Stop) {
			return nil, invalidResponse("unknown messages stop_reason")
		}
		ir.ID = up.ID
		ir.Finish = messagesFinish(up.Stop)
		ir.StopSequence = up.Sequence
		for _, b := range up.Content {
			switch b.Type {
			case "text":
				ir.Blocks = append(ir.Blocks, Block{Kind: BlockText, Text: b.Text})
			case "tool_use":
				if b.ID == "" || b.Name == "" {
					return nil, invalidResponse("invalid messages tool_use")
				}
				if _, err := argumentsObject(string(b.Input), b.ID); err != nil {
					return nil, invalidResponse(err.Error())
				}
				ir.Blocks = append(ir.Blocks, Block{Kind: BlockCall, CallID: b.ID, Name: b.Name, Arguments: argumentsFromInput(b.Input, b.ID)})
			case "thinking":
				if b.Thinking != "" {
					ir.Blocks = append(ir.Blocks, Block{Kind: BlockReasoning, Text: b.Thinking})
				}
			default:
				// redacted_thinking, server-tool blocks: not output a client
				// of another wire can render or act on.
			}
		}
	default:
		return nil, invalidResponse("unknown response source " + string(surface))
	}
	ir.Usage, err = decodeUsage(top["usage"], surface, opts)
	return ir, err
}

func EncodeResponse(ir *ResponseIR, surface provider.Surface, model string, opts CodecOptions) ([]byte, error) {
	if ir == nil {
		return nil, invalidResponse("nil response IR")
	}
	if ir.Finish == FinishFailed && !opts.Legacy && surface != provider.SurfaceResponses {
		er := ir.Error
		if er == nil {
			er = &ErrorIR{Type: "api_error", Message: "upstream response failed"}
		}
		return EncodeError(*er, surface), nil
	}
	switch surface {
	case provider.SurfaceAnthropic:
		res := AnthropicResponse{ID: ir.ID, Type: "message", Role: "assistant", Content: []AnthropicBlock{}, Model: model, StopReason: streamMessagesFinish(ir.Finish), StopSequence: ir.StopSequence}
		if !strings.HasPrefix(res.ID, "msg_") {
			res.ID = "msg_" + res.ID
		}
		for _, b := range ir.Blocks {
			if b.Kind == BlockText {
				res.Content = append(res.Content, AnthropicBlock{Type: "text", Text: b.Text})
			}
			if b.Kind == BlockCall {
				input := parseToolArguments(b.Arguments)
				if !opts.Legacy {
					var err error
					input, err = argumentsObject(b.Arguments, b.CallID)
					if err != nil {
						return nil, invalidResponse(err.Error())
					}
				}
				res.Content = append(res.Content, AnthropicBlock{Type: "tool_use", ID: b.CallID, Name: b.Name, Input: input})
			}
		}
		// Legacy marshals res directly (no optionalMessagesUsage), so the
		// projection must live here: input exclusive of cache plus the split,
		// identical to the strict path's streamMessagesUsage. An upstream that
		// reports no split leaves the cache fields nil and omitempty keeps the
		// legacy bytes unchanged.
		res.Usage = projectAnthropicUsage(ir.Usage.Input, ir.Usage.Output, ir.Usage.CacheRead, ir.Usage.CacheWrite)
		if opts.Legacy {
			return json.Marshal(res)
		}
		// Retain legacy envelope field order; only usage presence differs.
		return json.Marshal(struct {
			ID       string           `json:"id"`
			Type     string           `json:"type"`
			Role     string           `json:"role"`
			Content  []AnthropicBlock `json:"content"`
			Model    string           `json:"model"`
			Stop     string           `json:"stop_reason"`
			Sequence *string          `json:"stop_sequence"`
			Usage    json.RawMessage  `json:"usage,omitempty"`
		}{res.ID, res.Type, res.Role, res.Content, res.Model, res.StopReason, res.StopSequence, optionalMessagesUsage(ir.Usage)})
	case provider.SurfaceChat:
		msg := chatWireOutMessage{Role: "assistant"}
		for _, b := range ir.Blocks {
			if b.Kind == BlockText {
				if msg.Content != "" {
					msg.Content += "\n"
				}
				msg.Content += b.Text
			}
			if b.Kind == BlockReasoning {
				msg.ReasoningContent += b.Text
			}
			if b.Kind == BlockCall {
				msg.ToolCalls = append(msg.ToolCalls, chatWireOutToolCall{ID: b.CallID, Type: "function", Function: chatWireOutFunction{Name: b.Name, Arguments: b.Arguments}})
			}
		}
		out := chatWireOutCompletion{ID: ir.ID, Object: "chat.completion", Created: ir.Created, Model: model, Choices: []chatWireOutChoice{{Index: 0, Message: msg, FinishReason: streamChatFinish(ir.Finish)}}}
		if ir.Usage.Input != nil {
			out.Usage.PromptTokens = *ir.Usage.Input
		}
		if ir.Usage.Output != nil {
			out.Usage.CompletionTokens = *ir.Usage.Output
		}
		if opts.Legacy {
			return json.Marshal(out)
		}
		// OpenAI convention (unchanged from the wire it replaces):
		// prompt_tokens INCLUDES cached, split in prompt_tokens_details. The
		// tap subtracts, so tokens_in stays non-cached like a native row.
		var usage map[string]any
		if ir.Usage.Input != nil || ir.Usage.Output != nil {
			usage = map[string]any{}
			if ir.Usage.Input != nil {
				usage["prompt_tokens"] = *ir.Usage.Input
				if ir.Usage.CacheRead != nil && *ir.Usage.CacheRead >= 0 {
					usage["prompt_tokens_details"] = map[string]int{"cached_tokens": *ir.Usage.CacheRead}
				}
			}
			if ir.Usage.Output != nil {
				usage["completion_tokens"] = *ir.Usage.Output
			}
			if ir.Usage.Input != nil && ir.Usage.Output != nil {
				usage["total_tokens"] = *ir.Usage.Input + *ir.Usage.Output
			}
		}
		return json.Marshal(struct {
			ID      string              `json:"id"`
			Object  string              `json:"object"`
			Created int64               `json:"created"`
			Model   string              `json:"model"`
			Choices []chatWireOutChoice `json:"choices"`
			Usage   map[string]any      `json:"usage,omitempty"`
		}{out.ID, out.Object, out.Created, out.Model, out.Choices, usage})
	case provider.SurfaceResponses:
		// Reuse the Responses event encoder's item/envelope schema. No framing;
		// this is the same complete output object its terminal event carries.
		e := eventEncoder{id: ir.ID, created: ir.Created, model: model, usage: ir.Usage, blocks: map[string]*encodedBlock{}}
		if e.id == "" {
			e.id = streamIdentity()
		}
		if !strings.HasPrefix(e.id, "resp_") {
			e.id = "resp_" + e.id
		}
		for i, b := range ir.Blocks {
			if b.Kind == BlockReasoning {
				continue // no Responses reasoning item is synthesized
			}
			key := fmt.Sprint(i)
			prefix := "msg_"
			if b.Kind == BlockCall {
				prefix = "fc_"
			}
			v := &encodedBlock{event: Event{Block: b}, index: i, id: fmt.Sprintf("%s%s_%d", prefix, e.id, i), closed: true}
			v.text.WriteString(b.Text)
			v.arguments.WriteString(b.Arguments)
			e.blocks[key] = v
			e.order = append(e.order, key)
		}
		status := "completed"
		if ir.Finish == FinishFailed {
			status = "failed"
		}
		if ir.Finish == FinishLength || ir.Finish == FinishRefusal {
			status = "incomplete"
		}
		obj := e.responseObject(status, ir.Error)
		if status == "incomplete" {
			reason := "max_output_tokens"
			if ir.Finish == FinishRefusal {
				reason = "content_filter"
			}
			obj["incomplete_details"] = map[string]any{"reason": reason}
		}
		return json.Marshal(obj)
	}
	return nil, invalidResponse("unknown response destination " + string(surface))
}
func optionalMessagesUsage(u UsageIR) json.RawMessage {
	if u.Input == nil && u.Output == nil {
		return nil
	}
	return streamMessagesUsage(u)
}
func EncodeError(e ErrorIR, surface provider.Surface) []byte {
	if e.Type == "" {
		e.Type = "api_error"
	}
	if e.Message == "" {
		e.Message = "upstream error"
	}
	if surface == provider.SurfaceAnthropic {
		b, _ := json.Marshal(anthropicErrorEnvelope{Type: "error", Error: anthropicErrorEnvelope_{Type: e.Type, Message: e.Message}})
		return b
	}
	b, _ := json.Marshal(struct {
		Error responsesWireError `json:"error"`
	}{responsesWireError{Message: e.Message, Type: e.Type, Code: e.Code}})
	return b
}
