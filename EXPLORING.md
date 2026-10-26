# EXPLORING ezllm — Calypso's codebase tour notes

> **How to use this file:** it's your map for reading ezllm's code — reading order, invariants
> to verify, and per-milestone file landmarks (§4). Landmarks name a file and a symbol, never a
> line number: grep the symbol. Keep a milestone's landmarks current as its code evolves.
>
> **Decisions live elsewhere:** `docs/*-design.md` and `docs/*-runbook.md` hold the design
> records; this file is navigation only.

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
provider accounts (one namespace each), seven kinds: opencode-go ·
openai-compatible · anthropic-compatible · gemini-openai · github-copilot ·
gemini-web · claude-web — each ships an adapter (`provider.Adapter`); the
account row carries the base URL + key pool. The Claude and ChatGPT
subscription bridges are loopback accounts of the compatible kinds.
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
equal, something is buffering — go read `streamCopy` in `internal/proxy/proxy.go` and find out why).

## 3. Reading order (per milestone)

| # | Read first | Then | Ask yourself |
|---|-----------|------|--------------|
| M1 | `cmd/ezllm/main.go` → `internal/config` → `internal/server` | `config.example.yaml` | Where would I add a fourth account? (config seeds; the DB is runtime truth) |
| M2 | `internal/proxy/proxy.go` (`Forward` → `streamCopy` → `tapSSELine`) → `internal/store/ledger.go` (`NormalizeUsage`) | `git show` of M2 | Does ANY code path touch response bytes besides the scanner+Write+Flush loop? |
| M3 | `internal/registration` + `internal/provider/adapters.go` + admin handlers in `internal/server` | `docs/API.md` | Can I trace an account from DB row → adapter → first upstream byte? |
| M4 | `internal/router` (hops → strategies → key pick/cooldown) + `internal/rules` | `go test ./internal/router ./internal/rules` | What happens on 429 with 1 key left? (answer must be in a test) |
| M5 | `internal/compress` (engines + stage pipeline) + `internal/server/compression.go` | `docs/compression-design.md` | Where is "never touch final turn / tool results" enforced, and where does fail-open live? |
| M6 | `internal/store/ledger.go` (`UsageReport`) → `/admin/usage` · `/admin/requests` · `/admin/stream` + `web/src` | `docs/API.md` + a dashboard login | Can I kill -9 it and restart without corrupting the ledger? Does the dashboard render anything pre-auth? (it must not) |
| M7 | `internal/compress/rtk.go`, `caveman.go` + `internal/rules` windows/caps | `tests/prove_*.py` (live proofs) | After compression, do ledger rows still carry faithful pre/saved token counts? |
| M8 | `internal/router/router.go` `checkPin` + `internal/server/admin.go` `handleSetModelPin` | `internal/router/pin_test.go`, `internal/server/pinsurface_test.go` | Where does a refusal stop the request — before or after the upstream dial? Does every switch that renders a typed router error have a case for `ErrPinned`? |

## 4. Milestone landmarks

*(5–10 bullets per milestone: file → symbol → what's there. Half a screen each max.
No narration, just landmarks. Symbols re-verified 2026-10-23; M10's "the problem was" list
names the pre-fix call sites.)*

### M1 — skeleton + health ✅ (2026-10-05, verified)
- `cmd/ezllm/main.go` `run` — flags (`-config`, `-addr`); console logging via
  `internal/logx` (`EZLLM_LOG_LEVEL`, `EZLLM_LOG_COLOR`, `EZLLM_LOG_FORMAT`;
  `announceLogging` states the effective level at boot, see `docs/logging.md`);
  first-boot dashboard setup
  (`cmd/ezllm/setup.go`: env password or one-time setup code, env-only by design — it must
  work before config is read); addr precedence `-addr` > `EZLLM_ADDR` > config;
  `http.Server` (10s ReadHeaderTimeout, **no WriteTimeout on purpose** for SSE);
  SIGINT/SIGTERM → drain shutdown (`cmd/ezllm/shutdown.go`).
- `internal/config/config.go` `Load` / `Validate` — collects ALL problems into one error
  (missing env vars reported by NAME, never value); defaults applied here (`DefaultDataDir`).
- Auth moved out of config (M2+): the server resolves tokens through the store —
  the `Auth` interface (`ClientByToken`, `internal/server/server.go`) implemented by
  `internal/store/catalog.go` (tokens matched by SHA-256 hash, `crypto.go` `HashToken`).
- `internal/server/server.go` `New` — the route table is `routes()`: `GET /healthz` (public,
  liveness + `version`), `GET /v1/models`, the three inference POSTs (all `authed`).
