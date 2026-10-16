# ezllm

**A self-hosted LLM router and gateway.** One static Go binary that fronts any
mix of OpenAI-, Anthropic-, and Gemini-compatible providers behind a single
endpoint: namespace-aware routing, combo failover, per-model rules, request
compression, protocol translation, and a SQLite usage ledger that reconciles
token accounting across all three wire protocols — with an operator dashboard
in the same binary.

[![Tests](https://github.com/c4lyp5o/ezllm/actions/workflows/test.yml/badge.svg)](https://github.com/c4lyp5o/ezllm/actions/workflows/test.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

**Status: shipped.** Every image publish is CI-gated (gofmt · vet · full test
suite · web build). `GET /healthz` reports the running release version — see
[VERSION.md](VERSION.md) for the release history.

---

## Why ezllm

- **One endpoint, many providers.** Clients speak OpenAI, Anthropic, or
  Responses; upstreams are reached on whichever wire they actually support —
  natively or via probe-gated [translation](docs/translation-runbook.md).
- **Routing that survives failures.** Combos (ordered hop lists) with
  failover, round-robin, sticky, and block strategies; per-hop retry budgets,
  key pools with cooldown, and dead-endpoint backoff. A failed hop is never
  leaked to a live response (attempt/commit split).
- **Honest numbers.** A normalized ledger (`tokens_in/out/cached_read/cached_write`)
  across all three protocols, request-shape capture for cache research,
  versioned model prices with derived cost, and real-vs-notional compression
  savings.
- **Compression that ships bytes, not promises.** Staged engines —
  `session_dedup`, `rtk`, `headroom`, `lite`, `caveman`, `budget` — with
  per-profile configuration and fail-open semantics.
- **Ops surface included.** Dashboard, live request feed, analytics, key-test
  pipeline with protocol sweeps, and a disaster-recovery export — all served
  from the same binary.

## Quick start

Requires **Go 1.27+**. SQLite is pure Go (`modernc.org/sqlite`), so
`CGO_ENABLED=0` static builds work out of the box.

```bash
git clone https://github.com/c4lyp5o/ezllm.git && cd ezllm
make check                                 # fmt + vet + tests + build
cp config.example.yaml config.yaml         # provider metadata only —
                                           # credentials are added in the dashboard
make run                                   # http://127.0.0.1:20129
```

First run serves the dashboard at `/`; log in with the seeded dashboard
password and **change it immediately** (Settings). Add provider accounts and
keys from the Providers page — keys are validated by a live probe chain
(catalog → auth → quota → inference → protocol), never by format checks.

### Docker

```bash
docker build -t ezllm .                    # distroless, static, non-root
docker run -p 20129:20129 -v ./data:/data \
  -e EZLLM_ADDR=0.0.0.0:20129 ezllm
```

Or use the published image (`ghcr.io/c4lyp5o/ezllm:latest`) with
[compose.yml](compose.yml); [compose.traefik.yml](compose.traefik.yml) adds a
Traefik overlay that exposes only `/v1`, `/healthz`, and the dashboard shell
publicly and keeps `/admin` LAN-only.

## API surface

Full contract — every endpoint, request/response shape, and error code — in
**[docs/API.md](docs/API.md)**. The shape of it:

| Layer | Routes | Auth |
|---|---|---|
| Liveness | `GET /healthz` → `{"status":"ok","version":"0.0.12"}` | none |
| Inference | `POST /v1/chat/completions` · `/v1/messages` · `/v1/responses` | client token |
| Extended | `POST /v1/embeddings` · `/v1/images/generations` · `/v1/audio/speech` · `/v1/audio/transcriptions` | client token |
| Catalog | `GET /v1/models` (namespaced ids + combos; `?protocol=anthropic`) | client token |
| Admin | `/admin/*` — overview, usage, requests, accounts, combos, rules, compression, services, pricing, export, SSE feed, chat | admin session / token |

Errors are OpenAI-shaped (`{"error":{"type","message","code"}}`).

## Routing model

### Namespaces

A model is addressed `<namespace>/<model>`, where the namespace identifies a
**provider account** (combos have no namespace):

```bash
curl -s localhost:20129/v1/chat/completions \
  -H "Authorization: Bearer ***" -H "Content-Type: application/json" \
  -d '{"model":"myprov/qwen-flash","messages":[{"role":"user","content":"hi"}]}'
```

A bare model id is **404 by design**: the same id can exist on several
accounts, so guessing would mis-attribute spend. The 404 lists every
namespace that serves it.

### Combos

A combo is a bare name (`fallback`) → ordered hops `[{account, model}, …]`
plus a strategy:

| Strategy | Behaviour |
|---|---|
| `failover` (default) | transient failure (429/5xx/timeout/transport) → retry budget per hop → next hop; a 4xx goes straight back to the client |
| `true_round_robin` | stateless uniform roll across healthy hops |
| `block_round_robin` | consecutive dispatches per hop; with *hold-until-failure*, a hop keeps its block while it succeeds |
| `sticky_last_good` | stick to the hop that last served, until it fails |
| `least_used` | prefer the hop with the least ledger traffic |

Hop eligibility layers on top: per-(account, model) **rules** (token caps and
allowed-hours windows, see [docs/rules-engine-design.md](docs/rules-engine-design.md)),
per-model **surface pins**, context-size filtering, and cooldowns for dead
endpoints. Rules refuse with `429` naming the rule — before any upstream dial,
so a refusal costs zero tokens.

### Surface pins

Every model can carry an enforced pin (`openai` | `anthropic` | `responses` |
auto). A pinned model called on the wrong surface is refused with
`400 pinned_surface` **before dialling upstream** — enforcement, never a
silent reroute, because ezllm passes bodies through untranslated on native
paths and switching the shape behind the client's back would be dishonest.

### Protocol translation (opt-in)

Any client surface can be routed onto any upstream wire, gated behind
`translation.universal_enabled` (default **off**) and per-hop probe verdicts.
Native passthrough remains the default for every unmarked hop. Design and
runbooks: [docs/universal-wire-design.md](docs/universal-wire-design.md),
[docs/translation-runbook.md](docs/translation-runbook.md).

## Compression

Profiles of ordered stages over six engines, selected per request
(`x-ezllm-compression` header → combo default → off). Contract: the final
user message, tool calls, and structured tool results are never touched;
engine errors fail open. Savings are ledgered as **real** (bytes that
actually shipped shorter) vs **notional** (rejected by ratio floor), so
dashboards are never inflated. Design and measured numbers:
[docs/compression-design.md](docs/compression-design.md).

## Token accounting

The three surfaces disagree about caching, so raw numbers cannot be summed:

| Surface | cached tokens are… | cache-write reported? |
|---|---|---|
| `/v1/chat/completions` | **included** in `prompt_tokens` | no |
| `/v1/messages` | **excluded** from `input_tokens` | yes |
| `/v1/responses` | **included** in `input_tokens` | yes |

Every ledger row stores normalized `tokens_in / tokens_out /
tokens_cached_read / tokens_cached_write` (+ `reasoning_tokens`) plus the
verbatim `raw_usage` JSON for audit.

## Dashboard

Same binary, served at `/`; the UI renders and fetches nothing until
unlocked. Everything under `/admin/*` requires an admin session or an admin
client token. The health strip shows the running **version**, account/model
counts, and uptime.

## Configuration

Precedence: command-line flags → `EZLLM_*` environment → `config.yaml` →
built-in defaults.

- `EZLLM_ADDR` — listening address (set inside containers, where the default
  is loopback)
- `EZLLM_DATA_DIR` — ledger location (set inside containers, where the
  default is repo-relative)
- `EZLLM_LOG_LEVEL` — `debug` | `warn` | `error` (env-only: needed before
  the config is read)

Provider credentials live in SQLite only, managed from the dashboard or the
admin API — never in `config.yaml`. Upstream retry policy defaults to 3
retries per hop for transient failures only; keep `data/ezllm.sqlite` private
and backed up.

## Security

- **Provider keys** are stored in SQLite as plaintext (explicit design choice
  for this deployment class) — protect the database and its backups with
  filesystem access controls. AES-GCM field crypto is implemented and
  test-covered, intentionally unwired.
- `GET /healthz` is the only unauthenticated route: liveness plus the release
  version, nothing else. All counts, paths, and uptime live behind `/admin/*`.
- Client tokens are SHA-256 hashed, dashboard passwords bcrypt-hashed;
  plaintext credentials are never returned after creation. Only a masked key
  hint is ever exposed.
- The upstream's own credentials replace the client's on forwarding:
  `Authorization`, `x-api-key`, `Cookie`, and `Origin` (Anthropic rejects
  foreign browser Origins with a 401) are stripped, along with
  `X-Ezllm-Session`.
- Loopback by default; the dashboard hides all data pre-auth.

## Development

```bash
make check       # gofmt + go vet + full test suite + build
make test-short  # functional suite only — for memory-constrained machines
make bench       # phase benchmarks with -benchmem
make perf        # latency percentile harness + retained-memory measurements
make verify      # acceptance check against a live ledger
```

Repository layout:

- `cmd/`, `internal/` — Go source; `_test.go` files live **in-package**
  (Go toolchain requirement — see [CONTRIBUTING.md](CONTRIBUTING.md))
- `tests/` — live proof scripts (Python) run against a real instance
- `web/tests/` — Playwright screenshot/verification drivers for the dashboard
- `docs/` — design records and runbooks; `EXPLORING.md` is the codebase tour
- `VERSION.md` — release history (one version per change date)

CI: [test.yml](.github/workflows/test.yml) gates
[docker-publish.yml](.github/workflows/docker-publish.yml) — a red suite
never produces an image.

## Documentation

| Document | What it holds |
|---|---|
| [docs/API.md](docs/API.md) | the admin + public API contract |
| [VERSION.md](VERSION.md) | release history; what changed on each date |
| [CHANGELOG.md](CHANGELOG.md) | milestone-tagged change narrative |
| [EXPLORING.md](EXPLORING.md) | codebase tour: reading order, invariants, landmarks |
| [CONTRIBUTING.md](CONTRIBUTING.md) | workflow, conventions, known issues |
| [AGENTS.md](AGENTS.md) | agent/maintainer instructions: hard rules, layout map |
| [docs/](docs/) | design records and runbooks per subsystem |

## License

[MIT](LICENSE)
