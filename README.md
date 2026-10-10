# ezllm

Calypso's lightweight LLM router. Single static Go binary, **three passthrough
surfaces** (OpenAI chat · Anthropic messages · OpenAI Responses), namespace
routing across provider accounts, combo failover, per-model rules, request
compression, and a SQLite usage ledger that normalizes token accounting across
all three protocols — with a token-gated dashboard on top.

**Status: shipped and live** — last full verification 2026-10-10 (all Go packages + UI build
green; prod re-proved live). Deployed on the home box: dashboard + admin
API at `https://llm.calypso.dedyn.io`, local clients at `http://127.0.0.1:20129`.
Claude Code talks to it as a native Anthropic endpoint (`POST /v1/messages`).
Latest shipped batch (2026-10-10): per-model **surface pin** (`proto_pin`, schema v7,
enforced never rerouted; wrong-surface calls refuse with `400 pinned_surface` before any
upstream dial), `/healthz` reduced to liveness-only, a fourth provider kind (`gemini-openai`)
plus presets for Groq, DeepSeek, Command Code, TokenRouter and Gemini, a dashboard UI
cleanup pass, and the **M9 research schema** (schema v8: request-shape capture for
cacheability, versioned model prices + derived cost, real-vs-notional savings split, and
TTFT finally measured from dispatch).

## Read this first

- `docs/API.md` — the admin API contract (v1): every endpoint, shapes, semantics.
- `docs/compression-design.md` — the compression subsystem as shipped (M5–M7).
- `docs/rules-engine-design.md` — caps + allowed-hours rules (design record).
- `EXPLORING.md` — the codebase tour: what to read, in what order, which
  invariants to verify, and per-milestone file/line landmarks.
- `~/workspace/.hermes/plans/2026-10-05_223000-ezllm-v1-rev4-namespace-compression.md`
  — the plan (rev5, gated). Its §0 probe table is the evidence behind the design.

## Build & run

```bash
make check                          # fmt + vet + test + build
cp config.example.yaml config.yaml  # provider metadata only; add credentials through the dashboard

make run
go run ./cmd/m2verify               # acceptance check against the live ledger
```

Requires **Go 1.27** (`~/.local/go/bin/go`); SQLite is pure-Go
(`modernc.org/sqlite`) so `CGO_ENABLED=0` static builds work — that's what makes
the Docker image distroless-able.

## Endpoints (full contract: docs/API.md)

| Layer | Route | Auth | Notes |
|---|---|---|---|
| Surface | `GET /healthz` | none | liveness only — `{"status":"ok"}` after a DB ping; all detail lives behind `/admin/*` |
| Surface | `GET /v1/models` | client token | namespaced ids + bare combo ids; `?protocol=anthropic` for the Claude shape |
| Surface | `POST /v1/chat/completions` · `/v1/messages` · `/v1/responses` | client token (`x-api-key` ok on messages) | stream + non-stream; all three live-verified |
| Admin | `GET /admin/health` · `/admin/overview` · `/admin/usage` · `/admin/requests` | admin | rollups (`group_by=account\|key\|alias\|client\|model\|surface\|day`), token dissection, dashboard payload |
| Admin | `GET /admin/stream` | admin | SSE live call feed |
| Admin | `POST /admin/chat` | admin | in-dashboard chat via the normal inference path |
| Admin | `/admin/accounts` · `/admin/combos` · `/admin/model-rules` · `/admin/compression-profiles` · `/admin/tokens` · `/admin/export` | admin | lifecycle CRUD + disaster-recovery dump |
| Admin | `PUT /admin/accounts/{id}/model-pin` | admin | set/clear a model's surface pin (`{"model","pin","force"}`); unconfirmed surface → 409 unless `force` |
| Admin | `GET /admin/prices` · `PUT`/`DELETE /admin/accounts/{id}/prices` · `GET /admin/cost` | admin | versioned per-model USD rates + derived spend (`?from=`; unpriced calls counted, never zeroed) |
| Admin | `POST /admin/login` · `/admin/settings/password` | password / admin | dashboard session gate |

## Addressing models

Routing is `<namespace>/<model>`, where the namespace belongs to a **provider
account** (combos have no namespace):

