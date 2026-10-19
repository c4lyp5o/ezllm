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

- The official CLI binary, pinned by path (qualified: **2.1.299**):
  `/home/calypso/.local/share/claude/versions/2.1.299`.
- A subscription login in its own config dir, never the interactive one:

  ```sh
  CLAUDE_CONFIG_DIR=~/.claude-sub claude auth login
  CLAUDE_CONFIG_DIR=~/.claude-sub claude auth status   # loggedIn + subscriptionType
  ```

- Nothing else: the account's shared secret **auto-generates** into
  `./data/claude-bridge.token` (0600) on first boot and lives on the data volume,
  so it is backed up with the ledger.

## Enable

`compose.yml` already wires it: the CLI mounts read-only at `/opt/claude/claude`,
the login read-write at `/claude-sub`, `USER`/`LOGNAME` are set (the CLI's login
check reads process identity), and `EZLLM_CLAUDE_BRIDGE=on` turns the surface on.

```sh
docker compose -f compose.yml -f compose.traefik.yml up -d
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
2. **Combo** — one hop, this account, and *only* Claude Code clients may use it.
   Never add it to `daily`, never make it an automatic fallback: the plan's real
   limit is a shared rolling window the router cannot observe.

## Verify

```sh
python3 tests/prove_claude_bridge.py     # through ezllm's front door; real plan draw
```

## Failure modes

- **401** — no usable login where the CLI runs it. Check `USER`/`LOGNAME` are set
  and `/claude-sub/.credentials.json` exists and is readable by the container uid.
- **429 `rate_limit_error`** — the plan's window is exhausted (or overage is off and
  the window is full). Wait for the reset; the CLI's own words are in the body.
- **400 `max_output_tokens`** — the request's `max_tokens` is smaller than the
  model's thinking budget needs. Raise it (256+ is comfortable for Haiku 5.5).
- **400 `chat_tools_unsupported`** — M1 is text-only; tools arrive in M2.
- **Boot: "does not resolve to the official Claude Code CLI"** — the
  `CLAUDE_CLI_BIN` mount is missing or points at nothing.

## Rollback

`EZLLM_CLAUDE_BRIDGE=off` and recreate the container, then disable the account (or
delete the combo) in the dashboard. No schema, no other provider, and no client
configuration is affected.
