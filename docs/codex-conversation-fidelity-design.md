# Codex conversation fidelity

## Decision

From v1.14.3, `RunPreflight` detects `thread/inject_items` independently of fork
cache mode. `Runner.runAppServer` uses `nativeConversation` to inject prior
Responses items into a fresh request thread, then starts the latest user input.
On a cached request, injection occurs after `thread/fork`, never on the parent.
Existing parked continuations bypass injection and answer the native callbacks.
Incomplete or interrupted historical tool groups use legacy text replay before
inference, avoiding orphaned native call outputs. Unsupported older CLIs and exec
also keep flattened replay; an injection error does
not start inference or silently retry a generation.

`nativeConversation` preserves roles, text/image order, function names, argument
strings, call IDs and tool outputs. `assistantPhase` preserves explicit phase and
otherwise reconstructs the Chat Completions boundary: tool-bearing assistant
messages are commentary; messages without tool calls are final answers for that
API turn. This is not proof that the user's larger task was completed. Explicit
commentary without tool calls remains commentary. Canonical continuation hashes
include semantic phase and ordered text/image content, treating explicit and
inferred equivalent phases alike.
No native reasoning is invented or copied into prompts.

## Why

Claude's bridge passes structured Messages history through `BuildExtraBody`.
Codex previously turned the same kinds of roles and tool relationships into text
inside a user message. This discarded their protocol meaning. OpenAI's
[model guide](https://developers.openai.com/api/docs/guides/latest-model?model=gpt-5.3-codex)
identifies commentary/final-answer phase preservation as relevant to early
stopping; its [Responses reference](https://developers.openai.com/api/reference/resources/responses/methods/create)
requires preservation on follow-up requests for Codex models. The
[app-server API](https://learn.chatgpt.com/docs/app-server) exposes raw-item
injection without using the Cloud-only `thread/resume.history` field.

The Hermes trace showed Codex finishing short work turns while Claude continued
through longer sequences. Claude also encountered a Hermes foreground-process
rejection and continued afterward. That terminal guard is independent of either
bridge. These observations do not isolate model behavior from harness effects,
and do not make the guard the cause of premature final answers.

## Updater qualification

The updater and `bridge-check` bind proofs to
`history=native-if-supported-v1`; older proofs cannot certify this behavior.
The live qualification runner uses the production history selection and adds a
small prior-user-fact recall turn with explicit commentary phase when supported.
This is a protocol qualification check, not a guarantee of task persistence.

## Validation and limits

The bounded live test uses CLI 0.167.0, gpt-6.1-terra and Landlock. A synthetic
history includes a rejected Vite install and an incomplete assistant answer.
Caller tools write only a temporary workspace. The task requires six files for
a dependency-free Bun todo backend, tests and browser UI. Terminal dispatch runs
only the fixed `bun test` command, never model-supplied shell text. Independent
checks exercise health, item creation and listing after the model finishes.
Each case is capped at 16 tool batches and three minutes; no automatic retry is
used to conceal premature completion. The cache case also checks a second fresh
request reuses an empty template. Normal short tests skip all paid live runs.

The initial flat replay and structured prototype each completed with cache on
and off. Therefore the experiment does not reproduce the original large Hermes
session's failure or establish a cache regression. It verifies that native
history is supported and can complete the caller-tool task; permanent protocol
tests demonstrate the representation and isolation correction. Do not interpret
one-off token counters as a provider quota saving or a guaranteed persistence fix.

Run the optional live comparison explicitly:

```bash
EZLLM_CODEX_FIDELITY_LIVE=1 \
EZLLM_CODEX_FIDELITY_NATIVE=1 \
EZLLM_CODEX_LIVE_BIN=/path/to/codex \
EZLLM_CODEX_LIVE_HOME=/path/to/isolated-login \
go test -short ./internal/codexbridge -run '^TestLiveCallerTaskFidelity$' -count=1 -v
```

`EZLLM_CODEX_FIDELITY_NATIVE` is a test-only switch, not a deployment option.
Use its absence for the legacy replay comparison. The test requires Bun on PATH and spends subscription quota.
No ezdb files, Hermes installation files or live gateway configuration are
modified by the experiment. Numeric results are retained in
[codex-conversation-fidelity-results.json](codex-conversation-fidelity-results.json).
