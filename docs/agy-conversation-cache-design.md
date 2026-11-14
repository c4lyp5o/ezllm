# Antigravity conversation cache

## Decision

Offer bounded native conversation resume for experimental caller-tool requests.
`EZLLM_AGY_BRIDGE_SESSION_CACHE=on` requires
`EZLLM_AGY_EXPERIMENTAL_TOOLS=on` and CLI support for `--conversation`.
The default remains off. Plain text-only requests retain their current fresh
conversation behavior.

Each request still starts and exits one official CLI process. Only the opaque
conversation ID, bounded normalized caller history, expected assistant reply,
native usage baseline and private workspace are retained. There is no idle CLI
process, ACP/MCP endpoint, extra service, dependency or independent on-disk
metadata cache. The schema file stays in the private workspace while eligible
for reuse. The CLI continues managing its own backend conversation storage.

The official [headless protocol](https://antigravity.google/docs/cli/headless/)
supports explicit conversation resume and describes cumulative session metadata.
The installed CLI 1.4.12 was also tested directly and through the bridge.

## Native evidence and accounting correction

Gemini Flash showed no reads in previous repeated fresh-conversation probes.
Holding the native conversation open produced 15,695 cache-read tokens on its
second turn. Starting a new process with the same private workspace and explicit
conversation ID also worked: 15,697 reads and 2,400 new input tokens on its second
turn. Neither experiment alone isolates the effect of workspace versus
conversation identity; the implementation preserves both.

Eight authorized CLI user turns used synthetic Hermes-shaped data with 202
initial messages and 24 tools. No production image, configuration, quota limit
or usage-ledger row was changed. All four two-turn probes passed. The CLI can
perform internal model work, so this count is not an independently verified
vendor model-request count.

The implemented HTTP path produced:

| Probe | Initial cold input | Follow-up cold input | Follow-up cache read | Follow-up inclusive input |
| --- | ---: | ---: | ---: | ---: |
| Completed answer → new user message | 17,739 | 2,655 | 15,681 | 18,336 |
| Caller tool selection → caller tool result | 17,791 | 2,733 | 15,733 | 18,466 |

Cold input fell 85.03% and 84.64% respectively in these samples. This is not a
credit/quota discount claim: AGY reported tokens, not allowance consumption.
Output and thinking still consume tokens. The tool-result follow-up reported
200 output tokens including 157 thinking tokens, compared with 189/151 on the
initial tool-selection turn. CLI startup remains on every request; the tool
pair took approximately 7.7 and 6.3 seconds.

Native counters reveal an earlier accounting mistake: `input_tokens` is cold
input, separate from `cache_read_tokens`. The second resumed native result
contained 20,156 cumulative input tokens and 15,697 cumulative reads; subtracting
the first result gives 2,400 cold input plus 15,697 reads. The standard OpenAI
prompt total must therefore be 18,097, not 2,400. Native `total_tokens` excludes
cache reads; the bridge's OpenAI total includes them.

`server.go` `openAIUsage` now performs that conversion for all AGY responses.
Native output includes thinking; it is never added twice. `session_cache.go`
`usageDifference` returns per-request deltas from resumed cumulative counters.
Historical cache-hit rows may understate cold input under the old mapping;
they were not rewritten. Prior fresh Gemini measurements with zero reads are
unaffected. Full sanitized counters and HTTP/native reconciliation are in
[agy-cache-experiment-results.json](agy-cache-experiment-results.json).

## Matching and tool ownership

`session_cache.go` `cacheState` uses the adapter's normalized transcript and a
semantic hash of model, effort, tools, choice and parallel-call policy. JSON
object key order in tool schemas is normalized without rounding numeric schema
values. System instructions remain part of the exact historical prefix.
Transport-only cache markers already disappear in the adapter's text parsing;
actual tool arguments, text and call IDs remain data.

Reuse requires the entire prior caller history, followed by the exact assistant
reply and either one new user message or one complete group of matching caller
tool results. The latter must contain every outstanding ID exactly once. Added
system turns, edited text/arguments/IDs, changed tools/model/policy, partial
results, multiple new messages and identical stateless retries cannot select the
old native conversation. Images remain unsupported by the adapter.

The adapter returns tool calls as validated JSON; native finishes that turn
before the caller executes anything. There is no native pending external tool
callback to park. `tools.go` `toolAdapter.decode` uses the cache runner's already
validated reply when present, preserving generated caller tool-call IDs. For a
resumed tool-result request, the delta includes that exact assistant call record
alongside the new results, linking bridge-generated IDs to names/arguments the
native conversation already saw. Prior full history and tool definitions are
not sent again. Hermes or another caller still executes every selected tool.

Resume preserves native conversation state and context management; matching
verifies caller history rather than reconstructing hidden reasoning statelessly.
The current permission and native-tool checks remain in force on each new
process. Native `finish` may submit structured output; other observed native
tools fail the adapter turn. Resume uses only a validated CLI-generated UUID,
never a caller-selected conversation ID or global `--continue` lookup.

## Lifetime and failure behavior

Idle entries are capped by the existing concurrency setting. Each has at most
1 MiB of normalized history plus expected reply, a one-minute idle timer and
15-minute/100-turn lifetime checks between requests. Running CLI concurrency
is unchanged. In-flight borrowed entries are separate from the idle cap, so
metadata/workspaces can temporarily occupy up to twice the concurrency cap;
there are still no idle native processes.

Checkout removes an entry from the idle map before a process starts, so two
requests cannot resume the same conversation simultaneously. A busy entry is a
miss, not a wait or shared generation. Timer generations prevent stale callbacks
from deleting a borrowed or renewed workspace. Binary identity/size/mtime changes
invalidate cached entries. LRU capacity eviction, expiry and shutdown remove
idle private workspaces. Active requests own their workspaces until exit and
cannot return entries to a closed cache. Abrupt process termination can leave
temporary files; ezllm does not claim deletion of the CLI's backend history.

Invalid envelopes, observed native tools, changed conversation IDs, counter
regressions, errors and cancellations discard the borrowed entry and its
workspace. No internal retry or repair generation is attempted. A caller deadline also
terminates the child process group after a result has arrived, and a result that
finishes after the deadline cannot enter the cache. Valid usage on failed
resumes is still returned as a per-request delta. Native results
without valid IDs or complete usage cannot become resumable entries. An
unmatched request starts a normal fresh conversation; other idle branches can
remain eligible until expiry or capacity eviction.

## Qualification and rollout

The default and existing tool proof remain available. When enabled, the CLI
updater additionally requires a two-turn auto-choice caller-tool result proof
and a `policy.session_cache=conversation-resume` verdict. This adds two small
CLI turns to cache-mode live qualification. Static checks require the resume
flag. A tiny qualification prompt need not satisfy vendor cache thresholds;
the proof requires actual native resume and correct result consumption.

The bridge's loopback `/status` exposes `session_cache` and
`runtime.conversation_cache` (`enabled`, `idle`, `hits`, `closed`). `hits` counts
successful native resumes, not guaranteed provider cache reads. Compare the
ledger's separate cold/read counters and vendor dashboard over matching time
windows. This feature was tested with Gemini 3.8 Flash low on CLI 1.4.12; use
`--tool-model` during supervised qualification for another intended model.

Deploy v1.9.0 or later before enabling the mode. Turn it off and restart to
restore stateless tool replay. The production runtime was left unchanged by the
experiment. No AGY native calls were added to ordinary short tests; captured
counters and subprocess fixtures exercise cache matching, ID linkage, usage
deltas, failure cleanup, lease/timer races, binary invalidation and capacity.
