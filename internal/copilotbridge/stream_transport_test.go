package copilotbridge

import (
	"context"
	"errors"
	"net/http/httptest"
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
	sw := &sseWriter{w: w, id: "id", model: "m", transport: transport}
	if err := sw.delta("hello"); err == nil {
		t.Fatal("broken client write was swallowed")
	}
	before := w.writes
	sw.done()
	if w.writes != before {
		t.Fatalf("wrote %d more times after the stream failed", w.writes-before)
	}
}
