# SPEC — bridge CLI auto-update

Status: draft for operator review (2026-10-30). No production code until the
operator confirms this surface.

## 1. Goal

Keep the three subscription bridges — `claude-bridge`, `codex-bridge`,
`agy-bridge` — on current official CLI releases with no routine operator
action. An operator acts only when a release fails qualification or a promotion
is rolled back, and is told so on the dashboard and through an external
notification channel.

## 2. Facts this design rests on

- Every bridge spawns its CLI **once per request** (`claudebridge/runner.go`
  `Runner`, `codexbridge/runner.go` `Runner`, `agybridge/runner.go`). A new
  binary is used from the next request; only the boot checks
  (`claudeCLICheck`/`claudeLoginCheck`, `codexbridge.RunPreflight`,
  `agybridge.RunPreflight`) need a restart to re-run.
- Today each mount names a version (`compose.example.yml`), so a bump is a
  compose edit plus a recreate.
- `codexbridge.QualifiedVersion` and `agybridge.QualifiedVersion` are compile-time
  constants; any other version boots as `unqualified`. Claude has no qualified
  notion.
- The CLIs' own updaters must stay off for the running bridge: Claude's deletes
  old versions (2.1.299 lost 2026-10-21), agy updates in place, Codex runs with
  `--disable in_app_updates`.
- `compose.yml` is host-owned and gitignored.
- The bell is `store.Notify` / `GET /admin/notifications`; per-kind toggles live
  in `settings.Values` (`notify_*`). There is no external delivery today.

## 3. Decisions (operator, 2026-10-30)

1. agy is included alongside Claude and Codex.
2. Outcomes reach the dashboard bell **and** configurable external channels in
   Settings → Notifications, ntfy among them.
3. Restarts happen only inside a late-night window (default 02:00–05:00, host
   local time).
4. The pre-promotion live check uses the **shared** production login
   (`~/.claude-sub`, `~/.codex-sub`, `~/.agy-sub`). A separate canary login is the
   fallback if refresh contention shows up.

## 4. Design

### 4.1 Stable mount, `current` symlink

```
~/.local/share/claude-bridge/        → /opt/claude-bridge:ro
    2.1.306/claude
    2.1.312/claude
    current -> 2.1.312               (relative, resolves inside the container)
    qualified.json
    update-status.json
EZLLM_CLAUDE_BIN=/opt/claude-bridge/current/claude
```

Codex: `~/.local/share/codex-bridge/<ver>/` holds the whole release directory
(`bin/codex` + `bin/codex-code-mode-host`); `EZLLM_CODEX_BIN=/opt/codex-bridge/current/bin/codex`.
agy: `~/.local/share/agy-bridge/<ver>/agy`; `EZLLM_AGY_BIN=/opt/agy-bridge/current/agy`.
Promote and rollback are one atomic symlink swap (`ln -s` to a temp name, then
`rename`). The last three versions are kept; `current` and the previous version
are never pruned.

No Go change is needed for the layout — the bins are already env paths. It is a
`compose.example.yml` and runbook change, plus a one-time host edit of the live
`compose.yml` by the operator.

### 4.2 `ezllm bridge-check` — one gate for boot and updater

A subcommand of the existing binary:

```
ezllm bridge-check {claude|codex|agy} --bin PATH --home PATH [--live] [--json]
```

- **Static** (default): runs exactly the boot checks — version, flag surface,
  login state for Claude; `RunPreflight` (required methods, lockdown, channel)
  for Codex and agy. Unlike boot, **any** drift is a non-zero exit: boot reports
  flag drift and keeps serving, the updater refuses to promote on it.
- **`--live`**: additionally drives the bridge `Runner` in-process (no HTTP, no
  gateway, no ledger) through one plain turn and, for Claude and Codex, one
  client-tool turn — the two paths most likely to break across CLI releases.
  Smallest model per bridge; output is asserted on shape and usage presence,
  never logged.
