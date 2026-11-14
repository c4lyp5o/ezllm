# Bridge token usage audit — 2026-11-14

The historical ledger shows substantial Hermes context traffic, particularly
uncached Copilot input. It does not establish that all quota consumption is
unavoidable. This audit found accounting defects and an unsafe fallback path;
these are corrected in v1.9.0. Accounting corrections improve visibility and
must not be represented as equivalent reductions in vendor usage.

## Evidence and scope

Read-only snapshot of the local usage ledger, covering
2026-11-06T19:30:24.155Z through 2026-11-13T19:30:24.155Z (the latest recorded
call). Hermes attribution includes both operator-confirmed client keys,
`melissa` and `maxine`; dashboard and canary keys are excluded. Only metadata
and usage counters were inspected, not conversation bodies or credentials.
No subscription inference calls were made during the initial ledger-only phase.
The subsequent authorized live phase is recorded below. Earlier live
bridge acceptance checks are separate from this audit and can consume quota.

| Bridge | Successful / all calls | Cold input | Cache reads | Cache writes | Output | Cache-read share of input | Median messages / tools |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Claude | 166 / 169 | 421 | 21,486,234 | 1,136,572 | 136,814 | 94.97% | 206 / 24 |
| Codex | 240 / 251 | 3,899,267 | 11,849,600 | 0 | 41,146 | 75.24% | 67 / 24 |
| Copilot | 119 / 137 | 4,828,304 | 1,569,521 | 0 | 84,490 | 24.53% | 27 / 24 |

Token totals, cache shares and medians use successful calls only. **Correction
from the live phase:** Copilot's historical cold-input column includes any
unexposed cache writes. The old bridge dropped those fields on the wire, so
4,828,304 is ledger non-read input, not a verified total of truly cold tokens.
The cache-read share is unchanged by splitting non-read input into cold and write. Cache-read
share is read / (cold + read + write), not a monetary discount or a cache hit
rate measured at request granularity. Claude failures were three 401s, Codex
11 429s, and Copilot 18 400s. A failed response does not prove zero provider
work. Codex continuation segments can report zero usage before a later segment
reports a cumulative delta; these rows affect per-call medians.

Reconciliation across all successful bridge rows, including excluded clients,
found no mismatch between stored normalized token counters and stored raw usage
(Claude 168, Codex 249, Copilot 125 responses). This verifies ledger parsing of
what the bridges returned, not completeness against vendor billing. Historical
rows are not rewritten by the fixes below.

## Corrections

`run.go` `cliEvent.applyUsage` in Copilot now treats per-model `inputTokens`
as inclusive of cache tokens. Previously the fallback, used when primary
`tokenDetails` was absent, added cache again and could overstate input. The
normal primary-counter path was already inclusive. All reported model entries
are now included, and reasoning counters are retained even when primary
counters exist. Reasoning remains a subset of output, never extra output.
Recorded Hermes Copilot reasoning values were zero; that is not evidence of
zero reasoning work.

`bridge.go` `Server.chat` and `channel.go` `driveTurn` handling now restrict
plain-chat `-p` fallback to a positively identified pre-inference refusal with
no observed output, tools or usage. A `session.send` failure cannot authorize
another generation even if its error resembles tool registration failure.
Previously a tool-related failure after model work could cause another call.
Most Hermes requests carry tools and already cannot use the plain fallback;
this fix is not evidence that fallback caused the measured quota drain.

`stream.go` `StreamWriter.Handle` and `StreamWriter.Finish` in Claude now use
the terminal CLI result's aggregate usage. The recorded `m2_none.jsonl` fixture
contains two native model calls after an empty answer: each uses 416 input and
one output token; the terminal result reports 832 input and two output tokens.
Previously only the first message's usage reached the client. The first
message's content and stop reason remain isolated; text events still stream
immediately, while the terminal pair waits for final accounting. This exposes
internal retry costs; it does not disable those retries. The historical fixture
is not a measurement of retry frequency in the currently installed CLI.

Regression tests exercise these cases without vendor calls. No new process,
service, SDK or runtime dependency is introduced.

## What the bridges already do

Claude injects the caller's native request body, disables native tools, groups
historical frames without querying on each, and uses a stable conversation
session identifier. It does not intentionally generate an answer for each
historical user frame. Native internal retries remain an upstream limitation.

