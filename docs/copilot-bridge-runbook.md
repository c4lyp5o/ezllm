# GitHub Copilot subscription bridge — runbook

Replaces the retired OAuth/device-flow provider. The official Copilot CLI runs
behind a loopback listener; ezllm never speaks Copilot's private API and never
mints a session token. Design: `docs/copilot-cli-bridge-design.md`.

## What runs

`internal/copilotbridge` serves `/healthz`, `/status`, `/v1/models` and
`/v1/chat/completions` on `127.0.0.1:8793` (loopback only, bearer-gated with the
bridge token). Requests use `copilot --headless --stdio` with a fresh session.
Processes are cold by default; the opt-in warm pool reuses only successfully
cleaned-up processes. Plain text can fall back to `copilot -p`; requests carrying
tools or images cannot take that fallback. Prompts and native image blobs travel
on stdin, not argv.


## Enabling it

| Env | Meaning |
|---|---|
| `EZLLM_COPILOT_BRIDGE=on` | starts the surface |
| `EZLLM_COPILOT_BIN` | CLI binary (default `/opt/copilot/copilot`) |
| `EZLLM_COPILOT_HOME` | the bridge's own login directory (default `/copilot-sub`) |
| `EZLLM_COPILOT_BRIDGE_ADDR` | default `127.0.0.1:8793`, must be loopback |
| `EZLLM_COPILOT_BRIDGE_TOKEN_FILE` | shared secret; minted 0600 on first boot |

Compose sketch:

```yaml
    environment:
      EZLLM_COPILOT_BRIDGE: "on"
      EZLLM_COPILOT_BIN: /opt/copilot/copilot
      EZLLM_COPILOT_HOME: /copilot-sub
      EZLLM_COPILOT_BRIDGE_ADDR: 127.0.0.1:8793
      EZLLM_COPILOT_BRIDGE_TOKEN_FILE: /data/copilot-bridge.token
    volumes:
      - /opt/copilot:/opt/copilot:ro      # the CLI release
      - /home/<user>/.copilot-sub:/copilot-sub   # the login (read-write: the CLI
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

**Login.** Sign the bridge in to its own folder, `~/.copilot-sub`, never your
everyday `~/.copilot` — the same `*-sub` layout as the Claude, Codex and
Antigravity bridges. A personal `~/.copilot` also carries your sessions,
plugins and IDE links, and signing out there would break the bridge. Run this
once on the host, with the same CLI binary the bridge mounts:

```sh
env -u DBUS_SESSION_BUS_ADDRESS COPILOT_HOME=~/.copilot-sub \
  /opt/copilot/copilot login --device-code
