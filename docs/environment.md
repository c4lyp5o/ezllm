# Environment variables

Every environment variable ezllm reads, with its default. Supported operator
controls follow: command-line flags → `EZLLM_*` environment → saved SQLite
settings → explicit `config.yaml` → built-in defaults. Deployment-only options
continue to use flags/environment/YAML/defaults without a database override.

"Image" below means the published Docker image (`Dockerfile`); "compose" means
the shipped `compose.example.yml`, which lists every variable below. Copy it to
`compose.yml` (gitignored) and edit the copy.

Running a released binary instead of the container? Start at
[install-binary.md](install-binary.md) — the layout, the master-key path and the
service unit differ from the image, the variables do not.

## New install: what to set

Most installs need three settings, and the image already handles listen address
and data location.

1. **Usage timezone** — choose it in Settings → General, or set
   **`EZLLM_USAGE_TZ`** to enforce it from the deployment. Use an IANA name
   (`Europe/Berlin`, `America/New_York`, `Asia/Kuala_Lumpur`). It controls
   report days, model rules set to `instance`, client-token budget days and
   date-only token expiries, daily price/backup schedules and log timestamps
   (it is the only timezone setting; `TZ` is ignored). An explicit rule
   timezone remains independent. An environment override locks the dashboard
   control. Without an override, saved settings and YAML are considered before
   UTC; an unresolvable effective name stops boot.
2. **`EZLLM_DASHBOARD_PASSWORD_FILE`** (or `EZLLM_DASHBOARD_PASSWORD`) — the
   first dashboard password, at least 12 characters. Skip it and the first boot
   prints a one-time setup code to `docker logs ezllm` instead.
3. **The subscription bridges stay off** (`EZLLM_CLAUDE_BRIDGE`,
   `EZLLM_CODEX_BRIDGE`, `EZLLM_AGY_BRIDGE`) unless you have the vendor CLI
   and a subscription login on the host; turned on without them, the boot
   fails. To use one, follow [Turn on a bridge](bridge-setup.md).

A **binary install** (no container) has one more: pin
**`EZLLM_MASTER_KEY_FILE`** beside the data directory. The default is
home-global, so a second instance silently shares the first one's key and a
backup of `data/` alone leaves the provider keys unreadable. See
[install-binary.md](install-binary.md).

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
| `EZLLM_USAGE_TZ` | saved setting → YAML `usage_timezone` → UTC | The one timezone setting, as a deployment override: report days, inherited model-rule windows/caps, client-token budgets and date-only expiries, daily schedules and log timestamps; locks the dashboard control. An unresolvable name fails boot. Report `?tz=` overrides the default grouping timezone. |
| `EZLLM_LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error`. Env-only (needed before the config is read); an unknown value logs a warning and uses `info`. The effective value is stated at boot. |
| `EZLLM_LOG_COLOR` | `auto` | `auto` (color only on a terminal) \| `always` \| `never`. |
| `EZLLM_LOG_FORMAT` | `console` | `console` \| `json`. JSON emits one object per line for a collector; the console format is for `docker logs`. See [logging.md](logging.md). |
| `NO_COLOR` | unset | Any value disables color under `auto`. |
| `EZLLM_TRUSTED_PROXIES` | unset | Comma-separated IPs/CIDRs of reverse proxies in front of ezllm. The sign-in/setup rate limiter reads `X-Forwarded-For` only from these; unset, it keys on the socket peer. Set it behind any proxy, or every client shares the proxy's limit. |
| `TZ` | not used | ezllm ignores the process timezone. `EZLLM_USAGE_TZ` (or the dashboard/YAML setting) is the one timezone: report days, model rules, client-token budgets, schedules and log timestamps. |

## Secrets

| Variable | Default | What it does |
|---|---|---|
| `EZLLM_MASTER_KEY` | unset | Master key for at-rest encryption of provider keys. Wins over the file. |
| `EZLLM_MASTER_KEY_FILE` | `<data_dir>/ezllm-master.key` | Master key file; generated on first boot. Do not change it on an existing install without moving the key. |
| `EZLLM_DASHBOARD_PASSWORD` | unset | First dashboard password (at least 12 characters), used only while setup is pending. Never overwrites a password already set. |
| `EZLLM_DASHBOARD_PASSWORD_FILE` | unset | Same, read from a file (Docker/compose secrets); one trailing newline is trimmed. The plain variable wins. |

