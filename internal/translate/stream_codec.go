package translate

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// StreamReader is a pull-based codec: no worker, channel or whole-source
// prebuffer. Read advances one bounded source frame only when pending client
// bytes are drained. Close delegates to the real upstream body exactly once.
// Outcome is read after copying (M5 wires it to ledger/MarkHopGood).
type StreamReader struct {
	decoder     *eventDecoder
	encoder     *eventEncoder
	out         bytes.Buffer
	done        bool
	outcome     FinishKind
	maxOutput   int
	outputBytes int
	closer      io.Closer
	closeOnce   sync.Once
	closeErr    error
}

func NewStreamReader(r io.Reader, source, target provider.Surface, clientModel string, opts CodecOptions) *StreamReader {
	d := &eventDecoder{surface: source, opts: opts, frames: newFrameScanner(r), blocks: map[string]Event{}, items: map[string]string{}}
	e := &eventEncoder{source: source, surface: target, opts: opts, model: clientModel, blocks: map[string]*encodedBlock{}, current: ""}
	s := &StreamReader{decoder: d, encoder: e, maxOutput: maxCodecOutput}
	if c, ok := r.(io.Closer); ok {
		s.closer = c
	}
	if opts.Legacy && source == provider.SurfaceOpenAI && target == provider.SurfaceAnthropic {
		s.out.WriteString("event: ping\ndata: {\"type\":\"ping\"}\n\n")
		e.ping = true
	}
	return s
}
func (s *StreamReader) Outcome() FinishKind { return s.outcome }
func (s *StreamReader) Close() error {
	s.closeOnce.Do(func() {
		if s.closer != nil {
			s.closeErr = s.closer.Close()
		}
	})
	return s.closeErr
}
func (s *StreamReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for s.out.Len() == 0 && !s.done {
		events, err := s.decoder.Next()
		if err != nil {
			if err == io.EOF && s.outcome != "" {
				s.done = true
				break
			}
			s.end(err)
			break
		}
		for _, e := range events {
			if e.Kind == EventStart {
				s.outputBytes += len(e.ID)
			}
			if e.Kind == EventBlockStart {
				// Charge metadata and state per block, even if a source sends no text.
				s.outputBytes += 128 + len(e.Key) + len(e.ItemID) + len(e.Block.CallID) + len(e.Block.Name)
			}
			if e.Kind == EventTextDelta || e.Kind == EventArgumentsDelta {
				s.outputBytes += len(e.Delta)
				if s.outputBytes > s.maxOutput {
					s.fail(invalidResponse("translated output limit exceeded"))
					break
				}
			}
			if s.outputBytes > s.maxOutput {
				s.fail(invalidResponse("translated output limit exceeded"))
				break
			}
			if e.Kind == EventFinish || e.Kind == EventError {
				// Set done BEFORE terminal frames enter the queue: repeated Read must
				// never append a second close. The queue still drains in full.
				s.done = true
				s.outcome = e.Finish
				if e.Kind == EventError {
					s.outcome = FinishFailed
				}
			}
			if err := s.encoder.Emit(&s.out, e); err != nil {
				s.fail(err)
				break
			}
		}
	}
	if s.out.Len() == 0 {
		return 0, io.EOF
	}
	return s.out.Read(p)
}

// clientMessage strips internal prefixes so client frames carry only the
// operator-facing reason, never the Go error wrap.
func clientMessage(err error) string {
	var inv *InvalidUpstreamResponse
	if errors.As(err, &inv) {
		return inv.Reason
	}
	return err.Error()
}

func (s *StreamReader) fail(err error) {
	s.done = true
	s.outcome = FinishFailed
	_ = s.encoder.Emit(&s.out, Event{Kind: EventError, Error: &ErrorIR{Type: "api_error", Message: clientMessage(err)}, Usage: s.decoder.usage})
}

