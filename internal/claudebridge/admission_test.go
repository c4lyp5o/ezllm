package claudebridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"
)

// With the only slot busy and the queue full, a request is refused at once
// with 429 queue_full instead of waiting out the runner timeout.
func TestRunnerRefusesWhenTheQueueIsFull(t *testing.T) {
	r := &Runner{Bin: "/nonexistent", Timeout: 5 * time.Second, MaxConcurrent: 1, MaxQueue: 1}
	release, err := r.slots().Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	waitCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = r.slots().Acquire(waitCtx) }()
	for r.slots().Queued() == 0 {
		time.Sleep(time.Millisecond)
	}
	req := Request{Model: "claude-sonnet", MaxTokens: 16, Messages: []Message{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
	start := time.Now()
	_, err = r.Run(context.Background(), req, "sonnet", nil)
	var ne *NativeError
	if !errors.As(err, &ne) || ne.Status != 429 || ne.Code != "queue_full" {
		t.Fatalf("err = %v, want 429 queue_full", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("queue_full waited instead of refusing at once")
	}
	if r.AdmissionStats()["rejected_queue_full"] != int64(1) {
		t.Fatalf("stats = %v", r.AdmissionStats())
	}
	rec := httptest.NewRecorder()
	(&Server{}).writeRunError(rec, Result{}, err)
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "5" {
		t.Fatalf("http = %d Retry-After=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
}