```bash
curl localhost:20129/v1/chat/completions -H "Authorization: Bearer $TOKEN" \
  -d '{"model":"opengo/qwen3.8-flash","messages":[{"role":"user","content":"hi"}]}'
curl localhost:20129/v1/responses -H "Authorization: Bearer $TOKEN" \
  -d '{"model":"opengo/gpt-6-luna","input":"hi"}'
```

A bare model id is **404 by design** — there is no default account, and the same
model id can exist on several of your accounts (`gpt-6-luna` lives on both
`opengo` and `super-ssn`), so guessing would mis-attribute spend. The 404 lists
every namespace that does serve it.

## Combos & strategies

- A **combo** is a bare name (`daily`, `fallback`) → ordered hops `[{account, model}, …]`
  + a strategy. Combo names can't contain `/`.
- Strategies: `failover` (default), `true_round_robin`, `strict_round_robin`,
  `sticky_last_good`, `least_used`.
- Failover: transient failures (429, 5xx, timeout, transport) → retry budget per hop →
  next hop. 4xx goes straight back to the client — never retried.
- Keys: every account owns a pool; keys in cooldown after 429/failure are skipped until
  they expire.

## Rules engine

Per-(account, model) **token caps** and **allowed-hours** windows. Inside a combo, an
out-of-window hop is skipped silently (the next hop serves); a direct call to a restricted
model → `429` naming the rule and its window. Design + semantics:
`docs/rules-engine-design.md`.

## Surface pin (per model)

Every model can carry an operator-chosen **pin**: `openai`, `anthropic`, `responses`, or
NULL = auto (no enforcement). The pin is **enforce-only, never a reroute**: if a client
calls a pinned model on a different surface, the request is refused with
`400 pinned_surface` (body names the pinned surface + the one requested) **before** any
upstream dial — so a refusal costs zero tokens and zero provider quota. ezllm passes
request bodies through untranslated, so switching the upstream path would send the wrong
shape; refusing is the honest answer.

- Stored as `models.proto_pin` (schema v7); catalog resyncs preserve pins
  (snapshot/restore around the delete-and-reinsert).
- The pin lookup **fails closed**: a transient store error refuses the request rather
  than silently bypassing an operator's pin.
- The pin applies to both direct calls and combo hops.
- Written via the dashboard (Providers → model drawer) or `PUT /admin/accounts/{id}/model-pin`;
  pinning a surface the probe never confirmed needs `force`.

## Compression

