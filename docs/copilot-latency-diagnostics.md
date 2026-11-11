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