- `--json` prints the verdict for the updater. No credential, body or prompt is
  ever printed.

Boot calls the same functions, so the updater cannot qualify something boot
would reject.

### 4.3 Runtime qualification record

`qualified.json` in each bridge directory, written by the updater after a
version passes the post-promotion proof:

```json
{"versions": {"2.1.312": {"qualified_at": "2026-10-31T03:12:40Z", "check": "live", "proof": "prove_claude_bridge.py 7/7"}}}
```

Boot reads it from the mount: the running version is `qualified` if it is the
compiled constant **or** listed there. The constants stay as the floor. Claude
gains the same `qualified` field in its bridge `Info` and boot log line.

### 4.4 Host updater

`scripts/bridge_update.py`, stdlib only, with `scripts/systemd/ezllm-bridge-update.{service,timer}`.
The timer fires hourly; the script exits immediately outside the window.
Host config `~/.config/ezllm/bridge-update.json`:

```json
{"window": "02:00-05:00", "soak_days": 3, "keep": 3,
 "bridges": {"claude": {"enabled": true, "hold": []},
             "codex":  {"enabled": true, "hold": []},
             "agy":    {"enabled": true, "hold": []}},
 "compose_dir": "/home/calypso/ezllm", "service": "ezllm",
 "proof_token_file": "~/.config/ezllm/proof.token"}
```

One run, per enabled bridge, at most one promotion per bridge per night:

1. **Discover** the newest release through the official channel; skip
   prereleases, anything in `hold`, and anything published less than
   `soak_days` ago. M2 measures each channel before code depends on it:
   Codex — GitHub releases for `openai/codex`, standalone musl asset; Claude —
   the official installer run into a scratch `HOME`; agy — the official
   SHA-512-verified installer run into a scratch `HOME`. The CLIs' own updaters
   never touch the bridge directories.
2. **Stage** into `<bridge-dir>/<ver>/` (the whole release dir for Codex);
   verify the checksum or signature the channel publishes; refuse on mismatch;
   then `chmod a-w` the version folder before anything runs it. agy rewrites its
   own binary in place when run from a writable folder (measured 2026-10-30:
   one host `--version` turned the pinned 1.4.7 into 1.4.11, the original
   renamed `agy.<n>.old`), and the updater never runs a CLI from a writable path.
3. **Gate:** `docker run --rm` of the **running** ezllm image with the staged
   version and the shared login mounted, executing
   `ezllm bridge-check <bridge> --live --json`. Failure → `blocked`, staged copy
   kept for inspection, no promotion, no retry of that version until it is
   deleted or a newer one appears.
4. **Promote:** swap `current`, then `docker compose restart <service>` (the
   existing graceful shutdown drains in-flight requests). Wait for `/healthz`.
5. **Prove:** run `tests/prove_<bridge>_bridge.py` against production with the
   dedicated proof client token. Pass → record in `qualified.json`, status
   `updated`. Fail → swap `current` back, restart, re-check `/healthz`, status
   `rolled_back`.
6. **Report:** write `update-status.json` in the bridge directory (atomic
   write), log one line to the journal, prune to `keep`.

Bridges run sequentially so one restart window never stacks three restarts
without a check between them; when two bridges both have a candidate they share
one restart after both gates pass, and the proof runs per bridge.

`--dry-run` performs discover, stage and gate only. `--bridge NAME` and
`--force-window` exist for a supervised first run.

### 4.5 Status into ezllm

ezllm reads `update-status.json` from each mounted bridge directory once a
minute (and at boot) — no new credential, no inbound endpoint:

```json
{"bridge": "codex", "checked_at": "…", "current": "0.170.0", "previous": "0.163.0",
 "result": "updated|blocked|rolled_back|up_to_date|error",
 "candidate": "0.170.0", "detail": "flag drift: --disable unified_exec"}
```

New bell kinds, dedup key `bridge:<name>:<version>:<result>`:

