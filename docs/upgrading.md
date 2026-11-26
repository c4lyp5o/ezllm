# Upgrading from an older version

The rest of the docs describe only the version they ship with. What you must change
when coming from an older one is here; start at the version you run now and read down.
Back up first: [recovery and upgrades](recovery-runbook.md).

## From before 2.0.0

- **Master key.** It moved from `~/.hermes/secrets/ezllm-master.key` (or
  `<data_dir>/.hermes/secrets/…`) to `<data_dir>/ezllm-master.key`. ezllm refuses to
  start if it finds a key only at the old path. Move the file, keeping mode 0600, or set
  `EZLLM_MASTER_KEY_FILE` to the old path.
- **Copilot login.** The folder is now `/copilot-sub`, with no fallback. Sign in as
  [Turn on GitHub Copilot](bridges/copilot.md) says, mount `~/.copilot-sub:/copilot-sub`
  and recreate the container.
- **Bridge CLI paths.** Every `EZLLM_*_BIN` default is now
  `/opt/<bridge>-bridge/current/…`. If you mount a single binary, set its `EZLLM_*_BIN`,
  or redo the bridge with [its page](bridge-setup.md).
- **Bridges run one CLI process at a time** by default (was two). Raise
  `EZLLM_*_BRIDGE_CONCURRENCY` if you need more.
- **Timezone.** `EZLLM_USAGE_TZ` is the only timezone setting; `TZ` is ignored.
- **Data folder.** `compose.example.yml` no longer reads `EZLLM_DATA`: write the path in
  `volumes:`.
- **Caches and the Copilot warm pool are on** by default; `off` turns each one off.

## From before 2.1.0

- An admin key no longer calls models unless it has the `infer` role. The upgrade adds
  that role to existing admin keys, which could call models before.

## From before 2.2.0

- Every bridge has a wait queue of 8 (`EZLLM_BRIDGE_QUEUE`); past it the bridge answers
  429 and the router fails over.

## From 2.2.0

- **Copilot CLI.** 2.2.0 and older speak headless protocol 3 only: stay on Copilot CLI
  1.0.112 until you have upgraded ezllm.
- **Antigravity caller tools** are always on when the CLI supports them, and so is the
  conversation cache.
- **Rolling back** past the release check: Reset `update_check` and
  `notify_update_available` in Settings first; older builds refuse settings they do not
  know. The same goes for the early-warning settings (`notify_quota_warning`,
  `alert_quota_percent`, `notify_balance_low`, `alert_balance_below`,
  `notify_cookie_expiring`, `alert_cookie_hours`) and `ledger_retention_days`.
- **Database schema 39** adds the daily totals that ledger retention writes. Back up
  before you upgrade: an older ezllm refuses to open a schema 39 database, so going back
  means starting the old version with that backup.

## Settings that no longer exist

ezllm refuses to start while one of these is set, and names the replacement.

| Old setting | Use instead |
|---|---|
| `EZLLM_CLAUDE_CONFIG_DIR` | `EZLLM_CLAUDE_HOME` |
| `EZLLM_COPILOT_BRIDGE_SESSION_CACHE` | `EZLLM_COPILOT_BRIDGE_CACHE` |
| `EZLLM_AGY_BRIDGE_SESSION_CACHE` | `EZLLM_AGY_BRIDGE_CACHE` |
| `EZLLM_COPILOT_BRIDGE_QUEUE` | `EZLLM_BRIDGE_QUEUE` |
| `EZLLM_TRANSLATION_UNIVERSAL_ENABLED` | `EZLLM_TRANSLATION_ENABLED` |
| `EZLLM_CODEX_BRIDGE_CACHE=auto` | remove it |
| `EZLLM_CODEX_SANDBOX` | remove it: the sandbox is always writable |
| `EZLLM_CODEX_TOOLS_WITHOUT_LANDLOCK` | remove it: client tools need Landlock |
| `EZLLM_AGY_EXPERIMENTAL_TOOLS` | remove it: caller tools are always on |
| `EZLLM_WEB_DIR` | remove it: the dashboard is built in |

One old `config.yaml` key still works but logs a warning at start:
`translation.universal_enabled`. Use `translation.enabled` instead; an old
`universal_enabled: false` still turns translation off.

## A database with the old Copilot account

A database from before the Copilot bridge can hold a `github-copilot` account. ezllm
starts and shows it enabled, but **every request through it fails** with
`502 provider: unknown kind "github-copilot"`.

1. [Turn on GitHub Copilot](bridges/copilot.md), including the dashboard step.
2. Point every combo that named the old account at the new one.
3. Disable or delete the old account.
4. Send one request through the new one and check it in Requests.

## ezllm refuses to start: `combos rebuild: both current and staged tables exist`

An upgrade of a very old database stopped halfway, and both copies of the combos table are
left. ezllm will not pick one, because the staged copy can hold the only full set of
combos.

1. Stop ezllm and keep a copy of the whole `data` folder.
2. On a copy of the database, compare the tables `combos` and `combos_v9`, and keep the
   one with your combos.
3. Start ezllm again.