ls ~/.copilot-sub/config.json   # the stored login the container reads
```

Replace `/opt/copilot/copilot` with the host path of that binary. It prints a
URL and a code; open the URL, enter the code and approve with the GitHub account
that has the Copilot plan. Unsetting D-Bus matters on a desktop host: with a
keyring available the CLI stores the token there, where the container cannot
reach it; without one it writes `config.json` in `COPILOT_HOME`.

Boot fails loudly if `EZLLM_COPILOT_HOME` is not a directory — a bridge that
started without a login would look healthy while every request 401s.

**Upgrading from before 1.9.0.** The default login directory changed from
`/copilot-home` to `/copilot-sub`, and there is no fallback. Log in to
`~/.copilot-sub` as above, change the mount to `~/.copilot-sub:/copilot-sub`,
set `EZLLM_COPILOT_HOME: /copilot-sub` (or remove the variable to use the
default) and recreate the container (`docker compose up -d --no-build`). A
deployment that still mounts `/copilot-home` without setting the variable
refuses to start, naming the missing directory.

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
ezllm bridge-check copilot --bin ~/.local/bin/copilot --home ~/.copilot-sub --json
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

**Optional managed updates.** The host updater now supports Copilot through
stable, digest-verified official releases and the same gate/promotion/recovery
flow as the other bridges. It is disabled in updater defaults until you migrate
the legacy standalone binary mount to `/opt/copilot-bridge` and point
`EZLLM_COPILOT_BIN` at `/opt/copilot-bridge/current/copilot`. Initialization copies
the existing binary without replacing it. Follow the
[bridge update runbook](bridge-update-runbook.md) before enabling it.
The live gate honors warm-pool mode and closes its workers when finished.

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

## Opt-in warm CLI processes

Set `EZLLM_COPILOT_BRIDGE_WARM_POOL=on` and restart ezllm to enable process
reuse. The default is `off`; setting it back to `off` and restarting restores
one process per request. Accepted enabled values are `on`, `true`, `1`; disabled
values are `off`, `false`, `0`, or unset. Invalid values fail startup.

The pool starts lazily and uses the existing concurrency/queue settings. Each
process serves one request at a time. With session caching off, each request creates a fresh session
with its own system message and explicit tool allowlist. Reuse requires usage,
idle and a successful session deletion. Cancellation, timeouts, protocol errors
or uncertain cleanup discard the process; possibly billed inference is not
replayed automatically. Delayed events from completed sessions are discarded;
missing or unexpected session IDs fail the turn and retire the worker.

Idle workers expire after one minute. Workers also retire after 100 successful
turns or 15 minutes of age, checked between requests. Binary identity/size/mtime
is checked before reuse; a changed binary causes replacement. Configuration or
login changes should be followed by a restart. Shutdown drains HTTP requests
within the existing grace period, then kills and reaps warm workers.

`GET /status` exposes `runtime.pool` (`enabled`, `workers`, `idle`, `closed`) and
`runtime.stage_timings`. Watch setup timings and memory before enabling this
beyond a controlled rollout. On this host one CLI retained approximately
270–282 MiB RSS over a three-turn proof; two warm workers can therefore retain
substantial memory. That sample does not establish a steady-state memory bound.
The existing concurrency cap and worker expiry limit exposure. First requests
are still cold, and model processing time is unaffected by process reuse.

## Opt-in completed-conversation cache

On v1.9.0 or later, enable both settings and restart the built ezllm image:

```text
EZLLM_COPILOT_BRIDGE_WARM_POOL=on
EZLLM_COPILOT_BRIDGE_SESSION_CACHE=on
```

The second setting defaults off; enabling it without the warm pool fails startup.
A matching request must supply the unchanged prior messages and assistant answer,
then one new user message, with the same explicit model, system and tools.
Hermes cache markers may move without changing the actual conversation.
Only the new user turn is sent to the retained native conversation. No new
service or worker pool is added. Each worker retains at most one completed
session, capped at 1 MiB of normalized history and the existing expiry limits.

Tool availability does not prevent reuse, but a newly emitted tool batch is
accounted and deleted. Tool-result continuations, images, `auto`, edits and
identical stateless retries take the fresh-session path. This mode does not yet
cache an active Hermes tool loop. Failed cleanup or regressed usage counters
cannot trigger another inference automatically.

The loopback `/status` reports `session_cache`; watch
`runtime.stage_timings.session_cache_hit.count` along with vendor cache-read and
credit counters. Native checkpoints are cumulative, while HTTP/ledger usage
remains per request. A matching native session does not guarantee provider cache
hits. Setting the cache flag back to `off` and restarting restores fresh sessions
while retaining process pooling. Run CLI qualification in the same configured
mode; cache mode adds one small call to its live gate.

See [the cache design and measured evidence](copilot-completed-session-cache-design.md)
for matching, accounting, lifecycle and experiment limits.

## Shared transport status

Copilot still uses its own streaming transport. **Migration to the shared
`internal/bridgeutil` transport remains outstanding.** The current bridge
continues operating independently; no shared lifecycle migration has shipped.

Claude and Antigravity are migrated; Codex and Copilot still need expansion.
See [shared transport status and remaining work](shared-bridge-transport-design.md).

## Image input

Copilot accepts OpenAI `image_url` parts in user messages when the URL is an
inline `data:image/png;base64,...` or `data:image/jpeg;base64,...` value. The
bridge validates base64, image header/MIME agreement and dimensions, then sends
native in-memory blob attachments through `session.send`. Base64 stays out of
the text prompt; numbered markers preserve image order and historical-message
association in the flattened transcript. No image file is written by the bridge.

| Constraint | Behavior |
|---|---|
| Model | Use an explicit model ID; image requests with `auto` are refused. |
| Formats | Inline PNG and JPEG only. Remote HTTP URLs, file paths and other formats are refused. |
| Bridge bounds | At most four images, 4 MiB decoded per image, 8 MiB decoded total, 20 million pixels per image. |
| Model bounds | The CLI's `models.list` must advertise vision and usable count, size and MIME limits; its stricter limits apply before session creation/inference. |
| `detail` | Omitted or `auto`; `low`/`high` are refused because the CLI owns image processing. |
| Message roles | User messages only, including historical user messages. |
| Channel failure | Refusal is preserved; no text-only fallback that drops the image. |

Measured on CLI 1.0.105, `gpt-6-luna` advertises **one image, up to 3 MiB**.
Two tiny live HTTP requests through the implementation, non-streaming and
streaming, both identified a two-color PNG correctly and returned positive usage.
The warmed process remained reusable. This proves that tested model/path, not
vision availability on every model in the catalog. Usage remains the CLI's
reported accounting; each request creates its own session.

Image/parameter refusals occur before inference. Check the selected model's
advertised limits when an image is refused rather than increasing the bridge's
global bounds. Antigravity's headless stream-json input remains text-only.
