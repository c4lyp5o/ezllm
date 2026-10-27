# Claude subscription via the official Claude Code CLI — design

**Status:** signed off 2026-10-19 · M1 shipped 2026-10-19 · M2 shipped 2026-10-21
**Author:** melissa · **Operator gate:** Calypso (pushes/deploys)

## 1. Goal and the hard constraint

ezllm should serve **real Claude subscription capacity (Pro/Max)** to its clients, so every
client keeps pointing at ezllm and nothing is installed into Hermes. Hermes already points at
ezllm (`model.provider=custom:ezllm-openai`), so a new account + combo is the entire client-side
change.

**Hard constraint — "no trouble".** Only the **official Claude Code executable** may drive the
subscription. Reusing the subscription OAuth bearer from ezllm's own Go process (with a
`claude-cli` user agent and OAuth beta headers) is **rejected**: that is the pattern Anthropic
policies, and the Hermes plugin exists in its particular shape precisely becauseNous refused to
take that path.

Two consequences follow:

- The official **Hermes plugin `claude-subscription-directsdk` is not used and not needed.** It is
  a Hermes-internal Python model-provider: it has no service, no daemon and no HTTP endpoint, so
  ezllm cannot "use" it. Its source (MIT) is a design reference for the parts we do implement.
- The measured unit of subscription work is `claude -p`, so ezllm must own a process that spawns
  that binary. That is this design.

`claude-web` (the claude.ai cookie path) **stays in the tree** by explicit operator decision —
reserved for later use, not part of this design.

## 2. Measured baseline (live, 2026-10-19)

Environment: CLI **2.1.299**, `installMethod=native`, binary at
`~/.local/share/claude/versions/2.1.299`, subscription login in `~/.claude-sub`
(`authMethod=claude.ai`, `subscriptionType=pro`). All calls ran with
`CLAUDE_CONFIG_DIR=/home/calypso/.claude-sub`, isolated from `~/.claude` (which is pointed at
ezllm). Isolation proven: **zero** `calls` rows in the live ledger for any claude client during
the measurement window.

| probe | wall | usage | native list-$ |
| --- | --- | --- | --- |
| haiku, trivial, tools on (cold) | 4.48s (api 1751ms, ttft 2245ms) | in 2 / out 4 / cache_read 8607 / cache_create 11650 | 0.00242 |
| haiku, trivial, tools on (identical repeat) | 3.82s | in 2 / out 4 / cache_read 8608 / cache_create 13343 | 0.00276 |
| haiku, 12k-char prompt (cold) | 5.56s | in 2 / out 437 / cache_read 11180 / cache_create 16304 | 0.00359 |
| haiku, 12k-char prompt (identical repeat) | 5.27s | in 2 / out 308 / cache_read **27484** / cache_create **0** | **0.00043** |
| haiku, `--disallowedTools '*'` (cold) | 3.33s | in 2 / out 4 / cache_read 535 / cache_create 5527 | 0.00111 |
| haiku, `--disallowedTools '*'` (repeat) | 3.32s | in 2 / out 4 / cache_read 4983 / cache_create 333 | 0.00012 |
| opus, trivial, tools on | 6.43s | — | **0.10021** |

Findings that shape the build:

1. **No Node needed.** `claude` is a native ELF binary; the bridge image carries the binary and CA
   certificates, not a JS runtime.
2. **Harness overhead is the floor.** Every call carries the CLI's own system prompt and tool
   definitions: **~20.2k tokens with tools on** (8607 read + 11650 created on a *trivial* prompt),
   **~6.1k with `--disallowedTools '*'`**. The bridge can never serve cheaper than its own harness.
3. **Prompt caching works when the prefix is byte-stable, and breaks when it is not.** A repeated
   12k-char prompt read 27,484 tokens from cache with zero cache-creation and cost 8.4x less; a
   repeated *tiny* prompt did the opposite (cache_create grew 11650 → 13343), because the CLI
   injects per-invocation context into a short prefix. This is the same failure mode the Hermes
   plugin normalizes; it is deferred to M3 and measured, not assumed.
