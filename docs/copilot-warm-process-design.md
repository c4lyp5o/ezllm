# Copilot warm-process exploration — 2026-11-11

Status: feasibility measured; production pooling is not implemented.

## Finding

The same official CLI headless process can serve successive **fresh sessions**.
This removes most repeated handshake and session-creation overhead without
requiring conversational state to survive across HTTP requests. This differs
from holding a session across a tool loop, the option considered in
[copilot-cli-bridge-design.md](copilot-cli-bridge-design.md) §10.

A controlled live probe used CLI 1.0.105, model `gpt-6-luna`, the existing
`sessionConfig` restrictions and `driveTurn` accounting path. It ran two fresh
process text requests, then text, two caller tools, and text on one process.
Each turn created and deleted its own session. The temporary opt-in probe was
removed after the run; normal tests do not invoke live inference.

## Measurements

Milliseconds, rounded; these are individual samples, not percentiles:

| Request | Ping/readiness | Session create | First assistant event | Total turn |
|---|---:|---:|---:|---:|
| Cold text 1 | 983.5 | 1275.0 | 4932.6 | 5095 |
| Cold text 2 | 1066.9 | 1289.0 | 4992.3 | 5151 |
| Reused-process sequence: first text (still cold) | 887.7 | 1230.1 | 5265.7 | 5479 |
| Same process: fresh two-tool session | 0.6 | 34.3 | 4075.1 | 4148 |
| Same process: fresh text session after tools | 0.5 | 38.4 | 4920.3 | 4996 |

The first-event clock starts inside the channel, excluding OS spawn and slot
queueing. Total cold turns also include spawn and process reaping; the reused
turn totals exclude the shared process lifecycle.

Warm setup took 35–39 ms versus 2258–2356 ms in the two cold controls: roughly
2.2 seconds less setup. Model wait varied: 2.63–2.67 s in the cold controls,
4.04 s in the warm tool request and 4.88 s in the final text request. The final
warm text total was therefore close to the cold totals despite faster setup.
These observations support a setup improvement, not a promised total speedup.

Both warm plain requests returned the requested diagnostic marker and no tool
calls. The middle request returned the expected two-call batch. All five turns
reported positive prompt usage, and the reused connection's event queue was
empty after each turn. This is a behavioral smoke check, not proof against all
cross-session leakage: delayed events can still be in the OS pipe, and only one
model and one sequence were tested. Responses and credentials were not printed.

## Recommended production design

Use a small process pool, capped by the existing concurrency setting, with one
leased process serving one HTTP request at a time. Keep a fresh session for
every request. Initially make pooling opt-in and retain the current cold path
as the rollback mode. No session IDs need to become part of the client API.

A worker owns the process, stdin/stdout, one `rpcConn`, stderr capture and its
lifetime context. Its lifetime must be separate from an individual request's
context. A process becomes reusable only after successful cleanup. Keep the
existing system-message replacement, explicit `availableTools` allowlist,
empty MCP configuration and disabled permission/user-input handlers per session.

The existing `slotted` queue should remain the single admission control; avoid
a second independent queue inside the pool. Start workers lazily, allow idle
workers to expire, and measure retained RSS before choosing a default idle TTL.
Prewarming can be an explicit later option; it should not generate model calls.

### Required changes before reuse is safe

- **Confirm cleanup.** `driveTurn` currently ignores deletion failures because
  `withChannel` kills the process afterward. A pool must surface abort/drain and
  deletion outcomes and discard a worker on timeout, malformed frame, failed
  deletion or uncertain usage boundary. Never recycle merely because a callback
  returned successfully.
- **Route session events.** `rpcMessage.Params` currently retains the event but
  drops its session ID. Preserve the envelope's session ID and reject or discard
  events from completed sessions before they can affect a new turn. Retain
  JSON-RPC request IDs across leases; do not reset a live connection's counter.
- **Cancel safely.** On request cancellation or blocked client writes, terminate
  and reap the leased process. Replace it for later work. Do not let a cancelled
  turn's timer or close callback fire against a subsequent lease.
- **Recover without duplicate inference.** Replace a dead idle worker before
  work is sent. Do not replay a turn after `session.send` may have reached the
  CLI; its billing and output are then uncertain. Preserve typed failures and
  the existing pre-inference-only plain fallback behavior.
- **Bound lifetime and state.** Limit idle time and requests per worker; recycle
  after a CLI path/version change or credential reconfiguration. Preserve the
  protocol-version ping. A ping alone cannot detect a replaced binary: an old
  running process may speak the same protocol indefinitely.
- **Shut down explicitly.** Add runner shutdown that rejects new leases, ends
  active work within the gateway grace period, and reaps idle/active workers.
  The current bridge shutdown only owns the HTTP server because requests own
  their children. Pooling changes that ownership.

### Acceptance tests

1. Two isolated sessions on one PID, then tools followed by an empty allowlist;
   verify initialized tool metadata as well as observable assistant behavior.
2. Delayed old-session events, pending server requests, and queued usage cannot
   contaminate the next session or its ledger row.
3. Cancel or timeout during ping, send, tool drain, delete and client writes;
   the worker is reaped and the next lease starts on a healthy process.
4. Kill idle and active workers; replace safely without replaying a possibly
   billed request. Preserve quota/auth/model refusal classifications.
5. Concurrent requests never share one worker; existing queue capacity,
   rejection and cancellation semantics hold.
6. Binary replacement, idle expiry, lifetime cap and shutdown leave no children
   or goroutines behind. Retained RSS stays bounded under repeated sessions.
7. Interleaved cold/warm benchmarks with identical prompt, model, tools and
   history compare setup, first output, completion and exact usage separately.

## Next step

Implement the opt-in pool only with the lifecycle and isolation tests above.
The measured setup saving justifies that work; the current probe does not justify
enabling reuse by default. Keep the one-request/one-session contract and the
exact usage collection introduced by the existing bridge.
