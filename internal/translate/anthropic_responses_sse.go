package translate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// responsesAnthropicSSEReader wraps a Responses API SSE body and yields the
// equivalent Anthropic /v1/messages SSE stream — the streaming half of the
// anthropic→responses leg (Claude Code against a responses-only hop).
//
// Event handling mirrors responsesSSEReader (verified live against
// tokenrouter gpt-6.1-sol); only the emitted frames differ. Emission
// discipline matches sse.go: ping → message_start → blocks in open order →
// message_delta (stop_reason + usage) → message_stop. A transport cut or a
// clean EOF without response.completed still closes honestly — never a bare
// TCP close.
type responsesAnthropicSSEReader struct {
	scanner     *bufio.Scanner
	out         bytes.Buffer
	done        bool
	pingSent    bool
	started     bool
	clientModel string
	id          string

	blockOpen bool
	openKind  string // "text" | "tool_use" — the currently open block
	blockIdx  int
	toolIdx   map[string]int // call_id -> content_block index
	toolCalls bool
	finish    string
	usageIn   int
	usageOut  int
	usageSeen bool
}

// NewResponsesAnthropicSSEReader wraps a Responses SSE body and returns a
// reader yielding Anthropic events. clientModel is the model the client asked
// for (message_start echoes it).
func NewResponsesAnthropicSSEReader(r io.Reader, clientModel string) io.Reader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	return &responsesAnthropicSSEReader{
		scanner:     sc,
		clientModel: clientModel,
		toolIdx:     map[string]int{},
	}
}

func (s *responsesAnthropicSSEReader) Read(p []byte) (int, error) {
	for s.out.Len() == 0 && !s.done {
		if !s.advance() {
			if s.out.Len() == 0 {
				return 0, io.EOF
			}
			break
		}
	}
	if s.out.Len() == 0 {
		return 0, io.EOF
	}
	return s.out.Read(p)
}