4. **The stream-json channel works**: `-p --verbose --input-format stream-json --output-format
   stream-json --include-partial-messages` streams incremental `stream_event` deltas, an `init`
   frame carrying model / permissionMode / tools / mcp_servers / apiKeySource, a `result` frame
   carrying usage + cost + ttft, and a `rate_limit_event` frame carrying plan state. Without
   `--verbose`, `--print` + `--output-format=stream-json` is refused.
5. **`--max-turns 1` is accepted but absent from `--help`** — the flag surface drifts, so the
   bridge must assert its flags at boot instead of trusting help text.
6. **The CLI auto-loads an MCP server** (`claude.ai Claude Docs`, `source: claudeai`) — suppressed
   with `--strict-mcp-config` plus a bridge-owned config.
8. **Process startup is ~1.5–2s** of the 3.2–6.4s wall (api time 1.2–1.8s). Accepted in v1.
9. **The harness overhead is not the floor.** It is the floor of the naive `-p` path only: handing
   native the upstream body via `CLAUDE_CODE_EXTRA_BODY` measured **170 input tokens** for a
   three-message body against ~20k for the same turn with tools on. This hop's economics are set by
   the client's body, not by Claude Code's own prompt.
10. **Opus is reachable on Pro** (no usage-credit refusal) at $0.10 list-equivalent for a trivial
    call — 40x the haiku figure. It stays out of every automatic path.
11. **Mandatory thinking changes `max_tokens` semantics.** Haiku 5.5 thinks whether asked or not, and
    its thinking tokens count against `CLAUDE_CODE_MAX_OUTPUT_TOKENS`: a small `max_tokens` produced
    `API Error: Claude's response exceeded the … output token maximum` (measured at 32, fine at 256)
    rather than a truncated answer.

### 2.1 M2 spike — client tools through the extra body (live, 2026-10-21)

CLI **2.1.306** (2.1.299 had been removed by the CLI's auto-updater; requalified and
pinned as a copy, see the runbook). The bridge's exact argv and env; the only change is
`tools`/`tool_choice` added to `CLAUDE_CODE_EXTRA_BODY`. Redacted frames:
`internal/claudebridge/testdata/m2_*.jsonl`.

| case | result | upstream generations |
| --- | --- | --- |
| one tool, `auto` | `tool_use` named `get_weather`, `stop_reason: tool_use` | 1 |
| `write_file` sentinel | `tool_use` emitted, the file was never written | 1 |
| history: `tool_use` → `tool_result` | final text uses the result | 1 (plus a zero-turn replay ack) |
| `tool_choice` `tool` / `any` | forced `tool_use` | 1 |
| parallel (3 cities) | 3 `tool_use` blocks in one message | 1 |
| "list your tools" | answers `get_weather` only | 1 |
| `tool_choice: none` + tool history | text answer | 1 |
| history with tool blocks, no `tools` field | text answer, accepted | 1 |

Findings that shape M2:

12. **Approach A holds; the inert MCP server is not needed.** Upstream sees exactly the
    client's tools (the CLI's `init` frame reports `tools: []`), names come back
    unchanged, and nothing executes.
13. **Native then answers its own tool call locally.** After a `tool_use` it emits a
    `user` frame with `tool_result` "No such tool available" (`is_error`), and
    `--max-turns 1` ends the run with `result.subtype: error_max_turns`,
    `is_error: true`, `stop_reason: tool_use`, while `usage.iterations` has one
    entry. The bridge must treat that as success and never relay the synthetic result.
14. **One `assistant` frame per content block.** Parallel calls arrive as three
    `assistant` frames, so the non-stream body has to accumulate blocks, not keep the
    last frame. Blocks carry an extra `caller: {"type":"direct"}` field.
15. **The empty-reply nudge can run a second generation.** An empty first answer made
    native append "[Your previous response had no visible output…]" and generate
    again: two `message_start`s on the stream and two billed iterations. It is not
    tool-specific; the bridge relays only the first upstream message.