Ordered stages over six engines — `session_dedup`, `rtk`, `headroom`, `lite`, `caveman`,
`budget` — configured as profiles and selected per request (`x-ezllm-compression` header →
the combo's default profile → off). Contract: never touch the final user message, tool
calls, or structured tool results; fail-open on any engine error. Design + measured
numbers: `docs/compression-design.md`.

## Research capture & cost (schema v8)

The ledger now records enough to answer the questions token-savings research actually
needs. All of it is captured best-effort on the inference path — a failed parse records
nothing and never affects the request.

- **Cacheability** (`internal/server/shape.go`): every call stores `prefix_sha` — the hash
  of the request's stable core (system + tools + history minus the newest turn) — plus
  `msg_count`, `tool_count`, `session_id` (from an optional `X-Ezllm-Session` header) and
  `req_bytes`. A re-send of the same context shares the hash (sibling reuse); any edit to
  the stable core — an injected timestamp, a regrowing tool list, a compression profile
  rewriting history — changes it, which is exactly a cache break. `raw_usage` is a
  *response*, so this request structure was unreconstructable before.
- **Real vs notional savings** (`compression_saved_notional`): `tokens_saved` counts only
  bytes that actually shipped shorter; a ratio-floor rejection records its would-have-been
  saving separately, so the dashboard headline is never inflated (it was ~2x before).
- **Money** (`model_prices` + `GET /admin/cost`): per-(account, model) USD rates, versioned
  by `effective_from` so a reprice never rewrites history. Cost is **derived** at query
  time — not stamped per row — so correcting one rate fixes every past window. Calls with
  no effective rate are counted as `unpriced_calls` and excluded from the total, never
  silently zeroed. Economics shows spend and the unpriced gap; rates are set in
  Providers → Pricing.
- **TTFT** is measured from upstream dispatch, not from inside the stream copy, so it now
  reflects real prefill latency instead of a constant 1–3ms.

## Dashboard

Same binary, served at `/`; the UI renders and fetches nothing until unlocked. First login
uses the seeded dashboard password (`internal/store/dashboard_auth.go`) — **change it
immediately in Settings**. Everything under `/admin/*` requires an admin session or an
admin client token.

## Token accounting (the reason the ledger exists)

The three surfaces disagree about caching, so raw numbers can't be summed:

| Surface | cached tokens are… | cache-write reported? |
|---|---|---|
| `/v1/chat/completions` | **included** in `prompt_tokens` | no |
| `/v1/messages` | **excluded** from `input_tokens` | yes |
| `/v1/responses` | **included** in `input_tokens` | yes |

Every ledger row stores normalized `tokens_in / tokens_out / tokens_cached_read /
tokens_cached_write` (+ `reasoning_tokens`) plus the verbatim `raw_usage` JSON for
audit. Verified live: a cached repeat reports raw `prompt=2496, cached=2048` and
lands as `tokens_in=448 + cached_read=2048`.

## Config precedence

Command-line flags override `EZLLM_*` environment overrides, which override
`config.yaml`, which overrides the built-in defaults. The two overrides that
matter operationally are `EZLLM_ADDR` (listening address — always set inside a
container, where the config default is loopback) and `EZLLM_DATA_DIR` (ledger
location — always set inside a container, where the config default `data` is
repo-relative); `EZLLM_LOG_LEVEL` accepts `debug`, `warn` or `error`, and is
env-only because it must work before the config is read.

The global upstream retry policy defaults to
3 retries after the initial attempt per combo hop. A hop is retried only for transient
transport failures, timeouts, `429`, or `5xx`; then combo failover advances to the next
hop with a fresh retry budget. Credentials are managed in SQLite via admin APIs and
dashboard settings. Keep `data/ezllm.sqlite` private and include it in protected
backups. For phone-on-LAN access, use `-addr 0.0.0.0:20129`.

## Security notes

- Provider keys are stored in SQLite as plaintext per the operator's explicit
  testing-build choice; protect the database and its backups with filesystem access controls.
  AES-GCM field crypto (`internal/store/crypto.go`, master-key file) is implemented and
  test-covered but intentionally not wired in v1.
- `GET /healthz` is deliberately liveness-only (`{"status":"ok"}` after a DB ping) — it is the
  one unauthenticated route and leaks no counts, paths, or versions. Operational detail lives
  behind `/admin/*`.
- Key **shape** checks were removed from registration (2026-10-10): the probe chain (catalog →
  auth gate → quota → inference → protocol) is the sole validator, so exotic key formats are
  judged by the upstream itself.
- Client tokens are SHA-256 hashed and dashboard passwords bcrypt-hashed in SQLite; plaintext credentials are never returned after creation/login.
- Only a masked provider-key hint is ever exposed in admin responses or logs.
- The upstream's own credentials replace the client's: `Authorization`, `x-api-key`, `Cookie` —
  and **`Origin`** (Anthropic reads any browser `Origin` as CORS and rejects with a 401;
  regression-tested) — are stripped from client requests before forwarding.
- Loopback by default (`127.0.0.1`); the dashboard hides all data pre-auth.

## Testing & verification

- `make check` — fmt + vet + tests + build; `go run ./cmd/m2verify` — acceptance checker
  against the live ledger (PASS/FAIL per invariant, run after any soak).
- `web/*.mjs` — Playwright drivers (screenshots `shot-*.mjs`, checks `verify-*.mjs`);
  `scripts/prove_*.py` — live proofs (compression phases, caveman, day/night rules,
  requests, lmeval).
- GitHub Actions: `docker-publish.yml` builds & pushes `ghcr.io/c4lyp5o/ezllm` on master;
  `release.yml` cuts tagged releases.

## Non-goals (v1)

No Anthropic↔OpenAI translation (native passthrough covers all three surfaces), no
multi-node/Postgres. See EXPLORING.md §7 and the plan's §8.

**GitHub Copilot is parked, not rejected.** A dedicated OAuth app (`ezllm`) was registered
and the device flow completed end-to-end, but GitHub returns `403` at
`copilot_internal/v2/token` for a third-party app — an access decision, not a bug to
engineer around. No header spoofing or borrowed client IDs. Revisit only if GitHub
grants the app.