// advance consumes one upstream event and writes translated frames. Returns
// false at source exhaustion.
func (s *responsesAnthropicSSEReader) advance() bool {
	ev, payload, ok := s.nextEvent()
	if !ok {
		// done MUST be set before emitting (see responsesSSEReader): the
		// close frames unblock io.Copy and prevent duplicate closes.
		s.done = true
		if err := s.scanner.Err(); err != nil {
			s.emitError("api_error", "upstream stream failed")
		} else {
			// Clean EOF without response.completed: close honestly.
			s.computeFinish()
			s.emitClose()
		}
		return false
	}

	switch ev {
	case "response.created":
		var e struct {
			Response struct {
				ID string `json:"id"`
			} `json:"response"`
		}
		if err := json.Unmarshal(payload, &e); err == nil {
			s.id = e.Response.ID
		}
		s.startMessage()
		return true

	case "response.output_text.delta":
		var e struct {
			Delta string `json:"delta"`
		}
		if err := json.Unmarshal(payload, &e); err != nil {
			return true // one bad delta never kills the stream
		}
		s.ensureStarted()
		// One text block streams MANY deltas: only open a block when the
		// currently open one is not text (a tool block that has ended).
		if s.blockOpen && s.openKind != "text" {
			s.closeBlock()
		}
		if !s.blockOpen {
			s.frame("content_block_start", fmt.Sprintf(
				`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, s.blockIdx))
			s.blockOpen = true
			s.openKind = "text"
		}
		s.frame("content_block_delta", fmt.Sprintf(
			`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`,
			s.blockIdx, mustMarshalString(e.Delta)))
		return true

	case "response.output_item.added":
		var e struct {
			Item struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
				Name   string `json:"name"`
			} `json:"item"`
		}
		if err := json.Unmarshal(payload, &e); err != nil || e.Item.Type != "function_call" {
			return true // message/reasoning items: content arrives via deltas
		}
		s.ensureStarted()
		if s.blockOpen {
			s.closeBlock()
		}
		idx := s.blockIdx
		s.toolIdx[e.Item.CallID] = idx
		s.toolCalls = true
		s.frame("content_block_start", fmt.Sprintf(
			`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%s,"name":%s,"input":{}}}`,
			idx, mustMarshalString(e.Item.CallID), mustMarshalString(e.Item.Name)))
		s.blockOpen = true
		s.openKind = "tool_use"
		return true

	case "response.function_call_arguments.delta":
		var e struct {
			ItemID string `json:"item_id"`
			Delta  string `json:"delta"`
		}
		if err := json.Unmarshal(payload, &e); err != nil {
			return true
		}
		idx, seen := s.toolIdx[e.ItemID]
		if !seen {
			return true // fragment for a call we never saw open: skip
		}
		s.ensureStarted()
		// Stay inside this tool block even if a text delta interleaved.
		if s.blockOpen && s.blockIdx != idx {
			s.closeBlock()
			s.blockOpen = true
			s.openKind = "tool_use"
			s.blockIdx = idx
		}
		s.frame("content_block_delta", fmt.Sprintf(
			`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`,
			idx, mustMarshalString(e.Delta)))
		return true

	case "response.completed":
		var e struct {
			Response struct {
				Usage responsesOutUsage `json:"usage"`
			} `json:"response"`
		}
		_ = json.Unmarshal(payload, &e)
		s.ensureStarted()
		s.usageIn, s.usageOut = e.Response.Usage.InputTokens, e.Response.Usage.OutputTokens
		s.usageSeen = true
		s.computeFinish()
		s.emitClose()
		s.done = true
		return true

	case "response.incomplete":
		s.ensureStarted()
		s.finish = "max_tokens"
		s.emitClose()
		s.done = true
		return true

	case "response.failed":
		s.ensureStarted()
		var e struct {
			Response struct {
				Error responsesWireError `json:"error"`
			} `json:"response"`
		}
		_ = json.Unmarshal(payload, &e)
		msg := e.Response.Error.Message
		if msg == "" {
			msg = "upstream response failed"
		}
		s.emitError("api_error", msg)
		return true
	}
	// response.output_item.done, content filters, anything else: ignored —
	// the state we need is in the events above.
	return true
}

// computeFinish derives the stop_reason from what arrived.
func (s *responsesAnthropicSSEReader) computeFinish() {
	if s.finish == "" {
		if s.toolCalls {
			s.finish = "tool_use"
		} else {
			s.finish = "end_turn"
		}
	}
}

// startMessage emits ping + message_start exactly once (usage 0/1 matches the
// other anthropic emitter — real numbers ride message_delta).
func (s *responsesAnthropicSSEReader) startMessage() {
	if s.started {
		return
	}
	s.started = true
	if !s.pingSent {
		s.frame("ping", `{"type":"ping"}`)
		s.pingSent = true
	}
	id := s.id
	if id == "" {
		id = "msg_generated"
	}
	if !strings.HasPrefix(id, "msg_") {
		id = "msg_" + id
	}
	s.frame("message_start", fmt.Sprintf(
		`{"type":"message_start","message":{"type":"message","id":%s,"role":"assistant","content":[],"model":%s,"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":1}}}`,
		mustMarshalString(id), mustMarshalString(s.clientModel)))
}

func (s *responsesAnthropicSSEReader) ensureStarted() {
	if !s.started {
		s.startMessage()
	}
}

func (s *responsesAnthropicSSEReader) closeBlock() {
	if !s.blockOpen {
		return
	}
	s.frame("content_block_stop", fmt.Sprintf(
		`{"type":"content_block_stop","index":%d}`, s.blockIdx))
	s.blockOpen = false
	s.blockIdx++
}

// emitClose runs once: close open blocks, message_delta with the real usage,
// message_stop.
func (s *responsesAnthropicSSEReader) emitClose() {
	s.closeBlock()
	s.computeFinish()
	s.frame("message_delta", fmt.Sprintf(
		`{"type":"message_delta","delta":{"stop_reason":%s,"stop_sequence":null},"usage":{"input_tokens":%d,"output_tokens":%d}}`,
		mustMarshalString(s.finish), s.usageIn, s.usageOut))
	s.frame("message_stop", `{"type":"message_stop"}`)
}

func (s *responsesAnthropicSSEReader) emitError(errType, message string) {
	s.frame("error", fmt.Sprintf(
		`{"type":"error","error":{"type":%s,"message":%s}}`,
		mustMarshalString(errType), mustMarshalString(message)))
	s.done = true
}

func (s *responsesAnthropicSSEReader) frame(event, data string) {
	s.out.WriteString("event: ")
	s.out.WriteString(event)
	s.out.WriteString("\ndata: ")
	s.out.WriteString(data)
	s.out.WriteString("\n\n")
}

// nextEvent reads one SSE event+data pair, joining continuation lines. Returns
// false at source exhaustion. (Same loop shape as responsesSSEReader.)
func (s *responsesAnthropicSSEReader) nextEvent() (string, []byte, bool) {
	var ev string
	var data []byte
	inEvent, inData := false, false
	for s.scanner.Scan() {
		line := s.scanner.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			ev = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			inEvent = true
		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if inData && len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, payload...)
			inData = true
		case line == "":
			if inEvent || inData {
				return ev, data, true
			}
			// blank between events: keep scanning
		default:
			// comment (": keep-alive") or unknown line: ignore
		}
	}
	if inEvent || inData {
		return ev, data, true
	}
	return "", nil, false
}
