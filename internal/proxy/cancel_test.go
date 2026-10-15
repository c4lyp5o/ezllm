package proxy

// Context propagation to the upstream.
//
// The server-level cancellation tests (internal/server/lifecycle_test.go) prove
// the end-to-end behaviour, but at that level two mechanisms can cancel an
// upstream request: the request context, and the connection being torn down
// when commit's `defer resp.Body.Close()` runs. This test isolates the first
// one.
//
// It does so by removing the second: the client writer never blocks, so the
// stream copier keeps draining the upstream and nothing closes the upstream
// response body while the request is in flight. The ONLY thing that can cancel
// the mock's request context is the context handed to ForwardCandidates. With
// `attempt` building its upstream request from context.Background() instead,
// the mock streams its full 8 seconds and this test fails on the deadline.

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy/benchutil"
)

func TestForwardCandidatesCancelsUpstreamWhenContextIsCanceled(t *testing.T) {
	// 4000 chunks, 2ms apart: ~8s of stream, far longer than the deadline below,
	// so a request that is NOT canceled cannot accidentally finish in time.
	mock := benchutil.New(benchutil.Options{Chunks: 4000, ChunkBytes: 256, Delay: 2 * time.Millisecond})
	defer mock.Close()

	client := benchutil.NewClient()
	d := NewDispatcher(client, provider.NewRegistry(client))
	rt := testRoute(mock.URL(), provider.SurfaceOpenAI)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := benchutil.StreamChatBody(benchModel, 1<<10)
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))

	done := make(chan error, 1)
	go func() {
		_, _, err := d.ForwardCandidates(ctx, newDiscardWriter(), req, []Route{rt}, body)
		done <- err
	}()

	select {
	case <-mock.WroteFirst():
	case <-time.After(5 * time.Second):
		t.Fatal("mock never started streaming")
	}
	if n := mock.CancelCount(); n != 0 {
		t.Fatalf("fixture: upstream already canceled (%d) before the test canceled", n)
	}

	cancel()

	select {
	case <-mock.Canceled():
	case <-time.After(5 * time.Second):
		t.Fatalf("the upstream request context was NOT canceled within 5s of the caller canceling "+
			"(mock wrote %d bytes and is still streaming) — the dispatcher did not propagate the context",
			mock.BytesWritten())
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ForwardCandidates did not unwind after the cancel")
	}
}

// The mirror image: a request that runs to completion must never be reported as
// canceled, or the assertion above would be satisfied by a signal that always
// fires.
func TestForwardCandidatesDoesNotReportCancelOnSuccess(t *testing.T) {
	mock := benchutil.New(benchutil.Options{Chunks: 8, ChunkBytes: 64, ResponseBytes: 512})
	defer mock.Close()

	client := benchutil.NewClient()
	d := NewDispatcher(client, provider.NewRegistry(client))
	rt := testRoute(mock.URL(), provider.SurfaceOpenAI)

	for _, body := range [][]byte{
		benchutil.StreamChatBody(benchModel, 1<<10),
		benchutil.ChatBody(benchModel, 1<<10),
	} {
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		if _, _, err := d.ForwardCandidates(context.Background(), newDiscardWriter(), req, []Route{rt}, body); err != nil {
			t.Fatalf("ForwardCandidates: %v", err)
		}
	}
	if n := mock.CancelCount(); n != 0 {
		t.Errorf("CancelCount = %d after two successful requests, want 0", n)
	}
}