16. **The prefix cache works for agent tool lists, so M3 is not triggered.** With
    Hermes's real 57 tools (~65 KB) plus a ~1.5 KB system prompt, request 1 wrote
    **24,112** tokens to the cache (`ephemeral_1h`, a one-hour lifetime), and requests
    2–5 wrote **0** and read **24,112** each:

    | req | haiku in / create / read / out | sonnet in / create / read / out |
    | --- | --- | --- |
    | 1 | 122 / 24112 / 0 / 286 | 122 / 24112 / 0 / 152 |
    | 2 | 568 / 0 / 24112 / 287 | 434 / 0 / 24112 / 209 |
    | 3 | 906 / 0 / 24112 / 141 | 674 / 0 / 24112 / 78 |
    | 4 | 1078 / 0 / 24112 / 78 | 809 / 0 / 24112 / 63 |
    | 5 | 1213 / 0 / 24112 / 80 | 900 / 0 / 24112 / 47 |

    Cold input grows with the uncached history tail, roughly 100–350 tokens per turn
    here. Revisit M3 if long sessions show that tail dominating.
17. **`rate_limit_event` reports the plan's real window state.** It includes
    `unifiedWindows.five_hour.utilization` and `seven_day.utilization` with reset
    times, which is a better cap signal than any local meter. This is recorded for
    later and not used by M2.
18. **Native spends two of upstream's four cache breakpoints.** Two client
    `cache_control` markers plus Hermes's tools cached normally (23,512 read). Four
    were refused: `API Error: 400 A maximum of 4 blocks with cache_control may be
    provided. Found 6.` Hermes marks its system prompt and last three messages, so the
    bridge keeps only the newest two client markers (`clientCacheMarkers`,
    `internal/claudebridge/frames.go`).

## 3. Architecture: in-process loopback bridge, ezllm core untouched

```
client (Claude Code / Hermes / VS Code)
   └─> ezllm  ── combo claude-cc ──> account "claude-sub"
                                        │  kind: anthropic-compatible
                                        │  base_url: http://127.0.0.1:8790/v1
                                        │  key: shared secret from /data
                                        v
                              loopback listener, SAME process
                                        │  spawns: claude -p --input-format stream-json …
                                        v
                              api.anthropic.com  (subscription plan)
```

**Loopback surface, in-process.** One container, one process: `internal/claudebridge`
serves an Anthropic Messages surface on `127.0.0.1:8790`, and the ordinary
`anthropic-compatible` account dials it. It binds loopback and **refuses any other
bind** — the surface exists for exactly one consumer. Endpoints: `POST /v1/messages`
(inference), `GET /v1/models` (the catalog the account syncs and the key-test
oracle), `GET /healthz` (two fields, house contract), `GET /status` (operator
detail: CLI version, login state, plan).

What is mounted into the container:

- the **official CLI binary**, read-only, pinned by path
  (`~/.local/share/claude/versions/2.1.299` → `/opt/claude/claude`);
- the **subscription login directory**, read-write (`~/.claude-sub` → `/claude-sub`,
  because the CLI refreshes its own token there);
- the account's **shared secret**, auto-generated into `/data` on first boot (the
  master-key pattern: on the data volume, backed up with the ledger).

Container requirements: `USER`/`LOGNAME` must be set (the CLI's local login check
reads process identity; without them a logged-in CLI reports `logged-out` instantly
with no upstream request), and the runtime base must carry **glibc** — measured:
the CLI is a glibc-dynamic ELF (`ld-linux`, libc/librt/libpthread/libdl/libm), and
on `distroless/static` the exec fails with a misleading "no such file or directory".
Alpine cannot run it at all (musl). `distroless/base-debian12` (20.8 MB) is the
smallest maintained base that works: glibc + CA certs + NSS (`libnss_dns`, needed
for DNS, is invisible to `ldd`), still no shell and no package manager.

**ezllm side: no new code path.** It is an ordinary `anthropic-compatible` account — the existing
adapter already speaks `/v1/messages` with `x-api-key`, the existing usage tap reads the native
Anthropic envelope, and native SSE passes through byte-for-byte with no translation cell. Nothing
in `internal/router`, `internal/proxy` or `internal/translate` needs a new concept.

**Rejected alternatives.** A separate `claude-bridge` sidecar container — built first,
then deleted: a second image, a second container, an inter-service token and a port
for what is fundamentally "spawn this binary", and the isolation it bought is modest
on one box (ezllm already holds every provider key, encrypted at rest). Baking Node +
the CLI into the ezllm image (a JS runtime and a version-coupled subprocess supervisor
inside the public gateway, plus an image rebuild per CLI release). Running a Hermes
instance as a CLI-driving proxy and dialing it as an OpenAI-compatible upstream (two
agent harnesses stacked in the request path).

