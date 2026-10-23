# Environment variables

Every environment variable ezllm reads, with its default. Settings that also
exist in `config.yaml` follow: command-line flags → `EZLLM_*` environment →
`config.yaml` → built-in defaults.

"Image" below means the published Docker image (`Dockerfile`); "compose" means
the shipped `compose.example.yml`, which lists every variable below. Copy it to
`compose.yml` (gitignored) and edit the copy.

## New install: what to set

Most installs need three settings, and the image already handles listen address
and data location.

1. **`EZLLM_USAGE_TZ`** — your time zone, as an IANA name
   (`Europe/Berlin`, `America/New_York`, `Asia/Kuala_Lumpur`). It decides where
   a "day" starts on the dashboard's day view and in daily usage rules. Unset
   means UTC; a name that does not resolve stops the boot rather than silently
   shifting every day.
2. **`EZLLM_DASHBOARD_PASSWORD_FILE`** (or `EZLLM_DASHBOARD_PASSWORD`) — the
   first dashboard password, at least 12 characters. Skip it and the first boot
   prints a one-time setup code to `docker logs ezllm` instead.
3. **The subscription bridges stay off** (`EZLLM_CLAUDE_BRIDGE`,
   `EZLLM_CODEX_BRIDGE`) unless you have the vendor CLI and a subscription
   login on the host; turned on without them, the boot fails. To use one,
   follow [claude-bridge-runbook.md](claude-bridge-runbook.md) or
   [codex-bridge-runbook.md](codex-bridge-runbook.md).

Behind a reverse proxy, also set `EZLLM_TRUSTED_PROXIES`. Set these in your
`compose.yml`:

```sh
cp compose.example.yml compose.yml   # then edit EZLLM_USAGE_TZ and the rest
```

Back up the master key with the database. It is generated on first boot
(`/data/ezllm-master.key` under compose), and losing it makes every stored
provider key unreadable. See [master-key-runbook.md](master-key-runbook.md).

## Server

| Variable | Default | What it does |
|---|---|---|
| `EZLLM_ADDR` | config `listen` (loopback); image: `0.0.0.0:20129` | Listen address. `-addr` overrides it. |
| `EZLLM_DATA_DIR` | config `data_dir` (`data`); image: `/data` | Ledger and data location. |
| `EZLLM_USAGE_TZ` | UTC | IANA zone for day bucketing (dashboard day view, `group_by=day`, daily usage rules). An unresolvable name fails the boot. A request's `?tz=` overrides it. |
| `EZLLM_LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error`. Env-only (needed before the config is read); an unknown value logs a warning and uses `info`. |
| `EZLLM_LOG_COLOR` | `auto` | `auto` (color only on a terminal) \| `always` \| `never`. |
| `NO_COLOR` | unset | Any value disables color under `auto`. |
| `EZLLM_TRUSTED_PROXIES` | unset | Comma-separated IPs/CIDRs of reverse proxies in front of ezllm. The sign-in/setup rate limiter reads `X-Forwarded-For` only from these; unset, it keys on the socket peer. Set it behind any proxy, or every client shares the proxy's limit. |
| `EZLLM_WEB_DIR` | unset (embedded `web/dist`) | Serve the dashboard from a directory of built assets instead, to iterate on the UI without a Go rebuild. |
| `TZ` | image: `Asia/Kuala_Lumpur` | The container clock: log timestamps only. Usage days follow `EZLLM_USAGE_TZ`, not this. |

## Secrets

| Variable | Default | What it does |
|---|---|---|
| `EZLLM_MASTER_KEY` | unset | Master key for at-rest encryption of provider keys. Wins over the file. |
| `EZLLM_MASTER_KEY_FILE` | `~/.hermes/secrets/ezllm-master.key` (data dir when there is no usable home); compose: `/data/ezllm-master.key` | Master key file; generated on first boot. Do not change it on an existing install without moving the key. |
| `EZLLM_DASHBOARD_PASSWORD` | unset | First dashboard password (at least 12 characters), used only while setup is pending. Never overwrites a password already set. |
| `EZLLM_DASHBOARD_PASSWORD_FILE` | unset | Same, read from a file (Docker/compose secrets); one trailing newline is trimmed. The plain variable wins. |

## Feature switches

