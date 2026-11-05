# GitHub Copilot CLI bridge — design

Status: **SHIPPED 2026-11-05** on `melissa/copilot-cli-bridge`, off the
tool-pruning-drop tip. The body below is the original `-p` design: since then every
turn runs on the CLI's headless channel, with `-p` as plain chat's fallback (§10
second addendum). Supersedes the OAuth/device-flow provider
(`internal/githubcopilot`), which is deleted in the same branch — see §11 for what
changed between this design and the code, and
`docs/copilot-cli-bridge-runbook.md` for operating it.

## 1. What changes and why

Today the `github-copilot` account is an OAuth provider: ezllm runs a device
flow, mints a Copilot session token, and speaks the raw Copilot API
(`internal/githubcopilot/auth.go` 417 lines + tests, `cmd/ezllm/githublogin.go`,
`internal/server/github_device.go`, the dashboard device UI, the
`device_handle` key path). That minting path is the last non-bridge
subscription surface, and it is the thing that historically broke (the
2026-10-17 `slow_down` bug) because we are speaking a private API that changes
under us.

The other three subscriptions already ride the official CLI behind a loopback
surface (`claudebridge`, `codexbridge`, `agybridge`). Copilot CLI now has a
first-class headless interface, so Copilot joins that family and the OAuth
provider is retired.

## 2. Measured facts (probe 2026-11-05, this box, CLI 1.0.105)

`copilot -p "Reply with exactly: ok" -s --output-format=json --no-ask-user`
→ rc=0, JSONL on stdout. What the stream carries:

| Event | Use |
|---|---|
| `session.auto_mode_resolved` | `chosenModel`, and **`availableModels`** — the plan's catalog |
| `assistant.message_delta` | `deltaContent` → SSE `content` deltas (streaming works) |
| `assistant.message` | `model`, `content`, `toolRequests[]`, `phase` |
| `session.usage_checkpoint` | per-model `prompt_tokens`, `cache_read`, `cache_write`, `reasoningTokens`, `totalPremiumRequests` |
| `accountingSnapshot` | `tokenDetails.{input,cache_read,cache_write,output}` |
| `result` | `sessionId`, `exitCode`, usage summary — the terminal record |

Numbers from that single call: **prompt_tokens 13,035**, cache_read 1,536,
output 5, **1 premium request**. The CLI ships **27 tools (9,459 schema tokens)**
of its own — `bash`, `view`, `edit`, `edit`, `github-mcp-server-*`, … — because
it is a full agent, not a completion endpoint.

Auth: no token in the environment was needed; the CLI used its stored login in
`~/.copilot`. Precedence is `COPILOT_GITHUB_TOKEN` > `GH_TOKEN` > `GITHUB_TOKEN`,
and the accepted token types are **fine-grained PATs with the "Copilot Requests"
permission** or OAuth tokens from the CLI/`gh` — **classic `ghp_` tokens are
explicitly not supported**.

## 3. Architecture (mirrors `agybridge`, the newest of the three)

- `internal/copilotbridge/` — package with `Server` (`/healthz`, `/status`,
  `/v1/models`, `/v1/chat/completions`, bearer-gated), `Runner` (spawns the CLI
  per request), `request.go` (OpenAI chat → one prompt), `stream.go` (JSONL →
  SSE), `usage.go` (ledger-shaped usage), `models.go` (catalog + curated ids).
- `cmd/ezllm/copilotbridge.go` — boot wiring, env surface:
  `EZLLM_COPILOT_BRIDGE=on|off`, `EZLLM_COPILOT_BIN` (default
  `/opt/copilot/copilot`), `EZLLM_COPILOT_HOME` (its own login dir),
  `EZLLM_COPILOT_ADDR` (default `127.0.0.1:8793`), `EZLLM_COPILOT_TOKEN_FILE`.
  Boot preflight: `--version`, login check (a 1-token `-p` probe or
  `COPILOT_GITHUB_TOKEN` present), loopback assertion — same shape as
  `startClaudeBridge`.
