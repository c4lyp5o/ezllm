package server

// Slow-consumer behaviour: what the gateway does when the client reads far
// slower than the upstream produces, or stops reading altogether.
//
// Three questions, and the answers are reported as measurements rather than
// asserted away:
//
//	1. does the router buffer unboundedly?  -> mock.BytesWritten() vs the payload
//	2. does backpressure reach the upstream? -> the same counter, at the plateau
//	3. is there a write timeout on the ResponseWriter path? -> the stream still
//	   completes after the stall
//
// A found resource bound would be a deliverable, not something to paper over,
// so the assertions here are deliberately loose and the numbers are logged.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/proxy/benchutil"
)

// TestSlowConsumerBackpressure stalls the client mid-stream and measures what
// that does to the pipeline. There is no bare Sleep used as synchronization:
// the stall is detected by the mock's write counter going flat, and the stall
// window IS the measurement window.
func TestSlowConsumerBackpressure(t *testing.T) {
	const chunks = 4000
	const chunkBytes = 4096
	mock := benchutil.New(benchutil.Options{Chunks: chunks, ChunkBytes: chunkBytes})
	defer mock.Close()
	payload := int64(len(mock.StreamPayload()))

	client := benchutil.NewClient()
	h := newHarnessAt(t, mock.Server(), func(o *Options) {
		o.MaxBodyMiB = 32
		o.Dispatcher = proxy.NewDispatcher(client, provider.NewRegistry(client))
		quiet(o)
	})
	gw := httptest.NewServer(h.handler)
	defer gw.Close()

	req, err := http.NewRequest("POST", gw.URL+"/v1/chat/completions",
		bytes.NewReader(benchutil.StreamChatBody(benchServerModel, 1<<10)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok-infer")
	req.Header.Set("Content-Type", "application/json")

	baseHeap, baseRSS := gcAndRead()

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("streaming request: %v", err)
	}
	defer resp.Body.Close()

	// Read just enough to prove the stream started, then stop reading entirely.
	// This is the slow consumer: the socket receive buffer fills, the gateway's
	// Write blocks, and the upstream's Write should block in turn.
	read := 0
	buf := make([]byte, 4096)
	for i := 0; i < 2; i++ {
		n, rerr := resp.Body.Read(buf)
		if rerr != nil {
			t.Fatalf("client read %d: %v", i, rerr)
		}
		read += n
	}

	stalled, plateaued := waitPlateau(mock, 3, 25*time.Millisecond, 10*time.Second)
	heapDuring, rssDuring := gcAndRead()

	// Resume: drain the rest and confirm the stream was not corrupted or cut.
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		t.Fatalf("draining after the stall: %v", err)
	}
	delivered := int64(read) + n

	t.Logf("payload=%d B (%d chunks x %d B) | client read before stall=%d B | mock wrote at plateau=%d B",
		payload, chunks, chunkBytes, read, stalled)
	t.Logf("bytes in flight at the stall = %d B (%.2f%% of the payload) | gateway heap delta during stall=%+d B | RSS delta=%+d B (noisy: GC returns pages to the OS)",
		stalled-int64(read), 100*float64(stalled-int64(read))/float64(payload),
		int64(heapDuring.HeapInuse)-int64(baseHeap.HeapInuse),
		int64(rssDuring)-int64(baseRSS))
	t.Logf("kernel socket buffers: tcp_wmem=%s tcp_rmem=%s (these, not the gateway, hold most of the in-flight bytes)",
		sysctl("net/ipv4/tcp_wmem"), sysctl("net/ipv4/tcp_rmem"))

	// 1 + 2: the pipeline must block, not buffer. If the mock wrote the whole
	// payload while the client read nothing, the gateway (or the kernel) had
	// absorbed 16 MiB and nothing applied backpressure.
	if !plateaued {
		t.Errorf("the mock wrote its ENTIRE %d B payload while the client was not reading "+
			"(wrote=%d) — nothing in the pipeline applied backpressure", payload, stalled)
	} else if stalled >= payload {
		t.Errorf("mock completed the payload (%d of %d B) before the stall was detected", stalled, payload)
	}

	// 3: no write timeout cut the stream. The production server sets none
	// (cmd/ezllm/main.go:226 — "No WriteTimeout: SSE streams are unbounded by
	// design"), and this asserts the observable consequence: the stalled
	// connection is still alive and delivers every byte.
	if delivered != payload {
		t.Errorf("after the stall the client received %d B of %d — the stream was truncated by a "+
			"write timeout or a broken connection", delivered, payload)
	}

	// The router's OWN buffering bound. Bytes in flight include the kernel
	// socket buffers on both hops, which the gateway neither owns nor controls;
	// what the gateway could get wrong is holding the stream in its heap, so
	// that is what is asserted.
	heapDelta := int64(heapDuring.HeapInuse) - int64(baseHeap.HeapInuse)
	if heapDelta > int64(payload)/8 {
		t.Errorf("the gateway's heap grew by %d B during the stall (payload is %d B) — it is holding "+
			"the stream rather than forwarding it", heapDelta, payload)
	}
}

// sysctl reads a /proc/sys value, returning "" when it is unavailable.
func sysctl(path string) string {
	b, err := os.ReadFile("/proc/sys/" + path)
	if err != nil {
		return "unavailable"
	}
	return strings.TrimSpace(string(b))
}

// waitPlateau polls the mock's write counter until it has not moved for
// `stable` consecutive intervals, and reports the value it settled at. The
// polling interval is the measurement method here (a stall is by definition a
// period of no progress), not a synchronization sleep standing in for a
// channel.
func waitPlateau(mock *benchutil.Mock, stable int, interval, deadline time.Duration) (int64, bool) {
	end := time.Now().Add(deadline)
	last := mock.BytesWritten()
	same := 0
	for time.Now().Before(end) {
		time.Sleep(interval)
		cur := mock.BytesWritten()
		if cur == last {
			same++
			if same >= stable {
				return cur, true
			}
			continue
		}
		same = 0
		last = cur
	}
	return last, false
}

// ── memory helpers ──────────────────────────────────────────────────────────

// gcAndRead forces a collection and returns the live-heap and RSS figures. The
// GC is what makes the number "retained" rather than "allocated so far".
func gcAndRead() (runtime.MemStats, uint64) {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms, rssBytes()
}

// rssBytes reads the process's resident set size from /proc/self/statm (field 2
// is resident pages). Linux-only, which is the platform this baseline was taken
// on; a 0 means the figure was unavailable and must not be reported as a
// measurement.
func rssBytes() uint64 {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0
	}
	pages, err := strconv.ParseUint(f[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * uint64(os.Getpagesize())
}
