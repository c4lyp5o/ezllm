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
  -d '{"model":"ez/qwen-flash","messages":[{"role":"user","content":"count 1-5"}],"stream":true}'
sqlite3 data/ezllm.sqlite 'SELECT ts,alias,provider,status,ttft_ms,total_ms,prompt_tokens,completion_tokens FROM calls ORDER BY id DESC LIMIT 5;'
```

**Expect:** `ttft_ms` noticeably smaller than `total_ms` (that gap = streaming working; if they're
equal, something is buffering — go read `proxy/sse.go` and find out why).

## 3. Reading order (per milestone)

| # | Read first | Then | Ask yourself |
|---|-----------|------|--------------|
| M1 | `cmd/ezllm/main.go` → `internal/config` → `internal/server` | `config.example.yaml` | Where would I add a third provider? (config shape should make it obvious) |
| M2 | `internal/proxy/chat.go` → `internal/proxy/sse.go` → `internal/ledger` | `git show` of M2 | Does ANY code path touch response bytes besides `io.Copy`? |
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

### M2 — bailian passthrough + ledger write
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
