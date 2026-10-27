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
  `session_dedup`, `rtk`, `headroom`, `lite`, `caveman`, `ansi`, `fold`,
  `jsonmin`, `tool_ref`, `blobs` — with per-profile configuration and
  fail-open semantics.
- **Subscriptions as accounts.** Optional loopback bridges put a Claude
  ([runbook](docs/claude-bridge-runbook.md)), ChatGPT
  ([runbook](docs/codex-bridge-runbook.md)) or Google Antigravity
  ([runbook](docs/agy-bridge-runbook.md)) subscription behind the router
  through each vendor's official CLI, with plan windows on the provider card
  where the vendor reports them.
- **Ops surface included.** Dashboard, live request feed, analytics, key-test
  pipeline with protocol sweeps, and configuration exports — all served from
  the same binary. Full recovery uses the [database/key backup procedure](docs/master-key-runbook.md).

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

No Go toolchain? [docs/install-binary.md](docs/install-binary.md) covers the
released binaries (Linux, macOS, Windows): download, checksum, `config.yaml`,
and running one as a systemd unit or a launchd agent.

There is no default dashboard password. On first boot the server prints an
ezllm banner with a one-time **setup code**, and the dashboard at `/` shows
only a set-password page that asks for it. The code changes on every restart
until setup is done. To skip the page, set `EZLLM_DASHBOARD_PASSWORD` (or
`EZLLM_DASHBOARD_PASSWORD_FILE`) before the first boot. Add provider accounts and
keys from the Providers page — keys are validated by a live probe chain
(catalog → auth → quota → inference → protocol), never by format checks.

### Docker

```bash
docker build -t ezllm .                    # distroless, static, non-root
docker run -p 20129:20129 -v ./data:/data \
  -e EZLLM_ADDR=0.0.0.0:20129 ezllm
docker logs ezllm                          # first boot: the setup code
```

On first boot the dashboard asks for the setup code printed in the
container log. To skip setup, pass the first password instead, ideally as a
secret file: `-e EZLLM_DASHBOARD_PASSWORD_FILE=/run/secrets/ezllm-dashboard`.

Or use the published image (`ghcr.io/c4lyp5o/ezllm:latest`) with compose:

```bash
cp compose.example.yml compose.yml         # gitignored: your paths and labels
docker compose up -d
```

[compose.example.yml](compose.example.yml) lists every environment variable
with its default, and the bridge mounts commented out. Behind a reverse proxy,
add its network and labels to your copy and set `EZLLM_TRUSTED_PROXIES`;
`/admin` is public there too, so keep the dashboard password long and put a
per-source rate limit on `/admin/login` and `/admin/setup`.

## API surface

Full contract — every endpoint, request/response shape, and error code — in
**[docs/API.md](docs/API.md)**. The shape of it:

