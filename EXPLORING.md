# EXPLORING ezllm — Calypso's codebase tour notes

> **How to use this file:** it's your map for reading ezllm's code. Sections marked `⏳ FILL` get
> completed by fast as each milestone lands (that's a standing rule in the plan — if a milestone
> ships without updating its `⏳ FILL` section, it's not done). Read in "Reading order" below.
>
> **Source of truth:** `~/workspace/.hermes/plans/2026-10-05_151500-ezllm-v0-opencode-alibaba.md`
> (incl. its RECON CORRECTION block). This file = navigation; that file = decisions.

---

## 1. 60-second mental model

```
client (Hermes / curl / anything OpenAI-speaking)
   │  POST /v1/chat/completions        (bearer token = one of client_tokens)
   ▼
┌─────────────────────── ezllm (Go, 127.0.0.1:20129) ───────────────────────┐
│ server  → auth → router(alias → provider chain → key + cooldown)          │
│        → proxy(model swap, byte-passthrough SSE, usage sniff)             │
│        → ledger(async SQLite row)                                         │
└───────────────────────────────────────────────────────────────────────────┘
   │                                    │
   ▼                                    ▼
opencode-go                        bailian-coding
opencode.ai/zen/go/v1              token-plan.ap-southeast-1.maas.aliyuncs.com
Bearer + x-opencode-session        /compatible-mode/v1
```

Two upstreams only. No 359-provider ambition. The whole point: **SSE bytes flow through untouched** —
ezllm reads a request, swaps `model`, and copies the response stream while tapping the final
`usage` chunk for the ledger.

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
| M1 | `cmd/ezllm/main.go` → `internal/config` → `internal/server` | `config.example.yaml` | Where would I add a third provider? (config shape should make it obvious) |
| M2 | `internal/proxy/proxy.go` (`Forward` → `streamCopy` → `tapSSELine`) → `internal/store/ledger.go` (`NormalizeUsage`) | `git show` of M2 | Does ANY code path touch response bytes besides the scanner+Write+Flush loop? |
| M3 | `internal/router` | alias table in config | Can I trace `ez/omen` → opencode-go → model swap in 3 hops of reading? |
| M4 | `internal/router` cooldown + key selection | unit tests | What happens on 429 with 1 key left? (answer must be in a test) |
| M5 | `internal/ledger` queries → `internal/admin` | run `make usage` | Do the numbers match omniroute's dashboard roughly? |
| M6 | `cmd/ezllm/main.go` (shutdown), timeouts in `server.go` | `ezllm.service`, README | Can I kill -9 it and restart without corrupting the ledger? |

## 4. ⏳ FILL — milestone landmarks

*(fast: for each milestone, add 5–10 bullets: file → line range → what's there. Keep it under
half a screen per milestone. No narration, just landmarks.)*

### M1 — skeleton + health ✅ (2026-10-05, verified)
- `cmd/ezllm/main.go:30-97` — flags (`-config`, `-addr`), slog level via `EZLLM_LOG_LEVEL`,
  addr precedence `-addr` > `EZLLM_ADDR` > config, `http.Server` (10s ReadHeaderTimeout,
  **no WriteTimeout on purpose** for SSE), SIGINT/SIGTERM → 15s drain shutdown.
- `internal/config/config.go:71` `Load` / `:89` `Validate` — collects ALL problems into one
  error (missing env vars reported by NAME, never value); defaults applied here
  (`ledger.path`, cooldown 300s/60s/3).
- `internal/config/config.go:197` `Authenticate` — no early exit + `constantTimeEq` (`:208`);
  token map lives only in memory.
- `internal/server/server.go:39` `New` — route table: `GET /healthz` (public),
  `GET /v1/models`, `POST /v1/chat/completions` (both `authed`).
- `internal/server/server.go:49` `Handler()` — stack = `accessLog(recoverer(mux))`.
- `:93 authed` → writes `reqInfo.client` (holder created by accessLog at `:157` — this
  wiring was buggy in the first cut, log showed `client=""`; fixed + re-verified).
- `:128 statusWriter` — records status/bytes, **`Flush()` forwarded at `:151`** (the M2 SSE
  dependency lives here, not in the proxy).
- `internal/config/config_test.go` — 4 tests: valid+defaults, aggregate-error, bad fallback
  provider, duplicate token.
- **Live-verified:** healthz 200 · models 200 w/ token (`client=hermes` in log) · 401 w/o ·
  chat 501 stub · bind `127.0.0.1` only · SIGTERM → exit 0 · 0 secrets in log.
- **M1 invariants proven:** #1 no-secrets-in-logs (grep: 0 hits) · #6 graceful shutdown ·
  #7 loopback default. (#2 SSE, #3 passthrough, #4 retry policy, #5 ledger async = M2/M4)

### M2 — store + crypto + 3-surface passthrough ✅ (2026-10-06, LIVE-verified vs 2 real providers)
**Where to look first:** `internal/proxy/proxy.go:155 Forward` — it is the whole request path in
one function. Then `store.NormalizeUsage` (`ledger.go`) for the accounting rules.

- `internal/store/schema.sql` — 11 tables, embedded via `go:embed`. **FK order matters**:
  `compression_profiles` is declared *before* `combos` (combos references it). `calls` and
  `quota_snapshots` are append-only/immutable; `account`+`key_hint` are denormalized onto `calls`
  so history survives key/account deletion (proved by `TestLedgerRowsAreImmutableHistory`).
- `internal/store/store.go:66 Open` — WAL, `busy_timeout=5000`, `foreign_keys=ON`,
  **single writer conn (`SetMaxOpenConns(1)`) + read pool**. A write pool would only manufacture
  `SQLITE_BUSY`. `migrate()` at `:129` is idempotent and refuses to start on a *newer* schema.
- `internal/store/crypto.go:36 NewFieldCrypto` — scrypt(N=32768,r=8,p=1) → AES-256-GCM,
  wire format `enc:v1:<iv>:<ct>:<tag>`. `KeyHint` (`:121`) masks anything ≤8 chars rather than
  truncating, so a hint can never reconstruct its key. Master key is loaded from OUTSIDE `data/`.
- `internal/store/ledger.go:206 RecordCall` — **non-blocking by design** (drops + counts rather
  than adding latency to a stream; invariant #5). `Flush()` (`:281`) uses an **in-band barrier on
  the same FIFO channel** so it's deterministic — an earlier 2-channel version let `select` serve a
  flush while rows were still queued, silently losing ledger rows.
- `internal/store/ledger.go:110 NormalizeUsage` — **THE accounting decision.** Three surfaces
  disagree about cache: OpenAI includes `cached_tokens` in `prompt_tokens`; Anthropic EXCLUDES
  `cache_read` from `input_tokens`; Responses includes cached and is the only one reporting
  `cache_write`. Read the doc comment before touching this.
- `internal/provider/provider.go:78 Adapter` — the 4-method contract (`PrepareRequest`,
  `StripHeaders`, `ReadQuota`, `ListModels`). Adding a provider = one file + one `Kind`.
- `internal/provider/adapters.go:44 openCodeGo.PrepareRequest` — injects a **per-request**
  `x-opencode-session` (a shared one grows server-side until it blows context) and strips any
  client-supplied copy. `:135 anthropicCompatible` is its own kind because auth is `x-api-key`,
  not Bearer.
- `internal/provider/quota.go:22 parsePercentQuota` / `:60 parseMoneyQuota` / `:107 ClassifyQuota`
  — quota shapes differ per provider, so the result is discriminated (`percent|money|tokens|none`)
  and the verbatim JSON is always kept.
- `internal/proxy/proxy.go:118 SwapModel` — `map[string]any` round-trip with `UseNumber()`;
  only `model` changes, all 16 observed provider fields survive, big ints don't become floats.
  Rejects `null` explicitly (assigning into a nil map panics — a client could have crashed us).
- `internal/proxy/proxy.go:242 streamCopy` — scanner + `Flush()` per line, re-emitting the `\n`
  the scanner strips so forwarding stays byte-exact. Over-long lines fall back to a raw `io.Copy`.
- `internal/proxy/proxy.go:320 tapSSELine` + `:48 UsageTapper` — **merges** usage across events
  rather than taking the first hit. Two live-only bugs hid here: Anthropic's `message_start` has
  `output_tokens:0` (final totals come in `message_delta`), and a `max_output_tokens`-truncated
  Responses stream ends on `response.incomplete`, not `.completed`.
- `internal/router/router.go:66 Resolve` — combo name → `<namespace>/<model>` → else 404 **with
  hints** listing which namespaces serve that model. **No default account** (a bare model id is
  ambiguous: `gpt-6-luna` exists on both his providers).
- `internal/server/server.go:120 routes` — three POSTs + `/v1/models` (+`?protocol=anthropic`
  shape) + `/admin/health` + `/admin/usage`. `inference()` (`:207`) is one shared body for all
  three surfaces. `bearerToken` (`:474`) accepts Bearer *and* `x-api-key` (Claude Code).
- `cmd/m2verify/main.go` — acceptance checker that queries the live DB and prints PASS/FAIL per
  invariant. Run `go run ./cmd/m2verify` after any live soak.

**Live-verified 2026-10-06** (opencode-go 36 models + ssn-gpt 11 models synced at boot, 0 warnings):
- `/v1/chat/completions` → 200, and streamed: **TTFB 1.28s vs total 4.08s** = unbuffered
- `/v1/messages` with `x-api-key` → 200 **tool use** (`stop_reason: tool_use`, real `tool_use` block)
- `/v1/responses` → 200 `status: completed` with full usage incl. `cache_write_tokens`
- `super-ssn/gpt-6.1-sol` → 200 (second provider, second adapter kind)
- Cached repeat: raw `prompt=2496 cached=2048` → ledger **`tokens_in=448 + cached_read=2048`** ✅
- Bare `gpt-6-luna` → 404 listing **both** `opengo/` and `super-ssn/` ✅
- Keys at rest `enc:v1:…` (199 chars), hints only; `dropped_rows=0`; `CGO_ENABLED=0` static build ✅

**M2 invariants proven:** #1 no secrets in logs/responses (grep + `TestNoSecretsInResponses`) ·
#2 SSE unbuffered (flush-count + staggered-arrival test, live TTFB) · #3 unknown fields survive ·
#5 ledger never blocks (`TestRecordCallIsNonBlocking`, 5000-row flood) · #7 loopback default.
(#4 4xx-never-retried and #6 graceful-shutdown-under-load land in M4/M7.)

**Bugs found & fixed while building M2** (all had passing unit tests at the time — they only showed
up live or under `-count=3`): `Flush()` losing rows · usage window `ts < now` dropping fresh rows ·
empty-200 on upstream death (now 502 + ledgered) · `UpsertAccount` returning id 0 on the upsert path
· boot seeding passing `ID=0` into `PickKey` (silently hid the catalog) · Anthropic/Responses
streaming usage taps · TTFT truncating to 0 (now floored at 1ms).

### M3 — namespaces, registration + paced key test, protocol sync, smart quota
⏳

### M3 — opencode-go adapter + aliases
⏳

### M4 — key pool + cooldown
⏳

### M5 — usage queries + admin endpoint
⏳

### M6 — hardening + systemd
⏳

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

## 6. Key facts you'll need while exploring (verified 2026-10-05)

- Go **1.27.1** at `~/.local/go/bin/go` (PATH wired; apt's 1.22 is shadowed fallback)
- Keys (0600, never in repo/config): `~/.hermes/secrets/opencode-go.txt`,
  `~/.hermes/secrets/bailian-coding-plan.txt`
- opencode-go **requires** `x-opencode-session` (else 400 `MissingSessionID`); `/v1/models` needs
  no session header; 36 models incl. `omen-alpha`
- bailian base: `https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1`
  (NOT `coding-intl.dashscope…` — that 401s with this key)
- Both upstreams emit OpenAI `usage` in the final SSE chunk (verified) — ledger has real numbers
- Anthropic-format bonus surface on Alibaba: `/apps/anthropic/v1/messages` + `x-api-key` (Phase 2)

## 7. Deliberate non-goals (don't go looking for them in the code)

- No Anthropic↔OpenAI translation (Phase 2 — that's the Claude Code unlock)
- No quota-cookie scraping — exhaustion is *detected reactively* from 429s (cooldown), not polled
- No `/responses` API (Grok/GPT-Luna on opencode-go stay unreachable in v0)
- No dashboard UI yet — `/admin/usage` returns JSON only
- No Docker image — systemd user unit first
