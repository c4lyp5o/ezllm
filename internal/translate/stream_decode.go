package translate

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// Each source wire has one event decoder. No decoder knows the destination.
// State is per-reader; credentials, routes and source bodies never live here.
type eventDecoder struct {
	surface          provider.Surface
	opts             CodecOptions
	frames           *frameScanner
	started          bool
	terminal         bool
	finish           FinishKind
	stopSequence     *string
	usage            UsageIR
	rawMessagesUsage UsageIR
	blocks           map[string]Event
	order            []string
	items            map[string]string // upstream item_id OR call_id -> stable block key
	calls            bool
	id               string
	created          int64
	traces           map[string]*decodedTrace
	traceBytes       int
}

func (d *eventDecoder) start(id string, created int64) []Event {
	if d.started {
		return nil
	}
	d.started = true
	d.id = id
	d.created = created
	return []Event{{Kind: EventStart, ID: id, Created: created}}
}
func (d *eventDecoder) open(key string, block Block, item string, idx, content int) []Event {
	if _, ok := d.blocks[key]; ok {
		return nil
	}
	e := Event{Kind: EventBlockStart, Key: key, Block: block, ItemID: item, SourceIndex: idx, ContentIndex: content}
	d.blocks[key] = e
	d.order = append(d.order, key)
	if block.Kind == BlockCall {
		d.calls = true
	}
	return []Event{e}
}
func (d *eventDecoder) end(key string) []Event {
	if _, ok := d.blocks[key]; !ok {
		return nil
	}
	delete(d.blocks, key)
	return []Event{{Kind: EventBlockEnd, Key: key}}
}
func (d *eventDecoder) closeAll() []Event {
	var out []Event
	for _, key := range d.order {
		out = append(out, d.end(key)...)
	}
	return out
}
func (d *eventDecoder) finishEvents(kind FinishKind) []Event {
	d.terminal = true
	out := d.closeAll()
	return append(out, Event{Kind: EventFinish, Finish: kind, Usage: d.usage, StopSequence: d.stopSequence})
}

// streamTruncation is a source that ended without a usable terminal event.
// Legacy mode reproduces each historic reader's exact end-of-stream frames;
// strict mode fails visibly instead of inventing a completed answer.
type streamTruncation struct {
	Transport bool
	Reason    string
}

func (e *streamTruncation) Error() string { return "translate: " + e.Reason }

// sourceEnd applies the historic per-reader end-of-stream policy.
func (d *eventDecoder) sourceEnd(transport bool, cause error) ([]Event, error) {
	if d.opts.Legacy && d.surface == provider.SurfaceOpenAI && d.started {
		// Historic Chat reader closed the message as a completed answer on
		// ANY source end (transport cuts included). Preserve exactly.
		if d.finish != "" {
			return d.finishEvents(d.finish), nil
		}
		return d.finishEvents(FinishCompleted), nil
	}
	if d.opts.Legacy && d.surface == provider.SurfaceOpenAI {
		return nil, &streamTruncation{Reason: "upstream stream ended before any event"}
	}
	if transport {
		if d.opts.Legacy {
			return nil, &streamTruncation{Transport: true, Reason: "upstream stream failed"}
		}
		return nil, &streamTruncation{Transport: true, Reason: "upstream stream failed: " + cause.Error()}
	}
	return nil, &streamTruncation{Reason: "upstream stream ended without a terminal event"}
}

