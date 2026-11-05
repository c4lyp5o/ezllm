# GitHub Copilot CLI bridge — design

Status: **SHIPPED 2026-11-05** (M1; nine commits on
`melissa/copilot-cli-bridge`, off the tool-pruning-drop tip). Supersedes the
OAuth/device-flow provider (`internal/githubcopilot`), which is deleted in the
same branch — see §11 for what changed between this design and the code, and
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

- **Client tools are refused** (`chat_tools_unsupported`, the shared bridge
  error code). The CLI runs its own agent loop with its own 27 tools; a caller's
  tool schemas cannot be honoured. Any combo carrying this hop must not serve a
  tool-using client.
- **Client system prompts are advisory text inside the prompt**, not a real
  system role — the CLI prepends its own ~5k-token scaffolding regardless.
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

The 13k-token floor per call is the price of the CLI's agent scaffolding; a
`--model=auto` + `--available-tools`-restricted invocation is the lever if that
ever matters. Report it in the runbook so nobody reads the ledger and thinks the
bridge is leaking.

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
card is never told the hop can do something it cannot.


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
exits, and the client-tools refusal (`chat_tools_unsupported`). The §10 SDK
spike was NOT run; the refusal stands as a documented limitation.
