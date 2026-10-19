# Claude subscription via the official Claude Code CLI — design

**Status:** signed off 2026-10-19 (implementation not started)
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
7. **Process startup is ~1.5–2s** of the 3.2–6.4s wall (api time 1.2–1.8s). Accepted in v1.
8. **Opus is reachable on Pro** (no usage-credit refusal) at $0.10 list-equivalent for a trivial
   call — 40x the haiku figure. It stays out of every automatic path.

## 3. Architecture: sidecar bridge, ezllm core untouched

```
client (Claude Code / Hermes / VS Code)
   └─> ezllm  ── combo claude-cc ──> account "claude-sub"
                                        │  kind: anthropic-compatible
                                        │  base_url: http://claude-bridge:8790
                                        │  key: bridge shared secret
                                        v
                                  claude-bridge (new container)
                                        │  spawns: claude -p --input-format stream-json …
                                        v
                                  api.anthropic.com  (subscription plan)
```

**Bridge container.** Owns the CLI binary and the subscription login. Exposes an
Anthropic-shaped HTTP surface on the compose network only (never published):

- `POST /v1/messages` — the inference path.
- `GET /v1/models` — the catalog the account syncs and the key-test oracle.
- `GET /healthz` — binary present, login state, plan type (for the operator, not the router).

Container requirements: the documented `USER` / `LOGNAME` environment (the CLI's local login check
reads process identity; without it a logged-in CLI reports `loggedIn: false` instantly with no
upstream request), the `~/.claude-sub` login directory mounted **read-write** (the CLI refreshes
its own token there), CPU/memory limits, and a pinned CLI version baked into the image.

**ezllm side: no new code path.** It is an ordinary `anthropic-compatible` account — the existing
adapter already speaks `/v1/messages` with `x-api-key`, the existing usage tap reads the native
Anthropic envelope, and native SSE passes through byte-for-byte with no translation cell. Nothing
in `internal/router`, `internal/proxy` or `internal/translate` needs a new concept.

**Rejected alternatives.** Baking Node + the CLI into the ezllm image (drags a JS runtime and a
version-coupled subprocess supervisor into the public gateway, and forces an image rebuild per CLI
release); running a Hermes instance as a CLI-driving proxy and dialing it as an OpenAI-compatible
upstream (two agent harnesses stacked in the request path).

## 4. Bridge request contract

Per request: a private temporary `HOME`/config directory, `--settings` pointing at a bridge-owned
settings file, `--strict-mcp-config` plus a bridge-owned MCP config, `--max-turns 1`,
`--include-partial-messages`, model alias passed through unchanged (`sonnet`, `haiku`, `opus`, and
the `[1m]` forms the CLI advertises).

- **Streaming:** `stream_event` text deltas → Anthropic `content_block_delta`; final `result` frame
  → terminal usage; `rate_limit_event` → the plan state a 429 should carry.
- **Usage:** the native envelope is already Anthropic-shaped (measured `input_tokens=2`,
  `cache_read_input_tokens=8609`, `output_tokens=4`, i.e. cache **excluded** from `input_tokens`).
  The bridge account therefore declares `usage_convention=anthropic` (the vocabulary is
  `auto|anthropic|total`) so `tokens_in` means cache-COLD spend and `tokens_cached_read` is real.
  The router only *requires* an explicit convention for a **translated** Messages destination
  (`internal/router/router.go:362` and `:399`, reason string `messagesConventionReason`); our hop
  serves Messages natively, so this is declared for honest accounting rather than to pass a gate.
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

**M1 — transport, text only.** Bridge container + `/v1/messages` (stream and non-stream) + usage and
error mapping + `/v1/models` + the ezllm `anthropic-compatible` account + the Claude-Code-only combo.
Tool-bearing requests are **refused explicitly** with the existing `chat_tools_unsupported` cause
rather than silently answered without tools. Live proof: a real request through ezllm's front door
returns 200 and lands a ledger row with real token counts, `tokens_cached_read > 0` and the correct
account.

**M2 — tools.** An in-bridge MCP server advertises the *request's* tools as `mcp__client__<name>`
(stable 50-char hash alias above the length limit) with deny-execution, so the CLI emits the call
without running anything; the bridge captures the `tool_use` block and maps it back to the client's
original names. The CLI's own tools stay denied (`--disallowedTools '*'`), which also keeps the
harness overhead near the 6.1k-token floor.

**M3 — cache/replay normalization.** Only if M2 measurements show poor cache reads on long
agent-style conversations. The mechanics to port (replaying the native date line, stripping the
context reminder before the newest user frame, relocating cache markers without laundering host
content, one upstream request per call) are documented in the Hermes plugin, MIT-licensed, and
credited.

Each milestone: focused in-package tests, a live proof through the router, and the repository gate
(`make test-short` under the memory cap, `make check` on the uncapped shell or CI).

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
3. The CLI lives in a **separate bridge container**; no Node (or CLI) inside the ezllm image.
4. The ezllm side is a plain `anthropic-compatible` account — no new kind, surface or translation
   cell.
5. The subscription is reachable only through a Claude-Code-only combo; never a default hop, never
   in `daily`, never an automatic fallback.
6. `claude-web` stays.
7. Build order M1 → M2 → M3, M3 gated on measurement.
8. M1 refuses tool-bearing requests explicitly instead of dropping tools silently.
9. Usage is declared EXCLUSIVE (native Anthropic convention); nothing is synthesized.
10. The login directory `~/.claude-sub` is mounted into the bridge read-write; `USER`/`LOGNAME` are
    set in the container.
11. The bridge image pins CLI 2.1.299 and asserts its flags and login at boot.
12. Calypso is the only gate for push and deploy.

## 10. Open items for M1

- Spawn semaphore default (measured cold-start cost suggests a small number).
- Whether `/v1/models` serves the CLI's live picker via the offline `initialize` handshake or a
  static pinned list for the first cut.
- Whether subscription hops carry any dollar representation in the dashboard. Cost is derived at
  query time from the versioned `model_prices` table, and unpriced calls are **counted, not
  zeroed** — so the honest default is to leave the bridge account (and its models) with **no price
  rows**, which makes those calls read as unpriced rather than as a fake $0.00. To confirm at M1
  against the live cost panel.