| Variable | Default | What it does |
|---|---|---|
| `EZLLM_TRANSLATION_ENABLED` | on | The one switch for wire-protocol translation (pairwise translators and the universal matrix). `false` turns it off without a rebuild. Non-boolean values fail the boot. See [translation-runbook.md](translation-runbook.md). |
| `EZLLM_TRANSLATION_UNIVERSAL_ENABLED` | unset | **Deprecated** (merged into `EZLLM_TRANSLATION_ENABLED`). Still honored when the new variable is unset, with a warning at boot. |
| `EZLLM_COMBO_AUTOPROBE` | on | `off` stops automatic probing of free models after a catalog refresh. |
| `EZLLM_PRICE_REFRESH` | on | `off` stops the price catalog refresh (boot + daily). See [pricing-runbook.md](pricing-runbook.md). |

## Claude subscription bridge

Setup and operation: [claude-bridge-runbook.md](claude-bridge-runbook.md).

| Variable | Default | What it does |
|---|---|---|
| `EZLLM_CLAUDE_BRIDGE` | off | `on` starts the loopback surface. Enabled without a usable CLI or login, the boot fails. |
| `EZLLM_CLAUDE_BIN` | `/opt/claude/claude` | Official Claude Code CLI binary inside the container (compose mounts it here). |
| `EZLLM_CLAUDE_CONFIG_DIR` | `/claude-sub` | Subscription login directory inside the container (compose mounts it here). |
| `EZLLM_CLAUDE_BRIDGE_ADDR` | `127.0.0.1:8790` | Bridge listen address; non-loopback addresses are refused. |
| `EZLLM_CLAUDE_BRIDGE_TOKEN_FILE` | `/data/claude-bridge.token` | Shared secret for the account key; generated (0600) when missing. |
| `USER`, `LOGNAME` | unset | Both must be set when the bridge is on (the CLI's login check reads process identity). |
| `CLAUDE_CODE_*`, `DISABLE_*` | unset | Passed through to the CLI for tuning. `ANTHROPIC_*` and `CLAUDE_CONFIG_DIR` never pass. |

## Codex (ChatGPT subscription) bridge

Setup, platform requirements and the lockdown: [codex-bridge-runbook.md](codex-bridge-runbook.md). Linux only.

| Variable | Default | What it does |
|---|---|---|
| `EZLLM_CODEX_BRIDGE` | off | `on` starts the loopback surface. Enabled without a usable CLI, login or required app-server methods, the boot fails. |
| `EZLLM_CODEX_BIN` | `/opt/codex/bin/codex` | Official Codex CLI binary inside the container (compose mounts the release directory at `/opt/codex`). |
| `EZLLM_CODEX_HOME` | `/codex-sub` | `CODEX_HOME` holding the subscription login (compose mounts it here). |
| `EZLLM_CODEX_BRIDGE_ADDR` | `127.0.0.1:8791` | Bridge listen address; non-loopback addresses are refused. |
| `EZLLM_CODEX_BRIDGE_TOKEN_FILE` | `/data/codex-bridge.token` | Shared secret for the account key; generated (0600) when missing. |
| `EZLLM_CODEX_BRIDGE_CONCURRENCY` | `2` | Concurrent Codex children; a positive integer, anything else fails the boot. |
| `EZLLM_CODEX_TOOLS_WITHOUT_LANDLOCK` | unset | `on` allows client tools on a host without Landlock. Read the runbook's platform section before setting it. |

## GitHub Copilot provider

| Variable | Default | What it does |
|---|---|---|
| `GITHUB_COPILOT_CLI_VERSION` | built-in | Overrides the Copilot CLI version ezllm presents upstream. |
| `GITHUB_OAUTH_CLIENT_ID` | built-in | Overrides the OAuth client id for the GitHub device login. |

## Compose-only

Substituted into `compose.example.yml`; the binary never reads them. Bridge
mount paths are written into your `compose.yml` directly.

| Variable | Default | Used for |
|---|---|---|
| `UID`, `GID` | `1000` | Container user. |
| `EZLLM_PORT` | `20129` | Published host port. |
| `EZLLM_DATA` | `./data` | Host directory mounted at `/data`. |

## Tests only

| Variable | What it does |
|---|---|
| `EZLLM_CODEX_LIVE` | `1` runs the Codex bridge live tests against the real CLI and login (`go test ./internal/codexbridge/ -run Live -v`). |
| `EZLLM_CODEX_LIVE_BIN`, `EZLLM_CODEX_LIVE_HOME` | The CLI and `CODEX_HOME` those live tests use; default `codex` on `PATH` and `~/.codex-sub`. |
| `EZLLM_REHEARSE_DB` | Path to a **copy** of the production database; runs the store rehearsal test (`go test ./internal/store/ -run Rehearse -v`). |
