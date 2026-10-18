package claudeweb

import (
	"context"
	"io"
	"strings"
	"testing"
)

// TestCancelOnCloseKeepsBodyAlive pins the fix for the streaming killer: the
// per-request timeout must NOT cancel when Fetch returns, because Generate
// reads the SSE body afterwards. Before the fix the deferred cancel fired on
// return and every read failed with context.Canceled — which also masked
// upstream 429s as transport errors.
func TestCancelOnCloseKeepsBodyAlive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	body := &cancelOnClose{ReadCloser: pr, cancel: cancel}

	go func() {
		_, _ = pw.Write([]byte("data: hello\n\n"))
		_ = pw.Close()
	}()

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read after Fetch returned: %v", err)
	}
	if !strings.Contains(string(got), "hello") {
		t.Fatalf("body = %q, want the streamed data", got)
	}
	if ctx.Err() != nil {
		t.Fatal("request context canceled before Body.Close — this kills streaming")
	}

	if err := body.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("request context still alive after Body.Close — timeout leak")
	}
}
