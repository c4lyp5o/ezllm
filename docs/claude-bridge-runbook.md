# Claude subscription runbook

How the Claude bridge behaves, its limits and its caps. **To turn it on, follow
[Turn on Claude](bridges/claude.md).** Design: `design/claude-subscription-design.md`.

## What it is

A **loopback-only Anthropic Messages surface inside the ezllm process**
(`internal/claudebridge`, bound to `127.0.0.1:8790`) that drives the **official
Claude Code CLI** against a Claude subscription login. ezllm sees it as a plain
`anthropic-compatible` account. The CLI is the only thing that talks to Anthropic —
nothing else ever holds the subscription credential. One container, no sidecar.

## Boot and CLI releases

With the bridge on, a missing CLI or an unusable login directory **fails the boot
loudly**: this process would otherwise look healthy while every subscription request
fails. The bind is loopback-enforced. The account's shared secret generates itself into
`data/claude-bridge.token` (0600) on first boot.

Run a pinned **copy** of the CLI, as the setup page does, never a file under
`~/.local/share/claude/versions/`: the CLI's auto-updater deletes old versions there, and
the next container recreate then fails the boot.

`ezllm bridge-check claude` runs the boot checks against a staged CLI and exits non-zero
on any problem, stricter than boot, which reports flag drift and keeps serving. `--json` prints the verdict (`ok`, `version`, `release`, `live`,
`problems`) for the [updater](bridge-update-runbook.md). `--live` then drives real turns
through the bridge (plan quota, the shared login), only when every static check passed.
A version the updater proved is listed in `qualified.json` in the bridge directory, and
boot reports it qualified; any other version still boots and is reported `unqualified`.

## Wire it into ezllm

1. **Account** (dashboard → Providers → Add provider → **Claude Pro/Max (bridge)**).
   The preset fills in everything below, shows the bridge's CLI version, login
   and plan, and supplies the key itself — Save & validate key is the whole step.
   By hand (API):
   - kind `anthropic-compatible`
   - base URL `http://127.0.0.1:8790/v1`
   - key: `cat data/claude-bridge.token` (on the host; the file is gitignored)
   - `usage_convention`: `anthropic` (cache excluded from `input_tokens`, matching
     the native envelope the surface emits)
   - Sync the catalog, then **Test** — expect the `claude-haiku-5-5` probe to pass.
2. **Combo** — one hop, this account. Never add it to a combo your
   apps use by default, never make it an automatic fallback: the plan's real limit is a shared rolling window the router
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

## Tools

Client tools pass through to upstream unchanged and come back as `tool_use` under
their own names; nothing runs inside the bridge or the CLI. The client executes its
tools and sends `tool_result`s on the next request, as with any Messages endpoint.
OpenAI chat clients work through ezllm's normal translation. Only the newest
two `cache_control` markers survive (native spends two of upstream's four).

## Verify

For a simple first check, use the gateway's [model-list command](../README.md#2-finish-setup-in-the-dashboard)
and select a model from this account. Send a short prompt through the dashboard
or your client. Requests consume subscription quota.

The repository's optional live proof makes several calls and requires an inference
key. The script defaults to `http://127.0.0.1:20129`. Replace the model ID with one from your account:

```sh
EZLLM_URL=http://127.0.0.1:20129 EZLLM_TOKEN=YOUR_EZLLM_KEY \
  python3 tests/prove_claude_bridge.py YOUR_NAMESPACE/YOUR_MODEL
```

Run this only against a test instance you control; the optional ledger check uses
`EZLLM_ADMIN_TOKEN`.

## Plan windows on the provider card

Every CLI run reports the plan's state in a `rate_limit_event`: the 5-hour and
7-day windows as a utilization fraction plus reset times. The bridge keeps the
last reading and serves it on `GET /v1/usage` (same `x-api-key`) in the percent
shape the Providers page already draws — **rolling** = the 5-hour window,
**weekly** = the 7-day window. Press **Quota** on the claude-sub account to read it.

- **Fresh read (optional):** paste your claude.ai Cookie header into the account's
  `management_key` (it must contain `sessionKey` and `lastActiveOrg`; keep the
  `cf_clearance`/`__cf_bm` cookies — Cloudflare challenges a bare session). **Quota**
  then reads `GET claude.ai/api/organizations/{org}/usage` (`five_hour` → rolling,
  `seven_day` → weekly, `utilization` already a percent) without spending a request,
  and falls back to the bridge reading below if that read fails (expired cookie,
  challenge). `cf_clearance` expires; re-paste when the card goes back to `observed_at`.
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
  On macOS the login is in the Keychain instead of that file: run ezllm in the desktop
  session of the user who signed in (SSH cannot open the Keychain).
- **429 `rate_limit_error`** — the plan's window is exhausted (or overage is off and
  the window is full). Wait for the reset; the CLI's own words are in the body.
- **400 `max_output_tokens`** — the request's `max_tokens` is smaller than the
  model's thinking budget needs. Raise it (256+ is comfortable for Haiku 5.5).
- **400 `chat_tools_unsupported`** — the request carried a server tool
  (`web_search_*`, `code_execution_*`, …), an OpenAI-shaped tool on `/v1/messages`,
  or `parallel_tool_calls`. The message names the field; send client tools only.
- **Boot: "does not resolve to the official Claude Code CLI"** — the
  `/opt/claude-bridge` mount in your `compose.yml` is missing, or its `current`
  link points at nothing.

## Rollback

`EZLLM_CLAUDE_BRIDGE=off` and recreate the container, then disable the account (or
delete the combo) in the dashboard. No schema, no other provider, and no client
configuration is affected.
