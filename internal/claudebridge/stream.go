package claudebridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Frame is one native stream-json output line. Unknown fields are ignored so a CLI
// point release cannot break decoding.
type Frame struct {
	Type          string          `json:"type"`
	Subtype       string          `json:"subtype,omitempty"`
	Event         json.RawMessage `json:"event,omitempty"`
	Message       json.RawMessage `json:"message,omitempty"`
	Result        string          `json:"result,omitempty"`
	Usage         json.RawMessage `json:"usage,omitempty"`
	IsError       bool            `json:"is_error,omitempty"`
	Error         string          `json:"error,omitempty"`
	NumTurns      int             `json:"num_turns,omitempty"`
	TotalCostUSD  float64         `json:"total_cost_usd,omitempty"`
	RateLimitInfo json.RawMessage `json:"rate_limit_info,omitempty"`
	StopReason    string          `json:"stop_reason,omitempty"`
}

// ParseFrame decodes one stdout line.
func ParseFrame(line []byte) (Frame, error) {
	var f Frame
	if err := json.Unmarshal(line, &f); err != nil {
		return Frame{}, fmt.Errorf("claude-bridge: invalid native stream-json line: %s", truncateForError(line))
	}
	return f, nil
}

func truncateForError(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return fmt.Sprintf("%q", s)
}

// Usage is the Anthropic-shaped token envelope. Cache fields are separate from
// input_tokens exactly as Anthropic reports them (cache is excluded from
// input_tokens), so the ledger reads them by name with nothing synthesized.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

func parseUsage(raw json.RawMessage) Usage {
	var u Usage
	if len(raw) == 0 {
		return u
	}
	_ = json.Unmarshal(raw, &u)
	return u
}

// Overlay merges the fields present in raw onto u. Native splits its usage across
// message_start (input plus cache) and message_delta (output), so replacing instead
// of merging would drop the cache fields the ledger reads by name.
func (u *Usage) Overlay(raw json.RawMessage) {
	var partial struct {
		InputTokens              *int `json:"input_tokens"`
		OutputTokens             *int `json:"output_tokens"`
		CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &partial) != nil {
		return
	}
	if partial.InputTokens != nil {
		u.InputTokens = *partial.InputTokens
	}
	if partial.OutputTokens != nil {
		u.OutputTokens = *partial.OutputTokens
	}
	if partial.CacheCreationInputTokens != nil {
		u.CacheCreationInputTokens = *partial.CacheCreationInputTokens
	}
	if partial.CacheReadInputTokens != nil {
		u.CacheReadInputTokens = *partial.CacheReadInputTokens
	}
}

// StreamWriter turns native frames into an Anthropic stream, or assembles one
// complete message for a non-stream request. Streaming forwards native's own
// stream events verbatim — they already are Anthropic events — so there is no
// re-encoding of text and no second wire to keep in sync.
type StreamWriter struct {
	W     io.Writer
	Flush func()

	started bool
	stopped bool
	msgID   string
	model   string
	stop    string
	usage   Usage
	text    strings.Builder
	blocks  []map[string]any
}

