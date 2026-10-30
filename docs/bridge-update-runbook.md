# Bridge CLI updates — operator runbook

The Claude, Codex and Antigravity bridges each run a pinned copy of an official
CLI. `scripts/bridge_update.py` keeps those copies current with no routine
operator action: it finds a release on the CLI's official channel, waits for it
to soak, gates it, promotes it inside a late-night window, proves it in the
running container and rolls it back on failure. Design and decisions:
`SPEC-bridge-update.md`.

## How a release moves

| Step | When | What |
|---|---|---|
| discover | every run | newest releases on the channel; first sightings recorded |
| soak | — | a release waits `soak_days` (3) from publication — from first sighting for agy, whose channel publishes no date |
| stage | window | download, verify the published digest, unpack to `<root>/<version>/`, make it read-only |
| gate | window | `ezllm bridge-check NAME --live` in a throwaway container: production's image, user and bridge env, the staged CLI read-only, the shared login |
| promote | window | swap `<root>/current`, restart ezllm once (graceful, `-t 30`) |
| prove | window | `bridge-check --live` inside the running container, then `tests/prove_NAME_bridge.py` through the gateway where one exists |
| roll back | on a failed proof | swap `current` back, restart, re-check |

At most one promotion per bridge per night; a failed release is blocked until a
newer one appears. Restarts happen only inside the window.

Channels (measured 2026-10-30):

| Bridge | Version | Digest | Payload |
|---|---|---|---|
| claude | `downloads.claude.ai/claude-code-releases/stable` (or `latest`) | sha256, `<version>/manifest.json` | `<version>/linux-x64/claude` |
| codex | GitHub releases of `openai/codex`, newest `rust-vX.Y.Z` that is not a prerelease | sha256, the asset's `digest` | `codex-package-x86_64-unknown-linux-musl.tar.gz` (the whole release folder) |
| agy | `antigravity-cli-auto-updater…/manifests/linux_amd64.json` | sha512, the manifest | `cli_linux_x64.tar.gz`, member `antigravity` |

## Layout

```
~/.local/share/claude-bridge/       mounted read-only at /opt/claude-bridge
    2.1.306/claude
    2.1.308/claude
    current -> 2.1.308               relative: resolves inside the container
    qualified.json                   versions the updater proved
    update-status.json               last run, read by ezllm
```

Codex keeps the release folder (`<version>/bin/codex` and
`bin/codex-code-mode-host`); agy keeps `<version>/agy`. Version folders are
read-only: agy rewrites its own binary in place when run from a writable folder.

## Install

1. **Create the layout** from what production mounts today. This copies the
   Claude binary into `2.1.306/claude`, points each `current` at the running
   version and touches nothing production uses:

   ```sh
   python3 scripts/bridge_update.py --init
   ```

2. **Migrate the compose mounts** (`compose.yml` is host-owned). Replace the
   three version-pinned CLI mounts with the bridge roots, set the binaries to
   the `current` paths, and recreate the container:

   ```yaml
   environment:
     EZLLM_CLAUDE_BIN: /opt/claude-bridge/current/claude
     EZLLM_CODEX_BIN: /opt/codex-bridge/current/bin/codex
     EZLLM_AGY_BIN: /opt/agy-bridge/current/agy
   volumes:
     - /home/you/.local/share/claude-bridge:/opt/claude-bridge:ro
     - /home/you/.local/share/codex-bridge:/opt/codex-bridge:ro
     - /home/you/.local/share/agy-bridge:/opt/agy-bridge:ro
   ```

   Keep the login mounts (`/claude-sub`, `/codex-sub`, `/agy-sub`) as they are.
   Until this is done the updater gates releases but refuses to promote them.

3. **Configure** (optional — every key has a default):
   `cp scripts/bridge-update.example.json ~/.config/ezllm/bridge-update.json`.
   For the gateway proof, store a client token that can reach the bridge combos
   in `~/.config/ezllm/proof.token` (mode 0600); without it the in-container
   live check is the proof.

4. **Install the timer** as a user unit, and keep it running while logged out:

   ```sh
   mkdir -p ~/.config/systemd/user
   cp scripts/systemd/ezllm-bridge-update.{service,timer} ~/.config/systemd/user/
   systemctl --user daemon-reload
   systemctl --user enable --now ezllm-bridge-update.timer
   loginctl enable-linger "$USER"
   ```

Production must run ezllm 1.6.0 or later: the updater asks `ezllm help` for
`bridge-check` and refuses to promote without it. An ezllm that predates the
subcommand would take it as a stray argument and start a second router.

## On the dashboard

ezllm reads each bridge's `update-status.json` once a minute (from the mounted
bridge root, so only on the migrated layout):

- **Providers → Add provider → a bridge preset** shows *CLI updates*: what the
  updater last did, its reason and when it checked.
- **Settings → System** shows the same verdict per bridge.
- **The bell** rings once per report for a promotion (`bridge_updated`, info),
  a held-back release (`bridge_update_blocked`, warn) and a rollback
  (`bridge_rolled_back`, error). Each has its toggle under Settings →
  Notifications. Reports written before ezllm started are shown, not rung.

## Supervised runs

```sh
python3 scripts/bridge_update.py --dry-run --force-window            # stage + gate, never promote
python3 scripts/bridge_update.py --bridge agy --force-window         # one bridge, now
journalctl --user -u ezllm-bridge-update -n 50                        # last runs
cat ~/.local/share/agy-bridge/update-status.json
```

`gate_binary` in the config mounts a host-built ezllm into the gate container
in place of the image's own — for gating before 1.6.0 is deployed.

## Day-2

- **Hold a release:** add it to the bridge's `hold` list in the config.
- **Unblock a release** after fixing its cause: remove it from `blocked` in
  `~/.local/state/ezllm/bridge-update.json`.
- **Roll back by hand:** `ln -sfn VERSION ~/.local/share/NAME-bridge/current`
  (the link must stay relative), then `docker restart -t 30 ezllm`.
- **Restage a version:** `chmod -R u+w` its folder, remove it, run again.
- **ezllm down after a rollback:** the run exits 2 and posts to
  `fallback_ntfy_url` when one is set — the only channel the host holds.

## Tests

`python3 -m unittest scripts/bridge_update_test.py` — staging against local
payloads for all three formats, the soak and window rules, and the promotion
state machine (gate refusal, promotion, rollback) with docker stubbed.