Codex resumes a parked tool continuation when matched and returns deltas of
cumulative usage rather than charging the same thread totals on every segment.
Other requests start fresh threads with the flattened caller transcript. Native
harness instructions still add input. Private working directories retain their
isolation; this audit did not measure their effect on vendor prompt caching.

Copilot uses replacement system instructions and caller tools through the
headless channel. Its warm pool reuses a process, not the conversation session.
Each request creates a session and sends flattened history. Normal channel
traffic avoids the older `-p` harness overhead, but safe startup fallback can
still incur it. Reusing sessions is not automatically correct: clients resend
history and can edit it, so naïve reuse could duplicate context or mix state.

The gateway does not retry a 429 on the same hop, but combo failover can try
another account. Transport and selected transient server errors can trigger
retries. Exactly-once vendor billing cannot be guaranteed after an ambiguous
network failure. Direct CLI calls and background live update gates may consume
subscription quota outside the request ledger.

## Observed efficiency limits and next actions

Large histories dominate the recorded input. Claude's successful requests have
206 messages at the median and approximately 140k cached-read tokens at the
median. Copilot has approximately 22k ledger non-read input tokens at the median;
the live phase below shows that these can include cache writes. Repeated
Copilot cache reads often plateau at 22,043 tokens while cold input grows. This
is consistent with a stable cached prefix and less cache reuse for flattened
history, but it is an inference, not a controlled cache experiment.

The configured `combo-safe` profile produced actual compression on zero Claude,
17 Codex and five Copilot successful Hermes calls, reporting respectively zero,
29,806 and 278 saved tokens. Its fresh-tool-output policy is `keep`, tool calls
are preserved, the last turn is exempt, and session memoization preserves bytes
once shipped. These choices protect fidelity and stable cache prefixes, but
profile selection alone does not mean substantial compression occurred.
Changing them blindly could damage tool results or prompt cache reuse.

The immediate operator actions are to compact long Hermes conversations before
history grows further, select a lower-cost model for routine steps, and send
only tools required for the task where Hermes supports it. Model choice,
reasoning and output length also matter; cached history is discounted rather
than universally free. Apply any context/output caps deliberately: the audited
bridge accounts have no configured local context or token caps, and arbitrary
truncation can break tool conversations. Live models, compression settings and
limits were left unchanged by this audit.

Use vendor usage dashboards to validate actual allowance consumption over a
matched time window after deployment. The ledger alone cannot establish the
provider's remaining subscription quota or the exact allowance cost of every
hidden native call. Further controlled replay should be explicitly budgeted. The live phase below
measures a small synthetic sample and makes no deployment savings claim.

## Provider references

