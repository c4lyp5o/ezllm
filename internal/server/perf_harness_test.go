package server

// Per-request latency percentile harness.
//
// A benchmark's ns/op is a MEAN over a batch: it cannot show a tail, and it is
// explicitly not a percentile. This harness instead issues a fixed number of
// real requests over a real loopback socket through the real gateway handler
// and reports p50/p95/p99 of the per-request wall durations it recorded.
//
// Run with -v (the table is emitted with t.Logf) and without -short:
//
//	go test ./internal/server/ -run TestLatencyPercentiles -v -count=1
//	go test ./internal/server/ -run TestLatencyPercentiles -v -count=1 -short   # skipped
//
// What each interval contains is stated next to the numbers it produces,
// because "p99 latency" without an interval definition is not a measurement.

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/proxy/benchutil"
)

// perfRequests is the fixed sample count per mode.
const perfRequests = 2000

// perfWarmup is issued before timing starts, so every connection in the pool is
// established and every server-side lazy init has run.
const perfWarmup = 50

// sample is one request's observation.
type sample struct {
	total time.Duration
	ttft  time.Duration // zero for a non-streaming request
}

// TestLatencyPercentiles is the harness. It is skippable via -short.
func TestLatencyPercentiles(t *testing.T) {
	if testing.Short() {
		t.Skip("latency percentile harness: skipped under -short (it issues 4000 real requests)")
	}

	mock := benchutil.New(benchutil.Options{
		ResponseBytes: 4096,
		Chunks:        32,
		ChunkBytes:    64,
	})
	defer mock.Close()

	client := benchutil.NewClient()
	h := newHarnessAt(t, mock.Server(), func(o *Options) {
		o.MaxBodyMiB = 32
		o.Dispatcher = proxy.NewDispatcher(client, provider.NewRegistry(client))
		quiet(o)
	})
	gw := httptest.NewServer(h.handler)
	defer gw.Close()

	streamBody := benchutil.StreamChatBody(benchServerModel, 1<<10)
	plainBody := benchutil.ChatBody(benchServerModel, 1<<10)
	// 64 KiB variants: the task requires percentiles on representative LARGE
	// bodies too, not only the small one (bench ns/op is not a percentile).
	streamBody64 := benchutil.StreamChatBody(benchServerModel, 64<<10)
	plainBody64 := benchutil.ChatBody(benchServerModel, 64<<10)

	// Warm-up: establish the pooled connections and let the ledger writer start.
	for i := 0; i < perfWarmup; i++ {
		if _, err := observeRequest(client, gw.URL, streamBody, true); err != nil {
			t.Fatalf("warmup: %v", err)
		}
	}

	report := func(name string, s []sample) {
		totals := make([]time.Duration, len(s))
		ttfts := make([]time.Duration, 0, len(s))
		for i, x := range s {
			totals[i] = x.total
			if x.ttft > 0 {
				ttfts = append(ttfts, x.ttft)
			}
		}
		sort.Slice(totals, func(i, j int) bool { return totals[i] < totals[j] })
		sort.Slice(ttfts, func(i, j int) bool { return ttfts[i] < ttfts[j] })

		t.Logf("── %s ── n=%d  (nearest-rank percentiles over %d samples)", name, len(s), len(s))
		logPercentiles(t, "total", totals)
		if len(ttfts) > 0 {
			logPercentiles(t, "ttft ", ttfts)
		}
	}

	run := func(name string, concurrency int, body []byte, stream bool) []sample {
		s := make([]sample, perfRequests)
		if concurrency <= 1 {
			for i := 0; i < perfRequests; i++ {
				x, err := observeRequest(client, gw.URL, body, stream)
				if err != nil {
					t.Fatalf("%s request %d: %v", name, i, err)
				}
				s[i] = x
			}
			return s
		}
		var wg sync.WaitGroup
		next := make(chan int, perfRequests)
		for i := 0; i < perfRequests; i++ {
			next <- i
		}
		close(next)
		for w := 0; w < concurrency; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range next {
					x, err := observeRequest(client, gw.URL, body, stream)
					if err != nil {
						t.Errorf("%s request %d: %v", name, i, err)
						return
					}
					s[i] = x
				}
			}()
		}
		wg.Wait()
		return s
	}

	// Interval definitions, repeated in the report:
	//
	//   total  t0 = immediately before client.Do(req); t1 = the instant the
	//          client has read the LAST byte of the response body. Includes the
	//          loopback write of the request, the gateway's HTTP parse,
	//          middleware (access log → recoverer → CORS), the SQLite token
	//          lookup, body read + size cap, model extraction, router
	//          resolution (SQLite), compression-profile resolution, request
	//          shape capture, the upstream dial to the mock, the mock's
	//          response, the gateway's stream copy / body write, and the
	//          client's own read of the final byte. Excludes TCP connect
	//          (warm-up pooled it), DNS and TLS (loopback, plaintext).
	//
	//   ttft   t0 as above; t1 = the instant the client's first Read on
	//          resp.Body returned data. Includes everything above up to the
	//          mock's first flushed SSE frame plus the gateway's WriteHeader +
	//          Flush and the loopback delivery of that frame. Excludes the rest
	//          of the stream. Streaming requests only.
	report("stream, sequential", run("stream-seq", 1, streamBody, true))
	report(fmt.Sprintf("stream, %d concurrent", runtime.GOMAXPROCS(0)),
		run("stream-par", runtime.GOMAXPROCS(0), streamBody, true))
	report("non-stream, sequential", run("plain-seq", 1, plainBody, false))
	report(fmt.Sprintf("non-stream, %d concurrent", runtime.GOMAXPROCS(0)),
		run("plain-par", runtime.GOMAXPROCS(0), plainBody, false))
	report("stream 64KiB, sequential", run("stream64-seq", 1, streamBody64, true))
	report("non-stream 64KiB, sequential", run("plain64-seq", 1, plainBody64, false))

	t.Logf("hardware: %s | GOMAXPROCS=%d | mock: 32 SSE events x 64B / 4KiB JSON body",
		benchCPU(), runtime.GOMAXPROCS(0))
}

