# Antigravity subscription runbook

Operating the Google Antigravity subscription surface (agy-bridge). Decisions
live in `antigravity-subscription-design.md`; this is the how.

**Status:** text chat and streaming, with usage, against the Antigravity CLI
1.4.7. Client tools, images and plan windows are not offered (see
[limits](#limits)). Linux only.

## Quick setup

Start with a working ezllm installation and dashboard ([quick start](../README.md#quick-start)).
This guide uses Docker Compose in a Linux environment and a separate Google login.
This bridge is text-only and cannot serve client tool calls or image input.
Validation and test requests use subscription quota. Each step links to details below.

1. **Install the official CLI on the host** and pin a copy of the binary
   ([prerequisites](#prerequisites)).
2. **Log in to a separate folder, with D-Bus unset**, so the login is a file the
   container can read, not an entry in your desktop keyring
   ([log in](#log-in)).
3. **Clear Google's eligibility check** for that account if the first prompt
   asks you to ([eligibility](#eligibility)).
4. **Turn it on** in `compose.yml`: uncomment the two Antigravity mounts, set
   `EZLLM_AGY_BRIDGE: "on"`, then `docker compose up -d` ([enable](#enable)).
5. **Add the account** in the dashboard: Providers → Add provider →
   **Google Antigravity** → Save & validate key. The dialog shows
   whether the bridge is running and which models the plan has; there is no key
   to paste. Cap it under Rules before agents use it.

## What it is

A **loopback-only OpenAI Chat Completions surface inside the ezllm process**
(`internal/agybridge`, bound to `127.0.0.1:8792`) that runs the **official
Antigravity CLI** once per request in headless mode (`agy --print=
--input-format stream-json --output-format stream-json`) against a personal
Google account (Free, Pro or Ultra). ezllm sees it as a plain
`openai-compatible` account. The CLI is the only thing that talks to Google —
ezllm never reads or replays the OAuth token.

The plan's model list is whatever `agy models` prints for the login; the bridge
reads it at boot and serves it at `/v1/models`. Measured on a personal plan
(1.4.7): `gemini-3.8-flash-high|medium|low`, `claude-sonnet-4-6`,
`claude-opus-4-6-thinking`, `gpt-oss-120b-medium`.

## Prerequisites

```bash
# official installer: SHA-512-verified download to ~/.local/bin/agy
curl -fsSL https://antigravity.google/cli/install.sh | bash
agy --version                                   # check this before choosing a path

# pin a copy outside the self-updating path; compose mounts THIS file
# Replace VERSION with the actual installed version; 1.4.7 is qualified.
VERSION=1.4.7
mkdir -p "$HOME/.local/share/agy-bridge/$VERSION"
cp ~/.local/bin/agy "$HOME/.local/share/agy-bridge/$VERSION/agy"
```

`agy` updates itself in place in the background. Mounting `~/.local/bin/agy`
would let an update change the CLI under a running bridge; the pinned copy is
mounted read-only, so the bridge only moves to a new release when you copy one
and change the mount. A release other than 1.4.7 still boots, and `/status`
reports it as `unqualified` until the live proof ([verify](#verify)) has been
re-run and recorded in `qualified.json`.

**Gate a release before promoting it.** `ezllm bridge-check agy` runs the boot
checks against a staged CLI and exits non-zero on any problem — stricter than
boot, which reports an empty model list and keeps serving. `--json` prints the verdict
(`ok`, `version`, `release`, `problems`) for the bridge updater
(`SPEC-bridge-update.md`). A version the updater proved is listed in
`qualified.json` in the bridge directory (next to the version folders, up to two
levels above the binary); boot then reports it qualified.

The binary is ~228 MB, dynamically linked against glibc; the ezllm image
(distroless base-debian12) carries glibc.

## Log in

```bash
mkdir -p ~/.agy-sub && chmod 700 ~/.agy-sub
env -u DBUS_SESSION_BUS_ADDRESS HOME=$HOME/.agy-sub ~/.local/share/agy-bridge/1.4.7/agy
```

If you used a version other than 1.4.7 above, replace `1.4.7` in the command
with that version. Run it in a real terminal (SSH is fine; it prints a URL and asks for the code).
Pick a theme, trust the folder, sign in with the Google account that has the
plan, send one prompt, then `/exit`.

- **D-Bus unset is load-bearing.** On a desktop host `agy` stores the token in
  the Secret Service keyring, which the container cannot reach. With no session
  bus it writes `~/.agy-sub/.gemini/antigravity-cli/antigravity-oauth-token`
  (0600) instead — the file the bridge checks at boot.
- **`HOME=~/.agy-sub`** keeps this login apart from any personal `agy` use, as
  `~/.claude-sub` and `~/.codex-sub` do for the other bridges.

## Eligibility

A fresh personal account can fail every turn with
`Eligibility check failed: Your current account is not eligible for Antigravity.
Verify your account to continue.` — on every model, while `agy models` still
works. The CLI prints a one-time accounts.google.com link; open it signed in to
the same account and complete the verification. The bridge maps this to
**403 `account_ineligible`** and withholds the link (it is a sign-in
continuation), so run the login command above again to see it.

If Google still refuses after verifying, the account itself is out of scope
(region, age, or a Workspace domain that turned Antigravity off); use another
personal account.

## Enable

In `compose.yml`, under the ezllm service, add these entries to the existing
`environment` and `volumes` sections. Replace `/home/you` and the version with
your actual host paths. The Compose service user must be able to execute the
CLI and read and write the login directory:

```yaml
    environment:
      EZLLM_AGY_BRIDGE: "on"
    volumes:
      - /home/you/.local/share/agy-bridge/1.4.7/agy:/opt/agy/agy:ro
      - /home/you/.agy-sub:/agy-sub
```

The login directory is read-write: `agy` refreshes its token and keeps state
there. `docker compose up -d`, then `docker logs ezllm` shows
`agy bridge listening addr=127.0.0.1:8792 cli=1.4.7 release=qualified models=6`.

Boot fails, by design, when the binary is missing, a required flag is gone, the
token file is absent, `settings.json` loosens the tool policy
([lockdown](#lockdown)), or `agy models` fails. Every message names its fix.
All variables: [environment.md](environment.md#antigravity-google-subscription-bridge).

## Lockdown

`agy` is a coding agent with its own tools (`run_command`, `write_to_file`, …)
and it runs **on the ezllm host**, next to `/data`. No 1.4.7 flag switches
those tools off, so the bridge holds them to what headless mode does by default
and refuses anything looser:

- **No `--dangerously-skip-permissions`.** In print mode a tool that needs
  approval is soft-denied: the turn continues without it.
- **Per-turn check.** The CLI's `init` event states its `permission_mode`; a turn
  that reports anything but `request-review` or `strict` is killed before the
  model answers (500 `lockdown`).
- **Boot check.** `~/.agy-sub/.gemini/antigravity-cli/settings.json` must not set
  `toolPermission` to anything else, `allowNonWorkspaceAccess: true`,
  `agentMode: "auto"`, or any `permissions.allow` rule. The `trustedWorkspaces`
  the first interactive run writes is fine.
- **Empty workspace.** Each turn runs in a fresh temp directory that is deleted
  afterwards, so workspace-scoped reads and writes (auto-allowed) find nothing
  and leave nothing.
- **Allowlisted environment.** The child gets `PATH`, locale, CA and proxy
  variables and `HOME` — never `EZLLM_MASTER_KEY`, provider keys,
  `GEMINI_API_KEY`, `AGY_ADC_AUTH` or the D-Bus address.
- **`--disable-slash-commands`.** A prompt that starts with `/` stays a prompt.

The agent's tool steps are counted and logged per turn
(`agy-bridge: turn attempted N agent tool step(s)`); they never reach the client.

## Limits

- **Text only.** Client `tools`, `tool` messages and image parts are refused
  with 400 (`chat_tools_unsupported` / `invalid_request`), never dropped. The
  headless stream-json input takes text blocks only.
- **History is flattened.** `agy` has no system-prompt flag and no history
  replay, so system messages and earlier turns travel inside the one prompt as
  a deterministic transcript.
- **No plan windows.** `/usage` and `/credits` are interactive-only; the
  provider card shows usage from the ledger, not the plan's remaining quota.
  Quota exhaustion reaches the router as 429, so a combo moves on.
- **One process per request.** A trivial turn takes ~4.5–6 s and ~207 MB RSS
  (measured); `EZLLM_AGY_BRIDGE_CONCURRENCY` (default 2) caps them.
- **No token-by-token streaming.** 1.4.7 delivers the answer in one chunk at
  the end of the turn; a streaming client gets a single content delta.
- **Harness overhead.** Every turn carries the CLI's agent prompt: ~6.7k input
  tokens on Gemini, ~4k (mostly cache reads) on Claude, measured on a one-word
  reply.

## Verify

```bash
EZLLM_AGY_LIVE=1 go test -short -count=1 -v -run Live ./internal/agybridge
```

`TestLivePreflight` checks the pinned binary and login; `TestLiveTurn` spends
one small turn (default model `gemini-3.8-flash-low`, override with
`EZLLM_AGY_MODEL`) and checks streaming and usage. On an account that has not
cleared [eligibility](#eligibility) it skips, after checking the 403 and the
withheld link.

For an end-to-end check, use the gateway's [model-list command](../README.md#set-up-the-dashboard),
then send a short text prompt to a model from this account. The dashboard preset
shows bridge status; `docker compose logs ezllm` shows startup failures. The
internal listener is loopback-only, and the distroless image has no shell or curl.
