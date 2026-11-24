# Copilot bridge latency diagnosis — 2026-11-11

Read-only analysis of the local usage ledger, with call records through
2026-11-11T01:49:55.593Z. Copilot's latest record is 2026-11-08; these are
historical measurements, not a live benchmark. No prompts or credentials were
read for this analysis.

## Observed latency

Successful streaming calls, nearest-rank p90:

| Population | Calls | Median first response | p90 first response | Median total | p90 total |
|---|---:|---:|---:|---:|---:|
| CLI bridge (`copilot-sub`) | 53 | 15.820 s | 47.029 s | 16.449 s | 47.029 s |
| CLI bridge, November 6 onward | 35 | 14.966 s | 22.355 s | 15.430 s | 24.113 s |
| Retired integration (`github-copilot`) | 20 | 2.740 s | 4.233 s | 3.688 s | 4.643 s |

The retired integration is not a controlled baseline: different dates, request
sizes and implementation. Do not use this comparison to attribute the increase
solely to the CLI.

The current bridge population has median 21,147 cold input tokens, 14,366 cached
read tokens, 516 output tokens, 20 messages and 23 advertised tools. These are
independent medians. Of 53 calls, 52 advertise tools; this does not prove that
the responses actually called tools. In 41 calls, first response and total time
are within 1 ms, consistent with whole-turn delivery rather than early text.

## Code paths explaining possible delay

- `internal/copilotbridge/channel.go` `withChannel` spawns a fresh CLI process
  per request. `driveTurn` then waits for ping, session creation and send before
  consuming model events. Startup and model work both precede first output.
- `driveTurn` forwards text deltas immediately. Declared tool calls take a
  different path: `drainAborted` waits for abort acknowledgement and usage until
  idle (up to 15 seconds), followed by deferred session deletion (up to 5
  seconds). `internal/copilotbridge/bridge.go` `chat` emits the tool-call batch
  only after `RunChannel` returns. Thus up to 20 seconds of bounded cleanup
  can precede tool delivery; this is a code bound, not a measured contribution.
- `internal/copilotbridge/run.go` `slotted` may queue requests before starting
  the turn. Ledger timing includes the externally observed wait; the runner's
  first-byte statistic starts inside the slot and does not include queue time.
- A plain request can fall back to the separate `-p` process if channel setup
  fails. The ledger does not distinguish this fallback from the channel path.

## Conclusions and next measurement

The ledger establishes slow first output and frequent whole-turn delivery.
Large conversations and the tool cleanup path are plausible contributors;
these records cannot separate queue, process startup, model generation, abort
usage collection and deletion. Reasoning-token counts of zero do not prove
absence of model reasoning.

Before changing timeout or accounting behavior, measure queue wait, ping,
session creation, first model event, tool batch arrival, abort drain and delete
separately on a controlled text request and tool request. Compare client first
output with internal first-event timing. Preserve exact usage collection and
client-tool isolation when optimizing. No live inference was initiated by this
analysis.

## Test synchronization fix

`internal/copilotbridge/channel_test.go`
`TestRunChannelDeathMidTurnIsClassifiedFromStderr` now consumes each framed
request and checks its method before replying. The child cannot exit before
ping is written. The quota classification assertion is unchanged; the repaired
fixture passed 100 consecutive isolated runs.

## Mitigation and controlled live proof

`driveTurn` now invokes a synchronous `ChannelTurn.OnTools` callback as soon as
it has the complete declared batch. `chat` flushes this batch once, then waits
for the existing accounting and deletion path before emitting the finish reason,
usage (when requested), and `[DONE]`. Non-stream requests still wait for the
complete result. Clients that wait for stream completion before executing tools
will still wait for cleanup; this change improves early visibility, not every
client's full tool-loop time. No timeout was shortened and no usage was estimated.

Regression tests block cleanup until the HTTP client has received the batch,
verify the batch is emitted once, and check usage/completion after release.
Delivery-error coverage also verifies abort, deletion and usage collection.

Two controlled local calls through CLI 1.0.105 and model `gpt-6-luna` succeeded:

| Request | First output | Tool batch | Complete stream | Ping/readiness | Session create | Session delete |
|---|---:|---:|---:|---:|---:|---:|
| Short text | 10.178 s | — | 10.288 s | 4.529 s | 1.631 s | 0.057 s |
| One diagnostic tool | 5.766 s | 5.766 s | 5.840 s | 0.917 s | 1.343 s | 0.033 s |

Both returned usage and completed the stream. The tool call's abort RPC took
0.015 s. A preliminary forced-tool-choice request was rejected locally without
inference; the successful tool proof used the supported automatic choice.
These are two small samples, not an A/B benchmark or a production speedup claim.
They show significant startup/session cost and only about 74 ms after tool
arrival in that tool request. Warm-process reuse merits a separate design and
proof covering session isolation, cancellation, process crashes and concurrency.

### Reading stage timings

The loopback bridge `GET /status` returns `runtime.stage_timings`.
Each observed stage has `count`, `avg_ms`, and `last_ms` (fractional milliseconds).
Counters aggregate across requests and probes; concurrent requests mean last
values may belong to different calls. They are diagnostics, not percentiles.

- `queue`: wait for an acquired CLI slot, including immediately acquired slots.
- `spawn`: OS process-start call only; CLI readiness is included in `ping`.
- `ping`, `session.create`, `session.send`, `session.abort`, `session.delete`:
  elapsed time of each RPC, including failed calls.
- `first_event`: channel-turn start through first assistant text/message event.
- `model_wait`: send acknowledgement through that first assistant event.
- `tool_batch`: channel-turn start through the complete declared batch.
- `usage_drain`: abort request through checkpoint/idle collection or its bound.

Overlapping timings must not be summed. Queue aggregates also include plain
fallback requests; RPC stages describe only the headless channel. No prompt,
credential, tool name, arguments or session identifier is stored in these stats.

A subsequent [warm-process exploration](copilot-warm-process-design.md) measured
roughly 2.2 seconds less setup on fresh sessions within one process. It records
the live samples, their limits, and the worker lifecycle/isolation requirements
before a production pool can be enabled.
