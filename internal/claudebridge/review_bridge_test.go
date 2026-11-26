package claudebridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A CLI that dies without a result frame is a 502 carrying its stderr, not a
// 200 with an empty message.
func TestRunWithoutResultFrameIsAnError(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat >/dev/null\necho 'error while loading shared libraries' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Bin: bin, ConfigDir: dir, Timeout: 10 * time.Second}
	req := Request{Model: "claude-haiku-5-5", MaxTokens: 64, Messages: []Message{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
	_, err := r.Run(context.Background(), req, "haiku", nil)
	var ne *NativeError
	if !errors.As(err, &ne) || ne.Status != 502 {
		t.Fatalf("err = %v, want a 502 NativeError", err)
	}
	if !strings.Contains(ne.Message, "shared libraries") {
		t.Fatalf("message %q should carry the CLI's stderr", ne.Message)
	}
}

// The cache_control breakpoint a client moves off the first message after
// turn 1 must not change the conversation's session id.
func TestConversationSessionIDIgnoresCacheControl(t *testing.T) {
	turn1 := Request{Messages: []Message{{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}]`)}}}
	turn2 := Request{Messages: []Message{
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"hello"}]`)},
		{Role: "assistant", Content: json.RawMessage(`"hi"`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"more","cache_control":{"type":"ephemeral"}}]`)},
	}}
	if a, b := ConversationSessionID("sys", turn1), ConversationSessionID("sys", turn2); a != b {
		t.Fatalf("session id changed between turns: %s vs %s", a, b)
	}
	other := Request{Messages: []Message{{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"different"}]`)}}}
	if ConversationSessionID("sys", turn1) == ConversationSessionID("sys", other) {
		t.Fatal("different conversations share a session id")
	}
}

// A turn ended by a stop sequence names it in the non-streaming reply.
func TestMessageCarriesStopSequence(t *testing.T) {
	sw := &StreamWriter{}
	ev, _ := json.Marshal(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "stop_sequence", "stop_sequence": "END"}, "usage": map[string]any{"output_tokens": 3}})
	if err := sw.Handle(Frame{Type: "stream_event", Event: ev}); err != nil {
		t.Fatal(err)
	}
	if got := sw.Message("m")["stop_sequence"]; got != "END" {
		t.Fatalf("stop_sequence = %v, want END", got)
	}
}
