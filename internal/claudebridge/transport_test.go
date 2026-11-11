package claudebridge

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type failedStreamWriter struct {
	*httptest.ResponseRecorder
	writes    int
	deadlines []time.Time
}

func (w *failedStreamWriter) Write([]byte) (int, error) { w.writes++; return 0, io.ErrClosedPipe }
func (w *failedStreamWriter) SetWriteDeadline(d time.Time) error {
	w.deadlines = append(w.deadlines, d)
	return nil
}
func TestStreamUsesSharedDeadlinesAndStopsAfterWriteFailure(t *testing.T) {
	f := &fakeExec{frames: []Frame{frame(t, "stream_event", map[string]any{"type": "message_start", "message": map[string]any{"id": "m1", "model": "claude-sonnet-5-5"}})}}
	s := testServer(f)
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5-5","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "secret")
	w := &failedStreamWriter{ResponseRecorder: httptest.NewRecorder()}
	s.Handler().ServeHTTP(w, req)
	if !f.sawSink || w.writes != 1 {
		t.Fatalf("sink=%v writes=%d", f.sawSink, w.writes)
	}
	if len(w.deadlines) < 2 || w.deadlines[0].IsZero() || !w.deadlines[len(w.deadlines)-1].IsZero() {
		t.Fatalf("deadlines=%v", w.deadlines)
	}
}