## Feature switches

| Variable | Default | What it does |
|---|---|---|
| `EZLLM_TRANSLATION_ENABLED` | saved setting → YAML → on | The one switch for wire-protocol translation (pairwise translators and the universal matrix). `false` turns it off without a rebuild. Non-boolean values fail the boot. See [translation-runbook.md](translation-runbook.md). |
| `EZLLM_COMBO_AUTOPROBE` | saved setting → YAML `automatic_probing` → on | An explicit value locks the dashboard control. `off` disables automatic combo-hop and free-model probes; manual probes remain available. |
| `EZLLM_PRICE_REFRESH` | saved setting → YAML `price_refresh` → on | An explicit value locks the dashboard control. `off` stops scheduled price refresh (boot + daily); manual refresh remains available. See [pricing-runbook.md](pricing-runbook.md). |
| `EZLLM_UPDATE_CHECK` | saved setting → on | An explicit value locks the dashboard control. `off` stops the daily check of GitHub's latest ezllm release (boot + daily). The request carries nothing about this instance, and nothing is installed. |

## All subscription bridges

| Variable | Default | What it does |
|---|---|---|
| `EZLLM_BRIDGE_QUEUE` | `8` | Requests allowed to wait for a CLI slot, applied to each bridge (Claude, Codex, Antigravity, Copilot) separately. Past it that bridge answers 429 `queue_full` with `Retry-After: 5`, and the router parks the hop and fails over. A positive integer, anything else fails the boot. One value for all bridges: the right size follows the clients' burst and the shared 300 s runner timeout, while concurrency stays per bridge. |

## Claude subscription bridge

Setup and operation: [claude-bridge-runbook.md](claude-bridge-runbook.md).

