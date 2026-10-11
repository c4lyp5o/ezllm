package translate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// sseReader converts an OpenAI chat-completions SSE stream into an
// Anthropic messages SSE stream on the fly. It is a pure io.Reader
// wrapper: no goroutines, no whole-stream buffering — bytes are
// emitted as soon as the upstream yields them, so ttft stays honest.
//
// Event order (Claude Code's parser is strict about this):
//
//	ping -> message_start -> content_block_start/delta... ->
//	content_block_stop (each open block, in open order) ->
//	message_delta (stop_reason + usage) -> message_stop
//
// An upstream error frame or a transport cut mid-stream becomes an
// `event: error` frame followed by EOF — never a bare TCP close.
type sseReader struct {
	scanner *bufio.Scanner
	out     bytes.Buffer // pending translated bytes
	err     error        // sticky scanner error
	done    bool         // message_stop (or error frame) already emitted

	started      bool // message_start emitted
	finishSeen   bool // upstream sent a finish_reason
	usageSeen    bool // upstream sent a usage chunk
	blocksClosed bool // content_block_stop frames emitted
	clientModel  string
	msgID        string
	finishReason string

	openBlocks []int          // anthropic block indices, in open order
	blockKind  map[int]string // anthropic index -> "text" | "tool_use"
	tcIndex    map[int]int    // openai tool_call index -> anthropic block index
	nextIndex  int            // next anthropic block index to allocate
	pingSent   bool

	inTokens  int
	outTokens int
}

// NewSSEReader wraps an OpenAI SSE response body and returns a reader
// that yields the equivalent Anthropic SSE stream. clientModel is the
// model name the client asked for (message_start carries it verbatim).
func NewSSEReader(r io.Reader, clientModel string) io.Reader {
	sc := bufio.NewScanner(r)
	// Tool-call argument fragments can be large; give the scanner room.
	sc.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	return &sseReader{
		scanner:     sc,
		clientModel: clientModel,
		blockKind:   map[int]string{},
		tcIndex:     map[int]int{},
	}
}

func (s *sseReader) Read(p []byte) (int, error) {
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

// advance consumes one upstream SSE event and writes any translated
// frames to s.out. It returns false when the source is exhausted
// (clean EOF, [DONE], error frame, or transport failure).
func (s *sseReader) advance() bool {
	// Emit one ping before the first real event (matches real Anthropic).
	if !s.started && s.out.Len() == 0 && !s.pingSent {
		s.frame("ping", `{"type":"ping"}`)
		s.pingSent = true
		return true
	}

	line, ok := s.nextDataLine()
	if !ok {
		s.finish()
		return false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" {
		return true // keepalive comment or blank separator
	}
	if payload == "[DONE]" {
		s.finish()
		return false
	}

	var chunk openaiChunk
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		// Undecodable frame: never kill the stream for one bad chunk.
		slog.Warn("translate: skipping undecodable upstream SSE frame", "error", err)
		return true
	}
	if chunk.Error != nil {
		s.emitError("api_error", chunk.Error.Message)
		return false
	}

	// Usage-only chunk (include_usage): the final chunk before [DONE].
	if len(chunk.Choices) == 0 {
		if chunk.Usage != nil {
			s.inTokens = chunk.Usage.PromptTokens
			s.outTokens = chunk.Usage.CompletionTokens
			s.usageSeen = true
			s.emitFinalIfReady()
		}
		return true
	}

	if !s.started {
		s.startMessage(chunk.ID)
	}

	delta := chunk.Choices[0].Delta
	if delta.Content != "" {
		idx := s.ensureTextBlock()
		s.frame("content_block_delta", fmt.Sprintf(
			`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`,
			idx, mustMarshalString(delta.Content)))
	}
	for _, tc := range delta.ToolCalls {
		s.toolCallDelta(tc)
	}
	if fr := chunk.Choices[0].FinishReason; fr != nil && *fr != "" {
		s.closeBlocks()
		s.finishSeen = true
		s.finishReason = *fr
		s.emitFinalIfReady()
	}
	return true
}

// --- emission helpers -------------------------------------------------

func (s *sseReader) startMessage(upstreamID string) {
	s.started = true
	id := upstreamID
	if !strings.HasPrefix(id, "msg_") {
		id = "msg_" + id
	}
	s.msgID = id
	msg := fmt.Sprintf(
		`{"type":"message","id":%s,"role":"assistant","content":[],"model":%s,"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":1}}`,
		mustMarshalString(id), mustMarshalString(s.clientModel))
	s.frame("message_start", `{"type":"message_start","message":`+msg+`}`)
}

