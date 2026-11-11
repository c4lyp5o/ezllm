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
   must show `copilot-bridge` and a usage row with a cached split. Then drive
   one turn from a tool-using client: `/status` `client_tools` must say `native`.

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

- **Every turn runs over `copilot --headless --stdio`**, with `-p` as the fallback
  for plain chat. `/status` `channel` says which applies.
- **Client tools are native.** The model calls the caller's tools,
  the bridge returns the batch as `tool_calls` (`finish_reason: "tool_calls"`) and
  stops the turn, and the caller's next request replays the results as text in a
  fresh session. Each step of a tool loop is therefore one premium request.
  Consequences:
  - Only the caller's tools are visible. The CLI's shell and file tools are not,
    so nothing runs on this box, and a caller's tool named `bash` goes to the caller.
  - `tool_choice` none is a plain turn with no tools visible. Named and
    `required` choices are refused so another hop can enforce them.
  - Only `function` tools. Tool names may use ASCII letters, digits, underscores
    and hyphens, as the CLI enforces at session creation.
  - **If the channel breaks:** on a CLI whose channel is missing or at another
    protocol version, plain chat falls back to `-p` (logged as `plain chat falling
    back to -p`, and back to its scaffolding cost), and a request with tools answers
    400 `chat_tools_unsupported` before anything is billed, so the router fails over
    with no cooldown. A tool the CLI cannot register gets the same 400. `/status`
    reports the boot check and the log has `copilot bridge: channel unavailable`.
    After a CLI update that causes this, re-measure the channel and update `channelProtocolVersion`
    (`internal/copilotbridge/channel.go`).
- Output token limits are refused so another hop can enforce them. No
  `response_format`, `n>1`, logprobs, or `reasoning_effort` mapping.
- History is FLATTENED into one prompt. The caller's system text replaces the
  CLI's system message (on the `-p` fallback it is flattened too, behind the CLI's
  own scaffolding).

## Cost to expect

Measured 2026-11-05, CLI 1.0.105: **1 premium request per turn**, so one per step
of a tool loop. On AI-credit plans the charge follows tokens and the model, and
`copilot_nano_aiu` in `raw_usage` is that charge in billionths of a credit.

| Turn (no cache) | Prompt tokens | AI credits |
|---|---|---|
| Plain, short, `auto` on the channel | 95 | 0.025 |
| Two tools, `gpt-6-luna` on the channel | 182 | 0.00422 |
| Plain, 189 KB, `gpt-6-luna` on the channel | 59,095 | 0.740 |
| Plain, short, `gpt-6-luna` on the `-p` fallback | 13,957 | 0.1747 |

The `-p` floor of ~13–14k tokens is the CLI's agent scaffolding (≈9.4k of it its 27
tool schemas). Later `-p` calls would read part of it from cache. Channel turns took
11–27 s end to end on this box, and each request spawns a new child. One short reply
measured a model call of about 1 s, so most of a short turn's time is outside the
model.

## Accounting

Usage comes from the CLI's own `session.usage_checkpoint`:
`prompt_tokens` is INCLUSIVE (`input` + `cache_read` + `cache_write`), the cached
slice rides `prompt_tokens_details.cached_tokens`, and the ledger derives the
cold part. Verified end-to-end: bridge reported 13,971 inclusive with 9,420
cached; the ledger row stored `tokens_in 4,551` + `tokens_cached_read 9,420`.
A turn with no usage block stores ZEROS — "unknown", never estimated.
`raw_usage` also carries `copilot_premium_requests` and `copilot_nano_aiu`, the turn's
AI-credit charge in billionths of a credit.

## Failure classes

| CLI words | Bridge | Router behaviour |
|---|---|---|
| not logged in / authentication | 401 `not_logged_in` | fail fast, no backoff |
| premium requests / quota / rate limit | 429 `quota_exhausted` | cool the hop |
| not in your plan / id not in the catalog | 403 `model_not_in_plan` | do not park as unhealthy |
| anything else | 502 `cli_failed` | normal failover |

## Gates and updates

**`ezllm bridge-check copilot` exists** and is the gate to run after every
`copilot update`:

```
ezllm bridge-check copilot --bin ~/.local/bin/copilot --home ~/.copilot --json
ezllm bridge-check copilot --live --json     # what promotion requires
```

Two tiers, like the other bridges:

- **Static (free, no request):** the binary resolves, `--version` reports, and the
  undocumented `--headless --stdio` channel answers at protocol **3** and accepts
  a `session.create`. That last part is the point of the check: `--headless` is
  not in `copilot --help`, so a release can move it without any version number
  moving.
- **`--live` (two premium requests):** **proves the login**, then drives a plain
  turn and a client-tool batch plus the replay turn after it. The text is judged,
  never printed.

**The static tier cannot prove a login, and says so.** Measured 2026-11-06: an
empty `EZLLM_COPILOT_HOME` still passes `session.create`, because the CLI only
reads the credential when a prompt asks GitHub for something. With a stripped
environment (`env -i`, no session stores) the same call answers
`No GitHub OAuth token or Copilot HMAC key provided` — so **promotion requires
`--live`**, and a static pass is a note, not a verdict:

```
{"bridge":"copilot","ok":true,"version":"1.0.105","release":"qualified","live":true,"problems":[]}
```

**Not managed by `scripts/bridge_update.py`.** Its `BRIDGES` table lists claude,
codex and agy; there is no versioned bridge root to stage here, because the CLI is
a standalone binary updated by `copilot update`. So the gate is manual today.

**Version-sensitive:** a release that renames `session.usage_checkpoint` records
usage as unknown (zeros). Parser fixtures in `internal/copilotbridge/bridge_test.go`
(`-p`) and `channel_test.go` (the channel, numbers written as `1.0`) are the
tripwire — if they pass and a live turn reports zeros, the event names moved.

## Known gaps

- `--headless` is not listed in `copilot --help`; it is what the bundled SDK runs.
  A CLI update could change it, which is why every tool turn checks the protocol
  version and falls back to the refusal above.
- A tool loop costs one premium request per step. Keeping the session open between
  HTTP requests would make it one per loop, but measured only ~20% cheaper in AI
  credits and is not built (design §10 addendum).
- Only OpenAI-family models were driven through the channel live. A model the
  login lacks is refused at session creation at no cost.
- No quota panel reading: the CLI does not expose the plan's premium-request
  balance headlessly, so the dashboard shows the bridge's last usage, not live
  remaining quota.

## Slow responses

The loopback bridge's `/status` response includes `runtime.stage_timings`, with
`count`, `avg_ms` and `last_ms` for each observed stage. Inspect `ping` and
`session.create` for CLI readiness/session overhead, `model_wait` for the wait
after send acknowledgement, and `usage_drain`/`session.delete` for cleanup.
`queue` measures wait for an acquired concurrency slot. These are aggregate,
overlapping measurements, not additive phases or percentiles; concurrent calls
can update different last values. No request content is retained.

Streamed tool batches are flushed before accounting cleanup. Usage and stream
completion still follow cleanup, so clients that wait for `[DONE]` before
executing tools retain that wait. Do not shorten the accounting timeout based
on first-response latency alone. Measured examples and limitations are in
[copilot-latency-diagnostics.md](copilot-latency-diagnostics.md).
