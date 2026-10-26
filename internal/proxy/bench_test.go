package proxy

// Phase-separated benchmarks for the network half of the request path, all
// against the deterministic mock in internal/proxy/benchutil.
//
//	phase 3  BenchmarkUpstreamRoundTrip  ForwardCandidates: prepareHopBody + dial + copy
//	phase 4  BenchmarkStreamCopy         the SSE forwarding loop alone (ReadSlice + tap + write + burst flush)
//
// The server half (ingestion, inspection/routing) lives in
// internal/server/bench_test.go. Every benchmark here runs with -benchmem.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy/benchutil"
)

var proxyPayloadSizes = []struct {
	name  string
	bytes int
}{
	{"1KiB", 1 << 10},
	{"64KiB", 64 << 10},
	{"1MiB", 1 << 20},
}

const benchModel = "resolved-model"

// ── phase 3: the upstream round trip ────────────────────────────────────────

// BenchmarkUpstreamRoundTrip measures ForwardCandidates end to end against the
// mock: prepareHopBody (the model swap, a full JSON round trip whose cost grows
// with the request body), the dial, the upstream's response, and commit's copy
// to the client writer.
//
// The client is warmed before b.ResetTimer so keep-alive connections are
// already pooled; one mock serves every iteration.
func BenchmarkUpstreamRoundTrip(b *testing.B) {
	mock := benchutil.New(benchutil.Options{
		ResponseBytes: 4096, // non-stream response
		Chunks:        32,   // stream response
		ChunkBytes:    64,
	})
	defer mock.Close()

	client := benchutil.NewClient()
	d := NewDispatcher(client, provider.NewRegistry(client))
	rt := testRoute(mock.URL(), provider.SurfaceChat)

	for _, p := range proxyPayloadSizes {
		plain := benchutil.ChatBody(benchModel, p.bytes)
		stream := benchutil.StreamChatBody(benchModel, p.bytes)
		for _, v := range []struct {
			name string
			body []byte
		}{
			{"nonstream", plain},
			{"stream", stream},
		} {
			for _, c := range proxyConcurrency() {
				b.Run(fmt.Sprintf("%s/%s/%s", v.name, p.name, c.name), func(b *testing.B) {
					body := v.body
					b.SetBytes(int64(len(body)))
					b.ReportAllocs()
					warmupRoundTrip(b, d, rt, body)
					b.ResetTimer()
					c.run(b, newDiscardWriter, func(w *discardWriter) {
						req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
						if _, _, err := d.ForwardCandidates(context.Background(), w, req, []Route{rt}, body); err != nil {
							b.Error(err)
						}
					})
				})
			}
		}
	}
}

func warmupRoundTrip(b *testing.B, d *Dispatcher, rt Route, body []byte) {
	b.Helper()
	w := newDiscardWriter()
	for i := 0; i < 8; i++ {
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		w.reset()
		if _, _, err := d.ForwardCandidates(context.Background(), w, req, []Route{rt}, body); err != nil {
			b.Fatalf("warmup: %v", err)
		}
	}
}

// ── phase 4: stream forwarding and flushing ─────────────────────────────────

// BenchmarkStreamCopy measures streamCopy alone: ReadSlice over the SSE byte
// stream, the usage tap, each line written back verbatim, and one Flush per
// burst (when nothing more is buffered), not per line. The mock's own
// precomputed frames are the payload, so the bytes under test are exactly
// what a mock request would emit.
//
// The ResponseWriter is a no-op sink whose Flush is a no-op, which isolates the
// ROUTER's per-event work from the socket's — phase 3 and the end-to-end
// benchmark measure the socket. `frag` sets how many bytes one upstream Read
// returns, which is what a fragmented frame looks like to the reader.
func BenchmarkStreamCopy(b *testing.B) {
	d := NewDispatcher(benchutil.NewClient(), provider.NewRegistry(benchutil.NewClient()))
	for _, events := range []int{32, 256} {
		mock := benchutil.NewHandler(benchutil.Options{Chunks: events, ChunkBytes: 64})
		payload := mock.StreamPayload()
		for _, frag := range []struct {
			name string
			read int
		}{
			{"whole", 0}, // one Read hands the reader everything available
			{"8B", 8},    // frames split mid-`data: {...}` every 8 bytes
		} {
			for _, c := range proxyConcurrency() {
				b.Run(fmt.Sprintf("events=%d/frag=%s/%s", events, frag.name, c.name), func(b *testing.B) {
					b.SetBytes(int64(len(payload)))
					b.ReportAllocs()
					b.ResetTimer()
					c.run(b, newDiscardWriter, func(w *discardWriter) {
						resp := streamResponse(payload, frag.read)
						_, _, _, err := d.streamCopy(context.Background(), w, resp, provider.SurfaceChat, provider.UsageConventionAuto, time.Now())
						if err != nil {
							b.Error(err)
						}
					})
				})
			}
		}
	}
}

func streamResponse(payload []byte, readSize int) *http.Response {
	var body io.Reader = bytes.NewReader(payload)
	if readSize > 0 {
		body = &smallReadReader{r: bytes.NewReader(payload), n: readSize}
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(body),
	}
}

// smallReadReader hands out at most n bytes per Read, which is how a fragmented
// SSE frame arrives off a real socket.
type smallReadReader struct {
	r *bytes.Reader
	n int
}

func (s *smallReadReader) Read(p []byte) (int, error) {
	if len(p) > s.n {
		p = p[:s.n]
	}
	return s.r.Read(p)
}

// ── shared bench plumbing ───────────────────────────────────────────────────

// discardWriter is a ResponseWriter that costs nothing: Write reports success
// and Flush is a no-op. It exists so a benchmark measures the router rather
// than httptest.ResponseRecorder's buffer growth, which would otherwise
// dominate every streaming iteration.
type discardWriter struct {
	h http.Header
}

func newDiscardWriter() *discardWriter { return &discardWriter{h: make(http.Header, 8)} }

func (w *discardWriter) Header() http.Header         { return w.h }
func (w *discardWriter) WriteHeader(int)             {}
func (w *discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *discardWriter) Flush()                      {}

func (w *discardWriter) reset() {
	for k := range w.h {
		delete(w.h, k)
	}
}

type proxyConcurrencyLevel struct {
	name        string
	parallelism int
}

func proxyConcurrency() []proxyConcurrencyLevel {
	return []proxyConcurrencyLevel{
		{"seq", 0},
		{fmt.Sprintf("par%d", runtime.GOMAXPROCS(0)), 1},
		{fmt.Sprintf("par%d", 4*runtime.GOMAXPROCS(0)), 4},
	}
}

// run executes fn once per iteration, handing it a writer private to this
// goroutine: b.RunParallel calls fn concurrently, so a writer captured from the
// enclosing scope would be a data race (and -race would find it).
func (c proxyConcurrencyLevel) run(b *testing.B, newWriter func() *discardWriter, fn func(*discardWriter)) {
	if c.parallelism == 0 {
		w := newWriter()
		for i := 0; i < b.N; i++ {
			w.reset()
			fn(w)
		}
		return
	}
	b.SetParallelism(c.parallelism)
	b.RunParallel(func(pb *testing.PB) {
		w := newWriter()
		for pb.Next() {
			w.reset()
			fn(w)
		}
	})
}
