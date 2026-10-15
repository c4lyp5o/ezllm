# ezllm admin API — contract (v1 · M1–M9 shipped, current through 2026-10-10)

Status codes: `200` ok · `201` created · `400` bad request **or surface-pin refusal** ·
`401` no/invalid client token · `403` token lacks `admin` · `404` unknown id · `409` conflict (refs exist; unconfirmed pin surface) · `422` **key test failed** (with `step`) · `429` at cap.
All routes except `/healthz` require `Authorization: Bearer ***

Errors are OpenAI-shaped: `{"error":{"type":"...","message":"...","code":422}}`.
`422` key-test failures add `step`, `upstream_status`, `upstream_error`, `hint` (see below).

---

## Inference surface (M2, unchanged)

| Method | Path | Notes |
|---|---|---|
| GET | `/healthz` | no auth; **liveness only** — `{"status":"ok"}` after a DB ping (503 `{"status":"down"}` if the ping fails). No counts/paths/versions. |
| GET | `/v1/models?protocol=` | `openai` (default) / `anthropic` shape; ids are `<namespace>/<model>`, plus one bare id per **enabled** combo |
| POST | `/v1/chat/completions` | OpenAI stream + non-stream |
| POST | `/v1/messages` | Anthropic (`x-api-key` or Bearer) |
| POST | `/v1/responses` | OpenAI Responses |

**Surface-pin refusal (all three POSTs).** If the resolved model carries a `proto_pin` and the
client called a different surface, the request is refused **before any upstream dial** with
`400 {"error":{"type":"pinned_surface","message":"...","pinned":"openai","requested_surface":"anthropic","code":400}}`.
The pin never reroutes the request — native paths pass bodies through untranslated, so the
honest answer is a refusal that names the endpoint to use instead. (Translated hops — the
probe-gated anthropic→openai exception, `docs/translation-runbook.md` — do not bypass pins
either.) Lookup errors fail closed (refuse), and combo
hops pinned to another surface refuse the whole request rather than silently skipping.

`/v1/models` rows: namespaced ids carry the upstream's `owned_by` and, once probed, a `protocol`
map. A combo row is bare (`owned_by: "ezllm"`) and adds a non-standard `combo` key —
`{"strategy":"failover","hops":[{"account_id":1,"model":"mimo-v2.6-flash","enabled":true}],"context_size":200000}` — so a
picker can say what the name routes to; OpenAI clients that don't know the key ignore it. Combo names
can never contain `/`, so a bare id is unambiguous. Disabled combos are never listed.

## M3 — accounts, keys, models, quota

| GET | `/admin/combos` | Lists combos with strategy, hops, compression profile, and advertised `context_size` (default 200000) |
| POST | `/admin/combos` | Creates a combo; optional `context_size` is positive tokens, defaults to 200000 |
| PATCH | `/admin/combos/{id}` | Partial update; omitted compression profile keeps current; explicit `compression_profile_id: null` clears it |
| GET | `/v1/models` | Enabled combo rows include `context_size`; client-advertised only, not gateway enforcement |

**Combo PATCH semantics:** omitted fields keep their stored value; `compression_profile_id: null` clears the binding. `context_size` is required positive integer when supplied, defaults to 200000 on creation, and is emitted only on combo rows.

**Combo failover cooldown:** transport failures/timeouts and upstream 5xx responses cool only that account endpoint for 5 minutes. Model-specific 401/403/404/429 responses remain failover-able but do not mark the endpoint dead. After expiry exactly one half-open probe is admitted; if every candidate is cooling down, no upstream request is sent. When all attempted hops returned an upstream failover response, the last permitted response is returned rather than issuing another call.


Dashboard payload. One call so the first paint is a single round trip. (Admin-gated — this is
where the detail that `/healthz` no longer leaks lives.)
```jsonc
{
  "health": { "status":"ok", "schema_version":7, "accounts":5, "provider_keys":5,
               "models":51, "combos":2, "ledger_rows":1065, "dropped_rows":0,
               "ledger_span":{"oldest":"…","newest":"…"}, "uptime_s":18119,
               "db_path":"/data/ezllm.sqlite" },
  "usage":  [ {"k":"opengo","calls":64,"tin":1200000,"tout":84000,"cread":300000,"cwrite":21000,
                "reasoning":4000,"saved":0,"errors":1} ],          // group_by=account, last 24h
  "usage_by_surface": [ {"k":"openai","calls":50,"tin":…,"tout":…,"cread":…,"cwrite":…,"reasoning":…,"saved":…,"errors":0} ],
  "accounts": [ AccountSummary … ],
  "recent_calls": [ {"ts":"…","client":"hermes","surface":"openai","account":"opengo",
                      "model":"qwen3.8-flash","status":200,"ttft_ms":180,"total_ms":940,
                      "tin":120,"tout":40,"cread":0} ]            // last 20, newest first
}
```
`AccountSummary`:
```jsonc
{
  "id":1, "name":"OpenCode Go", "namespace":"opengo", "kind":"opencode-go",
  "base_url":"https://opencode.ai/zen/go/v1", "enabled":true,
  "probe_delay_ms":400, "cap_window":"monthly", "cap_tokens":0,
  "usage_convention":"auto",   // schema v13: how the upstream Messages wire counts
                               // input_tokens — auto|anthropic|total
  "keys":[ {"id":9,"label":"primary","hint":"sk…5E5i","enabled":true,
             "last_test_at":"…","last_test_ok":true,"last_test_detail":"200 · 36 models · auth ok · quota=percent"} ],
  "quota": { "kind":"percent", "percent_rolling":0, "percent_weekly":7, "percent_monthly":12,
              "balance":null, "unit":null, "resets_at":"…", "read_at":"…" },   // latest snapshot, null if none
  "models_count":36,
  "protocol_support": {"openai":36,"anthropic":12,"responses":5,"untested":0}
}
```

### `GET /admin/requests`
Token dissection explorer: filter the calls ledger by tokens-in / tokens-out, bound the
window, sort by either axis. `summary` covers the WHOLE matching set — not just the
returned page — so the totals answer "how much lives behind this filter".
```jsonc
{
  "rows": [ RecentCall … ],   // same projection as recent_calls: tin/tout/cread/
                              // reasoning/saved/compression/applied/rules_fired
                              // (rules_fired = caveman prose rules that fired; 0 otherwise),
                              // plus `translated` (schema v12): true when the served hop's
                              // WIRE differed from the client's surface, so the bodies were
                              // translated (anthropic client -> openai-only hop, or any client
                              // onto a hop curated for /v1/responses). On such a row `tin` is
                              // the vendor's INCLUSIVE total — the upstream's own total is
                              // passed through, cache counted inside — and `cread` is 0. Read
                              // the marker before comparing tin across rows; see
                              // docs/translation-runbook.md "Ledger attribution".
  "summary": { "count":23, "tin":966, "tout":69, "cread":0, "reasoning":0,
               "context_pre":0, "context_saved":0, "compression_saved":409,
               "max_tin":42, "max_tout":3,
               // compression accounting (whole matching set):
               // applied_* = rows where a rewrite ACTUALLY shipped shorter
               //   (applied_saved is the real token total — the plain
               //   compression_saved also sweeps legacy tokens_saved sitting
               //   on declined rows);
               // declined_* = rows a stage recorded a rewrite for but
               //   rejected (ratio floor / fail-open); held_notional sums
               //   what those rejected rewrites would have saved.
               "applied_calls":5, "applied_saved":400,
               "declined_calls":2, "held_notional":370 },
  "filter": { "from":"…","to":"…","limit":50,"sort":"ts_desc" }
}
```
Params: `min_tin` `max_tin` `min_tout` `max_tout` (non-negative integers);
`from`/`to` (RFC3339, `[from,to)` like `/admin/usage`, default last 24h with the same
+1min grace); `limit` (1-500, default 50); `sort` = `ts_desc` (default) | `tin_desc` |
`tout_desc`. Anything invalid → `400`.

### `GET /admin/usage?group_by=&from=&to=`
Dashboard aggregates (`store.UsageReport`), computed server-side over the FULL filtered
set — the client never sums rows. `group_by` ∈ `account` (default) | `key` | `alias` |
`client` | `model` | `surface` | `day`; `from`/`to` are RFC3339 on `[from,to)` and default
to the last 24h (+1min grace, same as `/admin/requests`). Each row carries
`p50_ttft_ms`.

**`group_by=day` is UTC by default.** Rows are stamped UTC and the default bucket is
`substr(ts,1,10)`, so the dashboard's day boundary is midnight UTC. Usage-rule CAPS, by contrast, are evaluated in the
rule's own `*time.Location` (`rules.Window.loc` — Asia/Kuala_Lumpur unless the rule names
another zone). The two therefore disagree about which day a call belongs to whenever a rule
runs in a non-UTC zone, by up to the zone offset: the dashboard's "today" counts a call the
daily cap already refuses, or vice versa. Compare `group_by=day` against a UTC-aligned rule,
or derive the day boundary from the rule's `tz` before trusting the two together. The
grouping is UTC-by-construction and deliberately not silently changed.

**Aligning the two — `EZLLM_USAGE_TZ` / `?tz=`.** Set `EZLLM_USAGE_TZ=Asia/Kuala_Lumpur` in the
environment and every `group_by=day` bucket is computed in that zone, using the *same*
`CountBucket` the usage meter writes its counters with — so the dashboard's day and the rule's
day are one implementation and cannot drift. A per-request `?tz=` overrides the env default.
Precedence: `?tz=` > `EZLLM_USAGE_TZ` > UTC. A `tz` that does not resolve is rejected (400 on the
request; the container fails to start naming the bad value), never silently treated as UTC.
The zone used is echoed back as `tz` in the response, because a day-grouped number is
meaningless without it. `tz` applies only to `group_by=day` and is rejected for any other
grouping rather than ignored. DST zones are handled per row, so a day boundary that moves
during the year is still correct.

**`p50_ttft_ms` convention:** the UPPER of the two middle values for an even-sized group
(`order by ttft_ms`, position `n/2 + 1`). This is the historical behaviour, kept unchanged —
it is directional dashboard guidance, not a statistical claim. The response's `totals` row
carries its OWN `p50_ttft_ms` — a single median over the whole window (medians do not add,
so it is never a sum of the group medians) — plus `context_pre`/`context_saved` summed like
every other metric.

### `POST /admin/accounts` → `201`
```jsonc
// request
{ "name":"SSN GPT", "namespace":"super-ssn", "kind":"openai-compatible",
  "base_url":"https://space.stationine.com/v1", "probe_delay_ms":400,
  "requires_session_header":false, "custom_headers":null, "notes":"" }
