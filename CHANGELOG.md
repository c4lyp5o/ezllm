# Changelog

Notable changes, reverse-chronological. Milestone-tagged (M1–M12); full history via `git log`.

## 2026-10-16 — M12 gemini-web provider: cookie auth, oracle, streaming, bridge ✅
The full Gemini **web** protocol as a router provider, ported slot-for-slot
from the reverse-engineered reference (digest: `~/workspace/gemini-web-api-protocol-digest.md`,
design: `docs/gemini-web-design.md`, runbook: `docs/gemini-web-runbook.md`).
Unofficial surface; risk posture accepted by Calypso: cookie material = live
account access, test accounts, no fallback, typed refusals.
- `internal/geminiweb/` — protocol package:
  - `frame.go` — wrb.fr incremental parser (length-prefixed UTF-16-unit
    frames, boundaries at any byte incl. mid-rune). Its own tests caught a
    units-rescan desync across Feeds (fixed with a `scanned` offset) and
    pinned two fixture traps (raw-string `\n`, wrong unit counts).
  - `client.go` — cookie-pair credentials (PSID required, PSIDTS optional),
    `/app` scrape of the five session tokens, `RotateCookies` (PSIDTS rotates
    ~10 min); expired cookies = the distinct `ErrNoAccessToken`.
  - `rpc.go` — batchexecute + `GET_USER_STATUS`: zero-quota auth verdict
    (AccountStatus 1000/1016/…/1060) AND model catalog from one call.
    Catalog ported index-for-index (capacity tiers Plus 4/12 > Pro > Free
    1/12, model-header build incl. the `gemini-gemini-flash` alias quirk).
  - `generate.go` — `StreamGenerate` request construction (81-slot
    inner_req_list, model-header per-turn mutation) + incremental response
    parse (error code → typed `ErrUpstream` 1013/1037/1050/1052/1060,
    metadata [cid,rid], candidates text@[1][0] with artifact cleanup,
    thoughts@[37][0][0], completion@[8][0]==2, final chunk@[25]) and
    appended-tail delta emission with an incremental UTF-8 decoder
    (`utf8.FullRune` tail test). Tests caught a real double-encode bug
    (`json.RawMessage` inlining where the wire wants a JSON *string*) and a
    final-chunk-before-candidates ordering bug.
- Wiring: kind `gemini-web` + adapter (sentinel base `internal://geminiweb`
  rewritten to the bridge, account+key stamping so bad-key calibration
  exercises the real path, Authorization dropped) + in-process OpenAI-wire
  bridge (`POST /internal/geminiweb/v1/chat/completions`, `GET /models` as
  the auth oracle) + per-account client cache + rotator (8-min cadence)
  persisting via the new `store.UpdateKeyPlain` + typed refusals above the
  package (tools/response_format) + upstream-status→HTTP mapping.
- All mock-proven (29 protocol tests + 4 adapter + 7 bridge-through-sqlite +
  1 store), bite-proven, gates green, `-race` green on touched packages.
  Live cookie proof is operator-gated (runbook).

## 2026-10-15 — M11 gap closure: 1 MiB percentiles, fuzz moat, cancel-latency certificate
- **1 MiB latency percentiles** (`TestLatencyPercentiles`, n=2000, seq):
  stream total p50 9.14 ms / p95 15.65 / p99 16.09, TTFT ≈ total (the gigabyte
  body's parse dominates first byte); non-stream p50 9.77 / p95 16.74 /
  p99 40.15 (one 596 ms max — a swap/GC spike on the loaded box, reported as
  measured). These are e2e totals incl. the mock round trip.
- **Router-only percentile test** (`TestInspectRoutePercentiles1MiB`, n=2000)
  grades spec §18's "inspection+routing, ≤1 MiB, p95 ≤ 5 ms" ISOLATED from
  e2e noise: p95 4.75–5.28 ms across 3 runs — **straddling the target inside
  the noise band** (direct 4.75–5.07, combo 5.07–5.28). Honest grade: NOT
  certified; single-parse's map decode is the cause (old struct-decode ~1.7 ms
  mean, new map-decode ~4.0–4.5 ms mean). The trade stands (e2e −25%) but the
  row is recorded as boundary-UNCERTAIN, exactly as instructed by the spec
  itself ("revise targets based on measurement").