- Provider: kind `copilot-bridge`, sentinel base `internal://copilotbridge`
  (added to the sentinel allow-list beside `internal://claudeweb` /
  `internal://geminiweb`), preset `github-copilot-sub` in
  `PROVIDER_PRESETS` next to `claude-sub` / `chatgpt-sub` / `antigravity-sub`
  (no key to paste — the bridge supplies it).
- `EnableBridge("github-copilot", …)` so `/admin/bridges`, the update notices and
  `scripts/bridge_update.py` cover it like the other three.

## 4. Request mapping (the honest cost)

`-p` is one-shot: it cannot replay history or accept a system-prompt flag.
Following the agy rule, system text and prior turns are flattened into ONE
deterministic prompt. Consequences, stated plainly:

- **Client tools are native** over the CLI's headless channel (§10 second
  addendum). This replaced an emulation through the prompt, which replaced the
  original refusal (`chat_tools_unsupported`).
- **Client system prompts replace the CLI's system message** on the channel. On the
  `-p` fallback they are advisory text behind the CLI's ~5k-token scaffolding.
- `response_format`, `n>1`, `logprobs`, `reasoning_effort` have no CLI
  equivalent (`--model` is the only lever); they are refused or ignored exactly
  as the other bridges do, per the shared translation rules.

## 5. Accounting (better than the OAuth path)

The JSONL carries a per-model usage block with a real cache split, so the bridge
books `tokens_in` (cold), `tokens_cached_read`, `tokens_cached_write`,
`reasoning_tokens` and `tokens_out` from the CLI's own numbers, plus
`totalPremiumRequests` as the subscription cost unit. **No estimation, no
synthesized envelope** — this is the first subscription surface where the ledger
gets exact numbers.

The 13k-token floor per call is the price of the CLI's agent scaffolding on `-p`.
The channel removes it (§10 second addendum), so it now applies only on the fallback.
The runbook reports both so nobody reads the ledger and thinks the bridge is leaking.

## 6. Model catalog

No non-interactive "list models" command exists. Two sources, in order:
1. `session.auto_mode_resolved.availableModels` from one boot probe — the plan's
   real catalog for that login.
2. A curated `--model`-safe id list (documented ids only) for routing seeds.

A model id is only marked usable after a probe answers, which is exactly the
`proto_*` discipline the sweeps already apply (a green probe proves the wire).

## 7. Operations

- Loopback-only, bearer-gated, never public — same as the other bridges.
- `bridge-check copilot` gate before promoting a CLI version; `bridge_update.py`
  learns `copilot` as a fourth target (official channel = `copilot update`).
- Failure classes to type honestly: not logged in, premium requests exhausted,
  model not in plan, CLI version event-schema drift (the JSONL `type` set is
  version-sensitive — a missing `usage_checkpoint` must record "usage unknown",
  never a zero).

## 8. Retirement of the OAuth provider — phased, not one big delete

- **M1** build the bridge, prove it against the real CLI on a loopback instance,
  add the preset + bridge card. Nothing is deleted.
- **M2** re-point the existing Copilot account and the `maxine` hop 3 to the
  bridge preset; soak real traffic; compare accounting against the OAuth rows.
- **M3** delete the OAuth surface in its own commit: `internal/githubcopilot/`
  (auth + provider + tests), `cmd/ezllm/githublogin.go`,
  `internal/server/github_device.go` + `/admin/github/device*` routes, the
  `device_handle` key path, the `pok` preset + device-flow UI in
  `Providers.tsx`, the mint-credential sentinel/branch in registration, and the
  `github-copilot` kind from the sentinel allow-list. Schema untouched: existing
  `provider_keys`/`accounts` rows keep their history; the kind becomes invalid
  for NEW accounts only.
