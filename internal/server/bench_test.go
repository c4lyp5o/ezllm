package server

// Phase-separated benchmarks for the server half of the request path. The
// phases are kept apart on purpose: a single end-to-end number cannot say
// whether a regression lives in body ingestion, in routing, or in the upstream
// round trip, and those three have completely different owners.
//
//	phase 1  BenchmarkIngestBody        server.go:464 — read + size-limit the body
//	phase 2  BenchmarkInspectAndRoute   server.go:474 extractModel + router.ResolveCandidates
//	e2e      BenchmarkEndToEnd          the whole handler over a real loopback socket
//
// The network half (ForwardCandidates, streamCopy) lives in
// internal/proxy/bench_test.go, and every one of these runs against the same
// deterministic upstream (internal/proxy/benchutil).
//
// Every benchmark is run with -benchmem; the report quotes allocs/op.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/jsonbody"
	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/proxy/benchutil"
	"github.com/c4lyp5o/ezllm/internal/router"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// payloadSizes are the three request-body sizes every phase is measured at.
var payloadSizes = []struct {
	name  string
	bytes int
}{
	{"1KiB", 1 << 10},
	{"64KiB", 64 << 10},
	{"1MiB", 1 << 20},
}

// benchServerModel is the namespaced id the harness's single account serves.
const benchServerModel = "up/gpt-6-luna"

// benchCombo is a one-hop combo created for the routing benchmark.
const benchCombo = "benchcombo"

// ── phase 1: request-body ingestion ─────────────────────────────────────────

// BenchmarkIngestBody measures the ingestion primitive itself — readBody, the
// helper both inference read sites use, against the cap a real Server
// computes from Options.MaxBodyMiB (production default 32 MiB, so none of
// these payloads is refused). `known` carries a Content-Length (the common
// case: preallocated read); `chunked` does not (the capped ReadAll path).
//
// Scope, stated honestly: this isolates the read + cap, not the socket read.
// httptest.NewRequest is hoisted out of the loop so the number is not
// dominated by the constructor, and the net/http read loop is covered by
// BenchmarkEndToEnd instead.
func BenchmarkIngestBody(b *testing.B) {
	s := New(Options{}) // no DB: New only computes the cap and builds the mux
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)

	for _, p := range payloadSizes {
		body := benchutil.ChatBody(benchServerModel, p.bytes)
		for _, mode := range []string{"known", "chunked"} {
			b.Run(mode+"/"+p.name, func(b *testing.B) {
				b.SetBytes(int64(len(body)))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					r.Body = io.NopCloser(bytes.NewReader(body))
					r.ContentLength = int64(len(body))
					if mode == "chunked" {
						r.ContentLength = -1
					}
					got, tooLarge, err := readBody(r, s.maxBody)
					if err != nil || tooLarge {
						b.Fatal(err, tooLarge)
					}
					if len(got) != len(body) {
						b.Fatalf("read %d of %d bytes", len(got), len(body))
					}
				}
			})
		}
	}
}

// ── phase 2: inspection and routing ─────────────────────────────────────────

// BenchmarkInspectAndRoute measures the two inspection steps against the real
// store and the real router, with no upstream involved: extractModel over the
// parsed body, then ResolveCandidates (namespace → account → model existence →
// wire/pin selection → key pick → rules eligibility, or combo → per-hop gates).
//
// `direct` is the ns/model path; `combo` is a bare combo name, which pays the
// per-hop gates as well.
func BenchmarkInspectAndRoute(b *testing.B) {
	mock := benchutil.New(benchutil.Options{})
	defer mock.Close()
	h := newHarnessAt(b, mock.Server())
	ctx := context.Background()

	acct, err := h.db.AccountByNamespace(ctx, "up")
	if err != nil {
		b.Fatal(err)
	}
	if _, err := h.db.UpsertCombo(ctx, store.Combo{
		Name: benchCombo, Strategy: "failover", Enabled: true,
		Hops: []store.ComboHop{{AccountID: acct.ID, ModelID: "gpt-6-luna", Weight: 1}},
	}); err != nil {
		b.Fatal(err)
	}
	resolver := router.New(h.db)

	for _, p := range payloadSizes {
		for _, target := range []string{benchServerModel, benchCombo} {
			name := "direct"
			if target == benchCombo {
				name = "combo"
			}
			// Each case routes ITS target: a shared direct-model body once
			// made the "combo" case measure direct routing.
			body := benchutil.ChatBody(target, p.bytes)
			b.Run(name+"/"+p.name, func(b *testing.B) {
				b.SetBytes(int64(len(body)))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					model, err := extractModel(body)
					if err != nil {
						b.Fatal(err)
					}
					routes, err := resolver.ResolveCandidates(ctx, model, provider.SurfaceOpenAI, "bench")
					if err != nil {
						b.Fatal(err)
					}
					if len(routes) != 1 {
						b.Fatalf("routes = %d, want 1", len(routes))
					}
					if routes[0].Alias != target {
						b.Fatalf("route alias = %q, want %q (the case must route its own target)", routes[0].Alias, target)
					}
				}
			})
		}
	}
}

