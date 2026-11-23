package copilotbridge

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/bridgeutil"
)

// failingWriter stands in for a client connection that broke mid-stream.
type failingWriter struct {
	*httptest.ResponseRecorder
	writes int
}

func (f *failingWriter) Write(p []byte) (int, error) {
	f.writes++
	return 0, errors.New("client connection broken")
}

// A failed stream write reaches the caller instead of being swallowed, and is
// sticky: nothing, including [DONE], is written after it.
func TestStreamWriteFailurePropagatesAndIsSticky(t *testing.T) {
	w := &failingWriter{ResponseRecorder: httptest.NewRecorder()}
	transport := bridgeutil.NewStream(context.Background(), w, bridgeutil.StreamWriteStall)
	defer transport.Close()
	sw := newSSEWriter(w, transport, "id", 0, "m")
	if err := sw.delta("hello"); err == nil {
		t.Fatal("broken client write was swallowed")
	}
	before := w.writes
	sw.Done()
	if w.writes != before {
		t.Fatalf("wrote %d more times after the stream failed", w.writes-before)
	}
}

// Copilot streams pass reverse proxies unbuffered like the other bridges:
// before the shared encoder it never sent X-Accel-Buffering.
func TestStreamDisablesProxyBuffering(t *testing.T) {
	rec := httptest.NewRecorder()
	transport := bridgeutil.NewStream(context.Background(), rec, bridgeutil.StreamWriteStall)
	defer transport.Close()
	if err := newSSEWriter(rec, transport, "id", 0, "m").delta("hi"); err != nil {
		t.Fatal(err)
	}
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering = %q, want no", got)
	}
	if strings.Contains(rec.Body.String(), `"role"`) {
		t.Fatalf("copilot stream gained a role chunk: %s", rec.Body.String())
	}
}