| `kind` | `severity` | Raised when |
|---|---|---|
| `bridge_updated` | info | a new CLI was promoted and proved |
| `bridge_update_blocked` | warn | a release failed the gate; production unchanged |
| `bridge_rolled_back` | error | a promoted release failed the proof and was reverted |

Each kind gets its `notify_*` toggle (default on). The Providers bridge card and
Settings → System show per bridge: running version, qualified state, last check,
pending or blocked candidate with its reason.

Gap: if ezllm itself fails to come back after a rollback, nothing inside ezllm
can report it. The updater then exits non-zero and the unit's `OnFailure=` posts
to an optional `fallback_ntfy_url` in the host config. That is the only channel
the host holds.

### 4.6 External notification channels (Settings → Notifications)

Delivery for **all** bell events, not only bridge ones; the existing per-kind
toggles still decide whether an event exists.

Channel types:

| Type | Fields |
|---|---|
| ntfy | server URL (default `https://ntfy.sh`), topic, optional access token |
| Discord | webhook URL |
| Telegram | bot token, chat id |
| Webhook | URL, optional bearer token; JSON `{kind, severity, title, body, created_at}` |

Per channel: enabled, minimum severity (`info`/`warn`/`error`, default `warn`),
**Send test**. Secrets (ntfy token, Discord URL, Telegram token, webhook bearer)
are encrypted with the master key like provider credentials and never returned
by the API; the editor shows them as set/unset.

Delivery: a new bell row is sent once; a folded repeat (`count` + 1) is not
re-sent within 24 h. Sending is asynchronous with a 10 s timeout and three
backoff retries; failures are logged by channel and reason only and surface as
a channel status, never as a bell event (no notification loops). Payloads carry
title and body as the bell shows them — no credentials, bodies or prompts.

Storage: a `notification_channels` table → `schema_version` bump with the
matching docs/API.md entries (`GET/PUT /admin/notification-channels`,
`POST /admin/notification-channels/{id}/test`).

## 5. Risks

- **Shared-login refresh contention.** The gate container and production use one
  login dir. Codex refresh tokens rotate; a simultaneous refresh can log one side
  out. Mitigation: the gate is short and sequential, and runs while production is
  quiet. Detection: post-gate `login status` on production; if it ever fails,
  switch to a canary login (decision 4 fallback).
- **Codex lockdown regressions.** A release could re-enable shell execution or a
  disabled feature. M1 adds a test proving `RunPreflight` fails when a lockdown
  flag stops being honoured, before any automation trusts it.
- **Subscription quota.** Two gated turns plus the proof per promotion — small,
  non-zero, at most once per bridge per night.
- **Proof token.** A dedicated client token scoped to the three bridge combos,
  held in a 0600 host file.
- **Window and DST.** The window is host local time; a skipped or repeated hour
  just shifts which hourly firing acts.

## 6. Build order

Each milestone ships with in-package tests, `make test-short`, and a version bump
(`internal/server/version.go` + `VERSION.md`).

- **M1 — gate and qualification (Go).** `ezllm bridge-check` static and `--live`
  for all three; `qualified.json` read at boot; Claude `qualified` field; lockdown
  regression test. Docs: the three runbooks gain the `current` layout.
- **M2 — updater (host).** Channel measurements recorded in the runbooks;
  `bridge_update.py`, systemd units, host config, `--dry-run`; exercised against a
  fake release directory, then a supervised `--force-window` run on the box.
- **M3 — status in ezllm.** `update-status.json` ingestion, three bell kinds and
  toggles, bridge card and System readout. `web/` rebuilt.
- **M4 — external channels.** Schema, API, encrypted secrets, delivery worker,
  Settings editor with Send test. `web/` rebuilt.
- **M5 — production.** Operator edits the live `compose.yml` to the stable mounts,
  installs the timer, first nightly run observed.

## 7. Out of scope

- Hot re-preflight without a restart.
- Pausing or holding updates from the dashboard (the mount is read-only; holds
  live in the host config).
- Email delivery.