- **M4** docs: runbook, CHANGELOG, EXPLORING landmark, and a supersession note
  at the top of `docs/github-copilot-design.md`.

## 9. Locked decisions (2026-11-05, Calypso)

1. **Auth: CLI login only.** The bridge uses a dedicated `EZLLM_COPILOT_HOME`
   (default `/copilot-home`, mounted) and never reads a PAT env var. The box is
   already logged in (`~/.copilot` exists and a headless call authenticated with
   no env token set), so the deployment mounts that directory. Fine-grained
   PATs remain documented as a manual fallback for a fresh host, not a code
   path.
2. **One branch.** The bridge and the deletion of the OAuth surface land
   together — no soak gate, no fallback provider. `internal/githubcopilot`,
   `cmd/ezllm/githublogin.go`, `internal/server/github_device.go`, the
   `/admin/github/device*` routes, the `device_handle` key path, the
   `github-copilot` preset/UI and the mint-credential branch all go in the same
   change set as `internal/copilotbridge`.
3. **Tool bridging is in scope** — client tool schemas must reach the CLI hop,
   not be refused. This is the part that changes the architecture; see §10.

## 10. Tool bridging — what the CLI actually allows (measured 2026-11-05)

The `-p` one-shot surface **cannot** carry a tool round-trip, and this was
verified against the installed CLI, not assumed:

- `assistant.message.data.toolRequests[]` exists (the model CAN ask for a
  tool), but in `-p` mode the CLI executes its own tools itself
  (`--allow-tool` / `--available-tools` gate what it may run) and there is **no
  option that returns a pending tool call to the caller and accepts a result
  back**: no `--input-format`, no permission mode that yields instead of
  executing, and `copilot sessions` only supports `import`. `--session-id` can
  resume a session by UUID, but nothing documented injects an external tool
  result into it.
- `--available-tools` / `--excluded-tools` can only narrow the CLI's OWN 27
  tools; they cannot declare a tool the CLI does not have.
- An MCP server (`--additional-mcp-config`) would let the CLI *see* custom
  tools, but MCP tools execute inside the CLI's loop: the result has to exist at
  call time, and an HTTP one-shot has no way to ask the upstream client for it.
- The CLI ships an extension/SDK surface (`--extension-sdk-path`, bundled
  `@github/copilot-sdk`). That — not `-p` — is the only credible route to real
  tool bridging: a session API with host-side tool callbacks.

**Therefore §10 splits into a spike plus a build, inside the same branch:**

- **M1 (ships):** `-p`-driven bridge, full chat, exact usage, model catalog —
  and client tools refused with `chat_tools_unsupported` *only while* the SDK
  path is unproven. The refusal is the honest default the other three bridges
  already use.
- **M1.5 (spike, time-boxed):** read the bundled `@github/copilot-sdk` surface
  for a session API that (a) lets the host declare tools with callbacks and
  (b) reports a tool call to the caller. If both exist, the bridge grows a
  Node sidecar (the SDK is JS) speaking our loopback contract; if they do not,
  §10 ends as a documented limitation and the `chat_tools_unsupported` refusal
  becomes permanent rather than temporary.
- **M2:** only if the spike proves it — client tools → SDK tool callbacks →
  OpenAI `tool_calls` back to the client, with the turn's tool results replayed
  into the same session, and the ledger recording that hop as translated.

The refusal in M1 is never silent: the bridge's `/status` names the capability
("client tools: not yet — SDK spike pending"), so an operator reading the bridge
card is never told the hop can do something it cannot. (The spike ran: see the
second addendum below.)


### §10 addendum (2026-11-05): emulation instead of refusal

**Superseded by the next addendum and removed from the code.** Kept because it
records why the text protocol failed.

The refusal above was the honest default, but it made the hop unusable for a
client that sends tools on every call (a live combo returned 400 on its first
request). The spike is not needed to do better than refusing: a one-shot CLI can
still *emit text*, so the bridge emulates function calling in the prompt layer.

