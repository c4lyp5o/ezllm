# EXPLORING ezllm — Calypso's codebase tour notes

> **How to use this file:** it's your map for reading ezllm's code — reading order, invariants
> to verify, and per-milestone file landmarks (§4). M1–M8 are all filled in; keep a milestone's
> landmarks updated as its code evolves (that's the standing rule from the plan).
>
> **Source of truth:** the v1 rev4 plan
> `~/workspace/.hermes/plans/2026-10-05_223000-ezllm-v1-rev4-namespace-compression.md` (it
> supersedes the v0 plan; the M3 key-test design lives beside it in
> `2026-10-06_070000-ezllm-m3-keytest-design.md`). This file = navigation; those = decisions.

---

## 1. 60-second mental model

```
client (Claude Code / Hermes / curl / anything OpenAI- or Anthropic-speaking)
   │  /v1/chat/completions · /v1/messages · /v1/responses   (bearer or x-api-key = client token)
   ▼
┌──────────────────────── ezllm (Go, 127.0.0.1:20129) ──────────────────────┐
│ server  → auth → router (combo name OR namespace/model → ordered hops,    │
│            surface-pin gate → strategy + rules eligibility + key pick +    │
│            cooldown)                                                       │
│        → compression (profile stages, fail-open, final turn exempt)        │
│        → proxy (model swap, byte-passthrough SSE, usage sniff, failover)   │
│        → ledger (async SQLite row) + dashboard feed (SSE)                  │
└────────────────────────────────────────────────────────────────────────────┘
   │                           │
   ▼                           ▼
provider accounts (one namespace each): opencode-go · openai-compatible ·
anthropic-compatible · gemini-openai — each ships an adapter (PrepareRequest,
StripHeaders, ReadQuota, ListModels); the account row carries the base URL +
key pool.
```

Several real upstreams, not 359. The whole point: **SSE bytes flow back untouched** — ezllm swaps
`model` upstream and copies the response stream while tapping the `usage` frames for the ledger.
Compression is the only thing that ever rewrites the request body (when a profile is active).

## 2. Quick start (run these before reading code — read after seeing it live)

```bash
cd ~/workspace/ezllm
make build && make run          # or: go run ./cmd/ezllm -config config.yaml
curl -s localhost:20129/healthz
curl -s localhost:20129/v1/models | python3 -m json.tool
# real call (streaming, watch it print incrementally):
curl -N localhost:20129/v1/chat/completions \
  -H "Authorization: Bearer $EZLLM_TOKEN_HERMES" -H "Content-Type: application/json" \
  -d '{"model":"opengo/qwen3.8-flash","messages":[{"role":"user","content":"count 1-5"}],"stream":true}'
go run ./cmd/m2verify    # acceptance checker: prints PASS/FAIL per invariant
# raw SQL equivalent (note: no sqlite3 CLI on this box — use `go run ./cmd/m2verify`):
#   SELECT ts,client,surface,account,model,status,ttft_ms,total_ms,
#          tokens_in,tokens_out,tokens_cached_read,tokens_cached_write
#   FROM calls ORDER BY id DESC LIMIT 5;
```

**Expect:** `ttft_ms` noticeably smaller than `total_ms` (that gap = streaming working; if they're
equal, something is buffering — go read `proxy/sse.go` and find out why).

## 3. Reading order (per milestone)

| # | Read first | Then | Ask yourself |
|---|-----------|------|--------------|
| M1 | `cmd/ezllm/main.go` → `internal/config` → `internal/server` | `config.example.yaml` | Where would I add a fourth account? (config seeds; the DB is runtime truth) |
| M2 | `internal/proxy/proxy.go` (`Forward` → `streamCopy` → `tapSSELine`) → `internal/store/ledger.go` (`NormalizeUsage`) | `git show` of M2 | Does ANY code path touch response bytes besides the scanner+Write+Flush loop? |
| M3 | `internal/registration` + `internal/provider/adapters.go` + admin handlers in `internal/server` | `docs/API.md` | Can I trace an account from DB row → adapter → first upstream byte? |
| M4 | `internal/router` (hops → strategies → key pick/cooldown) + `internal/rules` | `go test ./internal/router ./internal/rules` | What happens on 429 with 1 key left? (answer must be in a test) |
| M5 | `internal/compress` (engines + stage pipeline) + `internal/server/compression.go` | `docs/compression-design.md` | Where is "never touch final turn / tool results" enforced, and where does fail-open live? |
| M6 | `internal/store/usage.go` → `/admin/usage` · `/admin/requests` · `/admin/stream` + `web/src` | `docs/API.md` + a dashboard login | Can I kill -9 it and restart without corrupting the ledger? Does the dashboard render anything pre-auth? (it must not) |
| M7 | `internal/compress/rtk.go`, `caveman.go` + `internal/rules` windows/caps | `scripts/prove_*.py` (live proofs) | After compression, do ledger rows still carry faithful pre/saved token counts? |
| M8 | `internal/router/router.go` `checkPin` + `internal/server/admin.go` `handleSetModelPin` | `internal/router/pin_test.go`, `internal/server/pinsurface_test.go` | Where does a refusal stop the request — before or after the upstream dial? Does every switch that renders a typed router error have a case for `ErrPinned`? |

## 4. Milestone landmarks

*(5–10 bullets per milestone: file → line range → what's there. Half a screen each max.
No narration, just landmarks. M1–M8 are all filled — keep them current as the code evolves.)*

### M1 — skeleton + health ✅ (2026-10-05, verified)
- `cmd/ezllm/main.go:39` flags (`-config`, `-addr`), slog level via `EZLLM_LOG_LEVEL`
  (`:43`, env-only by design — must work before config is read), addr precedence
  `-addr` > `EZLLM_ADDR` > config, `http.Server` (10s ReadHeaderTimeout,
  **no WriteTimeout on purpose** for SSE), SIGINT/SIGTERM → 15s drain shutdown.
- `internal/config/config.go:96` `Load` / `:113` `Validate` — collects ALL problems into one
  error (missing env vars reported by NAME, never value); defaults applied here
  (`DefaultDataDir = "data"` at `:22`, cooldown defaults/validation at `:209`).
- Auth moved out of config (M2+): the server resolves tokens through the store —
  `Auth` interface (`internal/server/server.go:44`, `ClientByToken`) implemented by
  `internal/store/catalog.go` (tokens matched by SHA-256 hash, `HashToken`).
- `internal/server/server.go:81` `New` — route table starts at `:112`: `GET /healthz` (public,
  liveness-only since 2026-10-10), `GET /v1/models`, `POST /v1/chat/completions` (both `authed`).
- `internal/server/server.go:183` `Handler()` — stack = `accessLog(recoverer(mux))`.
- `:632 authed` → writes `reqInfo.client` (holder created by accessLog at `:722`; client set at
  `:647` — this wiring was buggy in the first cut, log showed `client=""`; fixed + re-verified).
- `:690 statusWriter` — records status/bytes, **`Flush()` forwarded** (the M2 SSE
  dependency lives here, not in the proxy).
- `internal/config/config_test.go` — 4 tests: valid+defaults, aggregate-error, bad fallback
  provider, duplicate token.
- **Live-verified:** healthz 200 · models 200 w/ token (`client=hermes` in log) · 401 w/o ·
  chat 501 stub · bind `127.0.0.1` only · SIGTERM → exit 0 · 0 secrets in log.
- **M1 invariants proven:** #1 no-secrets-in-logs (grep: 0 hits) · #6 graceful shutdown ·
  #7 loopback default. (#2 SSE, #3 passthrough, #4 retry policy, #5 ledger async = M2/M4)

### M2 — store + crypto + 3-surface passthrough ✅ (2026-10-06, LIVE-verified vs 2 real providers)
**Where to look first:** `internal/proxy/proxy.go:333 Forward` — it is the whole request path in
one function. Then `store.NormalizeUsage` (`ledger.go:91`) for the accounting rules.

- `internal/store/schema.sql` — 11 tables + `proto_pin` (v7), embedded via `go:embed`. **FK order matters**:
  `compression_profiles` is declared *before* `combos` (combos references it). `calls` and
  `quota_snapshots` are append-only/immutable; `account`+`key_hint` are denormalized onto `calls`
  so history survives key/account deletion (proved by `TestLedgerRowsAreImmutableHistory`).
- `internal/store/store.go:56 Open` — WAL, `busy_timeout=5000`, `foreign_keys=ON`,
  **single writer conn (`SetMaxOpenConns(1)`) + read pool**. A write pool would only manufacture
  `SQLITE_BUSY`. `migrate()` at `:163` is idempotent and refuses to start on a *newer* schema
  (`schemaVersion` const at `:27`; the v7 `alterModelProtoPin` migration at `:234`).
- `internal/store/crypto.go:39 NewFieldCrypto` — scrypt(N=32768,r=8,p=1) → AES-256-GCM,
  wire format `enc:v1:<iv>:<ct>:<tag>`. `KeyHint` (`:122`) masks anything ≤8 chars rather than
  truncating, so a hint can never reconstruct its key. Master key is loaded from OUTSIDE `data/`.
  **Not wired in v1** — provider keys ship plaintext by explicit choice (commit `dfe323c`); this
  path is kept for a future hardening pass.
- `internal/store/ledger.go:204 RecordCall` — **non-blocking by design** (drops + counts rather
  than adding latency to a stream; invariant #5). `Flush()` (`:293`) uses an **in-band barrier on
  the same FIFO channel** so it's deterministic — an earlier 2-channel version let `select` serve a
  flush while rows were still queued, silently losing ledger rows.
- `internal/store/ledger.go:91 NormalizeUsage` — **THE accounting decision.** Three surfaces
  disagree about cache: OpenAI includes `cached_tokens` in `prompt_tokens`; Anthropic EXCLUDES
  `cache_read` from `input_tokens`; Responses includes cached and is the only one reporting
  `cache_write`. Read the doc comment before touching this.
- `internal/provider/provider.go:101 Adapter` — the 4-method contract (`PrepareRequest`,
  `StripHeaders`, `ReadQuota`, `ListModels`). Adding a provider = one file + one `Kind`.
- `internal/provider/adapters.go:39 openCodeGo.PrepareRequest` — injects a **per-request**
  `x-opencode-session` (a shared one grows server-side until it blows context) and strips any
  client-supplied copy. `:168 anthropicCompatible` is its own kind because auth is `x-api-key`,
  not Bearer. `gemini-openai` (M8) reuses the openai-compatible adapter shape; its quirk lives in
  the probe gate (`AuthRejectStatuser`), not the adapter.
- `internal/provider/quota.go:22 parsePercentQuota` / `:57 parseMoneyQuota` / `:110 ClassifyQuota`
  — quota shapes differ per provider, so the result is discriminated (`percent|money|tokens|none`)
  and the verbatim JSON is always kept.
- `internal/proxy/proxy.go:197 SwapModel` — `map[string]any` round-trip with `UseNumber()`;
  only `model` changes, all 16 observed provider fields survive, big ints don't become floats.
  Rejects `null` explicitly (assigning into a nil map panics — a client could have crashed us).
- `internal/proxy/proxy.go:500 streamCopy` — scanner + `Flush()` per line, re-emitting the `\n`
  the scanner strips so forwarding stays byte-exact. Over-long lines fall back to a raw `io.Copy`.
- `internal/proxy/proxy.go:578 tapSSELine` + `:49 UsageTapper` — **merges** usage across events
  rather than taking the first hit. Two live-only bugs hid here: Anthropic's `message_start` has
  `output_tokens:0` (final totals come in `message_delta`), and a `max_output_tokens`-truncated
  Responses stream ends on `response.incomplete`, not `.completed`.
- `internal/router/router.go:172 Resolve` — combo name → `<namespace>/<model>` → else 404 **with
  hints** listing which namespaces serve that model. **No default account** (a bare model id is
  ambiguous: `gpt-6-luna` exists on both his providers).
- `internal/server/server.go` routes table (`New`, from `:112`) — three POSTs + `/v1/models`
  (+`?protocol=anthropic` shape) + admin surface. `inference()` (`:378`) is one shared body for all
  three surfaces. `bearerToken` (`:746`) accepts Bearer *and* `x-api-key` (Claude Code).
- `cmd/m2verify/main.go` — acceptance checker that queries the live DB and prints PASS/FAIL per
  invariant. Run `go run ./cmd/m2verify` after any live soak.

**Live-verified 2026-10-06** (opencode-go 36 models + ssn-gpt 11 models synced at boot, 0 warnings):
- `/v1/chat/completions` → 200, and streamed: **TTFB 1.28s vs total 4.08s** = unbuffered
- `/v1/messages` with `x-api-key` → 200 **tool use** (`stop_reason: tool_use`, real `tool_use` block)
- `/v1/responses` → 200 `status: completed` with full usage incl. `cache_write_tokens`
- `super-ssn/gpt-6.1-sol` → 200 (second provider, second adapter kind)
- Cached repeat: raw `prompt=2496 cached=2048` → ledger **`tokens_in=448 + cached_read=2048`** ✅
- Bare `gpt-6-luna` → 404 listing **both** `opengo/` and `super-ssn/` ✅
- Keys at rest: **plaintext** (`key_plain`) — explicit operator choice; the crypto path
  (`internal/store/crypto.go`) is implemented + test-covered but deliberately unwired — hints only;
  `dropped_rows=0`; `CGO_ENABLED=0` static build ✅

**M2 invariants proven:** #1 no secrets in logs/responses (grep + `TestNoSecretsInResponses`) ·
#2 SSE unbuffered (flush-count + staggered-arrival test, live TTFB) · #3 unknown fields survive ·
#5 ledger never blocks (`TestRecordCallIsNonBlocking`, 5000-row flood) · #7 loopback default.
(#4 4xx-never-retried and #6 graceful-shutdown-under-load land in M4/M7.)

**Bugs found & fixed while building M2** (all had passing unit tests at the time — they only showed
up live or under `-count=3`): `Flush()` losing rows · usage window `ts < now` dropping fresh rows ·
empty-200 on upstream death (now 502 + ledgered) · `UpsertAccount` returning id 0 on the upsert path
· boot seeding passing `ID=0` into `PickKey` (silently hid the catalog) · Anthropic/Responses
streaming usage taps · TTFT truncating to 0 (now floored at 1ms).

### M3 — namespaces, registration + paced key test, protocol sync, smart quota ✅
- `internal/store/schema.sql` — `accounts`, `provider_keys`, `models`, `client_tokens`, `calls`;
  keys stored plaintext in `key_plain` (explicit operator choice, commit `dfe323c`); `key_hint`
  is the only key material any API returns.
- `internal/registration/registration.go` — paced registration flow (step chain + `oracle`);
  a new key is tested before it joins a pool.
- Namespaces: model ids are `<account-slug>/<model>`; the account slug is the routing key;
  the same upstream model on two accounts = two distinct ids.
- Protocol sync: `/v1/messages` + `/v1/responses` mirror the chat catalog and auth; one router
  behind three surfaces; `?protocol=anthropic` on `/v1/models` for the Claude-shaped list.

### M3.5 — opencode-go adapter + aliases ✅ (superseded by namespaces)
- `internal/provider/adapters.go` — `Kind` switch: `opencode-go`, `openai-compatible`,
  `anthropic-compatible`; adapter contract: `PrepareRequest`, `StripHeaders`, `ReadQuota`,
  `ListModels`.
- Aliases (`ez/omen` → account+model) were the M3-era routing shortcut; **superseded by
  namespace routing in M4** — legacy fields linger in the ledger; routing never guesses.

### M4 — combos, strategies, key pool + cooldown ✅
- `internal/router/router.go` — combo OR `ns/model` resolution → ordered hops; strategy switch:
  `failover` / `true_round_robin` / `strict_round_robin` / `sticky_last_good` / `least_used`;
  failover retry budget; 4xx passthrough (never retried).
- Key pool: per-account key selection + cooldown after 429/failure (skipped until expiry).
- `internal/rules/engine.go` — token caps (rolling window) + allowed-hours; combo hops that
  violate a rule are skipped silently; direct calls get a 429 naming the rule.
- Admin surface: `/admin/accounts` (incl. key pools), `/admin/combos`, `/admin/model-rules` CRUD.

### M5 — compression: engines, stages, profiles ✅
- `internal/compress/compress.go` — engine registry: `session_dedup`, `rtk`, `headroom`, `lite`,
  `caveman`, `budget`; ordered stage pipeline with per-stage options; fail-open + final-turn /
  tool-result exemption enforced here (see §6).
- `internal/server/compression.go` — `/admin/compression-profiles` CRUD; per-request profile
  resolution (`x-ezllm-compression` header → combo default → off).
- `internal/compress/caveman.go`, `rtk.go` — the aggressive pair (levels / surface-aware);
  live proof scripts in `scripts/prove_*.py` (real runs, not unit mocks).

### M6 — dashboard + live surfaces ✅
- `internal/server/dashboard_auth.go` — bcrypt dashboard password + sessions; seeded default
  password must be changed on first login; `/admin/login`, `/admin/settings/password`.
- `internal/server/spa.go` — `go:embed`-served SPA; nothing renders or fetches pre-auth.
- `internal/server/stream.go` — `/admin/stream` SSE (snapshot + call events); dashboard pages:
  Dashboard, Providers, Combos, Rules, Compression, Requests, RecentFeed, Chat, Connect,
  Economics, Stats, Settings.
- `/admin/usage` + `/admin/requests` + `/admin/export` — rollups (group_by …), token-dissection
  explorer, disaster-recovery dump.

### M7 — shipped, hardened, live ✅
- Post-ship fixes (2026-10-09): `Origin` added to `stripAlways` in the proxy (Anthropic 401s on
  browser origins; regression-tested — commit `8c832ef`); SPA clears session only on our
  numeric-code 401, provider 401s surface inline (commit `358cda3`).
- Deployment: `ghcr.io/c4lyp5o/ezllm` image; compose base (port 20129) + `compose.traefik.yml`
  overlay (`llm.calypso.dedyn.io`, `/admin` guard); served bundle verified against `web/dist`.
- `cmd/m2verify` remains the end-to-end acceptance checker against live traffic; every landmark
  in this file re-verified against code on 2026-10-10 (line numbers were stale by ~400 lines in
  places — that's why §4 says re-verify, not trust).

### M8 — providers batch + surface pin ✅ (2026-10-10, live-proved on prod)
- `internal/provider/provider.go:26` `KindGeminiOpenAI` — 4th kind (`ValidKinds()` at `:66`).
  Its bad-key signal is `400` (Google says "Please pass a valid API key"), accepted as an auth
  rejection **only for this kind** via the optional `AuthRejectStatuser` interface
  (`internal/provider/probe.go`); the auth gate reads it, the inference step still treats only
  401/403 as auth failure (a 400 there can mean a malformed request).
- `internal/registration/registration.go` — all key **shape** checks deleted (`9aa9cf1`);
  `StepFormat` survives only as the unregistered-kind failure (`:202`). The probe chain is the
  sole validator.
- `internal/server/server.go:240` `handleHealth` — liveness-only `{"status":"ok"}` after a 2s
  DB ping (503 `{"status":"down"}` on failure); `TestHealthIsPublic` greps the body for leak
  fields. The detail moved to `GET /admin/overview` (`internal/server/admin.go:195`).
- **Surface pin** (`340442b`, schema v7): `models.proto_pin` (`schema.sql:92`), migration
  `alterModelProtoPin` (`store.go:234`), store API `ModelPin`/`SetModelPin`
  (`internal/store/catalog.go`), router gate `checkPin` (`router.go:91`, called at `:222`
  direct + `:315` combo — **before any upstream dial**; lookup errors fail CLOSED; resync
  preserves pins via snapshot/restore in `UpsertModels`). Admin write
  `PUT /admin/accounts/{id}/model-pin` (`server.go:143`, handler `admin.go:744` — model id in
  the body because ids contain `/`; `pin` is `json.RawMessage` so absent/""/value are three
  states; unconfirmed surface → 409 unless `force`). Client-facing refusal:
  `400 pinned_surface` (`server.go:421`) — **never a reroute**: ezllm passes bodies through
  untranslated, so switching the path would send the wrong shape to upstream.
  - Trap found by live prod test (`03a6317`): `errorStatus()` mapped `ErrPinned`→400 for the
    LEDGER, but the proxy error switch had no case and answered clients 502. Text-only tests
    passed. Any new typed router error needs a case in EVERY switch that renders it.
- UI (`9915582` etc.): `ConfirmModal` (`web/src/ui.tsx`) replaces `window.confirm` everywhere;
  Providers key status is text (`tested ok`/`test failed`/`untested`, still driven by
  `last_test_ok`); pin select lives in the model drawer (auto + three surfaces, unconfirmed
  goes through force-confirm); Connect shows two surfaces (Responses dropped from the PICKER —
  the server route `POST /v1/responses` still exists, `server.go:117`).
- **`web/dist` ships the UI.** The image build embeds it (`go:embed`, Dockerfile comment at
  `:8-9`), so a `web/src`-only commit deploys the OLD interface with every test green. Rebuild
  + commit dist in the same push; after deploy verify the served hash
  (`curl https://llm.calypso.dedyn.io/ | grep assets/index-`) + grep one marker string.

## 5. Invariants to check while reading (review checklist)

These are the "if it's broken here, the project failed" spots — each must be visibly true in code:

1. **No secret ever reaches a log line** — grep the logging calls for `Authorization`, key env
   names, `Bearer`. Config has `env:` indirection only.
2. **SSE is never buffered** — no `bufio.Scanner` over the response, no full-body `ReadAll` on the
   stream path; `Flush()` called after each chunk to the client.
3. **Unknown request fields survive** — `extra_body`, `enable_thinking`, `thinking` pass through;
   the only mutation is `model`.
4. **4xx ≠ retry** — only 429/5xx/timeouts trigger cooldown + next hop; a 400 (bad request) goes
   straight back to the client. Retrying a 400 burns quota for nothing.
5. **Ledger writes can't block the response path** — async/batched; a slow SQLite must never add
   latency to a token stream.
6. **Graceful shutdown** — SIGTERM finishes in-flight streams (bounded), then closes the DB.
7. **Loopback by default** — binds `127.0.0.1` unless `EZLLM_ADDR` says otherwise; token gate on
   `/admin/*` with nothing rendered pre-auth.

## 6. Key facts you'll need while exploring (verified through 2026-10-10)

- Go **1.27.1** at `~/.local/go/bin/go` (PATH wired; apt's 1.22 is shadowed fallback)
- Keys (0600, never in repo/config): `~/.hermes/secrets/opencode-go.txt`,
  `~/.hermes/secrets/ssn-gpt.txt`, `~/.hermes/secrets/bailian-coding-plan.txt`,
  `~/.hermes/secrets/xiaomi-mimo-token-plan.txt`;
  ezllm client/admin tokens: `ezllm-infer.token`, `ezllm-admin.token`;
  field-encryption master key `ezllm-master.key` (present, unwired — see M2 landmarks)
- opencode-go **requires** `x-opencode-session` for generation (+`x-opencode-request` recommended).
  Missing → `400 MissingSessionID`. **But `/v1/models` and `/v1/usage` need NO session header.**
- ⚠️ **`GET /v1/models` on opencode-go is UNAUTHENTICATED** — it returns 200 + the full catalog for
  an empty key, `not-a-key`, even `sk-`. Never use it as a credential check. The auth oracle is
  **`GET /v1/usage`** (401 on a bad key, 200 on a good one, no session header, ~0.75 s, 0 tokens).
  ssn-gpt's `/models` *does* 401 — **auth behaviour is per-provider, so never assume.**
- ⚠️ Cloudflare `403 "error code: 1010"` on space.stationine.com is a **User-Agent block on
  `python urllib`**, not a rate limit. Go's default UA is fine (200 everywhere), and 12 rapid curls
  were never blocked. Practical rule: **403 = bad credentials → fail fast**; retry/backoff only for
  429/5xx/timeout. Don't write probing scripts in Python against that host.
- bailian base: `https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1`
  (NOT `coding-intl.dashscope…` — that 401s with this key). Also speaks Anthropic format at
  `/apps/anthropic/v1/messages` with `x-api-key`.
- `/v1/responses` and `/v1/messages` both signal an unsupported model with the **same** error type:
  `400 {"error":{"type":"ModelProtocolUnsupported"}}` — one classifier covers both surfaces.
- `max_tokens: 1` does **not** bound cost on reasoning models (`qwen3.8-flash` returned
  `completion_tokens: 25` anyway). Cheapest measured inference proof: `deepseek-v4.1-flash`
  (1689 ms, prompt=31, compl=1).
- All three surfaces emit usage: OpenAI in the final `data:` chunk, Anthropic across
  `message_start` **+** `message_delta` (merge them — start reports `output_tokens: 0`), Responses
  on `response.completed` **or `response.incomplete`** when `max_output_tokens` truncates.
- Quota shapes differ per provider on the *same* path: opencode-go → `percent`
  (`{usage:{rolling,weekly,monthly}{percent,resetsAt}}`), ssn-gpt → `money`
  (`{balance: 0.2563, unit: "USD", planName: 钱包余额, usage:{today,total}, daily_usage[], model_stats[]}`).
- **Auth-error status is per-provider — never assume.** Measured with bogus keys (2026-10-09/10):
  Gemini `…/v1beta/openai` → `400` "Please pass a valid API key" (the only kind where 400 counts
  as auth rejection); Groq → `401`; DeepSeek → `401`; TokenRouter → `401`; Command Code's
  `/models` → **200 without any key** (public catalog, NOT an oracle — registration falls back to
  the inference probe since `/usage` 404s).
- OpenCode Zen **free** tier is client-gated: `403 FreeTierError` ("only be used from within
  OpenCode") from a plain POST — don't add it as a provider. GitHub Models retired 2026-07-30.
- Groq free (Developer) limits are third-party approximations (~30 RPM / 14.4k RPD / 6k TPM) —
  official docs confirm "no card, no per-token charge" but not the numbers.
- SQLite backups: **no `sqlite3` CLI on this box** — use python3 `sqlite3.Connection.backup()`
  against a `mode=ro` URI (safe on the live WAL db); a raw file copy can catch mid-write state.

**M3 design doc:** `~/workspace/.hermes/plans/2026-10-06_070000-ezllm-m3-keytest-design.md`
(probes N1–N12, the corrected 7-step key test, acceptance criteria).

## 7. Deliberate non-goals (don't go looking for them in the code)

- No Anthropic↔OpenAI translation — all three surfaces are **native passthrough**; Claude Code
  runs on the native Anthropic surface (`/v1/messages`). Translation stays out of scope — it's
  also why the surface pin (M8) refuses instead of rerouting.
- No quota-cookie scraping — quotas come from provider usage endpoints where they exist;
  otherwise exhaustion is *detected reactively* on 429s (cooldown).
- No multi-node / Postgres — single-writer SQLite, one home box.
- No OAuth provider kinds beyond the four adapters (`opencode-go`, `openai-compatible`,
  `anthropic-compatible`, `gemini-openai`). **GitHub Copilot was tried and parked (2026-10-09):**
  the device flow works end-to-end with a dedicated OAuth app, but GitHub answers the
  `copilot_internal/v2/token` exchange with `403` (ToS) for third-party apps. Not circumvented —
  no spoofing or borrowed client IDs. See the vault note in `Projects/ezllm.md`.