func (d *eventDecoder) Next() ([]Event, error) {
	if d.terminal {
		return nil, io.EOF
	}
	name, b, err := d.frames.Next()
	if err != nil {
		if err == io.EOF && d.finish != "" {
			return d.finishEvents(d.finish), nil
		}
		return d.sourceEnd(err != io.EOF, err)
	}
	if len(b) == 0 {
		return nil, nil
	}
	if string(b) == "[DONE]" {
		if d.finish != "" {
			return d.finishEvents(d.finish), nil
		}
		return d.sourceEnd(false, nil)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		if d.opts.Legacy {
			return nil, nil
		}
		return nil, invalidResponse("malformed SSE JSON: " + err.Error())
	}
	if raw := top["error"]; len(raw) > 0 && string(raw) != "null" {
		if d.opts.Legacy && d.surface == provider.SurfaceResponses {
			// Historic Responses readers skipped bare error frames; the
			// terminal event (or the truncation path) carried the news.
			return nil, nil
		}
		var e ErrorIR
		if err := json.Unmarshal(raw, &e); err != nil {
			if d.opts.Legacy {
				return nil, nil // historic readers never killed the stream here
			}
			return nil, invalidResponse("invalid SSE error envelope")
		}
		if d.opts.Legacy {
			// Historic chat error frames always named api_error and passed
			// the message through unchanged (even empty).
			e.Type = "api_error"
		} else {
			if e.Message == "" {
				e.Message = "upstream stream failed"
			}
			if e.Type == "" {
				e.Type = "api_error"
			}
		}
		d.terminal = true
		return []Event{{Kind: EventError, Error: &e, Usage: d.usage}}, nil
	}
	if !d.opts.Legacy && d.surface != provider.SurfaceOpenAI && name != "" {
		var typ string
		if raw := top["type"]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &typ); err != nil || typ != name {
				return nil, invalidResponse("SSE event name/type mismatch")
			}
		}
	}
	switch d.surface {
	case provider.SurfaceOpenAI:
		return d.chat(b)
	case provider.SurfaceAnthropic:
		return d.messages(name, b, top)
	case provider.SurfaceResponses:
		events, err := d.responses(name, b, top)
		if err != nil {
			return nil, err
		}
		if !d.opts.Legacy {
			if err := d.captureResponseEvents(events); err != nil {
				return nil, err
			}
		}
		return events, nil
	default:
		return nil, invalidResponse("unknown source wire " + string(d.surface))
	}
}
func (d *eventDecoder) usageEvents(raw json.RawMessage) ([]Event, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	opts := d.opts
	if d.surface == provider.SurfaceAnthropic {
		opts.Legacy = true
	}
	u, err := decodeUsage(raw, d.surface, opts)
	if err != nil {
		return nil, err
	}
	if d.surface == provider.SurfaceAnthropic && !d.opts.Legacy {
		d.rawMessagesUsage.Merge(u)
		u, err = NormalizeMessagesUsage(d.rawMessagesUsage, d.opts.MessagesUsage)
		if err != nil {
			return nil, err
		}
	}
	d.usage.Merge(u)
	return []Event{{Kind: EventUsage, Usage: d.usage}}, nil
}
func chatFinish(v string) FinishKind {
	switch v {
	case "length":
		return FinishLength
	case "tool_calls", "function_call":
		return FinishTools
	case "content_filter":
		return FinishRefusal
	default:
		return FinishCompleted
	}
}
func messagesFinish(v string) FinishKind {
	switch v {
	case "max_tokens":
		return FinishLength
	case "tool_use":
		return FinishTools
	case "refusal":
		return FinishRefusal
	case "stop_sequence":
		return FinishStopSequence
	default:
		return FinishCompleted
	}
}
func (d *eventDecoder) chat(b []byte) ([]Event, error) {
	var c struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Role      string            `json:"role"`
				Content   string            `json:"content"`
				Refusal   string            `json:"refusal"`
				ToolCalls []openaiToolDelta `json:"tool_calls"`
			} `json:"delta"`
			Finish *string `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		if d.opts.Legacy {
			return nil, nil // undecodable chunk: historic reader skipped it
		}
		return nil, invalidResponse(err.Error())
	}
	if len(c.Choices) > 1 && !d.opts.Legacy {
		return nil, invalidResponse("multiple chat choices are not portable")
	}
	var out []Event
	if len(c.Choices) > 0 {
		ch := c.Choices[0]
		if ch.Index != 0 && !d.opts.Legacy {
			return nil, invalidResponse("nonzero chat choice index")
		}
		out = append(out, d.start(c.ID, c.Created)...)
		delta := ch.Delta
		if !d.opts.Legacy && d.finish != "" && (delta.Content != "" || delta.Refusal != "" || len(delta.ToolCalls) > 0) {
			return nil, invalidResponse("content arrived after chat finish_reason")
		}
		if delta.Content != "" || delta.Refusal != "" {
			key := "chat:text"
			out = append(out, d.open(key, Block{Kind: BlockText}, "", 0, 0)...)
			text := delta.Content
			if delta.Refusal != "" {
				text = delta.Refusal
				d.finish = FinishRefusal
			}
			out = append(out, Event{Kind: EventTextDelta, Key: key, Delta: text})
		}
		for _, tc := range delta.ToolCalls {
			key := fmt.Sprintf("chat:call:%d", tc.Index)
			if tc.Function == nil {
				if _, ok := d.blocks[key]; !ok {
					return nil, invalidResponse("tool call opened without function metadata")
				}
				continue
			}
			if _, ok := d.blocks[key]; !ok {
				if !d.opts.Legacy && (tc.ID == "" || tc.Function.Name == "") {
					return nil, invalidResponse("tool call missing id or name")
				}
				out = append(out, d.open(key, Block{Kind: BlockCall, CallID: tc.ID, Name: tc.Function.Name}, "", tc.Index, 0)...)
			}
			if tc.Function.Arguments != "" {
				out = append(out, Event{Kind: EventArgumentsDelta, Key: key, Delta: tc.Function.Arguments})
			}
		}
		if ch.Finish != nil && *ch.Finish != "" {
			if !d.opts.Legacy && !validChatFinish(*ch.Finish) {
				return nil, invalidResponse("unknown chat finish_reason")
			}
			d.finish = chatFinish(*ch.Finish)
		}
	}
	u, err := d.usageEvents(c.Usage)
	if err != nil {
		return nil, err
	}
	out = append(out, u...)
	// Legacy messages synthesis closes at finish+usage rather than waiting for
	// [DONE]. Preserve this policy; strict mode waits so late errors remain visible.
	if d.opts.Legacy && d.surface == provider.SurfaceOpenAI && d.finish != "" && (d.usage.Input != nil || d.usage.Output != nil) {
		out = append(out, d.finishEvents(d.finish)...)
	}
	return out, nil
}

func (d *eventDecoder) messages(name string, b []byte, top map[string]json.RawMessage) ([]Event, error) {
	var e struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			ID    string          `json:"id"`
			Usage json.RawMessage `json:"usage"`
		} `json:"message"`
		Block struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content_block"`
		Delta struct {
			Type     string  `json:"type"`
			Text     string  `json:"text"`
			Partial  string  `json:"partial_json"`
			Stop     string  `json:"stop_reason"`
			Sequence *string `json:"stop_sequence"`
		} `json:"delta"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, invalidResponse(err.Error())
	}
	if name == "" {
		name = e.Type
	}
	key := fmt.Sprintf("messages:%d", e.Index)
	switch name {
	case "ping":
		return nil, nil
	case "message_start":
		out := d.start(e.Message.ID, 0)
		u, err := d.usageEvents(e.Message.Usage)
		return append(out, u...), err
	case "content_block_start":
		switch e.Block.Type {
		case "text":
			out := d.open(key, Block{Kind: BlockText}, "", e.Index, 0)
			if e.Block.Text != "" {
				out = append(out, Event{Kind: EventTextDelta, Key: key, Delta: e.Block.Text})
			}
			return out, nil
		case "tool_use":
			out := d.open(key, Block{Kind: BlockCall, CallID: e.Block.ID, Name: e.Block.Name}, "", e.Index, 0)
			if len(e.Block.Input) > 0 && string(e.Block.Input) != "{}" {
				out = append(out, Event{Kind: EventArgumentsDelta, Key: key, Delta: string(e.Block.Input)})
			}
			return out, nil
		case "thinking", "redacted_thinking":
			return nil, nil // not portable output; never replayed
		default:
			return nil, invalidResponse("unsupported messages content block " + e.Block.Type)
		}
	case "content_block_delta":
		if _, ok := d.blocks[key]; !ok {
			return nil, invalidResponse("delta for unopened/stopped messages block")
		}
		switch e.Delta.Type {
		case "text_delta":
			return []Event{{Kind: EventTextDelta, Key: key, Delta: e.Delta.Text}}, nil
		case "input_json_delta":
			return []Event{{Kind: EventArgumentsDelta, Key: key, Delta: e.Delta.Partial}}, nil
		case "thinking_delta", "signature_delta":
			return nil, nil
		default:
			return nil, invalidResponse("unsupported messages delta " + e.Delta.Type)
		}
	case "content_block_stop":
		return d.end(key), nil
	case "message_delta":
		if e.Delta.Stop != "" {
			if !d.opts.Legacy && !validMessagesFinish(e.Delta.Stop) {
				return nil, invalidResponse("unknown messages stop_reason")
			}
			d.finish = messagesFinish(e.Delta.Stop)
			d.stopSequence = e.Delta.Sequence
		}
		return d.usageEvents(e.Usage)
	case "message_stop":
		if d.finish == "" {
			return nil, invalidResponse("message_stop without stop_reason")
		}
		return d.finishEvents(d.finish), nil
	}
	_ = top
	return nil, nil
}

func validChatFinish(s string) bool {
	switch s {
	case "stop", "length", "tool_calls", "function_call", "content_filter":
		return true
	}
	return false
}
func validMessagesFinish(s string) bool {
	switch s {
	case "end_turn", "max_tokens", "tool_use", "refusal", "stop_sequence":
		return true
	}
	return false
}

type decodedTrace struct {
	block Block
	text  strings.Builder
	args  strings.Builder
}

// Responses terminals carry an authoritative assembled output. Trace only
// this source to detect contradictions and backfill terminal-only output;
// Chat/Messages sources do not acquire whole-output decoder accumulation.
func (d *eventDecoder) captureResponseEvents(events []Event) error {
	if d.traces == nil {
		d.traces = map[string]*decodedTrace{}
	}
	for _, e := range events {
		switch e.Kind {
		case EventBlockStart:
			d.traceBytes += 128 + len(e.Key) + len(e.Block.CallID) + len(e.Block.Name)
			if _, ok := d.traces[e.Key]; !ok {
				d.traces[e.Key] = &decodedTrace{block: e.Block}
			}
		case EventTextDelta, EventArgumentsDelta:
			d.traceBytes += len(e.Delta)
			v := d.traces[e.Key]
			if v == nil {
				return invalidResponse("delta for untraced response block")
			}
			if e.Kind == EventTextDelta {
				v.text.WriteString(e.Delta)
			} else {
				v.args.WriteString(e.Delta)
			}
		}
		if d.traceBytes > maxCodecOutput {
			return invalidResponse("translated output limit exceeded")
		}
	}
	return nil
}
func (d *eventDecoder) responseSnapshot(raw json.RawMessage, status string) ([]Event, error) {
	// Real terminal envelopes are complete Responses objects, not arbitrary {}.
	ir, err := DecodeResponse(raw, provider.SurfaceResponses, d.opts)
	if err != nil {
		return nil, err
	}
	var top map[string]json.RawMessage
	_ = json.Unmarshal(raw, &top)
	var got string
	_ = json.Unmarshal(top["status"], &got)
	if got != status {
		return nil, invalidResponse("Responses terminal event/status mismatch")
	}
	if d.id != "" && ir.ID != d.id {
		return nil, invalidResponse("Responses terminal identity changed")
	}
	seen := 0
	for _, key := range d.order {
		v := d.traces[key]
		if v == nil {
			continue
		}
		block := v.block
		block.Text = v.text.String()
		block.Arguments = v.args.String()
		if seen >= len(ir.Blocks) || block != ir.Blocks[seen] {
			return nil, invalidResponse("Responses terminal output contradicts delivered deltas")
		}
		seen++
	}
	var out []Event
	for i := seen; i < len(ir.Blocks); i++ {
		b := ir.Blocks[i]
		key := fmt.Sprintf("responses:terminal:%d", i)
		out = append(out, d.open(key, Block{Kind: b.Kind, CallID: b.CallID, Name: b.Name}, "", i, 0)...)
		if b.Kind == BlockText {
			out = append(out, Event{Kind: EventTextDelta, Key: key, Delta: b.Text})
		} else {
			out = append(out, Event{Kind: EventArgumentsDelta, Key: key, Delta: b.Arguments})
		}
	}
	return out, nil
}

// responsesEvent is the decode shape for every Responses wire frame.
type responsesEvent struct {
	Type         string `json:"type"`
	ItemID       string `json:"item_id"`
	CallID       string `json:"call_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Delta        string `json:"delta"`
	Item         struct {
		ID        string `json:"id"`
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"item"`
	Part struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"part"`
	Response struct {
		ID         string               `json:"id"`
		Created    int64                `json:"created_at"`
		Status     string               `json:"status"`
		Usage      json.RawMessage      `json:"usage"`
		Error      *ErrorIR             `json:"error"`
		Incomplete *responsesIncomplete `json:"incomplete_details"`
	} `json:"response"`
}