- **Fuzz moat** for the single-parse hot path: 4 targets, seed corpus +
  60 s live fuzz each (FuzzSwapParity: 467 306 execs, 0 divergence). Two
  findings worth their keep: (1) legacy `SwapModel` standing alone TOLERATES
  trailing garbage (dec.Decode stops after the first value) while
  `extractModel` rejected it — the old pipeline rejected such bodies at the
  gate, the new pipeline rejects at `jsonbody.Open`, so PIPELINE parity is
  what the fuzz asserts (oracle = a reimplementation of the old extractModel
  gate); (2) exponent literals overflowing float64 (`1e700`) survive the
  round trip via json.Number — the crasher is now a permanent seed.
- **Cancel propagation certified** (`TestCancelPropagationLatency`, n=1000,
  hard assertion in the test): client-cancel → upstream-cancel **p50 22 µs /
  p95 63 µs / p99 119 µs / max 216 µs** vs the spec's ≤ 20 ms. The §18 row
  now has a measurement instead of a 5 s correctness bound.
- Verified: gofmt/vet/build/full `go test ./...`/-race proxy+jsonbody all
  rc=0.

## 2026-10-15 — single-parse dispatch (M10, perf)
- `perf(proxy,server,compress)` the request body was JSON-decoded up to **5×**
  per request (`extractModel`, `IsStream` per attempt, `SwapModel` per hop,
  `compress.Apply`, `captureShape`). Now: **one decode** — new
  `internal/jsonbody.BodyDoc` (map + `UseNumber`, trailing-garbage rejection),
  opened once in `inference()` and threaded through compression, shape capture
  and every native hop's model swap (`ForwardCandidatesDoc`,
  `prepareHopBody(body, rt, doc)`). Translation legs, multipart STT and admin
  combo-test keep the byte path — `doc` is an optional hint with a legacy
  fallback (no flag; byte-equality tests are the rollback proof).
- Measured (same box as M1 baseline): e2e **−25.0%** non-stream / **−26.1%**
  stream at 1 MiB seq, −10…−16% par4/par16, −13.7% stream par16 at 64 KiB;
  e2e allocs ~748→~690/op. Honest negative: `InspectAndRoute` in ISOLATION
  got heavier (the one shared parse is a full map decode where the old
  single-field struct decode skipped the messages array) — the request-level
  metric is the one that counts. All numbers + the two bite proofs in
  `docs/single-parse-dispatch-design.md`.
- `prefix_sha` now sees JSON number literals verbatim (`1e10` and
  `10000000000` hash differently, big ids keep exact digits) instead of
  collapsing through float64 — correct for a cache-BREAK detector; documented.
- Verified: full `go test ./...` green with zero edits to existing call
  shapes (doc=nil path exercised by the entire old suite), byte-equality
  tests vs legacy `SwapModel` on 7 fixture classes (unknown fields, big ints,
  unicode, nested tools, dup keys), stream-flag parity, captureShape parity,
  two bite proofs (UseNumber drop → mangled big int; model-assignment drop →
  upstream sees `"model":null`), gofmt/vet/build/race green.

## 2026-10-13 — chat searchable model picker + per-model Test in the providers drawer
- `feat(web)` chat model `<select>` → **searchable combobox**: typing filters by model id
  OR label (namespace prefix included), ↑/↓ highlight, Enter picks, Escape/click-outside
  closes; empty states (loading / no models / no match) preserved, same input classes so
  the controls row does not move. Built for the "too many models to scroll" complaint.