```
`kind` ∈ `opencode-go|openai-compatible|anthropic-compatible|gemini-openai`. `namespace`:
`^[a-z0-9][a-z0-9._-]{0,62}$` and must not contain `/`. Unique → `409`.
`usage_convention` (schema v13, optional; default `auto`) is the operator-declared input
accounting of the upstream **Messages** wire: `auto` = unstated (never inferred from `kind` or
`base_url`; cross-wire conversions onto a Messages upstream are refused), `anthropic` =
`input_tokens` EXCLUDES cache tokens, `total` = `input_tokens` INCLUDES cache tokens. See
`docs/usage-convention-design.md`.
For `anthropic-compatible`, `base_url` is the host only (e.g. `https://api.anthropic.com`) —
the `/v1` version segment is appended automatically (`/v1/messages`, `/v1/models`).
`gemini-openai` is Google's OpenAI-compatible endpoint (`…/v1beta/openai`): it answers a bad
key with `400`, not `401`/`403`, and that is accepted as an auth rejection **only for this kind**
(see `AuthRejectStatuser`).
Returns the `AccountSummary`.

### `GET /admin/accounts` → `200` → `[AccountSummary, …]`

### `GET|PATCH|DELETE /admin/accounts/{id}`
`PATCH` accepts any subset of the create fields (namespace change allowed — it re-points routing).
Absent fields keep their current values — including `usage_convention`; an invalid value is
`422` naming the field and leaves the stored value untouched.
`DELETE` → **`409`** when combos reference it, body lists the combos:
```jsonc
{"error":{"type":"conflict","message":"account referenced by 2 combos",
  "combos":[{"id":3,"name":"daily"},{"id":7,"name":"fallback"}]}}
```
Add `?force=true` to delete anyway (cascades: keys, models, quota rows).

