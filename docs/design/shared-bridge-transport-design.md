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

## Shared helpers beyond the stream (v2.2.0)

| Helper | Users | Not shared |
|---|---|---|
| `bridgeutil` `SetProcessGroup` / `KillGroup` (`proc_unix.go`, `proc_windows.go`) | Claude, Codex (app-server and exec fallback), Antigravity | Copilot does not start CLIs in a process group. |
| `bridgeutil` `OpenAIErrorEnvelope` (`errors.go`) | Codex, Antigravity | Claude keeps its Anthropic envelope (`"type":"error"`, derived default code); Copilot keeps its `"param": null` variant. |
| `bridgeutil` `ChatChunks` (`chatchunks.go`): SSE headers, `chat.completion.chunk` encoding, usage chunk, `[DONE]` | Codex, Copilot, Antigravity | When headers go out, what a delta carries (Codex adds `reasoning_content`), the opening role chunk (`RoleChunk`: Codex and Antigravity yes, Copilot no) and finish/usage ordering stay in each adapter. Claude streams Anthropic events. |
| `bridgeutil` `Slots` (`admission.go`): `MaxConcurrent` slots, a bounded wait queue (`DefaultMaxQueue` 8), `/status` counters | All four | Each bridge maps `ErrQueueFull` to its own 429 `queue_full` (`Retry-After: 5`) and a slot-wait timeout to its existing 504. Codex's template cleanup uses `Hold`, which ignores the queue bound and counters. |
| `bridgeutil` `Authorized` (`admission.go`): constant-time bridge-token check | All four | The refusal body: Claude 401 `authentication_error` (Anthropic) and accepts `x-api-key`; Codex, Copilot and Antigravity 401 `invalid_request_error` (OpenAI). |
| `bridgeutil` `TailBuffer`, `WriteJSONBytes`, `RandomHex` (`util.go`): stderr tail for error reports, raw JSON response, ids | Tail: Claude, Codex, Antigravity (Copilot keeps its own stderr capture); the others where each bridge used them | Each bridge's `writeJSON` (encode-failure envelope) stays local. |
| `cmd/ezllm` `bridgeTokenFromFile`, `serveBridge` | All four | Each bridge logs its own "listening" facts before `serveBridge`. |

The process and envelope helpers were byte-identical copies, so moving them
changes no wire output or process behavior. `ChatChunks` keeps each bridge's
event bytes (`TestChatChunksWireBytes` pins them) with two header changes for
Copilot: it now sends `X-Accel-Buffering: no` like the others
(`TestStreamDisablesProxyBuffering`), and it no longer sends
`Connection: keep-alive`, which Go's server handles itself. `TestKillGroupReachesGrandchild` proves a grandchild of the CLI dies
with the group (it fails when the group kill is removed);
`TestOpenAIErrorEnvelopeShape` pins the envelope bytes.

## Admission and auth changes (v2.2.0)

These change what clients see, deliberately, so every bridge behaves alike:

- Claude, Codex and Antigravity used to let a request wait for a slot until the
  runner timeout (504). They now bound the wait queue like Copilot did: past
  `EZLLM_BRIDGE_QUEUE` waiting requests (one setting for every bridge,
  default 8; it replaces `EZLLM_COPILOT_BRIDGE_QUEUE`) they answer 429
  `queue_full` with `Retry-After: 5`, and the router parks the hop for 5 s and
  fails over (`hop_health.go` `hopOutcome`; a sole hop is not parked).
- Every bridge's `/status` reports `runtime` with `in_flight`, `queued`,
  `max_queue`, `max_concurrent`, `served` and `rejected_queue_full`
  (Antigravity keeps `conversation_cache` inside `runtime`).
- Codex's 401 type changed from `authentication_error` to
  `invalid_request_error`, matching OpenAI and the other OpenAI-shaped bridges.

Tests: `bridgeutil` `TestSlotsBoundTheQueueAndCount`,
`TestSlotsQueuedCallerLeavesOnCancel`, `TestSlotsDefaultsAndHold`,
`TestAuthorized`; per bridge `TestRunnerRefusesWhenTheQueueIsFull` (429,
`Retry-After`, immediate) and Codex `TestRefusalUsesOpenAIErrorType`.

Observability added in the same release: `Slots` records each request's wait
for a slot (`TrackSlotWait`, header `X-Ezllm-Slot-Wait-Ms`, ledger column
`calls.slot_wait_ms`) and logs a throttled `WARN` when it refuses; the live
counters reach the dashboard through `/admin/bridges` `runtime`. Tests:
`TestTrackSlotWait*`, per bridge `TestAuthedReportsTheSlotWait`,
`TestForwardRecordsBridgeSlotWait`, `TestSlotWaitMigrationFromV36`,
`TestQueueFullWarnsOncePerWindow`, `TestBridgesServeLiveRuntime`.

Nothing else is planned for sharing. Process pools, sessions, CLI environments,
sandboxing and provider accounting stay adapter-owned; there is still no
universal bridge lifecycle.
