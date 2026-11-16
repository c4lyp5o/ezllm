# Prometheus metrics

`GET /metrics` serves Prometheus text exposition (format 0.0.4) for scraping by
Prometheus, VictoriaMetrics, Grafana Agent/Alloy or the OpenTelemetry
Collector's Prometheus receiver. Added in v2.1.0. Code: `internal/server/metrics.go`.

## Decisions

- **No request-path work.** Counters and histograms are fed by
  `store.ObserveLedger`: the ledger goroutine hands each committed batch to the
  collector after the SQLite commit. A request does nothing extra, and a scrape
  counts exactly what the ledger committed. `BenchmarkMetricsObserve` measures
  the observer at about 255 ns per row with zero allocations (4 vCPU, 2026-11-16).
- **Never drops a batch.** `ObserveLedger` is synchronous, unlike
  `SubscribeLedger`, which drops batches for a slow consumer.
- **Off by default.** Without a scrape token the route answers 404 and the
  ledger is not observed, so an install that does not scrape pays nothing.
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

## Enable and authenticate

Set a scrape secret of at least 16 characters, either directly or by file:

| Variable | Meaning |
|---|---|
| `EZLLM_METRICS_TOKEN` | Scrape secret. Turns `/metrics` on. |
| `EZLLM_METRICS_TOKEN_FILE` | File whose first line is the secret; used when `EZLLM_METRICS_TOKEN` is unset. |

A scrape authenticates with `Authorization: Bearer <secret>`. The secret works
only on `/metrics`: it is not a client token and cannot run inference or reach
the admin API. An admin client token or a dashboard session can also read the
endpoint. An inference-only client token gets 403.

```yaml
# prometheus.yml
scrape_configs:
  - job_name: ezllm
    scrape_interval: 30s
    authorization:
      credentials_file: /etc/prometheus/ezllm-metrics.token
    static_configs:
      - targets: ["ezllm.internal:20129"]
```

For an OpenTelemetry Collector, use the same block under
`receivers.prometheus.config.scrape_configs`.

## Metrics

| Name | Type | Labels | Meaning |
|---|---|---|---|
| `ezllm_requests_total` | counter | `surface`, `account`, `model`, `status` | Requests committed to the ledger |
| `ezllm_tokens_total` | counter | `account`, `model`, `kind` | Normalized tokens; `kind` is `input`, `output`, `cached_read`, `cached_write` or `reasoning` |
| `ezllm_compression_saved_tokens_total` | counter | `account`, `model` | Prompt tokens compression removed before the upstream call |
| `ezllm_request_duration_seconds` | histogram | `surface`, `account` | Total request time |
| `ezllm_time_to_first_token_seconds` | histogram | `surface`, `account` | Time to first streamed token |
| `ezllm_inflight_requests` | gauge | | Inference requests admitted now |
| `ezllm_inflight_limit` | gauge | | Configured global in-flight limit |
| `ezllm_ledger_lost_rows_total` | counter | | Rows discarded after a failed flush and retry |
| `ezllm_ledger_dropped_rows_total` | counter | | Rows dropped because the ledger queue was full |
| `ezllm_ledger_unmetered_rows_total` | counter | | Committed rows the usage meter did not count |
| `ezllm_bridge_cli_info` | gauge | `bridge`, `version`, `update_result` | 1 per running bridge, from the host updater's last report |
| `ezllm_build_info` | gauge | `version` | The running release |
| `ezllm_start_time_seconds` | gauge | | Process start, Unix seconds |
| `go_goroutines`, `go_memory_heap_objects_bytes`, `go_memory_total_bytes` | gauge | | Go runtime |

Histogram buckets (seconds): 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300.
`account` is the provider account name; `model` is the model sent upstream.
Cost is not exported: prices are versioned and can be re-rated after the fact,
so `/admin/cost` stays the source of truth.

## Example

From the test harness after one chat request (HELP lines and most buckets
trimmed):

```text
# TYPE ezllm_requests_total counter
ezllm_requests_total{surface="chat",account="up",model="gpt-6-luna",status="200"} 1
# TYPE ezllm_tokens_total counter
ezllm_tokens_total{account="up",model="gpt-6-luna",kind="input"} 252
ezllm_tokens_total{account="up",model="gpt-6-luna",kind="output"} 72
ezllm_tokens_total{account="up",model="gpt-6-luna",kind="cached_read"} 1792
ezllm_tokens_total{account="up",model="gpt-6-luna",kind="cached_write"} 0
ezllm_tokens_total{account="up",model="gpt-6-luna",kind="reasoning"} 28
# TYPE ezllm_request_duration_seconds histogram
ezllm_request_duration_seconds_bucket{surface="chat",account="up",le="0.5"} 1
ezllm_request_duration_seconds_bucket{surface="chat",account="up",le="+Inf"} 1
ezllm_request_duration_seconds_sum{surface="chat",account="up"} 0.001
ezllm_request_duration_seconds_count{surface="chat",account="up"} 1
# TYPE ezllm_inflight_requests gauge
ezllm_inflight_requests 0
# TYPE ezllm_inflight_limit gauge
ezllm_inflight_limit 32
# TYPE go_goroutines gauge
go_goroutines 22
```

## Useful queries

```promql
# error ratio by account, 5 minutes
sum by (account) (rate(ezllm_requests_total{status=~"5.."}[5m]))
  / sum by (account) (rate(ezllm_requests_total[5m]))

# p95 time to first token by account
histogram_quantile(0.95, sum by (le, account) (rate(ezllm_time_to_first_token_seconds_bucket[5m])))

# admission saturation
ezllm_inflight_requests / ezllm_inflight_limit

# ledger health: should stay flat
increase(ezllm_ledger_lost_rows_total[1h]) + increase(ezllm_ledger_dropped_rows_total[1h])
```