// Handle consumes one frame. Frames that are not part of the stream (system,
// rate_limit_event, result bookkeeping) are absorbed and reported through fields.
//
// Only the first upstream message reaches the client. Measured on CLI 2.1.306: an
// empty first answer makes native append its own "[Your previous response had no
// visible output…]" user turn and generate a second message, and a tool call is
// followed by native's synthetic "No such tool available" tool_result. Neither is
// the client's conversation, so everything after the first message_stop is dropped.
func (s *StreamWriter) Handle(f Frame) error {
	switch f.Type {
	case "stream_event":
		if s.stopped {
			return nil
		}
		var ev map[string]any
		if err := json.Unmarshal(f.Event, &ev); err != nil {
			return fmt.Errorf("claude-bridge: malformed native event: %w", err)
		}
		kind, _ := ev["type"].(string)
		switch kind {
		case "message_start":
			s.started = true
			if m, ok := ev["message"].(map[string]any); ok {
				s.msgID, _ = m["id"].(string)
				s.model, _ = m["model"].(string)
				if raw, err := json.Marshal(m["usage"]); err == nil {
					s.usage.Overlay(raw)
				}
			}
		case "message_delta":
			if d, ok := ev["delta"].(map[string]any); ok {
				if sr, ok := d["stop_reason"].(string); ok {
					s.stop = sr
				}
			}
			if raw, err := json.Marshal(ev["usage"]); err == nil {
				s.usage.Overlay(raw)
			}
		case "message_stop":
			s.stopped = true
		case "content_block_delta":
			if d, ok := ev["delta"].(map[string]any); ok {
				if d["type"] == "text_delta" {
					if t, ok := d["text"].(string); ok {
						s.text.WriteString(t)
					}
				}
			}
		}
		return s.writeEvent(kind, f.Event)
	case "assistant":
		// Native emits one assistant frame per content block (measured: three
		// parallel tool calls arrive as three frames sharing one message id), so
		// blocks accumulate rather than the last frame replacing the rest.
		var msg struct {
			ID      string           `json:"id"`
			Content []map[string]any `json:"content"`
		}
		if err := json.Unmarshal(f.Message, &msg); err == nil && len(msg.Content) > 0 {
			if s.msgID != "" && msg.ID != "" && msg.ID != s.msgID {
				return nil
			}
			s.blocks = append(s.blocks, msg.Content...)
		}
	}
	return nil
}

func (s *StreamWriter) writeEvent(kind string, payload json.RawMessage) error {
	if s.W == nil {
		return nil
	}
	var buf bytes.Buffer
	buf.WriteString("event: ")
	buf.WriteString(kind)
	buf.WriteString("\ndata: ")
	buf.Write(payload)
	buf.WriteString("\n\n")
	if _, err := s.W.Write(buf.Bytes()); err != nil {
		return err
	}
	if s.Flush != nil {
		s.Flush()
	}
	return nil
}

// Finish closes an open stream. If native died before message_stop (a mid-stream
// error, a kill), the client still gets a well-formed terminal pair instead of a
// stream that simply stops.
func (s *StreamWriter) Finish() error {
	if s.stopped || !s.started {
		return nil
	}
	payload := map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": s.stopReasonOr("end_turn"), "stop_sequence": nil},
		"usage": s.usageMap(),
	}
	raw, _ := json.Marshal(payload)
	if err := s.writeEvent("message_delta", raw); err != nil {
		return err
	}
	rawStop, _ := json.Marshal(map[string]any{"type": "message_stop"})
	s.stopped = true
	return s.writeEvent("message_stop", rawStop)
}

func (s *StreamWriter) stopReasonOr(fallback string) string {
	if s.stop != "" {
		return s.stop
	}
	return fallback
}

// usageMap is the terminal usage an Anthropic client expects on message_delta,
// with the cache fields carried so the ledger's name-based reads work unchanged.
func (s *StreamWriter) usageMap() map[string]any {
	return map[string]any{
		"input_tokens":                s.usage.InputTokens,
		"output_tokens":               s.usage.OutputTokens,
		"cache_creation_input_tokens": s.usage.CacheCreationInputTokens,
		"cache_read_input_tokens":     s.usage.CacheReadInputTokens,
	}
}

// Message assembles the complete response for a non-stream request.
func (s *StreamWriter) Message(model string) map[string]any {
	content := s.blocks
	if content == nil {
		content = []map[string]any{{"type": "text", "text": s.text.String()}}
	}
	if s.model != "" {
		model = s.model
	}
	id := s.msgID
	if id == "" {
		id = "msg_claudebridge"
	}
	return map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   s.stopReasonOr("end_turn"),
		"stop_sequence": nil,
		"usage":         s.usageMap(),
	}
}
