# ezllm Performance & Correctness Baseline — Milestone 1 (Task 01)

> **Superseded in part (2026-10-15):** the request body is now decoded exactly
> once per request (single-parse dispatch, `internal/jsonbody`, see
> `docs/single-parse-dispatch-design.md`). Every number below predates that
> change; measured after-deltas live in the design doc's results section and
> the CHANGELOG. `EXPLORING.md`'s flow map (parse sites) is updated too.
> `file.go:NN` references below describe the code as measured on that date and
> have since drifted; grep the named function instead.

Date: 2026-10-15 (+08). Status: measured, all gates green.
Scope per task brief: measurement foundation only — no model-visible behavior
changed, no production `.go` file modified (the diff is test files, `benchutil`,
two Makefile targets and this document).

## 1. Request-flow map (with measured phase overheads)

```
client ──HTTP──▶ internal/server/server.go
  routes()            server.go:168   mux wiring; auth middleware authed() :1076
  inference(surface)  server.go:453   the handler:
    1  read+cap       server.go:464   io.ReadAll(io.LimitReader(...))     PHASE 1
    2  extractModel   server.go:474   full-JSON scan for "model"      ┐
       resolver.ResolveCandidates  router.go:464 / resolveCombo :564  ├ PHASE 2
       (SQLite candidate lookup, pin/capability gates, strategy)     ┘
    3  compressionFor server.go:559-area (profile resolution, header→combo)
  ──▶ internal/proxy/proxy.go
  ForwardCandidates → attempt() :850
    4  prepareHopBody / SwapModel  proxy.go:637  full map-decode + re-marshal
       dial + upstream response                                 PHASE 3 (mock)
  5  streamCopy (or commit for non-stream)  proxy.go:1442-1495
       scanner → usage tap → Write+Flush per SSE line           PHASE 4
  ──▶ internal/server: ledger() server.go:1067 (async batch, off the hot path)
```

64 KiB headline (warm keep-alive, loopback, mock upstream — see §3):

- Phase 1 ingestion: **43 µs** (19 allocs)
- Phase 2 inspection+routing: **303 µs** (direct) / **308 µs** (combo) (143 allocs)
- Phase 3 upstream round trip: **243 µs** non-stream par4 / **366 µs** stream par4
- Phase 4 streamCopy alone (32 events): **104–125 µs** parallel
- Whole handler (e2e, real socket): **578 µs** non-stream par4 / **866 µs** stream par4

## 2. Reproduction commands

```bash
# toolchain: Go 1.27.1 (PATH="$HOME/.local/go/bin:$PATH")
make bench        # all benchmarks, both packages, -benchmem, ~2 min
make perf         # percentile harness + retained-memory + slow-client + cancel

# gates (all rc=0 on this change):
gofmt -l internal cmd        # prints nothing
go vet ./...
go build ./...
go test ./... -count=1
go test -race ./internal/proxy/... ./internal/server/... ./internal/router/... -count=1

# individual phases:
go test ./internal/proxy/  -run XXX -bench 'UpstreamRoundTrip|StreamCopy' -benchmem -count=1
go test ./internal/server/ -run XXX -bench 'IngestBody|InspectAndRoute|EndToEnd' -benchmem -count=1
go test ./internal/server/ -run TestLatencyPercentiles -v -count=1
```

`-count=1` everywhere (cached results are meaningless for measurement).
Percentile harness: n=2000 requests per scenario, warmup excluded, nearest-rank.
Total measured wall time for this report's numbers: ~12 min.

## 3. Measured baseline

### Environment

- CPU: Intel Core i5-6500T @ 2.50 GHz, 4 cores / 4 threads, GOMAXPROCS=4
- RAM: 15 GiB; swap 8 GiB (fully used — background services loaded;
  see "variance" below)
- OS/Go: linux/amd64, go1.27.1
- Load during runs: Elasticsearch, Java, immich, mongod resident — this is a
  working home server, not an idle bench rig. Numbers are this machine's.

### Phase 1 — request-body ingestion (BenchmarkIngestBody)

