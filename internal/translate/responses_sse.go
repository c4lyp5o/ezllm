package translate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// responsesSSEReader converts an OpenAI Responses API SSE stream into an
// OpenAI chat-completions SSE stream on the fly — a pure io.Reader wrapper,
// no goroutines, no whole-stream buffering (same rules as the anthropic
// synthesis; contracts in docs/responses-wire-design.md).
//
// Upstream events in, chat chunks out:
//
//	response.created               -> first chunk, delta {role:assistant}
//	response.output_text.delta     -> content delta
//	response.output_item.added     -> tool_call OPEN (function_call items only;
//	                                 message/reasoning items are skipped)
//	response.function_call_arguments.delta -> tool_call fragment (index mapped
//	                                 from the item_id of the open call)
//	response.completed             -> finish_reason chunk + usage chunk + [DONE]
//	response.failed                -> the upstream error object verbatim, [DONE]
//	response.incomplete            -> finish_reason "length" close
//	anything else                  -> skipped, never fatal
type responsesSSEReader struct {
	scanner *bufio.Scanner
	out     bytes.Buffer
	err     error
	done    bool

	clientModel string
	id          string
	created     int64
	started     bool

	callIndex map[string]int // item_id OR call_id -> chat tool_call index
	nextCall  int
}

// NewResponsesSSEReader wraps a Responses API SSE body and returns a reader
// yielding the equivalent chat-completions SSE stream. clientModel is the
// model name the client asked for (chunks echo it).
func NewResponsesSSEReader(r io.Reader, clientModel string) io.Reader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	return &responsesSSEReader{
		scanner:     sc,
		clientModel: clientModel,
		callIndex:   map[string]int{},
	}
}

