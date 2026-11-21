package codexbridge

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// With the only slot busy and the queue full, a request is refused at once
// with 429 queue_full instead of waiting out the runner timeout.
func TestRunnerRefusesWhenTheQueueIsFull(t *testing.T) {
	r := &Runner{Bin: os.Args[0], Timeout: 5 * time.Second, MaxConcurrent: 1, MaxQueue: 1}
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
	start := time.Now()
	_, err = r.Run(context.Background(), simpleRequest(), nil)
	var ne *NativeError
	if !errors.As(err, &ne) || ne.Status != 429 || ne.Code != "queue_full" {
		t.Fatalf("err = %v, want 429 queue_full", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("queue_full waited instead of refusing at once")
	}
	rec := httptest.NewRecorder()
	(&Server{}).writeErr(rec, err)
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "5" {
		t.Fatalf("http = %d Retry-After=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

// The 401 uses OpenAI's error type like the other OpenAI-shaped bridges
// (it was authentication_error before the shared auth check).
func TestRefusalUsesOpenAIErrorType(t *testing.T) {
	rec := do(t, testServer(&fakeExec{}), "POST", "/v1/chat/completions", hiBody, "wrong")
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), `"type":"invalid_request_error"`) {
		t.Fatalf("401 = %d %s", rec.Code, rec.Body.String())
	}
}

func TestStatusReportsAdmission(t *testing.T) {
	r := &Runner{MaxConcurrent: 2, MaxQueue: 3}
	s := &Server{Runner: r, Token: "secret", Info: map[string]any{"status": "ok"}}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if !strings.Contains(rec.Body.String(), `"max_concurrent":2`) || !strings.Contains(rec.Body.String(), `"max_queue":3`) {
		t.Fatalf("/status = %s", rec.Body.String())
	}
}
