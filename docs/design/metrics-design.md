# Prometheus metrics: decisions

Why `GET /metrics` works the way it does. Written 2026-11-16 (v2.1.0). How to use it:
[docs/metrics.md](../metrics.md). Code: `internal/server/metrics.go`.

- **No request-path work.** Counters and histograms are fed by
  `store.ObserveLedger`: the ledger goroutine hands each committed batch to the
  collector after the SQLite commit. A request does nothing extra, and a scrape
  counts exactly what the ledger committed. `BenchmarkMetricsObserve` measures
  the observer at about 255 ns per row with zero allocations (4 vCPU, 2026-11-16).
- **Never drops a batch.** `ObserveLedger` is synchronous, unlike
  `SubscribeLedger`, which drops batches for a slow consumer.
- **Always collecting, key-gated reads.** The observer costs about 255 ns per
  committed row off the request path, so it always runs; reading needs a key.
- **No dependency.** The exposition format is written by hand. No Prometheus
  client library and no OpenTelemetry SDK; the binary does not grow.
- **No stop-the-world on scrape.** Runtime gauges come from `runtime/metrics`,
  not `runtime.ReadMemStats`.
- **Counters restart at zero** with the process, as Prometheus expects;
  `rate()` and `increase()` handle the reset. Totals that must survive restarts
  are in the ledger (`/admin/usage`, `/admin/cost`).
- **Bounded cardinality.** Labels are the surface, the provider account, the
  upstream model and the HTTP status. Client token names, IPs, sessions and
  aliases are never labels. The model can be caller-supplied on a refused
  request, so each family stops at 2,000 series; new models after that fold into
  `model="_other"`.
- **Not OTLP.** Push export, traces and OTel logs are out of scope. An OTel
  pipeline scrapes this endpoint with the Collector's Prometheus receiver.
- **Cost is not exported.** Prices are versioned and can be re-rated after the
  fact, so `/admin/cost` stays the source of truth.