- `internal/server/server.go` `Handler()` — the middleware stack (access log, recoverer,
  security headers, CORS for `/v1/*`).
- `authed` writes `reqInfo.client` into the holder the access log created — this wiring was
  buggy in the first cut (log showed `client=""`); fixed + re-verified.
- `statusWriter` — records status/bytes, **`Flush()` forwarded** (the M2 SSE dependency lives
  here, not in the proxy).
- **Live-verified:** healthz 200 · models 200 w/ token (`client=hermes` in log) · 401 w/o ·
  chat 501 stub · bind `127.0.0.1` only · SIGTERM → exit 0 · 0 secrets in log.
- **M1 invariants proven:** #1 no-secrets-in-logs (grep: 0 hits) · #6 graceful shutdown ·
  #7 loopback default. (#2 SSE, #3 passthrough, #4 retry policy, #5 ledger async = M2/M4)

### M2 — store + crypto + 3-surface passthrough ✅ (2026-10-06, LIVE-verified vs 2 real providers)
**Where to look first:** `internal/proxy/proxy.go` `Forward` — it is the whole request path in
one function. Then `store.NormalizeUsage` (`ledger.go`) for the accounting rules.

- `internal/store/schema.sql` — the schema (v28 now; `schemaVersion` in `store.go`), embedded via `go:embed`. **FK order matters**:
  `compression_profiles` is declared *before* `combos` (combos references it). `calls` and
  `quota_snapshots` are append-only/immutable; `account`+`key_hint` are denormalized onto `calls`
  so history survives key/account deletion (proved by `TestLedgerRowsAreImmutableHistory`).
- `internal/store/store.go` `Open` — WAL, `busy_timeout=5000`, `foreign_keys=ON`,
  **single writer conn (`SetMaxOpenConns(1)`) + read pool**. A write pool would only manufacture
  `SQLITE_BUSY`. `migrate()` is idempotent and refuses to start on a *newer* schema
  (`schemaVersion`; one `alter…` function per migration, e.g. `alterModelProtoPin` for v7).
- `internal/store/crypto.go` `NewFieldCrypto` — scrypt(N=32768,r=8,p=1) → AES-256-GCM,
  wire format `enc:v1:<iv>:<ct>:<tag>`. `KeyHint` masks anything ≤8 chars rather than
  truncating, so a hint can never reconstruct its key. Master key is loaded from OUTSIDE `data/`.
  Unwired in v1 (plaintext keys, commit `dfe323c`); **wired since `81359d4`** — provider and
  management keys encrypt through it, see `docs/master-key-runbook.md`.