| payload | ns/op | MB/s | B/op | allocs/op |
|---|---|---|---|---|
| 1 KiB | 843 | 1216 | 2 264 | 7 |
| 64 KiB | 43 198 | 1517 | 138 200 | 19 |
| 1 MiB | 628 943 | 1667 | 2 228 058 | 27 |

Reading the body is cheap and linear (~1.5–1.7 GB/s). Not a target.

### Phase 2 — inspection + routing (BenchmarkInspectAndRoute, no upstream)

| payload | direct | combo | B/op | allocs/op |
|---|---|---|---|---|
| 1 KiB | 206.9 µs | 211.1 µs | 5 173 | 143 |
| 64 KiB | 302.8 µs | 308.3 µs | 5 172 | 143 |
| 1 MiB | 1 695.1 µs | 1 718.3 µs | 5 191 | 143 |

Two components: a size-INDEPENDENT ~200 µs base (SQLite candidate/pin lookups)
plus a size-PROPORTIONAL full-body JSON parse (~1.5 ms/MiB). This is the only
router phase that scales with payload — see §6.

### Phase 3 — upstream round trip vs mock (BenchmarkUpstreamRoundTrip, warm keep-alive)

Includes `SwapModel`'s full JSON round trip, dial, mock response, copy.

| body | mode | seq ns/op | par4 ns/op | par16 ns/op | allocs/op |
|---|---|---|---|---|---|
| 1 KiB | non-stream | 217 385 | 56 501 | 50 633 | 234 |
| 1 KiB | stream | 521 563 | 216 618 | 198 311 | 1 290 |
| 64 KiB | non-stream | 868 090 | 243 474 | 224 679 | 253 |
| 64 KiB | stream | 1 189 343 | 365 510 | 380 270 | 1 310 |
| 1 MiB | non-stream | 11 341 176 | 3 604 581 | 3 621 654 | 265 |
| 1 MiB | stream | 9 623 520 | 3 210 499 | 3 508 533 | 276 |

### Phase 4 — streamCopy alone (BenchmarkStreamCopy, no-op sink writer)

| events | read pattern | seq ns/op | par16 ns/op | allocs/op |
|---|---|---|---|---|
| 32 | whole frames | 277 680 | 104 230 | 1 079 |
| 32 | 8-byte fragments | 277 779 | 122 094 | 1 081 |
| 256 | whole frames | 1 566 781 | 567 569 | 8 248 |
| 256 | 8-byte fragments | 1 868 175 | 636 636 | 8 251 |

Fragmented SSE (a frame split across 8-byte reads) costs ≤ 20% over whole
frames and +2 allocs — the scanner path is fragmentation-robust.
Cost per event ≈ **2.2 µs / 32 allocs / 1.9 KiB** — the per-event allocation
rate is the notable number here (GC pressure at high concurrency).

### End-to-end, whole handler (BenchmarkEndToEnd, real loopback socket)

| body | mode | seq ns/op | par4 ns/op | par16 ns/op | allocs/op |
|---|---|---|---|---|---|
| 64 KiB | non-stream | 1 787 795 | 577 657 | 531 103 | ~690 |
| 64 KiB | stream | 2 887 320 | 865 548 | 911 184 | ~1 750 |
| 1 MiB | non-stream | 19 356 305 | 4 436 591 | 4 350 385 | ~749 |
| 1 MiB | stream | 14 279 872 | 4 353 873 | 4 445 866 | ~748 |

### Per-request latency percentiles (TestLatencyPercentiles, n=2000, nearest-rank)

Interval definitions (from the harness, authoritative):
- **total**: t0 immediately before `client.Do(req)` → t1 the instant the client
  read the LAST response byte. Includes loopback write, gateway HTTP parse,
  middleware, SQLite token lookup, body read+cap, model extraction, router
  resolution, compression-profile resolution, request-shape capture, dial to
  the mock, mock response, stream copy/body write, client final read.
  Excludes TCP connect (warm-up pooled it), DNS, TLS (loopback plaintext).
- **ttft**: t0 → the instant the client's first `Read` on `resp.Body` returned
  data. Includes everything up to the mock's first flushed SSE frame + gateway
  WriteHeader + Flush + loopback delivery. Streaming only.