// BenchmarkShapeCapture hashes the reusable history of an agent-loop body
// (HistoryChatBody): the per-request canonicalization cost that the
// single-final-message fixtures above never reach.
func BenchmarkShapeCapture(b *testing.B) {
	for _, p := range payloadSizes {
		body := benchutil.HistoryChatBody(benchServerModel, p.bytes, 40)
		doc := jsonbody.Open(body)
		b.Run(p.name, func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if sh := captureShapeDoc(doc, provider.SurfaceOpenAI); sh.PrefixSHA == "" {
					b.Fatal("no prefix hash")
				}
			}
		})
	}
}

// ── end to end (the whole handler, real socket) ─────────────────────────────

// BenchmarkEndToEnd drives the complete gateway over a real loopback socket:
// HTTP parse → middleware → auth (SQLite) → ingestion → inspection → routing →
// upstream dial (mock) → stream copy / body write → ledger enqueue.
//
// It is the only number that includes the net/http read loop on both sides and
// the client's own read of the final byte. Its purpose in the report is as the
// reconciliation figure: phases 1–4 plus the ledger should add up to it.
//
// Request payload varies (1 KiB/64 KiB/1 MiB); the mock's response is fixed at
// ~4 KiB (non-stream) or 32 chunks × 64 B (stream) so the request size is the
// only moving part.
func BenchmarkEndToEnd(b *testing.B) {
	mock := benchutil.New(benchutil.Options{
		ResponseBytes: 4096,
		Chunks:        32,
		ChunkBytes:    64,
	})
	defer mock.Close()

	client := benchutil.NewClient()
	h := newHarnessAt(b, mock.Server(), func(o *Options) {
		o.MaxBodyMiB = 32 // production default
		o.Dispatcher = proxy.NewDispatcher(client, provider.NewRegistry(client))
		quiet(o)
	})
	gw := httptest.NewServer(h.handler)
	defer gw.Close()

	for _, p := range payloadSizes {
		plain := benchutil.ChatBody(benchServerModel, p.bytes)
		stream := benchutil.StreamChatBody(benchServerModel, p.bytes)
		for _, variant := range []struct {
			name string
			body []byte
		}{
			{"nonstream", plain},
			{"stream", stream},
		} {
			for _, conc := range concurrencyLevels() {
				b.Run(fmt.Sprintf("%s/%s/%s", variant.name, p.name, conc.name), func(b *testing.B) {
					body := variant.body
					b.SetBytes(int64(len(body)))
					b.ReportAllocs()
					// Warm the connection pool so the first measured iteration
					// does not pay a handshake the later ones do not.
					warmupEndToEnd(b, client, gw.URL, body)
					b.ResetTimer()
					conc.run(b, func() {
						if err := oneEndToEnd(client, gw.URL, body); err != nil {
							b.Error(err)
						}
					})
				})
			}
		}
	}
}

func warmupEndToEnd(b *testing.B, client *http.Client, base string, body []byte) {
	b.Helper()
	for i := 0; i < 8; i++ {
		if err := oneEndToEnd(client, base, body); err != nil {
			b.Fatalf("warmup: %v", err)
		}
	}
}

// oneEndToEnd posts body to the gateway and drains the response, which is what
// makes the measurement cover "client send → final byte received" rather than
// "headers received".
func oneEndToEnd(client *http.Client, base string, body []byte) error {
	req, err := http.NewRequest("POST", base+"/v1/chat/completions", bytes.NewReader(body))
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

// ── concurrency levels ──────────────────────────────────────────────────────

// concurrency is one documented parallelism setting for b.RunParallel.
//
// goroutines = GOMAXPROCS * parallelism, which is what b.SetParallelism means.
// The two settings below are GOMAXPROCS (= saturated cores, no oversubscription)
// and 4*GOMAXPROCS (= 16 goroutines on this 4-core box, i.e. deliberately
// oversubscribed so queueing shows up).
type concurrency struct {
	name        string
	parallelism int
}

func concurrencyLevels() []concurrency {
	return []concurrency{
		{"seq", 0},
		{fmt.Sprintf("par%d", gomaxprocs()), 1},
		{fmt.Sprintf("par%d", 4*gomaxprocs()), 4},
	}
}

// run executes fn once for the sequential level and via b.RunParallel
// otherwise, so every benchmark reports the same three settings.
func (c concurrency) run(b *testing.B, fn func()) {
	if c.parallelism == 0 {
		for i := 0; i < b.N; i++ {
			fn()
		}
		return
	}
	b.SetParallelism(c.parallelism)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			fn()
		}
	})
}

func gomaxprocs() int { return runtime.GOMAXPROCS(0) }

// quiet points the server's logger at io.Discard. The access log runs on every
// request and writing it to stderr would both slow the measurement down and
// bury the results.
func quiet(o *Options) {
	o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
}

// benchCPU names the CPU the numbers were taken on, so a later reader can tell
// whether a comparison is apples-to-apples.
func benchCPU() string {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return "unknown"
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "model name") {
			if i := strings.Index(line, ":"); i >= 0 {
				return strings.TrimSpace(line[i+1:])
			}
		}
	}
	return "unknown"
}
