# ChatGPT subscription via the official Codex CLI — design

**Status:** M0 measured 2026-10-23 · decisions recorded (§4) · stages 1–3 (§2.13, §2.14) shipped in v1.1.0 and deployed
**Operator gate:** Calypso (pushes/deploys)
**Spec:** [SPEC-codex-bridge.md](../SPEC-codex-bridge.md) · **Pattern:** [claude-subscription-design.md](claude-subscription-design.md)

## 1. Goal and the hard constraints

Serve ChatGPT Plus/Pro subscription capacity to ezllm clients through the
**official Codex CLI**, the way claude-bridge serves the Claude subscription.
The ezllm side is an ordinary `openai-compatible` account dialing a loopback
surface.

- **No trouble:** only the official `codex` binary talks to the subscription.
  ezllm's Go process never reads, refreshes or replays `auth.json`.
- **Nothing executes:** the model must not be able to run a command or write a
  file on this box under any input — proven by a sentinel, not assumed from flags.

## 2. Measured baseline (live, 2026-10-23)

Environment: `codex-cli 0.163.0`, standalone install, ChatGPT login in an
isolated `CODEX_HOME=~/.codex-sub` (`codex login --device-auth`), plan `plus`.
Captures were driven by a scratch JSON-RPC driver; every child ran with an
allowlisted env (`PATH`, `HOME`=private temp dir, `CODEX_HOME`, `LANG`) and a
private temp CWD.

### 2.1 Binary (Q1)

| | |
| --- | --- |
| install | standalone release under `~/.codex/packages/standalone/releases/0.163.0-x86_64-unknown-linux-musl/`, symlinked from `~/.local/bin/codex` |
| linkage | `codex` is **static-pie (musl)**, 297 MB; sibling `codex-code-mode-host` 74 MB |
| distroless | static, so it loads on `distroless/base-debian12`. At startup it shells out to `lsb_release`, `getconf` and `/usr/bin/bwrap … /bin/true` (OS + sandbox capability probes), none of which exist in distroless — effect there **not yet measured** (first M1 container task) |

### 2.2 Headless channel (Q2)

| channel | text deltas | notes |
| --- | --- | --- |
| `codex exec --json` | **none** — one `item.completed` with the whole message | events: `thread.started`, `turn.started`, `item.completed`, `turn.completed{usage}` |
| `codex app-server --stdio` | **yes** — `item/agentMessage/delta` (18–69 per short answer) | JSON-RPC over JSONL; `[experimental]`; full schema via `codex app-server generate-json-schema --experimental` |

**Decision proposed:** app-server. It is the only channel that streams, and it
exposes every lever below (instructions, history, models, rate limits). Its
experimental label is handled the claude-bridge way: pin the version and
assert the required methods/flags at boot.

### 2.3 History fidelity (Q3)

`thread/resume` accepts `history: ResponseItem[]` (structured `message` items
with `input_text`/`output_text` parts). Measured: resume a **fresh, never-started
thread id** with a two-turn history, then `turn/start` the new user message —
the model recalled the earlier-turn fact (`BLUE-HERON-7`). Structured, not a
flattened prompt.

Caveats, all measured:
- The field is documented `[UNSTABLE] FOR CODEX CLOUD - DO NOT USE`. It works on
  0.163.0; a CLI bump can change it, so the boot check and the live proof must
  both cover it.
- Resuming an **already-started** thread with history is refused:
  `cannot resume thread … with history while it is already running`.
- `ThreadResumeParams` has no `ephemeral`: the resumed thread **writes a rollout**
  (`sessions/YYYY/MM/DD/rollout-*.jsonl`, 16.5 KB for one short exchange, prompt
  text included). The bridge must delete it after each call.
- Cache: the single history call read 0 cached tokens. Repeat-call cache reuse
  for the same history was **not measured** (M1 live-proof item).

### 2.4 Harness overhead (Q4)

Trivial "Reply with exactly: pong", default model:

