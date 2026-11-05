# GitHub Copilot CLI bridge — runbook

Replaces the retired OAuth/device-flow provider. The official Copilot CLI runs
behind a loopback listener; ezllm never speaks Copilot's private API and never
mints a session token. Design: `docs/copilot-cli-bridge-design.md`.

## What runs

`internal/copilotbridge` serves `/healthz`, `/status`, `/v1/models` and
`/v1/chat/completions` on `127.0.0.1:8793` (loopback only, bearer-gated with the
bridge token). One CLI child per request: `copilot -s --output-format=json
--no-ask-user [--model <id>]`, prompt on **stdin** (argv would break on a
600 KB prompt: one argv element caps at 128 KB).

## Enabling it

| Env | Meaning |
|---|---|
| `EZLLM_COPILOT_BRIDGE=on` | starts the surface |
| `EZLLM_COPILOT_BIN` | CLI binary (default `/opt/copilot/copilot`) |
| `EZLLM_COPILOT_HOME` | the CLI's own login directory (default `/copilot-home`) |
| `EZLLM_COPILOT_BRIDGE_ADDR` | default `127.0.0.1:8793`, must be loopback |
| `EZLLM_COPILOT_BRIDGE_TOKEN_FILE` | shared secret; minted 0600 on first boot |

Compose sketch:

```yaml
    environment:
      EZLLM_COPILOT_BRIDGE: "on"
      EZLLM_COPILOT_BIN: /opt/copilot/copilot
      EZLLM_COPILOT_HOME: /copilot-home
      EZLLM_COPILOT_BRIDGE_ADDR: 127.0.0.1:8793
      EZLLM_COPILOT_BRIDGE_TOKEN_FILE: /data/copilot-bridge.token
    volumes:
      - /opt/copilot:/opt/copilot:ro      # the CLI release
      - /home/<user>/.copilot:/copilot-home   # the login (read-write: the CLI
                                              # writes session state)
      - ./data:/data
```

**Image base.** The CLI links `libstdc++`/`libgcc_s`, so the image runs on
`distroless/cc-debian12`, not `base`. On `base` the bridge boots with
`cli=unknown` and every turn fails with "error while loading shared libraries".

**Cache directory.** The CLI unpacks a ~236 MB bundled package into its cache on
first run, and a container uid has no home (`HOME=/`), so it fails with
`EACCES: mkdir '/.cache'`. The bridge defaults the child's `XDG_CACHE_HOME` to a
temp dir; set `XDG_CACHE_HOME: /data/copilot-cache` in compose so the unpack
survives restarts.

**Login.** One-time, on the host: `copilot login` (device code on a headless
box). The token lands in `~/.copilot/`, which is what the mount above carries.
Boot fails loudly if `EZLLM_COPILOT_HOME` is not a directory — a bridge that
started without a login would look healthy while every request 401s.

## Adding the provider

Providers → Add provider → **GitHub Copilot (bridge)**. No key to paste: the
server resolves the bridge token on the key-add call (`kind: copilot-bridge`,
base `http://127.0.0.1:8793/v1`). If the bridge is off, the add answers 409
naming the env var.

## Migrating a database that still carries an OAuth account

A pre-retirement database keeps its `kind: github-copilot` account, and nothing
stops the router from booting with it — measured on 2026-11-05 with the shipped
binary and a planted row:

- **Boot: UP.** No refusal, no warning; the account still appears in
  `/admin/accounts` with `enabled: true`, so the dashboard looks healthy.
- **Every request through it fails**, and this is the only signal:
  `502 provider: unknown kind "github-copilot" (want one of [opencode-go
  openai-compatible anthropic-compatible gemini-openai gemini-web claude-web
  copilot-bridge])`.

So the migration is not optional and it is not automatic — the old account is a
trap that looks fine until traffic arrives. In the deploy window:

1. Providers → Add provider → **GitHub Copilot (bridge)**, namespace `copilot`,
   then Save & validate (no key to paste; the bridge must be running).
2. Point every combo/hop that named the old account at the new one. A combo
   referencing a dead account keeps failing even after the new one exists.
3. Disable (or delete) the `github-copilot` account so it stops looking usable.
4. Drive one real turn through the new hop and check `/admin/requests`: the hop
   must show `copilot-bridge` and a usage row with a cached split. Remember:
   a turn that needs client tools will refuse (`chat_tools_unsupported`) — that
   is expected on this hop, not a failed migration.

## Models

There is NO non-interactive model list. The bridge advertises a **curated seed**
(`copilotModelIDs` in `cmd/ezllm/copilotbridge.go`: `auto`, `gpt-6.1-sol`,
`gpt-6-luna`, `gpt-5.3-codex`) plus whatever the CLI reports for this login on
`auto` turns (`learned_models` on `/status`). `copilot help config` documents
every id the CLI knows, but not which your plan includes; the interactive
`/model` picker is the only plan-aware list.

- `auto` stays FIRST: the key test probes the first candidate, so it must be the
  one id every plan answers (the picker shows it as "Copilot Fusion").
- Ids after `auto` are listed UNTESTED. A model the plan does not include fails
  with a typed 403 `model_not_in_plan`; the operator decides what to enable.
- To add one, use an id from `copilot help config`; do not guess display names
  ("GPT-6.1 Terra" is not a documented id).

## What this hop can and cannot do

- **Cannot** accept client tool schemas (`chat_tools_unsupported`): the CLI runs
  its own 27-tool agent loop and there is no option that yields a pending tool
  call back to the caller. `/status` says so — never assume it from silence.
- No `response_format`, `n>1`, logprobs, or `reasoning_effort` mapping.
- History and system text are FLATTENED into one prompt; the CLI prepends its
  own scaffolding regardless.

## Cost to expect

Measured 2026-11-05, CLI 1.0.105: **~13–14k prompt tokens per call even for a
one-word answer** (≈9.4k of it is the CLI's 27 tool schemas), and **1 premium
request** per call. That is the price of routing through a real agent; the
ledger shows it as `tokens_in` + `tokens_cached_read`, so it is visible rather
than surprising.

## Accounting

Usage comes from the CLI's own `session.usage_checkpoint`:
`prompt_tokens` is INCLUSIVE (`input` + `cache_read` + `cache_write`), the cached
slice rides `prompt_tokens_details.cached_tokens`, and the ledger derives the
cold part. Verified end-to-end: bridge reported 13,971 inclusive with 9,420
cached; the ledger row stored `tokens_in 4,551` + `tokens_cached_read 9,420`.
A turn with no usage block stores ZEROS — "unknown", never estimated.

## Failure classes

| CLI words | Bridge | Router behaviour |
|---|---|---|
| not logged in / authentication | 401 `not_logged_in` | fail fast, no backoff |
| premium requests / quota / rate limit | 429 `quota_exhausted` | cool the hop |
| not in your plan | 403 `model_not_in_plan` | do not park as unhealthy |
| anything else | 502 `cli_failed` | normal failover |

## Gates and updates

`ezllm bridge-check copilot` before promoting a CLI release;
`scripts/bridge_update.py` covers it like the other three (official channel:
`copilot update`). The JSONL event set is version-sensitive: a release that
renames `session.usage_checkpoint` records usage as unknown (zeros), and the
parser fixture in `internal/copilotbridge/bridge_test.go` is the tripwire.

## Known gaps

- Client tools: refused (design §10; the `@github/copilot-sdk` route is a
  time-boxed spike, not a shipped behaviour).
- No quota panel reading: the CLI does not expose the plan's premium-request
  balance headlessly, so the dashboard shows the bridge's last usage, not live
  remaining quota.
