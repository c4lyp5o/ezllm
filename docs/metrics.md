# Scrape Prometheus metrics

ezllm serves Prometheus metrics at `GET /metrics`: requests, tokens, timings and ledger
health. Prometheus, VictoriaMetrics, Grafana Alloy and the OpenTelemetry Collector can read
them. Collecting is always on and costs nothing per request.

You need ezllm running and a dashboard login.

## 1. Create a metrics key

In the dashboard, open **Connect** and choose **Create key**. Enter a name. Under
**Roles**, untick **Inference** and tick **Metrics**, then choose **Create key**. Copy the
key now: you see it once.

A metrics key reads only `/metrics`. Admin and inference keys get `403` there, so use
this one.

## 2. Save the key

Save the key in a file on the Prometheus machine, for example
`/etc/prometheus/ezllm-metrics.key`. Only Prometheus should be able to read it.

## 3. Add a scrape job

Add this to `prometheus.yml`. Change `ezllm.example.com` to the address of your ezllm.

```yaml
scrape_configs:
  - job_name: ezllm
    scrape_interval: 30s
    authorization:
      credentials_file: /etc/prometheus/ezllm-metrics.key
    static_configs:
      - targets: ["ezllm.example.com:20129"]
```

For the OpenTelemetry Collector, put the same block under
`receivers.prometheus.config.scrape_configs`. **Connect** also shows the scrape address
and this job, ready to paste. Its **Test it from a terminal** command checks the key.

Reload Prometheus. You should see the `ezllm` target as **UP** on its **Targets** page.

## What you get

| Name | Type | Labels | Meaning |
|---|---|---|---|
| `ezllm_requests_total` | counter | `surface`, `account`, `model`, `status` | Requests committed to the ledger |
| `ezllm_tokens_total` | counter | `account`, `model`, `kind` | Normalized tokens; `kind` is `input`, `output`, `cached_read`, `cached_write` or `reasoning` |
| `ezllm_compression_saved_tokens_total` | counter | `account`, `model` | Prompt tokens compression removed before the upstream call |
| `ezllm_request_duration_seconds` | histogram | `surface`, `account` | Total request time |
| `ezllm_time_to_first_token_seconds` | histogram | `surface`, `account` | Time to first streamed token |
| `ezllm_inflight_requests` | gauge | | Inference requests admitted now |
| `ezllm_inflight_limit` | gauge | | Configured global in-flight limit |
| `ezllm_count_tokens_total` | counter | `source` | `POST /v1/messages/count_tokens` answers; `source` is `upstream` (an Anthropic-wire hop answered) or `estimate` |
| `ezllm_ledger_lost_rows_total` | counter | | Rows discarded after a failed flush and retry |
| `ezllm_ledger_dropped_rows_total` | counter | | Rows dropped because the ledger queue was full |
| `ezllm_ledger_unmetered_rows_total` | counter | | Committed rows the usage meter did not count |
| `ezllm_bridge_cli_info` | gauge | `bridge`, `version`, `update_result` | 1 per running bridge, from the host updater's last report |
| `ezllm_build_info` | gauge | `version` | The running release |
| `ezllm_start_time_seconds` | gauge | | Process start, Unix seconds |
| `go_goroutines`, `go_memory_heap_objects_bytes`, `go_memory_total_bytes` | gauge | | Go runtime |

Histogram buckets (seconds): 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300.
`account` is the provider account name; `model` is the model sent upstream.

Counters start at zero when ezllm restarts; `rate()` and `increase()` allow for that.
Totals that last across restarts, and cost, are in the dashboard's **Analytics** and in
`/admin/usage` and `/admin/cost`.

The number of series is capped. Past 2,000 per metric, new models are counted as
`model="_other"`.

This is what one chat request looks like (most lines left out):

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

Error ratio per account, over 5 minutes:

```promql
sum by (account) (rate(ezllm_requests_total{status=~"5.."}[5m]))
  / sum by (account) (rate(ezllm_requests_total[5m]))
```

Time to first token, 95th percentile, per account:

```promql
histogram_quantile(0.95, sum by (le, account) (rate(ezllm_time_to_first_token_seconds_bucket[5m])))
```

How full the request limit is:

```promql
ezllm_inflight_requests / ezllm_inflight_limit
```

Lost ledger rows. This should stay at zero:

```promql
increase(ezllm_ledger_lost_rows_total[1h]) + increase(ezllm_ledger_dropped_rows_total[1h])
```

---

**How it works:** [metrics decisions](design/metrics-design.md)
