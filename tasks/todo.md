# Todo: claude-bridge M2 — tool calls on claude-sub

**Historical checklist:** The implementation shipped. Unchecked operator items below
are not evidence of current deployment or configuration; verify them on the live
instance before acting.

Plan: [plan.md](plan.md) · Spec: [SPEC.md](../SPEC.md)

Focused test command used below: `go test -short -count=1 ./internal/claudebridge/ ./cmd/ezllm/`
(never `make check` locally; full gate = CI via `gh workflow run test.yml --ref <branch>`).

---

## Task 0: Requalify the CLI and pin it stably

**Description:** 2.1.299 is gone from the host and the next container recreate will break the
bridge. Choose a version, copy it to a path the updater doesn't manage, point the mount at it,
and confirm that the boot assertions (flag surface, login) and the M1 live proof still pass.

**Acceptance criteria:**
- [x] The binary is copied to `~/.local/share/claude-bridge/claude-<ver>` and `CLAUDE_CLI_BIN` points at it (compose default updated)
- [x] Bridge boot checks pass on that version (flag surface + `auth status` logged in)
- [x] The M1 proof `tests/prove_claude_bridge.py` passes on it (5/5 on 2.1.306, ledger check skipped: no admin token)

**Verification:**
- [x] Tests pass: focused test command
- [x] Manual check: recreate the container; `/status` reports the new CLI version

**Dependencies:** None
**Files likely touched:** `compose.yml`, `docs/claude-bridge-runbook.md`, `cmd/ezllm/claudebridge.go` (only if flag checks drift)
**Estimated scope:** S

## Task 1: Spike approach A on the pinned CLI

**Description:** Spawn the CLI with the bridge's exact argv (`internal/claudebridge/runner.go:107`),
with `tools`/`tool_choice` added to `CLAUDE_CODE_EXTRA_BODY`. Record raw stream-json output.
Use Max's real Hermes tool list for the cache run. The spike script lives in the scratchpad;
only redacted fixtures are committed.

**Acceptance criteria:**
- [x] Each of the 4 must-hold properties (SPEC § Approach) is recorded as pass/fail with the raw frames as evidence
- [x] A 5-turn agent conversation's cache read/create and cold tokens per turn are tabulated
- [x] Redacted frame fixtures (single call, round-trip, tool_choice forced) are committed under `internal/claudebridge/testdata/`

**Verification:**
- [x] Manual check: the sentinel `write_file` target does not exist after the runs
- [x] Findings appended to `docs/claude-subscription-design.md` §2 as measured rows

**Dependencies:** Task 0
**Files likely touched:** `internal/claudebridge/testdata/*.jsonl`, `docs/claude-subscription-design.md`
**Estimated scope:** M

## Checkpoint 1: Spike verdict — review with Calypso
- [x] A or B decided (B → stop, re-plan per plan.md § Contingency)
- [x] M3 needed? (numbers, not guesses)
- [x] 5h cap values proposed and approved

---

## Task 2: Single tool call works (stream + non-stream)

**Description:** Remove the `tools`/`tool_choice` refusal in `ParseRequest`. Carry both fields
through `BuildExtraBody` unchanged. Make `StreamWriter` relay `tool_use` content blocks and
`stop_reason: tool_use` in stream and non-stream mode, and suppress any native follow-up the
spike observed after an unknown tool call.

**Acceptance criteria:**
- [x] A request with `tools` returns 200; stream emits `content_block_start{type:tool_use}` + `input_json_delta` + `stop_reason: tool_use`
- [x] Non-stream `Message()` returns the `tool_use` block with its id, client name and parsed input
- [x] The M1 "tools → 400" assertion is replaced by a test that fails on the old code (commit message names the contract change)

**Verification:**
- [x] Tests pass: focused test command (replays the T1 fixtures)
- [x] `gofmt -l internal/claudebridge` empty, `go vet ./internal/claudebridge/`

**Dependencies:** Checkpoint 1 (A chosen)
**Files likely touched:** `internal/claudebridge/frames.go`, `stream.go`, `frames_test.go`, `stream_test.go`, `server_test.go`
**Estimated scope:** M

## Task 3: Tool history round-trip

**Description:** History with assistant `tool_use` and user `tool_result` blocks (including a
final user turn made only of `tool_result`s) replays correctly. Adjust `BuildFrames` (and the
last-message check in `ParseRequest`) to match what the spike showed native accepts.

**Acceptance criteria:**
- [x] `ParseRequest` accepts a final user message whose content is only `tool_result` blocks
- [x] `BuildFrames` output for tool history matches the shape the spike showed native accepting (fixture test)
- [x] `ConversationSessionID` stays stable across the turns of one tool conversation

**Verification:**
- [x] Tests pass: focused test command

**Dependencies:** Task 2
**Files likely touched:** `internal/claudebridge/frames.go`, `frames_test.go`
**Estimated scope:** S

## Task 4: tool_choice variants + typed refusal for server tools

**Description:** `auto`, `any`, `{"type":"tool","name":…}` and `none` reach upstream as sent.
Anthropic server tools (`web_search_*`, `code_execution_*`, …) and an OpenAI-only
`parallel_tool_calls` stay explicit `Unsupported` refusals. Update the refusal text so it no
longer says "M1 is text-only".

**Acceptance criteria:**
- [x] Every `tool_choice` form round-trips into the extra body unchanged (table test)
- [x] A server-tool entry returns 400 `Unsupported{Field:"tools[n].type=…"}` before any spawn
- [x] The refusal message names the field and the reason, with no stale milestone wording

