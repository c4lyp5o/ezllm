# claude-bridge runbook

Operating the Claude subscription sidecar. Decisions live in
`claude-subscription-design.md`; this is the how.

## What it is

A container that serves an Anthropic Messages surface by driving the **official
Claude Code CLI** against a Claude subscription login. ezllm sees it as a plain
`anthropic-compatible` account. The CLI is the only thing that talks to
Anthropic — the bridge never holds the subscription credential.

## Prerequisites

- The official CLI binary, pinned (qualified: **2.1.299**). On this host:
  `/home/calypso/.local/share/claude/versions/2.1.299`.
- A subscription login in its own config dir, never the interactive one:

  ```sh
  CLAUDE_CONFIG_DIR=~/.claude-sub claude auth login
  CLAUDE_CONFIG_DIR=~/.claude-sub claude auth status   # loggedIn + subscriptionType
  ```

- A shared secret for ezllm:

  ```sh
  openssl rand -hex 24 > data/claude-bridge.token && chmod 600 data/claude-bridge.token
  ```

## Bring it up

```sh
docker compose -f compose.yml -f compose.claude-bridge.yml up -d
docker logs claude-bridge | head -5        # boot line reports cli version + login + plan
```

Boot refuses to serve without a login (`claude auth login` hint in the log) and
warns when `--help` no longer lists a flag the design depends on. No ports are
published: only containers on the compose network can reach it.

## Wire it into ezllm

1. **Account** (dashboard → Providers → Add):
   - kind `anthropic-compatible`
   - base URL `http://claude-bridge:8790` (must end in `/v1` per normalization;
     use `http://claude-bridge:8790/v1`)
   - key: the contents of `data/claude-bridge.token`
   - `usage_convention`: `anthropic` (cache excluded from `input_tokens`, matching
     the native envelope the bridge emits)
   - Sync the catalog, then **Test** — expect the `claude-haiku-5-5` probe to pass.
2. **Combo** — one hop, this account, and *only* Claude Code clients may use it.
   Never add it to `daily`, never make it an automatic fallback: the plan's real
   limit is a shared rolling window the router cannot observe.

## Verify

```sh
python3 tests/prove_claude_bridge.py     # 13 checks, spends a few Haiku calls
```

## Failure modes

- **401** — no usable login where the CLI runs it. Check `USER`/`LOGNAME` are set
  and `~/.claude-sub/.credentials.json` exists and is readable.
- **429 `rate_limit_error`** — the plan's window is exhausted (or overage is off
  and the window is full). Wait for the reset; the CLI's own words are in the body.
- **400 `max_output_tokens`** — the request's `max_tokens` is smaller than the
  model's thinking budget needs. Raise it (256+ is comfortable for Haiku 5.5).
- **400 `chat_tools_unsupported`** — M1 is text-only; tools arrive in M2.
- **503 `claude_missing`** — the mounted CLI binary is gone or not executable.

## Rollback

Disable the account (or delete the combo) in ezllm, then
`docker compose -f compose.yml -f compose.claude-bridge.yml down`. No schema, no
other provider, and no client configuration is affected.