## 4. Bridge request contract

Per request: a private temporary `HOME` (so nothing touches the operator's `~/.claude` state),
`--settings` carrying `CLAUDE_CODE_EXTRA_BODY`, `--system-prompt-file` carrying the client's system
prompt, `--setting-sources ''`, `--strict-mcp-config` with a bridge-owned empty MCP config,
`--tools ''`, `--max-turns 1`, `--permission-mode dontAsk`, `--no-session-persistence`, a
conversation-stable `--session-id`, `--include-partial-messages`, and the model alias passed
through unchanged (`sonnet`, `haiku`, `opus`, and the `[1m]` forms the CLI advertises).

- **Body (`CLAUDE_CODE_EXTRA_BODY`)**: the client's own Messages body is what the subscription
  sees — measured: with the body carrying one history and the stdin frames carrying a different
  one, the answer followed the body. Rules discovered live and mirrored from the official plugin's
  `request_body()`: no `system` (native owns that slot via `--system-prompt-file`), no message `id`s
  (the strict schema rejects them: `messages.1.id: Extra inputs are not permitted`), no
  `temperature`/`top_p` (subscription routes reject sampling controls).
- **Replay**: non-final user frames carry `shouldQuery:false`, so native acknowledges them with a
  zero-turn `result` instead of generating — without it, measured, every user frame produced its own
  upstream request and its own plan draw. The last user frame is the only trigger.
- **Streaming:** `stream_event` text deltas → Anthropic `content_block_delta`; final `result` frame
  → terminal usage; `rate_limit_event` → the plan state a 429 should carry.
- **Usage:** the native envelope is already Anthropic-shaped (measured `input_tokens=2`,
  `cache_read_input_tokens=8609`, `output_tokens=4`, i.e. cache **excluded** from `input_tokens`).
  The bridge account therefore declares `usage_convention=anthropic` (the vocabulary is
  `auto|anthropic|total`) so `tokens_in` means cache-COLD spend and `tokens_cached_read` is real.
  The convention is not a routing gate (the router's `messagesConventionReason` gate was removed
  in `61a403c`; `auto` reads Messages usage as exclusive, see `internal/router/router.go`), so
  this is declared for honest accounting, not to pass a check.
- **Errors:** plan or session limit → 429; missing login → 401; binary missing → 503; malformed
  native output → 502. Each carries the CLI's own words, unrewritten.
- **Lifecycle:** one process per request; writes go to the bridge-owned MCP server over stdio;
  client cancel or timeout kills the whole process group; a spawn semaphore bounds concurrency.

## 5. Drain scoping — the safety property

The subscription's real limit is a shared rolling window the router cannot observe, and every call
draws at the full `claude -p` rate. Agent traffic alone has been ~1,000 calls/day through this
gateway.

Therefore: the subscription account is reachable **only** through a dedicated combo whose
consumers are Claude Code clients. It is never a hop in `daily`, never a default provider, never an
automatic fallback. The combo's hop set is the bridge account alone, so a plan limit surfaces to
that client as a 429 instead of silently spilling other traffic onto a capped hop. A
`cap_window`/`cap_tokens` fuse on the account is set as a second line of defence, and the account's
`notes` carry the window caveat and the Opus list-price figure so the operator sees it in the UI.

## 6. Staged build

**M1 — transport, text only. SHIPPED 2026-10-19.** Bridge (`internal/claudebridge`; first a
separate `cmd/claude-bridge` binary and container, folded in-process since — see §3),
`/v1/messages` (stream and non-stream), usage and error
mapping, `/v1/models`, with tool-bearing requests **refused explicitly** via
`chat_tools_unsupported` rather than silently answered without tools. Live proof
`tests/prove_claude_bridge.py` runs the real binary against the real CLI and a real Pro
subscription: 13/13 (health contract, auth, catalog, streaming SSE, native usage fields, refusal,
history fidelity). Remaining and operator-side: create the `anthropic-compatible` account and the
Claude-Code-only combo in the dashboard once the container is up (see
`docs/claude-bridge-runbook.md`).

**M2 — tools. SHIPPED 2026-10-21 (approach A, extra-body passthrough).** The spike
(§2.1) showed the planned inert MCP server is unnecessary. The client's `tools` and
`tool_choice` ride in `CLAUDE_CODE_EXTRA_BODY` next to its messages, so upstream sees
exactly the client's tools and returns `tool_use` under their own names. Native is
still spawned with `--tools ''` and an empty MCP config, so it has nothing to run; it
answers the unknown call locally and stops at `--max-turns 1` (`error_max_turns`),
which the bridge treats as success. Only the first upstream message is relayed, which
drops native's synthetic `tool_result` and its empty-reply nudge. Blocks accumulate
across native's one-frame-per-block output. The newest two client `cache_control`
markers are kept (native spends two of upstream's four). Server tools
(`web_search_*` & co.) and the OpenAI-only `parallel_tool_calls` are refused before
spawn. The original MCP plan stays the fallback if a CLI release breaks passthrough.

**M3 — cache/replay normalization.** Only if M2 measurements show poor cache reads on long
agent-style conversations. The mechanics to port (replaying the native date line, stripping the
context reminder before the newest user frame, relocating cache markers without laundering host
content, one upstream request per call) are documented in the Hermes plugin, MIT-licensed, and
credited.

Each milestone: focused in-package tests, a live proof through the router, and the repository gate
(`make test-short` locally; the full gate runs in CI).

## 7. Version pinning and boot checks

The measured, qualified CLI version is **2.1.299**, pinned in the bridge image. A bump is a
deliberate act followed by the M1 live proof. The bridge asserts at boot that the binary exists,
that its flag surface still accepts the flags this design depends on, and that `auth status`
reports a logged-in subscription — because `--max-turns` already showed that a flag can work while
vanishing from `--help`.

## 8. Rollback

Stopping the bridge and disabling the account (or removing the alias) restores the previous
behaviour with no schema change and no effect on any other provider. The previous bridge image tag
is the container rollback. `claude-web` and every existing account are untouched.

## 9. Locked decisions

1. The official Claude Code executable is the only driver of the subscription; no OAuth-bearer
   reuse, no client impersonation.
2. Nothing is installed into Hermes; Hermes reaches the subscription only through ezllm.
3. The surface runs **in-process** on a loopback-only listener inside the ezllm container; the CLI
   is **bind-mounted** from the host (never baked into an image) and there is no Node anywhere. The
   runtime base is `distroless/base` because the CLI is a glibc-dynamic binary (measured).
4. The ezllm side is a plain `anthropic-compatible` account — no new kind, surface or translation
   cell.
5. *(Amended 2026-10-21: agent traffic such as Hermes's Max may use it, but only behind M5 Rules caps
   on the account — sonnet and haiku 300,000 tokens per 5h block, opus 1 — set before the agent is moved.)*
   The subscription is reachable only through a dedicated combo; never a default hop, never
   in `daily`, never an automatic fallback.
6. `claude-web` stays.
7. Build order M1 → M2 → M3, M3 gated on measurement.
8. M1 refused tool-bearing requests explicitly instead of dropping tools silently. M2 honours client
   tools and keeps that rule for what it still cannot do (server tools, `parallel_tool_calls`).
9. Usage is declared EXCLUSIVE (native Anthropic convention); nothing is synthesized.
10. The login directory `~/.claude-sub` mounts read-write (`/claude-sub`) and the CLI binary mounts
    read-only (`/opt/claude/claude`, pinned to 2.1.299); `USER`/`LOGNAME` are set in the container.
11. Boot asserts the CLI's version, flag surface and login state; a CLI upgrade means re-pointing
    the mount and re-running the live proof.
12. Calypso is the only gate for push and deploy.
13. The CLI runs with an **allowlisted** environment (2026-10-19): identity, locale, temp dir, CA
    trust, proxy settings and the operator's `CLAUDE_CODE_*` / `DISABLE_*` tuning pass; everything
    else — `EZLLM_MASTER_KEY`, provider and cloud credentials, `ANTHROPIC_*` — stays out of a process
    that has no reason to see it.

## 10. M1 open items — how they closed

- Spawn semaphore: two concurrent CLI children by default (`Runner.MaxConcurrent`).
- `/v1/models` serves a static pinned list of the routes the CLI resolves its family aliases to.
- Dollars: leave the bridge account without price rows, so its calls read as unpriced rather
  than as a fake $0.00 (cost is derived at query time from `model_prices`).