// ensureTextBlock returns the index of an open text block, opening a
// new one only when no text block is currently open.
func (s *sseReader) ensureTextBlock() int {
	if len(s.openBlocks) > 0 {
		last := s.openBlocks[len(s.openBlocks)-1]
		if s.blockKind[last] == "text" {
			return last
		}
	}
	idx := s.nextIndex
	s.nextIndex++
	s.openBlocks = append(s.openBlocks, idx)
	s.blockKind[idx] = "text"
	s.frame("content_block_start", fmt.Sprintf(
		`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, idx))
	return idx
}

func (s *sseReader) toolCallDelta(tc openaiToolDelta) {
	idx, seen := s.tcIndex[tc.Index]
	if !seen {
		idx = s.nextIndex
		s.nextIndex++
		s.tcIndex[tc.Index] = idx
		s.openBlocks = append(s.openBlocks, idx)
		s.blockKind[idx] = "tool_use"
		s.frame("content_block_start", fmt.Sprintf(
			`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%s,"name":%s,"input":{}}}`,
			idx, mustMarshalString(tc.ID), mustMarshalString(tc.Function.Name)))
	}
	if tc.Function != nil && tc.Function.Arguments != "" {
		s.frame("content_block_delta", fmt.Sprintf(
			`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`,
			idx, mustMarshalString(tc.Function.Arguments)))
	}
}

func (s *sseReader) closeBlocks() {
	if s.blocksClosed {
		return
	}
	s.blocksClosed = true
	for _, idx := range s.openBlocks {
		s.frame("content_block_stop", fmt.Sprintf(
			`{"type":"content_block_stop","index":%d}`, idx))
	}
}

// emitFinalIfReady closes the message once both the finish_reason and
// (when the upstream honors include_usage) the usage chunk have arrived.
func (s *sseReader) emitFinalIfReady() {
	if s.done || !s.finishSeen || !s.usageSeen {
		return
	}
	s.closeBlocks()
	s.frame("message_delta", fmt.Sprintf(
		`{"type":"message_delta","delta":{"stop_reason":%s,"stop_sequence":null},"usage":{"input_tokens":%d,"output_tokens":%d}}`,
		mustMarshalString(mapStopReason(s.finishReason)), s.inTokens, s.outTokens))
	s.frame("message_stop", `{"type":"message_stop"}`)
	s.done = true
}

// finish runs at source exhaustion. With include_usage honored the
// message was already closed; otherwise close it honestly: end_turn
// with real usage when we have it, end_turn with zeros plus one log
// line when we never saw a usage chunk (locked decision Q5).
func (s *sseReader) finish() {
	if s.done {
		return
	}
	if !s.finishSeen && !s.started {
		// The upstream produced no events at all — surface it, never hang.
		s.emitError("api_error", "upstream stream ended before any event")
		return
	}
	if !s.usageSeen {
		slog.Warn("translate: upstream stream ended without a usage chunk; recording zero tokens",
			"model", s.clientModel, "finish_reason", s.finishReason)
	}
	if !s.finishSeen {
		s.finishReason = "stop" // stream cut without finish_reason; treat as completed
	}
	s.closeBlocks()
	s.finishSeen = true
	s.usageSeen = true // emit with whatever we have (possibly zeros)
	s.emitFinalIfReady()
	s.done = true
}

// emitError converts an upstream error into an Anthropic error event,
// then ends the stream. Never a bare TCP close.
func (s *sseReader) emitError(errType, message string) {
	s.frame("error", fmt.Sprintf(
		`{"type":"error","error":{"type":%s,"message":%s}}`,
		mustMarshalString(errType), mustMarshalString(message)))
	s.done = true
}

func (s *sseReader) frame(event, data string) {
	s.out.WriteString("event: ")
	s.out.WriteString(event)
	s.out.WriteString("\ndata: ")
	s.out.WriteString(data)
	s.out.WriteString("\n\n")
}

// nextDataLine returns the next `data:` line, skipping comments and
// event-name lines. ok is false at clean EOF or on a transport error
// (which is converted into an error frame, not propagated).
func (s *sseReader) nextDataLine() (string, bool) {
	for s.scanner.Scan() {
		line := s.scanner.Text()
		if line == "" || strings.HasPrefix(line, ":") ||
			strings.HasPrefix(line, "event:") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			return line, true
		}
	}
	if err := s.scanner.Err(); err != nil {
		s.emitError("api_error", "upstream stream failed: "+err.Error())
	}
	return "", false
}

// mustMarshalString JSON-encodes s; it cannot fail for a string.
func mustMarshalString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// --- upstream chunk decode types (OpenAI streaming shape) -------------

type openaiChunk struct {
	ID      string         `json:"id"`
	Choices []openaiChoice `json:"choices"`
	Usage   *OpenAIUsage   `json:"usage,omitempty"`
	Error   *openaiError   `json:"error,omitempty"`
}

type openaiChoice struct {
	Index        int         `json:"index"`
	Delta        openaiDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type openaiDelta struct {
	Role      string            `json:"role,omitempty"`
	Content   string            `json:"content,omitempty"`
	ToolCalls []openaiToolDelta `json:"tool_calls,omitempty"`
}

type openaiToolDelta struct {
	Index    int              `json:"index"`
	ID       string           `json:"id,omitempty"`
	Function *openaiFuncDelta `json:"function,omitempty"`
}

type openaiFuncDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type openaiError struct {
	Message string `json:"message"`
}
