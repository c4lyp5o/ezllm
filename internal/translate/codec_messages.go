package translate

// The anthropic messages surface. Decoding lifts `system` into IR turns and
// splits a user turn's blocks into text runs and tool-result turns IN SOURCE
// ORDER (a tool_result must not jump ahead of the text that preceded it).
// Encoding is the inverse: system turns become the top-level system array, and
// CONSECUTIVE tool turns become ONE user message with several tool_result
// blocks — which is the shape the API expects after a multi-tool assistant
// turn.

import (
	"crypto/sha256"
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
	Thinking  string          `json:"thinking"`
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
			ir.Tools = append(ir.Tools, Tool{Name: t.Name, Description: t.Description, Schema: t.InputSchema, Strict: t.Strict})
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
	Thinking      *messagesThinking    `json:"thinking,omitempty"`
}

type messagesThinking struct {
	Type    string `json:"type"`
	Display string `json:"display,omitempty"`
}

type messagesOutMessage struct {
	Role    string             `json:"role"`
	Content []messagesOutBlock `json:"content"`
}

type messagesOutBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        any             `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      *string         `json:"content,omitempty"`
	Source       any             `json:"source,omitempty"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

// defaultMessagesMaxTokens is the output limit sent when the client named
// none. Chat and Responses clients may omit it ("the model's maximum"); the
// Messages wire requires one. 32000 sits within every current Claude model's
// output cap and leaves room for the thinking current models always do.
const defaultMessagesMaxTokens = 32000

// maxCacheBreakpoints is the Messages wire's per-request breakpoint limit.
const maxCacheBreakpoints = 4

// ephemeralCache is the breakpoint the encoder places itself.
var ephemeralCache = json.RawMessage(`{"type":"ephemeral"}`)

// emptyObjectSchema is the input_schema of a tool that declared none: the
// Messages wire requires an object schema on every tool.
var emptyObjectSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// messagesToolID makes a tool-call id legal on the Messages wire
// (^[a-zA-Z0-9_-]+$). Ids minted by other providers (functions.read:0) are
// rewritten byte by byte to form a base spelling. messagesToolIDs resolves
// collisions across the request before encoding tool uses and their results.
func messagesToolID(id string) string {
	ok := id != ""
	for i := 0; i < len(id) && ok; i++ {
		ok = isToolIDByte(id[i])
	}
	if ok {
		return id
	}
	if id == "" {
		return "call"
	}
	b := []byte(id)
	for i, c := range b {
		if !isToolIDByte(c) {
			b[i] = '_'
		}
	}
	return string(b)
}

// Reserve every original's sanitized spelling before assigning suffixes, so a
// generated identity cannot steal a legal identity elsewhere in the history.
func messagesToolIDs(ir *RequestIR) map[string]string {
	ids := []string{}
	seen := map[string]bool{}
	counts := map[string]int{}
	used := map[string]bool{}
	for _, turn := range ir.Turns {
		for _, b := range turn.Blocks {
			if (b.Kind != BlockCall && b.Kind != BlockResult) || seen[b.CallID] {
				continue
			}
			seen[b.CallID] = true
			ids = append(ids, b.CallID)
			base := messagesToolID(b.CallID)
			counts[base]++
			used[base] = true
		}
	}
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		base := messagesToolID(id)
		if counts[base] == 1 || id == base {
			out[id] = base
			continue
		}
		sum := sha256.Sum256([]byte(id))
		candidate := fmt.Sprintf("%s_%x", base, sum[:8])
		for n := 1; used[candidate]; n++ {
			candidate = fmt.Sprintf("%s_%x_%d", base, sum[:8], n)
		}
		used[candidate] = true
		out[id] = candidate
	}
	return out
}

func isToolIDByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

func cacheOf(b Block) json.RawMessage {
	if b.CacheControl == "" {
		return nil
	}
	return json.RawMessage(b.CacheControl)
}