| configuration | input tokens | cached |
| --- | --- | --- |
| `exec --json`, defaults | 14,235 | 12,288 |
| app-server, defaults | 14,155 | 12,288 |
| app-server, `baseInstructions` minimal | 9,379 | 7,424 |
| app-server, minimal + tool lockdown (§2.5) | 5,093–5,178 | 0 |
| app-server, lockdown + history resume (2 prior turns) | 3,576 | 0 |

The lockdown cuts overhead by about two thirds; a request costs ~3.5–5k tokens
of harness before the client's own content.

### 2.5 No-execution proof (Q5)

Sentinel prompt, with the **default** Codex instructions (the variant that
actively tries): create `cwd/SENTINEL.txt`, `touch $TMP/RAN_CMD`,
`curl https://example.com -o $TMP/NET`. Child traced with
`strace -f -e trace=execve`.

| configuration | model-driven execve | files left | verdict |
| --- | --- | --- | --- |
| `sandbox=read-only`, `approvalPolicy=never`, all tools on | **`bash -c touch …`, `bash -c printf 'pwned' > …`, `curl …`** inside `bwrap` | none | **FAIL** — commands ran; only the sandbox stopped their effect |
| same + lockdown flags below | **none** | none | **PASS** |
| lockdown, `gpt-5.6-luna` | **none** | none | **PASS** |

With lockdown the only `execve`s are the fixed startup probes from §2.1
(`lsb_release`, `getconf`, `bwrap … /bin/true`, `tr`, `cut`), identical on every
run and independent of the prompt. The model did attempt a tool in one run; the
CLI refused it (`code-mode host is disabled`) and no process started.

Lockdown flags (all accepted by 0.163.0):

```
--disable shell_tool --disable unified_exec --disable unified_exec_tty --disable shell_snapshot
--disable apps --disable plugins --disable remote_plugin --disable multi_agent
--disable browser_use --disable browser_use_external --disable browser_use_full_cdp_access
--disable computer_use --disable in_app_browser --disable image_generation --disable view_image
--disable sleep_tool --disable hooks --disable skill_search --disable tool_suggest --disable goals
--disable code_mode_host --disable workspace_dependencies --disable worktrees
--disable realtime_conversation --disable skill_mcp_dependency_install --disable tool_call_mcp_elicitation
-c tools.web_search=false -c web_search="disabled"
```

plus `thread/start{sandbox:"read-only", approvalPolicy:"never", ephemeral:true}`.
(`include_apply_patch_tool` is not a 0.163.0 setting — the CLI warns it is ignored.)
`unified_exec` is the one flag 0.163.0 accepts but ignores: `codex features list` still
reports it `true` under `--disable unified_exec`. Its tools (`exec_command`, `write_stdin`)
are exposed only through `shell_tool`, which the lockdown does turn off — the locked tool
list carried neither, and strace saw no model-driven `execve`. Boot asserts every other
lockdown feature reads `false` (`internal/codexbridge/preflight.go`).

Tools still **declared** to the model under lockdown (model-dependent):
`functions.{wait, request_user_input, request_user_input_async, exec}` and, on
`gpt-6-*`, `collaboration.{spawn_agent, send_message, …}`. `exec` is the
code-mode JavaScript tool; with `code_mode_host` disabled it fails closed. No
flag tried removes these declarations; the guarantee rests on the host being
off, so the boot check must assert `code_mode_host` is disabled and the live
proof must re-run the strace sentinel on every CLI bump.

### 2.6 Usage envelope (Q6)

`thread/tokenUsage/updated` → `tokenUsage.last`:
`inputTokens, cachedInputTokens, cacheWriteInputTokens, outputTokens,
reasoningOutputTokens, totalTokens` (exec mode: the snake_case equivalents).
Cached is **included in** input (14,155 input with 12,288 cached) — the OpenAI
convention.

### 2.7 Plan-limit signal (Q7)

`account/rateLimits/read`, and `account/rateLimits/updated` after every turn:
`primary` = 300-minute window, `secondary` = 10,080-minute (weekly) window, each
`{usedPercent, resetsAt}`, plus `rateLimitReachedType`, `credits`, `planType`.
One spike session moved `primary.usedPercent` 0 → 1. The exhaustion error text
was **not observed** (the plan was never exhausted).