- `internal/store/ledger.go` `RecordCall` — **non-blocking by design** (drops + counts rather
  than adding latency to a stream; invariant #5). `Flush()` uses an **in-band barrier on
  the same FIFO channel** so it's deterministic — an earlier 2-channel version let `select` serve a
  flush while rows were still queued, silently losing ledger rows.
- `internal/store/ledger.go` `NormalizeUsage` — **THE accounting decision.** Three surfaces
  disagree about cache: OpenAI includes `cached_tokens` in `prompt_tokens`; Anthropic EXCLUDES
  `cache_read` from `input_tokens`; Responses includes cached and is the only one reporting
  `cache_write`. Read the doc comment before touching this.
- `internal/provider/provider.go` `Adapter` — the contract (`PrepareRequest`, `StripHeaders`,
  `ReadQuota`, `ListModels`, plus `AuthOracle` and `CatalogIsPublic` for the key test). Adding a
  provider = one file + one `Kind`.
- `internal/provider/adapters.go` `openCodeGo.PrepareRequest` — injects a **per-request**
  `x-opencode-session` (a shared one grows server-side until it blows context) and strips any
  client-supplied copy. `anthropicCompatible` is its own kind because auth is `x-api-key`,
  not Bearer. `gemini-openai` (M8) reuses the openai-compatible adapter shape; its quirk lives in
  the probe gate (`AuthRejectStatuser`), not the adapter.
- `internal/provider/quota.go` `parsePercentQuota` / `parseMoneyQuota` / `ClassifyQuota`
  — quota shapes differ per provider, so the result is discriminated (`percent|money|tokens|none`)
  and the verbatim JSON is always kept.
- `internal/proxy/proxy.go` `SwapModel` — `map[string]any` round-trip with `UseNumber()`;
  only `model` changes, all 16 observed provider fields survive, big ints don't become floats.
  Rejects `null` explicitly (assigning into a nil map panics — a client could have crashed us).
- `internal/proxy/proxy.go` `streamCopy` — scanner + `Flush()` per line, re-emitting the `\n`
  the scanner strips so forwarding stays byte-exact. Over-long lines fall back to a raw `io.Copy`.
- `internal/proxy/proxy.go` `tapSSELine` + `UsageTapper` — **merges** usage across events
  rather than taking the first hit. Two live-only bugs hid here: Anthropic's `message_start` has
  `output_tokens:0` (final totals come in `message_delta`), and a `max_output_tokens`-truncated
  Responses stream ends on `response.incomplete`, not `.completed`.
- `internal/router/router.go` `Resolve` — combo name → `<namespace>/<model>` → else 404 **with
  hints** listing which namespaces serve that model. **No default account** (a bare model id is
  ambiguous: `gpt-6-luna` exists on both his providers).
- `internal/server/server.go` `routes()` — three POSTs + `/v1/models`
  (+`?protocol=anthropic` shape) + admin surface. `inference()` is one shared body for all
  three surfaces. `bearerToken` accepts Bearer *and* `x-api-key` (Claude Code).
- `cmd/m2verify/main.go` — acceptance checker that queries the live DB and prints PASS/FAIL per
  invariant. Run `go run ./cmd/m2verify` after any live soak.

**Live-verified 2026-10-06** (opencode-go 36 models + ssn-gpt 11 models synced at boot, 0 warnings):
- `/v1/chat/completions` → 200, and streamed: **TTFB 1.28s vs total 4.08s** = unbuffered
- `/v1/messages` with `x-api-key` → 200 **tool use** (`stop_reason: tool_use`, real `tool_use` block)
- `/v1/responses` → 200 `status: completed` with full usage incl. `cache_write_tokens`
- `super-ssn/gpt-6.1-sol` → 200 (second provider, second adapter kind)
- Cached repeat: raw `prompt=2496 cached=2048` → ledger **`tokens_in=448 + cached_read=2048`** ✅
- Bare `gpt-6-luna` → 404 listing **both** `opengo/` and `super-ssn/` ✅
- Keys at rest (at M2): **plaintext** (`key_plain`) — the crypto path was implemented +
  test-covered but unwired until `81359d4`; hints only;
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
  keys stored in `key_plain` (plaintext at M3, commit `dfe323c`; AES-GCM ciphertext since `81359d4`); `key_hint`
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
  `failover` / `true_round_robin` / `sticky_last_good` / `least_used` /
  `block_round_robin` (strict_round_robin removed 2026-10-13 — a held hop);
  failover retry budget; 4xx passthrough (never retried).
- Key pool: per-account key selection + cooldown after 429/failure (skipped until expiry).
- `internal/rules/engine.go` — token caps (rolling window) + allowed-hours; combo hops that
  violate a rule are skipped silently; direct calls get a 429 naming the rule.
- Admin surface: `/admin/accounts` (incl. key pools), `/admin/combos`, `/admin/model-rules` CRUD.

### M5 — compression: engines, stages, profiles ✅
- `internal/compress/compress.go` — engine registry: `session_dedup`, `rtk`, `headroom`, `lite`,
  `caveman`, `ansi`, `fold`, `jsonmin`, `tool_ref`, `blobs`; ordered stage pipeline with
  per-stage options; fail-open + final-turn / tool-result exemption enforced here (see §6).
- `internal/server/compression.go` — `/admin/compression-profiles` CRUD; per-request profile
  resolution (`x-ezllm-compression` header → combo default → off).
- `internal/compress/caveman.go`, `rtk.go` — the aggressive pair (levels / surface-aware);
  live proof scripts in `tests/prove_*.py` (real runs, not unit mocks).

### M6 — dashboard + live surfaces ✅
- `internal/server/dashboard_auth.go` — bcrypt dashboard password + sessions. No default
  password: first boot prints a one-time setup code (or takes `EZLLM_DASHBOARD_PASSWORD`);
  `/admin/setup`, `/admin/login`, `/admin/logout`, `/admin/settings/password`. The session
  cookie is `SameSite=Strict`, and cookie-authenticated writes from another origin are refused
  (`cookieOrigins` in `admin.go`).
- `internal/server/spa.go` — `go:embed`-served SPA; nothing renders or fetches pre-auth.
- `internal/server/stream.go` — `/admin/stream` SSE (snapshot + call events); dashboard pages:
  Dashboard, Providers, Combos, Rules, Compression, Requests, RecentFeed, Chat, Connect,
  Economics, Stats, Settings.
- `/admin/usage` + `/admin/requests` + `/admin/export` — rollups (group_by …, shared through
  a 10 s report cache), token-dissection explorer, versioned non-secret configuration export.
  Full recovery is the database + key backup in `docs/recovery-runbook.md`, not the export.

### M7 — shipped, hardened, live ✅
- Post-ship fixes (2026-10-09): `Origin` added to `stripAlways` in the proxy (Anthropic 401s on
  browser origins; regression-tested — commit `8c832ef`); SPA clears session only on our
  numeric-code 401, provider 401s surface inline (commit `358cda3`).
- Deployment: `ghcr.io/c4lyp5o/ezllm` image; compose (port 20129) from
  `compose.example.yml`; the host's own gitignored `compose.yml` adds the Traefik labels
  (`llm.calypso.dedyn.io`, `/admin` guard); served bundle verified against `web/dist`.
- `cmd/m2verify` remains the end-to-end acceptance checker against live traffic. Line-number
  landmarks were stale by ~400 lines in places by 2026-10-10, which is why §4 now cites
  symbols only.

### M8 — providers batch + surface pin ✅ (2026-10-10, live-proved on prod)
- `internal/provider/provider.go` `KindGeminiOpenAI` — 4th kind (`ValidKinds()`).
  Its bad-key signal is `400` (Google says "Please pass a valid API key"), accepted as an auth
  rejection **only for this kind** via the optional `AuthRejectStatuser` interface
  (`internal/provider/probe.go`); the auth gate reads it, the inference step still treats only
  401/403 as auth failure (a 400 there can mean a malformed request).
- `internal/registration/registration.go` — all key **shape** checks deleted (`9aa9cf1`);
  `StepFormat` survives only as the unregistered-kind failure (`Run`). The probe chain is the
  sole validator.
- `internal/server/server.go` `handleHealth` — liveness + release version
  (`{"status":"ok","version":…}`) after a 2s DB ping (503 `{"status":"down","version":…"}` on
  failure); `TestHealthIsPublic` greps the body for leak fields and pins the two-field shape.
  The detail moved to `GET /admin/overview` (`admin.go` `handleOverview`).
- **Surface pin** (`340442b`, schema v7): `models.proto_pin`, migration `alterModelProtoPin`,
  store API `ModelPin`/`SetModelPin` (`internal/store/catalog.go`), router gate `checkPin`
  (`router.go`, called from `selectWire` for the responses override and the native path —
  **before any upstream dial**; lookup errors fail CLOSED; resync preserves pins via
  snapshot/restore in `UpsertModels`). Admin write `PUT /admin/accounts/{id}/model-pin`
  (`admin.go` `handleSetModelPin` — model id in the body because ids contain `/`; `pin` is
  `json.RawMessage` so absent/""/value are three states; unconfirmed surface → 409 unless
  `force`). Client-facing refusal: `400 pinned_surface` (`resolveErrInfo` in `server.go`) —
  **never a reroute**: switching the path behind the client's back would be dishonest.
  - Trap found by live prod test (`03a6317`): `errorStatus()` mapped `ErrPinned`→400 for the
    LEDGER, but the proxy error switch had no case and answered clients 502. Text-only tests
    passed. Any new typed router error needs a case in EVERY switch that renders it.
- UI (`9915582` etc.): `ConfirmModal` (`web/src/components/ui.tsx`) replaces `window.confirm`;
  Providers key status is text (`tested ok`/`test failed`/`untested`, still driven by
  `last_test_ok`); pin select lives in the model drawer (auto + three surfaces, unconfirmed
  goes through force-confirm); Connect shows two surfaces (Responses dropped from the PICKER —
  the server route `POST /v1/responses` still exists).
- **`web/dist` ships the UI.** The image build embeds it (`go:embed`, see the Dockerfile), so a `web/src`-only commit deploys the OLD interface with every test green. Rebuild
  + commit dist in the same push; after deploy verify the served hash
  (`curl https://llm.calypso.dedyn.io/ | grep assets/index-`) + grep one marker string.

### Universal wire matrix (M0–M5) ✅ (2026-10-13, shipped)
- `docs/universal-wire-design.md` — the architecture; runbook section at the end of
  `docs/translation-runbook.md`.
- `internal/translate/` — `ir.go` (RequestIR) + `codec_{chat,messages,responses}.go` (one codec
  per surface), `response_ir.go`/`response_codec.go` (ResponseIR, usage convention, errors),
  `events.go`/`stream_decode.go`/`stream_codec.go` (event model, per-source decoders,
  per-destination encoders, bounded SSE framing), `universal.go` (`IsUniversalCell`,
  `RequestUniversal`, `ReorientError`), `eligibility.go` (`Eligible`, `IsUniversalCell`).
- `internal/router/router.go` — `selectWire` (wire-first), `universalOrder` (fixed fallback
  chains), `pinSurface`, `WithUniversal` (the `messagesConventionReason` gate was removed in
  `61a403c`: usage convention no longer gates a translated Messages hop).
- `internal/proxy/proxy.go` — `prepareHopBody` (pair-aware codecs), the stream wrapper +
  `streamOutcomeFrom`, `Result.StreamOutcome` / `StreamCompleted`, `universalCodecOptions`.
- `internal/store/{schema.sql,store.go,admin.go}` — `accounts.usage_convention` (schema v13),
  `alterAccountUsageConvention`; `internal/provider/provider.go` — the `UsageConvention`
  vocabulary; `internal/config/config.go` — `translation.universal_enabled` (deprecated 2026-10-22,
  folded into `translation.enabled` by `Load`) + `universalValueError` (field-named boot
  failure); `cmd/ezllm/main.go` — `translationFromEnv` (env override) + boot log.
- Proof files: `internal/proxy/wire_matrix_{e2e,paths}_test.go` (9 cells × stream/nonstream,
  tool loops, 429/400/preparation/late-error paths), `internal/router/universal_selection_test.go`,
  `internal/server/wire_matrix_test.go`, `internal/translate/usage_convention_test.go`.
- Translated hops are ordinary combo members now (operator decision 2026-10-13): strategies order
  them like native hops, and a translated success/failure feeds the strategy the same way.
  Earlier revisions parked them outside the strategy ordering; do not reintroduce that tail.

### M10 — single-parse dispatch (OPT-1) ✅ (2026-10-15, measured; design `docs/single-parse-dispatch-design.md`)
- **The problem was 5 full JSON decodes per request**: `extractModel`
  (`server.go`), `IsStream` (`proxy.go`, per attempt), `SwapModel`
  (`proxy.go`, per hop), `compress.Apply`, `captureShape` (`shape.go`). The fix: `internal/jsonbody/jsonbody.go`
  `BodyDoc` — one decode (map + `UseNumber` + trailing-garbage check),
  opened once in `inference()` and threaded as an **optional hint**:
  `ForwardCandidatesDoc`, `prepareHopBody(body, rt, doc)`,
  `ApplyWithDoc`, `captureShapeDoc`. Every legacy call site passes nil and
  keeps the exact old path — the whole pre-existing suite proves it.
- `WithModel` is the hop swap on the shared map; it is byte-identical to the
  old `SwapModel` by test (7 fixture classes) and by two bites (UseNumber
  drop mangles `12345678901234567890`; assignment drop sends `"model":null`).
- Measured: e2e −25% @1 MiB seq (−10…−16% parallel), −13.7% stream par16
  @64 KiB; the isolated `InspectAndRoute` bench gets HEAVIER (one full map
  decode where the old struct decode skipped the messages array) — request
  level decides, phase-2 label is about inspection alone.
- `prefix_sha` now sees number literals verbatim (`json.Number`): `1e10` ≠
  `10000000000` in the hash. Intended (documented in the design doc §parity).
- Behavioral parity to re-check after any edit here: `extractModel`
  absent/null → ("", nil), non-string model → `400 "body is not a JSON
  object"`; `IsStream` only a real `true`; trailing garbage rejected at
  Open (the old per-site decoders rejected it too).

### M11 — gap closure trio ✅ (2026-10-15, measured; CHANGELOG carries the numbers)
- `internal/server/perf_harness_test.go` — `TestInspectRoutePercentiles1MiB`
  (router-only, grades spec §18's ≤1 MiB row: p95 4.75–5.28 ms vs 5 ms target,
  boundary-UNCERTAIN, single-parse's map decode is the cause) + 1 MiB e2e rows
  in `TestLatencyPercentiles`.
- `internal/jsonbody/fuzz_test.go` — 3 fuzz targets (panic-freedom, round-trip
  model/stream invariants, extractModel decision tree) with a 22-seed corpus
  (dup keys, deep nesting, escapes, `1e700`, trailing junk, non-objects).
- `internal/proxy/swap_parity_fuzz_test.go` — `FuzzSwapParity`: the PIPELINE
  parity oracle (reimplements the old `extractModel` gate, then compares
  legacy `SwapModel` vs `doc.WithModel` byte-for-byte). **The trailing-garbage
  subtlety lives here:** legacy `SwapModel` alone accepts trailing junk
  (dec.Decode stops after the first value), the old pipeline never let such a
  body reach a hop — pipeline parity, not component parity, is the contract.
  `TestSwapParityTrailingGarbageGate` pins it.
- `internal/proxy/cancel_latency_test.go` — `TestCancelPropagationLatency`
  (n=1000, channel-sync + 5 s failure guards): client→upstream cancel p95
  **63 µs** vs the spec's 20 ms. The §18 row is a certificate now.

### M12 — gemini-web provider ✅ (2026-10-16, code complete, mock-proven; design `docs/gemini-web-design.md`, runbook `docs/gemini-web-runbook.md`)
- `internal/geminiweb/` — the ported protocol: `frame.go` (wrb.fr frames;
  caught the units-rescan desync), `client.go` (cookie auth + init scrape +
  rotation), `rpc.go` (GET_USER_STATUS = zero-quota oracle + catalog),
  `generate.go` (81-slot construction + incremental parse + delta tails;
  caught the f.req double-encode bug and the final-chunk ordering bug).
- `internal/provider/geminiweb.go` — kind `gemini-web`, sentinel-base
  rewrite, key stamping (calibration exercises the real path), status-gated
  ListModels.
- `internal/server/geminiweb_bridge.go` — the in-process OpenAI-wire bridge:
  `/chat/completions` (SSE + JSON) + `/models` (oracle), per-account client
  cache, 8-min rotator, upstream-status→HTTP mapping. JSPB speaks no ezllm
  wire, so the bridge — not a translated upstream URL — is the integration.
- `internal/store/catalog.go` — `UpdateKeyPlain` (rotator persistence, same
  row, refreshed hint).
- 41 new tests across four packages, bite-proven, `-race` green. Live proof
  operator-gated (test account, see runbook).

### M13 — github-copilot provider ✅ (2026-10-16, device-flow live-proved; design `docs/github-copilot-design.md`, runbook `docs/github-copilot-runbook.md`)
- `internal/githubcopilot/` — the ported protocol: `auth.go` (device flow
  with the PUBLIC CLI client id + read:user, `token <gh>` mint, refresh grant
  without client_secret), `provider.go` (3-endpoint routing — claude* →
  /v1/messages, codex → /responses, default /chat/completions — CLI header
  set with configurable version, body quirks, /models discovery +
  37-model fallback, EnsureSession chain).
- `internal/provider/githubcopilot.go` — KindGitHubCopilot: per-account
  in-memory mint cache, /v1/messages anchoring, header install, in-place
  quirks, GET /models oracle (mint in PrepareRequest so calibration
  exercises the real path), honest quota-unsupported.
- `cmd/ezllm/githublogin.go` — `ezllm github-login`: prints the device URL +
  code, polls, verifies the mint, lists the catalog, prints the session row.
  Live: the device-code request against real GitHub succeeds (code issued).
- Evidence doc records the correction: the parked experiment's 403 was a
  CUSTOM app; the CLI's public identity + read:user works in production
  (omniroute, verified 2026-10-02).

### M14 — claude-web provider ✅ (2026-10-16; design `docs/claude-web-design.md`, runbook `docs/claude-web-runbook.md`)
- `internal/claudeweb/` — the ported protocol. `tlsclient.go` (uTLS
  Chrome-146 via `HelloChrome_Auto` behind an injectable Fetcher — Go's
  native TLS dies at claude.ai's Cloudflare; the REAL handshake runs green
  in-test against a local h2 server), `client.go` (sessionKey
  normalization, org discovery = the zero-quota key oracle, one fresh
  upstream conversation per request (client-generated conv uuid in the URL
  path; chaining every caller into one conversation was removed 2026-10-23),
  payload parity incl. opus-5 → high/auto + the styles block, typed failures:
  expired / challenge / forbidden / rate / cut). One shared HTTP/2 transport
  per fetcher, TLS verified.
- Non-ezllm wire → loopback bridge pattern, same as gemini-web:
  `internal/server/claudeweb_bridge.go` at `/internal/claudeweb/v1`
  (stream + non-stream, history flattening, tools pass through, typed
  refusal for response_format), adapter `internal/provider/claudeweb.go`
  with the sentinel base `internal://claudeweb`.
- NO model discovery on this surface (unlike copilot/gemini): the curated
  set only seeds routing, ids pass through, the upstream answer is the
  entitlement truth. The bootstrap catalog is documented as ids, not
  guarantees.

### Optimization pass — savings correctness + compression safety (2026-10-16)
Feasibility-audited first (current-state map + per-workstream verdicts; two
subagent audits were wrong and were corrected by direct source reads + one
executable Go probe). Five slices, all local, all gates-green:
- §11 fixes: nil-profile panic order (latent) and compression_ms was 0 on
  EVERY row (unnamed return + deferred write — proven by execution).
- WS-A: UsageReport gains applied_saved (the one authoritative applied
  savings aggregate; legacy tokens_saved stays as evidence). UI totals read
  it; the Economics double count (context_saved + tokens_saved, with an
  admitting comment) is gone.
- WS-E stage 1: rtk filters by tool class — reads/diffs/unknown preserved,
  log/search filtered, both wire shapes resolved.
- WS-B: disjoint input/output compositions (cache-read and cache-write are
  SUBSETS of input), cache-write rendered, honest labels.
- WS-C scoped (Calypso chose the session memo over the DB epoch table):
  internal/compress/sessionmemo.go — TTL'd, session+profile scoped commit
  ledger; committed history replays byte-identical; the ratio floor judges
  new work only. Bugs the tests caught during the build: budget's message
  drops broke the positional reassembly; the nothing-saved path never
  committed (exempt tail never froze); verbatim forms needed freezing too.
Caveman: please_could_you_tell_me no longer eats the question word (the
engine forbids capture re-emission, so the word left the pattern);
drop_there_seems_to_be removed. docs/compression-design.md carries the full
corrections section.

### v1.1 — subscription bridges ✅ (2026-10-23; runbooks `docs/claude-bridge-runbook.md`, `docs/codex-bridge-runbook.md`)
- `internal/claudebridge/` — Claude subscription behind a loopback Messages surface:
  `runner.go` `Run` spawns the official CLI per request, `stream.go`/`frames.go` turn its
  output into SSE, `limits.go` serves the plan windows on `/v1/usage`. Wired in
  `cmd/ezllm/claudebridge.go`.
- `internal/codexbridge/` — ChatGPT subscription behind a loopback OpenAI surface:
  `runner.go` `Run` drives `codex app-server` over JSON-RPC with Codex's own tools disabled,
  `tools.go` parks a turn that stopped on client tool calls until the results arrive
  (`park`, `Shutdown` reaps them), `landlock_linux.go`/`sandbox.go` confine every child,
  `exec.go` is the `codex exec` fallback. Wired in `cmd/ezllm/codexbridge.go`.
- Both bridges are loopback-only accounts of the compatible kinds; children end with their
  request, and the runner timeout covers the wait for a slot.

### v1.1.1 — audit fixes ✅ (2026-10-23)
- Security boundaries: `cookieOrigins` (`internal/server/admin.go`) refuses cookie writes
  from another origin; `provider.SameOriginRedirect` (`internal/provider/redirect.go`) keeps
  provider keys and prompts from following a redirect to another host.
- Compression memo (`internal/compress/sessionmemo.go`): `ClientSession` scopes an explicit
  session to its client; per-session and global byte budgets, `Sweep` on a server timer; a
  session runs the profile snapshot it was bound to (`Bind`). `refsResolve`
  (`toolref.go`) replays a `tool_ref` only while its target holds the same output, and
  engines class tool outputs by names from the whole request (`callNames`).
- Lifetimes: `goBackground`/`StopBackground` (`internal/server/background.go`) own probes and
  refresh loops; shutdown drains handlers before the ledger closes.

### v1.2.0 — reasoning vocabulary, model-access park, console logging ✅ (2026-10-24)
- Reasoning effort is a per-model vocabulary: `provider.ParseReasoningRule` reads it out of the
  refusal, `ReasoningRule.Clamp` maps a value onto it, `models.reasoning_vocab` /
  `reasoning_required` (schema v29) keep it, and `proxy.prepareHopBody` applies it per hop
  without re-encoding an accepted value. Design: `docs/reasoning-effort-design.md`.
- `deniesModelAccess` (`internal/proxy/proxy.go`) parks a hop on a 401/403 whose body names model
  access — body-matched, `hasAlternate`-gated, so a bare 403 still changes no state.
- Console logging has a written contract: `docs/logging.md` (levels, what is never logged, the
  measured cost of an argument built for a filtered line). `accessLog` carries `alias` and the
  failover trail from `reqInfo`; refusals before dispatch are visible at `debug`;
  `EZLLM_LOG_FORMAT=json` opts into one object per line.

### v1.3.0 — Antigravity subscription bridge ✅ (2026-10-26; design `docs/antigravity-subscription-design.md`, runbook `docs/agy-bridge-runbook.md`)
- `internal/agybridge/` — Google Antigravity plan behind a loopback OpenAI surface:
  `runner.go` `Run` spawns `agy --print=` headless per request in a fresh empty workspace,
  kills a turn whose `init` reports a loose permission mode; `preflight.go` `RunPreflight`
  (version, flags, token file, `CheckSettings`, `agy models`) gates the boot; `errors.go`
  `mapError` turns the CLI's prose errors into statuses and withholds Google's sign-in links;
  `env.go` `childEnv` is the allowlisted child environment. Wired in `cmd/ezllm/agybridge.go`.


### v1.4.0 — Settings administration (2026-10-26)

- `internal/settings/settings.go` `Manager.Patch` validates and saves a revision
  before publishing an immutable policy. `cmd/ezllm/settings.go` `operatorSettings`
  resolves environment → saved settings → explicit YAML → defaults; deployment
  flags remain higher-priority controls for their supported options.
- `internal/store/settings.go` `SaveSettings` owns revision-checked persistence;
  `InheritedRuleUsage` reads an indexed ledger range for mutable instance timezones.
  `internal/rules/engine.go` `Evaluate` binds inherited windows to the admitted
  request. Schema v31 preserves legacy empty zones as UTC and explicit zones intact.
- `internal/server/settings.go` `handleSettings` / `handleDiagnostics` expose the
  authenticated control and allowlisted diagnostic views. `server.go` `Handler`
  attaches the admission snapshot used by routing, proxy retries and report defaults.
- `internal/server/settings_jobs.go` `StartSettingsJobs` owns runtime scheduling;
  `prices_refresh.go` `nextDailyAt` handles skipped and repeated local hours.
  `settings_sessions.go` `handleSessions` / `handleRevokeSession` manage opaque
  session IDs; password changes still revoke every dashboard session.
- `internal/store/backup.go` `BackupTo` snapshots through SQLite's online API;
  `internal/backup/manager.go` `RunReserved` verifies and encrypts archives before
  publication and retention. `archive.go` `Verify` / `Restore` implement offline
  checks and recovery. `cmd/ezllm/backup.go` `runBackupCommand` wires CLI commands.
- `web/src/pages/Settings.tsx` `Settings` renders the six administration sections;
  `web/src/utils/preferences.tsx` `PreferencesMotion` / `readRange` preserve local
  browser preferences. Browser regressions: `web/tests/verify-settings.mjs`.
  Operational guidance: `docs/settings-runbook.md` and `docs/recovery-runbook.md`.
- `web/src/components/RailwayBackground.tsx` `RailwayBackground` restores the
  login rails with first-paint drawing independent of the small-screen limiter.
  `web/src/components/NeighbourhoodScene.tsx` `NeighbourhoodScene` draws the
  layered streets; `Neighbourhood.tsx` `buildHouses` preserves the real usage
  mapping and represents missing activity as unknown. Visual/interaction coverage
  is in `web/tests/verify-railway.mjs` and `verify-neighbourhood-polish.mjs`.

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

## 6. Key facts you'll need while exploring (provider facts measured 2026-10-06 … 10)

- Go **1.27** (`go.mod`) at `~/.local/go/bin/go` (PATH wired; apt's 1.22 is shadowed fallback)
- Keys (0600, never in repo/config): `~/.hermes/secrets/opencode-go.txt`,
  `~/.hermes/secrets/ssn-gpt.txt`, `~/.hermes/secrets/bailian-coding-plan.txt`,
  `~/.hermes/secrets/xiaomi-mimo-token-plan.txt`;
  ezllm client/admin tokens: `ezllm-infer.token`, `ezllm-admin.token`;
  field-encryption master key `ezllm-master.key` (wired since `81359d4` — see M2 landmarks)
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
- SQLite backups: **no `sqlite3` CLI required** — Settings can create encrypted
  verified online backups through the Go driver. `scripts/backup_sqlite.py` remains
  available for the separate unwrapped database/key format. A raw live file copy
  can miss committed WAL data; use the matching verification/restore procedure.

## 7. Deliberate non-goals (don't go looking for them in the code)

- Silent translation — every surface can reach every wire since the universal wire matrix
  (`docs/universal-wire-design.md`), but only onto hops a probe sweep PROVED, with native
  passthrough preferred, and the surface pin still refuses instead of rerouting.
- No multi-node / Postgres — single-writer SQLite, one home box.
- No guessing on quota: quotas come from provider usage endpoints (or, for Command Code, its
  billing endpoint with the account cookie); otherwise exhaustion is *detected reactively* on
  429s (cooldown).
