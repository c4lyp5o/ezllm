# Spec: claude-bridge M2 — tool calls on the claude-sub hop

**Historical specification:** implemented 2026-10-21 (approach A) and included in
subsequent releases. Deployment and operator-specific completion are not established
by this document; see [tasks/todo.md](tasks/todo.md) and verify the live instance.
The requirements and commands below record the original plan, not current status.
**Parent design:** [docs/claude-subscription-design.md](docs/claude-subscription-design.md) §6 (M2)

## Objective

Hermes agents (e.g. Max) reach `claude-sub` through ezllm and send a tool list on
**every** turn. M1 rejects any request that carries `tools`, `tool_choice` or
`parallel_tool_calls` before spawning the CLI (M1's `ParseRequest`),
so every agent turn on this hop fails with:

```
HTTP 400: claude-bridge: tools is not supported on this hop (M1 is text-only)
```

M2 makes the bridge a working **tool-calling** Messages surface. Upstream receives the
client's tools. The model's `tool_use` blocks come back to the client under the
client's original tool names. The CLI never executes anything. The client runs its own
tools and sends the results back in the next request, the same way it would with any
Anthropic or OpenAI endpoint.

**Users:** Hermes agents (OpenAI chat surface, `custom:ezllm-openai`, translated to
Messages by ezllm), and Claude Code clients (Anthropic Messages, native).

**Locked decisions it must keep:** 1 (only the official CLI drives the subscription),
3 (loopback-only, in-process), 4 (plain `anthropic-compatible` account, no new
router or translate concepts), 9 (native usage), 13 (allowlisted env).

## Approach (decided by Task 0's spike)

There are two mechanisms. The spike measures **A** on CLI 2.1.299 first, because A is
much smaller. If A fails any must-hold property below, we build **B**, which is the
design doc's original M2 plan.

| | A — extra-body passthrough | B — inert MCP server (design doc M2) |
|---|---|---|
| How | Put the client's `tools`/`tool_choice` in `CLAUDE_CODE_EXTRA_BODY`, the body that upstream already treats as authoritative. Keep `--tools ''` and the empty MCP config | The bridge serves a stdio MCP server that advertises the request's tools as `mcp__client__<name>` and denies execution. The bridge maps the names back on the way out |
| New code | Remove the refusal, pass the fields through, handle `tool_use` blocks in the stream, accept tool blocks in frames | Everything in A, plus an MCP stdio server (an `ezllm` subcommand, since distroless has no shell), name aliasing (50-char hash above the length limit), and rewriting names in history and responses |
| Unknowns | Whether native merges or overrides `tools` from the extra body. What native does when it sees a `tool_use` for a tool it doesn't know (error `tool_result`? a second turn? a filtered block?) | Whether deny-execution still emits a clean `tool_use` frame with `--max-turns 1` |

**Must-hold properties (the spike measures each one; B must meet them too):**
1. Upstream sees the client's tool definitions unchanged (name, description, schema).
2. A `tool_use` block reaches the client stream intact: `content_block_start` with
   `id`/`name`, `input_json_delta`s, and `stop_reason: tool_use`.
3. Nothing is executed: there is no CLI built-in tool, no MCP side effect, and the
   bridge sends no second upstream generation (exactly one generation per call, as in M1).
4. History that contains assistant `tool_use` and user `tool_result` blocks replays
   without native rejecting it, including when the final user turn is only `tool_result`s.

## Tech Stack

Go 1.27, `CGO_ENABLED=0`, no new modules (any MCP framing is hand-written JSON-RPC
over stdio). Claude Code CLI bind-mounted read-only, pinned to the version Task 0 qualifies (2.1.299 has been removed from the host).

## Commands

```bash
# focused, safe on this box (it is also the load balancer)
go test -short -count=1 ./internal/claudebridge/ ./cmd/ezllm/
make test-quiet                      # functional gate, CPU/IO-throttled
# full gate → CI only: push a non-master branch, then
gh workflow run test.yml --ref <branch>
# live proof against the real CLI + Pro subscription (operator-run)
python3 tests/prove_claude_bridge.py
gofmt -l . && go vet ./...
```

`make check` is **not** run locally (see CONTRIBUTING.md § Memory and the shared-box rule).

## Project Structure

```
internal/claudebridge/frames.go     request parse (refusal removed), extra body, frames
internal/claudebridge/stream.go     tool_use block assembly for stream and non-stream
internal/claudebridge/runner.go     argv / mcp.json (B: MCP server wiring)
internal/claudebridge/mcp.go        B only: inert stdio MCP server + name aliasing
internal/claudebridge/*_test.go     in-package tests (fake CLI fixtures, no real spawn)
cmd/ezllm/claudebridge.go           /status capability string ("tools": ...)
tests/prove_claude_bridge.py        live proof: refusal check → tool round-trip
docs/claude-subscription-design.md  §6 M2 → SHIPPED, record of the A/B decision
docs/claude-bridge-runbook.md       remove the chat_tools_unsupported entry, add tool notes
CHANGELOG.md, VERSION.md, internal/server/version.go   0.0.21 → 0.0.22, same commit
```

## Code Style

Match `internal/claudebridge`: comments state *measured* behaviour and the CLI version,
refusals are typed rather than silent drops, and errors start with `claude-bridge:`.

```go
// Measured on CLI 2.1.299: <what native did>. Without <x>, <observed failure>.
if _, ok := raw["tools"]; ok && !toolsSupported(req) {
	return Request{}, Unsupported{Field: "tools"}
}
```

## Testing Strategy

- **Unit (in-package):** `ParseRequest` accepts `tools`/`tool_choice` and still
  refuses anything this hop cannot honour (e.g. Anthropic server tools such as
  `web_search_*`, with a typed refusal). `BuildExtraBody` carries tools unchanged.
  `BuildFrames` accepts `tool_use`/`tool_result` history. `StreamWriter` relays a
  recorded native tool-call frame sequence, and the non-stream `Message()` returns
  `tool_use` blocks and `stop_reason: tool_use`. B adds alias round-trip tests and
  MCP handshake/deny tests.
- **Fixtures** for the native frame sequences come from the spike's real 2.1.299
  output, not from hand-written guesses.
- **Prove-It:** the existing M1 assertion "tools → 400" is *replaced* by "tools → 200
  with tool_use". This is a deliberate contract change and is called out in the commit.
  It does not weaken a test.
- **Live proof:** extend `tests/prove_claude_bridge.py` with these cases: a single tool
  call; a full round-trip (call → result → final text); `tool_choice` forcing a named
  tool; a no-execution sentinel (a `write_file` tool whose target path must not exist
  afterwards); and the Hermes path through ezllm's OpenAI surface returning
  `tool_calls` with `finish_reason: tool_calls`.

## Boundaries

- **Always:** keep the CLI's own tools disabled. Keep one generation per call. Keep
  refusals explicit for anything this hop can't honour. Base frame fixtures on
  measured output. Bump the version in `version.go` and `VERSION.md` together.
- **Ask first:** building B if A fails the spike; any change to combo or drain
  scoping beyond the agreed Rules cap; bumping the CLI version; adding an `ezllm`
  subcommand (B).
- **Never:** let the CLI or the bridge execute a client tool; reuse the OAuth bearer
  from Go; add fields to `healthz`; let `ANTHROPIC_*` or provider keys into the child
  env; commit `config.yaml`/`data/`; run `make check` locally.

## Success Criteria

1. A `/v1/messages` request with `tools` gets a 200, not a 400. Stream and non-stream
   both return `tool_use` blocks with the **client's original** `name`, a valid `id`,
   and `input` that parses as JSON, with `stop_reason: tool_use`.
2. A follow-up request whose history carries that `tool_use` and its matching
   `tool_result` produces a final text answer that uses the result.
3. `tool_choice` `auto` / `any` / `{"type":"tool","name":…}` are honoured, and `none`
   results in no tool call.
4. No tool executes: the live proof's sentinel file is never created, and the CLI
   reports no tools of its own in its `init` frame.
5. Through ezllm, a Hermes-shaped OpenAI chat request with `tools` on the claude-sub
   combo returns `tool_calls` (this is the exact failure in the bug report, now fixed).
6. Usage stays native and exactly one generation is billed per call, as measured
   in the ledger.
7. Drain-cap rules exist on `claude-sub` before Max is moved back, and an over-cap
   request gets a 429 that names the rule.
8. `make test-quiet` passes locally and the full CI gate is green. Docs and version are
   updated, and nothing in `*.md` is left marked stale.

## Decisions (Calypso, 2026-10-21)

1. **Spike A first.** Build B only if A fails a must-hold property.
2. **Max on `claude-sub` is intended to last, behind a cap.** This overrides locked
   decision 5 for agent traffic; the design doc records the change when M2 ships.
   The cap must be live **before** Max is moved back (see Drain cap below).
3. **The spike measures cache economics.** It records cache read/create across a
   5-turn agent conversation with Max's real tool list, and that decides whether
   M3 is needed.
4. **No interim failover.** Max stays off `claude-sub` until M2 ships.

## Drain cap (recommended: Rules, no new code)

The cap uses the **existing M5 Rules engine** (`internal/rules`, Rules page). It
does not revive `accounts.cap_tokens`.

- **Why:** Rules are already enforced in the request path. They refuse with an
  honest 429 that names the rule and the window, and their meter is updated inside
  the ledger transaction (`internal/store/usage_counters.go`). `accounts.cap_tokens`
  is stored but never checked anywhere, and the provider modal deliberately stopped
  showing it (`web/src/pages/Providers.tsx:448`: "cap_window + cap_tokens live on
  Rules"). Putting that field back in the modal would add a second, competing cap
  path.
- **How:** set one rule on `claude-sub` for each model Max can use (`sonnet`,
  `haiku`) with `cap_window: 5h`. The cap value comes from the spike's measured
  tokens per agent turn × an agreed turn budget. Opus gets a near-zero cap so it can
  never be drawn automatically (design §2 finding 10).
- **Known gaps (accepted):**
  - Rules count `tokens_in + tokens_out` only (cache reads are excluded).
  - The 5h window is a calendar block (00–04, 05–09, …), not Anthropic's rolling
    window.
  - Caps are per model, so the effective account total is the sum of the rules.
  - If one account-wide total is needed later, the change is a small Rules
    extension (a wildcard-model rule), not the account field.
- **Verify:** once the 5h cap is reached, a claude-sub request returns 429 with
  the rule detail, and the operator runbook documents the rules.

5. **Rules confirmed for the cap. The cap value is proposed after the spike**, from
   the measured tokens per agent turn, and Calypso approves it.

## Open Questions

1. **CLI pin drift (found while planning, 2026-10-21).** The CLI's auto-updater has
   removed 2.1.299 from the host, leaving only 2.1.304–306. The running container
   keeps working only because its bind mount still holds the deleted file, so the
   next container recreate breaks the bridge at boot. Recommended fix: requalify
   on the newest CLI and copy that binary to a path the updater doesn't manage
   (`tasks/plan.md` Task 0).