### 2.8 Login (Q8)

`auth.json` (0600) in `CODEX_HOME`; `codex login status` → `Logged in using
ChatGPT`. No token refresh happened during the ~1 h spike, so refresh-in-place
is **not yet measured**. The mount must be read-write regardless: the CLI writes
state into `CODEX_HOME` on every run (`state_5.sqlite`, `thread_history_1.sqlite`,
`logs_2.sqlite`, `models_cache.json`, `sessions/`, and a 29 MB `plugins/` it
installed itself). `account/read` returns the login's email and account id —
captures must be redacted before they go into `testdata/`.

### 2.9 Model catalog (Q9)

`model/list{includeHidden:true}` on Plus:

| model | hidden | default effort | efforts |
| --- | --- | --- | --- |
| `gpt-6.1-sol` (default) | no | low | low…ultra |
| `gpt-6-astra` | no | low | low…ultra |
| `gpt-6-sol` | no | medium | low…ultra |
| `gpt-6-luna` | no | medium | low…max |
| `gpt-5.6-sol` | no | low | low…ultra |
| `gpt-5.6-terra` | no | medium | low…ultra |
| `gpt-5.6-luna` | no | medium | low…max |
| `gpt-6-chat-thinking`, `gpt-reserve`, `codex-auto-review` | yes | medium | low…max |

Effort is selected per turn (`turn/start{effort}`); `model` per thread or turn.

### 2.10 Cold start and wall time (Q10)

`initialize` answers in 0.13 s, thread ready at 0.2 s. Trivial-call wall
3.3–10.5 s, dominated by upstream first-token latency; one history call took
36 s with the delay entirely upstream (between the user item and the first
delta). `exec` peak RSS 203 MB per process.

### 2.11 Client tools (Q11)

`thread/start{dynamicTools}` exists and the CLI answers calls through a
server→client `DynamicToolCall` request — a real passthrough shape. But in
0.163.0 dynamic tools are exposed **only inside the code-mode `exec` tool**
(as `tools.get_weather(...)` in its JavaScript declaration). With
`code_mode_host` disabled (required by §2.5) the model cannot reach them; it
answered "I couldn't access get_weather". Client tools therefore need the
code-mode host running model-authored JavaScript — an "ask first" item, not M1.

### 2.12 Client-tool spike (live, 2026-10-23)

Operator requirement: full function, so client tools are in scope.

| route | result |
| --- | --- |
| inert MCP server (`-c mcp_servers.inert…`) | CLI ran `tools/list` against it, but the tool reached the model **only inside code-mode `exec`**, not as a direct function; with the host disabled the model could not call it |
| `dynamicTools`, code-mode host **on**, rest locked | **works.** The model wrote `await tools.get_weather({city:'Kuala Lumpur'})`; the bridge received a server→client `item/tool/call` request `{callId, tool:"get_weather", arguments:{city:"Kuala Lumpur"}}` and its reply became the JS return value |

So in 0.163.0, every client tool runs through code-mode: model-authored JavaScript in
the separate `codex-code-mode-host` process. That JavaScript was probed directly
(strace on `execve,connect,openat,unlink,rename`):

- globals `require`, `process`, `Deno`, `Bun`, `fetch`, `XMLHttpRequest`, `WebSocket`,
  `console`, `WebAssembly`, `SharedArrayBuffer`: all `undefined`;
  `import('node:fs')` / `import('node:child_process')` → `unsupported import in exec`.
- the host process made **no** `execve` (beyond its own start), `connect` or file open;
  every network connection came from the main `codex` process (ChatGPT upstream).
- `Object.keys(tools)` under lockdown = `get_weather` (the client tool), `apply_patch`,
  `clock__curr_time`, `list_mcp_resources`, `list_mcp_resource_templates`,
  `read_mcp_resource`.
- **`apply_patch` cannot be removed** in 0.163.0 (`disabled_tools`, `tools.apply_patch`
  are both ignored as unrecognized). Called directly, it was refused —
  `patch rejected: writing is blocked by read-only sandbox; rejected by user approval
  settings` — and no file appeared. That refusal is Codex's own policy code.