- **Out:** `tools` → a "Functions" block (name, description, JSON-schema
  parameters) plus the instruction to reply with `<tool_call>{"name","arguments"}</tool_call>`
  and to treat those functions as its ONLY tools. `tool_choice` none drops the
  block; required / a named function adds a MUST line.
- **Back:** the reply is scanned for `<tool_call>` blocks. A block counts only if
  it parses and names a DECLARED function; anything else stays text. Valid
  blocks become OpenAI `tool_calls` (ids `call_…`, `arguments` as a JSON
  string), text outside them stays as `content`, `finish_reason` is `tool_calls`.
- **Round trip:** a prior assistant `tool_calls` message is rendered in the same
  tag format and a `tool` message becomes `Tool result for <name> (<id>):`, so
  the next request's transcript reads consistently.
- **Streaming:** with tools declared the reply is buffered and then emitted
  (a `<tool_call>` block cannot be unsent once streamed); without tools,
  streaming is unchanged.
- **The CLI's own tools are hidden on these turns** (`--available-tools=` a
  nonexistent name, which allowlists nothing, plus `--disable-builtin-mcps`).
  Measured 2026-11-05 on a live agent client: offered its 27 natively formatted
  built-ins beside the text protocol, the model reached for its `terminal` tool,
  the CLI denied it (no `--allow-tool`), and the turn ended with "terminal access
  was denied" and no `<tool_call>`. Plain chat is unchanged.
- **What it is not:** native tool calling. Compliance is the model's choice, the
  CLI's own tools stay denied (no `--allow-tool`), and the scaffolding cost per
  turn is unchanged. A wrong or missing format degrades to a plain-text answer.
- **Router:** a hop that still refuses tools (`chat_tools_unsupported`, now
  only for non-function tool types) fails over without a cooldown instead of
  ending the client run (`refusesTools`, proxy.go).

### §10 second addendum (2026-11-05): native tools over the headless channel

Emulation did not work live. Its last measured turn charged one premium request
for an empty reply: the model's reasoning said it had "the perfect tool", and it
returned no text and no call. Tuning the text format looked like a poor bet, so the
M1.5 spike ran, and it needs no Node sidecar: the SDK is a client for a JSON-RPC
server already in the CLI, `copilot --headless --stdio`, and Go can speak that
directly.

Measured on CLI 1.0.105 with `gpt-6-luna`:

- `session.create` takes `tools`, an `availableTools` allowlist and
  `systemMessage: {mode: "replace"}`. Read after `session.tools.initializeAndValidate`,
  `session.tools.getCurrentMetadata` shows 0 tools for an empty allowlist and 18 for
  none at all, including `bash` and `apply_patch`, so the key must always be sent.
  (Without that initialize call it lists only caller tools and proves nothing.)
- `assistant.message.toolRequests` lists the whole batch up front, with the ids the
  model chose. The CLI then emits `external_tool.requested` and waits.
- `session.abort` ends the wait. The usage checkpoint still arrives before
  `session.idle`, so the turn can be booked.
- A fresh session given the results as transcript text answered correctly and did
  not repeat the calls.
- Refusals at `session.create` (model not available, invalid tool name) are free,
  because no model has been called.

**One session per HTTP request (shipped), or one held open across a tool loop?**
Answering the pending call with `session.tools.handlePendingToolCall` also worked.
Measured on one two-tool loop:

| | Premium requests | AI credits |
|---|---|---|
| One session per request (2 turns) | 2 | 0.00893 |
| One session held open | 1 | 0.00719 |

The operator's plan bills AI credits, so holding sessions saves ~20% on this small
sample, in exchange for parked children, held concurrency slots, timeouts, and a
failover or bridge restart losing the session. Not built. Long conversations were
not measured, so that figure could change.

**The failure shapes that decided the code:**

- `overridesBuiltInTool` is always set, so a caller's tool that shares a built-in's
  name is unambiguously the caller's. Measured: a forced `bash` call reached the
  caller and its command did not run on this box.