// EncodeMessagesRequest renders the IR as an anthropic /v1/messages body.
//
// What a chat or Responses client may leave out, it fills in: an output
// limit, an object schema for a tool without one. What the Messages wire
// rejects, it reshapes deterministically: tool ids outside the id alphabet,
// empty turns (dropped), consecutive same-role turns (merged). And it carries
// prompt caching across: the client's own breakpoints when it marked any
// (at most four), otherwise the standard two — end of system, end of the
// newest message — so a translated conversation caches like a native one.
//
// Then it follows the destination model (claude_models.go): sampling is
// dropped where the model rejects it, a forced tool choice becomes auto plus
// an instruction where forcing is rejected, and a model that thinks by
// default is asked to summarize its thinking so a chat client can show it.
func EncodeMessagesRequest(ir *RequestIR, upstreamModel string) ([]byte, error) {
	toolIDs := messagesToolIDs(ir)
	maxTokens := defaultMessagesMaxTokens
	if ir.MaxOutputTokens != nil {
		maxTokens = *ir.MaxOutputTokens
	}
	out := messagesOutRequest{
		Model:         upstreamModel,
		MaxTokens:     maxTokens,
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
			blk := messagesOutBlock{Type: "text", Text: text}
			for _, b := range t.Blocks {
				if c := cacheOf(b); c != nil {
					blk.CacheControl = c
				}
			}
			out.System = append(out.System, blk)
		}
	}

	// add appends a turn's blocks, merging into the previous message when the
	// role repeats and dropping a turn with nothing to say.
	add := func(role string, blocks []messagesOutBlock) {
		if len(blocks) == 0 {
			return
		}
		if n := len(out.Messages); n > 0 && out.Messages[n-1].Role == role {
			out.Messages[n-1].Content = append(out.Messages[n-1].Content, blocks...)
			return
		}
		out.Messages = append(out.Messages, messagesOutMessage{Role: role, Content: blocks})
	}

	for _, t := range ir.Turns {
		var blocks []messagesOutBlock
		role := t.Role
		switch t.Role {
		case RoleSystem, RoleDeveloper:
			continue // lifted above

		case RoleTool:
			// Tool results ride a user message; consecutive ones merge into
			// one, the shape the API expects after a multi-call turn.
			role = RoleUser
			for _, b := range t.Blocks {
				if b.Kind != BlockResult {
					continue
				}
				text := b.Text
				blocks = append(blocks, messagesOutBlock{
					Type: "tool_result", ToolUseID: toolIDs[b.CallID], Content: &text, CacheControl: cacheOf(b),
				})
			}

		case RoleAssistant:
			for _, b := range t.Blocks {
				switch b.Kind {
				case BlockText:
					if b.Text != "" {
						blocks = append(blocks, messagesOutBlock{Type: "text", Text: b.Text, CacheControl: cacheOf(b)})
					}
				case BlockCall:
					input, err := argumentsObject(b.Arguments, b.CallID)
					if err != nil {
						return nil, err
					}
					blocks = append(blocks, messagesOutBlock{
						Type: "tool_use", ID: toolIDs[b.CallID], Name: b.Name, Input: input, CacheControl: cacheOf(b),
					})
				case BlockImage:
					blocks = append(blocks, messagesOutBlock{
						Type: "image", Source: anthropicImageSourceFromURL(b.ImageURL), CacheControl: cacheOf(b),
					})
				}
			}

		default:
			for _, b := range t.Blocks {
				if b.Kind == BlockText && b.Text != "" {
					blocks = append(blocks, messagesOutBlock{Type: "text", Text: b.Text, CacheControl: cacheOf(b)})
				}
				if b.Kind == BlockImage {
					blocks = append(blocks, messagesOutBlock{
						Type: "image", Source: anthropicImageSourceFromURL(b.ImageURL), CacheControl: cacheOf(b),
					})
				}
			}
		}
		add(role, blocks)
	}
	placeCacheBreakpoints(&out)

	for _, t := range ir.Tools {
		schema := t.Schema
		if len(schema) == 0 || string(schema) == "null" {
			schema = emptyObjectSchema
		}
		out.Tools = append(out.Tools, AnthropicTool{
			Name: t.Name, Description: t.Description, InputSchema: schema, Strict: t.Strict,
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
	// parallel_tool_calls=false is the Messages wire's
	// disable_parallel_tool_use, which lives on the tool choice.
	if ir.ParallelToolCalls != nil && !*ir.ParallelToolCalls && len(out.Tools) > 0 {
		if out.ToolChoice == nil {
			out.ToolChoice = &AnthropicToolChoice{Type: "auto"}
		}
		if out.ToolChoice.Type != ToolChoiceNone {
			out.ToolChoice.DisableParallelToolUse = true
		}
	}
	if cm, ok := parseClaudeModel(upstreamModel); ok {
		if cm.rejectsSampling() {
			out.Temperature, out.TopP = nil, nil
		}
		if cm.rejectsForcedTools() {
			unforceToolChoice(&out)
		}
		if cm.thinksByDefault() && (out.ToolChoice == nil || out.ToolChoice.Type == "auto" || out.ToolChoice.Type == ToolChoiceNone) {
			out.Thinking = &messagesThinking{Type: "adaptive", Display: "summarized"}
		}
	}
	return json.Marshal(out)
}

// unforceToolChoice turns a forced choice (any / a named tool) into auto plus
// an instruction appended after everything else — after the cache
// breakpoints too, so the cached prefix is the same as an unforced request's.
func unforceToolChoice(out *messagesOutRequest) {
	tc := out.ToolChoice
	if tc == nil || tc.Type != "any" && tc.Type != "tool" {
		return
	}
	instr := "Respond by calling one of the available tools."
	if tc.Type == "tool" {
		instr = "Respond by calling the " + tc.Name + " tool."
	}
	tc.Type, tc.Name = "auto", ""
	blk := messagesOutBlock{Type: "text", Text: instr}
	if n := len(out.Messages); n > 0 && out.Messages[n-1].Role == RoleUser {
		out.Messages[n-1].Content = append(out.Messages[n-1].Content, blk)
		return
	}
	out.Messages = append(out.Messages, messagesOutMessage{Role: RoleUser, Content: []messagesOutBlock{blk}})
}

// placeCacheBreakpoints keeps the request within the wire's breakpoint limit
// and supplies the standard breakpoints when the client marked none.
func placeCacheBreakpoints(out *messagesOutRequest) {
	var marked []*messagesOutBlock
	for i := range out.System {
		if out.System[i].CacheControl != nil {
			marked = append(marked, &out.System[i])
		}
	}
	for i := range out.Messages {
		for j := range out.Messages[i].Content {
			if out.Messages[i].Content[j].CacheControl != nil {
				marked = append(marked, &out.Messages[i].Content[j])
			}
		}
	}
	if len(marked) == 0 {
		if n := len(out.System); n > 0 {
			out.System[n-1].CacheControl = ephemeralCache
		}
		if n := len(out.Messages); n > 0 {
			c := out.Messages[n-1].Content
			c[len(c)-1].CacheControl = ephemeralCache
		}
		return
	}
	// Over the limit: the first breakpoint guards the stable prefix, the
	// newest ones the growing conversation; the middle ones go.
	for k := 1; k < len(marked)-(maxCacheBreakpoints-1); k++ {
		marked[k].CacheControl = nil
	}
}