### `POST /admin/accounts/{id}/keys` → `201` | `422`
```jsonc
// request
{ "label":"primary", "api_key":"sk-…", "skip_inference":false }
```
Runs the **7-step test** (design: `2026-10-06_070000-ezllm-m3-keytest-design.md`).
`201` response:
```jsonc
{ "key": {"id":9,"label":"primary","hint":"sk…5E5i","enabled":true,"last_test_ok":true,
           "last_test_at":"…","last_test_detail":"…"},
  "test": { "ok":true, "steps":[ {"step":"format","ok":true,"ms":0},
                                  {"step":"catalog","ok":true,"ms":640,"detail":"36 models"},
                                  {"step":"auth","ok":true,"ms":760,"detail":"usage oracle 200"},
                                  {"step":"quota","ok":true,"ms":780,"detail":"percent weekly=7"},
                                  {"step":"inference","ok":true,"ms":1690,"detail":"deepseek-v4.1-flash"},
                                  {"step":"protocol","ok":true,"ms":4200,"detail":"openai 6/6 anthropic 4/6 responses 2/6"} ],
             "catalog":[ {"id":"qwen3.8-flash","owned_by":"opencode"} , … ],
             "quota": { "kind":"percent", … } } }
```
`422` failure (nothing persisted — not even a disabled row):
```jsonc
{"error":{"type":"key_test_failed","message":"provider rejected this key","code":422,
  "step":"auth","upstream_status":401,
  "upstream_error":{"type":"AuthError","message":"Invalid API key."},
  "hint":"opencode-go's GET /v1/models succeeds for ANY key, so a working catalog does not prove the key is valid. This key failed the usage-oracle check."}}
```
Steps: `format` → `catalog` → `auth`(oracle; **gate**) → `quota`(non-fatal) →
`inference`(unless `skip_inference`) → `protocol` (≤ `probe_max_models`, default 6, async-flagged
in response when deferred) → persist.