- Every turn pings first. `--headless` is undocumented and the updater can swap the
  binary under a running bridge, so a boot check alone would go stale. The ping costs
  nothing, and a failed gate is the refusal the router already fails over on.
- `tool_choice` has no channel equivalent. A named function narrows `availableTools`;
  `required` is an instruction only.
- The channel writes `totalPremiumRequestCost` as `1.0` where `-p` wrote `1`. An int
  field dropped the whole event, so the first live run booked zero usage while every
  unit test passed on a fixture written with `1`. Numeric event fields now use a
  tolerant type, and the fixture carries the channel's real form.

**Plain chat moved to the channel too.** The same replaced system message removes
the CLI's scaffolding, the largest cost on the hop: 95 prompt tokens against 13,957
on `-p` for a short question. `-p` stays as plain chat's fallback when the gate
refuses, so a CLI update that breaks the undocumented channel degrades cost rather
than availability. Only the gate's refusal falls back: anything later happened at
the CLI and may have been billed. A plain turn's system message tells the model it
has no tools, and a turn with tool history and `tool_choice: none` answered in text
rather than going empty as emulation did.

## 11. What changed between this design and the shipped code

Recorded because each one was found by a gate, not by review — the design was
right about the shape and wrong about four details.

1. **The catalog must be PROVEN ids only.** §5 said "curated ids plus per-model
   probe verdicts". In practice the first catalog carried documented ids this
   plan does not include (`gpt-5.3-codex`, `claude-haiku-4.5`, `mai-code-1.1-flash`).
   The key test probes the first catalog candidate, so the CLI's "that model is
   not included in your Copilot plan" came back as a REJECTED KEY (422) and a
   perfectly good bridge was refused. `copilotModelIDs` is now `auto` +
   `gpt-6.1-sol`, both proven by a real turn. Rule: an id joins the list after a
   turn answers on it, never because it is documented.
   **Superseded (2026-11-05):** the real fault was the key test probing a
   non-`auto` first candidate. With `auto` pinned FIRST (a test guards it) the
   catalog is now every id `copilot help config` documents, read at boot, and the
   plan decides per request with a typed 403 `model_not_in_plan`; the operator
   chooses what to enable. See the runbook's Models section.
2. **The bridge registry entry's `Kind` must be the ACCOUNT kind**, not the wire
   kind. Registering `github-copilot` with `Kind: "openai-compatible"` made the
   key-add refuse a `copilot-bridge` account ("that account does not point at
   this bridge"), because `bridgeForURL` compares the account's kind. It is now
   `Kind: "copilot-bridge"` — one account kind, one wire shape.
3. **`KnownWireSupport` died with the provider.** The router consulted it before
   a probe ("the provider's own API shape outranks an observation"). Copilot's
   OAuth API was the only kind declaring such facts, so the hook is gone and a
   probe verdict is authoritative again. A future kind that needs to outrank a
   probe should get a capability hook, not a model-id table in the router.
4. **`config.yaml` has a SECOND kind allow-list.** `internal/config/config.go`
   duplicates the provider kinds as strings so validation stays
   dependency-free; it did not know `copilot-bridge` and still accepted
   `github-copilot`. Both are fixed — a kind added to `provider.ValidKinds`
   without this list is a config that refuses valid YAML.

Unchanged from the design: prompt on stdin (measured — `MAX_ARG_STRLEN` 128 KB
against 250–600 KB prompts), no `--allow-tool`, exact-or-zero usage, typed
and exits. On the `-p` fallback that all still holds. The client-tools refusal did
NOT survive contact with a live combo: it was replaced by prompt-level emulation
(§10 addendum), which failed live and was replaced in turn by native tools over the
headless channel (§10 second addendum). Over the channel the prompt goes in a
JSON-RPC message instead of on stdin; a 189 KB prompt was measured.