- `feat(web)` providers models drawer gets a **`test` column**: a per-row `Test` button
  probes JUST that model (the sync endpoint's `models` scope, force re-test) instead of a
  whole `Test all models` sweep. Curated rows disable the button with an explanation —
  probe writes cannot overwrite an operator-curated verdict (server-proven), so clicking
  would only spend. While a sweep runs, the row button spins and the header line carries
  the clock; a scoped probe of an id that left the live catalog reports THAT as the cause.
- `api.ts` `startSweep` accepts `models?: string[]` (scoping the sweep; the catalog
  refresh still upserts the FULL list — a one-model test never prunes the drawer).
- Verified: `tsc`+vite green; live browser walk at 390×844 — all 8 routes
  `scrollWidth == clientWidth == 390`, `scrollX` stays 0, models drawer shows the 4-column
  table with 80 Test buttons and no overflow; combobox filter/enter/arrow/escape all
  exercised in the live page.

## 2026-10-13 — Cloudflare Workers AI key test fixed (account-aware oracle + native catalog)
- `fix(provider,registration)` adding Cloudflare Workers AI failed with `cannot verify this
  key: no credential-checking endpoint and no model to probe with / This gateway exposes
  neither /usage nor a catalog`. Two separate misses, both live-measured 2026-10-13:
  - **Oracle** — the generic `GET {base}/usage` does not exist there (405), and
    Cloudflare's real check lives outside the account base URL:
    `GET https://api.cloudflare.com/client/v4/user/tokens/verify` → 200 `{"status":"active"}`
    for a valid token, **401** for a well-formed `cfut_`+40-hex fake, but **400 format
    error** for a malformed fake — which calibration would have read as inconclusive.
    New optional `HostedAuthOracle` interface gives the adapter the ACCOUNT so
    openai-compatible can return a host-scoped oracle; `AuthProbe.CalibrationKey` carries
    the format-valid fake; `doProbe` now accepts an absolute `http(s)` Path (used verbatim,
    everything else still joins at `UpstreamURL`).
  - **Catalog** — the OpenAI-compat prefix has NO `/models` (405 live), and the native
    `GET /client/v4/accounts/{id}/ai/models/search` answers the Cloudflare envelope
    `{"success":true,"result":[...]}` which the OpenAI parser reads as ZERO models.
    `isCloudflareAI` (kind+https+host+`/ai/v1`, OllamaCloud parity) switches `ListModels`
    to the native envelope, skipping id-less entries.
- Non-Cloudflare openai-compatible accounts are untouched (explicit regression test), and a
  `success:false` catalog envelope now surfaces the provider's own words instead of
  passing as an empty catalog.
- Verification: 5 new tests (e2e key test through the real flow + detector + oracle shape +
  envelope parse + non-leak), **bites 3/3 fell for the right reason** (hosted oracle
  removed / CalibrationKey ignored / catalog branch removed), full suite green.

## 2026-10-13 — NVIDIA NIM key-test fix: measured-fast candidates, honest timeout verdict
- `fix(registration)` adding NVIDIA NIM failed with `generation probe failed: Post
  "https://integrate.api.nvidia.com/v1/chat/completions": context deadline exceeded`, and
  the hint blamed a provider rejection for a response no provider ever sent. NIM's shared
  free tier answered `deepseek-ai/deepseek-v4.1-flash` (the generic buckets' FIRST pick, by
  name) with nothing inside 90s and `z-ai/glm-5.3-flash` with 43.7s, against
  `defaultStepTimeout = 15s`. Ledger evidence for the failed add: `admin:test …
  deepseek-ai/deepseek-v4.1-flash status=0 15000ms`.
- `pickProbeModels` now leads with `nvidiaNIMProbeModels` on this host (host-scoped like
  `isOllamaCloud`, preset base URL `https://integrate.api.nvidia.com/v1`):
  `nemotron-3.5-lightning-30b-a3b` 0.6s · `llama-3.2-11b-vision-instruct` 1.7s ·
  `gpt-oss-20b` 6.2s — measured, matching the probe playbook's "pick by measurement, never
  by 'flash'/'mini' in the id". Each entry is matched against the account's own catalog; a
  full miss falls through to the generic buckets (NIM rotates ids: `meta/llama-3.1-8b-instruct`
  went EOL 2026-08-26 while still listed).
- A transport error/timeout in the generation probe is no longer `probeFailed` with a
  rejection hint: it carries an honest "did not answer within 15s" verdict and ADVANCES to
  the next candidate; only the last candidate's timeout fails the test. 401/403 and 4xx
  verdicts are unchanged.
- NIM's `/v1/models` verified PUBLIC (200 + full catalog for a garbage key), so the catalog
  can never prove a credential — the generation probe stays the gate.
- Verification: 5 new tests (2/2 bites fell for the right reason), full suite green, and a
  live scratch-instance proof through the real handler — `scripts/prove_nim_keytest.py`,
  201 in 2.1s, `inference: nvidia/nemotron-3.5-lightning-30b-a3b · 766ms`, ledger row
  `status=200`.

## 2026-10-13 — provider presets: NVIDIA NIM, Cloudflare Workers AI, OpenRouter
- Dashboard add-provider presets: **NVIDIA NIM** (`https://integrate.api.nvidia.com/v1`),
  **OpenRouter** (`https://openrouter.ai/api/v1`), and **Cloudflare Workers AI**, whose
  account ID composes the endpoint `https://api.cloudflare.com/client/v4/accounts/{id}/ai/v1`.
  All three are ordinary `openai-compatible` accounts — Workers AI serves the whole OpenAI
  family under that prefix (chat, responses and an anthropic-compatible `/messages`), so no
  new protocol was needed. The `cf-aig-gateway-id` header only applies to Cloudflare's AI
  Gateway (`/ai/run`), not to these endpoints.

## 2026-10-13 — strategy citizenship, the hold checkbox, request IP (M7; local, not pushed)
- `feat(router)` two operator decisions on the universal matrix: translated hops are ordinary
  combo members (the old "native hop always wins" tail rule is removed), and
  `translation.universal_enabled` is the SINGLE switch for the matrix (no longer requires
  `translation.enabled`) (`b1a2f5b`).
- `feat(store,router,web)` schema v14 — `combo_hops.hold_until_failure`: a block_round_robin
  hop the operator checks HOLDS its block while it keeps succeeding and releases it on a
  hop-attributable failure (429/5xx/transport/failed-truncated stream; a 4xx keeps the hold).
  Combo editor checkbox + a `hold` chip in the combo list.
- `refactor(router)` `strict_round_robin` REMOVED (no live combo used it; its success-driven
  advance is now block_round_robin with a held hop).
- `feat(store,server,web)` schema v14 — `calls.ip`: the caller address (X-Real-IP, then the
  first X-Forwarded-For entry, then the socket peer) captured for traffic mapping; shown in
  the dashboard under the request timestamp. Lat/long dropped by operator decision.
- `feat(web)` requests explorer: tokens in/out consolidated into one `tokens ↑↓` column with
  hover titles; the feed's arrows get proper hover titles too.
- Migration rehearsed on a live-DB copy: v11→v14, calls 6052 / accounts 11 / models 624 /
  combos 11 / hops 39 / tokens 6 preserved, zero invented holds or IPs, second boot idempotent,
  production untouched at v11.

## 2026-10-12/13 — universal wire matrix (M0–M6; local, not pushed)
- `fix(translate)` M0: Responses streams lost tool arguments when the item id and call id
  differed, and dropped terminal usage on incomplete/failed runs (`00232c5`).
- `feat(router,proxy,server)` M1: wire identity — `Route.WireSurface`/`EffectiveWire()`, the
  wire decided BEFORE capability/pin gates, typed `wire_unsupported` (400) with zero upstream
  calls for a disproved destination (`14e4730`).
- `feat(translate)` M2: surface-neutral request IR + one codec per surface; unsupported
  features refuse with a reason; absent vs zero preserved (`3a3ab3c`).
- `feat(translate)` M3: shared response/event codecs with the legacy wrapper cutover —
  byte-identical to the pre-cutover package across a 35×3 + 35×3 corpus (`f7ee36b`).
- `feat(store,provider,translate)` M4: `accounts.usage_convention` (schema v13) — the
  operator-declared Messages input-token accounting; a cross-wire Messages destination refuses
  an unstated convention. Also fixed two pre-existing bugs: a partial PATCH blanked `notes`,
  and `UpdateAccount` swallowed non-conflict write errors (`7294755`).
- `feat(proxy,router,server,config)` M5: the three new cells (chat→messages, responses→chat,
  responses→messages) behind `translation.universal_enabled` (default OFF; env rollback), the
  fixed fallback chains, pin-as-intent, and the completion outcome (`Result.StreamOutcome` +
  ledger err; late stream failures withhold the hop-good signal) (`5a145dd`).

## 2026-10-10 — M9: research schema (v8) + honest savings + TTFT + cost
- `fix(proxy)`: TTFT was timed from inside the stream copy — after headers had already
  arrived — so every row read 1–3ms. Now timed from upstream dispatch; the number finally
  means time-to-first-token, which is what caching research measures (`f48d20a`).
- `fix(compress)`: `Saved` was set before the ratio-floor check, so rejected rewrites still
  reported savings (prod week: 18.2k real vs 21.4k notional in one SUM). `Saved` is now 0 on
  any skip; `SavedNotional` carries the rejected potential (`19b743d`).
- `feat(store)` schema v8: request-shape capture (`prefix_sha` of system+tools+history-minus-
  newest-turn, `msg_count`, `tool_count`, `session_id` via `X-Ezllm-Session`, `req_bytes`) —
  the cacheability data `raw_usage` could never reconstruct; `compression_saved_notional`;
  versioned `model_prices` (account×model×date) with cost DERIVED at query time, unpriced
  calls counted not zeroed (`393460b`).
- `feat(server)`: capture runs on the inference path after compression (a history rewrite
  measures as the cache break it is); admin API `GET /admin/prices`,
  `PUT/DELETE /admin/accounts/{id}/prices`, `GET /admin/cost`; requests projection surfaces
  the shape columns (`0e24e32`).
- `feat(web)`: Economics cost panel (spend, priced vs unpriced, per day/account/model) +
  Providers Pricing drawer (per-account rate versions; all four rates required) (`ec27973`).

## 2026-10-09/10 — providers batch, UI cleanup pass, surface pin (deployed 2026-10-10)
- `feat(provider)`: fourth kind `gemini-openai` — Gemini's OpenAI-compat endpoint; its bad-key
  signal is `400`, honored by the auth gate only for that kind via the optional
  `AuthRejectStatuser` (`a8776ee`).
- `feat(web)`: provider presets — Command Code, DeepSeek, TokenRouter, Gemini, Groq built-in
  (`5335423`, `1d3f3d7`, `fb2a2fd`).
- `feat(registration)`: dropped all API-key **shape** checks; the probe chain is the sole
  validator, so exotic key formats are judged by the upstream (`9aa9cf1`).
- `fix(server)`: `GET /healthz` is liveness-only (`{"status":"ok"}` after a DB ping) — the
  public route no longer leaks counts, paths, or versions (`9a7b620`).
- `feat(web)`: dashboard UI cleanup pass — shared `ConfirmModal` replaces `window.confirm`
  and one-click deletes; Providers key status shown as text (`tested ok` / `test failed` /
  `untested`) not a bare dot; quota copy "quota not available"; Connect drops the Responses
  surface from the picker (server route unchanged), 2-column desktop layout; Dashboard strip
  trimmed (no ledger/dropped/schema chips, no context column); combo "custom model id" flag now
  defaults from what's actually rendered, and the profile-number pill is gone (`c099dd5`…`9915582`).
- `feat(pin)`: per-model **surface pin**, enforce-only (schema v7 `models.proto_pin`). A
  wrong-surface call to a pinned model is refused `400 pinned_surface` before any upstream
  dial; never a reroute (bodies pass through untranslated). Lookup fails closed; catalog
  resyncs preserve pins (`340442b`).
- `fix(server)`: the pin refusal reached clients as `502` while the ledger logged `400` —
  the proxy error switch had no `ErrPinned` case. Now `400 pinned_surface` with structured
  `{pinned, requested_surface}`; live-proved on prod (`03a6317`).
- `build(web)`: **rebuilt and committed `web/dist`** — the image embeds it, so source-only UI
  commits shipped the old bundle; after every UI commit, `npm run build` + commit `dist` +
  verify the served hash post-deploy (`831ecda`).
- GitHub Copilot: OAuth app registered, device flow works end-to-end, but the
  `copilot_internal/v2/token` exchange returns `403` for third-party apps → parked, not
  circumvented. OAuth artifact deleted.

## 2026-10-09 — post-ship fixes
- `fix(proxy)`: strip client `Origin` before forwarding — Anthropic reads any browser `Origin`
  as CORS and rejects with a 401; regression-tested (`8c832ef`).
- `fix(web)`: clear the dashboard session only on ezllm's own auth 401; provider 401s surface
  inline (`358cda3`).

## 2026-10-08 — Docker, dashboard polish, budget engine
- Distroless image + home-box Traefik overlay; explicit router priorities and a dedicated
  `/admin/login` limiter (`3f46f00`, `3155b76`).
- `POST /admin/chat` behind the real routing path; Chat page, Token Economics, budget controls
  (`d0f8407`, `d8d0f68`).
- Budget stage with its own context-token accounting (schema v5) (`39cbdb8`).
- Combo retry with exponential backoff before failover; `EZLLM_*` env overrides restored for
  containers; per-combo enable toggle; enabled combos advertised in `/v1/models`.
- Provider add-in-one-dialog (key validated on save) with a Claude Platform preset; lm-eval
  canary proving traffic goes through the gateway.
- MIT license.

## 2026-10-07 — compression, rules, live surface
- Compression: M5 pipeline (engines, profile CRUD, inference wiring); headroom + lite engines
  (M6.5 phase 2); caveman lossy prose condensation, gated by construction (M7).
- Rules eligibility engine — token caps + allowed-use windows; dashboard rules editor.
- Live recent-requests feed (SSE over fetch); stats explorer (granularity × range); Requests
  page with token dissection.
- Dashboard password + plaintext provider keys (explicit operator choice).

## 2026-10-06 — core: M1–M4
- M1: skeleton + health.
- M2: store + field crypto + three-surface passthrough (`/v1/chat/completions`, `/v1/messages`,
  `/v1/responses`), usage normalized into the SQLite ledger.
- M3: admin API + embedded dashboard; registration with capped key-test protocol sweep; live
  model-catalog probes; adapters (`opencode-go`, `openai-compatible`, `anthropic-compatible`).
- M4: combos resolve to ordered candidates + strategies; proxy attempt/commit split (failover
  that never leaks a failed hop); admin persistence (accounts, provider keys, combos, tokens).