func (s *responsesSSEReader) Read(p []byte) (int, error) {
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

// advance consumes one upstream SSE event and writes translated chunks.
// Returns false at source exhaustion.
func (s *responsesSSEReader) advance() bool {
	ev, payload, ok := s.nextEvent()
	if !ok {
		// done MUST be set before emitting: the close frames below unblock
		// io.Copy, and without it every subsequent Read would append another
		// (duplicate) close — an infinite stream instead of EOF.
		s.done = true
		if err := s.scanner.Err(); err != nil {
			// Transport cut mid-stream: surface the failure as a chat error
			// frame rather than a bare close.
			s.frame(`{"error":{"message":"upstream stream failed","type":"api_error"}}`)
			s.frame("[DONE]")
		} else {
			// Clean EOF with NO terminal event. That is a truncated stream,
			// not a completion: synthesizing finish_reason "stop" here told the
			// client (and MarkHopGood) that a cut-off answer was successful.
			s.ensureStarted()
			s.frame(`{"error":{"message":"upstream stream ended without a terminal event","type":"api_error"}}`)
			s.frame("[DONE]")
		}
		return false
	}

	switch ev {
	case "response.created":
		var e struct {
			Response struct {
				ID        string `json:"id"`
				CreatedAt int64  `json:"created_at"`
			} `json:"response"`
		}
		if err := json.Unmarshal(payload, &e); err == nil {
			s.id, s.created = e.Response.ID, e.Response.CreatedAt
		}
		s.startMessage() // emits the role chunk once
		return true

	case "response.output_text.delta":
		var e struct {
			Delta string `json:"delta"`
		}
		if err := json.Unmarshal(payload, &e); err != nil {
			return true // skip an undecodable delta, never kill the stream
		}
		s.ensureStarted()
		s.chunk(fmt.Sprintf(`{"content":%s}`, mustMarshalString(e.Delta)), "null")
		return true

	case "response.output_item.added":
		var e struct {
			Item struct {
				ID     string `json:"id"`
				Type   string `json:"type"`
				CallID string `json:"call_id"`
				Name   string `json:"name"`
			} `json:"item"`
		}
		if err := json.Unmarshal(payload, &e); err != nil || e.Item.Type != "function_call" {
			return true // message/reasoning items: content arrives via deltas
		}
		s.ensureStarted()
		idx := s.nextCall
		s.nextCall++
		// Register the item id AND the call id. Argument deltas reference the
		// item id while the client-visible tool id is the call id, and the two
		// are DIFFERENT strings in real streams (item "fc_...", call "call_...").
		// Keying on only one of them silently dropped every argument fragment.
		s.registerCall(idx, e.Item.ID, e.Item.CallID)
		s.chunk(fmt.Sprintf(`{"tool_calls":[{"index":%d,"id":%s,"type":"function","function":{"name":%s,"arguments":""}}]}`,
			idx, mustMarshalString(e.Item.CallID), mustMarshalString(e.Item.Name)), "null")
		return true

	case "response.function_call_arguments.delta":
		var e struct {
			ItemID string `json:"item_id"`
			CallID string `json:"call_id"`
			Delta  string `json:"delta"`
		}
		if err := json.Unmarshal(payload, &e); err != nil {
			return true
		}
		idx, seen := s.lookupCall(e.ItemID, e.CallID)
		if !seen {
			return true // fragment for a call we never saw open: skip
		}
		s.ensureStarted()
		s.chunk(fmt.Sprintf(`{"tool_calls":[{"index":%d,"function":{"arguments":%s}}]}`,
			idx, mustMarshalString(e.Delta)), "null")
		return true

	case "response.completed":
		var e struct {
			Response struct {
				Usage responsesOutUsage `json:"usage"`
			} `json:"response"`
		}
		_ = json.Unmarshal(payload, &e)
		s.ensureStarted()
		finish := "stop"
		if s.nextCall > 0 {
			finish = "tool_calls"
		}
		s.emitFinish(finish, e.Response.Usage.InputTokens, e.Response.Usage.OutputTokens)
		s.frame("[DONE]")
		s.done = true
		return true

	case "response.incomplete":
		// Terminal like response.completed, but truncated: the payload still
		// carries real usage, and dropping it recorded zero spend for the call.
		var e struct {
			Response struct {
				Usage responsesOutUsage `json:"usage"`
			} `json:"response"`
		}
		_ = json.Unmarshal(payload, &e)
		s.ensureStarted()
		s.emitFinish("length", e.Response.Usage.InputTokens, e.Response.Usage.OutputTokens)
		s.frame("[DONE]")
		s.done = true
		return true

	case "response.failed":
		var e struct {
			Response struct {
				Error *responsesWireError `json:"error"`
				Usage responsesOutUsage   `json:"usage"`
			} `json:"response"`
		}
		_ = json.Unmarshal(payload, &e)
		if e.Response.Error != nil {
			if b, err := json.Marshal(e.Response.Error); err == nil {
				s.frame(`{"error":` + string(b) + `}`)
			} else {
				s.frame(`{"error":{"message":"upstream stream failed","type":"api_error"}}`)
			}
		} else {
			s.frame(`{"error":{"message":"upstream stream failed","type":"api_error"}}`)
		}
		// A failed run still consumed tokens: report them so the ledger records
		// the spend instead of a zero.
		if e.Response.Usage.InputTokens > 0 || e.Response.Usage.OutputTokens > 0 {
			s.usageChunk(e.Response.Usage.InputTokens, e.Response.Usage.OutputTokens)
		}
		s.frame("[DONE]")
		s.done = true
		return true
	}
	return true // any other event type: skipped by design
}

// startMessage emits the first chunk (role delta). Callers use it when the
// upstream explicitly signals creation; ensureStarted covers streams whose
// first interesting event is a delta.
func (s *responsesSSEReader) startMessage() {
	if s.started {
		return
	}
	s.started = true
	s.chunk(`{"role":"assistant"}`, "null")
}

func (s *responsesSSEReader) ensureStarted() { s.startMessage() }

// chunk emits one chat.completion.chunk with the given delta VALUE
// (e.g. `{"role":"assistant"}`) and finish_reason JSON literal.
func (s *responsesSSEReader) chunk(delta, finish string) {
	s.frame(fmt.Sprintf(`{"id":%s,"object":"chat.completion.chunk","created":%d,"model":%s,"choices":[{"index":0,"delta":%s,"finish_reason":%s}]}`,
		mustMarshalString(s.id), s.created, mustMarshalString(s.clientModel), delta, finish))
}

// registerCall maps an open function call's item id AND call id to its chat
// tool_call index. Sources differ on which one a later frame quotes, and the
// pair is not equal in practice (item "fc_x" vs call "call_y").
func (s *responsesSSEReader) registerCall(idx int, id, callID string) {
	if id != "" {
		s.callIndex[id] = idx
	}
	if callID != "" {
		s.callIndex[callID] = idx
	}
}

// lookupCall resolves a fragment to its tool_call index by whichever id the
// frame carried (empty ids are ignored, never treated as a match).
func (s *responsesSSEReader) lookupCall(ids ...string) (int, bool) {
	for _, id := range ids {
		if id == "" {
			continue
		}
		if idx, ok := s.callIndex[id]; ok {
			return idx, true
		}
	}
	return 0, false
}

// usageChunk writes the usage-only chunk (empty choices) OpenAI sends when
// usage is requested. Used where a terminal event carries usage but there is no
// successful finish chunk to attach it to.
func (s *responsesSSEReader) usageChunk(in, out int) {
	s.frame(fmt.Sprintf(`{"id":%s,"object":"chat.completion.chunk","created":%d,"model":%s,"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d}}`,
		mustMarshalString(s.id), s.created, mustMarshalString(s.clientModel), in, out))
}

// emitFinish writes the closing pair in OpenAI's include_usage shape: the
// finish_reason chunk, then a usage-only chunk with empty choices.
func (s *responsesSSEReader) emitFinish(finish string, in, out int) {
	s.ensureStarted()
	s.chunk(`{}`, mustMarshalString(finish))
	s.usageChunk(in, out)
}

func (s *responsesSSEReader) frame(data string) {
	s.out.WriteString("data: ")
	s.out.WriteString(data)
	s.out.WriteString("\n\n")
}

// nextEvent reads one upstream SSE event block (event: + data: lines) and
// returns its name and JSON payload.
func (s *responsesSSEReader) nextEvent() (string, []byte, bool) {
	var ev string
	var data strings.Builder
	inBlock := false
	for s.scanner.Scan() {
		line := s.scanner.Text()
		if line == "" {
			if inBlock {
				return ev, []byte(data.String()), true
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "event: "):
			ev = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			inBlock = true
		case strings.HasPrefix(line, "data: "):
			data.WriteString(strings.TrimPrefix(line, "data: "))
			inBlock = true
		}
	}
	if inBlock {
		return ev, []byte(data.String()), true
	}
	return "", nil, false
}
