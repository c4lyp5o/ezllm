# Completed Copilot session cache

## Decision

Reuse a normally completed native conversation only when the next caller request
contains the same prior history and assistant answer, followed by one new user
message. This is an opt-in extension of the existing warm pool, controlled by
`EZLLM_COPILOT_BRIDGE_SESSION_CACHE=on`; it requires
`EZLLM_COPILOT_BRIDGE_WARM_POOL=on`. Both default off. It adds no dependency,
service, RPC endpoint, separate worker pool or persistent ezllm cache.

The bridge still receives full caller history. `session_cache.go`
`canonicalCacheHistory` produces bounded normalized message records;
`completedSession.continuation` compares them and the model/system/tool
configuration. Text blocks are normalized to the same text the parser sends.
Hermes's moving `cache_control` markers and empty `tool_calls` metadata do not
constitute conversation edits. Actual tool arguments, including fields named
`cache_control`, remain data. History edits, changed answers/configuration,
images, `auto`, identical stateless retries, tool-result follow-ups and multiple
new messages cannot match. No caller-controlled session ID selects native state.

## Why retain a conversation

On Copilot CLI 1.0.105, repeated fresh-session Hermes-shaped requests each
reported three cold input tokens and 7,405 cache-write tokens, with no reads.
Recreating a stable session UUID in separate processes did not improve that
sample. Keeping the native session and sending a new user turn did. Installed
CLI code associates cache lineage with its native session root event; the probe
shows that merely repeating the public UUID is insufficient here.

The actual bridge HTTP proof used GPT 6 Luna, 24 synthetic tools and 202 initial
messages. The continuation supplied 204 messages, including the exact previous
answer and new user question:

| Quantity | Initial call | Continued call |
| --- | ---: | ---: |
| Inclusive input | 7,408 | 7,476 |
| Cold input | 3 | 3 |
| Cache reads | 0 | 7,405 |
| Cache writes | 7,405 | 68 |
| Output, including reasoning | 16 | 23 |
| Reasoning subset | 9 | 16 |
| Reported nano-AIU | 93,392,500 | 9,435,000 |
| Wall time | 7,034 ms | 1,726 ms |

That continuation reused 99.05% of its input and reported 89.90% fewer credits
than the initial call. It is a small synthetic, model-specific sample, not a
production-wide discount guarantee or an identical-request benchmark. Native
cumulative snapshots were reconciled with the HTTP request deltas. Sanitized
numeric evidence is in [bridge-cache-experiment-results.json](bridge-cache-experiment-results.json).
No production configuration, deployment or ledger rows were changed.

## Lifecycle and accounting

Each idle warm worker can retain one completed session. The existing pool selects
a matching idle worker when available. Normal pool admission, queue and process
limits still apply. The existing one-minute idle timeout, 100-turn lifetime and
15-minute age limit bound retention; normalized history plus the expected answer
is capped at 1 MiB per worker. Busy workers are never shared, and concurrent
requests are not joined or response-cached.

Retention requires a nonempty answer, usage checkpoint and idle acknowledgement,
with no error, undeclared tool, pending tool batch or uncertain protocol state.
A mismatch deletes the previous session before creating a fresh one. Failed
cleanup does not start another model call. Idle eviction and graceful close
attempt bounded native session deletion before reaping the worker. Active
cancellation still terminates the child promptly. A hard crash cannot guarantee
cleanup of the CLI's own session files; ezllm writes no independent cache file.

`channel.go` `driveTurn` treats a retained native checkpoint as cumulative. `session_cache.go` `usageDelta`
subtracts the previous counters for input, cache read/write, output, reasoning,
premium requests and nano-AIU. Negative/reset counters fail the turn and discard
the worker; they never trigger a repair inference. Streaming usage is emitted
from the finalized per-request result.

A continued request can select a caller tool. The existing batch-delivery,
abort/accounting and session-deletion path then runs; the pending tool session
is not retained. Its next tool-result request starts fresh. This first cache
mode therefore improves completed-answer continuations, including requests that
have tools available, but does not solve repeated input across an active Hermes
tool loop. Native reasoning already held by a matching session stays native;
this mode deliberately permits conversation continuity rather than promising a
stateless reconstruction of hidden reasoning.

## Qualification and diagnostics

`bridge-check copilot --live` replaces its plain probe with a two-turn native
continuation proof when the cache mode is on. It verifies actual session reuse,
answer shape and nonempty per-request usage; it cannot require a provider cache
hit for a tiny prompt. The tool-batch/replay proof still runs. Cache mode adds
one small inference call to the default live gate.

The updater copies the session-cache environment setting and requires
`policy.session_cache=completed-prefix` when configured. An older gate that
ignores the mode cannot qualify it. Cache-disabled policy remains compatible.

The bridge's loopback `/status` reports `session_cache`; a matching continuation adds
to `runtime.stage_timings.session_cache_hit.count`. It logs no history, prefix,
hash or model output. A lack of hits can mean expired/busy workers, moving actual
system text/tool definitions, edited or non-prefix history, tool continuations,
images, automatic model selection or an over-limit request. The counter identifies
matched native-session reuse attempts, not a particular vendor token discount.

## Tests

`session_cache_test.go` covers exact and edited histories/configuration, moving
Hermes cache metadata, protected tool-argument data, HTTP reuse with delta billing,
tool-batch deletion, failed cleanup without inference, regressed counters,
default-off behavior, worker selection, idle deletion, graceful close and the
cache-mode live gate using subprocess fixtures. `bridge_live_usage_test.go`
replays measured counters through the ledger tap without vendor calls.