**Streamed steps (`Accept: application/x-ndjson`)** — both this endpoint and the retest endpoint
accept a request with `Accept: application/x-ndjson` and answer `200` with
`Content-Type: application/x-ndjson` instead: one flushed line per event, terminal line closes the
stream. Same request body; same payloads as below — only delivery changes.
```jsonc
{"type":"step","step":{"step":"catalog","ok":true,"ms":640,"detail":"36 models"}}
{"type":"step","step":{"step":"auth","ok":true,"ms":760,"detail":"usage oracle 200"}}
{"type":"step","step":{"step":"inference","ok":false,"ms":1690,"detail":"generation probe failed"}}
{"type":"error","status":422,"error":{ …exactly the 422 body below… }}   // failure terminal line
{"type":"result","key":{…},"test":{…}}                                    // success terminal line (retest: same shape, HTTP 200 semantics)
```
Because headers precede the body, the stream's HTTP status is always `200`; the failure line
carries the status the one-shot path would have used in its `status` field (and the `error.code`
inside — identical bytes, single source: `keyTestError`). Clients that do not send the header get
the historical one-shot JSON unchanged. Clients must not assume a status line for streamed
failures — branch on the terminal line's `type`.
**`format` no longer inspects the key.** Since 2026-10-10 all shape checks (empty, whitespace,
min/max length, `sk-` prefix) were removed — providers change key formats and the upstream probe
is the real validator; the step still fails here for an unregistered `kind`. The `format` step
name is kept stable — the UI maps it to a chip and the 422 body names it.
A **public `/models`** (Command Code returns 200 with no auth) proves nothing: the catalog step
records models, the auth gate still decides, and when a kind has no usage oracle registration
falls back to the real inference probe.
`429`/`5xx`/timeout/transport failure → retried with exponential backoff, using the global
`retry.retries` setting (default `3` retries after the initial attempt) per combo hop.
Retries set to 3 after the initial call per hop. When all four total attempts fail retryably, combo failover advances and grants the next hop its own budget. For `n` hops, worst-case network attempts are `4n`; use `retry.retries: 0` to disable extra tries. **`400`/`401`/`403`/`404`/`422` → no retry.** Streaming responses are never replayed after bytes reach the client.

