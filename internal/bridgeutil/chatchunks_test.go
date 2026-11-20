package bridgeutil

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

const chunkHead = `{"choices":[{"delta":`

// The bytes match what the per-bridge writers produced before the move:
// sorted keys, null finish_reason until the end, a usage chunk with empty
// choices, and [DONE].
func TestChatChunksWireBytes(t *testing.T) {
	for _, role := range []bool{true, false} {
		rec := httptest.NewRecorder()
		s := NewStream(context.Background(), rec, StreamWriteStall)
		c := NewChatChunks(rec, s, "id", 7, "m")
		c.RoleChunk = role
		if c.Started() {
			t.Fatal("started before Begin")
		}
		c.Begin()
		c.Begin() // idempotent
		_ = c.Chunk(map[string]any{"content": "hi"}, "")
		c.Model = "served"
		_ = c.Chunk(map[string]any{}, "stop")
		_ = c.Usage(map[string]int{"total_tokens": 3})
		_ = c.Event(json.RawMessage(`{"error":{}}`))
		c.Done()
		s.Close()

		want := ""
		if role {
			want += `data: ` + chunkHead + `{"content":"","role":"assistant"},"finish_reason":null,"index":0}],"created":7,"id":"id","model":"m","object":"chat.completion.chunk"}` + "\n\n"
		}
		want += `data: ` + chunkHead + `{"content":"hi"},"finish_reason":null,"index":0}],"created":7,"id":"id","model":"m","object":"chat.completion.chunk"}` + "\n\n" +
			`data: ` + chunkHead + `{},"finish_reason":"stop","index":0}],"created":7,"id":"id","model":"served","object":"chat.completion.chunk"}` + "\n\n" +
			`data: {"choices":[],"created":7,"id":"id","model":"served","object":"chat.completion.chunk","usage":{"total_tokens":3}}` + "\n\n" +
			`data: {"error":{}}` + "\n\n" +
			"data: [DONE]\n\n"
		if got := rec.Body.String(); got != want {
			t.Fatalf("role=%v body:\n%s\nwant:\n%s", role, got, want)
		}
		if !c.Started() || rec.Code != 200 {
			t.Fatalf("role=%v started=%v code=%d", role, c.Started(), rec.Code)
		}
	}
}

func TestChatChunksHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	s := NewStream(context.Background(), rec, StreamWriteStall)
	defer s.Close()
	NewChatChunks(rec, s, "id", 0, "m").Begin()
	for k, v := range map[string]string{"Content-Type": "text/event-stream", "Cache-Control": "no-cache", "X-Accel-Buffering": "no"} {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if !rec.Flushed {
		t.Error("Begin without a role chunk must flush the headers")
	}
}