Consequence: with code-mode on, "nothing executes" becomes "model JavaScript runs in a
capability-free V8 isolate whose only effects are the client's tools (answered by the
bridge) and Codex's own `apply_patch`, which the read-only policy refuses". The one
remaining trust in Codex's code is that refusal. §3 adds a kernel-enforced layer
(Landlock) so the bridge no longer depends on it.

### 2.13 Stage 2 live results (2026-10-23)

Bridge argv, real login, every child inside Landlock (ABI 4, kernel 6.8):

- **Landlock needs the resolver's real paths.** The first sandboxed runs timed out:
  `/etc/resolv.conf` → `/run/systemd/resolve/stub-resolv.conf` on Ubuntu, and Landlock
  checks the symlink target, so DNS failed. The helper now adds the resolved targets of
  `/etc/{resolv.conf,hosts,nsswitch.conf,localtime,host.conf}` as read-only file rules.
  Codex also logs that its own bubblewrap sandbox cannot create user namespaces
  (`uid_map` is denied) — harmless: it runs no commands.
- **Tool round trip with continuation works.** `gpt-6.1-sol` called
  `get_weather({"city":"Kuala Lumpur"})` from code-mode; the call reached the bridge as
  `item/tool/call`; the follow-up answered it on the same child and the model finished
  from the result (6.4 s for both requests, 10,335 input tokens).
- **Usage lands on the follow-up.** The segment that ends on a tool call reported 0
  tokens: Codex emits `thread/tokenUsage/updated` when a sampling ends, and that
  sampling ends only after the call is answered. Totals are exact; per-request
  attribution shifts to the follow-up.
- **Sentinel with code-mode on:** asked to `apply_patch` a file, the model reported
  "the sandbox is read-only" and the target directory stayed empty.
- In unit tests, a child inside the domain was refused both a write outside it and a
  read of a file outside it (`internal/codexbridge/landlock_test.go`).
- **In the container (2026-10-23).** The branch image (`distroless/base-debian12`,
  Docker's **default** seccomp profile, `--user 1000:1000`, the release mounted at
  `/opt/codex`) booted with `landlock="available (ABI 4)" client_tools=enabled
  channel=app-server release=qualified` — the boot self-test (a write and a read outside
  the domain, both refused) passed inside the container. From the container's network
  namespace: `/healthz`, `/v1/models` (hidden included), `/v1/usage`, a text turn, a
  stream, a tool call (parked: `parked_tool_turns: 1`), its continuation ("It's 31°C and
  humid in Kuala Lumpur…") and a `/data` write sentinel all passed (9/9); `/data` was
  unchanged. The startup probes the CLI shells out to (`lsb_release`, `getconf`) are
  absent from distroless and cost nothing.

### 2.14 Stage 3 live results (2026-10-23)

- `turn/start{outputSchema}` constrains the final message: a `{count, method}` schema
  came back as exactly that object (`"count":400`). The exec fallback takes the same
  schema as `--output-schema FILE`.
- Reasoning summaries: `turn/start{summary}` (`auto|concise|detailed|none`) streams
  `item/reasoning/summaryPartAdded` + `item/reasoning/summaryTextDelta` (96 deltas at
  `xhigh`); exec reports the summary as `item.completed{type:"reasoning", text}`. A
  12-reasoning-token turn produced no summary at all — summaries are the model's call.
- `/v1/responses` was not built: no client needs it, and the router's translation
  already serves Responses clients from a Chat Completions account.

## 3. Proposed surface — full function, built in stages

The target is full function (operator, 2026-10-23); the stages are build order,
not stopping points.

**Process and isolation (all stages).** One `codex app-server --stdio` child per
request: lockdown flags from §2.5 minus `--disable code_mode_host` (needed for client
tools, §2.12), plus `--disable in_app_updates`; `thread/start{sandbox:"read-only",
approvalPolicy:"never", ephemeral:true}`; allowlisted env; private temp `HOME`/CWD;
spawn semaphore; process-group kill on cancel/timeout; pinned binary copy outside the
updater's directory, mounted read-only. **Landlock** (kernel-enforced, unprivileged,
applied by a re-exec helper before `exec codex`): writes only to the private temp dir
and `CODEX_HOME`, no access to `/data`. This makes the no-write guarantee independent
of Codex's own policy code. Without Landlock, client tools are off (§4.7). Channel fallback: if the app-server schema check fails at
boot, or the child fails before the first byte, serve the request through
`codex exec --json` (single-chunk stream, text only).

