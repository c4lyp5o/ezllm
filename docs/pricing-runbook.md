# Prices and cost

ezllm puts a cost on each request from a price list. The cost is an estimate from list
prices, not a bill.

## Where prices come from

ezllm fills the list itself from a public price snapshot (LiteLLM's): 45 seconds after it
starts, then daily at 03:00 in your timezone. It needs to reach GitHub for that.

A model gets a price when its ID ends with a name in the snapshot: `gpt-5-nano` matches
`openai/gpt-5-nano`, but `gpt-5` never prices `gpt-5-nano`. Other models stay unpriced
until you set a rate.

Turn the daily refresh off in **Settings** → **Gateway** → **Automatic price refresh**, or
with `EZLLM_PRICE_REFRESH=off`.

## Set a price yourself

In **Providers**, choose **Pricing** on the account. Rates are US dollars per million
tokens: **Input**, **Output**, **Cache read** and **Cache write**.

Each rate starts on a date. A request uses the rate that applied on its day, so a new
price never changes the cost of older requests. Setting the same date again replaces that
rate.

The refresh never changes a model you priced yourself, by hand or by import: it keeps
your rate. To let the snapshot price that model again, delete your rates for it.

## See the cost

**Token Economics** shows spend for a time range. **Unpriced** counts requests with no rate
on their date. They add `$0`, so while it is above zero, the total is too low.

## With the API

These need an admin key. Refresh from the snapshot now:

```sh
curl -s -X POST -H "Authorization: Bearer YOUR_ADMIN_KEY" \
  'http://127.0.0.1:20129/admin/prices/refresh?source=litellm'
```

The answer says how many models it priced (`matched`) and how many it left alone because
you priced them (`kept`). Add `&effective_from=2026-01-01`
to apply today's prices from an earlier date.

Import rates from your own source, such as a billing export. Each model must exist in that
account, or the import is refused:

```sh
curl -s -X POST -H "Authorization: Bearer YOUR_ADMIN_KEY" -H 'Content-Type: application/json' \
  http://127.0.0.1:20129/admin/prices/import -d '{"source":"my-billing-export","prices":[
  {"account_id":6,"model_id":"model-name","effective_from":"2026-01-01",
   "price_in":0.2,"price_out":1.6,"price_cache_read":0.04,"price_cache_write":0.25}]}'
```

Any key that can call models can read a price. Add `&at=2026-01-01` for the price on a
date:

```sh
curl -s -H "Authorization: Bearer YOUR_EZLLM_KEY" 'http://127.0.0.1:20129/v1/prices?model=gpt-5-nano'
```

`GET /admin/cost` gives the spend, with `unpriced_calls` and `pricing_complete`.

---

**Every field:** [API.md](API.md)