// end renders a source that stopped without a usable terminal event. Legacy
// mode reproduces each historic reader's exact end-of-stream frames; strict
// mode fails visibly instead of synthesizing a completed answer.
func (s *StreamReader) end(err error) {
	var tr *streamTruncation
	if !errors.As(err, &tr) {
		s.fail(err)
		return
	}
	s.done = true
	s.outcome = FinishFailed
	if !s.encoder.opts.Legacy {
		_ = s.encoder.Emit(&s.out, Event{Kind: EventError, Error: &ErrorIR{Type: "api_error", Message: tr.Reason}, Usage: s.decoder.usage})
		return
	}
	switch s.encoder.surface {
	case provider.SurfaceAnthropic:
		// Historic cut frames never started a message first.
		s.encoder.frame(&s.out, "error", string(EncodeError(ErrorIR{Type: "api_error", Message: tr.Reason}, provider.SurfaceAnthropic)))
	case provider.SurfaceOpenAI:
		if !tr.Transport {
			s.encoder.start(&s.out, s.decoder.id, s.decoder.created) // historic ensureStarted
		}
		s.encoder.frame(&s.out, "", `{"error":{"message":`+mustMarshalString(tr.Reason)+`,"type":"api_error"}}`)
		s.encoder.frame(&s.out, "", "[DONE]")
	}
}

type encodedBlock struct {
	event     Event
	index     int
	callIndex int
	id        string
	text      strings.Builder
	arguments strings.Builder
	closed    bool
}
type eventEncoder struct {
	source   provider.Surface
	surface  provider.Surface
	opts     CodecOptions
	model    string
	id       string
	created  int64
	started  bool
	ping     bool
	terminal bool
	usage    UsageIR
	blocks   map[string]*encodedBlock
	order    []string
	current  string
	calls    int
	seq      int
}

