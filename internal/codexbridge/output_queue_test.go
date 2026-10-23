package codexbridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// A stalled output consumer must not turn the reader into an unbounded spool.
// Keep the reader free to process cancellation and tear down the child.
func TestOutputQueueBoundsBytesAndEvents(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		n    int
	}{
		{"bytes", strings.Repeat("x", 32<<10), 100},
		{"events", "x", 10000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newTurnState(func(Delta) error { return nil })
			p, _ := json.Marshal(map[string]string{"itemId": "m", "delta": tc.text})
			for i := 0; i < tc.n; i++ {
				st.handle("item/agentMessage/delta", p)
			}
			if st.fail == nil || st.fail.Code != "output_backpressure" {
				t.Fatalf("queued %d deltas without a backlog refusal", len(st.out))
			}
			if !st.finished || len(st.out) != 0 {
				t.Fatalf("overflow kept output live: finished=%v queued=%d", st.finished, len(st.out))
			}
			if st.text.Len() > 1<<20 {
				t.Fatalf("buffered %d text bytes past the queue budget", st.text.Len())
			}
		})
	}
}

func TestOutputQueueOverflowReapsChildDuringBlockedDelivery(t *testing.T) {
	r, home := fakeRunner(t, "hang")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := newTurnState(nil)
	c, err := r.spawn(ctx, spawnOpts{}, st.handle, declineRequests)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	st.bindChild(c)
	_, _, pid, _ := readStarted(t, home)
	entered, release := make(chan struct{}), make(chan struct{})
	st.sink = func(Delta) error {
		close(entered)
		<-release
		return context.Canceled
	}
	st.handle("item/agentMessage/delta", json.RawMessage(`{"itemId":"m","delta":"start"}`))
	done := make(chan error, 1)
	go func() { _, _, err := r.segment(ctx, c, st); done <- err }()
	<-entered
	defer func() { close(release); <-done }()
	p, _ := json.Marshal(map[string]string{"itemId": "m", "delta": strings.Repeat("x", 32<<10)})
	for i := 0; i < 100; i++ {
		st.handle("item/agentMessage/delta", p)
	}
	waitGone(t, pid, 3*time.Second)
	st.mu.Lock()
	failure := st.fail
	st.mu.Unlock()
	if failure == nil || failure.Code != "output_backpressure" {
		t.Fatalf("overflow failure = %v", failure)
	}
}

func TestOutputQueueDrainsInOrderAndResetsBudget(t *testing.T) {
	var got strings.Builder
	st := newTurnState(func(d Delta) error { got.WriteString(d.Content); return nil })
	for _, text := range []string{"one", "two", "three"} {
		p, _ := json.Marshal(map[string]string{"itemId": "m", "delta": text})
		st.handle("item/agentMessage/delta", p)
	}
	st.handle("turn/completed", json.RawMessage(`{"turn":{"status":"completed"}}`))
	res, park, err := (&Runner{}).segment(context.Background(), &child{readDone: make(chan struct{})}, st)
	if err != nil || park || got.String() != "onetwothree" || res.Text != got.String() {
		t.Fatalf("delivery=%q result=%+v park=%v err=%v", got.String(), res, park, err)
	}
	if st.outBytes != 0 {
		t.Fatalf("drained queue still reserves %d bytes", st.outBytes)
	}
}

func TestOutputQueueOverflowReportsItsCauseAfterCancellation(t *testing.T) {
	st := newTurnState(nil)
	st.fail = &NativeError{Status: 502, Type: "api_error", Code: "output_backpressure", Message: "output backlog exceeded"}
	st.finished = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := (&Runner{}).segment(ctx, &child{readDone: make(chan struct{})}, st)
	var ne *NativeError
	if !errors.As(err, &ne) || ne.Code != "output_backpressure" {
		t.Fatalf("err=%v, want the backlog failure", err)
	}
}

func TestOutputQueueKeepsPartialTextOnUpstreamFailure(t *testing.T) {
	var got strings.Builder
	st := newTurnState(func(d Delta) error { got.WriteString(d.Content); return nil })
	st.handle("item/agentMessage/delta", json.RawMessage(`{"itemId":"m","delta":"partial answer"}`))
	failure := &NativeError{Status: 429, Type: "api_error", Code: "rate_limit", Message: "upstream stopped"}
	st.fail = failure
	st.handle("turn/completed", json.RawMessage(`{"turn":{"status":"failed"}}`))
	_, _, err := (&Runner{}).segment(context.Background(), &child{readDone: make(chan struct{})}, st)
	if err != failure || got.String() != "partial answer" {
		t.Fatalf("partial=%q err=%v, want the partial answer followed by the upstream error", got.String(), err)
	}
}