func responsesTerminal(name string) bool {
	switch name {
	case "response.completed", "response.incomplete", "response.failed", "error":
		return true
	}
	return false
}

func (d *eventDecoder) responses(name string, b []byte, top map[string]json.RawMessage) ([]Event, error) {
	var e responsesEvent
	if err := json.Unmarshal(b, &e); err != nil {
		if !d.opts.Legacy {
			return nil, invalidResponse(err.Error())
		}
		// Historic Responses readers ignored unmarshal failures: ordinary
		// frames were skipped, terminal frames fell through to their
		// zero-value failed/completed handling.
		if !responsesTerminal(name) {
			return nil, nil
		}
		e = responsesEvent{}
	}
	if name == "" && !d.opts.Legacy {
		// Strict decoders read the wire event name from the payload; the
		// historic readers dispatched on the SSE event line alone.
		name = e.Type
	}
	switch name {
	case "response.created":
		return d.start(e.Response.ID, e.Response.Created), nil
	case "response.output_item.added":
		if e.Item.Type != "function_call" {
			return nil, nil
		}
		out := d.start(e.Response.ID, e.Response.Created)
		key := "responses:call:" + e.Item.ID
		if e.Item.ID == "" {
			key = "responses:call:" + e.Item.CallID
		}
		if e.Item.ID == "" && e.Item.CallID == "" {
			return nil, invalidResponse("function_call missing identities")
		}
		d.items[e.Item.ID] = key
		d.items[e.Item.CallID] = key
		out = append(out, d.open(key, Block{Kind: BlockCall, CallID: e.Item.CallID, Name: e.Item.Name}, e.Item.ID, e.OutputIndex, 0)...)
		if e.Item.Arguments != "" {
			out = append(out, Event{Kind: EventArgumentsDelta, Key: key, Delta: e.Item.Arguments})
		}
		return out, nil
	case "response.content_part.added":
		if d.opts.Legacy || e.Part.Type != "output_text" {
			return nil, nil
		}
		key := fmt.Sprintf("responses:text:%s:%d:%d", e.ItemID, e.OutputIndex, e.ContentIndex)
		return d.open(key, Block{Kind: BlockText}, e.ItemID, e.OutputIndex, e.ContentIndex), nil
	case "response.output_text.delta", "response.refusal.delta":
		key := fmt.Sprintf("responses:text:%s:%d:%d", e.ItemID, e.OutputIndex, e.ContentIndex)
		if d.opts.Legacy {
			key = "responses:text"
		}
		out := d.start(e.Response.ID, e.Response.Created)
		out = append(out, d.open(key, Block{Kind: BlockText}, e.ItemID, e.OutputIndex, e.ContentIndex)...)
		out = append(out, Event{Kind: EventTextDelta, Key: key, Delta: e.Delta})
		if name == "response.refusal.delta" {
			d.finish = FinishRefusal
		}
		return out, nil
	case "response.function_call_arguments.delta":
		key, ok := d.items[e.ItemID]
		if !ok && e.CallID != "" {
			key, ok = d.items[e.CallID]
		}
		if !ok {
			if d.opts.Legacy {
				return nil, nil
			}
			return nil, invalidResponse("argument delta for unknown item_id")
		}
		if _, open := d.blocks[key]; !open {
			return nil, invalidResponse("argument delta for stopped block")
		}
		return []Event{{Kind: EventArgumentsDelta, Key: key, Delta: e.Delta}}, nil
	case "response.content_part.done":
		if d.opts.Legacy {
			return nil, nil
		}
		return d.end(fmt.Sprintf("responses:text:%s:%d:%d", e.ItemID, e.OutputIndex, e.ContentIndex)), nil
	case "response.output_item.done":
		if d.opts.Legacy {
			return nil, nil
		}
		key := d.items[e.Item.ID]
		if key == "" {
			key = d.items[e.Item.CallID]
		}
		return d.end(key), nil
	case "response.completed", "response.incomplete", "response.failed":
		var backfill []Event
		if !d.opts.Legacy {
			var err error
			backfill, err = d.responseSnapshot(top["response"], strings.TrimPrefix(name, "response."))
			if err != nil {
				return nil, err
			}
		}
		var out []Event
		// Historic failed frames never synthesized a start; the messages leg
		// starts inside its own error path instead, the chat leg not at all.
		if !(d.opts.Legacy && name == "response.failed") {
			out = append(out, d.start(e.Response.ID, e.Response.Created)...)
		}
		out = append(out, backfill...)
		u, err := d.usageEvents(e.Response.Usage)
		if err != nil {
			return nil, err
		}
		out = append(out, u...)
		if name == "response.failed" {
			d.terminal = true
			return append(out, Event{Kind: EventError, Error: e.Response.Error, Usage: d.usage}), nil
		}
		finish := FinishCompleted
		if d.finish == FinishRefusal {
			finish = FinishRefusal
		} else if d.calls {
			finish = FinishTools
		}
		if name == "response.incomplete" {
			finish = FinishLength
			if e.Response.Incomplete != nil && e.Response.Incomplete.Reason == "content_filter" {
				finish = FinishRefusal
			}
		}
		out = append(out, d.finishEvents(finish)...)
		return out, nil
	case "error":
		if d.opts.Legacy {
			return nil, nil // historic Responses readers had no bare error case
		}
		var er struct {
			Code    any    `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &er)
		d.terminal = true
		return []Event{{Kind: EventError, Error: &ErrorIR{Type: "api_error", Message: er.Message, Code: er.Code}}}, nil
	}
	_ = top
	if strings.HasPrefix(name, "response.") {
		return nil, nil
	}
	return nil, nil
}
