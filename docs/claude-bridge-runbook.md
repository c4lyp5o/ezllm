# Claude subscription runbook

Operating the Claude subscription surface. Decisions live in
`claude-subscription-design.md`; this is the how.

## What it is

A **loopback-only Anthropic Messages surface inside the ezllm process**
(`internal/claudebridge`, bound to `127.0.0.1:8790`) that drives the **official
Claude Code CLI** against a Claude subscription login. ezllm sees it as a plain
`anthropic-compatible` account. The CLI is the only thing that talks to Anthropic —
nothing else ever holds the subscription credential. One container, no sidecar.

## Prerequisites

- The official CLI binary, pinned by path (qualified: **2.1.306**):
  `/home/calypso/.local/share/claude-bridge/claude-2.1.306`. Pin a **copy**, never
  a file under `~/.local/share/claude/versions/`: the CLI's auto-updater deletes old
  versions there, and a deleted file under a live bind mount keeps working only
  until the next container recreate, which then fails the boot (2.1.299 was lost
  this way, 2026-10-21).

  ```sh
  mkdir -p ~/.local/share/claude-bridge
  cp ~/.local/share/claude/versions/<ver> ~/.local/share/claude-bridge/claude-<ver>
  # then point the mount in your compose.yml at it and re-run the live proof
  ```
- A subscription login in its own config dir, never the interactive one:

  ```sh
  CLAUDE_CONFIG_DIR=~/.claude-sub claude auth login
  CLAUDE_CONFIG_DIR=~/.claude-sub claude auth status   # loggedIn + subscriptionType
  ```

- Nothing else: the account's shared secret **auto-generates** into
  `./data/claude-bridge.token` (0600) on first boot and lives on the data volume,
  so it is backed up with the ledger.

## Enable

In your `compose.yml` (from `compose.example.yml`): uncomment the two Claude
mounts (the CLI read-only at `/opt/claude/claude`, the login read-write at
`/claude-sub`), set `USER`/`LOGNAME` to the login's owner (the CLI's login check
reads process identity), and set `EZLLM_CLAUDE_BRIDGE: "on"`.

```sh
docker compose up -d
docker logs ezllm | grep -i "claude bridge"   # cli version + login + plan + flag drift
```

On a host without a CLI + login, set `EZLLM_CLAUDE_BRIDGE=off`. Enabling it without
either **fails the boot loudly** — this process would otherwise look healthy while
every subscription request fails. The bind is loopback-enforced: a non-loopback
address is refused.

## Wire it into ezllm

1. **Account** (dashboard → Providers → Add):
   - kind `anthropic-compatible`
   - base URL `http://127.0.0.1:8790/v1`
   - key: `cat data/claude-bridge.token` (on the host; the file is gitignored)
   - `usage_convention`: `anthropic` (cache excluded from `input_tokens`, matching
     the native envelope the surface emits)
   - Sync the catalog, then **Test** — expect the `claude-haiku-5-5` probe to pass.
2. **Combo** — one hop, this account. Never add it to `daily`, never make it an
   automatic fallback: the plan's real limit is a shared rolling window the router
   cannot observe.
3. **Drain cap (Rules) — required before any agent uses the combo.** On the Rules
   page, for the claude-sub account:

   | model | cap_tokens | cap_window |
   | --- | --- | --- |
   | `claude-sonnet-5-5` | 300,000 | `5h` |
   | `claude-haiku-5-5` | 300,000 | `5h` |
   | `claude-opus-5-5` | 1 | `5h` |

   Rules count cache-cold input plus output (cache reads excluded). A measured agent
   turn counts about 300–1,000 tokens early in a session and more as history grows,
   so 300,000 is roughly 300+ short turns, about half the plan's window
   (design §2.1). The 5h block is calendar-aligned (00–04, 05–09, …), not
   Anthropic's rolling window. Over cap, the hop answers 429 naming the rule. Opus
   stays out of agent combos; its cap of 1 is a backstop, since one call still
   passes per block.

## Tools (M2)

Client tools pass through to upstream unchanged and come back as `tool_use` under
their own names; nothing runs inside the bridge or the CLI. The client executes its
tools and sends `tool_result`s on the next request, as with any Messages endpoint.
OpenAI chat clients (Hermes) work through ezllm's normal translation. Only the newest
two `cache_control` markers survive (native spends two of upstream's four).

## Verify

```sh
python3 tests/prove_claude_bridge.py     # through ezllm's front door; real plan draw
```

## Plan windows on the provider card

Every CLI run reports the plan's state in a `rate_limit_event`: the 5-hour and
7-day windows as a utilization fraction plus reset times. The bridge keeps the
last reading and serves it on `GET /v1/usage` (same `x-api-key`) in the percent
shape the Providers page already draws — **rolling** = the 5-hour window,
**weekly** = the 7-day window. Press **Quota** on the claude-sub account to read it.

- The reading is only as fresh as the **last call** (`observed_at` in the raw
  body); the CLI has no way to read it without making one.
- Before the first call after a restart the bridge answers 404 and the card shows
  `–` ("measured"). The first request through the account fills it in.
- These are Anthropic's own numbers for the whole subscription, including use
  from claude.ai and other devices — they will not match ezllm's ledger, and
  they are not the Rules caps (those count only ezllm traffic, in calendar blocks).

## Failure modes

- **401** — no usable login where the CLI runs it. Check `USER`/`LOGNAME` are set
  and `/claude-sub/.credentials.json` exists and is readable by the container uid.
- **429 `rate_limit_error`** — the plan's window is exhausted (or overage is off and
  the window is full). Wait for the reset; the CLI's own words are in the body.
- **400 `max_output_tokens`** — the request's `max_tokens` is smaller than the
  model's thinking budget needs. Raise it (256+ is comfortable for Haiku 5.5).
- **400 `chat_tools_unsupported`** — the request carried a server tool
  (`web_search_*`, `code_execution_*`, …), an OpenAI-shaped tool on `/v1/messages`,
  or `parallel_tool_calls`. The message names the field; send client tools only.
- **Boot: "does not resolve to the official Claude Code CLI"** — the
  `/opt/claude/claude` mount in your `compose.yml` is missing or points at nothing.

## Rollback

`EZLLM_CLAUDE_BRIDGE=off` and recreate the container, then disable the account (or
delete the combo) in the dashboard. No schema, no other provider, and no client
configuration is affected.
