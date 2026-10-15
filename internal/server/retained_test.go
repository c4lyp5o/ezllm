package server

// Retained-memory measurement: after N sequential and N concurrent requests
// through the full proxy path, force a GC and report the HeapAlloc / HeapInuse
// deltas against a post-warm-up baseline.
//
// METHOD, stated so the numbers can be judged:
//
//   - Each measurement gets its OWN harness (store, server, mock, client),
//     created inside a subtest so the previous measurement's database and
//     listener are closed before the next baseline is taken. Sharing one
//     harness across runs would make every delta include the previous run.
//   - The baseline is taken AFTER a 50-request warm-up, so connection pools,
//     the ledger's batch buffer and the SQLite page cache are already
//     populated. What is measured is growth, not first-touch allocation.
//   - The ledger is flushed (db.Flush()) and the GC run twice before each
//     reading, so queued rows and finalizer-deferred objects are not counted as
//     retained.
//   - The WHOLE measurement is run twice, in one test, so the noise floor is
//     visible: if run 2 disagrees with run 1 by more than 2x, the harness says
//     so rather than presenting one number as if it were reproducible.
//   - The SQLite ledger is part of the measured path, so its growth is included
//     by design; it is the largest legitimate retainer here.

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/proxy/benchutil"
)

// retainedRequests is the request count per measurement.
const retainedRequests = 500

// retainedWarmup is issued before the baseline is taken.
const retainedWarmup = 50

type retainedResult struct {
	heapAlloc   int64
	heapInuse   int64
	heapObjects int64
}

// TestRetainedMemory is skippable via -short.
func TestRetainedMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("retained-memory measurement: skipped under -short (it runs 2200 requests across 4 harnesses)")
	}

	type key struct {
		run  int
		mode string
	}
	results := map[key]retainedResult{}

	for run := 1; run <= 2; run++ {
		for _, mode := range []struct {
			name    string
			workers int
		}{
			{"sequential", 1},
			{"concurrent", runtime.GOMAXPROCS(0)},
		} {
			k := key{run, mode.name}
			t.Run(fmt.Sprintf("run%d/%s", run, mode.name), func(t *testing.T) {
				r := measureRetained(t, retainedRequests, mode.workers)
				results[k] = r
				t.Logf("n=%d workers=%d | heap_alloc delta=%+d B (%.1f B/request) | "+
					"heap_inuse delta=%+d B (%.1f B/request) | heap_objects delta=%+d",
					retainedRequests, mode.workers,
					r.heapAlloc, float64(r.heapAlloc)/float64(retainedRequests),
					r.heapInuse, float64(r.heapInuse)/float64(retainedRequests),
					r.heapObjects)
			})
		}
	}

	// The noise floor, made explicit.
	for _, mode := range []string{"sequential", "concurrent"} {
		a, b := results[key{1, mode}], results[key{2, mode}]
		t.Logf("noise floor %s: run1 heap_inuse=%+d B run2 heap_inuse=%+d B ratio=%.2fx",
			mode, a.heapInuse, b.heapInuse, ratio(a.heapInuse, b.heapInuse))
		if ratio(a.heapInuse, b.heapInuse) > 2 {
			t.Logf("WARNING: the two runs disagree by more than 2x for %s — the retained-memory "+
				"figure is dominated by noise at this request count and must not be read as a "+
				"per-request retention rate", mode)
		}
	}
}

// ratio is |a|/|b| (or its inverse, whichever is >= 1), guarding against a zero
// denominator.
func ratio(a, b int64) float64 {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	if a == 0 || b == 0 {
		return 1
	}
	if a < b {
		a, b = b, a
	}
	return float64(a) / float64(b)
}

func measureRetained(t *testing.T, n, workers int) retainedResult {
	t.Helper()

	mock := benchutil.New(benchutil.Options{ResponseBytes: 4096})
	defer mock.Close()
	client := benchutil.NewClient()
	h := newHarnessAt(t, mock.Server(), func(o *Options) {
		o.MaxBodyMiB = 32
		o.Dispatcher = proxy.NewDispatcher(client, provider.NewRegistry(client))
		quiet(o)
	})
	gw := httptest.NewServer(h.handler)
	defer gw.Close()

	body := benchutil.ChatBody(benchServerModel, 1<<10)
	do := func() error {
		req, err := http.NewRequest("POST", gw.URL+"/v1/chat/completions", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer tok-infer")
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("status %d", resp.StatusCode)
		}
		return nil
	}

	for i := 0; i < retainedWarmup; i++ {
		if err := do(); err != nil {
			t.Fatalf("warmup: %v", err)
		}
	}
	base := settledStats()

	if workers <= 1 {
		for i := 0; i < n; i++ {
			if err := do(); err != nil {
				t.Fatalf("request %d: %v", i, err)
			}
		}
	} else {
		var wg sync.WaitGroup
		work := make(chan int, n)
		for i := 0; i < n; i++ {
			work <- i
		}
		close(work)
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range work {
					if err := do(); err != nil {
						t.Errorf("concurrent request: %v", err)
						return
					}
				}
			}()
		}
		wg.Wait()
	}

	// Let the async ledger drain so queued rows are not counted as retained.
	if err := h.db.Flush(); err != nil {
		t.Fatalf("ledger flush: %v", err)
	}
	after := settledStats()

	return retainedResult{
		heapAlloc:   int64(after.HeapAlloc) - int64(base.HeapAlloc),
		heapInuse:   int64(after.HeapInuse) - int64(base.HeapInuse),
		heapObjects: int64(after.HeapObjects) - int64(base.HeapObjects),
	}
}

// settledStats runs the collector twice before reading: one pass can leave
// finalizer-deferred objects behind, and a single GC would report them as
// retained.
func settledStats() runtime.MemStats {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms
}
