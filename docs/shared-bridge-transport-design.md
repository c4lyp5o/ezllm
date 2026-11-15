# Shared CLI bridge streaming transport

Implemented in v1.8.0 for Claude and Antigravity, and in v2.0.0 for Codex and
Copilot. All four subscription bridges now stream through `bridgeutil.Stream`.

## Boundary

`internal/bridgeutil` `Stream` owns writing already-encoded bytes, flushing,
write/flush deadlines and context cancellation. It implements `io.Writer`; it
has no dependency on either provider package or inference wire protocol.

Claude's `StreamWriter` still encodes Anthropic named events. Antigravity's
`sseWriter` still encodes OpenAI chunks and `[DONE]`. Each adapter owns headers,
when the response starts, error framing, completion and usage. Claude continues
opening its stream before the CLI runs; Antigravity continues delaying headers
until its first delta, preserving HTTP errors for failures before output.

Authentication headers/refusals, request parsing, admission/queue timeouts,
CLI commands/environment, process cleanup, provider accounting and quota
interpretation are unchanged. No universal bridge interface or process pool
was introduced by this extraction.

## Transport behavior

- Each event write and flush has a 60-second stall deadline.
- Cancellation sets an immediate deadline, interrupting a blocked supported
  HTTP writer. Failed writes, short writes and flush errors propagate to the
  adapter/runner. Once failed, no later bytes or terminal marker are written.
- Closing detaches or joins the cancellation callback before clearing the
  deadline. A callback cannot mutate the connection after handler cleanup.
- Writers without deadline/flush support preserve the existing best-effort
  behavior via `http.ErrNotSupported`. Production HTTP writers support the
  response-controller deadlines; wrappers should expose `Unwrap` when needed.
- One handler owns writes and calls `Close` after its final write. Only the
  cancellation callback runs concurrently; the type is not a general concurrent
  event multiplexer.

Claude gains explicit stalled-write protection. Antigravity's existing
protection now also covers terminal writes/flushes and joins cancellation before
cleanup. The shared layer neither re-encodes events nor invents usage.

## Baseline and validation

Before changing either adapter, the pinned Antigravity 1.4.7 passed preflight
(six models) and a live runner turn (about 6.0 s, positive input/output usage,
zero tool steps). The deployment's current CLI 1.4.12 also passed a live runner
turn (about 6.6 s).

HTTP requests using the actual 1.4.12 CLI were tested before and after migration:

| HTTP mode | Before | After |
|---|---:|---:|
| Non-stream | 5.867 s | 5.044 s |
| Stream, usage requested | 6.143 s | 5.403 s |

Both pairs returned the requested diagnostic answer and usage; streaming
completed with `[DONE]`. These are single samples per mode, not evidence of a
speed improvement. The opt-in `TestLiveHTTP` exercises the real HTTP adapter;
normal short tests do not invoke subscription inference.

Shared tests cover byte preservation for both formats, flush errors, short
writes, sticky failure, blocked-write cancellation, stall expiry, deadline reset
and repeated cancellation/close races. Adapter regressions prove deadlines are
used and only one failed write is attempted, including error/terminal paths.
Existing Anthropic/OpenAI framing, auth, usage and process tests remain intact.
Claude validation for this extraction uses regression fixtures, not live
subscription inference.

## Migration status and remaining work

| Bridge | Shared `bridgeutil.Stream` | Remaining work |
|---|---|---|
| Claude | Implemented in v1.8.0 | Maintain Anthropic framing and stalled-write/cancellation coverage. |
| Antigravity (`agybridge`) | Implemented in v1.8.0 | Maintain OpenAI framing and live HTTP acceptance coverage. |
| Codex | Implemented in v2.0.0 | Maintain OpenAI framing; app-server RPC, sandboxing, parked tool turns and usage stay adapter-owned. |
| Copilot | Implemented in v2.0.0 | Maintain OpenAI framing; image attachments, early tool delivery, exact usage and warm workers stay adapter-owned. |

Codex and Copilot were migrated in separate commits (2026-11-15). Header timing,
error framing and tool/finish/usage ordering are unchanged; a failed write is now
sticky for both, and Copilot no longer ignores write errors (a client that stops
reading ends the turn). Each passed `make test-quiet`, targeted `-race -short`
runs, vet and a static build, plus live acceptance: Codex `TestLiveHTTPSurface`
(streamed text, usage, `[DONE]`) and `TestLiveToolRoundTripWithContinuation`;
Copilot streamed `gpt-6-luna` text with usage and a `get_weather` tool call with
`finish_reason: tool_calls`, usage and `[DONE]`, through the branch's bridge.

The next scope is transport reuse, not a universal bridge lifecycle. Shared
authentication mechanics, admission primitives and timing helpers remain later
candidates after equivalent behavior is established. Process pools, sessions,
CLI environments, sandboxing and provider accounting stay adapter-owned.
