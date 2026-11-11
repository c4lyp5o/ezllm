package agybridge

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
	f := &fakeExec{}
	s := &Server{Runner: f, Token: "tok", Models: ParseModels(measuredModels)}
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gemini-3.8-flash-low","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer tok")
	w := &failedStreamWriter{ResponseRecorder: httptest.NewRecorder()}
	s.Handler().ServeHTTP(w, req)
	if f.calls != 1 || w.writes != 1 {
		t.Fatalf("runner=%d writes=%d", f.calls, w.writes)
	}
	if len(w.deadlines) < 2 || w.deadlines[0].IsZero() || !w.deadlines[len(w.deadlines)-1].IsZero() {
		t.Fatalf("deadlines=%v", w.deadlines)
	}
}
