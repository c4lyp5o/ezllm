# Bridge CLI updates — operator runbook

`scripts/bridge_update.py` maintains pinned official CLI releases for Claude,
Codex, Antigravity and optionally Copilot. It uses the existing host timer and
stdlib Python; no new daemon, sidecar or dashboard write endpoint is required.
Design history: `SPEC-bridge-update.md`.

## Policy

| Control | Default | Behavior |
|---|---|---|
| `window` | `02:00-05:00` | Host-local promotion window; checked again after gating |
| `soak_days` | `3` | From publication, or first sighting when a channel has no date |
| `retry_hours` | `6` | Backoff after operational failures, rollback or a stage/gate exception |
| `keep` | `3` | Newest version folders retained; current and previous remain protected; minimum 2 |
| `bridges.NAME.enabled` | true for Claude/Codex/agy; false for Copilot | Explicit enrollment |
| `bridges.NAME.hold` | empty | Versions excluded until the operator removes the hold |
| `bridges.NAME.model` | bridge gate default | Optional model override for the plain live check |
| `bridges.NAME.tool_model` | bridge gate default | Optional model override for caller-tool checks |

The existing twelve-hour minimum between successful promotions per bridge is
retained. Gates and ordinary restarts run only inside the window unless a
supervised run uses `--force-window`. **Rollback/recovery is the exception:**
it can restart outside the window solely to restore a journalled previous
version. A dry run never promotes, rolls back or restarts; an unfinished
transaction makes it exit 2 instead of attempting recovery.

Quota, authentication and transport failures do not establish CLI
incompatibility. They defer the candidate with backoff. Known operational
blocks left by older updater versions migrate to backoff automatically.
Compatibility failures remain blocked until explicitly cleared or superseded;
operator holds are never removed automatically.

## How a release moves

1. Discover official releases and record first sightings on every run. Status
   keeps the newest pending version even when an older release is still current.
2. Inside the window, download a ready candidate, verify its published digest,
   unpack the complete release, and make its version directory read-only.
3. Run `bridge-check --live --json` in a throwaway container using the running
   production **image ID**, user, network, actual login mount and allowlisted
   environment. Environment values travel in a private temporary file rather
   than process arguments. A unique container name permits cleanup even if the
   Docker client times out. The shared production login remains read-write.
4. Require a matching bridge/version, successful process exit, live-tier verdict
   and configured policy. Recheck the window and production container identity,
   image, environment, network and mounts before changing any symlink.
5. Persist and fsync a recovery journal in the existing updater state file.
   Swap the batch's relative `current` symlinks, then restart ezllm once with a
   30-second graceful stop and wait for health.
6. Prove every promoted bridge in the running container, followed by its
   gateway proof script when both the script and proof token exist. Only after
   every proof passes are qualification records and promotion times committed.
7. If any proof fails or raises, restore the **whole batch**, restart once and
   statically check every restored bridge. Do not mark successful siblings
   updated while another member of the batch is rolling back.
8. Clear the recovery journal only after a successful commit or verified
   rollback. An interrupted run recovers before discovery on its next invocation
   and does not attempt another promotion in that run. An incomplete rollback
   retains the journal and exits 2. Recovery refuses to restart a container that
   has been replaced since the transaction began.

Version folders used by current or previous releases are not pruned. A failure
while writing metadata after a symlink change is handled by the same rollback
path. If recovery cannot complete, investigate before deleting the journal.

## Channels and layout

| Bridge | Official channel | Verification | Version directory |
|---|---|---|---|
| Claude | `downloads.claude.ai/claude-code-releases/stable` or `latest` | manifest SHA-256 | `<version>/claude` |
| Codex | stable `rust-vX.Y.Z` releases of `openai/codex` | GitHub asset SHA-256 | complete package, including `bin/codex` and code-mode host |
| Antigravity | official Linux amd64 updater manifest | manifest SHA-512 | `<version>/agy` |
| Copilot | stable `vX.Y.Z` releases of `github/copilot-cli` | GitHub asset SHA-256 | complete `copilot-linux-x64.tar.gz` release, including `copilot` |

