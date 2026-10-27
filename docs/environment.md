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
   report days, model rules set to `instance`, and daily price/backup schedules.
   An explicit rule timezone remains independent; client-token budgets retain
   their server-local boundaries. An environment override locks the dashboard
   control. Without an override, saved settings and YAML are considered before
   UTC; an unresolvable effective name stops boot.
2. **`EZLLM_DASHBOARD_PASSWORD_FILE`** (or `EZLLM_DASHBOARD_PASSWORD`) — the
   first dashboard password, at least 12 characters. Skip it and the first boot
   prints a one-time setup code to `docker logs ezllm` instead.
3. **The subscription bridges stay off** (`EZLLM_CLAUDE_BRIDGE`,
   `EZLLM_CODEX_BRIDGE`, `EZLLM_AGY_BRIDGE`) unless you have the vendor CLI
   and a subscription login on the host; turned on without them, the boot
   fails. To use one, follow [claude-bridge-runbook.md](claude-bridge-runbook.md),
   [codex-bridge-runbook.md](codex-bridge-runbook.md) or
   [agy-bridge-runbook.md](agy-bridge-runbook.md).

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
| `EZLLM_USAGE_TZ` | saved setting → YAML `usage_timezone` → UTC | Deployment override for report days, inherited model-rule windows/caps and daily schedules; locks the dashboard control. An unresolvable name fails boot. Report `?tz=` overrides the default grouping timezone. |
| `EZLLM_LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error`. Env-only (needed before the config is read); an unknown value logs a warning and uses `info`. The effective value is stated at boot. |
| `EZLLM_LOG_COLOR` | `auto` | `auto` (color only on a terminal) \| `always` \| `never`. |
| `EZLLM_LOG_FORMAT` | `console` | `console` \| `json`. JSON emits one object per line for a collector; the console format is for `docker logs`. See [logging.md](logging.md). |
| `NO_COLOR` | unset | Any value disables color under `auto`. |
| `EZLLM_TRUSTED_PROXIES` | unset | Comma-separated IPs/CIDRs of reverse proxies in front of ezllm. The sign-in/setup rate limiter reads `X-Forwarded-For` only from these; unset, it keys on the socket peer. Set it behind any proxy, or every client shares the proxy's limit. |
| `EZLLM_WEB_DIR` | unset (embedded `web/dist`) | Serve the dashboard from a directory of built assets instead, to iterate on the UI without a Go rebuild. |
| `TZ` | image: `Asia/Kuala_Lumpur` | Server-local clock used by log timestamps and existing client-token budget boundaries. Report defaults, inherited model rules and schedules use the effective usage timezone. |

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
| `EZLLM_TRANSLATION_ENABLED` | saved setting → YAML → on | The one switch for wire-protocol translation (pairwise translators and the universal matrix). `false` turns it off without a rebuild. Non-boolean values fail the boot. See [translation-runbook.md](translation-runbook.md). |
| `EZLLM_TRANSLATION_UNIVERSAL_ENABLED` | unset | **Deprecated** (merged into `EZLLM_TRANSLATION_ENABLED`). Still honored when the new variable is unset, with a warning at boot. |
| `EZLLM_COMBO_AUTOPROBE` | saved setting → YAML `automatic_probing` → on | An explicit value locks the dashboard control. `off` disables automatic combo-hop and free-model probes; manual probes remain available. |
| `EZLLM_PRICE_REFRESH` | saved setting → YAML `price_refresh` → on | An explicit value locks the dashboard control. `off` stops scheduled price refresh (boot + daily); manual refresh remains available. See [pricing-runbook.md](pricing-runbook.md). |

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

## Antigravity (Google subscription) bridge

Setup and the login that must avoid the desktop keyring: [agy-bridge-runbook.md](agy-bridge-runbook.md). Linux only.

| Variable | Default | What it does |
|---|---|---|
| `EZLLM_AGY_BRIDGE` | off | `on` starts the loopback surface. Enabled without a usable CLI, a token file or a working `agy models`, the boot fails. |
| `EZLLM_AGY_BIN` | `/opt/agy/agy` | Official Antigravity CLI binary inside the container (compose mounts a pinned copy here). |
| `EZLLM_AGY_HOME` | `/agy-sub` | `HOME` for the CLI: holds `.gemini/antigravity-cli/antigravity-oauth-token` (compose mounts the login here, read-write). |
| `EZLLM_AGY_BRIDGE_ADDR` | `127.0.0.1:8792` | Bridge listen address; non-loopback addresses are refused. |
| `EZLLM_AGY_BRIDGE_TOKEN_FILE` | `/data/agy-bridge.token` | Shared secret for the account key; generated (0600) when missing. |
| `EZLLM_AGY_BRIDGE_CONCURRENCY` | `2` | Concurrent `agy` children (~207 MB each); a positive integer, anything else fails the boot. |

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
| `EZLLM_CODEX_LIVE` | `1` runs the Codex bridge live tests against the real CLI and login (`go test -short ./internal/codexbridge/ -run Live -v`). |
| `EZLLM_CODEX_LIVE_BIN`, `EZLLM_CODEX_LIVE_HOME` | The CLI and `CODEX_HOME` those live tests use; default `codex` on `PATH` and `~/.codex-sub`. |
| `EZLLM_AGY_LIVE` | `1` runs the Antigravity bridge live tests against the real CLI and login (`go test -short ./internal/agybridge/ -run Live -v`); a turn spends plan quota. |
| `EZLLM_AGY_BIN`, `EZLLM_AGY_HOME`, `EZLLM_AGY_MODEL` | The CLI, login directory and model those live tests use. On the host, unlike the container defaults above, they default to `~/.local/share/agy-bridge/1.4.7/agy`, `~/.agy-sub` and `gemini-3.8-flash-low`. |
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