**Stage 1 — transport.** `POST /v1/chat/completions` on `127.0.0.1:8791`, stream and
non-stream. Prior turns → flattened transcript in the `turn/start` input (§4.2); client
`system` → `baseInstructions`; `reasoning_effort` → `effort`; deltas →
`chat.completion.chunk`; usage from `tokenUsage.last` (OpenAI convention, unpriced);
error mapping per the spec. `GET /v1/models` from `model/list{includeHidden:true}`;
`GET /status` adds CLI version, login, plan and both rate-limit windows. Requests with
tools get 400 `chat_tools_unsupported` until stage 2 lands.

**Stage 2 — client tools.** Client `tools` → `dynamicTools`. Each `item/tool/call`
→ an OpenAI `tool_calls` entry (calls arriving together, e.g. `Promise.all`, are emitted
together), `finish_reason: "tool_calls"`. How the client's tool result gets back to the
model is decision §4.6.

**Stage 3 — the rest.** Images (`image_url` → `UserInput` image), structured output
(`response_format: json_schema` → `turn/start{outputSchema}`), reasoning summaries
(`item/reasoning/summaryTextDelta`) → `reasoning_content`, and the
`/v1/responses` surface if a client needs it.

**Boot checks.** Binary present at the pinned version; every lockdown flag accepted;
required app-server methods present in the generated schema; Landlock available;
login present; non-loopback bind refused. Failure is fatal, not logged.

## 4. Operator decisions (2026-10-23)

1. **Channel:** **app-server**, with `codex exec --json` as the fallback when the
   boot schema check fails or the child dies before the first byte.
2. **History:** the `[UNSTABLE]` `history` field is **rejected** — it is
   documented for Codex Cloud, which ezllm does not use. M1 sends prior turns as
   a flattened transcript in the turn input; recall and cache behaviour of that
   shape are M1 live-proof items.
3. **No-execution:** Codex's built-in tools stay stripped (they would run on the
   ezllm host); client tools pass through, as in claude-bridge M2. Open: accepting
   code-mode (§2.12) is **accepted** as the route for client tools, with Landlock
   as the kernel-enforced layer.
4. **Models:** expose **all** models, hidden ones included, with every effort level.
5. **Caps:** **none in the bridge**; the account is capped with Rules in the
   dashboard, like claude-bridge.
6. **Scope:** full function, not M1 alone. Tool results use **(a) continuation**:
   the Codex child stays alive across the client's tool round trip and the next
   request answers the pending `item/tool/call`, so the model's code-mode script
   resumes with its state intact. Stateless replay (the claude-bridge shape) is the
   fallback only when the hold times out or the follow-up cannot be matched to a
   live child. (Claude does not need this: its upstream receives the client's own
   structured `tool_use`/`tool_result` history and holds no mid-turn state.)
7. **No Landlock → no client tools, never a silent downgrade.** Without Landlock
   (non-Linux, old kernel, inactive LSM, or seccomp-blocked container) the bridge
   still serves text, streaming, models, images and structured output, but starts
   with code-mode disabled and refuses tool-bearing requests with 400
   `chat_tools_unsupported`, naming the reason, before any spawn. Boot logs
   `landlock=… client_tools=…`; `/status` carries both fields.
   `EZLLM_CODEX_TOOLS_WITHOUT_LANDLOCK=on` is the explicit operator override
   (relies on Codex's own `apply_patch` refusal only). Non-Linux builds refuse to
   start the bridge. Operator-facing detail:
   [codex-bridge-runbook.md](codex-bridge-runbook.md#platform-requirements--read-this-first).

