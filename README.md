# ezllm

ezllm is a self-hosted LLM gateway. It is one Go binary that sits between your
apps and your model providers. Your apps talk to a single endpoint using the
OpenAI or Anthropic API, and ezllm routes each request to the right provider
account, retries or fails over when a provider has trouble, and records every
request's token usage and cost. It ships with a web dashboard for setup and
monitoring.

[![Tests](https://github.com/c4lyp5o/ezllm/actions/workflows/test.yml/badge.svg)](https://github.com/c4lyp5o/ezllm/actions/workflows/test.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

## What it does

- **One endpoint for many providers.** Clients can call Chat Completions,
  Anthropic Messages or the Responses API. ezllm sends each request to the
  provider in a format that provider supports, translating between formats
  when needed ([translation runbook](docs/translation-runbook.md)).
- **Failover and load balancing.** Group models into a *combo* with a strategy
  such as failover or round-robin. A failing hop is retried or skipped before
  any response reaches your app.
- **Usage you can compare.** Token counts, cache reads and writes, and cost are
  stored in one consistent format across all three APIs, alongside the
  provider's original usage data.
- **Request compression.** Optional profiles shrink large tool outputs and
  repeated context before they are sent, without changing tool calls or
  results.
- **Subscriptions as providers.** Optional bridges let you use a Claude,
  ChatGPT, GitHub Copilot or Google Antigravity subscription through each
  vendor's official CLI. An optional updater keeps those CLIs current.
- **Dashboard included.** Provider setup, API keys, a live request feed,
  analytics, rules and backups, all served by the same binary.

## Quick start

### 1. Start ezllm

Pick one of these.

**Docker** (Linux or macOS shell):

```bash
git clone https://github.com/c4lyp5o/ezllm.git
cd ezllm
cp config.example.yaml config.yaml
mkdir -p data
docker run -d --name ezllm --user "$(id -u):$(id -g)" \
  -p 127.0.0.1:20129:20129 -v "$(pwd)/data:/data" \
  -v "$(pwd)/config.yaml:/app/config.yaml:ro" \
  -e EZLLM_MASTER_KEY_FILE=/data/ezllm-master.key \
  ghcr.io/c4lyp5o/ezllm:latest
docker logs ezllm
```

**Docker Compose**, from the cloned repository:

```bash
cp config.example.yaml config.yaml
cp compose.example.yml compose.yml
mkdir -p data
docker compose up -d --no-build
docker compose logs ezllm
```

The Compose example runs as UID/GID 1000. If your `data` directory belongs to
another user, set `UID` and `GID` in your Compose environment.

**Released binary** for Linux, macOS or Windows, with no Go or Docker needed:
follow the [installation guide](docs/install-binary.md). It also covers running
ezllm as a systemd service or launchd agent.

**From source** (Go 1.27.4 or newer):

```bash
git clone https://github.com/c4lyp5o/ezllm.git
cd ezllm
cp config.example.yaml config.yaml
make run
```

ezllm listens on `http://127.0.0.1:20129`. The `data` directory holds the
SQLite database and the master key that encrypts your provider keys. Keep it
private and back up both files together; without the key, stored provider keys
cannot be recovered ([master key runbook](docs/master-key-runbook.md)).

### 2. Finish setup in the dashboard

1. Open [http://127.0.0.1:20129](http://127.0.0.1:20129). There is no default
   password: the server prints a one-time **setup code** in its output
   (`docker logs ezllm` for Docker). Enter it and choose a dashboard password.
   The code changes on each restart until setup is done. To skip this step,
   set `EZLLM_DASHBOARD_PASSWORD` or `EZLLM_DASHBOARD_PASSWORD_FILE` before the
   first start.
2. In **Providers**, choose **Add provider** and enter the account and API key.
   ezllm tests the key with real requests and discovers the available models.
   These test requests may be billed by the provider.
3. In **Connect**, under **Create an API key**, enter a name, choose
   **Inference only** and select **Create key**. Copy the key now; it is shown
   only once. Your apps use this key to call ezllm. It is not your provider key.
4. Still in **Connect**, pick a model and copy its full ID, for example
   `myprovider/model-name`. Connect also shows the base URL and ready-made
   environment variables for common clients.

### 3. Send a request

```bash
curl http://127.0.0.1:20129/v1/chat/completions \
  -H "Authorization: Bearer YOUR_EZLLM_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"myprovider/model-name","messages":[{"role":"user","content":"hi"}]}'
```

For OpenAI-compatible apps, set the base URL to `http://127.0.0.1:20129/v1` and
the API key to your ezllm key. For Anthropic-compatible apps, use
`http://127.0.0.1:20129` as the base URL.

If something fails:

- `401`: the ezllm key is missing, wrong or disabled.
- `404` for a model: use the full `namespace/model` ID. The error lists the
  namespaces that serve that model.
- `GET /v1/models` with your key lists every model ID you can use.
- `GET /healthz` needs no key and confirms the server is up.

## Use a subscription bridge

A bridge lets ezllm send requests through a vendor's official CLI, signed in to
your subscription. Each bridge is optional and off by default. Finish the
dashboard setup first, then follow the guide for your account:

| Account | Guide | Notes |
|---|---|---|
| Claude Pro/Max | [Claude bridge](docs/claude-bridge-runbook.md) | Install Claude Code and sign in with a separate CLI profile. |
| ChatGPT Plus/Pro | [Codex bridge](docs/codex-bridge-runbook.md) | Linux. Client tools need Landlock for the default protection. |
| GitHub Copilot | [Copilot bridge](docs/copilot-bridge-runbook.md) | Sign in to the official `copilot` CLI on the host. Supports client tools and PNG/JPEG images with a vision-capable model. |
| Google Antigravity | [Antigravity bridge](docs/agy-bridge-runbook.md) | Linux. Text chat by default; caller tools are experimental. No image input. |

The guides use Docker Compose and cover installing the CLI, signing in, the
mounts and the dashboard preset. If a bridge is enabled but something it needs
is missing, ezllm refuses to start and says why in `docker compose logs ezllm`.
Requests through a bridge, including key tests, use your plan's quota.

### Recommended cache settings

Prompt caching makes long conversations cheaper and faster, and uses less of
your plan. To get the most out of it, add these lines to
`services.ezllm.environment` in `compose.yml`, keeping only the bridges you use.
They turn on caching; they do not enable the bridges themselves.

```yaml
environment:
  # Codex: use the prompt cache whenever the installed CLI supports it (default).
  EZLLM_CODEX_BRIDGE_CACHE: "auto"

  # Copilot: keep workers warm and reuse finished conversations (needs both).
  EZLLM_COPILOT_BRIDGE_WARM_POOL: "on"
  EZLLM_COPILOT_BRIDGE_SESSION_CACHE: "on"

  # Antigravity: resume conversations (needs the experimental tool adapter).
  EZLLM_AGY_EXPERIMENTAL_TOOLS: "on"
  EZLLM_AGY_BRIDGE_SESSION_CACHE: "on"
```

Apply changes with `docker compose up -d --no-build`; `docker compose restart`
does not pick up new environment values. For a binary or source install, set
the same variables in the environment that starts ezllm.

A few things worth knowing:

- Caching works best when your app keeps the model, system prompt and tool
  definitions the same between requests and sends the full conversation each
  time.
- With `auto`, Codex falls back to uncached requests on CLIs that lack support,
  and `/status` shows why. Use `on` to make caching mandatory or `off` to
  disable it.
- The Claude bridge needs no cache setting.
- Compression does not break caching: each session resends the exact bytes it
  sent before, so the start of the conversation stays the same.
- Cached tokens show up in the usage records, but how much plan quota they save
  depends on the vendor.

Details and limits: [Codex](docs/codex-empty-template-cache-design.md),
[Copilot](docs/copilot-completed-session-cache-design.md),
[Antigravity](docs/agy-conversation-cache-design.md). The Antigravity tool
adapter is experimental; read its [limitations](docs/agy-bridge-runbook.md)
first.

## How routing works

### Model IDs

Every model is addressed as `<namespace>/<model>`, where the namespace is the
provider account, for example `myprov/qwen-flash`. A bare model name returns
`404` on purpose: the same model can exist on several accounts, and guessing
would charge the wrong one. The error lists the namespaces that serve it.

### Combos

A combo has a plain name (such as `fallback`) and an ordered list of
`{account, model}` hops, plus a strategy:

| Strategy | Behaviour |
|---|---|
| `failover` (default) | On a temporary failure (429, 5xx, timeout, connection error), retry the hop, then move to the next. Other 4xx errors go straight back to the client. |
| `true_round_robin` | Pick a healthy hop at random for each request. |
| `block_round_robin` | Send a run of requests to one hop before moving on; optionally stay on a hop until it fails. |
| `sticky_last_good` | Keep using the hop that last succeeded until it fails. |
| `least_used` | Send each request to the hop with the least recent traffic. |

### Rules, pins and translation

- **Rules** set token caps and allowed hours per account and model. A request
  that breaks a rule gets a `429` naming the rule, before anything is sent
  upstream, so it costs nothing ([rules design](docs/rules-engine-design.md)).
- **Surface pins** tie a model to one API (`openai`, `anthropic` or
  `responses`). Calling it through another API returns `400 pinned_surface`
  instead of silently changing the request.
- **Translation** lets any client API reach any provider format once a probe
  has verified that hop. It is on by default; set
  `EZLLM_TRANSLATION_ENABLED=false` to turn it off
  ([design](docs/universal-wire-design.md), [runbook](docs/translation-runbook.md)).

## Compression

Compression profiles run a chain of engines (for example deduplication, JSON
minification and long tool-output folding) on each request. A profile is chosen
by the `x-ezllm-compression` header, or by the combo's default; otherwise
compression is off. The last message is never compressed (except a final
tool-output message), tool-call arguments are never touched, tool results are
never dropped, and if an engine fails the original request is sent. Savings are
reported as **real** (bytes actually removed) or **notional** (a saving that was
rejected as too small), so the numbers are never inflated. Details and
measurements: [compression design](docs/compression-design.md).

## Token accounting

The three APIs count cached tokens differently, so their raw numbers cannot be
added together:

| API | Cached tokens are | Cache writes reported? |
|---|---|---|
| `/v1/chat/completions` | included in `prompt_tokens` | no |
| `/v1/messages` | excluded from `input_tokens` | yes |
| `/v1/responses` | included in `input_tokens` | yes |

ezllm stores every request as `tokens_in`, `tokens_out`, `tokens_cached_read`,
`tokens_cached_write` and `reasoning_tokens` in one consistent form, plus the
provider's original usage JSON.

## Dashboard

The dashboard is served at `/` and shows nothing until you sign in. It covers
providers and keys, API keys and client setup (**Connect**), a chat playground,
live requests, usage and cost analytics, compression savings, rules and
**Settings** (sessions, backups, gateway policy, notifications through ntfy,
Discord, Telegram or a webhook, and diagnostics). The sidebar shows the running
version. Scheduled encrypted backups are off by default; see the
[recovery runbook](docs/recovery-runbook.md) before turning them on.

## Configuration

Settings are applied in this order, first match wins: command-line flags,
`EZLLM_*` environment variables, values saved in the dashboard's Settings,
`config.yaml`, then built-in defaults. Bridges and file paths are set only by
environment or `config.yaml`.

- Every environment variable and its default: [docs/environment.md](docs/environment.md).
- Settings page reference: [settings runbook](docs/settings-runbook.md).
- Logs go to stdout only (`docker logs ezllm`): [docs/logging.md](docs/logging.md).
- Set your usage timezone in Settings → General, or fix it with `EZLLM_USAGE_TZ`.
  The Compose example sets `UTC`; remove that line to manage it from Settings.
- Provider keys are stored encrypted in SQLite and managed from the dashboard
  or admin API, never in `config.yaml`.
- `request_limits` in [config.example.yaml](config.example.yaml) caps concurrent
  work: by default 32 active requests in total, 8 per API key and a 256 MiB
  input buffer. Extra requests get a retryable `429`.
- Behind a reverse proxy, set `EZLLM_TRUSTED_PROXIES` and rate-limit
  `/admin/login` and `/admin/setup` at the proxy.

## Security

- Provider keys are encrypted at rest with AES-256-GCM. The master key is kept
  outside the database; back it up with the database.
- `GET /healthz` is the only route that returns data without credentials, and it
  returns only `status` and `version`.
- The dashboard cookie is `HttpOnly` and `SameSite=Strict`, and cookie-based
  writes from another origin are refused.
- API keys are stored as SHA-256 hashes and dashboard passwords with bcrypt.
  A key is shown once when created; afterwards only a masked hint is visible.
- Your client's credentials and ezllm's own headers are removed before a request
  is forwarded, and provider clients never follow redirects to another host.
- ezllm listens on localhost by default.

## Development

```bash
make test-short  # functional test suite; always pass -short to go test
make verify      # acceptance check against a live usage ledger
```

The full suite includes performance tests that need several GiB of memory; CI
runs it. Always use `-short` locally.

- `cmd/`, `internal/`: Go source. Tests live next to the code they test
  ([CONTRIBUTING.md](CONTRIBUTING.md) explains why).
- `web/`: the React dashboard. `web/dist/` is committed and embedded in the
  binary, so rebuild and commit it with every web change; CI checks it matches.
- `tests/`: live proof scripts (Python) run against a real instance.
- `scripts/`: operator tools such as `backup_sqlite.py` and `ezllm-mac.sh`.
- `docs/`: design records and runbooks. [EXPLORING.md](EXPLORING.md) is the
  code tour.

CI runs [test.yml](.github/workflows/test.yml) before
[docker-publish.yml](.github/workflows/docker-publish.yml) and
[release.yml](.github/workflows/release.yml), so a failing suite never produces
an image or release. Claude and Antigravity bridges share a streaming transport
in `internal/bridgeutil`; moving Codex and Copilot onto it is still to do
([status](docs/shared-bridge-transport-design.md)).

## Documentation

| Document | Contents |
|---|---|
| [docs/install-binary.md](docs/install-binary.md) | Installing a released binary: platforms, checksums, service units, upgrades |
| [docs/API.md](docs/API.md) | Public and admin API reference |
| [docs/environment.md](docs/environment.md) | Every environment variable and its default |
| [docs/settings-runbook.md](docs/settings-runbook.md) | Dashboard settings, timezones, sessions and diagnostics |
| [docs/recovery-runbook.md](docs/recovery-runbook.md) | Encrypted backups, restore and upgrades |
| [docs/master-key-runbook.md](docs/master-key-runbook.md) | Key encryption: paths, backup, rotation |
| [docs/pricing-runbook.md](docs/pricing-runbook.md) | Model price catalog: refresh, import, query |
| [docs/claude-bridge-runbook.md](docs/claude-bridge-runbook.md) | Claude subscription bridge |
| [docs/codex-bridge-runbook.md](docs/codex-bridge-runbook.md) | ChatGPT subscription bridge, including model choice for long tool tasks |
| [docs/copilot-bridge-runbook.md](docs/copilot-bridge-runbook.md) | GitHub Copilot subscription bridge |
| [docs/agy-bridge-runbook.md](docs/agy-bridge-runbook.md) | Google Antigravity subscription bridge |
| [docs/bridge-update-runbook.md](docs/bridge-update-runbook.md) | Automatic bridge CLI updates, holds and rollback |
| [docs/bridge-token-usage-audit.md](docs/bridge-token-usage-audit.md) | Bridge token usage findings and accounting fixes |
| [VERSION.md](VERSION.md) | Release history |
| [CHANGELOG.md](CHANGELOG.md) | Detailed change history |
| [EXPLORING.md](EXPLORING.md) | Code tour: reading order, invariants, landmarks |
| [CONTRIBUTING.md](CONTRIBUTING.md) | Workflow and conventions |
| [AGENTS.md](AGENTS.md) | Rules for AI agents and maintainers |
| [docs/](docs/) | All design records and runbooks |

## License

[MIT](LICENSE)
