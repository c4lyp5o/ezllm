# ezllm admin API — contract (v1 · M1–M7 shipped)

Status codes: `200` ok · `201` created · `401` no/invalid client token · `403` token lacks `admin` ·
`404` unknown id · `409` conflict (refs exist) · `422` **key test failed** (with `step`) · `429` at cap.
All routes except `/healthz` require `Authorization: Bearer <client-token>`.

Errors are OpenAI-shaped: `{"error":{"type":"...","message":"...","code":422}}`.
`422` key-test failures add `step`, `upstream_status`, `upstream_error`, `hint` (see below).

---

## Inference surface (M2, unchanged)

| Method | Path | Notes |
|---|---|---|
| GET | `/healthz` | no auth |
| GET | `/v1/models?protocol=` | `openai` (default) / `anthropic` shape; ids are `<namespace>/<model>`, plus one bare id per **enabled** combo |
| POST | `/v1/chat/completions` | OpenAI stream + non-stream |
| POST | `/v1/messages` | Anthropic (`x-api-key` or Bearer) |
| POST | `/v1/responses` | OpenAI Responses |

`/v1/models` rows: namespaced ids carry the upstream's `owned_by` and, once probed, a `protocol`
map. A combo row is bare (`owned_by: "ezllm"`) and adds a non-standard `combo` key —
`{"strategy":"failover","hops":[{"account_id":1,"model":"mimo-v2.6-flash","enabled":true}]}` — so a
picker can say what the name routes to; OpenAI clients that don't know the key ignore it. Combo names
can never contain `/`, so a bare id is unambiguous. Disabled combos are never listed.

## M3 — accounts, keys, models, quota

### `GET /admin/overview`
Dashboard payload. One call so the first paint is a single round trip.
```jsonc
{
  "health": { "status":"ok", "schema_version":1, "accounts":2, "provider_keys":3,
               "models":47, "combos":1, "ledger_rows":128, "dropped_rows":0, "uptime_s":900 },
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
                              // (rules_fired = caveman prose rules that fired; 0 otherwise)
  "summary": { "count":23, "tin":966, "tout":69, "cread":0, "reasoning":0,
               "max_tin":42, "max_tout":3 },
  "filter": { "from":"…","to":"…","limit":50,"sort":"ts_desc" }
}
```
Params: `min_tin` `max_tin` `min_tout` `max_tout` (non-negative integers);
`from`/`to` (RFC3339, `[from,to)` like `/admin/usage`, default last 24h with the same
+1min grace); `limit` (1-500, default 50); `sort` = `ts_desc` (default) | `tin_desc` |
`tout_desc`. Anything invalid → `400`.

### `POST /admin/accounts` → `201`
```jsonc
// request
{ "name":"SSN GPT", "namespace":"super-ssn", "kind":"openai-compatible",
  "base_url":"https://space.stationine.com/v1", "probe_delay_ms":400,
  "requires_session_header":false, "custom_headers":null, "notes":"" }
```
`kind` ∈ `opencode-go|openai-compatible|anthropic-compatible`. `namespace`:
`^[a-z0-9][a-z0-9._-]{0,62}$` and must not contain `/`. Unique → `409`.
For `anthropic-compatible`, `base_url` is the host only (e.g. `https://api.anthropic.com`) —
the `/v1` version segment is appended automatically (`/v1/messages`, `/v1/models`).
Returns the `AccountSummary`.

### `GET /admin/accounts` → `200` → `[AccountSummary, …]`

### `GET|PATCH|DELETE /admin/accounts/{id}`
`PATCH` accepts any subset of the create fields (namespace change allowed — it re-points routing).
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
Steps: `format`(no network) → `catalog` → `auth`(oracle; **gate**) → `quota`(non-fatal) →
`inference`(unless `skip_inference`) → `protocol` (≤ `probe_max_models`, default 6, async-flagged
in response when deferred) → persist.
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

### `GET /admin/accounts/{id}/models` → `200`
```jsonc
[ {"id":"qwen3.8-flash","display":"…","owned_by":"opencode",
   "openai":1,"anthropic":1,"responses":0,"tested_at":"…"} ]
```
`null` = untested (the UI must render three states: ✓ / ✗ / ?).

### `GET /admin/accounts/{id}/quota` → `200`
Live `ReadQuota` → classified shape + `raw`. `{"kind":"none"}` when the provider has no endpoint
(`404`/`405` → NOT an error).

## M3 — combos, tokens, export

### `GET|POST /admin/combos` · `GET|PATCH|DELETE /admin/combos/{id}`
```jsonc
{ "name":"daily","strategy":"failover","sticky_idle_s":1800,"enabled":true,
  "compression_profile_id":null,
  "hops":[{"account_id":1,"model_id":"gpt-6-luna","weight":1,"enabled":true}, …] }
```
`strategy` ∈ `failover|true_round_robin|strict_round_robin|sticky_last_good|least_used`.
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

### `GET /admin/stream` → SSE
Two event types: `snapshot` (last 20 calls on connect, newest first) and `calls` (each committed
ledger batch).

### `GET /admin/health` · `POST /admin/chat`
`health` counts the same rows the dashboard headline uses; `chat` executes through the **normal
inference path** (router → rules → compression → proxy → ledger) under an admin session — it is
the dashboard's own Chat page.

---

## Conventions

- Timestamps: RFC3339 UTC. `ms` fields are integers ≥ 1 (`ttft_ms` floors at 1).
- `hint` is the only surviving form of a credential: `sk…5E5i` (≤8-char secrets mask fully).
- Paging: `?limit=` (default 100, max 1000) + `?offset=` on list endpoints.
- The frontend is same-origin-served by the Go binary from `web/dist` in production; in dev it
  proxies `/admin`, `/v1`, `/healthz` to `127.0.0.1:20129`.