### `POST /admin/accounts/{id}/keys/{keyId}/retest` → `200` | `422`
Same flow against an already-stored key. On failure the row is **kept** but marked
`last_test_ok=0` (it was valid before; the world may have changed) — unlike first-add.

### `GET /admin/accounts/{id}/keys` → `200` → keys array (hints only, never plaintext)

### `DELETE /admin/accounts/{id}/keys/{keyId}` → `204`

### `POST /admin/accounts/{id}/sync` → `200`
Full catalog + protocol probe (all models, or `{"models":[…],"surfaces":["openai"]}` to scope).
```jsonc
{ "synced":36, "protocol_tested":36,
  "protocol_support":{"openai":36,"anthropic":12,"responses":5,"untested":0},
  "ms":12400 }
```
**Test-all fields** (2026-10-12): `{"probe_all":true}` sweeps the WHOLE catalog on the
requested surfaces instead of sampling `max_models` of it, and defaults
`only_untested:true` — models that already carry a verdict on every requested surface
(probed or curated) are skipped, so a second click never re-sends. Send
`{"probe_all":true,"only_untested":false}` for a forced re-test. The response gains
`probing` (models actually queued), `surfaces`, `budget_s`, and `probe_all`. One sweep
per account: a second `POST` while one runs → `409` with the stop instruction.
- `GET /admin/accounts/{id}/sync` → `{"running":false}` or
  `{running, started_at, surfaces, probing, elapsed_s}`. Progress itself is the model
  list: probes write verdicts per model as they run.
- `DELETE /admin/accounts/{id}/sync` → `204` stops the running sweep (`404` when none).
  Already-written verdicts stay.

### `GET /admin/accounts/{id}/models` → `200`
```jsonc
[ {"id":"qwen3.8-flash","display":"…","owned_by":"opencode",
   "openai":1,"anthropic":1,"responses":0,"tested_at":"…","pin":"",
   "source":"probe","note":""} ]
```
`null` = untested (the UI must render three states: ✓ / ✗ / ?). `pin` is `""` (auto) or the
pinned surface; enforced by the router (see Surface-pin refusal above). `source` = who
last wrote the verdicts (`probe` / `curated` / absent for a legacy row); `note` is the
operator's reason for a curation.

### `PUT /admin/accounts/{id}/model-pin` → `200`
Set or clear a model's surface pin. The model id travels **in the body** (ids can contain `/`,
so it can never be a path segment). `pin` distinguishes three states — **absent** field → `400`
(this endpoint always states intent), `""` → clear (back to auto), or one of `openai|anthropic|responses`.
```jsonc
// request                                   // 200 response
{ "model":"qwen3.8-flash",                   { "model":"qwen3.8-flash",
  "pin":"openai",                              "pin":"openai",
  "force":false }                              "force":false }
```
Unknown model → `404`. Pinning a surface the probe never confirmed (`null`/`0`) → `409
unconfirmed_surface` unless `force:true`. A pin set with `force` on an unconfirmed surface is
honored exactly like any other — the refusal then protects you from the mistake downstream.
Catalog resyncs **preserve** pins (models that vanish upstream lose their pin with their row).

## M9 — prices, cost, and the research columns (schema v8)

### `GET /admin/prices` → `200` → `[Price, …]`
```jsonc
{ "account_id":6, "model_id":"qwen3.8-flash", "effective_from":"2026-01-01",
  "currency":"USD", "price_in":0.2, "price_out":1.6,
  "price_cache_read":0.04, "price_cache_write":0.25, "note":"source + date" }
```
Rates are **USD per 1M tokens**, versioned per `(account, model, effective_from)`. Same
version re-set = overwrite; new date = new version. Cost joins each call to the newest
rate effective **on or before the call's date**, so a reprice never rewrites history.

