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
   client tools are EMULATED on this hop (see below): check a tool-using client
   once, because compliance depends on the model.

## Models

There is NO non-interactive model list, and the CLI does not say which ids a
plan includes. So the catalog is every id the CLI KNOWS, and the plan decides at
request time:

- At boot the bridge runs `copilot help config` (a local help page: no network,
  no premium request) and reads the documented `model` ids, so a CLI update is
  picked up at the next restart. `copilotModelIDs` in `cmd/ezllm/copilotbridge.go`
  is the built-in copy of that list (CLI 1.0.105) for when the read fails, and
  `TestParseHelpConfigAgainstTheRealCLI` fails if the help page changes shape.
- Ids the CLI reports on `auto` turns are learned on top (`learned_models` on
  `/status`).
- A model the login does not include answers **403 `model_not_in_plan`**: from the
  bridge before any spawn when the id is not in the catalog, and from the CLI's
  own refusal otherwise. The router treats it as "model unavailable", not as an
  unhealthy hop.
- `auto` stays FIRST: the key test probes the first candidate, and `auto` is the
  one id every plan answers (the picker shows it as "Copilot Fusion").
- The interactive `/model` picker is the only plan-aware list. Which models are
  enabled for routing is the operator's call in the dashboard.

## What this hop can and cannot do

- **Client tools are emulated, not native.** The CLI runs its own agent loop and
  has no option that yields a pending tool call to the caller, so the bridge
  renders the caller's `tools` into the prompt, asks the model to reply with
  `<tool_call>{"name":…,"arguments":{…}}</tool_call>`, and parses that into
  OpenAI `tool_calls` (`finish_reason: "tool_calls"`). Tool results sent on the
  next request are replayed into the transcript; `tool_choice` none / required /
  a named function is honoured in the prompt. On such turns the CLI's own tools
  are hidden from the model, so it cannot reach for them instead. Consequences: it depends on the
  model following the format (a reply that does not parse comes back as plain
  text, never as an invented call); with tools declared the stream is buffered
  until the reply is complete; the 13–14k-token scaffolding cost still applies;
  only `function` tools (a non-function type is refused
  `chat_tools_unsupported`). `/status` says "emulated".
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
| not in your plan / id not in the catalog | 403 `model_not_in_plan` | do not park as unhealthy |
| anything else | 502 `cli_failed` | normal failover |

## Gates and updates

`ezllm bridge-check copilot` before promoting a CLI release;
`scripts/bridge_update.py` covers it like the other three (official channel:
`copilot update`). The JSONL event set is version-sensitive: a release that
renames `session.usage_checkpoint` records usage as unknown (zeros), and the
parser fixture in `internal/copilotbridge/bridge_test.go` is the tripwire.

## Known gaps

- Client tools are emulated through the prompt (design §10 addendum), not
  native; the `@github/copilot-sdk` route would make them native and is still a
  time-boxed spike, not a shipped behaviour.
- No quota panel reading: the CLI does not expose the plan's premium-request
  balance headlessly, so the dashboard shows the bridge's last usage, not live
  remaining quota.