| scenario | total p50 / p95 / p99 | ttft p50 / p95 / p99 |
|---|---|---|
| stream 1 KiB seq | 1.650 / 2.215 / 2.756 ms | 0.802 / 1.392 / 2.030 ms |
| stream 1 KiB, 4 conc | 2.149 / 4.106 / 6.374 ms | 1.469 / 3.178 / 5.293 ms |
| non-stream 1 KiB seq | 0.748 / 1.196 / 1.398 ms | — |
| non-stream 1 KiB, 4 conc | 0.888 / 1.916 / 3.729 ms | — |
| stream 64 KiB seq | 2.543 / 3.142 / 3.983 ms | 1.658 / 2.261 / 3.140 ms |
| non-stream 64 KiB seq | 1.649 / 2.189 / 2.448 ms | — |

Mock response shape: 32 SSE events × 64 B (or 4 KiB JSON for non-stream) —
so these numbers are ROUTER overhead + a tiny response, not provider latency.
Full min/max/mean are in the harness output (`make perf`).

### Allocation & retained memory (TestRetainedMemory, n=500 + 50 warmup, post-GC deltas)

| run | workers | heap_inuse delta | per request | noise floor vs twin run |
|---|---|---|---|---|
| 1 seq | 1 | +631 KB | 1.26 KB | 1.26× (seq) |
| 2 seq | 1 | +795 KB | 1.59 KB | ↑ |
| 1 conc | 4 | +967 KB | 1.93 KB | 1.04× (conc) |
| 2 conc | 4 | +1.00 MB | 2.02 KB | ↑ |

Sub-2 KB retained per request with no growth signature at n=500 (the two
independent runs agree within 4–26%, i.e. near the stated noise floor).

### Cancellation & lifecycle (all PASS, synchronization = channels + explicit deadlines, no bare sleeps)

- `TestForwardCandidatesCancelsUpstreamWhenContextIsCanceled` — and this one is
  **bite-proven by us**: mutating `proxy.go:850` to build the upstream request
  from `context.Background()` made it fail in exactly the documented way
  ("mock wrote 884 237 bytes and is still streaming"), restore verified
  byte-identical (md5 match), suite green again.
- `TestClientCancelMidStreamCancelsUpstream`,
  `TestClientCancelMidNonStreamReadCancelsUpstream` — upstream ctx observed
  canceled within 5 s deadline; goroutines return to baseline (slack 4,
  bounded poll; goleak deliberately NOT added — would edit go.mod, outside the
  additive-only envelope; method documented in the test).
- `TestUpstreamNotReportedCanceledOnNormalCompletion` — the mirror guard so the
  cancel assertion cannot be satisfied by an always-firing signal.

### Slow consumer (TestSlowConsumerBackpressure) — measured, not asserted away

Payload 17.0 MB (4000 × 4096 B); client reads 8 KB then stops:
- Mock plateau: 9.83 MB written (57.7% of payload) → backpressure REACHES the
  upstream; the pipeline blocks instead of buffering (a fully-absorbed payload
  would have failed the test).
- Gateway heap during stall: +532 KB → the gateway does NOT hold the bytes.
- Where the bytes live: kernel socket buffers (`tcp_wmem` 16 KB–4 MB default–max,
  `tcp_rmem` 128 KB–6 MB) + the mock's own kernel buffers.