### `PUT /admin/accounts/{id}/prices` → `200` (echoes the stored row)
Model id in the body (ids can contain `/`). **All four rates are required** — a missing
one is refused rather than treated as zero, because "free" must be a stated decision.
Negative rates → `400`; model not in the account's catalog → `404` (mirrors the pin
endpoint). `effective_from` accepts a date or full timestamp (normalized to `YYYY-MM-DD`).

### `DELETE /admin/accounts/{id}/prices` → `204` | `404`
Body `{"model","effective_from"}` deletes that exact version. Unknown version → `404`.

### `GET /admin/cost?from=&to=` → `200` (default: last 7 days)
Derived spend per `(day, account, model)` — computed at query time, never stored per row:
```jsonc
{ "rows": [ { "day":"2026-10-09", "account":"alibaba-token-plan", "model":"qwen3.8-flash",
              "calls":81, "unpriced_calls":3, "tokens_in":242413, "tokens_out":40880,
              "tokens_cached_read":10353920, "tokens_cached_write":0, "usd":4.31 } ],
  "totals": { "usd":4.31, "calls":1106, "unpriced_calls":3,
              "pricing_complete":false, "note":"usd counts priced calls only" } }
```
**Unpriced calls are excluded from `usd`, never zeroed into it** — `unpriced_calls` > 0
means the total is a floor, not the full bill. The bill formula (see `NormalizeUsage`:
`tokens_in` already excludes cached, so the four columns partition input cleanly):
`usd = (in·price_in + out·price_out + cached_read·price_cache_read + cached_write·price_cache_write)/1e6`.

### Requests projection — new v8 fields
`/admin/requests` and the SSE feed now also emit, per call:
`saved_notional` (what a floor-rejected rewrite *would* have saved — `saved` is only real
shipped savings), `prefix_sha` (stable-core hash: system+tools+history minus the newest
turn; identical for re-sends of the same context, changes on ANY core edit — the cache-
break signal), `session_id` (from the optional `X-Ezllm-Session` client header; a pure
grouping key, never routing), `msg_count`, `tool_count`, `req_bytes` (exact body bytes
posted upstream, after compression). Also: `ttft_ms` is now genuinely time-to-first-token
(measured from dispatch; before this it timed from inside the stream copy and read ~1–3ms
for every call).

### Requests projection — v14 fields
`ip` — the caller address captured at request time (`X-Real-IP`, then the FIRST
`X-Forwarded-For` entry, then the socket peer; empty when nothing was available).
Attribution for traffic mapping, not authentication: a direct connection can
claim any value, so it must never gate anything. The dashboard shows it under
the request's timestamp.

### `GET /admin/accounts/{id}/quota` → `200`
Live `ReadQuota` → classified shape + `raw`. `{"kind":"none"}` when the provider has no endpoint
(`404`/`405` → NOT an error).

## M3 — combos, tokens, export

### `GET|POST /admin/combos` · `GET|PATCH|DELETE /admin/combos/{id}`
```jsonc
{ "name":"daily","strategy":"failover","sticky_idle_s":1800,"enabled":true,
  "compression_profile_id":null,
  "hops":[{"account_id":1,"model_id":"gpt-6-luna","weight":1,"enabled":true,
           "hold_until_failure":false}, …] }

Each hop carries `hold_until_failure` (block_round_robin only, default false):
when true the hop does not rotate when its block runs out — it HOLDS the block
while it keeps succeeding and releases it on a hop-attributable failure (429,
5xx, transport error, or a translated stream that failed/truncated late). A 4xx
is the request's fault and keeps the hold. The hop's block tag renders pinned at
`▮ 1/N` while it holds.
```
`strategy` ∈ `failover|true_round_robin|sticky_last_good|least_used|block_round_robin`.
(`strict_round_robin` was removed 2026-10-13: zero combos used it and its
success-driven advance is now expressed as `block_round_robin` with a held hop.)
`POST` upserts by name (idempotent). `hops` replace the whole ordered list (positions = array order).
`PATCH` is a partial update — only the fields you send change, `enabled` included, so the one-field
toggle is `PATCH /admin/combos/{id}` `{"enabled":false}` (the `?enabled=true|false` query form is
still honored). A disabled combo refuses inference with `combo "x" is disabled` and drops out of
`/v1/models`.