| Layer | Routes | Auth |
|---|---|---|
| Liveness | `GET /healthz` → `{"status":"ok","version":"<release>"}` | none |
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
| `least_used` | send each request to the hop with the least recent traffic (in-process; each hop's count halves every 10 minutes, ties keep hop order) |

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

### Protocol translation

Any client surface can be routed onto any upstream wire, gated behind
`translation.enabled` (default **on**; `EZLLM_TRANSLATION_ENABLED=false`
turns it off) and per-hop probe verdicts.
Native passthrough remains the default for every unmarked hop. Design and
runbooks: [docs/universal-wire-design.md](docs/universal-wire-design.md),
[docs/translation-runbook.md](docs/translation-runbook.md).

## Compression

Profiles of ordered stages over the ten engines, selected per request
(`x-ezllm-compression` header → combo default → off). Contract: the final
message is exempt (a profile can open a purely tool-output final message to
tool-output engines), tool-call arguments are never touched, tool results are
never dropped or structurally changed, and engine errors fail open. A session
replays what it already shipped, so history stays byte-stable across requests. Savings are ledgered as **real** (bytes that
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
unlocked. Everything under `/admin/*` except sign-in and first-boot setup
requires an admin session or an admin client token. The sidebar shows the
running **version**; the overview shows uptime and account, model and combo
counts. Settings covers browser preferences, sessions, backup/recovery, gateway
policy, notification preferences and safe system diagnostics. Encrypted backup
scheduling is disabled by default; configure a separate encryption secret and a
mounted destination before enabling it. See the
[recovery runbook](docs/recovery-runbook.md).

## Configuration

Precedence for supported operator controls: command-line flags → `EZLLM_*`
environment → saved Settings → explicit `config.yaml` → built-in defaults.
Settings manages runtime policy, preferences, sessions and encrypted backups;
deployment paths and bridge startup remain YAML/environment controls. See
[Settings administration](docs/settings-runbook.md).

New install? Choose your usage timezone in Settings → General, or enforce it
with `EZLLM_USAGE_TZ` (which locks that dashboard control). Keep subscription
bridges off unless you run them (they are off in `compose.example.yml`).
The compose example explicitly sets `EZLLM_USAGE_TZ: UTC`; remove that entry
to manage the timezone from Settings.
Every variable and its default: **[docs/environment.md](docs/environment.md)**.
Logs go to stdout, one line per record — `docker logs ezllm`; what each level
carries and why there is no log file: **[docs/logging.md](docs/logging.md)**.

Provider credentials live in SQLite only, managed from the dashboard or the
admin API — never in `config.yaml`. Upstream retry policy defaults to 3
retries per hop for transient failures only; keep `data/ezllm.sqlite` private
and backed up.

## Security

- **Provider keys** (and per-account management keys) are encrypted at rest
  with AES-256-GCM under a master key that never enters the database. Back up
  the master key with the database, or the stored keys are unrecoverable — see
  [docs/master-key-runbook.md](docs/master-key-runbook.md).
- `GET /healthz` is the only unauthenticated data route: liveness plus the
  release version, nothing else. Besides it, only the dashboard's static files
  and the sign-in/setup endpoints answer without credentials; all counts,
  paths, and uptime live behind `/admin/*`.
- The dashboard session cookie is `HttpOnly`, `SameSite=Strict`, and
  cookie-authenticated writes from another origin (even another port on the
  same host) are refused with `403`.
- Client tokens are SHA-256 hashed, dashboard passwords bcrypt-hashed;
  plaintext credentials are never returned after creation. Only a masked key
  hint is ever exposed.
- The upstream's own credentials replace the client's on forwarding:
  `Authorization`, `x-api-key`, `Cookie`, and `Origin` (Anthropic rejects
  foreign browser Origins with a 401) are stripped, along with
  `X-Ezllm-Session`, `X-Ezllm-Compression` and `X-Ezllm-Bridge-Secret`.
- Provider clients never follow a redirect to another origin, so provider
  keys and prompts stay with the host they were sent to.
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
- `scripts/` — operator utilities: `backup_sqlite.py` (online SQLite backup),
  `ezllm-mac.sh` (macOS binary installer + launchd agent)
- `docs/` — design records and runbooks; `EXPLORING.md` is the codebase tour
- `VERSION.md` — release history (semantic versioning)

CI: [test.yml](.github/workflows/test.yml) gates
[docker-publish.yml](.github/workflows/docker-publish.yml) and
[release.yml](.github/workflows/release.yml) — a red suite never produces
an image or a release asset.

## Documentation

| Document | What it holds |
|---|---|
| [docs/install-binary.md](docs/install-binary.md) | running a released binary: platforms, checksum, `config.yaml`, service units, upgrades |
| [docs/API.md](docs/API.md) | the admin + public API contract |
| [docs/settings-runbook.md](docs/settings-runbook.md) | settings ownership, live updates, timezones, sessions and diagnostics |
| [docs/recovery-runbook.md](docs/recovery-runbook.md) | encrypted scheduled backups, offline verification/restore and upgrades |
| [docs/environment.md](docs/environment.md) | every environment variable and its default; what a new install sets |
| [docs/master-key-runbook.md](docs/master-key-runbook.md) | at-rest key encryption: paths, backup, rotation |
| [docs/pricing-runbook.md](docs/pricing-runbook.md) | price catalog: refresh cadence, import, query |
| [docs/claude-bridge-runbook.md](docs/claude-bridge-runbook.md) | Claude subscription bridge: setup, account, plan windows |
| [docs/codex-bridge-runbook.md](docs/codex-bridge-runbook.md) | ChatGPT subscription bridge: platform requirements, lockdown, tools, setup |
| [docs/agy-bridge-runbook.md](docs/agy-bridge-runbook.md) | Antigravity (Google) subscription bridge: keyring-free login, setup, limits |
| [VERSION.md](VERSION.md) | release history; what changed on each date |
| [CHANGELOG.md](CHANGELOG.md) | milestone-tagged change narrative |
| [EXPLORING.md](EXPLORING.md) | codebase tour: reading order, invariants, landmarks |
| [CONTRIBUTING.md](CONTRIBUTING.md) | workflow, conventions, known issues |
| [AGENTS.md](AGENTS.md) | agent/maintainer instructions: hard rules, layout map |
| [docs/](docs/) | design records and runbooks per subsystem |

## License

[MIT](LICENSE)