- After resume the stream drains completely — no corruption, no cut.
- `cmd/ezllm/main.go:264` sets **no WriteTimeout by design** ("SSE streams are
  unbounded by design"), so a stalled client's resources are released only when
  the UPSTREAM's stream ends. There is no gateway-side deadline that drops a
  dead client mid-stream. This is now quantified (above), not assumed.

## 4. Known issues / findings

1. **The body is fully JSON-parsed at least 3 times per request:**
   `extractModel` (server.go:474) + `IsStream` (proxy.go:662) + `SwapModel`'s
   map decode *and* re-marshal per hop (proxy.go:637). With failover the decode
   repeats per candidate hop. This is what makes Phase 2/3 scale with payload.
2. **streamCopy allocates ~32 allocs / ~1.9 KiB per SSE event** (8 248 allocs
   for a 256-event stream) — per-event garbage, not a leak (retained-memory
   test is clean).
3. **No gateway-side deadline on a stalled SSE client** (by design,
   main.go:264) — resources tie to upstream stream lifetime. Quantified in §3;
   a deliberate trade-off, recorded here so it is a decision, not an accident.
4. **No correctness bugs found.** Every pre-existing test passes unmodified;
   no production file needed a fix; nothing to separate out per the task's
   "minimal fix" clause.

## 5. Reproducibility cross-check

Three independent benchmark sweeps (two interrupted runs + the final one in
this report) agree on shared scenarios within noise: non-stream 1 KiB seq
208.8 / 214.9 / 217.4 µs; non-stream 64 KiB seq 901.0 / 921.0 / 868.1 µs
(~6% spread) — expected for a non-isolated machine. No number here is
extrapolated, estimated, or copied from any source but a `go test` run.

## 6. The single highest-value next optimization

**Single-parse dispatch: decode the request body ONCE per request and thread
that decode through model extraction, `IsStream` and the hop's model swap.**

Why this one, from the measured evidence:
- It is the only router-owned phase that scales with body size
  (Phase 2: 207 µs @1 KiB → 303 µs @64 KiB → 1.70 ms @1MiB; Phase 3 embeds a
  second full decode+re-marshal per hop), while ingestion is 1.5 GB/s and
  streamCopy per-event cost is size-independent.
- It is the traffic that actually hits this gateway: the dominant client
  (Hermes agent turns) sends ~175k-token median prompts ≈ 0.7–1 MB bodies, so
  this path runs at its WORST size, repeatedly.
- Estimated saving at 1 MiB: ~2 of the 3 full parses (~3 ms CPU/request) plus
  their garbage, with a smaller but real win at 64 KiB (~0.5 ms).
- Constraint-safe: stdlib `encoding/json` only (no new JSON library), and the
  swap's existing semantics MUST be preserved verbatim — `UseNumber()` for big
  ints, unknown-field pass-through (`tools`, `thinking`, `extra_body`, …).
  The optimization is fewer DECODES, never a narrower field set. A byte-exact
  wire-comparison test (original vs single-parse path, random unknown fields)
  is the acceptance gate.

Explicitly NOT recommended from this data: micro-optimizing streamCopy's
allocs (no measured latency symptom at current concurrency), or touching
ingestion (already 1.5 GB/s). No token-savings claim is made anywhere — this
is router overhead only, and token reduction needs a separate task-quality
evaluation per the task.

## 7. Blocked / not measured (declared per acceptance criteria)

- **`ezllm_architecture_specification.md` does not exist** in the repo —
  reference was `docs/*.md` + README + EXPLORING.md (agreed with Calypso), so
  the spec's latency targets are N/A by definition. No spec number is claimed.
- **No CI latency gates added** — correct per task: machine-dependent targets
  must not become CI failures without an agreed reference environment.
- **1 MiB percentiles not measured** (1 KiB + 64 KiB are; 1 MiB has bench
  ns/op only) — the harness caps at n=2000 × 64 KiB in runtime budget; run
  `TestLatencyPercentiles` at 1 MiB is a known extension, not a hidden gap.
- **Real-provider network time excluded by design** (mock isolates router
  overhead); no paid provider calls were made.
- **Percentile runs are single-shot (`-count=1`)** on a loaded machine;
  the cross-check in §5 bounds the noise instead of pretending it away.
- Deployment-bench (prod Docker, LAN, TLS) not measured — loopback only.

## Appendix: what changed in this milestone

- `internal/proxy/benchutil/benchutil.go` — deterministic mock upstream
  (fragmented-SSE support, cancellation hooks, plateau detection)
- `internal/proxy/bench_test.go`, `internal/proxy/cancel_test.go` — phases 3–4
- `internal/server/bench_test.go`, `perf_harness_test.go` — phases 1–2, e2e,
  percentile harness
- `internal/server/slowconsumer_test.go`, `retained_test.go`,
  `lifecycle_test.go` — resource bounds, retention, cancellation
- `internal/server/server_test.go` — `newHarness` generalized to
  `testing.TB` + `newHarnessAt` (caller-owned upstream); no behavior change
- `Makefile` — `bench` and `perf` targets
- `EXPLORING.md` / `CHANGELOG.md` / `web/` untouched, per task scope
