# Codex subscription runbook

Operating the ChatGPT subscription surface (codex-bridge). Decisions live in
`codex-subscription-design.md`; this is the how.

**Status:** stages 1–3 (text, streaming, images, reasoning effort and summaries,
structured output, plan windows, client tools with continuation) are built on
`feat/codex-bridge` and not in a release yet. Read the [platform requirements](#platform-requirements--read-this-first)
before you rely on tools.

## What it is

A **loopback-only OpenAI Chat Completions surface inside the ezllm process**
(`internal/codexbridge`, bound to `127.0.0.1:8791`) that drives the **official
Codex CLI** (`codex app-server`) against a ChatGPT Plus/Pro login. ezllm sees it
as a plain `openai-compatible` account. The CLI is the only thing that talks to
OpenAI — ezllm never reads or replays the ChatGPT tokens.

Codex is a coding agent. Its **own** tools (shell, file edits, browser,
sub-agents) would run on the ezllm host, next to `/data` with the master key and
every provider key, so the bridge switches them off. **Your client's tools are a
different thing and do work**: the model asks for them, the bridge hands the call
to your client as an ordinary OpenAI `tool_calls` entry, and your client runs it
on its own machine.

## Platform requirements — read this first

Client tool calling through this bridge **requires Landlock**, a Linux kernel
feature the bridge uses to stop the Codex process from writing anywhere except
its own scratch directory and login directory. Without Landlock the bridge still
runs, but **client tools are switched off** and every request that carries
`tools` is refused with a 400. This is deliberate, not a bug.

### Why

In Codex 0.163.0 a client tool can only be called from inside Codex's JavaScript
tool (`exec`), so the bridge has to leave that tool's runtime on. The runtime
itself was measured to have no file system, network or process access — but
Codex also exposes its own `apply_patch` (file-writing) tool inside it, and no
0.163.0 setting removes it. Codex's read-only policy refused every `apply_patch`
the spike tried; Landlock makes that refusal a **kernel guarantee** instead of a
promise from Codex's code. With client tools off, `exec` is disabled and
`apply_patch` is unreachable, so the guarantee holds without Landlock.
Measurements: `codex-subscription-design.md` §2.5 and §2.12.
When Landlock is available, **every** Codex child runs inside it, text-only
requests included; tools additionally switch the code-mode runtime on.

### What works where

| host | Landlock | text, streaming, all models, reasoning effort, images, structured output | client tools |
| --- | --- | --- | --- |
| Linux, kernel ≥ 5.13, Landlock enabled, container allows the syscalls | yes | ✅ | ✅ |
| Linux without Landlock (old kernel, disabled LSM, or blocked by seccomp) | no | ✅ | ❌ — 400 `chat_tools_unsupported` |
| Linux without Landlock + `EZLLM_CODEX_TOOLS_WITHOUT_LANDLOCK=on` | no | ✅ | ✅ — weaker guarantee, see below |
| macOS, Windows (native binaries) | — | ❌ bridge refuses to start | ❌ |
| Docker on Linux, **default** seccomp profile (verified 2026-10-23, kernel 6.8) | yes | ✅ | ✅ |
| Docker Desktop (Mac/Windows) | depends on its Linux VM kernel | check `/status` | check `/status` |

### Check your host

```sh
uname -r                          # needs 5.13 or newer
cat /sys/kernel/security/lsm      # must list "landlock", e.g. lockdown,capability,landlock,yama,apparmor
```

Neither command proves the **container** may use it — Docker's seccomp profile
can block the Landlock syscalls even when the kernel has them. The bridge's own
answer is authoritative: it tries Landlock at boot and reports the result.

```sh
docker logs ezllm | grep -i "codex bridge"    # boot line: landlock=… client_tools=…
```

`GET /status` on the bridge carries the same two fields:

| field | values |
| --- | --- |
| `landlock` | `available (ABI <n>)` · `unavailable: <reason from the kernel>` |
| `client_tools` | `enabled` · `disabled (no Landlock)` · `enabled without Landlock (operator override)` |

### What a client sees when tools are off

```json
{"error":{"type":"invalid_request_error","code":"chat_tools_unsupported",
 "message":"client tools are disabled on this host: Landlock is unavailable (<reason>). See docs/codex-bridge-runbook.md#platform-requirements--read-this-first"}}
```

The request is refused **before** Codex starts, so it costs no plan quota. Plain
text requests on the same account keep working. An agent that needs tools
(Hermes, Claude Code, any tool-using client) cannot use this hop until the host
has Landlock — route it to another account, or fix the host.

### Fixing a host without Landlock

- **Old kernel (< 5.13):** upgrade the kernel. There is no workaround.
- **Kernel has Landlock but it is not active** (`landlock` missing from
  `/sys/kernel/security/lsm`): add it to the kernel's `lsm=` boot parameter,
  keeping the modules already listed, e.g.
  `lsm=lockdown,capability,landlock,yama,apparmor`, then reboot. Where that
  parameter lives depends on your bootloader and distro.
- **Kernel is fine but the container is blocked:** the container's seccomp
  profile must allow `landlock_create_ruleset`, `landlock_add_rule` and
  `landlock_restrict_self`. Use a profile that adds those three syscalls. Do
  not run the container with `seccomp=unconfined` to get there: it removes
  far more protection than Landlock adds.

### The override, and what you give up

`EZLLM_CODEX_TOOLS_WITHOUT_LANDLOCK=on` turns client tools on without Landlock.
The bridge then relies **only** on Codex's own read-only policy to refuse
`apply_patch` writes. That held in every spike run, but it is Codex's code, it
can change with any CLI version, and nothing in the kernel backs it. Turn it on
only on a host where a write by the Codex process would not matter (nothing
sensitive the ezllm user can write to). It is off by default, it is logged at
boot, and `/status` shows `enabled without Landlock (operator override)` for as
long as it is set.

## Prerequisites

- **The official CLI release, pinned by path** (qualified: **0.163.0**, the
  standalone musl build). Pin a **copy of the whole release directory** outside
  `~/.codex/packages/`, which the CLI's updater manages. The directory matters:
  `bin/` holds the CLI **and** `codex-code-mode-host`, the runtime client tools
  need — copy only the CLI and tools stay off (`/status` → `client_tools:
  disabled (code-mode host missing)`).

  ```sh
  mkdir -p ~/.local/share/codex-bridge
  cp -a "$(dirname "$(dirname "$(readlink -f "$(command -v codex)")")")" ~/.local/share/codex-bridge/0.163.0
  ls ~/.local/share/codex-bridge/0.163.0/bin    # codex  codex-code-mode-host
  ```

  The bridge also starts the CLI with `--disable in_app_updates`, so a running
  bridge never updates itself. A different version still boots — the lockdown and
  protocol checks below guard it — but `/status` reports it `unqualified` until the
  live proof has been re-run against it.
- **An isolated ChatGPT login**, never the interactive `~/.codex`:

  ```sh
  mkdir -p ~/.codex-sub && chmod 700 ~/.codex-sub
  CODEX_HOME=~/.codex-sub codex login --device-auth
  CODEX_HOME=~/.codex-sub codex login status      # → Logged in using ChatGPT
  ```

  Do not copy `~/.codex/auth.json` across: two copies of one refresh token log
  each other out when either refreshes.

## Enable

In your `compose.yml` (from `compose.example.yml`): uncomment the two Codex
mounts (the release directory read-only at `/opt/codex`, the login read-write
at `/codex-sub`), set `EZLLM_CODEX_BRIDGE: "on"`, and recreate the container:

```bash
docker compose up -d
```

Boot fails loudly — the process does not start — when:

- the binary is missing (`EZLLM_CODEX_BIN`, default `/opt/codex/bin/codex`);
- the CLI refuses a lockdown flag, or reports a locked feature as still enabled
  (one exception, `unified_exec`, is explained in design §2.5);
- the app-server lacks a method **and** `codex exec` lacks a fallback flag;
- there is no login in `EZLLM_CODEX_HOME` (default `/codex-sub`);
- `EZLLM_CODEX_BRIDGE_ADDR` is not loopback (default `127.0.0.1:8791`);
- the host is not Linux.

| variable | default | meaning |
| --- | --- | --- |
| `EZLLM_CODEX_BRIDGE` | `off` | `on` starts the surface |
| `EZLLM_CODEX_BIN` | `/opt/codex/bin/codex` | the CLI inside the mounted release |
| `EZLLM_CODEX_HOME` | `/codex-sub` | the mounted login (`CODEX_HOME`) |
| `EZLLM_CODEX_BRIDGE_ADDR` | `127.0.0.1:8791` | loopback listener |
| `EZLLM_CODEX_BRIDGE_TOKEN_FILE` | `/data/codex-bridge.token` | shared secret, generated on first boot |
| `EZLLM_CODEX_BRIDGE_CONCURRENCY` | `2` | CLI children at once (a turn parked on tool calls holds none) |
| `EZLLM_CODEX_TOOLS_WITHOUT_LANDLOCK` | `off` | `on` = client tools without Landlock ([what you give up](#the-override-and-what-you-give-up)) |

```sh
docker logs ezllm | grep -i "codex bridge"   # cli version, release, login, channel
```

## Wire it into ezllm

1. **Account** (dashboard → Providers → Add):
   - kind `openai-compatible`
   - base URL `http://127.0.0.1:8791/v1`
   - key: `cat data/codex-bridge.token` (on the host; gitignored)
   - `usage_convention`: leave `auto` — it describes the Messages wire only; this
     surface speaks Chat Completions, whose `prompt_tokens` already include
     `prompt_tokens_details.cached_tokens`
   - Sync the catalog (every model the login serves, hidden ones included), then **Test**.
2. **Combo / fallback chains:** allowed (operator decision). Cap the account with
   **Rules** before agents use it — the bridge itself has no cap.

## Client tools

Send ordinary OpenAI `tools`. What happens behind the bridge:

1. The client's function tools are declared to Codex as dynamic tools. In 0.163.0
   the model calls them from inside its code-mode JavaScript
   (`await tools.get_weather({city: "KL"})`), which runs in a V8 isolate with no
   file system, network or process access, inside the Landlock domain.
2. Each call comes back to the client as a normal `tool_calls` entry,
   `finish_reason: "tool_calls"`. Calls the script makes together
   (`Promise.all`) arrive together in one response.
3. **Continuation:** the Codex child stays alive with the script paused on the
   pending calls. Your next request — the usual one carrying the `tool` messages,
   matched by `tool_call_id` — answers those calls, and the **same script
   resumes**, its variables intact. A script that awaits one result before making
   the next call simply stops twice.
4. A paused turn is held for **10 minutes**, at most **8** at once (the oldest is
   dropped). A follow-up that finds nothing held — expired, dropped, or the bridge
   restarted — still works: the bridge replays the conversation, calls and
   results included, as a transcript on a fresh child. Correct, but slower and
   without the paused script's state. `/status` → `parked_tool_turns` shows how
   many are waiting. Shutting ezllm down reaps every paused turn's child.
5. A request's 300-second budget covers the wait for a free slot as well as the
   run (a full bridge answers 504 instead of queueing forever). Cancelling a
   request ends its child even when the CLI or the client has stopped reading;
   a stream write the client leaves undrained for 60 seconds ends the request.

Mappings and limits:

| OpenAI | Codex |
| --- | --- |
| `tools` (function) | dynamic tools; other tool types → 400 |
| `tool_choice: "auto"` | default |
| `tool_choice: "none"` | no tools declared |
| `tool_choice: "required"` / `{function: {name}}` | an instruction to call (Codex has no forced choice; best effort) |
| `parallel_tool_calls: false` | an instruction to call one at a time (best effort) |
| legacy `functions` | 400 — send `tools` |

Usage attribution: Codex reports a sampling's tokens when the sampling ends, and
the sampling that makes a tool call ends only after the result comes back. So the
response carrying `tool_calls` can report 0 tokens and the follow-up carries the
whole turn. The sum is exact; the split between the two requests is not.

## Structured output and reasoning

| request | what the bridge does |
| --- | --- |
| `response_format: {type: "json_schema", json_schema: {schema}}` | the schema constrains the final message (`turn/start outputSchema`; `--output-schema` on the exec fallback) |
| `response_format: {type: "json_object"}` | an instruction to answer with one JSON object — no schema exists to enforce |
| `reasoning_effort` (or `reasoning.effort`) | the turn's effort; `minimal`/`none` → `low`; anything the model does not offer → 400 naming what it does |
| `reasoning.summary`: `auto` (default) · `concise` · `detailed` · `none` | Codex's reasoning summaries stream as `reasoning_content` (chat delta and message), before the answer |

Summaries appear only when the model reasons enough to summarize: a trivial
prompt at low effort produces none. `max_tokens`, `temperature`, `top_p` and
`stop` have no Codex equivalent and are ignored.

There is no `/v1/responses` surface: Responses clients reach this account
through ezllm's router translation, like any other Chat Completions account.

## Plan windows on the provider card

Press **Quota** on the account: the bridge answers `GET /v1/usage` from
`account/rateLimits/read` — **rolling** = the 5-hour window, **weekly** = the
7-day window, both as Codex reports them. The read starts a CLI child for about
0.2 s and spends no plan quota. These are OpenAI's numbers for the whole
subscription (ChatGPT and other Codex use included), not ezllm's ledger and not
the Rules caps.

## Verify

```sh
EZLLM_CODEX_LIVE=1 go test -count=1 -run Live ./internal/codexbridge/   # real plan draw, ~5 calls
python3 tests/prove_codex_bridge.py                                     # through ezllm's front door
```

## Failure modes

| symptom | cause | fix |
| --- | --- | --- |
| 429 `rate_limit` naming a usage limit | the plan window is spent | wait for `resetsAt` on the card; a combo skips the hop |
| 401 `unauthorized` | the login expired or was revoked | `CODEX_HOME=~/.codex-sub codex login --device-auth` |
| 400 `model_not_found` | the model is not on this login | Sync the catalog |
| 400 `chat_tools_unsupported` | tools are off on this host: no Landlock, the code-mode host is missing, or the exec fallback is active | `/status` → `landlock`, `client_tools`, `channel`; see the platform requirements |
| a tool follow-up is slow and the model "forgets" what it was doing | the paused turn expired (10 min) or was dropped (8 held) and the bridge replayed | answer tool calls promptly; check `parked_tool_turns` |
| 503 `binary_missing` | the mount is gone | re-copy the pinned binary, recreate |
| 502 `native_closed` | the CLI died mid-answer | `docker logs ezllm`; the message carries the CLI's stderr |
| answers arrive in one chunk, `/v1/models` 404 | the bridge is on the **exec fallback** (`/status` → `channel`) | the CLI release dropped an app-server method; pin the qualified release |

## Rollback

Set `EZLLM_CODEX_BRIDGE=off` (or disable the account) and recreate. No schema
change, nothing else is touched.

## Common confusion

- **"Codex can't run commands / edit my files."** By design: Codex's own tools
  are off on every host. The bridge serves the **model**; your client is the
  agent and runs its own tools.
- **"Tool calls fail with `chat_tools_unsupported`."** The host has no
  Landlock. Check `/status` → `landlock`, then see
  [fixing a host](#fixing-a-host-without-landlock).
- **"It works on my laptop but not on the server."** Different kernels or
  different container seccomp profiles. Compare `/status` on both.
- **"The bridge won't start on macOS / Windows."** It is a Linux feature; run
  ezllm in a Linux container.