| Variable | Default | What it does |
|---|---|---|
| `EZLLM_CLAUDE_BRIDGE` | off | `on` starts the loopback surface. Enabled without a usable CLI or login, the boot fails. |
| `EZLLM_CLAUDE_BIN` | `/opt/claude-bridge/current/claude` | Official Claude Code CLI binary inside the container: the updater-managed bridge root mounted at `/opt/claude-bridge`. |
| `EZLLM_CLAUDE_HOME` | `/claude-sub` | Subscription login directory inside the container (compose mounts it here); passed to the CLI as `CLAUDE_CONFIG_DIR`. |
| `EZLLM_CLAUDE_BRIDGE_CONCURRENCY` | `1` | Concurrent Claude CLI processes; a positive integer, anything else fails the boot. |
| `EZLLM_CLAUDE_BRIDGE_ADDR` | `127.0.0.1:8790` | Bridge listen address; non-loopback addresses are refused. |
| `EZLLM_CLAUDE_BRIDGE_TOKEN_FILE` | `/data/claude-bridge.token` | Shared secret for the account key; generated (0600) when missing. |
| `USER`, `LOGNAME` | unset | Both must be set when the bridge is on (the CLI's login check reads process identity). |
| `CLAUDE_CODE_*`, `DISABLE_*` | unset | Passed through to the CLI for tuning. `ANTHROPIC_*` and `CLAUDE_CONFIG_DIR` never pass. |

## Codex (ChatGPT subscription) bridge

Setup, platform requirements and the lockdown: [codex-bridge-runbook.md](codex-bridge-runbook.md). Linux only. Every child runs Codex's `workspace-write` policy in its own temp directory.

| Variable | Default | What it does |
|---|---|---|
| `EZLLM_CODEX_BRIDGE` | off | `on` starts the loopback surface. Enabled without a usable CLI, login or required app-server methods, the boot fails. |
| `EZLLM_CODEX_BIN` | `/opt/codex-bridge/current/bin/codex` | Official Codex CLI binary inside the container: the updater-managed bridge root mounted at `/opt/codex-bridge`. |
| `EZLLM_CODEX_HOME` | `/codex-sub` | `CODEX_HOME` holding the subscription login (compose mounts it here). |
| `EZLLM_CODEX_BRIDGE_ADDR` | `127.0.0.1:8791` | Bridge listen address; non-loopback addresses are refused. |
| `EZLLM_CODEX_BRIDGE_TOKEN_FILE` | `/data/codex-bridge.token` | Shared secret for the account key; generated (0600) when missing. |
| `EZLLM_CODEX_BRIDGE_CONCURRENCY` | `1` | Concurrent Codex children; a positive integer, anything else fails the boot. |
| `EZLLM_CODEX_BRIDGE_CACHE` | on where supported | Unset, empty-template forks are used whenever the CLI supports them; otherwise the original path is used and `/status` reports why. `on` requires support; `off` opts out. Any other value fails startup. No idle CLI workers; bounded native parent retention and cleanup. Set before startup; recreate containers to change. |

## Antigravity (Google subscription) bridge

Setup and the login that must avoid the desktop keyring: [agy-bridge-runbook.md](agy-bridge-runbook.md). Linux only.

| Variable | Default | What it does |
|---|---|---|
| `EZLLM_AGY_BRIDGE` | off | `on` starts the loopback surface. Enabled without a usable CLI, a token file or a working `agy models`, the boot fails. |
| `EZLLM_AGY_BIN` | `/opt/agy-bridge/current/agy` | Official Antigravity CLI binary inside the container: the updater-managed bridge root mounted at `/opt/agy-bridge`. |
| `EZLLM_AGY_HOME` | `/agy-sub` | `HOME` for the CLI: holds `.gemini/antigravity-cli/antigravity-oauth-token` (compose mounts the login here, read-write). |
| `EZLLM_AGY_BRIDGE_ADDR` | `127.0.0.1:8792` | Bridge listen address; non-loopback addresses are refused. |
| `EZLLM_AGY_BRIDGE_TOKEN_FILE` | `/data/agy-bridge.token` | Shared secret for the account key; generated (0600) when missing. |
| `EZLLM_AGY_BRIDGE_CONCURRENCY` | `1` | Concurrent `agy` children (~207 MB each); a positive integer, anything else fails the boot. |
| `EZLLM_AGY_BRIDGE_CACHE` | on when the CLI supports it | Exact-history conversation resume for tool-adapter requests. Unset, it is on whenever the CLI supports both `--json-schema` (caller tools) and `--conversation`; an explicit `on` without either fails the boot; `off` disables it. Retains bounded metadata/private workspaces, no idle CLI processes. Complete caller tool-result groups can resume. Restart to change. |

## GitHub Copilot subscription bridge

The OAuth provider and its two variables (`GITHUB_COPILOT_CLI_VERSION`,
`GITHUB_OAUTH_CLIENT_ID`) are deleted — nothing reads them. The login is the
CLI's own, so there is no client id to configure. Runbook:
`docs/copilot-bridge-runbook.md`.

| Variable | Default | What it does |
|---|---|---|
| `EZLLM_COPILOT_BRIDGE` | off | `on` starts the loopback surface; without it the provider add answers 409 naming this variable. |
| `EZLLM_COPILOT_BRIDGE_WARM_POOL` | `on` | CLI process reuse; sessions are fresh unless session caching is enabled; `off`/`false`/`0` disables it. Existing concurrency and queue limits apply. Restart to change. |
| `EZLLM_COPILOT_BRIDGE_CACHE` | follows warm pool | Exact-history reuse of completed native conversations. Unset, it follows the warm pool; an explicit `on` without the pool fails the boot; `off` disables it. Active tool-result continuations, images and `auto` use fresh sessions. Existing worker expiry and a 1 MiB history bound apply. Restart to change. |
| `EZLLM_COPILOT_BIN` | `/opt/copilot-bridge/current/copilot` | The GitHub Copilot CLI binary: the updater-managed bridge root mounted at `/opt/copilot-bridge`. |
| `EZLLM_COPILOT_HOME` | `/copilot-sub` | `COPILOT_HOME` for the CLI — the bridge's dedicated login directory, mounted from `~/.copilot-sub` (read-write; the CLI writes session state). |
| `EZLLM_COPILOT_BRIDGE_CONCURRENCY` | `1` | Concurrent Copilot CLI children; a positive integer, anything else fails the boot. |
| `EZLLM_COPILOT_BRIDGE_TIMEOUT` | `300s` | Per-turn limit, a Go duration of at least `1s`. |
| `EZLLM_COPILOT_BRIDGE_ADDR` | `127.0.0.1:8793` | Listen address; non-loopback is refused. |
| `EZLLM_COPILOT_BRIDGE_TOKEN_FILE` | `/data/copilot-bridge.token` | Shared secret for the account key; generated (0600) when missing. |

## Compose-only

Substituted into `compose.example.yml`; the binary never reads them. The data
directory and bridge mount paths are written into your `compose.yml` directly; inside
the container the data path is always `EZLLM_DATA_DIR`, `/data`.

| Variable | Default | Used for |
|---|---|---|
| `UID`, `GID` | `1000` | Container user. |
| `EZLLM_PORT` | `20129` | Published host port. |

## Tests only

| Variable | What it does |
|---|---|
| `EZLLM_CODEX_LIVE` | `1` runs the Codex bridge live tests against the real CLI and login (`go test -short ./internal/codexbridge/ -run Live -v`). |
| `EZLLM_CODEX_LIVE_BIN`, `EZLLM_CODEX_LIVE_HOME` | The CLI and `CODEX_HOME` those live tests use; default `codex` on `PATH` and `~/.codex-sub`. |
| `EZLLM_AGY_LIVE` | `1` runs the Antigravity bridge live tests against the real CLI and login (`go test -short ./internal/agybridge/ -run Live -v`); a turn spends plan quota. |
| `EZLLM_AGY_BIN`, `EZLLM_AGY_HOME`, `EZLLM_AGY_MODEL` | The CLI, login directory and model those live tests use. On the host, unlike the container defaults above, they default to `agy` on `PATH`, `~/.agy-sub` and `gemini-3.8-flash-low`. |
| `EZLLM_REHEARSE_DB` | Path to a **copy** of the production database; runs the store rehearsal test (`go test -short ./internal/store/ -run Rehearse -v`). |

## Operator settings and encrypted backups

The Settings page manages ordinary runtime policy. For supported fields the
precedence is flags → environment → saved SQLite settings → explicit YAML →
built-in defaults. Environment-controlled fields are read-only in the dashboard.
Reset removes the saved override; it does not rewrite YAML. Existing settings
are not imported into SQLite automatically.

The supported YAML additions are `usage_timezone`, `price_refresh`,
`automatic_probing`, and `backup.directory` / `backup.secret_file`. Existing
retry, cooldown, translation, and model-refresh YAML fields remain supported.
Network binding, data/key locations, trusted proxies and bridge startup remain
deployment configuration. Browser theme, motion and analytics range are local
preferences.

| Variable | Default | What it does |
|---|---|---|
| `EZLLM_BACKUP_DIR` | unset | Absolute destination directory for encrypted backups; overrides `backup.directory`. Mount separate storage here for protection from loss of the data disk. |
| `EZLLM_BACKUP_SECRET_FILE` | unset | File containing a separate backup encryption secret of at least 24 bytes after trimming whitespace; overrides `backup.secret_file`. Must differ from the active master key. Read at startup; retain it separately for recovery. |

Automation is disabled until enabled in Settings. With storage configured,
manual backups are available independently of the schedule. Defaults on enabling:
daily at 03:00 in the instance timezone, retain seven verified successful backups.
The schedule hour and retention (1–365) are editable. Each job has a five-minute
budget. Pruning starts only after a new archive verifies and is published;
snapshot/verification failures preserve earlier backups. The destination and encryption
secret require a restart to change.

See [settings-runbook.md](settings-runbook.md) for live-update behavior and
[recovery-runbook.md](recovery-runbook.md) for encrypted offline recovery.

## Request admission (configuration YAML)

`request_limits.global_inflight` defaults to 32,
`request_limits.per_client_inflight` to 8 and
`request_limits.body_memory_mib` to 256. These are YAML settings, not environment
variables. All must be positive and at most 4096; per-client concurrency cannot
exceed global concurrency, and the memory budget must cover `max_body_mib`.
They cannot be disabled with zero. Configure them before startup.

Admission runs after authentication and before reading inference bodies. It
reserves the declared `Content-Length`, or a whole allowed body when the length
is unknown (chunked input), shrinks to the actual body size after completion and retains that reservation until the
handler/stream ends. Excess requests receive 429 with `Retry-After: 1`; there is
no unbounded waiting queue. Native CLI concurrency limits continue to apply too.
The budget counts retained input bytes, not JSON object overhead, response
buffers or CLI processes; choose limits appropriate to measured process memory.