Prereleases, drafts and assets without the required digest are not candidates.
The Copilot asset and checksum publication were checked against the
[official release channel](https://github.com/github/copilot-cli/releases) on
2026-11-14. This updater's release selection is Linux x86-64-specific.

Each host root is `~/.local/share/NAME-bridge`, mounted read-only at
`/opt/NAME-bridge`, with a relative `current -> VERSION` symlink,
`qualified.json`, and `update-status.json`. Login directories remain separate
and read-write. `qualified.json` records successful live qualification, including
the tested policy; qualification is not a promise about every model or feature.
Changing policy or models warrants a fresh supervised check.

## What the live gates prove

| Bridge | Checks |
|---|---|
| Claude | Plain answer with usage; caller tool selection |
| Codex | Plain answer; caller tool selection and correlated result continuation with usage, where tools are enabled; configured `EZLLM_CODEX_SANDBOX` |
| Antigravity | Plain answer; with `EZLLM_AGY_EXPERIMENTAL_TOOLS=on`, schema capability, caller selection and stateless result replay; with `EZLLM_AGY_BRIDGE_SESSION_CACHE=on`, an additional exact caller-result conversation-resume proof |
| Copilot | Plain answer, caller-tool batch and result replay, with configured `EZLLM_COPILOT_BRIDGE_WARM_POOL`; exact completed-session continuation when `EZLLM_COPILOT_BRIDGE_SESSION_CACHE=on` |

Gate verdicts carry `policy` and `retryable` fields. An older binary that ignores
a configured policy cannot qualify that policy: deploy the updated ezllm image
before relying on these gates. `gate_binary` can mount a host-built ezllm for
pre-deployment dry runs, but production must also provide the matching gate for
a real post-promotion proof.

These checks consume subscription quota and are deliberately small. They do not
replace model-specific image, long-context, compression or harness acceptance
proofs. No caller filesystem tool is executed by a tool-loop check; it supplies
a synthetic result. Answers, tool arguments and result values are not printed.

## Install or migrate

1. Deploy an ezllm build with the revised gates (v1.9.0 or later). Keep the
   current CLI versions while doing so.
2. Initialize any missing roots from the running container's legacy mounts:

   ```sh
   python3 scripts/bridge_update.py --init
   python3 scripts/bridge_update.py --init --bridge copilot
   ```

   Initialization copies the legacy release without replacing the original
   binary or changing production's mount. For a standalone Copilot mount,
   the CLI version is read with `--no-auto-update --version` inside production.
3. Set the binary paths and mount the roots in the host-owned `compose.yml`:

   ```yaml
   environment:
     EZLLM_CLAUDE_BIN: /opt/claude-bridge/current/claude
     EZLLM_CODEX_BIN: /opt/codex-bridge/current/bin/codex
     EZLLM_AGY_BIN: /opt/agy-bridge/current/agy
     EZLLM_COPILOT_BIN: /opt/copilot-bridge/current/copilot
   volumes:
     - /home/you/.local/share/claude-bridge:/opt/claude-bridge:ro
     - /home/you/.local/share/codex-bridge:/opt/codex-bridge:ro
     - /home/you/.local/share/agy-bridge:/opt/agy-bridge:ro
     - /home/you/.local/share/copilot-bridge:/opt/copilot-bridge:ro
   ```

   Keep the login mounts and their existing bridge home settings. Recreate the
   service after editing mounts. Promotion is refused until the corresponding
   root is mounted at its managed location.
4. Copy `scripts/bridge-update.example.json` to
   `~/.config/ezllm/bridge-update.json` and adjust it. Copilot is disabled by
   default; set `bridges.copilot.enabled` true after its mount migration.
   Optional `model` and `tool_model` values should name models your plan serves.
5. For gateway proofs, store a suitably scoped client token in
   `~/.config/ezllm/proof.token` with mode 0600. Without it the in-container live
   checks are the proof; the status says the gateway script was skipped.
6. Install or refresh the existing user unit and timer:

   ```sh
   mkdir -p ~/.config/systemd/user
   cp scripts/systemd/ezllm-bridge-update.{service,timer} ~/.config/systemd/user/
   systemctl --user daemon-reload
   systemctl --user enable --now ezllm-bridge-update.timer
   loginctl enable-linger "$USER"
   ```

   The service timeout is 90 minutes for up to four gates and post-promotion
   proofs. An installed older unit keeps its old timeout until refreshed.

## Supervised checks and recovery

```sh
python3 scripts/bridge_update.py --dry-run --force-window
python3 scripts/bridge_update.py --bridge copilot --dry-run --force-window
journalctl --user -u ezllm-bridge-update -n 50
cat ~/.local/share/codex-bridge/update-status.json
```

A non-dry `--force-window` permits promotion/restart now. It does not bypass
soak, compatibility blocks, holds or operational backoff. Do not use it to force
an unqualified release into production.

- Hold a release in `bridges.NAME.hold`.
- After fixing a compatibility failure, remove that version's entry from
  `blocked` in `~/.local/state/ezllm/bridge-update.json`.
- Operational retries use the state's per-version `retry_after` timestamp.
- An unfinished transaction is in `pending`, tied to `pending_container`.
  Keep it until recovery succeeds. If production was deliberately replaced,
  reconcile its mounts and versions before clearing stale recovery state.
- Manual rollback: point the relative `current` link at the previous version,
  then `docker restart -t 30 ezllm` and run the static/live checks.
- `fallback_ntfy_url`, when configured, is used only if automatic recovery cannot
  finish. Otherwise the dashboard and existing notification channels carry
  update outcomes. No new notification credential or delivery service is added.

## Dashboard and tests

The Providers bridge card and Settings → System read `update-status.json` once a
minute on the managed-root layout. Updated, compatibility-blocked and rolled-back
reports use the existing bell kinds and notification toggles. Operational waiting
and errors appear on the card. Boot status remains a boot snapshot.

```sh
python3 -m unittest scripts/bridge_update_test.py
make test-short
```

The Python tests stub Docker/network and cover discovery, checksums, all release
formats, policy validation, environment isolation, promotion transactions,
timeouts, backoff and interrupted recovery. Go tests exercise the gates and tool
replay helpers without real subscription calls. Live checks are explicit and
consume quota.

### Supervised gate validation — 2026-11-14

The rebuilt gate was run directly against existing CLI copies and logins:

| CLI | Version | Exercised policy | Result |
|---|---|---|---|
| Claude | 2.1.311 | Plain turn and caller tool selection | passed |
| Codex | 0.167.0 | `workspace-write`, plain turn, caller call and continuation | passed |
| Antigravity | 1.4.12 | Structured-output caller tools, plain turn and result replay | passed |
| Copilot | 1.0.105 | Warm pool on, plain turn, caller tools and replay | passed |

These runs did not promote releases, change production mounts or install units.
Promotion/rollback failure paths were tested with Docker/network stubbed, not
through deliberate production outages. The checked-out host script is what the
installed timer invokes; it will use source changes on subsequent runs. The
running ezllm image must be refreshed to supply the new policy-aware gate, and
Copilot remains unenrolled until its root mount and updater config are migrated.