func streamIdentity() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("generated_%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
func (e *eventEncoder) frame(out *bytes.Buffer, name, data string) {
	if name != "" {
		out.WriteString("event: ")
		out.WriteString(name)
		out.WriteByte('\n')
	}
	out.WriteString("data: ")
	out.WriteString(data)
	out.WriteString("\n\n")
}
func (e *eventEncoder) jsonFrame(out *bytes.Buffer, name string, p map[string]any) {
	if e.surface == provider.SurfaceResponses {
		p["type"] = name
		p["sequence_number"] = e.seq
		e.seq++
	}
	b, _ := json.Marshal(p)
	e.frame(out, name, string(b))
}
func (e *eventEncoder) chatChunk(out *bytes.Buffer, delta, finish string) {
	e.frame(out, "", fmt.Sprintf(`{"id":%s,"object":"chat.completion.chunk","created":%d,"model":%s,"choices":[{"index":0,"delta":%s,"finish_reason":%s}]}`,
		mustMarshalString(e.id), e.created, mustMarshalString(e.model), delta, finish))
}
func (e *eventEncoder) start(out *bytes.Buffer, id string, created int64) {
	if e.started {
		return
	}
	e.started = true
	e.id = id
	e.created = created
	if !e.opts.Legacy {
		if e.id == "" {
			e.id = streamIdentity()
		}
		if e.created == 0 {
			e.created = time.Now().Unix()
		}
	}
	switch e.surface {
	case provider.SurfaceOpenAI:
		e.chatChunk(out, `{"role":"assistant"}`, "null")
	case provider.SurfaceAnthropic:
		if !e.ping {
			e.frame(out, "ping", `{"type":"ping"}`)
			e.ping = true
		}
		id := e.id
		if id == "" && e.opts.Legacy && e.source == provider.SurfaceResponses {
			id = "msg_generated"
		}
		if !strings.HasPrefix(id, "msg_") {
			id = "msg_" + id
		}
		usage := "{}"
		if e.opts.Legacy {
			usage = `{"input_tokens":0,"output_tokens":1}`
		} else {
			usage = string(streamMessagesUsage(e.usage))
		}
		e.frame(out, "message_start", fmt.Sprintf(`{"type":"message_start","message":{"type":"message","id":%s,"role":"assistant","content":[],"model":%s,"stop_reason":null,"stop_sequence":null,"usage":%s}}`, mustMarshalString(id), mustMarshalString(e.model), usage))
	case provider.SurfaceResponses:
		if !strings.HasPrefix(e.id, "resp_") {
			e.id = "resp_" + e.id
		}
		e.jsonFrame(out, "response.created", map[string]any{"response": e.responseObject("in_progress", nil)})
		e.jsonFrame(out, "response.in_progress", map[string]any{"response": e.responseObject("in_progress", nil)})
	}
}
func (e *eventEncoder) Emit(out *bytes.Buffer, v Event) error {
	if e.terminal {
		return nil
	}
	switch v.Kind {
	case EventStart:
		e.start(out, v.ID, v.Created)
		return nil
	case EventUsage:
		e.usage.Merge(v.Usage)
		return nil
	case EventError:
		e.terminal = true
		e.usage.Merge(v.Usage)
		absent := v.Error == nil
		er := v.Error
		if absent {
			er = &ErrorIR{}
		}
		switch e.surface {
		case provider.SurfaceResponses:
			if !e.started {
				e.terminal = false
				e.start(out, "", 0)
				e.terminal = true
			}
			e.jsonFrame(out, "response.failed", map[string]any{"response": e.responseObject("failed", er)})
		case provider.SurfaceOpenAI:
			if e.opts.Legacy {
				// Historic shape: the upstream error object verbatim
				// (message-first), a usage chunk only for real spend, [DONE],
				// and no role chunk was synthesized first. An absent error
				// object fell back to exactly this frame.
				if absent {
					e.frame(out, "", `{"error":{"message":"upstream stream failed","type":"api_error"}}`)
				} else if b, err := json.Marshal(responsesWireError{Message: er.Message, Type: er.Type, Code: er.Code}); err == nil {
					e.frame(out, "", `{"error":`+string(b)+`}`)
				} else {
					e.frame(out, "", `{"error":{"message":"upstream stream failed","type":"api_error"}}`)
				}
				in, outN := 0, 0
				if e.usage.Input != nil {
					in = *e.usage.Input
				}
				if e.usage.Output != nil {
					outN = *e.usage.Output
				}
				if in > 0 || outN > 0 {
					e.chatUsage(out)
				}
				e.frame(out, "", "[DONE]")
				return nil
			}
			if absent {
				er = &ErrorIR{Type: "api_error", Message: "upstream stream failed"}
			}
			e.frame(out, "", string(EncodeError(*er, e.surface)))
			if e.usage.Input != nil || e.usage.Output != nil {
				e.chatUsage(out)
			}
			e.frame(out, "", "[DONE]")
		case provider.SurfaceAnthropic:
			if !e.opts.Legacy && (e.usage.Input != nil || e.usage.Output != nil) {
				e.frame(out, "message_delta", `{"type":"message_delta","delta":{"stop_reason":null,"stop_sequence":null},"usage":`+string(streamMessagesUsage(e.usage))+`}`)
			}
			if e.opts.Legacy {
				// Historic emitError frames carried type+message only, always
				// named api_error, with the message verbatim (even empty).
				er = &ErrorIR{Type: "api_error", Message: er.Message}
				if e.source == provider.SurfaceOpenAI {
					// Historic chat reader: message passes through unchanged
					// and no message_start was synthesized first.
				} else {
					// Historic Responses reader: starts the message and
					// defaults only this leg's empty message.
					if er.Message == "" {
						er.Message = "upstream response failed"
					}
					if !e.started {
						e.terminal = false
						e.start(out, e.id, e.created)
						e.terminal = true
					}
				}
				e.frame(out, "error", `{"type":"error","error":{"type":`+mustMarshalString(er.Type)+`,"message":`+mustMarshalString(er.Message)+`}}`)
				return nil
			}
			if absent {
				er = &ErrorIR{Type: "api_error", Message: "upstream stream failed"}
			}
			e.frame(out, "error", string(EncodeError(*er, e.surface)))
		default:
			return invalidResponse("unknown destination wire " + string(e.surface))
		}
		return nil
	case EventFinish:
		e.usage.Merge(v.Usage)
		if !e.started {
			e.start(out, "", 0)
		}
		for _, key := range e.order {
			if err := e.closeBlock(out, key); err != nil {
				return err
			}
		}
		e.terminal = true
		switch e.surface {
		case provider.SurfaceOpenAI:
			e.chatChunk(out, `{}`, mustMarshalString(streamChatFinish(v.Finish)))
			if e.opts.Legacy || e.usage.Input != nil || e.usage.Output != nil {
				e.chatUsage(out)
			}
			e.frame(out, "", "[DONE]")
		case provider.SurfaceAnthropic:
			usage := e.usage
			if e.opts.Legacy {
				if usage.Input == nil {
					z := 0
					usage.Input = &z
				}
				if usage.Output == nil {
					z := 0
					usage.Output = &z
				}
			}
			sequence := "null"
			if v.StopSequence != nil {
				sequence = mustMarshalString(*v.StopSequence)
			}
			e.frame(out, "message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%s,"stop_sequence":%s},"usage":%s}`, mustMarshalString(streamMessagesFinish(v.Finish)), sequence, streamMessagesUsage(usage)))
			e.frame(out, "message_stop", `{"type":"message_stop"}`)
		case provider.SurfaceResponses:
			status := "completed"
			if v.Finish == FinishLength || v.Finish == FinishRefusal {
				status = "incomplete"
			}
			obj := e.responseObject(status, nil)
			if status == "incomplete" {
				reason := "max_output_tokens"
				if v.Finish == FinishRefusal {
					reason = "content_filter"
				}
				obj["incomplete_details"] = map[string]any{"reason": reason}
			}
			e.jsonFrame(out, "response."+status, map[string]any{"response": obj})
		default:
			return invalidResponse("unknown destination wire " + string(e.surface))
		}
		return nil
	}
	if !e.started {
		e.start(out, "", 0)
	}
	switch v.Kind {
	case EventBlockStart:
		if old, ok := e.blocks[v.Key]; ok {
			if old.closed {
				return invalidResponse("reopened stopped block")
			}
			return nil
		}
		b := &encodedBlock{event: v, index: len(e.order), callIndex: -1}
		if v.Block.Kind == BlockCall {
			b.callIndex = e.calls
			e.calls++
		}
		e.blocks[v.Key] = b
		e.order = append(e.order, v.Key)
		switch e.surface {
		case provider.SurfaceOpenAI:
			if v.Block.Kind == BlockCall {
				e.chatChunk(out, fmt.Sprintf(`{"tool_calls":[{"index":%d,"id":%s,"type":"function","function":{"name":%s,"arguments":""}}]}`, b.callIndex, mustMarshalString(v.Block.CallID), mustMarshalString(v.Block.Name)), "null")
			}
		case provider.SurfaceAnthropic:
			// Strict and Responses legacy targets are single-open-block. Chat
			// legacy keeps its exact characterized multi-open event ordering.
			if !(e.opts.Legacy && e.source == provider.SurfaceOpenAI) && e.current != "" {
				if err := e.closeBlock(out, e.current); err != nil {
					return err
				}
			}
			e.current = v.Key
			if v.Block.Kind == BlockCall {
				e.frame(out, "content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%s,"name":%s,"input":{}}}`, b.index, mustMarshalString(v.Block.CallID), mustMarshalString(v.Block.Name)))
			} else {
				e.frame(out, "content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, b.index))
			}
		case provider.SurfaceResponses:
			prefix := "msg_"
			if v.Block.Kind == BlockCall {
				prefix = "fc_"
			}
			b.id = fmt.Sprintf("%s%s_%d", prefix, e.id, b.index)
			e.jsonFrame(out, "response.output_item.added", map[string]any{"output_index": b.index, "item": e.itemObject(b, "in_progress")})
			if v.Block.Kind == BlockText {
				e.jsonFrame(out, "response.content_part.added", map[string]any{"output_index": b.index, "content_index": 0, "item_id": b.id, "part": responseText("")})
			}
		}
	case EventTextDelta, EventArgumentsDelta:
		b, ok := e.blocks[v.Key]
		if !ok || b.closed {
			if e.opts.Legacy {
				return invalidResponse("upstream interleaved tool argument streams; cannot map to the messages wire")
			}
			return invalidResponse("upstream interleaved tool argument streams; cannot map delta into unopened/stopped block")
		}
		if v.Kind == EventArgumentsDelta && b.event.Block.Kind != BlockCall || v.Kind == EventTextDelta && b.event.Block.Kind != BlockText {
			return invalidResponse("delta kind does not match block")
		}
		// Accumulate only when required: Responses terminal output, or strict
		// Messages JSON-object validation. Chat text/args never need retention.
		if e.surface == provider.SurfaceResponses {
			if v.Kind == EventTextDelta {
				b.text.WriteString(v.Delta)
			} else {
				b.arguments.WriteString(v.Delta)
			}
		}
		if e.surface == provider.SurfaceAnthropic && !e.opts.Legacy && v.Kind == EventArgumentsDelta {
			b.arguments.WriteString(v.Delta)
		}
		switch e.surface {
		case provider.SurfaceOpenAI:
			if v.Kind == EventTextDelta {
				e.chatChunk(out, `{"content":`+mustMarshalString(v.Delta)+`}`, "null")
			} else {
				e.chatChunk(out, fmt.Sprintf(`{"tool_calls":[{"index":%d,"function":{"arguments":%s}}]}`, b.callIndex, mustMarshalString(v.Delta)), "null")
			}
		case provider.SurfaceAnthropic:
			if v.Kind == EventTextDelta {
				e.frame(out, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`, b.index, mustMarshalString(v.Delta)))
			} else {
				e.frame(out, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`, b.index, mustMarshalString(v.Delta)))
			}
		case provider.SurfaceResponses:
			name := "response.output_text.delta"
			p := map[string]any{"item_id": b.id, "output_index": b.index, "delta": v.Delta}
			if v.Kind == EventTextDelta {
				p["content_index"] = 0
				p["logprobs"] = []any{}
			} else {
				name = "response.function_call_arguments.delta"
			}
			e.jsonFrame(out, name, p)
		}
	case EventBlockEnd:
		// Chat legacy closes all blocks together, not as soon as they end.
		if e.opts.Legacy && e.source == provider.SurfaceOpenAI && e.surface == provider.SurfaceAnthropic {
			return nil
		}
		return e.closeBlock(out, v.Key)
	}
	return nil
}
func (e *eventEncoder) closeBlock(out *bytes.Buffer, key string) error {
	b, ok := e.blocks[key]
	if !ok || b.closed {
		return nil
	}
	if e.surface == provider.SurfaceAnthropic && !e.opts.Legacy && b.event.Block.Kind == BlockCall {
		if _, err := argumentsObject(b.arguments.String(), b.event.Block.CallID); err != nil {
			return invalidResponse(err.Error())
		}
	}
	b.closed = true
	if e.current == key {
		e.current = ""
	}
	switch e.surface {
	case provider.SurfaceAnthropic:
		e.frame(out, "content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, b.index))
	case provider.SurfaceResponses:
		p := map[string]any{"item_id": b.id, "output_index": b.index}
		if b.event.Block.Kind == BlockText {
			p["content_index"] = 0
			p["text"] = b.text.String()
			p["logprobs"] = []any{}
			e.jsonFrame(out, "response.output_text.done", p)
			e.jsonFrame(out, "response.content_part.done", map[string]any{"item_id": b.id, "output_index": b.index, "content_index": 0, "part": responseText(b.text.String())})
		} else {
			p["arguments"] = b.arguments.String()
			p["name"] = b.event.Block.Name
			e.jsonFrame(out, "response.function_call_arguments.done", p)
		}
		e.jsonFrame(out, "response.output_item.done", map[string]any{"output_index": b.index, "item": e.itemObject(b, "completed")})
	}
	return nil
}
func streamChatFinish(f FinishKind) string {
	switch f {
	case FinishTools:
		return "tool_calls"
	case FinishLength:
		return "length"
	case FinishRefusal:
		return "content_filter"
	default:
		return "stop"
	}
}
func streamMessagesFinish(f FinishKind) string {
	switch f {
	case FinishTools:
		return "tool_use"
	case FinishLength:
		return "max_tokens"
	case FinishRefusal:
		return "refusal"
	case FinishStopSequence:
		return "stop_sequence"
	default:
		return "end_turn"
	}
}
func streamMessagesUsage(u UsageIR) []byte {
	m := map[string]int{}
	if u.Input != nil {
		m["input_tokens"] = *u.Input
	}
	if u.Output != nil {
		m["output_tokens"] = *u.Output
	}
	// Fixed order matches the legacy byte contract (maps sort input before output).
	b, _ := json.Marshal(m)
	return b
}
func (e *eventEncoder) chatUsage(out *bytes.Buffer) {
	if e.opts.Legacy {
		in, ot := 0, 0
		if e.usage.Input != nil {
			in = *e.usage.Input
		}
		if e.usage.Output != nil {
			ot = *e.usage.Output
		}
		e.frame(out, "", fmt.Sprintf(`{"id":%s,"object":"chat.completion.chunk","created":%d,"model":%s,"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d}}`, mustMarshalString(e.id), e.created, mustMarshalString(e.model), in, ot))
		return
	}
	u := map[string]int{}
	if e.usage.Input != nil {
		u["prompt_tokens"] = *e.usage.Input
	}
	if e.usage.Output != nil {
		u["completion_tokens"] = *e.usage.Output
	}
	if e.usage.Input != nil && e.usage.Output != nil {
		u["total_tokens"] = *e.usage.Input + *e.usage.Output
	}
	b, _ := json.Marshal(u)
	e.frame(out, "", fmt.Sprintf(`{"id":%s,"object":"chat.completion.chunk","created":%d,"model":%s,"choices":[],"usage":%s}`, mustMarshalString(e.id), e.created, mustMarshalString(e.model), b))
}
func responseText(s string) map[string]any {
	return map[string]any{"type": "output_text", "text": s, "annotations": []any{}, "logprobs": []any{}}
}
func (e *eventEncoder) itemObject(b *encodedBlock, status string) map[string]any {
	if b.event.Block.Kind == BlockCall {
		return map[string]any{"type": "function_call", "id": b.id, "status": status, "call_id": b.event.Block.CallID, "name": b.event.Block.Name, "arguments": b.arguments.String()}
	}
	content := []any{}
	if status != "in_progress" || b.text.Len() > 0 {
		content = append(content, responseText(b.text.String()))
	}
	return map[string]any{"type": "message", "id": b.id, "status": status, "role": "assistant", "content": content}
}
func (e *eventEncoder) responseObject(status string, err *ErrorIR) map[string]any {
	output := []any{}
	for _, key := range e.order {
		b := e.blocks[key]
		itemStatus := "in_progress"
		if b.closed {
			itemStatus = "completed"
		}
		output = append(output, e.itemObject(b, itemStatus))
	}
	var usage any
	if e.usage.Input != nil || e.usage.Output != nil {
		u := map[string]any{}
		if e.usage.Input != nil {
			u["input_tokens"] = *e.usage.Input
			u["input_tokens_details"] = map[string]int{"cached_tokens": 0}
		}
		if e.usage.Output != nil {
			u["output_tokens"] = *e.usage.Output
			u["output_tokens_details"] = map[string]int{"reasoning_tokens": 0}
		}
		if e.usage.Input != nil && e.usage.Output != nil {
			u["total_tokens"] = *e.usage.Input + *e.usage.Output
		}
		usage = u
	}
	return map[string]any{"id": e.id, "object": "response", "created_at": e.created, "status": status, "model": e.model, "error": err, "incomplete_details": nil, "output": output, "usage": usage,
		"instructions": nil, "max_output_tokens": nil, "metadata": map[string]any{}, "parallel_tool_calls": true, "tool_choice": "auto", "tools": []any{}, "temperature": nil, "top_p": nil, "truncation": "disabled", "store": false, "text": map[string]any{"format": map[string]any{"type": "text"}}, "reasoning": nil, "user": nil}
}