### `POST /admin/combos/{id}/hops` → `200`
Replaces the hop list atomically (same body shape, `hops` only).

### `GET|POST /admin/tokens` · `PATCH|DELETE /admin/tokens/{id}`
```jsonc
// POST  → 201 (token plaintext returned EXACTLY ONCE, never again)
{ "name":"hermes","roles":["infer"],"token":"ezllm_…" }   // token optional; generated if absent
// GET   → [{"id":4,"name":"hermes","hint":"ezllm…a91f","roles":["infer"],"enabled":true,"last_used_at":"…"}]
// PATCH → {"enabled":false} | {"roles":["infer","admin"]}
```
`roles` ⊆ `infer|admin`.

### `GET /admin/export` → `200`
Full config dump: accounts (no key plaintext — hints + `key_ct` omitted), combos, tokens (hints),
compression profiles, and per-key latest quota. `?include_ledger=true` adds the call ledger.
This is the disaster-recovery file; it must re-seed a fresh instance minus credentials.
The config half is written first and the ledger is **streamed** row by row, so the response is
chunked (no `Content-Length`) and memory stays flat regardless of ledger size — the ledger is
**complete**, with no row cap. If the ledger stream fails mid-way the document is still closed
validly rather than truncated, and `/admin/health` carries the row counts to reconcile against.

## M3 — quota snapshots

### `GET /admin/quota?keys=1,2` → `200`
Latest snapshot per key (or all keys when `keys` omitted) — the poller's read side.
Also `POST /admin/keys/{keyId}/quota` → `200` to force a live read + snapshot now.

---

## M4 — model rules (caps + allowed-use windows)

### `GET|POST /admin/model-rules` · `GET|PATCH|DELETE /admin/model-rules/{id}` → `200`
One rule per `(account_id, model_id)`: a rolling **token cap** and/or an **allowed-use window**.

- `cap_tokens` — `0` = unlimited; `cap_window` — `5h | daily | weekly | monthly` (default `monthly`).
- `win_start` / `win_end` — `HH:MM` local to `win_tz`; `end < start` wraps midnight; both empty =
  always allowed; an identical pair is rejected (empty window, not all-day).
- `win_days` — CSV of `0–6` (Sun=0), `""` = every day; `win_tz` — IANA name or fixed offset.
- `enabled`, `note` optional.

Enforcement: inside a combo, a hop whose rule says "not now" is **skipped silently** (the next
hop serves); a direct call returns `429` naming the rule and its window; a combo refused on
every hop returns `429`, not `502`.

## M5–M7 — compression profiles

### `GET|POST /admin/compression-profiles` · `GET|PATCH|DELETE /admin/compression-profiles/{id}` → `200`
`stages` is an ordered engine pipeline over `session_dedup`, `rtk`, `headroom`, `lite`, `caveman`,
`budget`, validated against the engine registry on write. Stage options are engine-specific
(e.g. `{"engine":"rtk","keep_lines":6}`); `budget` carries its own context-token accounting.

- `exempt_last_turn` (default true) · `min_compress_ratio` (0.05) · `fail_open` (true) ·
  `auto_trigger_tokens` (0 = off) · `enabled` · `notes`.
- Selection: per-request `x-ezllm-compression` header → the combo's `compression_profile_id` →
  off. Fail-open: an engine error passes the original request through untouched.

## M6 — dashboard session & live feed

### `POST /admin/login` → `200` + session · `POST /admin/settings/password`
bcrypt dashboard password; the SPA renders and fetches nothing pre-auth.
Both answers carry `default_password` (bool): whether the stored hash still
matches the factory `STRONGPASSWORD` — the login page uses it to show the
first-run hint only while it is true (fail-closed on errors). Success:
`{"token":"…","default_password":false}`; the 401 nests it under `error`.

