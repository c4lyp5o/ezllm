# Implementation Plan: claude-bridge M2 — tool calls on claude-sub

**Historical plan:** This checklist records the original plan and is not current
status. Implementation shipped; [todo.md](todo.md) records the historical task
evidence and unchecked operator follow-ups, whose live status needs verification.
Use `make test-short` for local testing.

Spec: [SPEC.md](../SPEC.md) (approved 2026-10-21). Task checklist: [todo.md](todo.md).

## Overview

Make the `claude-sub` hop accept Anthropic Messages requests that carry tools, and
return `tool_use` blocks under the client's own tool names without the CLI ever
executing anything. That is enough for Hermes agents like Max to run on it. The
first step is a spike of the cheap **extra-body passthrough** (approach A). The
inert MCP server (approach B) is a contingency that only gets planned if A fails a
must-hold property. A Rules-based 5h drain cap goes live before Max is moved back.

## Architecture Decisions

- **Spike before code (Task 1).** Every frame fixture and every comment that says
  "measured" comes from real CLI output. The spike decides between A and B, records
  the cache numbers (the M3 trigger), and yields tokens per turn, which sizes the cap.
- **Requalify the CLI first (Task 0).** 2.1.299 is gone from the host
  (`~/.local/share/claude/versions/` holds only 2.1.304–306). The running container
  survives only because its bind mount still holds the deleted file. Recommended:
  qualify the newest version and copy it to a path the updater doesn't manage
  (e.g. `~/.local/share/claude-bridge/claude-<ver>`), then point `CLAUDE_CLI_BIN`
  at it so the next auto-update can't break the mount again.
- **Vertical slices.** Each Phase 2 task delivers one client-visible tool
  capability end to end (parse → spawn → stream/non-stream → test), not one layer.
- **Contract change, not a weakened test.** The M1 assertion "tools → 400" in
  `server_test.go` and in `tests/prove_claude_bridge.py` is replaced on purpose, and
  the commit says so.
- **Cap through Rules.** This is configuration only, with no code change
  (`internal/rules`, Rules page).
- **ezllm core stays untouched (locked decision 4).** The OpenAI→Messages tool
  translation already exists (`internal/translate/codec_messages.go:479` maps
  `parallel_tool_calls`). Task 5 only proves the Hermes path works; it adds a
  translate test only if it finds a gap.

## Dependency Graph

```
T0 CLI requalify + stable pin
 └─ T1 Spike (A vs B, fixtures, cache, tokens/turn)
     ├─ [Checkpoint 1: A/B go-no-go + cap proposal → Calypso]
     ├─ T2 single tool call (stream + non-stream)
     │   ├─ T3 tool history round-trip
     │   │   └─ T4 tool_choice + server-tool refusal
     │   │       └─ T5 Hermes path through ezllm
     │   │           └─ [Checkpoint 2]
     │   │               ├─ T6 live proof + /status
     │   │               └─ T7 docs + version 0.0.22
     │   │                   └─ [Checkpoint 3: CI green, Calypso push/deploy]
     └─ T8 drain-cap rules (needs T1 numbers; needs deploy)
         └─ T9 Max back on claude-sub (operator)
```

T8 can be configured in parallel with T2–T7, but has to be live before T9.

## Task List

### Phase 0: Foundation
- [ ] Task 0: Requalify the CLI and pin it stably
- [ ] Task 1: Spike approach A on the pinned CLI

### Checkpoint 1: Spike verdict (human)
- [ ] A/B verdict with evidence for each must-hold property
- [ ] Cache read/create table (5 turns), M3 needed or not
- [ ] Proposed 5h cap values for sonnet/haiku/opus, approved by Calypso

### Phase 1: Core (approach A)
- [ ] Task 2: Single tool call works (stream + non-stream)
- [ ] Task 3: Tool history round-trip (tool_use → tool_result → answer)
- [ ] Task 4: tool_choice variants + typed refusal for server tools
- [ ] Task 5: Hermes path through ezllm's OpenAI surface

### Checkpoint 2: Core
- [ ] `go test -short -count=1 ./internal/claudebridge/ ./cmd/ezllm/ ./internal/translate/` green
- [ ] `make test-quiet` green

### Phase 2: Ship
- [ ] Task 6: Live proof + /status capability
- [ ] Task 7: Docs + version bump 0.0.22
- [ ] Task 8: Drain-cap rules on claude-sub
- [ ] Task 9: Move Max back onto claude-sub (operator)

### Checkpoint 3: Complete
- [ ] CI full gate green on the branch
- [ ] Live proof passes against the deployed build
- [ ] Calypso pushes/deploys (operator gate, locked decision 12)

## Contingency: approach B

If Checkpoint 1 rules A out, stop. Tasks 2–5 are re-planned around the inert MCP
server, roughly:
- B1: an `ezllm` subcommand serving stdio MCP that advertises the request's tools
  and denies execution
- B2: `mcp__client__<name>` aliasing (hashed above 50 chars), with names rewritten
  in history and responses
- B3: runner wiring (`--mcp-config`, `--disallowedTools` for everything except
  the MCP server's tools)

B needs approval (SPEC "Ask first") and gets its own task breakdown then.

## Risks and Mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| Native rejects or rewrites `tools` coming from the extra body | High | Spike tests it first; fall back to B |
| Native acts on an unknown `tool_use` (injects an error `tool_result`, runs a second turn) | High | Spike observes it; Task 2 suppresses it in the stream, `--max-turns 1` stays; a sentinel test proves nothing executes |
| Container recreate breaks the bridge (deleted 2.1.299 inode) | High | Task 0 before any deploy |
| The CLI updater deletes the newly pinned version too | Med | Pin a copy outside `~/.local/share/claude/versions` |
| Large tool lists make cache creation dominate cost | Med | Spike measures 5 turns; M3 decision at Checkpoint 1 |
| Max drains the Pro window | Med | T8 cap before T9; the cap counts cache-cold tokens only (accepted gap) |
| Running the gate locally stalls the load balancer | Med | `make test-quiet` / focused packages only; full gate in CI |

## Open Questions

- Which CLI version to qualify (recommended: the newest installed, 2.1.306)? This
  is decided at Task 0.
