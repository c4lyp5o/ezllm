# Antigravity subscription via the official Antigravity CLI — design

**Status:** M1 built 2026-10-26 against CLI 1.4.7, proven live end to end on an
eligible personal account (`TestLivePreflight`, `TestLiveTurn`); the eligibility
refusal was measured live on a second, unverified account.
Operation: `agy-bridge-runbook.md`.

## 1. Goal and the hard constraint

Serve a personal Google Antigravity plan (Free / Pro / Ultra) — Gemini, and the
Claude and GPT-OSS models Google resells through it — as an ordinary ezllm
account, with the same constraint as the Claude and Codex bridges: **only the
official CLI talks to the subscription.** ezllm never reads, refreshes or
replays the OAuth token; it spawns `agy`, which does.

## 2. Measured baseline (live, 2026-10-26, CLI 1.4.7)

Host: Linux amd64. Login made with `DBUS_SESSION_BUS_ADDRESS` unset and
`HOME=~/.agy-sub`.

| probe | result |
| --- | --- |
| install | official script, SHA-512 against its manifest; 228 MB glibc ELF at `~/.local/bin/agy`; self-updates in place |
| login storage, no session bus | file `~/.agy-sub/.gemini/antigravity-cli/antigravity-oauth-token` (0600); with a bus it goes to the Secret Service keyring |
| `agy models` | 3.8 s; six models on a personal plan (Gemini 3.8 Flash high/medium/low, Claude Sonnet 4.6, Claude Opus 4.6 Thinking, GPT-OSS 120B) |
| turn on a fresh account | exit 1 in 2.5–4.4 s, one `result` event, `status: ERROR`, `Eligibility check failed …` plus a single-use accounts.google.com link — identical on every model |
| process RSS | ~207 MB per `agy -p` |
| `-p --output-format …` | exit 2: `-p` takes the prompt as its value and swallowed the next flag; `--print=` (empty value, prompt on stdin) is accepted |

Findings that shape the build:

1. **The keyring is the container blocker, and D-Bus decides it.** The same
   login lands in a file only when no session bus is present, so the runbook's
   login command unsets it and the child environment never carries it.
2. **Eligibility is per account, not per model**, and `agy models` does not
   check it. The preflight therefore proves the token, not the plan; the first
   turn proves the plan.
3. **Errors are prose.** No error codes on the wire: `mapError` matches the
   measured eligibility phrase and the documented auth, quota and model
   phrases, defaulting to 502.
4. **The error carries a sign-in continuation URL.** It is withheld from every
   client-facing message and log tail.

## 3. Decisions

1. **Surface: OpenAI Chat Completions** on `127.0.0.1:8792`, an
   `openai-compatible` account (`usage_convention: auto`). ezllm's translation
   layer gives Anthropic and Responses clients the same account.
2. **One headless process per request**:
   `agy --print= --output-format stream-json --input-format stream-json
   --disable-slash-commands --model M [--effort E]`, prompt as one
   `{"event":"user",…}` line on stdin (never argv: `ps`, `ARG_MAX`), stdin
   closed so the session ends after the turn. `text_delta`s of
   `agent_response` steps stream as content; the `result` envelope is
   authoritative for text and usage.
3. **Flattened transcript.** No system-prompt flag, no history replay: system
   messages become an `<instructions>` block and earlier turns a deterministic
   `<turn role=…>` transcript (byte-stable prefix for caching), as in the Codex
   bridge. A lone user message passes through unchanged.
4. **Usage mapping.** `prompt_tokens = input_tokens` (includes
   `cache_read_tokens`, reported as `cached_tokens`), `completion_tokens =
   output_tokens` (includes `thinking_tokens`, reported as `reasoning_tokens`),
   per the documented envelope where total = input + output.
5. **Text only in M1.** Client tools, tool messages and images are refused with
   400, not dropped. `agy` has no client-tool channel in headless mode, and its
   stream-json input takes text blocks only.
6. **Lockdown without a tools-off switch.** 1.4.7 cannot disable the agent's own
   tools. The bridge keeps the headless default (approval-needing tools are
   soft-denied), runs each turn in a fresh empty workspace, allowlists the child
   environment, kills any turn whose `init` reports a permission mode other than
   `request-review`/`strict`, and refuses to boot on a `settings.json` that
   loosens the policy. Tool steps are counted and logged.
7. **Model list from the login.** `/v1/models` serves what `agy models`
   printed at boot; an unknown model is refused before a spawn (a refusal that
   would otherwise cost a ~3 s CLI start).
8. **Pinned binary.** compose mounts a copy outside the self-updating path,
   read-only; the login directory is read-write. `QualifiedVersion` marks other
   releases `unqualified` in `/status`.
9. **Concurrency 2 by default**, from the measured RSS.

## 4. Measured on the first eligible turns (2026-10-26, CLI 1.4.7)

| probe | result |
| --- | --- |
| `pong` on `gemini-3.8-flash-low` | 4.8–5.9 s wall; 6,750 input tokens, 1 output token — the agent harness is ~6.7k input tokens per turn, none of it cache-read |
| `pong` on `claude-sonnet-4-6` | 4.6 s wall; 4,076 input tokens, 3,851 of them cache reads; 15 output tokens |
| 200-word paragraph, Gemini Flash low | 9.5 s wall; 1,323 characters, 238 output tokens |
| streaming | the answer arrives as **one** chunk at the end of the turn, even at 1.3k characters: clients see a single content delta, not token-by-token text |
| two concurrent turns, one `HOME` | both succeeded (4.8 s, 6.4 s); no contention on the CLI's state files |
| agent tool steps on plain chat turns | 0 on every measured turn |
| fresh, untrusted temp workspace | turns run normally; no trust prompt in headless mode |

Consequences: the harness overhead makes a trivial turn cost ~4–7k input
tokens of plan quota, so this account suits interactive chat, not
high-frequency agent traffic; and streaming clients get time-to-first-token
equal to the whole turn.