**Verification:**
- [x] Tests pass: focused test command

**Dependencies:** Task 2
**Files likely touched:** `internal/claudebridge/frames.go`, `frames_test.go`, `errors.go`
**Estimated scope:** S

## Task 5: Hermes path through ezllm's OpenAI surface

**Description:** Prove that an OpenAI chat request with `tools` (shaped like Hermes's), routed to
an `anthropic-compatible` account pointing at a fake bridge, returns `tool_calls` with
`finish_reason: tool_calls`, and that the follow-up `role: tool` message reaches the bridge as
a `tool_result`. Add a translate or server test only where coverage is missing.

**Acceptance criteria:**
- [x] An in-package test drives OpenAI chat → Messages → fake bridge tool_use → OpenAI `tool_calls` (stream and non-stream)
- [x] `parallel_tool_calls=false` arrives as `tool_choice.disable_parallel_tool_use`, never as a top-level field the bridge refuses

**Verification:**
- [x] Tests pass: `go test -short -count=1 ./internal/claudebridge/` (path test imports translate)

**Dependencies:** Task 4
**Files likely touched:** `internal/claudebridge/hermes_path_test.go`, `internal/claudebridge/frames.go`

**Found and fixed:** translated Hermes turns carry up to 3–4 message `cache_control` markers, and native adds 2 more. Upstream caps a request at 4, so live it returned `400 … Found 6`. The bridge now keeps the newest 2 (`clientCacheMarkers`). Existing proxy coverage: `TestWireMatrixToolLoopCrossCells`, `TestChatToMessagesHermesBody`.
**Estimated scope:** S

## Checkpoint 2: Core
- [x] Focused packages green: `go test -short -count=1 ./internal/claudebridge/ ./cmd/ezllm/ ./internal/translate/ ./internal/server/`
- [x] `make test-quiet` green
- [ ] Review the diff with Calypso

---

## Task 6: Live proof + /status capability

**Description:** Extend `tests/prove_claude_bridge.py` with these cases: single call,
round-trip, forced `tool_choice`, the no-execution sentinel, and the Hermes path through ezllm.
Change `/status` from `"tools": "unsupported (M1 text-only)"` to the new capability.

**Acceptance criteria:**
- [x] The proof passes in full against the real CLI and Pro login (the refusal case is replaced by tool cases)
- [x] The sentinel file is absent after the run; the CLI's `init` frame lists no built-in tools
- [x] `/status` reports tool support accurately

**Verification:**
- [x] Manual check: `python3 tests/prove_claude_bridge.py` output pasted into the PR
- [x] Tests pass: focused test command

**Dependencies:** Checkpoint 2
**Files likely touched:** `tests/prove_claude_bridge.py`, `cmd/ezllm/claudebridge.go`
**Estimated scope:** S

## Task 7: Docs (version stays v0.0.21: same change date)

**Description:** Mark design doc §6 M2 SHIPPED and record the A/B decision. Amend locked
decision 5 for capped agent traffic. Update the runbook (drop `chat_tools_unsupported`, add tool
and cap notes). Add the CHANGELOG milestone. Bump `VERSION.md` `Current:` and
`internal/server/version.go` in the same commit.

**Acceptance criteria:**
- [x] `version_test.go` passes; M2 lands under v0.0.21 per VERSION.md's one-version-per-date rule
- [x] No stale markers in the touched `*.md` files; `file.go:NN` landmarks re-verified
- [x] SPEC.md marked implemented at the time; deployment was then pending

**Verification:**
- [x] Tests pass: `go test -short -count=1 ./internal/server/`
- [x] stale-marker grep over docs/claude-*.md, CHANGELOG.md, VERSION.md is empty

**Dependencies:** Task 6
**Files likely touched:** `docs/claude-subscription-design.md`, `docs/claude-bridge-runbook.md`, `CHANGELOG.md`, `VERSION.md`, `internal/server/version.go`
**Estimated scope:** M (docs-heavy)

## Checkpoint 3: Complete
- [x] CI full gate green: `gh workflow run test.yml --ref <branch>`
- [x] Calypso pushes/deploys

---

## Task 8: Drain-cap rules on claude-sub

**Description:** Set the approved Checkpoint 1 values as Rules on the `claude-sub` account:
sonnet and haiku with `cap_window: 5h`, opus near zero. This is configuration via the dashboard
Rules page or the admin API; no code changes.

**Acceptance criteria:**
- [ ] Three rules exist with the approved values
- [ ] An over-cap request returns 429 naming the rule and window (verify with a temporary tiny cap, then restore)
- [x] The runbook records the values and the rationale (done within Task 7 if the timing works)

**Verification:**
- [ ] Manual check: the Rules page shows the caps; the 429 body is captured

**Dependencies:** Checkpoint 1; deployed build for the 429 check
**Files likely touched:** none (config); `docs/claude-bridge-runbook.md`
**Estimated scope:** XS

## Task 9: Move Max back onto claude-sub (operator)

**Description:** Calypso points Max's combo at claude-sub, sends a real tool-using turn, and
watches the ledger.

**Acceptance criteria:**
- [ ] Max answers a turn that uses a tool, with no 400
- [ ] The ledger shows one call per turn on claude-sub with native usage, and the Rules meter rises

**Dependencies:** Tasks 7, 8 (deployed)
**Estimated scope:** XS (operator)
