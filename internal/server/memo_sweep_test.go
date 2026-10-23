package server

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/compress"
)

// An idle gateway must still drop expired memo entries (prompt-derived text):
// the server sweeps on a timer instead of waiting for the next request.
func TestServerSweepsIdleMemoEntries(t *testing.T) {
	defer func(d time.Duration) { memoSweepEvery = d }(memoSweepEvery)
	memoSweepEvery = 5 * time.Millisecond
	var clock atomic.Int64
	clock.Store(time.Unix(1700000000, 0).UnixNano())
	memo := compress.NewSessionMemo(&compress.SessionMemoOptions{TTL: time.Minute,
		Now: func() time.Time { return time.Unix(0, clock.Load()) }})
	memo.Put("s", "p", map[string]any{"role": "user", "content": "PRIVATE_PROMPT"}, map[string]any{"role": "user", "content": "shipped"})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {}, func(o *Options) { o.SessionMemo = memo })
	defer h.server.StopBackground(time.Second)

	time.Sleep(50 * time.Millisecond)
	if memo.Len() != 1 {
		t.Fatalf("a live entry was swept: len=%d", memo.Len())
	}
	clock.Add(int64(2 * time.Minute))
	deadline := time.Now().Add(2 * time.Second)
	for memo.Len() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("idle expired entry still resident; nothing swept it")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
