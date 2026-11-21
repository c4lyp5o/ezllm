package bridgeutil

import (
	"encoding/json"
	"net/http"
)

// ChatChunks encodes an OpenAI chat.completion.chunk SSE stream onto a Stream.
// It is shared by the OpenAI-shaped bridges (Codex, Copilot, Antigravity).
// The adapter still decides when Begin runs (headers are delayed until the
// first delta so a failure before any output keeps its real HTTP status), what
// each delta carries, and the finish/usage ordering.
type ChatChunks struct {
	ID      string
	Created int64
	Model   string // may change before Begin; Chunk and Usage read it per event
	// RoleChunk opens the stream with {"role":"assistant","content":""}
	// (Codex, Antigravity). Without it Begin only flushes the headers (Copilot).
	RoleChunk bool

	w       http.ResponseWriter
	stream  *Stream
	started bool
}

// NewChatChunks binds an encoder to a response and its stream transport.
func NewChatChunks(w http.ResponseWriter, stream *Stream, id string, created int64, model string) *ChatChunks {
	return &ChatChunks{ID: id, Created: created, Model: model, w: w, stream: stream}
}

// Started reports whether headers (status 200) have been written.
func (c *ChatChunks) Started() bool { return c.started }

// Begin writes the SSE headers and status 200 once.
func (c *ChatChunks) Begin() {
	if c.started {
		return
	}
	c.started = true
	h := c.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // reverse proxies must not buffer the stream
	c.w.WriteHeader(http.StatusOK)
	if c.RoleChunk {
		_ = c.Chunk(map[string]any{"role": "assistant", "content": ""}, "")
		return
	}
	_ = c.stream.Flush()
}

// Event writes one data: line. A json.RawMessage is written as is.
func (c *ChatChunks) Event(v any) error {
	b, ok := v.(json.RawMessage)
	if !ok {
		var err error
		if b, err = json.Marshal(v); err != nil {
			return err
		}
	}
	_, err := c.stream.Write([]byte("data: " + string(b) + "\n\n"))
	return err
}

// Chunk writes a single-choice chunk; an empty finish encodes null.
func (c *ChatChunks) Chunk(delta map[string]any, finish string) error {
	var fr any
	if finish != "" {
		fr = finish
	}
	return c.Event(map[string]any{
		"id": c.ID, "object": "chat.completion.chunk", "created": c.Created, "model": c.Model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": fr}},
	})
}

// Usage writes the trailing usage chunk (empty choices) of include_usage.
func (c *ChatChunks) Usage(usage any) error {
	return c.Event(map[string]any{
		"id": c.ID, "object": "chat.completion.chunk", "created": c.Created, "model": c.Model,
		"choices": []any{}, "usage": usage,
	})
}

// Done writes the [DONE] terminator. After a failed write it writes nothing.
func (c *ChatChunks) Done() {
	_, _ = c.stream.Write([]byte("data: [DONE]\n\n"))
}
