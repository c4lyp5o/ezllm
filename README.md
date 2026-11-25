# ezllm

ezllm is a self-hosted LLM gateway: one program between your apps and your AI
providers. Your apps talk to one address with the OpenAI or Anthropic API. ezllm sends
each request to the right provider account, retries or switches when one has trouble, and
keeps track of tokens and cost. You set it up in a web dashboard.

[![Tests](https://github.com/c4lyp5o/ezllm/actions/workflows/test.yml/badge.svg)](https://github.com/c4lyp5o/ezllm/actions/workflows/test.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

## What it does

- **One address for many providers.** Apps use Chat Completions, Anthropic Messages or
  the Responses API. ezllm [translates](docs/translation-runbook.md) when a provider
  needs another format. Embeddings, images, audio and other services pass through too.
- **Failover and load balancing.** Group models into a *combo*. A model that fails
  before it answers is retried or skipped, and your app sees only the answer.
- **Usage you can compare.** Tokens, cache use and cost, recorded the same way for every
  provider.
- **Compression.** Optional profiles shrink long tool output and repeated text before
  they are sent.
- **Subscriptions as providers.** Optional bridges use your Claude, ChatGPT, GitHub
  Copilot or Google Antigravity plan through each vendor's own CLI.
- **Dashboard included.** Providers, API keys, live requests, analytics, rules, backups
  and notifications. It also tells you when a new release is out.

## Quick start

### 1. Start ezllm

Pick one:

- **[Docker](#docker)**: on Linux or macOS.
- **[Docker Compose](#docker-compose)**: the same, from a `compose.yml`.
- **[Binary on Linux](#binary-on-linux)**, **[macOS](#binary-on-macos)** or
  **[Windows](#binary-on-windows)**: one file, no Docker.
- **[From source](#from-source)**: with Go.

The first start prints a `setup code`. You need it in step 2.

#### Docker

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
sleep 5
docker logs ezllm
```

#### Docker Compose

```bash
git clone https://github.com/c4lyp5o/ezllm.git
cd ezllm
cp config.example.yaml config.yaml
cp compose.example.yml compose.yml
mkdir -p data
docker compose up -d --no-build
sleep 5
docker compose logs ezllm
```

`id -u` and `id -g` should both print `1000`. If not, put your numbers in a file named
`.env` next to `compose.yml`: one line `UID=1001`, one line `GID=1001`.

#### Binary on Linux

This gets the latest release and checks it:

```bash
VER=$(curl -fsSLI -o /dev/null -w '%{url_effective}' https://github.com/c4lyp5o/ezllm/releases/latest | sed 's|.*/v||')
ARCH=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
NAME=ezllm_v${VER}_linux_$ARCH
URL=https://github.com/c4lyp5o/ezllm/releases/download/v$VER
mkdir -p ~/ezllm/bin ~/ezllm/data && chmod 700 ~/ezllm/data && cd ~/ezllm
curl -fsSLO "$URL/$NAME.tar.gz" && curl -fsSLO "$URL/SHA256SUMS" &&
  grep " $NAME.tar.gz\$" SHA256SUMS | sha256sum -c - &&
  tar xzf "$NAME.tar.gz" &&
  install -m 0755 "$NAME/ezllm" bin/ezllm &&
  { [ -e config.yaml ] || cp "$NAME/config.example.yaml" config.yaml; } &&
  rm -r "$NAME" "$NAME.tar.gz" SHA256SUMS
```

Start it:

```bash
cd ~/ezllm
EZLLM_MASTER_KEY_FILE=$HOME/ezllm/data/ezllm-master.key ./bin/ezllm -config config.yaml
```

To leave it on, [make it a service](docs/install-binary.md#keep-it-running-on-linux).

#### Binary on macOS

This gets the latest release, checks it, and starts it each time you log in:

```bash
mkdir -p ~/ezllm && cd ~/ezllm
curl -fsSLO https://raw.githubusercontent.com/c4lyp5o/ezllm/master/scripts/ezllm-mac.sh
bash ezllm-mac.sh install && bash ezllm-mac.sh start
grep "setup code" ~/ezllm/logs/ezllm.log | tail -2
```

#### Binary on Windows

Follow [these steps](docs/install-binary.md#windows) in PowerShell.

#### From source

You need Go 1.27.4 or newer.

```bash
git clone https://github.com/c4lyp5o/ezllm.git
cd ezllm
cp config.example.yaml config.yaml
mkdir -p data
EZLLM_MASTER_KEY_FILE=$PWD/data/ezllm-master.key make run
```

### 2. Finish setup in the dashboard

1. Open [http://127.0.0.1:20129](http://127.0.0.1:20129). Enter the setup code and choose a
   password. The code changes each time ezllm starts, so use the latest one:
   `docker logs ezllm` with Docker, `~/ezllm/logs/ezllm.log` on macOS, or the terminal you
   started it in.
2. In **Providers**, choose **Add provider**, enter the account and its API key, then
   **Save & validate key**. ezllm tests the key with small real requests, which your
   provider may bill, and lists its models.
3. In **Connect**, choose **Create key**, enter a name, leave **Inference** ticked and
   choose **Create key**. Copy the key now: you see it once. Your apps use this key, not
   your provider's.
4. Still in **Connect**, pick a model and copy its full ID, for example
   `myprovider/model-name`.

No one at the keyboard? Set `EZLLM_DASHBOARD_PASSWORD` or
`EZLLM_DASHBOARD_PASSWORD_FILE` before the first start, and there is no setup code.

The `data` folder has your settings, history and the master key that unlocks your stored
provider keys. Keep it private. To back it up, stop ezllm and copy the whole folder, or use an
[online backup](docs/recovery-runbook.md#verified-databasekey-backup). Without the key,
your provider keys cannot be read ([master key](docs/master-key-runbook.md)).

### 3. Send a request

```bash
curl http://127.0.0.1:20129/v1/chat/completions \
  -H "Authorization: Bearer YOUR_EZLLM_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"myprovider/model-name","messages":[{"role":"user","content":"hi"}]}'
```

You should see the model's reply. In your apps, set:

| App | Base URL | API key |
|---|---|---|
| OpenAI-compatible | `http://127.0.0.1:20129/v1` | your ezllm key |
| Anthropic-compatible | `http://127.0.0.1:20129` | your ezllm key |

**Connect** has these ready to paste for common apps.

If it fails:

| You see | Do this |
|---|---|
| `401` | Check the ezllm key: it is missing, wrong or disabled |
| `404` for a model | Use the full `provider/model` ID. The error lists who serves it, and `GET /v1/models` lists all you can use |
| No answer | Open `http://127.0.0.1:20129/healthz`. It needs no key and says if ezllm is up |

## Use a subscription bridge

A bridge sends requests through a vendor's own CLI, signed in to your plan. Each one is
optional and off until you turn it on. Finish the dashboard setup first, then follow the
page for your plan. You need Linux (on Windows, [use WSL](docs/wsl.md)); each page covers
Docker Compose, `docker run` and the binary.

| Plan | Turn it on | Reference | Notes |
|---|---|---|---|
| Claude Pro/Max | [Turn on](docs/bridges/claude.md) | [Claude bridge](docs/claude-bridge-runbook.md) | Client tools. No server tools (web search, code execution). |
| ChatGPT Plus/Pro | [Turn on](docs/bridges/codex.md) | [Codex bridge](docs/codex-bridge-runbook.md) | Linux. Client tools need Landlock. |
| GitHub Copilot | [Turn on](docs/bridges/copilot.md) | [Copilot bridge](docs/copilot-bridge-runbook.md) | Client tools, and PNG/JPEG images with a vision-capable model. |
| Google Antigravity | [Turn on](docs/bridges/antigravity.md) | [Antigravity bridge](docs/agy-bridge-runbook.md) | Linux. Text and client tools; no image input. |

Not working? [Troubleshooting](docs/bridges/troubleshooting.md).

Requests through a bridge use your plan's quota, and so do key tests. Each bridge has its
own sign-in folder (`~/.claude-sub`, `~/.codex-sub`, `~/.copilot-sub`, `~/.agy-sub`), so
signing out of your everyday CLI does not break it.

An optional [updater](docs/bridge-update-runbook.md) keeps the CLIs current with Docker.

### Caching

Caching makes long conversations faster and uses less of your plan. Every bridge does it
on its own where its CLI can, so there is nothing to set. It works best when your app keeps the same model,
system prompt and tools, and sends the whole conversation each time.

To turn it off, set `EZLLM_CODEX_BRIDGE_CACHE`, `EZLLM_COPILOT_BRIDGE_CACHE` or
`EZLLM_AGY_BRIDGE_CACHE` to `"off"` and start ezllm again
([every setting](docs/environment.md)).

## How routing works

### Model IDs

A model's ID is `provider/model`, for example `myprovider/model-name`. A bare model name
returns `404` on purpose: several accounts can have the same model, and a guess could
charge the wrong one.

### Combos

A combo has a name, such as `fallback`, that your apps use as the model. It has a list of
models, called hops, and a strategy:

| Strategy | Behaviour |
|---|---|
| `failover` (default) | Use the first hop. On a temporary failure (429, 5xx, timeout, connection error), try again, then move to the next. Other 4xx errors go straight back to your app. |
| `true_round_robin` | Pick a hop at random for each request. |
| `block_round_robin` | Send a run of requests to one hop, then move on. Optionally stay on a hop until it fails. |
| `sticky_last_good` | Keep using the hop that last succeeded until it fails. |
| `least_used` | Send each request to the hop with the least recent traffic. |

### Rules, pins and translation

- **Rules** set token caps and allowed hours per account and model. A request
  that breaks a rule gets a `429` naming the rule, before anything is sent
  upstream, so it costs nothing ([rules design](docs/design/rules-engine-design.md)).
- **Surface pins** tie a model to one API: `chat` (Chat Completions),
  `anthropic` (Messages), `responses`, or a service such as `embeddings`. With
  translation on, a call through another of the first three is translated to the
  pinned API. With it off, or between these and a service, the call returns
  `400 pinned_surface`.
- **Translation** lets any client API reach any provider format once a probe
  has verified that hop. It is on by default; set
  `EZLLM_TRANSLATION_ENABLED=false` to turn it off
  ([design](docs/design/universal-wire-design.md), [runbook](docs/translation-runbook.md)).

## Compression

A profile runs a chain of steps on each request, such as removing repeats, shrinking JSON
and folding long tool output. Set one on a combo, or name one in the
`x-ezllm-compression` header. Otherwise nothing is compressed.

Tool-call arguments are never changed and tool results are never dropped. If a step fails, the
original request is sent. Savings are shown as **real** (bytes removed) or **notional**
(too small to be used). Details: [compression design](docs/design/compression-design.md).

## Token accounting

The three APIs count cached tokens differently, so their raw numbers cannot be
added together:

| API | Cached tokens are | Cache writes reported? |
|---|---|---|
| `/v1/chat/completions` | included in `prompt_tokens` | no |
| `/v1/messages` | excluded from `input_tokens` | yes |
| `/v1/responses` | included in `input_tokens` | yes |

ezllm records all three the same way: `tokens_in`, `tokens_out`, `tokens_cached_read`,
`tokens_cached_write` and `reasoning_tokens`, with the provider's original numbers beside
them.

Recording never holds up a response, so a few rows can be lost under heavy load or a disk
problem. The dashboard health shows how many. Do not use it as a bill or a hard spending
limit.

## Dashboard

The dashboard is at `/` and shows nothing until you sign in.

| Page | What it is for |
|---|---|
| **Dashboard** | Overview, health and recent calls |
| **Analytics** | Stats, token economics and every request |
| **Providers** | Accounts, keys, models, tests and quota |
| **Services** | Embeddings, images, audio and other services |
| **Chat** | Try a model |
| **Combos** | Groups of models and their strategy |
| **Rules** | Token caps and allowed hours |
| **Compression** | Profiles and a preview |
| **Connect** | API keys, model IDs and app setup |
| **Neighbourhood** | Your providers drawn as a city |
| **Settings** | General, security, backups, gateway, notifications (ntfy, Discord, Telegram, webhook) and system |

Scheduled encrypted backups are off until you set them up:
[recovery](docs/recovery-runbook.md).

## Configuration

The first one that is set wins: command-line flags, `EZLLM_*` environment variables,
values saved in Settings, `config.yaml`, then the defaults. Bridges and file paths come
only from the environment or `config.yaml`.

- Every environment variable and its default: [docs/environment.md](docs/environment.md).
- Settings page reference: [settings runbook](docs/settings-runbook.md).
- Logs go to stdout only (`docker logs ezllm` with Docker): [docs/logging.md](docs/logging.md).
- Set your timezone in **Settings** → **General**. The Compose example sets `UTC` with
  `EZLLM_USAGE_TZ`; remove that line to choose it in Settings.
- Provider keys are managed in the dashboard or admin API, never in `config.yaml`.
- `request_limits` in [config.example.yaml](config.example.yaml) caps concurrent
  work: by default 32 active requests in total, 8 per API key and a 256 MiB
  input buffer. Extra requests get a retryable `429`.
- Behind a reverse proxy, set `EZLLM_TRUSTED_PROXIES` and rate-limit
  `/admin/login` and `/admin/setup` at the proxy.

## Security

- Provider keys are encrypted at rest with AES-256-GCM. The master key is kept
  outside the database; back it up with the database.
- Without credentials, ezllm answers only the dashboard's own files, `GET /healthz`
  (`status` and `version`), and `GET /admin/setup` (whether setup is still to do).
- Keys carry roles: `infer` calls models, `metrics` reads `GET /metrics`
  (Prometheus, see [docs/metrics.md](docs/metrics.md)) and `admin` manages the
  gateway. Each role opens only its own endpoints, and none implies another.
- The dashboard cookie is `HttpOnly` and `SameSite=Strict`, and cookie-based
  writes from another origin are refused.
- API keys are stored as SHA-256 hashes and dashboard passwords with bcrypt.
  A key is shown once when created; afterwards only a masked hint is visible.
- Your client's credentials and ezllm's own headers are removed before a request
  is forwarded, and requests with your provider keys never follow a redirect to another
  host.
- ezllm listens on localhost by default.

## Development

```bash
make test-short  # functional test suite; always pass -short to go test
make verify      # acceptance check against a live usage ledger
```

The full suite includes performance tests that need several GiB of memory; CI
runs it. Always use `-short` locally.

- `cmd/`, `internal/`: Go source. Tests live next to the code they test
  ([DEVELOPER.md](DEVELOPER.md) explains why).
- `web/`: the React dashboard. `web/dist/` is committed and embedded in the
  binary, so rebuild and commit it with every web change; CI checks it matches.
- `tests/`: [live proof scripts](tests/README.md) (Python) run against a real instance.
- `scripts/`: operator tools such as `backup_sqlite.py` and `ezllm-mac.sh`.
- `docs/`: user docs; `docs/design/`: design records. [EXPLORING.md](EXPLORING.md) is the
  code tour.

CI runs [test.yml](.github/workflows/test.yml) before
[publish.yml](.github/workflows/publish.yml) publishes anything, so a failing suite
never produces an image or release. Pushes to `development` publish
`ghcr.io/c4lyp5o/ezllm:development`; `:latest` follows `master`. All four subscription bridges share one streaming
transport in `internal/bridgeutil` ([design](docs/design/shared-bridge-transport-design.md)).

## Documentation

The docs describe the version they ship with. Running another one? Open its tag, for
example [v2.2.0](https://github.com/c4lyp5o/ezllm/tree/v2.2.0), or see
[upgrading](docs/upgrading.md).

| Document | Contents |
|---|---|
| [docs/install-binary.md](docs/install-binary.md) | Installing the released binary on Linux, macOS or Windows: start, service, update, go back |
| [docs/wsl.md](docs/wsl.md) | Bridges on Windows: ezllm in WSL |
| [docs/API.md](docs/API.md) | Public and admin API reference |
| [docs/environment.md](docs/environment.md) | Every environment variable and its default |
| [docs/logging.md](docs/logging.md) | Logs: levels, format, where they go |
| [docs/metrics.md](docs/metrics.md) | Prometheus `/metrics`: enabling, auth, metric names, example queries |
| [docs/settings-runbook.md](docs/settings-runbook.md) | Dashboard settings, timezones, sessions and diagnostics |
| [docs/recovery-runbook.md](docs/recovery-runbook.md) | Encrypted backups, restore and upgrades |
| [docs/master-key-runbook.md](docs/master-key-runbook.md) | Key encryption: paths, backup, rotation |
| [docs/translation-runbook.md](docs/translation-runbook.md) | Translation between APIs: when it happens, how to turn it off |
| [docs/pricing-runbook.md](docs/pricing-runbook.md) | Model price catalog: refresh, import, query |
| [docs/bridge-setup.md](docs/bridge-setup.md) | Turning on a bridge: one page per plan, and troubleshooting |
| [docs/upgrading.md](docs/upgrading.md) | What to change when you come from an older version |
| [docs/bridges/troubleshooting.md](docs/bridges/troubleshooting.md) | What a bridge log line means, and what to do |
| [docs/claude-bridge-runbook.md](docs/claude-bridge-runbook.md) | Claude subscription bridge |
| [docs/codex-bridge-runbook.md](docs/codex-bridge-runbook.md) | ChatGPT subscription bridge, including model choice for long tool tasks |
| [docs/copilot-bridge-runbook.md](docs/copilot-bridge-runbook.md) | GitHub Copilot subscription bridge |
| [docs/agy-bridge-runbook.md](docs/agy-bridge-runbook.md) | Google Antigravity subscription bridge |
| [docs/bridge-update-runbook.md](docs/bridge-update-runbook.md) | Automatic bridge CLI updates, holds and rollback |
| [docs/claude-web-runbook.md](docs/claude-web-runbook.md), [docs/gemini-web-runbook.md](docs/gemini-web-runbook.md) | Unofficial web sign-in providers, and their risks |
| [VERSION.md](VERSION.md) | Release history |
| [CHANGELOG.md](CHANGELOG.md) | Detailed change history |
| [EXPLORING.md](EXPLORING.md) | Code tour: reading order, invariants, landmarks |
| [DEVELOPER.md](DEVELOPER.md) | Workflow, conventions, branches and releases |
| [AGENTS.md](AGENTS.md) | Rules for AI agents and maintainers |
| [docs/](docs/) | Every user doc |
| [docs/design/](docs/design/README.md) | Design records, specs and measurements (not needed to run ezllm) |

## License

[MIT](LICENSE)