// observeRequest performs one timed request. stream selects whether a TTFT is
// recorded: a non-streaming response has no "first SSE byte" to measure.
func observeRequest(client *http.Client, base string, body []byte, stream bool) (sample, error) {
	req, err := http.NewRequest("POST", base+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return sample{}, err
	}
	req.Header.Set("Authorization", "Bearer tok-infer")
	req.Header.Set("Content-Type", "application/json")

	t0 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return sample{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return sample{}, fmt.Errorf("status %d", resp.StatusCode)
	}

	var out sample
	buf := make([]byte, 4096)
	if stream {
		n, rerr := resp.Body.Read(buf)
		out.ttft = time.Since(t0)
		if rerr != nil && rerr != io.EOF {
			return sample{}, rerr
		}
		if n == 0 {
			return sample{}, fmt.Errorf("stream produced no first byte")
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			return sample{}, err
		}
	} else {
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			return sample{}, err
		}
	}
	out.total = time.Since(t0)
	return out, nil
}

// logPercentiles prints the nearest-rank p50/p95/p99 of a sorted sample slice.
// Nearest rank: the p-th percentile is the element at index ceil(p/100 * n) - 1,
// which is always an actually-observed duration rather than an interpolation.
func logPercentiles(t *testing.T, label string, sorted []time.Duration) {
	t.Helper()
	n := len(sorted)
	if n == 0 {
		t.Logf("  %s: no samples", label)
		return
	}
	at := func(p float64) time.Duration {
		idx := int(float64(n)*p+0.999999) - 1 // ceil, without importing math
		if idx < 0 {
			idx = 0
		}
		if idx >= n {
			idx = n - 1
		}
		return sorted[idx]
	}
	t.Logf("  %s  p50=%-12s p95=%-12s p99=%-12s min=%-12s max=%-12s mean=%s",
		label,
		ms(at(0.50)), ms(at(0.95)), ms(at(0.99)),
		ms(sorted[0]), ms(sorted[n-1]), ms(mean(sorted)))
}

func mean(s []time.Duration) time.Duration {
	var sum time.Duration
	for _, d := range s {
		sum += d
	}
	return sum / time.Duration(len(s))
}

func ms(d time.Duration) string {
	return fmt.Sprintf("%.3fms", float64(d.Microseconds())/1000)
}