- [Copilot SDK usage semantics](https://github.com/github/copilot-sdk/blob/main/docs/features/usage-and-billing.md)
  documents inclusive input counters and usage accounting.
- [Copilot billing](https://docs.github.com/en/copilot/concepts/billing-and-usage/individuals/billing)
  and [usage optimization](https://docs.github.com/en/copilot/tutorials/optimize-ai-usage)
  explain token/model costs and discounted cached input.
- [Codex pricing](https://learn.chatgpt.com/docs/pricing) distinguishes input,
  cached input and output consumption.
- [Claude Code usage and limits](https://support.claude.com/en/articles/14552983-models-usage-and-limits-in-claude-code)
  describes conversation history and cache effects on usage.


## Authorized native CLI experiment

On 2026-11-14 the operator authorized real calls and requested Hermes-shaped
requests. Eleven successful inference requests were made: Claude five, Codex
three and Copilot three. An additional Copilot request was rejected locally
before the CLI started because the harness initially supplied an unsupported
output limit. The initial nine-call plan expanded by two Claude calls to test
Hermes-style cache markers after the unmarked baseline exposed their importance.
No calls were made to Antigravity. All successful answers were `pong`; no new
tools were called. Stop after this bounded sample; no automatic live loop runs.

The isolated harness used current-checkout bridge HTTP handlers, installed native
CLIs and their existing login directories. It did not restart production, route
through combos/compression, insert usage rows in the production ledger, or run
Hermes itself. Codex retained its native workspace policy and tool lockdown;
the host harness did not reproduce the container's Landlock wrapper. These
limits matter when extrapolating cache behavior to production.

Each bridge received a short request and two identical larger requests. The
larger shape contained 50 synthetic conversation cycles with historical tool
calls/results, 24 function definitions and a final request to answer without
calling tools: 201 Messages-wire messages or 202 Chat-wire messages including
the system message. Requests were approximately 42–43 kB. Claude additionally
received a pair with ephemeral cache markers on the final three message blocks,
matching the existing Hermes cache-marker regression fixture. The bridge kept
the newest two client markers, reserving native's own two slots as designed.
These were synthetic tool schemas and content, not private Hermes transcripts.

Only numeric fields and protocol event types/methods were captured from CLI
stdout. The permanent [sanitized results](bridge-token-usage-live-results.json)
contain native counters and corresponding HTTP usage. CLI versions were Claude
2.1.311, Codex 0.167.0 and Copilot 1.0.105. Small models were selected to bound
quota: Haiku 5.5, GPT 5.6 Luna and GPT 6 Luna respectively. This is not a model
matched reproduction of the historical Opus/Terra/Sonnet mix.

### Findings that change the interpretation

| Repeated larger request | Native cold input | Native cache read | Native cache write | Total input |
| --- | ---: | ---: | ---: | ---: |
| Claude, no client history markers | 9,652 | 2,615 | 0 | 12,267 |
| Claude, Hermes-style markers | 4 | 12,263 | 0 | 12,267 |
| Codex | 12,595 | 0 | 0 | 12,595 |
| Copilot, reused warm process / fresh session | 3 | 0 | 7,405 | 7,408 |

Claude's marker-bearing repeat reused approximately 99.97% of input, compared
with approximately 21.32% without client history markers. This directly shows
why a superficially similar request without Hermes cache metadata is a poor
proxy for Hermes. All five Claude calls reported one positive native turn;
replayed-history acknowledgements reported zero turns and zero usage.

Codex's small request reported 3,261 input tokens, including 2,176 cached. Both
large requests reported zero cached input, with totals of 12,587 and 12,595.
That is observed failure to reuse cache in this pair, not proof that all Codex
traffic misses cache. Per-request working-directory/environment changes,
provider cache policy, tool transport and model differences remain hypotheses.
The bridge should not weaken filesystem isolation based on two samples.

Copilot's short request reported 126 input tokens. Each larger request reported
three cold tokens plus 7,405 cache-write tokens, zero cache reads, 15 output
tokens including eight reasoning tokens, and 93,342,500 nano-AIU. Both ran in
the same warm CLI process with fresh sessions. This demonstrates cache
population repeated in this sample; process reuse did not produce a cache hit.
It does not prove whether session identity, backend behavior or another prefix
component caused the miss, or how Sonnet would behave. No currency conversion
or allowance percentage is inferred from nano-AIU.

### Additional accounting fixes from real output

The live probe exposed two further omissions that the ledger-only comparison
could not detect:

- Copilot parsed native cache-write counters but `bridge.go` `openAIUsage`
  dropped them. It now emits `prompt_tokens_details.cache_write_tokens`, so
  the ledger can distinguish three cold tokens from 7,405 cache writes without
  changing the inclusive input total. Historical rows cannot be corrected from
  their old raw HTTP usage alone.
- Claude's aggregate result included `output_tokens_details.thinking_tokens`
  absent from the visible stream's usage. `stream.go` `StreamWriter` now
  preserves that breakdown in both terminal SSE and assembled JSON. One marked
  call reported 26 output tokens including 22 thinking tokens; reasoning is a
  subset, not added again to output.

Both new regression tests failed before the fixes and pass afterward. Captured
counters are also replayed through the production ledger usage tap without
vendor calls in `bridge_live_usage_test.go`
`TestLiveBridgeUsageCountersReachLedgerTap`. This validates ingestion of the
corrected envelopes; it is not a claim that the isolated live harness wrote
production ledger rows.

The live experiment supports investigating cache reuse, especially Copilot's
fresh-session path and Codex's per-request context. It does not justify a claim
of zero waste or an immediate session-retention rewrite. A follow-up comparison
should hold the actual Hermes model, tool schemas, cache markers and history
constant and change one session/prefix variable at a time, with an explicit
call budget.

## Cache follow-up, including Antigravity

A further 11 authorized successful model calls tested Antigravity, stable context
identifiers and native conversation retention. They used synthetic content and
numeric-only native capture, with no production restart or ledger insertion.
Detailed evidence is in
[bridge-cache-experiment-results.json](bridge-cache-experiment-results.json).

Antigravity CLI 1.4.12 on `gemini-3.8-flash-low` reported 6,802 input tokens for
the short request. Its 202-message, 24-tool Hermes-shaped request and identical
repeat both reported 17,745 input tokens, 19 output tokens and zero cache reads.
The experimental structured-output adapter was enabled and both returned the
required answer. Native result usage matched bridge HTTP usage. Gemini Flash
therefore showed no cache benefit in this pair; this does not establish cache
behavior for Antigravity's Claude models or a retained native conversation.

Two Codex calls with a fixed isolated working directory both reported 12,587
input tokens and no cache reads. Two Copilot calls recreating a fixed session
UUID in separate processes still wrote 7,405 cache tokens each and read none.
These experiments did not justify changing working-directory isolation or
reusing public session IDs as a cache optimization. The installed Codex 0.167.0
thread-start schema exposes no separate prompt-cache-key field.

The retained Copilot-session probe used two calls and showed 7,405 cache reads
on the next user turn. A further two-call proof through the implemented bridge
HTTP handlers reported 7,405 reads, 68 writes and three cold input tokens on the
continuation. Native cumulative counters matched HTTP per-request deltas.
Reported credits were 9,435,000 nano-AIU versus 93,392,500 on the initial call,
about 90% lower for that continuation. Wall times were approximately 1.7 seconds
and 7 seconds respectively. This comparison uses a growing conversation and a
small model, not identical stateless requests or a model-matched production
Hermes run.

The resulting v1.9.0 feature is a bounded opt-in completed-history cache inside
the existing Copilot warm workers. It reuses only an unchanged prior history and
answer plus one new user message with the same model/system/tools. It reports
usage deltas and deletes sessions on mismatch or normal eviction. It does not
retain pending tool batches or tool-result continuations; those still follow
the existing fresh-session path. See the
[completed-session cache design](copilot-completed-session-cache-design.md) for
matching, limits, rollout and qualification. Production remains unchanged until
an updated build is deployed with both warm-pool and session-cache flags enabled.


## AGY cache resolution and accounting correction

The follow-up in [agy-conversation-cache-design.md](agy-conversation-cache-design.md)
used eight more authorized native CLI turns. Gemini Flash caching worked with a
retained conversation and with explicit resume in a new process using the same
private workspace. The implemented HTTP caller-tool round trip reused 15,733
tokens and reported 2,733 new input tokens after 17,791 new input tokens on the
initial turn. This measures cold input, not vendor credits. Production remains
unchanged. The v1.9.0 opt-in cache includes complete external tool-result groups
without retaining an idle CLI process or executing tools in ezllm.

The experiment also disproved the old AGY input mapping: native input excludes
cache reads. Adding them to the OpenAI prompt total fixes the ledger's cold/read
split, and resumed cumulative counters are returned as per-request deltas. Old
cached rows are unchanged; earlier zero-cache Gemini measurements are unaffected.
Captured counters and validation checks are in
[agy-cache-experiment-results.json](agy-cache-experiment-results.json).


### Codex empty-template cache follow-up — 2026-11-14

The earlier fresh-thread cache gap now has an opt-in implementation:
`EZLLM_CODEX_BRIDGE_CACHE=on`. Empty configuration parents feed independent
prediction forks across fresh CLI processes, without a paid priming turn.
The synthetic Hermes-shaped implementation proof read 12,416 cached tokens of
12,657 input on its second request (98.1%). Native caller-tool continuation and
parent deletion passed, including Landlock confinement. See the
[design and limits](codex-empty-template-cache-design.md) and
[numeric evidence](codex-cache-experiment-results.json).
Exec fallback now requires positive pre-inference evidence; silence after
`turn/start` cannot trigger a duplicate paid generation.


### Codex automatic default — v1.9.0

Codex cache selection now defaults to `auto`. Supported CLIs enable the tested
empty-template path; unsupported CLIs keep the original path with a reason in
`/status.cache_selection`. Explicit `on` requires support and `off` disables it.
The native CLI approval policy is pinned to `never` at startup, thread creation
and resume. Caller-tool instructions continue already-authorized work without
repeated permission questions, while respecting actual caller approval requirements.
The real CLI auto-mode live gate passed plain reuse, caller-tool continuation and
native parent cleanup without setting the cache environment variable.