### `GET /admin/stream` → SSE
Two event types: `snapshot` (last 20 calls on connect, newest first) and `calls` (each committed
ledger batch).

### `GET /admin/health` · `POST /admin/chat`
`health` counts the same rows the dashboard headline uses; `chat` executes through the **normal
inference path** (router → rules → compression → proxy → ledger) under an admin session — it is
the dashboard's own Chat page.

---

## M10 — service defaults (schema v15)

The four non-chat surfaces (`/v1/embeddings`, `/v1/images/generations`,
`/v1/audio/speech`, `/v1/audio/transcriptions`) get per-endpoint default models,
backed by `service_bindings` + `service_probes`. The default applies **only when a
request omits `model`** — an explicit `model` always wins.

### `GET /admin/services` → `200` → `{services: [ServiceCard, …]}`
Always all four cards in fixed order, including a first run with no providers:
`surface` (`embeddings|images_gen|audio_tts|audio_stt`), `endpoint`, `label`,
`description`, `mode` (`off|manual|auto` — `off` = no row stored), `model_target`
(stored binding, `manual` only), `resolved_target` (what a model-less request uses
right now; `""` = it will refuse), `target_exists`, `verified_models` (green probes
for THIS surface), `accounts` (enabled providers; `0` drives the "connect a
provider" state), `verdict` (`{model, ok, detail, tested_at}` for the resolved
target, or `null`).

### `PUT /admin/services/{surface}` → `200` (echoes the card) | `400` | `404`
Body `{"mode": "manual"|"auto"|"off", "model": "ns/model"}` (unknown keys rejected).
`manual` requires `model`, validated against what EXISTS — a namespaced id in the
account's live catalog (split on the FIRST slash, ids may contain slashes) or an
enabled combo name — never against transient rules/cooldown state. `auto` ignores
and clears `model`; `off` deletes the binding.

### `POST /admin/services/{surface}/test` → `200` (echoes the verdict) | `400` | `404`
Body `{"model": "ns/model", "prompt": "…"}` — `prompt` optional (max 2000 bytes). It
is sent verbatim on the endpoint's own wire field (`prompt` for images, `input` for
embeddings/tts); empty falls back to the per-surface default (embeddings `ping`,
images `a small red circle`, tts `ok`). `audio_stt` rejects a non-empty prompt with
`400` — it tests a file upload, not text, and an input the operator believes was
tested must never be silently dropped. The probe runs ONE real request through the
normal dispatch path (ResolveCandidates → ForwardCandidates) with an endpoint-appropriate
shape — embeddings `{"input":…}` · images `{"prompt":…,"n":1}` ·
tts `{"input":…,"voice":"alloy"}` · stt multipart with a 0.2 s silent WAV — and
stores the verdict in `service_probes` per `(surface, account, model)`. The call is
ledgered like any other (client `service-test`): invisible spend is unauditable
spend. **Image probes can cost provider quota — the UI confirms before each one and
no sweep ever runs one unattended.** `ok` means "the wire answered this service
shape", not output quality; `auto` may only pick tested-ok models.

### Model-less request behavior
- Service surface + usable binding → the default fills in and dispatches normally.
- Service surface + no usable default → `400` `service_not_configured` (typed, with
  `service` + `configure: "/services"`), ledgered like every other resolve failure.
- Chat surfaces are unchanged: `model` is still required (`400`).
- Auth is unchanged: unauthenticated calls to these routes return `401`.

---

## Conventions

- Timestamps: RFC3339 UTC. `ms` fields are integers ≥ 1 (`ttft_ms` floors at 1).
- `hint` is the only surviving form of a credential: `sk…5E5i` (≤8-char secrets mask fully).
- Paging: `?limit=` (default 100, max 1000) + `?offset=` on list endpoints.
- The frontend is same-origin-served by the Go binary from `web/dist` in production; in dev it
  proxies `/admin`, `/v1`, `/healthz` to `127.0.0.1:20129`.
