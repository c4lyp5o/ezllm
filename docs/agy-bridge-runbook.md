# Antigravity subscription runbook

How the Antigravity bridge behaves, its limits and its lockdown. **To turn it on,
follow [Turn on Google Antigravity](bridges/antigravity.md).** Design:
`design/antigravity-subscription-design.md`.

**Status:** text chat and streaming, with usage, against compatible Antigravity
CLI releases. Caller tools are on by default when the CLI supports
`--json-schema`; see [caller tools](#caller-tools).
Images are not offered. Linux only.

## What it is

A **loopback-only OpenAI Chat Completions surface inside the ezllm process**
(`internal/agybridge`, bound to `127.0.0.1:8792`) that runs the **official
Antigravity CLI** once per request in headless mode (`agy --print=
--input-format stream-json --output-format stream-json`) against a personal
Google account (Free, Pro or Ultra). ezllm sees it as a plain
`openai-compatible` account. The CLI is the only thing that talks to Google —
ezllm never reads or replays the OAuth token.

The plan's model list is whatever `agy models` prints for the login; the bridge
reads it at boot and serves it at `/v1/models`. Measured on a personal plan
(1.4.7): `gemini-3.8-flash-high|medium|low`, `claude-sonnet-4-6`,
`claude-opus-4-6-thinking`, `gpt-oss-120b-medium`.

## CLI releases

`agy` updates itself in place in the background, even from `--version`. Keep the version
folder read-only, as the setup page does, so the bridge moves to a new release only when
you copy one and move the `current` link.

`ezllm bridge-check agy` runs the boot checks against a staged CLI and exits non-zero
on any problem. `--json` prints the verdict (`ok`, `version`, `release`, `live`,
`problems`) for the [updater](bridge-update-runbook.md). `--live` then drives real turns
through the bridge (plan quota, the shared login), only when every static check passed.
A version the updater proved is listed in `qualified.json` in the bridge directory, and
boot reports it qualified; any other version still boots and is reported `unqualified`.

## The login

The login must be a file the container can read:
`~/.agy-sub/.gemini/antigravity-cli/antigravity-oauth-token` (0600), which the bridge
checks at boot. That is why the setup page unsets D-Bus: with a session bus `agy` stores
the token in the desktop keyring instead. `HOME=~/.agy-sub` keeps this login apart from
your own `agy`.

## Eligibility

A fresh personal account can fail every turn with
`Eligibility check failed: Your current account is not eligible for Antigravity.
Verify your account to continue.` — on every model, while `agy models` still
works. The CLI prints a one-time accounts.google.com link; open it signed in to
the same account and complete the verification. The bridge maps this to
**403 `account_ineligible`** and withholds the link (it is a sign-in
continuation), so run the [sign-in command](bridges/antigravity.md#3-sign-in) again to see it.

If Google still refuses after verifying, the account itself is out of scope
(region, age, or a Workspace domain that turned Antigravity off); use another
personal account.

## Boot checks

ezllm does not start, by design, when the bridge is on and the binary is missing, a
required flag is gone, the token file is absent, `settings.json` loosens the tool policy
([lockdown](#lockdown)), or `agy models` fails. Every message names its fix.
All variables: [environment.md](environment.md#antigravity-google-subscription-bridge).

## Lockdown

`agy` is a coding agent with its own tools (`run_command`, `write_to_file`, …)
and it runs **on the ezllm host**, next to `/data`. No 1.4.7 flag switches
those tools off, so the bridge holds them to what headless mode does by default
and refuses anything looser:

- **No `--dangerously-skip-permissions`.** In print mode a tool that needs
  approval is soft-denied: the turn continues without it.
- **Per-turn check.** The CLI's `init` event states its `permission_mode`; a turn
  that reports anything but `request-review` or `strict` is killed before the
  model answers (500 `lockdown`).
- **Boot check.** `~/.agy-sub/.gemini/antigravity-cli/settings.json` must not set
  `toolPermission` to anything else, `allowNonWorkspaceAccess: true`,
  `agentMode: "auto"`, or any `permissions.allow` rule. The `trustedWorkspaces`
  the first interactive run writes is fine.
- **Empty workspace.** Each turn runs in a fresh temp directory that is deleted
  afterwards, so workspace-scoped reads and writes (auto-allowed) find nothing
  and leave nothing.
- **Allowlisted environment.** The child gets `PATH`, locale, CA and proxy
  variables and `HOME` — never `EZLLM_MASTER_KEY`, provider keys,
  `GEMINI_API_KEY`, `AGY_ADC_AUTH` or the D-Bus address.
- **`--disable-slash-commands`.** A prompt that starts with `/` stays a prompt.

The agent's tool steps are counted and logged per turn
(`agy-bridge: turn attempted N agent tool step(s)`); they never reach the client.

## Caller tools

On by default: there is no setting. Boot turns the adapter on when the mounted
CLI's help lists `--json-schema`; the bridge `/status` field `client_tools`
reports `on (structured output; caller executes)`. A CLI without the
flag still boots, text-only, with a warning in the log and `client_tools: off`.
`ezllm bridge-check agy` fails such a CLI, so the updater will not promote a
release that drops the flag.

The bridge supplies tool definitions and conversation history as JSON in a
prompt and passes an output schema through a private temporary file. It
validates the CLI's complete envelope, then emits ordinary OpenAI tool calls.
Your client executes those calls using its own workspace and
permissions. The follow-up request must include assistant tool calls and all
matching tool results. Each request starts a new `agy` process: nothing stays
parked while the caller runs its tools. The tradeoff is repeated CLI startup
and transcript/prompt overhead. The optional conversation cache below avoids
replaying unchanged transcript prefixes while preserving one process per request.

Supported: function tools, `auto`, `none`, `required` or named function choice,
and `parallel_tool_calls=false`. Limits: 128 declared tools, 16 generated calls
per turn, and a 1 MiB output envelope. Parameter schemas must be inline JSON
Schema objects with `type: object`; references are refused because embedding
changes their resolution root. The CLI performs argument-schema validation;
ezllm independently checks envelope structure, names, object arguments, choice
and call limits. The harness must validate and authorize execution as usual.
Legacy functions, images and combining tools with non-text `response_format`
are unsupported.

The envelope is buffered even for streaming requests, so time to first output
includes the full CLI turn. Invalid envelopes return `502 invalid_tool_output`
without leaking JSON into chat. There are no automatic repair prompts. The
existing router retry/failover policy can handle the upstream failure.

Native agent tool permissions are not loosened. The prompt directs the CLI to
select caller tools instead of using its built-ins. The CLI's `finish` tool
may submit structured output; every other reported native tool step fails the
adapter turn. The submitted output still has to pass validation. This is a detection guard, not a native tools-off
switch: existing headless lockdown and the empty workspace remain necessary.
Model compliance is not guaranteed. Route workloads that need proven native
function calling to another provider.

Run the two-turn live proof (uses subscription quota) against your pinned CLI:

```bash
EZLLM_AGY_LIVE=1 EZLLM_AGY_BIN=/path/to/pinned/agy \
  go test -short ./internal/agybridge -run '^TestLiveCallerToolRoundTrip$' -count=1 -v
```

It requests a harmless lookup call, simulates the result in the caller, and
checks that the next turn consumes a fresh result value. It does not execute
filesystem or shell tools. Use `EZLLM_AGY_MODEL` to test another model.

## Limits

- **Text input only.** Image parts are always refused. On a CLI without
  `--json-schema`, client tools and tool messages are also refused with 400,
  never dropped. The adapter serializes tool definitions and history into text;
  it does not change the CLI's stream-json input protocol.
- **History is flattened.** `agy` has no system-prompt flag and no history
  replay, so system messages and earlier turns travel inside the one prompt as
  a deterministic transcript.
- **Quota exhaustion** reaches the router as 429, so a combo moves on. The plan's
  limits are on the provider card: see [Plan limits](#plan-limits-on-the-provider-card).
- **No capacity at Google looks like a timeout.** From CLI 1.5.10 the CLI waits and
  retries in silence when Google has no capacity for a model (measured: three 503s with
  waits of 18, 60 and 63 s). The bridge ends the turn at 300 s with a 504, and a combo
  moves on; the live check ends it at 120 s. The reason is in the CLI's log, under
  `.gemini/antigravity-cli/log/` in the login folder: look for `attempt 1 failed`.
- **One process per request.** A trivial turn takes ~4.5–6 s and ~207 MB RSS
  (measured); `EZLLM_AGY_BRIDGE_CONCURRENCY` (default 1) caps them.
- **No token-by-token streaming.** 1.4.7 delivers the answer in one chunk at
  the end of the turn; a streaming client gets a single content delta.
- **Harness overhead.** Every turn carries the CLI's agent prompt: ~6.7k input
  tokens on Gemini; the earlier Claude sample reported ~4k native cold input
  plus ~3.8k separate cache reads, measured on a one-word reply. Cached reads
  are separate from native input, then included in the OpenAI prompt total.

## Plan limits on the provider card

Press **Quota** on the account. The bridge answers `GET /v1/usage` by running
`agy -p /usage`, which the CLI answers itself: it runs no turn and spends no quota.
The plan has one weekly limit per group of models (Gemini, and Claude and GPT), and
the card shows how much of each is used. A full group does not stop the other one.
These are Google's numbers for the whole login, not ezllm's ledger. A group whose
number the CLI leaves out is not shown. A CLI older than 1.1.11 reports nothing.

## Conversation cache

On by default whenever the CLI supports both `--json-schema` (caller tools)
and `--conversation`. Set
`EZLLM_AGY_BRIDGE_CACHE=off` to disable it; an explicit `on` without
the adapter or `--conversation` fails startup. It applies to
tool-adapter requests, including completed caller tool-result
groups. Plain text-only requests stay fresh. Clients continue sending full
history; they provide no native conversation ID.

A match requires unchanged prior messages, the exact previous assistant reply,
and the same model, effort and tool policy. Only the new user message or matching
result group is sent. Caller tool IDs remain stable, and the caller executes the
tools. Edited histories, changed IDs/tools/model/policy, partial results and
identical stateless retries cannot reuse unrelated context.

The CLI still exits after every request. The cache retains a private workspace
with its schema file and bounded in-memory metadata, with no idle process,
extra service or persistent ezllm metadata store. Idle entries are capped by the
existing concurrency setting; history plus expected reply is limited to 1 MiB.
Idle expiry is one minute; lifetime checks between requests cap age at 15 minutes
and turns at 100. Busy entries cannot be shared. Expiry/eviction/graceful shutdown
remove idle workspaces; hard crashes cannot guarantee cleanup of temporary files
or the CLI's own backend conversation storage.

The loopback `/status` reports `session_cache` and
`runtime.conversation_cache` (`enabled`, `idle`, `hits`, `closed`). A hit confirms
native resume, not a particular vendor discount. On Gemini Flash CLI 1.4.12,
a synthetic caller-tool result used 2,733 cold input tokens plus
15,733 cache reads, compared with 17,791 cold input tokens on the initial turn.
This is a token measurement, not a quota-credit reduction percentage.

Native AGY input excludes cache reads. The bridge adds them for standard OpenAI
prompt totals and subtracts resumed cumulative counters for per-request usage.
Qualification in cache mode requires a separate caller-result resume proof,
adding two small CLI turns to the normal live gate. No internal repair call is
made after a failed resume. Set the flag to `off` and restart for stateless replay.

See [the cache design and captured counters](design/agy-conversation-cache-design.md)
for matching, lifecycle, accounting and experiment limits.

## Verify

```bash
EZLLM_AGY_LIVE=1 go test -short -count=1 -v -run Live ./internal/agybridge
```

`TestLivePreflight` checks the selected binary and login without requiring an exact version; `TestLiveTurn` spends
one small turn (default model `gemini-3.8-flash-low`, override with
`EZLLM_AGY_MODEL`) and checks streaming and usage. On an account that has not
cleared [eligibility](#eligibility) it skips, after checking the 403 and the
withheld link.

For an end-to-end check, use the gateway's [model-list command](../README.md#2-finish-setup-in-the-dashboard),
then send a short text prompt to a model from this account. The dashboard preset
shows bridge status; `docker compose logs ezllm` shows startup failures. The
internal listener is loopback-only, and the distroless image has no shell or curl.
