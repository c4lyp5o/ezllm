# Pricing runbook

ezllm prices are **versioned rows** in `model_prices`, keyed
`(account_id, model_id, effective_from)`. Cost is derived at query time from the
rate effective on the call's date, so a reprice never rewrites history.

## Cadence

Refresh **daily** — provider list prices change on the order of weeks, and a
daily pass is enough to keep the catalog honest without hammering the source.
ezllm runs this **inside the process**: once 45 seconds after boot (so a fresh
deploy fills the catalog immediately), then every day at 03:00 local. No
external cron or systemd timer is needed. The pass is idempotent for the day
(same `effective_from` upserts), and an operator can still POST the endpoint
manually at any time.

Manual refresh is always available:

```bash
curl -s -X POST \
  -H "Authorization: Bearer $(cat ~/.hermes/secrets/ezllm-admin.token)" \
  'https://llm.calypso.dedyn.io/admin/prices/refresh?source=litellm'
```

The response reports `entries` (source rows), `matched` (catalog matches
written), and `effective_from`. Unmatched models still need manual rates — the
matcher uses an **exact model-id suffix** (`gpt-5-nano` ↔
`openai/gpt-5-nano`), never a prefix, so `gpt-5` cannot price `gpt-5-nano`.

For a one-off historical backfill, add `&effective_from=2026-09-18` (or another
past date). The cost report will apply today's list price to calls from that
date onward; the rows still carry `source="litellm"` as an estimate, not an
invoice.

## What the refresh writes

- `source = "litellm"`, `fetched_at = now`, `note = "litellm snapshot; list
  price, not a bill"`.
- Each matched account/model gets today's `effective_from`.
- The source is a public community snapshot, not a provider invoice. Treat it
  as a list-price estimate; override a specific account/model with
  `PUT /admin/accounts/{id}/prices` when you have the real rate.

## Bulk import

`POST /admin/prices/import` accepts a snapshot from any source:

```json
{
  "source": "my-billing-export",
  "prices": [
    {"account_id": 6, "model_id": "qwen3.8-flash", "effective_from": "2026-10-01",
     "price_in": 0.2, "price_out": 1.6, "price_cache_read": 0.04, "price_cache_write": 0.25}
  ]
}
```

Rows must name a model that exists in that account's catalog; a typo is a 400,
not a silently unpriced row.

## Query

```bash
# current rate
curl -s -H "Authorization: Bearer $EZLLM_INFER_TOKEN" \
  'https://llm.calypso.dedyn.io/v1/prices?model=gpt-5-nano'

# the rate that was effective on a date (for a historical bill)
curl -s -H "Authorization: Bearer $EZLLM_INFER_TOKEN" \
  'https://llm.calypso.dedyn.io/v1/prices?model=gpt-5-nano&at=2026-10-01'
```

`GET /admin/cost` then reports USD plus `unpriced_calls`; `pricing_complete`
is false while any call has no effective rate. Never read `usd` as the full
bill when `unpriced_calls > 0`.

## Scheduling

The scheduler is **inside ezllm** — no external cron, no systemd timer:

- first pass ~45 seconds after boot, so a deploy fills prices immediately;
- then daily at **03:00** in `EZLLM_USAGE_TZ` (UTC if unset);
- logs one line: `scheduled price refresh source=litellm effective_from=… entries=… matched=…`.

Set `EZLLM_PRICE_REFRESH=off` to disable it (for an air-gapped instance).
Prices stay manual/import-only unless the process can reach GitHub.

The admin endpoint remains available for an immediate manual pass or a
backfill; schedule nothing else.
