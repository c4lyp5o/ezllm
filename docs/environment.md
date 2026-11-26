# Environment variables

Every environment variable ezllm reads, and its default. Set them in `compose.yml`, in the
systemd service, or in the terminal before you start ezllm. Restart ezllm after a change.

When the same setting is set in several places, the first one wins: command-line flag,
environment variable, value saved in **Settings**, `config.yaml`, default. A setting fixed
here is locked in the dashboard. Details: [Settings](settings-runbook.md#which-value-wins).

Every on/off switch takes `on` or `off`, in any case; `true`/`false` and `1`/`0` work too.
Anything else stops ezllm from starting, with a message naming the variable.

"Image" means the Docker image; its paths (`/data`, `/opt/…`) differ from a binary
install. `compose.example.yml` lists every variable below.

## What most installs set

- **`EZLLM_MASTER_KEY_FILE`**: where the master key is. The quick start sets it to the
  data folder. Back the key up with the database ([master key](master-key-runbook.md)).
- **`EZLLM_USAGE_TZ`**: your timezone, such as `Europe/Berlin`, or choose it in
  **Settings** → **General**.
- **`EZLLM_DASHBOARD_PASSWORD_FILE`**: the first dashboard password, for an install with
  nobody at the keyboard. Without it, the first start prints a setup code.
- **`EZLLM_TRUSTED_PROXIES`**: when ezllm is behind a reverse proxy.

Bridges stay off until you [turn one on](bridge-setup.md).

## Server

| Variable | Default | What it does |
|---|---|---|
| `EZLLM_ADDR` | `listen` in `config.yaml`; image: `0.0.0.0:20129` | The address ezllm listens on. `-addr` overrides it |
| `EZLLM_DATA_DIR` | `data_dir` in `config.yaml`; image: `/data` | The data folder |
| `EZLLM_USAGE_TZ` | the saved setting, then `usage_timezone`, then UTC | The only timezone ezllm uses: report days, rule windows, API key budgets and date-only expiries, daily schedules, log times. An unknown name stops the start. `TZ` is ignored |
| `EZLLM_TRUSTED_PROXIES` | unset | Reverse proxies in front of ezllm, as IPs or CIDRs, comma-separated. The sign-in rate limit reads `X-Forwarded-For` only from these. Without it, every client behind the proxy shares one limit |
| `EZLLM_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. An unknown value logs a warning and uses `info` |
| `EZLLM_LOG_FORMAT` | `console` | `console`, or `json` for one object per line ([logging](logging.md)) |
| `EZLLM_LOG_COLOR` | `auto` | `auto` (colour only in a terminal), `always` or `never` |
| `NO_COLOR` | unset | Any value turns colour off under `auto` |

## Secrets

| Variable | Default | What it does |
|---|---|---|
| `EZLLM_MASTER_KEY` | unset | The master key itself. Wins over the file |
| `EZLLM_MASTER_KEY_FILE` | `ezllm-master.key` in the data folder | The master key file, made on the first start. Move the key before you change this |
| `EZLLM_DASHBOARD_PASSWORD` | unset | The first dashboard password: 12 characters to 72 bytes. Used only until setup is done; it never replaces a password already set |
| `EZLLM_DASHBOARD_PASSWORD_FILE` | unset | The same, from a file. One trailing newline is removed. The plain variable wins |

## Features

Each of these locks its switch in **Settings** when set.

| Variable | Default | What it does |
|---|---|---|
| `EZLLM_TRANSLATION_ENABLED` | on | `false` turns off [translation](translation-runbook.md) between APIs. Anything other than true or false stops the start |
| `EZLLM_COMBO_AUTOPROBE` | on | `off` stops automatic tests of new combo hops and free models. You can still test by hand |
| `EZLLM_PRICE_REFRESH` | on | `off` stops the daily [price](pricing-runbook.md) refresh. You can still refresh by hand |
| `EZLLM_UPDATE_CHECK` | on | `off` stops the daily check for a new ezllm release. The check sends nothing about your ezllm |

## Backups

Both are needed for [encrypted backups](recovery-runbook.md#encrypted-backups).

| Variable | Default | What it does |
|---|---|---|
| `EZLLM_BACKUP_DIR` | `backup.directory` in `config.yaml` | The folder backups go to, as an absolute path. Use another disk |
| `EZLLM_BACKUP_SECRET_FILE` | `backup.secret_file` in `config.yaml` | A file with the backup secret: at least 24 characters, not the master key. A missing file stops the start |

## All bridges

| Variable | Default | What it does |
|---|---|---|
| `EZLLM_BRIDGE_QUEUE` | `8` | How many requests may wait for each bridge. Past it, the bridge answers `429 queue_full` with `Retry-After: 5`, and a combo moves to its next hop |

Each bridge also has these. A wrong value stops the start.

| Variable | Default | What it does |
|---|---|---|
| `EZLLM_<BRIDGE>_BRIDGE_CONCURRENCY` | `1` | How many CLI processes run at once |
| `EZLLM_<BRIDGE>_BRIDGE_ADDR` | see below | The bridge's own address. It must be on this computer (loopback) |
| `EZLLM_<BRIDGE>_BRIDGE_TOKEN_FILE` | `<bridge>-bridge.token` in the data folder | The key between ezllm and the bridge. Made when missing |

## Claude bridge

[Turn on Claude](bridges/claude.md) · [reference](claude-bridge-runbook.md)

| Variable | Default | What it does |
|---|---|---|
| `EZLLM_CLAUDE_BRIDGE` | off | `on` starts the bridge. Without a CLI that runs, or without `USER` and `LOGNAME`, ezllm does not start. A missing login does not stop it: the start line says `login=logged-in` or why not |
| `EZLLM_CLAUDE_BIN` | `/opt/claude-bridge/current/claude` | The Claude Code CLI |
| `EZLLM_CLAUDE_HOME` | `/claude-sub` | The bridge's login folder |
| `EZLLM_CLAUDE_BRIDGE_ADDR` | `127.0.0.1:8790` | |
| `USER`, `LOGNAME` | unset | Both must be set: the CLI's login check reads them |
| `CLAUDE_CODE_*`, `DISABLE_*` | unset | Passed to the CLI. `ANTHROPIC_*` never is |

## Codex bridge

[Turn on ChatGPT](bridges/codex.md) · [reference](codex-bridge-runbook.md). Linux only.

| Variable | Default | What it does |
|---|---|---|
| `EZLLM_CODEX_BRIDGE` | off | `on` starts the bridge. Without a working CLI or login, ezllm does not start |
| `EZLLM_CODEX_BIN` | `/opt/codex-bridge/current/bin/codex` | The Codex CLI |
| `EZLLM_CODEX_HOME` | `/codex-sub` | The bridge's login folder |
| `EZLLM_CODEX_BRIDGE_ADDR` | `127.0.0.1:8791` | |
| `EZLLM_CODEX_BRIDGE_CACHE` | on when the CLI supports it | `off` turns [caching](../README.md#caching) off. `on` stops the start when the CLI cannot cache |

## Antigravity bridge

[Turn on Antigravity](bridges/antigravity.md) · [reference](agy-bridge-runbook.md). Linux only.

| Variable | Default | What it does |
|---|---|---|
| `EZLLM_AGY_BRIDGE` | off | `on` starts the bridge. Without a working CLI, login file or `agy models`, ezllm does not start |
| `EZLLM_AGY_BIN` | `/opt/agy-bridge/current/agy` | The Antigravity CLI |
| `EZLLM_AGY_HOME` | `/agy-sub` | The CLI's home folder, which holds the login |
| `EZLLM_AGY_BRIDGE_ADDR` | `127.0.0.1:8792` | |
| `EZLLM_AGY_BRIDGE_CACHE` | on when the CLI supports it | `off` turns caching off. `on` stops the start when the CLI cannot cache |

Each Antigravity process uses about 200 MB of memory.

## Copilot bridge

[Turn on GitHub Copilot](bridges/copilot.md) · [reference](copilot-bridge-runbook.md)

| Variable | Default | What it does |
|---|---|---|
| `EZLLM_COPILOT_BRIDGE` | off | `on` starts the bridge. Adding the provider without it answers `409` |
| `EZLLM_COPILOT_BIN` | `/opt/copilot-bridge/current/copilot` | The Copilot CLI |
| `EZLLM_COPILOT_HOME` | `/copilot-sub` | The bridge's login folder. It must be writable |
| `EZLLM_COPILOT_BRIDGE_ADDR` | `127.0.0.1:8793` | |
| `EZLLM_COPILOT_BRIDGE_WARM_POOL` | `on` | Keeps CLI processes ready between requests. `off` starts a new one each time |
| `EZLLM_COPILOT_BRIDGE_CACHE` | same as the warm pool | `off` turns caching off. `on` without the warm pool stops the start |
| `EZLLM_COPILOT_BRIDGE_TIMEOUT` | `300s` | The longest one answer may take, at least `1s` |

## Compose only

`compose.example.yml` uses these; ezllm itself does not read them.

| Variable | Default | Used for |
|---|---|---|
| `UID`, `GID` | `1000` | The user the container runs as |
| `EZLLM_PORT` | `20129` | The port on your computer |

## Request limits

These are in `config.yaml`, not the environment. They protect ezllm from too many requests
at once.

| Setting | Default | What it limits |
|---|---|---|
| `request_limits.global_inflight` | `32` | Requests running at once |
| `request_limits.per_client_inflight` | `8` | Requests running at once per API key |
| `request_limits.body_memory_mib` | `256` | Memory for request bodies being read |

Each must be between 1 and 4096. The per-key limit cannot be above the total, and the
memory must hold at least one `max_body_mib` body. A request past a limit gets `429` with
`Retry-After: 1`; nothing waits in a queue.

## Tests only

| Variable | What it does |
|---|---|
| `EZLLM_CODEX_LIVE` | `1` runs the Codex live tests against the real CLI and login (`go test -short ./internal/codexbridge/ -run Live -v`) |
| `EZLLM_CODEX_LIVE_BIN`, `EZLLM_CODEX_LIVE_HOME` | The CLI and login for them. Default: `codex` on `PATH`, `~/.codex-sub` |
| `EZLLM_AGY_LIVE` | `1` runs the Antigravity live tests (`go test -short ./internal/agybridge/ -run Live -v`). They use plan quota |
| `EZLLM_AGY_BIN`, `EZLLM_AGY_HOME`, `EZLLM_AGY_MODEL` | The CLI, login and model for them. Default: `agy` on `PATH`, `~/.agy-sub`, `gemini-3.8-flash-low` |
| `EZLLM_REHEARSE_DB` | A **copy** of a real database, for the store rehearsal test (`go test -short ./internal/store/ -run Rehearse -v`) |
