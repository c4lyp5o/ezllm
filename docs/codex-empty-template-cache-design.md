# Codex empty-template prompt-cache affinity

## Decision

Use the installed CLI's experimental prediction-fork path when available.
`EZLLM_CODEX_BRIDGE_CACHE` defaults to `auto` from v1.14.0: startup validates
support, enables caching when supported and otherwise reports a fallback reason.
Explicit `on` requires support; `off` disables it. Unknown modes fail startup. Codex remains a bridge for caller-owned tools.
No ACP adapter, daemon, pool of idle processes, extra service or dependency is
needed. Capability fallback uses the existing independent ephemeral-thread path
before any inference; requests never probe compatibility with a paid turn.

The native probe found that identical Hermes-shaped requests in fresh threads
had zero cache reads; retaining a directory alone also had zero reads. Forking
an empty persisted parent retained cache affinity, including across CLI process
boundaries. `thread/inject_items` materializes an empty user seed without a paid
model turn. The bridge never calls `turn/start` against the parent.

## Request path

`cache.go` `borrowTemplate` keys immutable configuration by model, instructions,
ordered tools, effort, summary, output schema, sandbox and binary identity.
JSON object order and whitespace normalize while numbers preserve precision.
Caller history is excluded because it goes only to the independent child.
Configurations over 1 MiB bypass caching before inference.

`runner.go` `runAppServer` starts one CLI per request. A new template uses
`thread/start` with `ephemeral=false` and an empty injected user item. An existing
one uses `thread/resume` with the same private scratch paths. `forkTemplate`
creates an ephemeral prediction fork; `turn/start` receives the full request
input and its effort, summary and schema. A request never receives another
request's child history. Only events and callbacks belonging to its fork are
accepted. No deltas are subtracted from the empty parent: the fork's native usage
starts at zero, with cached reads included in native input.

Pending caller-tool callbacks retain the child as before. `tools.go` `resume`
returns native per-segment usage deltas. Configuration changes cause full
transcript replay instead of resuming an incompatible cached child. Only a
successfully completed turn with valid usage can return a template to idle.

## Bounds and cleanup

Idle entries are capped at concurrency, with one-minute idle expiry and
15-minute/100-request lifetime limits. Credits cap all templates plus cleanup
at concurrency plus the parked-turn limit. A busy cache bypasses into the
original path before any inference. Pointer identity prevents delayed expiry
callbacks from deleting a later checkout.

`child.dispose` reaps the CLI before `templateLease.release` removes all scratch
contents; symlinks are removed without following them. Private path names remain
stable, but each independent request recreates empty HOME, CWD and temporary
directories. Existing Landlock and native-tool lockdown still apply.

Retirement runs `thread/delete` through a transient control-only CLI. Cleanup
uses the same admission slots, has a 20-second deadline and never invokes
`turn/start`. A failed deletion consumes its credit permanently for that runner,
bounding orphan growth. Status reports count-only diagnostics. Graceful shutdown
drains for up to 21 seconds. Abrupt termination may leave empty native templates
containing configuration in `CODEX_HOME`; crash recovery does not edit vendor
SQLite or clone authentication files.

`Runner.Run` also restricts exec fallback to failures known to precede inference.
Silence after `turn/start` is ambiguous and cannot trigger another generation.

## Qualification and evidence

`RunCachePreflight` checks the experimental methods and fork shape at startup
for auto/on modes; off skips the optional check. `bridge-check` reports the
startup decision under `cache_selection`, and proves fresh-process template reuse
and the existing caller-tool cycle. The updater includes `cache=auto`,
`cache=ephemeral-forks` (explicit on) or `cache=off`, plus `approval_policy=never`,
in its qualification policy. Auto qualification proves reuse when its native
capabilities are present and reports a reason when unavailable. Small live gates
require reuse rather than an unreliable minimum native KV-cache read count.

The synthetic Hermes-shaped proof on CLI 0.167.0 and gpt-5.6-luna reported
12,657 input tokens on each request, with 12,416 cached on the second (98.1%).
Both independent caller-tool cycles passed under Landlock; the second resumed
parent retained its dynamic tools. Cleanup deleted the persisted parent through
the native API. Numeric evidence is in
[codex-cache-experiment-results.json](codex-cache-experiment-results.json).
These observations do not guarantee provider quota accounting or cache retention
for other models, configurations or times.

Fake-CLI regressions cover isolation, full-input forwarding, canonical keys,
configuration changes, tool continuation, scratch cleanup, stale expiry,
capacity, age, cancellation, shutdown, event filtering and pre-inference retry.


## Noninteractive approval policy

`LockdownArgs` pins `approval_policy="never"` before app-server or exec starts.
New and resumed threads also specify `approvalPolicy=never`; prediction forks
inherit it. Client tools remain caller-owned. Their instruction note asks the
model to continue already-authorized work without repeated permission questions,
while respecting actual caller approval requirements. Local scratch restrictions
do not describe the caller's workspace permissions.
