# Changelog

Notable changes, reverse-chronological. Milestone-tagged (M1–M14), then by release; full history via `git log`.

## Unreleased

- **Subscription bridges in one click.** Add provider has two presets, **Claude
  subscription (bridge)** and **ChatGPT subscription (bridge)**. Each shows
  whether its bridge is running (CLI version, login, plan or tool state) or the
  exact steps to turn it on, fills in name, base URL and usage convention, and
  needs no key: the add names `bridge` and the server supplies the shared secret,
  so nobody copies `data/*-bridge.token` off the host. New `GET /admin/bridges`
  (the secret is never in it); a key test against a bridge that is not running
  now names the variable that fixes it. Both runbooks open with a five-step
  quick setup and say how to install each CLI.
- **Lighter login backdrop.** At most 12 packets on a desktop and 5 on a phone
  (was 24), no per-packet `shadowBlur`, and phones draw at 1.5x density and 30
  fps, at the same on-screen speed.

- **Neighbourhood page (dashboard).** Every provider key is a house whose windows
  glow with its tokens per minute over the last five minutes; smoke means it is
  serving traffic, a rain cloud means a quarter or more of its recent calls
  failed, dark windows mean it is disabled. SVG and motion only, polled every
  10 s, with an exact-figures table underneath (glow meter per key; hovering a
  house highlights its row and back), a glyph legend, an "updated Ns ago"
  indicator, boarded windows on disabled keys, and a sun/moon. The one scene exempt from the
  no-glow rule — fenced in `web/DESIGN.md`. The Playwright harness's unlock
  marker was updated to the session marker the app has used since v1.2.0.

## 2026-10-26 — v1.3.0: chat wire rename, probes out of the ledger, live Claude quota

- **The chat-completions wire is named `chat`, not `openai`** (schema v30,
  breaking). The ledger `surface`, model pins, `protocol_support`, the
  `/admin/models` capability keys and the `proto_openai` column all become
  `chat` / `proto_chat`. The migration renames the column and rewrites stored
  pins and existing ledger rows in place; it is idempotent. `PUT .../pin` and
  the combo-test `surface` still accept `openai` as a legacy alias on input.
  Provider kind names (`openai-compatible`, `gemini-openai`) are unchanged.
- **Probes are no longer written to the ledger.** Key tests (`admin:test`),
  protocol sweeps, combo tests (`combo-test`) and service tests
  (`service-test`) leave no row; the verdicts they produce are still stored.
  Rows already in the ledger are untouched. This reverses the earlier rule
  that probe spend must be visible in the ledger.
- **Claude subscription quota can be read live.** Pasting the claude.ai Cookie
  header into the claude-sub account's `management_key` makes the Quota button
  read `claude.ai/api/organizations/{org}/usage` (five-hour and weekly
  windows) instead of the bridge's last-observed reading. The dialog adds the
  Opus/Sonnet windows and the per-product split of the week, names the source,
  and states why it fell back when the live read failed.

## 2026-10-24 — v1.2.0: reasoning-effort clamp, model-access park, logging

- **Reasoning effort follows the model's own vocabulary** (schema v29). A 4xx
  that enumerates accepted `reasoning_effort` values, or says reasoning cannot
  be disabled, is stored per account and model; later requests are clamped
  down into it instead of refused. Key-test probes learn it too, and an
  invalid-effort 400 no longer records a wire as unsupported.
- **A 403 that names model access parks that model on that hop** for the
  normal cooldown when another candidate exists, so a block of requests stops
  re-dialling it and losing its prompt cache to a fallback.
- **The per-key model allowlist fails closed.** A store error now answers
  `503 authorization_unavailable` instead of suspending the allowlist.
- **Key tests:** Copilot credentials refused at token exchange count as proven
  discrimination; an endpoint-mismatch refusal is re-probed on `/responses` and
  the wire recorded; failed probes write their reason into the ledger `err`.
- **Leaks:** parked fallback responses and failed Gemini audio rewrites close
  their upstream bodies on every exit.
- **Dashboard:** the Connect page reads `GET /admin/routable-models` through
  the session; the unused localStorage bearer path is gone.
- **Console logging says what it knows.** The access line names the requested
  target (`alias`) and carries the failover trail when a candidate did not
  serve. Auth refusals — which reached neither the ledger nor any log — are
  visible at `debug`, and the client-budget refusal logs once per window at
  `info`. The reasoning clamp now reports both values (`none` → `minimal`) and a
  mandatory-reasoning drop as a drop; a learned vocabulary and a wire recorded
  after an endpoint-mismatch probe are reported at `info`. The boot states the
  effective level. `EZLLM_LOG_FORMAT=json` for collectors; stdout only, no log
  file. The contract and the measured cost are in
  [docs/logging.md](docs/logging.md).

## 2026-10-23 — v1.1.1: audit fixes — security, compression sessions, dashboard, recovery, performance

- **Same-origin cookie writes.** A signed-in operator's browser sends the
  `SameSite=Strict` session cookie to every origin of the same site, and a
  `text/plain` POST needs no preflight, so a page on another port could mint an
  admin token. Cookie-authenticated writes and sign-out now pass
  `http.CrossOriginProtection` (`cookieOrigins`) or get `403`; bearer and
  non-browser clients are unaffected.
- **No cross-origin redirects with provider keys.** Go drops `Authorization` on
  a cross-host redirect but keeps `x-api-key`, and a 307 replays the prompt.
  The upstream client and the key tester now use `provider.SameOriginRedirect`.
- **Compression sessions are per client and honest about their version.** An
  explicit session is scoped to the authenticated client. A `tool_ref`
  reference is replayed only while its target still holds the same output. A
  bound session runs its bound profile snapshot, not just its version stamp.
  The memo has per-session and global byte budgets and an idle sweep. Tool
  outputs are classed with names from the whole request, so a file read after
  a memoized call stays byte-identical. Caveman lite lost the rules that
  changed quantity, time or certainty.
- **Dashboard fixes**, each with a Playwright regression driver on the new
  `web/tests/mockdash.mjs` harness:
  - Policy saves no longer move the key's expiry.
  - Pricing saves work.
  - Chat surfaces stream errors.
  - Analytics ignore stale responses.
  - Grouped rule writes reconcile after a partial failure.
- **Metering, pricing and recovery:**
  - USD-only rates.
  - Source-wire usage capture.
  - Retries and failed hops are ledgered.
  - Missed meter debits are replayed (schema v28).
  - Master keys are preserved.
  - Atomic legacy combo rebuild.
  - Verified WAL-safe backups (`scripts/backup_sqlite.py`).
  - Versioned configuration exports.
- **Docs pass:**
  - Landmarks cite symbols instead of line numbers.
  - The README, AGENTS, CONTRIBUTING and EXPLORING claims are re-verified
    against the code.
  - The translation plan is reduced to a design record.

- **Audit fixes** (the first pass on `fix/audit-findings`):
  - The Claude web provider verifies TLS and opens one upstream conversation
    per request.
  - Rules retry after a failed boot load.
  - The Gemini web client resolves under the per-account lock.
  - Strict JSON body endings.
  - Shutdown drains handlers and background jobs before the ledger closes.
  - Bridge children end with their request.
  - Budget reservations are released by the flush that counts them.
  - The CSP admits generated media previews.
- **Performance pass:**
  - O(1) memo eviction.
  - Streamed shape hashing (1 MiB history: 1.4 KB allocated instead of
    1.9–3.5 MB).
  - A faster local-timezone day report.
  - 10 s shared usage reports.
  - Preallocated body reads.
  - One HTTP/2 connection for Claude web.
  - Lazy dashboard pages (290 KB → 124 KB gzipped at sign-in).

- **Bound Codex output buffering:** a pending queue holds at most 1 MiB or
  4,096 deltas, in addition to the batch currently being delivered. Overflow
  fails with `output_backpressure` and independently reaps the child; the stdout
  reader never waits on a stalled client while holding a state lock.
- **Fix Claude web transport cleanup races:** closing a fetcher uses the same
  initialization barrier as its first request.
- **Keep report-cache errors and filters honest:** a completed failed computation
  cannot become a successful nil result, and explicit `from` boundaries no
  longer collide after rounding into the same ten-second interval.

## 2026-10-23 — v1.1.0: ChatGPT subscription bridge, plan windows, probe and Copilot fixes

- **ChatGPT subscription as an account** (`internal/codexbridge`, off by default).
  The official `codex app-server` sits behind a loopback OpenAI-compatible surface
  (`127.0.0.1:8791`); every child runs with Codex's own shell, patch, web and
  update features disabled and a read-only sandbox, so it executes nothing of its
  own. Client tools travel through code-mode and come back as `tool_calls`; the
  child is parked until the results arrive (continuation), and every child is
  confined by Landlock — without Landlock, tools are refused unless
  `EZLLM_CODEX_TOOLS_WITHOUT_LANDLOCK=on`. Structured output, reasoning summaries
  as `reasoning_content`, all models including hidden ones, and a `codex exec`
  fallback when the app-server lacks a required method
  (`docs/codex-bridge-runbook.md`, `docs/codex-subscription-design.md`).
- **Plan windows on the provider card.** The Claude bridge serves the CLI's 5h/7d
  `rate_limit_event` on `/v1/usage`, the Codex bridge serves `account/rateLimits/read`
  in the same shape; ezllm reads it for loopback accounts.
- **Fix: key test advances past endpoint-mismatch models** ("not accessible via
  /chat/completions") and no longer blocks a proven key on a non-auth generation
  failure.
- **Fix: Copilot presents CLI 1.0.103.** GitHub's HTTP 466 floor rejected 1.0.81-6;
  compose passes `GITHUB_COPILOT_CLI_VERSION` through.
- **Deploy change: one compose file, yours.** `compose.yml` is no longer shipped:
  copy `compose.example.yml` (every variable with its default, bridges off, mounts
  commented) to `compose.yml`, which is gitignored. `compose.traefik.yml` is gone;
  proxy labels go in your copy. **Before pulling,** back up your `compose.yml`:
  git deletes a formerly tracked file on pull.

## 2026-10-22 — v1.0.0: one translation switch, env reference, probe fixes, arm64 image

- **Behaviour change: `translation.enabled` is the one switch.** It now covers the
  pairwise translators and the universal matrix together. `translation.universal_enabled`
  and `EZLLM_TRANSLATION_UNIVERSAL_ENABLED` still work but warn at boot; an explicit
  `universal_enabled: false` now turns all translation off, not just the matrix
  (`docs/translation-runbook.md`).
- **Every environment variable documented.** `docs/environment.md` lists each one
  with its default and opens with what a new install sets (`EZLLM_USAGE_TZ`, the
  first dashboard password, `EZLLM_CLAUDE_BRIDGE=off` without a Claude CLI). The
  duplicate Claude bridge defaults left `compose.yml` and the `Dockerfile`; the
  binary's own defaults (`/opt/claude/claude`, `/claude-sub`) are unchanged.
- **Shipped compose still turns the Claude bridge on**, with paths for this
  maintainer's host. Installs without the Claude CLI and a subscription login set
  `EZLLM_CLAUDE_BRIDGE=off`, or the boot fails.
- **Fix: a retired model no longer fails a key test.** Ollama answers a retired
  model with `410 Gone` ("… was retired on …"); the probe now treats it like
  Gemini's retired-model 404 and moves to the next candidate.
- **Fix: Ollama (local) keeps the operator's base URL**, so a remote Ollama host
  is no longer replaced by `localhost`; quota percentages render whole.
- **Fix: the models drawer refreshes after a catalog sync** without a page reload.
- **arm64 image again.** `ghcr.io/c4lyp5o/ezllm` is built for `linux/amd64` and
  `linux/arm64`; the Dockerfile cross-compiles on the runner (`GOARCH=$TARGETARCH`),
  so arm64 adds no emulated build time.
- **Semantic versioning from v1.0.0.** A release tag must match
  `internal/server/version.go`; both release workflows refuse one that does not.
- **Docs accuracy pass.** README Security now states keys are AES-256-GCM
  encrypted at rest (it said plaintext); EXPLORING.md landmarks re-verified;
  resolved known issues removed from CONTRIBUTING.md.

## 2026-10-21 — Claude subscription M2: tools on claude-sub

- **Agents can run on the subscription.** Hermes sends tools on every turn, so M1's
  blanket refusal answered every agent message with a 400. Client `tools` and
  `tool_choice` now travel in `CLAUDE_CODE_EXTRA_BODY`; upstream sees exactly the
  client's tools and the `tool_use` blocks come back under their names
  (`docs/claude-subscription-design.md` §2.1, §6).
- **Measured, not assumed (CLI 2.1.306).** A live spike replaced the planned inert
  MCP server: one generation per call, a `write_file` sentinel never written, and
  Hermes's real 57-tool list cached normally (24,112 tokens written once, then read
  on every later turn).
- **Native's quirks stay inside the bridge.** Its synthetic "No such tool"
  `tool_result`, the `error_max_turns` ending, the empty-reply nudge (a second
  message), and its one-frame-per-block output are all handled: the client sees one
  well-formed message, with every parallel call.
- **Cache breakpoint budget.** Native spends two of upstream's four
  `cache_control` slots; four client markers were a live
  `400 … Found 6`, so the newest two are kept.
- **Drain cap through Rules.** sonnet/haiku 300,000 tokens per 5h, opus 1, set
  before an agent is moved onto the combo (runbook).
- **CLI pin survives updates.** 2.1.306 is pinned as a copy outside the
  auto-updater's directory, which had deleted 2.1.299 under the live mount.

## 2026-10-21 — Cloudflare free models in auto-free combos

- **Built-in Cloudflare free list.** `internal/provider/cloudflare_free.go`
  names the 65 Workers AI models the dashboard shows free, with their task.
  `syncAutoFree` adds the chat-capable ones (`Text Generation`) when the
  account is a host-verified Workers AI account (`api.cloudflare.com`,
  `/ai/v1`); the other classes are data only — a chat combo cannot serve an
  embedding or an image model, and catalogs do not advertise capability
  (`docs/services-design.md`).
- **Five more service endpoints.** `/v1/images/edits` (multipart, shares the
  STT handler), `/v1/moderations`, `/v1/rerank`, `/v1/videos/generations`,
  `/v1/music/generations` — routed, ledgered under their own surface,
  pinnable, with Services cards and probes (paid ones confirmed first).
- **Fix: service surfaces never read the chat probe column.** `proto_openai`
  was consulted for every non-chat surface, so a model that failed the chat
  probe was refused on `/v1/embeddings` & co.
- **Fix: service-surface pins and multipart refusals.** `surfaceName` named
  every service surface "openai", so an `embeddings` pin refused
  `/v1/embeddings` and an `openai` pin admitted `/v1/rerank`; pin translation
  is now chat-wire only. The multipart handler shares `writeResolveErr`, so
  `pinned_surface` / `wire_unsupported` are 400 there too, and the ledger
  records `wire_unsupported` as 400 rather than 502.
- **Services "Try it".** `POST /admin/services/{surface}/generate` runs the
  operator's model-less request through the normal pipeline with the saved
  default (like `/admin/chat`); the card renders images, audio, video,
  transcripts or the raw JSON. Video/music have no common upstream format,
  so only the prompt is sent and async job answers are shown as they come.
- **Local Ollama preset.** Ollama serves both wires natively
  (`/v1/chat/completions` and `/v1/messages`); the preset only fills the
  base URL.

## 2026-10-20 — ten compression engines, universal translation, data-terminal dashboard

- **Auto-free combos (schema v25).** `combos.auto_free` makes a combo's hops
  the free-marked models (`:free` / `-free` ids) of the account catalogs,
  kept in sync on every catalog write, account enable flip and combo save;
  `auto_free_accounts` narrows the sources and `auto_free_include` pulls in
  unmarked promos such as Ollama's `beam`. Fresh databases are seeded with
  an empty `ezllm-auto-free` combo that fills as providers are added.

- **Compression: ten engines on one wire-neutral text walker.** New: `ansi`
  and `fold` (terminal output), `jsonmin` (whitespace-free JSON tool output),
  `tool_ref` (repeated tool results become references), `blobs` (base64 runs
  and data URIs become markers). The `budget` engine is deleted — it needed a
  known `max_tokens` and did nothing without one. A request is transformed
  identically whether it arrives on chat, messages or responses.
- **The pipeline is ordered by meaning.** A per-profile `tool_classes` table
  names what each tool is (file read, edit, diff, …); file reads ship
  byte-identical so the agent's next edit still matches; an unknown tool is
  its own class — never truncated, only tidied by the lossless engines.
  `fresh_tool_output` decides whether a purely-tool final message may be
  trimmed on its first send (schema v24). A committed-prefix memo keeps the
  cached prefix exactly equal to what was committed.
- **Dashboard**: the `/admin/usage` totals row now carries `applied_saved`
  (Stats' "Removed by compression" card read 0 over non-zero rows); the
  budget-stage box, the "context saved" column, the version in the Dashboard
  strip and the "(unconfirmed)"/"(no probe)" surface tags are gone; Chat opens
  with one column.
- **Universal wire translation is default-on** — no yaml needed, and an
  anthropic-only account is pre-marked so the universal matrix translates
  instead of refusing. One translation path in `commit`.
- **A hop that refuses the WIRE now fails over.** An upstream 400 that names a
  wire-protocol gap (`ModelProtocolUnsupported`, `not supported for format`)
  describes the HOP, not the body, so the next hop serves the identical
  request and the refusing hop is parked. Ordinary 400s still pass through
  unchanged, and a sole hop still returns its own 400 verbatim. Motivated by
  `daily-pro`: the opencode-go hop refused `/v1/messages` while the command-code
  hop was never tried.
- **Combo hop auto-probe.** A combo write (`POST /admin/combos`, `PATCH`,
  `POST|PUT …/hops`) starts a background protocol probe of that combo's
  ENABLED hop models only — never the account catalog — on the surfaces still
  lacking a verdict. An account sweep samples a 500+ model catalog, so the hop
  an operator actually routes through was often never sampled; curated rows,
  disabled hops and disabled combos cost nothing.
  `EZLLM_COMBO_AUTOPROBE=off` disables it.
- **Proxy**: every input token lands in one billed bucket, the account usage
  convention is honoured, the hop model is spliced into the client bytes
  instead of being re-marshalled, a hop stays parked after a transport error,
  and an oversized body is a 413.
- **Provider/registration**: quota and catalog reads honour custom headers,
  pagination and no-endpoint statuses; the management key stays on
  tokenrouter; retries honour http statuses; sweeps keep proven verdicts; a
  claude-web key keeps its wire.
- **Perf**: one catalog read per hop; an in-memory routing catalog snapshot
  invalidated by `catalog_gen` (schema v23); a throttled reservation prune;
  an allocation-free header strip.
- **Dashboard: the data-terminal redesign.** Exact numbers (never
  abbreviated), motion.dev throughout, and a screen-by-screen polish pass
  verified in a live browser — including three fixes the pass surfaced: the
  modal's exit animation was unreachable, a page-level `AnimatePresence
  initial={false}` was suppressing every descendant entrance, and the
  committed bundle was carrying dead CSS classes because Tailwind was
  scanning the tracked `web/dist/` output. The build is now reproducible.
- **Dashboard: the roster and rules re-read.** The Providers page is a roster
  — one expandable row per account with a fixed-width quota column, so every
  figure sits on one right edge instead of ragged per-card alignment. Rules
  are one table row per SETTING rather than per model: rules on the same
  provider with the same cap, window, state and note collapse into one group
  listing its models (the wire stays one rule per account+model — a group is a
  view, not a schema change). The Dashboard's health strip becomes a header
  pill: an uptime indicator, with the account/model/combo counts gone from
  it because inventory is not health and those counts live on their own
  screens. Labels are sentence-case throughout.
- `unmetered_rows` on health, overview, live feed and dashboard; the tz day
  report's `applied_saved`; the meter is all-or-nothing per batch; web
  bridges require a process secret.

## 2026-10-19 — claude-bridge M1: the Claude subscription as an ezllm account

The Claude subscription becomes an ezllm account without the router learning a new
trick. A **loopback-only surface inside the ezllm process** serves an Anthropic
Messages shape by driving the **official Claude Code CLI** — bind-mounted from the
host — against a subscription login, and ezllm dials it as an ordinary
`anthropic-compatible` account. The CLI stays the only thing that speaks to the
subscription — the credential never leaves its own login store — which is the
whole point: no OAuth-bearer reuse, no client impersonation
(`docs/claude-subscription-design.md`, locked decision 1).

- **Request path.** The client's Messages body is handed to native through
  `CLAUDE_CODE_EXTRA_BODY` (the mechanism the official Hermes plugin documents),
  `system` travels via `--system-prompt-file`, and replayed history frames carry
  `shouldQuery:false` so native acknowledges them with a zero-turn result instead of
  generating — and charging the plan — once per frame.
- **Measured, not assumed.** With the body carrying history and the stdin frames
  carrying a *different* history, the answer followed the body: the injection is the
  upstream request. One generation per call. And the CLI's own ~20k-token harness
  overhead is gone — a three-message body measured 170 input tokens on the wire.
- **M1 is text-only by declaration.** Tool-bearing requests are refused with
  `chat_tools_unsupported` before a process is spawned; tools arrive in M2 through an
  inert MCP server (advertise, deny execution, capture the call).
- **Failures stay legible.** Native error codes map to honest statuses
  (`rate_limit`→429, `billing_error`→402, `authentication_failed`→401,
  `overloaded`→529) and the CLI's own words ride along unrewritten. A client's own
  `max_tokens` ceiling is a 400, never a retryable 5xx.
- **Key test, operator's call.** The generation probe is **skipped** for this
  account (`skip_inference`) — it is official Anthropic, Messages-only, and the
  credential is already proven by the auth oracle (a fake key is rejected).
  Inference evidence comes from the live proof instead of a "ping".
- **Probe budget (the NIM lesson, again).** The remaining probes budgeted 8–16
  output tokens, which a mandatory-thinking model (Haiku 5.5) cannot answer in —
  measured `max_output_tokens` failures on a perfectly good key. Probes now budget
  256 (`internal/registration/probe_body.go`), with a test that fails if anyone
  shrinks it.
- **Proof.** `tests/prove_claude_bridge.py`, through ezllm's front door against a
  real Pro subscription: **7/7** — completion, native usage fields, conversation
  history fidelity, tool refusal, and the ledger rows.
- **Container.** One container, `distroless/base` runtime: the CLI is glibc-dynamic,
  and on `/static` the exec fails with a misleading "no such file or directory"
  (observed live); Alpine is musl and cannot run it at all. The CLI binary mounts
  read-only and pinned by path, the login directory read-write (the CLI refreshes
  its own token), the account secret auto-generates into `/data` like the master
  key, and the surface binds loopback only — it refuses any other bind.

## 2026-10-18 — security milestones, native surfaces, playground, per-key budgets

- Security Milestone A: provider keys encrypted at rest (AES-256-GCM), master key, migration + tests.
- Security Milestone B: httpOnly cookie session, per-IP login rate limit, frontend cookie auth; cookie is `Secure` when the proxy reports `X-Forwarded-Proto: https` and scoped to `/admin`.
- Native image/audio adapters (Cloudflare, Google AI Studio); per-account management key (schema v19) for TokenRouter balance.
- Price catalog: `GET /v1/prices`, bulk import, and daily LiteLLM refresh; rates carry `source`/`fetched_at` (schema v22).
- Chat playground (1-3 columns), per-key budget meter + editor in Connect; budgets are now enforced (schema v20 `client_usage_counters`, 429 before dispatch, backfill for caps set after spend), with `max_tokens` reservation, per-key model allowlists, and key expiry (schema v21).
- Dashboard: routing-rescue panel; Linear-inspired palette and sidebar icons.
- UI: Providers/Services redesign, Connect leftover localStorage removed.
- Tests + docs updated; security audit report.

## 2026-10-17 — release versioning, docs sync, test-file housekeeping ✅
Version plumbing and a full documentation sync pass (release **v0.0.12**,
see `VERSION.md`):
- **`GET /healthz` now reports the release version**: new single source of
  truth `internal/server/version.go`, served as `{"status":"ok","version":…}`
  (503 down-payload carries it too); `/admin/overview` health object gains
  `version`, rendered as the first chip in the dashboard health strip.
  Guards: `version_test.go` fails on VERSION.md ↔ version.go drift and on a
  healthz payload that grows beyond `{status, version}`.
- **VERSION.md** (new): the release history — one version per change date,
  patches on the same date grouped under one entry, back-filled v0.0.1
  (2026-10-06) → v0.0.11 (2026-10-16) from `git log`.
- **Docs rewritten/synced**: README rebuilt as the project front door (no
  personal deploy details; version/healthz contract updated), CONTRIBUTING.md
  (new — workflow, test placement rules, memory findings, known issues),
  AGENTS.md (new — agent/maintainer hard rules, layout map),
  EXPLORING.md landmark re-verified against code (`RecordCall`, model-pin
  handler/route, M1 block line numbers, `Auth` interface), API.md healthz row
  + health object example refreshed from the new contract, design-doc
  citations corrected (`ValidateAccountInput`, `main.go` WriteTimeout).
- **Test-file housekeeping**: Python live proofs `scripts/prove_*.py` →
  `tests/`; Playwright drivers `web/{shot,verify}-*.mjs` → `web/tests/`. Go
  `_test.go` files intentionally stay in-package (toolchain constraint —
  CONTRIBUTING explains why).
- **Makefile**: `test` now runs `-count=1 -timeout 15m` (CI parity; cached
  results had masked changes), new `test-short` target for 4 GiB cgroups, and
  an honest comment on what GOMEMLIMIT does/doesn't cap (measured: soft heap
  limit ≠ RSS cap; full suite peaks ~4.21 GiB in the percentile harness).
- **CI**: `release.yml` tag builds now call the reusable `test.yml`
  (`needs: test`) — same gate as `docker-publish.yml`, so a red suite never
  produces an image or a release asset. CONTRIBUTING known-issue #1 closed.
- **Provider credential entry fixed (the "waiting for auth" stall)**: GitHub
  device-flow `slow_down` is surfaced (`GHDeviceStatus.slow_down`) and the
  dashboard backs its poll interval off per RFC 8628 §3.5 — it was folded into
  plain `pending`, so GitHub withheld the token forever while
  `github.com/login/device` already showed Success (reproved live: GitHub does
  send `slow_down`, 6/8 polls at 1s; its own advertised 5s is clean). Poll and
  mint now run on `context.WithoutCancel`, so a tab reload/blur mid-mint can no
  longer cancel an already-granted authorization and poison the handle;
  5xx/429/context errors classify retryable instead of terminal; the pending
  panel renders `detail`; every status failure reaches `ErrBox` (a non-ApiError
  used to be swallowed, leaving "Waiting…" with nothing under it); the flow now
  logs start/poll/authorize/refusal/expiry (8-char handle prefix only — the
  full handle is the capability to read a session).
- **gemini-web cookie paste**: credential field becomes a textarea when the
  preset says `multilineKey`, and `ParseKey` accepts every realistic paste —
  `Cookie: a=b; c=d` header, `document.cookie`, or one cookie per line — keeping
  only `__Secure-1PSID`/`__Secure-1PSIDTS` by EXACT name. The old line-wise
  split stored the whole rest of the header as the PSID value, so a whole-jar
  paste reached Google as a corrupt session; `__Secure-1PSIDCC` (prefix
  sibling) can no longer satisfy the required PSID.
- **Guards**: 4 new `ParseKey` paste-shape tests, `TestGitHubDeviceSlowDownIsSurfaced`,
  `TestDeviceErrClassification` — each bite-proven RED without its fix, with
  the pre-bite hash re-verified after restore; `make test-short` 16 pkgs green,
  gofmt/vet/build clean, `web/dist` rebuilt in the same commit.
- **Combos account select grouped by provider**: hops can already pin an account
  (`combo_hops.account_id`) and `accounts` allows several rows per provider
  (`name`/`namespace` are the only UNIQUE columns — `kind` is not), but the
  dropdown was a flat list of names with no way to see which accounts belong to
  the same provider. It now renders `<optgroup>` per provider, derived from
  `base_url` (no `provider` column — a schema bump is deliberately deferred),
  labelled `groq.com · 2 accounts` only when a provider has more than one; single
  accounts stay bare and the control stays enabled because it also selects the
  provider. Verified against the live account list (16 accounts → 16 clean groups)
  plus the multi-account case (`groq.com · 2 accounts` → "Groq | Groq alt").

## 2026-10-16 — M14 claude-web provider: claude.ai web protocol ✅
Claude.ai's web app (sessionKey cookie + SSE completion) as a provider,
ported from omniroute's production implementation (design:
`docs/claude-web-design.md`, runbook: `docs/claude-web-runbook.md`).
- `internal/claudeweb/tlsclient.go` — uTLS Chrome-146 impersonation
  (`HelloChrome_Auto`, h2 dialer) behind an injectable Fetcher (tests run
  plain HTTP; one test runs the REAL profile against a local h2 TLS server).
  New dependency: refraction-networking/utls v1.8.2 — Go's native TLS
  fingerprint is rejected by claude.ai's Cloudflare.
- `internal/claudeweb/client.go` — sessionKey normalization (CR/LF
  injection rejected), org discovery via GET /api/organizations (the
  zero-quota key-test oracle), the conversation state machine ported from
  the compiled reference: the conversation UUID is CLIENT-generated and
  rides the URL path; follow-ups chain conversation_id +
  parent_message_uuid in turn_message_uuids with a fresh assistant uuid;
  state commits ONLY after message_stop (a cut stream commits nothing).
  Payload parity: opus-5 → high/auto effort, effort vocab
  low|medium|high|xhigh|max, the Default personalized_styles block verbatim,
  create_conversation_params on the first turn, Anthropic-native tool shape
  {name, description, input_schema}. Typed failures: ErrSessionExpired
  (401), ErrCloudflareChallenge (403 + markers), ErrForbidden,
  ErrRateLimited, ErrUnavailable; upstream error events surface; a 401
  mid-stream classifies before the generic stream-cut error.
- `internal/provider/claudeweb.go` + `internal/server/claudeweb_bridge.go`
  — kind `claude-web`, sentinel base `internal://claudeweb` rewritten to the
  in-process bridge (the web SSE is not an ezllm wire), account-id +
  credential stamped in loopback headers and stripped, bridge routes
  /chat/completions (stream + non-stream, history flattening, typed
  refusal for response_format, tools passed through natively) and /models
  (org-discovery auth oracle + the bootstrap catalog). NO model discovery
  exists on this surface: ids pass through, the upstream answer is
  authoritative; the curated set only seeds routing.
- Tests: 11 client tests (cookie forms, the five failure modes, first-turn
  payload parity, follow-up chaining, no-commit-on-cut, error events,
  401-mid-stream, header set, real TLS handshake) + 9 bridge tests + 3
  adapter tests. Bite-proofs: core commit-on-cut, parent chaining,
  challenge detection; bridge ErrForbidden→403 mapping, response_format
  refusal, the removed model gate. Full suite green, -race green.

## 2026-10-16 — M13 github-copilot provider: official CLI device flow ✅
GitHub Copilot as a provider, ported from omniroute's production
implementation (design: `docs/github-copilot-design.md`, evidence:
`docs/github-copilot-policy-evidence.md`, runbook:
`docs/github-copilot-runbook.md`). The auth path is the OFFICIAL CLI's:
device flow with GitHub's public Copilot CLI client id and scope
`read:user` — not a custom third-party app (restricted tier/403), no
client-id spoofing. Live-proved 2026-10-16: the device-code request
against real GitHub succeeds.
- `internal/githubcopilot/auth.go` — device-code request + poll
  (slow_down honored, ctx-cancellable, injectable sleep so tests never
  sleep), `token <gh>` mint at copilot_internal/v2/token (NOT Bearer),
  refresh-token grant with the public client id and NO client_secret
  (9router#442), session encode/decode (JSON row; legacy bare-token rows
  lifted).
- `internal/githubcopilot/provider.go` — endpoint routing (claude* →
  /v1/messages native shim, *codex*/responses → /responses, gemini never
  /responses), full CLI header set with configurable CLI version (the 466
  floor), body quirks (tools ≤128, gpt-5.4 temperature strip, assistant
  reasoning strip, prefill drop, chat-only part sanitize, claude-native
  untouched), /models discovery (chat/enabled/picker filters) with the
  37-model curated fallback, NeedsRefresh 5-min margin, EnsureSession
  mint→refresh→mint chain.
- `internal/provider/githubcopilot.go` — KindGitHubCopilot adapter: session
  decode + per-account in-memory mint cache, /messages → /v1/messages
  anchoring, header set installed over client-supplied values, body quirks
  applied in place, GET /models auth oracle (mint happens in
  PrepareRequest, so calibration exercises the real path), ListModels via
  the minted token, ReadQuota honestly unsupported.
- `cmd/ezllm github-login` — interactive device flow: prints the
  verification URL + code, polls, verifies the mint, lists the account
  catalog, prints the session JSON to store.
- 19 tests (13 protocol + 6 adapter), three bite-proofs red-then-restored,
  `-race` green.

## 2026-10-16 — M12 gemini-web provider: cookie auth, oracle, streaming, bridge ✅
The full Gemini **web** protocol as a router provider, ported slot-for-slot
from the reverse-engineered reference (digest: `~/workspace/gemini-web-api-protocol-digest.md`,
design: `docs/gemini-web-design.md`, runbook: `docs/gemini-web-runbook.md`).
Unofficial surface; risk posture accepted by Calypso: cookie material = live
account access, test accounts, no fallback, typed refusals.
- `internal/geminiweb/` — protocol package:
  - `frame.go` — wrb.fr incremental parser (length-prefixed UTF-16-unit
    frames, boundaries at any byte incl. mid-rune). Its own tests caught a
    units-rescan desync across Feeds (fixed with a `scanned` offset) and
    pinned two fixture traps (raw-string `\n`, wrong unit counts).
  - `client.go` — cookie-pair credentials (PSID required, PSIDTS optional),
    `/app` scrape of the five session tokens, `RotateCookies` (PSIDTS rotates
    ~10 min); expired cookies = the distinct `ErrNoAccessToken`.
  - `rpc.go` — batchexecute + `GET_USER_STATUS`: zero-quota auth verdict
    (AccountStatus 1000/1016/…/1060) AND model catalog from one call.
    Catalog ported index-for-index (capacity tiers Plus 4/12 > Pro > Free
    1/12, model-header build incl. the `gemini-gemini-flash` alias quirk).
  - `generate.go` — `StreamGenerate` request construction (81-slot
    inner_req_list, model-header per-turn mutation) + incremental response
    parse (error code → typed `ErrUpstream` 1013/1037/1050/1052/1060,
    metadata [cid,rid], candidates text@[1][0] with artifact cleanup,
    thoughts@[37][0][0], completion@[8][0]==2, final chunk@[25]) and
    appended-tail delta emission with an incremental UTF-8 decoder
    (`utf8.FullRune` tail test). Tests caught a real double-encode bug
    (`json.RawMessage` inlining where the wire wants a JSON *string*) and a
    final-chunk-before-candidates ordering bug.
- Wiring: kind `gemini-web` + adapter (sentinel base `internal://geminiweb`
  rewritten to the bridge, account+key stamping so bad-key calibration
  exercises the real path, Authorization dropped) + in-process OpenAI-wire
  bridge (`POST /internal/geminiweb/v1/chat/completions`, `GET /models` as
  the auth oracle) + per-account client cache + rotator (8-min cadence)
  persisting via the new `store.UpdateKeyPlain` + typed refusals above the
  package (tools/response_format) + upstream-status→HTTP mapping.
- All mock-proven (29 protocol tests + 4 adapter + 7 bridge-through-sqlite +
  1 store), bite-proven, gates green, `-race` green on touched packages.
  Live cookie proof is operator-gated (runbook).

## 2026-10-15 — M11 gap closure: 1 MiB percentiles, fuzz moat, cancel-latency certificate
- **1 MiB latency percentiles** (`TestLatencyPercentiles`, n=2000, seq):
  stream total p50 9.14 ms / p95 15.65 / p99 16.09, TTFT ≈ total (the gigabyte
  body's parse dominates first byte); non-stream p50 9.77 / p95 16.74 /
  p99 40.15 (one 596 ms max — a swap/GC spike on the loaded box, reported as
  measured). These are e2e totals incl. the mock round trip.
- **Router-only percentile test** (`TestInspectRoutePercentiles1MiB`, n=2000)
  grades spec §18's "inspection+routing, ≤1 MiB, p95 ≤ 5 ms" ISOLATED from
  e2e noise: p95 4.75–5.28 ms across 3 runs — **straddling the target inside
  the noise band** (direct 4.75–5.07, combo 5.07–5.28). Honest grade: NOT
  certified; single-parse's map decode is the cause (old struct-decode ~1.7 ms
  mean, new map-decode ~4.0–4.5 ms mean). The trade stands (e2e −25%) but the
  row is recorded as boundary-UNCERTAIN, exactly as instructed by the spec
  itself ("revise targets based on measurement").
- **Fuzz moat** for the single-parse hot path: 4 targets, seed corpus +
  60 s live fuzz each (FuzzSwapParity: 467 306 execs, 0 divergence). Two
  findings worth their keep: (1) legacy `SwapModel` standing alone TOLERATES
  trailing garbage (dec.Decode stops after the first value) while
  `extractModel` rejected it — the old pipeline rejected such bodies at the
  gate, the new pipeline rejects at `jsonbody.Open`, so PIPELINE parity is
  what the fuzz asserts (oracle = a reimplementation of the old extractModel
  gate); (2) exponent literals overflowing float64 (`1e700`) survive the
  round trip via json.Number — the crasher is now a permanent seed.
- **Cancel propagation certified** (`TestCancelPropagationLatency`, n=1000,
  hard assertion in the test): client-cancel → upstream-cancel **p50 22 µs /
  p95 63 µs / p99 119 µs / max 216 µs** vs the spec's ≤ 20 ms. The §18 row
  now has a measurement instead of a 5 s correctness bound.
- Verified: gofmt/vet/build/full `go test ./...`/-race proxy+jsonbody all
  rc=0.

## 2026-10-15 — single-parse dispatch (M10, perf)
- `perf(proxy,server,compress)` the request body was JSON-decoded up to **5×**
  per request (`extractModel`, `IsStream` per attempt, `SwapModel` per hop,
  `compress.Apply`, `captureShape`). Now: **one decode** — new
  `internal/jsonbody.BodyDoc` (map + `UseNumber`, trailing-garbage rejection),
  opened once in `inference()` and threaded through compression, shape capture
  and every native hop's model swap (`ForwardCandidatesDoc`,
  `prepareHopBody(body, rt, doc)`). Translation legs, multipart STT and admin
  combo-test keep the byte path — `doc` is an optional hint with a legacy
  fallback (no flag; byte-equality tests are the rollback proof).
- Measured (same box as M1 baseline): e2e **−25.0%** non-stream / **−26.1%**
  stream at 1 MiB seq, −10…−16% par4/par16, −13.7% stream par16 at 64 KiB;
  e2e allocs ~748→~690/op. Honest negative: `InspectAndRoute` in ISOLATION
  got heavier (the one shared parse is a full map decode where the old
  single-field struct decode skipped the messages array) — the request-level
  metric is the one that counts. All numbers + the two bite proofs in
  `docs/single-parse-dispatch-design.md`.
- `prefix_sha` now sees JSON number literals verbatim (`1e10` and
  `10000000000` hash differently, big ids keep exact digits) instead of
  collapsing through float64 — correct for a cache-BREAK detector; documented.
- Verified: full `go test ./...` green with zero edits to existing call
  shapes (doc=nil path exercised by the entire old suite), byte-equality
  tests vs legacy `SwapModel` on 7 fixture classes (unknown fields, big ints,
  unicode, nested tools, dup keys), stream-flag parity, captureShape parity,
  two bite proofs (UseNumber drop → mangled big int; model-assignment drop →
  upstream sees `"model":null`), gofmt/vet/build/race green.

## 2026-10-13 — chat searchable model picker + per-model Test in the providers drawer
- `feat(web)` chat model `<select>` → **searchable combobox**: typing filters by model id
  OR label (namespace prefix included), ↑/↓ highlight, Enter picks, Escape/click-outside
  closes; empty states (loading / no models / no match) preserved, same input classes so
  the controls row does not move. Built for the "too many models to scroll" complaint.
- `feat(web)` providers models drawer gets a **`test` column**: a per-row `Test` button
  probes JUST that model (the sync endpoint's `models` scope, force re-test) instead of a
  whole `Test all models` sweep. Curated rows disable the button with an explanation —
  probe writes cannot overwrite an operator-curated verdict (server-proven), so clicking
  would only spend. While a sweep runs, the row button spins and the header line carries
  the clock; a scoped probe of an id that left the live catalog reports THAT as the cause.
- `api.ts` `startSweep` accepts `models?: string[]` (scoping the sweep; the catalog
  refresh still upserts the FULL list — a one-model test never prunes the drawer).
- Verified: `tsc`+vite green; live browser walk at 390×844 — all 8 routes
  `scrollWidth == clientWidth == 390`, `scrollX` stays 0, models drawer shows the 4-column
  table with 80 Test buttons and no overflow; combobox filter/enter/arrow/escape all
  exercised in the live page.

## 2026-10-13 — Cloudflare Workers AI key test fixed (account-aware oracle + native catalog)
- `fix(provider,registration)` adding Cloudflare Workers AI failed with `cannot verify this
  key: no credential-checking endpoint and no model to probe with / This gateway exposes
  neither /usage nor a catalog`. Two separate misses, both live-measured 2026-10-13:
  - **Oracle** — the generic `GET {base}/usage` does not exist there (405), and
    Cloudflare's real check lives outside the account base URL:
    `GET https://api.cloudflare.com/client/v4/user/tokens/verify` → 200 `{"status":"active"}`
    for a valid token, **401** for a well-formed `cfut_`+40-hex fake, but **400 format
    error** for a malformed fake — which calibration would have read as inconclusive.
    New optional `HostedAuthOracle` interface gives the adapter the ACCOUNT so
    openai-compatible can return a host-scoped oracle; `AuthProbe.CalibrationKey` carries
    the format-valid fake; `doProbe` now accepts an absolute `http(s)` Path (used verbatim,
    everything else still joins at `UpstreamURL`).
  - **Catalog** — the OpenAI-compat prefix has NO `/models` (405 live), and the native
    `GET /client/v4/accounts/{id}/ai/models/search` answers the Cloudflare envelope
    `{"success":true,"result":[...]}` which the OpenAI parser reads as ZERO models.
    `isCloudflareAI` (kind+https+host+`/ai/v1`, OllamaCloud parity) switches `ListModels`
    to the native envelope, skipping id-less entries.
- Non-Cloudflare openai-compatible accounts are untouched (explicit regression test), and a
  `success:false` catalog envelope now surfaces the provider's own words instead of
  passing as an empty catalog.
- Verification: 5 new tests (e2e key test through the real flow + detector + oracle shape +
  envelope parse + non-leak), **bites 3/3 fell for the right reason** (hosted oracle
  removed / CalibrationKey ignored / catalog branch removed), full suite green.

## 2026-10-13 — NVIDIA NIM key-test fix: measured-fast candidates, honest timeout verdict
- `fix(registration)` adding NVIDIA NIM failed with `generation probe failed: Post
  "https://integrate.api.nvidia.com/v1/chat/completions": context deadline exceeded`, and
  the hint blamed a provider rejection for a response no provider ever sent. NIM's shared
  free tier answered `deepseek-ai/deepseek-v4.1-flash` (the generic buckets' FIRST pick, by
  name) with nothing inside 90s and `z-ai/glm-5.3-flash` with 43.7s, against
  `defaultStepTimeout = 15s`. Ledger evidence for the failed add: `admin:test …
  deepseek-ai/deepseek-v4.1-flash status=0 15000ms`.
- `pickProbeModels` now leads with `nvidiaNIMProbeModels` on this host (host-scoped like
  `isOllamaCloud`, preset base URL `https://integrate.api.nvidia.com/v1`):
  `nemotron-3.5-lightning-30b-a3b` 0.6s · `llama-3.2-11b-vision-instruct` 1.7s ·
  `gpt-oss-20b` 6.2s — measured, matching the probe playbook's "pick by measurement, never
  by 'flash'/'mini' in the id". Each entry is matched against the account's own catalog; a
  full miss falls through to the generic buckets (NIM rotates ids: `meta/llama-3.1-8b-instruct`
  went EOL 2026-08-26 while still listed).
- A transport error/timeout in the generation probe is no longer `probeFailed` with a
  rejection hint: it carries an honest "did not answer within 15s" verdict and ADVANCES to
  the next candidate; only the last candidate's timeout fails the test. 401/403 and 4xx
  verdicts are unchanged.
- NIM's `/v1/models` verified PUBLIC (200 + full catalog for a garbage key), so the catalog
  can never prove a credential — the generation probe stays the gate.
- Verification: 5 new tests (2/2 bites fell for the right reason), full suite green, and a
  live scratch-instance proof through the real handler — `tests/prove_nim_keytest.py`,
  201 in 2.1s, `inference: nvidia/nemotron-3.5-lightning-30b-a3b · 766ms`, ledger row
  `status=200`.

## 2026-10-13 — provider presets: NVIDIA NIM, Cloudflare Workers AI, OpenRouter
- Dashboard add-provider presets: **NVIDIA NIM** (`https://integrate.api.nvidia.com/v1`),
  **OpenRouter** (`https://openrouter.ai/api/v1`), and **Cloudflare Workers AI**, whose
  account ID composes the endpoint `https://api.cloudflare.com/client/v4/accounts/{id}/ai/v1`.
  All three are ordinary `openai-compatible` accounts — Workers AI serves the whole OpenAI
  family under that prefix (chat, responses and an anthropic-compatible `/messages`), so no
  new protocol was needed. The `cf-aig-gateway-id` header only applies to Cloudflare's AI
  Gateway (`/ai/run`), not to these endpoints.

## 2026-10-13 — strategy citizenship, the hold checkbox, request IP (M7; local, not pushed)
- `feat(router)` two operator decisions on the universal matrix: translated hops are ordinary
  combo members (the old "native hop always wins" tail rule is removed), and
  `translation.universal_enabled` is the SINGLE switch for the matrix (no longer requires
  `translation.enabled`) (`b1a2f5b`).
- `feat(store,router,web)` schema v14 — `combo_hops.hold_until_failure`: a block_round_robin
  hop the operator checks HOLDS its block while it keeps succeeding and releases it on a
  hop-attributable failure (429/5xx/transport/failed-truncated stream; a 4xx keeps the hold).
  Combo editor checkbox + a `hold` chip in the combo list.
- `refactor(router)` `strict_round_robin` REMOVED (no live combo used it; its success-driven
  advance is now block_round_robin with a held hop).
- `feat(store,server,web)` schema v14 — `calls.ip`: the caller address (X-Real-IP, then the
  first X-Forwarded-For entry, then the socket peer) captured for traffic mapping; shown in
  the dashboard under the request timestamp. Lat/long dropped by operator decision.
- `feat(web)` requests explorer: tokens in/out consolidated into one `tokens ↑↓` column with
  hover titles; the feed's arrows get proper hover titles too.
- Migration rehearsed on a live-DB copy: v11→v14, calls 6052 / accounts 11 / models 624 /
  combos 11 / hops 39 / tokens 6 preserved, zero invented holds or IPs, second boot idempotent,
  production untouched at v11.

## 2026-10-12/13 — universal wire matrix (M0–M6; local, not pushed)
- `fix(translate)` M0: Responses streams lost tool arguments when the item id and call id
  differed, and dropped terminal usage on incomplete/failed runs (`00232c5`).
- `feat(router,proxy,server)` M1: wire identity — `Route.WireSurface`/`EffectiveWire()`, the
  wire decided BEFORE capability/pin gates, typed `wire_unsupported` (400) with zero upstream
  calls for a disproved destination (`14e4730`).
- `feat(translate)` M2: surface-neutral request IR + one codec per surface; unsupported
  features refuse with a reason; absent vs zero preserved (`3a3ab3c`).
- `feat(translate)` M3: shared response/event codecs with the legacy wrapper cutover —
  byte-identical to the pre-cutover package across a 35×3 + 35×3 corpus (`f7ee36b`).
- `feat(store,provider,translate)` M4: `accounts.usage_convention` (schema v13) — the
  operator-declared Messages input-token accounting; a cross-wire Messages destination refuses
  an unstated convention. Also fixed two pre-existing bugs: a partial PATCH blanked `notes`,
  and `UpdateAccount` swallowed non-conflict write errors (`7294755`).
- `feat(proxy,router,server,config)` M5: the three new cells (chat→messages, responses→chat,
  responses→messages) behind `translation.universal_enabled` (default OFF; env rollback), the
  fixed fallback chains, pin-as-intent, and the completion outcome (`Result.StreamOutcome` +
  ledger err; late stream failures withhold the hop-good signal) (`5a145dd`).

## 2026-10-10 — M9: research schema (v8) + honest savings + TTFT + cost
- `fix(proxy)`: TTFT was timed from inside the stream copy — after headers had already
  arrived — so every row read 1–3ms. Now timed from upstream dispatch; the number finally
  means time-to-first-token, which is what caching research measures (`f48d20a`).
- `fix(compress)`: `Saved` was set before the ratio-floor check, so rejected rewrites still
  reported savings (prod week: 18.2k real vs 21.4k notional in one SUM). `Saved` is now 0 on
  any skip; `SavedNotional` carries the rejected potential (`19b743d`).
- `feat(store)` schema v8: request-shape capture (`prefix_sha` of system+tools+history-minus-
  newest-turn, `msg_count`, `tool_count`, `session_id` via `X-Ezllm-Session`, `req_bytes`) —
  the cacheability data `raw_usage` could never reconstruct; `compression_saved_notional`;
  versioned `model_prices` (account×model×date) with cost DERIVED at query time, unpriced
  calls counted not zeroed (`393460b`).
- `feat(server)`: capture runs on the inference path after compression (a history rewrite
  measures as the cache break it is); admin API `GET /admin/prices`,
  `PUT/DELETE /admin/accounts/{id}/prices`, `GET /admin/cost`; requests projection surfaces
  the shape columns (`0e24e32`).
- `feat(web)`: Economics cost panel (spend, priced vs unpriced, per day/account/model) +
  Providers Pricing drawer (per-account rate versions; all four rates required) (`ec27973`).

## 2026-10-09/10 — providers batch, UI cleanup pass, surface pin (deployed 2026-10-10)
- `feat(provider)`: fourth kind `gemini-openai` — Gemini's OpenAI-compat endpoint; its bad-key
  signal is `400`, honored by the auth gate only for that kind via the optional
  `AuthRejectStatuser` (`a8776ee`).
- `feat(web)`: provider presets — Command Code, DeepSeek, TokenRouter, Gemini, Groq built-in
  (`5335423`, `1d3f3d7`, `fb2a2fd`).
- `feat(registration)`: dropped all API-key **shape** checks; the probe chain is the sole
  validator, so exotic key formats are judged by the upstream (`9aa9cf1`).
- `fix(server)`: `GET /healthz` is liveness-only (`{"status":"ok"}` after a DB ping) — the
  public route no longer leaks counts, paths, or versions (`9a7b620`).
- `feat(web)`: dashboard UI cleanup pass — shared `ConfirmModal` replaces `window.confirm`
  and one-click deletes; Providers key status shown as text (`tested ok` / `test failed` /
  `untested`) not a bare dot; quota copy "quota not available"; Connect drops the Responses
  surface from the picker (server route unchanged), 2-column desktop layout; Dashboard strip
  trimmed (no ledger/dropped/schema chips, no context column); combo "custom model id" flag now
  defaults from what's actually rendered, and the profile-number pill is gone (`c099dd5`…`9915582`).
- `feat(pin)`: per-model **surface pin**, enforce-only (schema v7 `models.proto_pin`). A
  wrong-surface call to a pinned model is refused `400 pinned_surface` before any upstream
  dial; never a reroute (bodies pass through untranslated). Lookup fails closed; catalog
  resyncs preserve pins (`340442b`).
- `fix(server)`: the pin refusal reached clients as `502` while the ledger logged `400` —
  the proxy error switch had no `ErrPinned` case. Now `400 pinned_surface` with structured
  `{pinned, requested_surface}`; live-proved on prod (`03a6317`).
- `build(web)`: **rebuilt and committed `web/dist`** — the image embeds it, so source-only UI
  commits shipped the old bundle; after every UI commit, `npm run build` + commit `dist` +
  verify the served hash post-deploy (`831ecda`).
- GitHub Copilot: OAuth app registered, device flow works end-to-end, but the
  `copilot_internal/v2/token` exchange returns `403` for third-party apps → parked, not
  circumvented. OAuth artifact deleted.

## 2026-10-09 — post-ship fixes
- `fix(proxy)`: strip client `Origin` before forwarding — Anthropic reads any browser `Origin`
  as CORS and rejects with a 401; regression-tested (`8c832ef`).
- `fix(web)`: clear the dashboard session only on ezllm's own auth 401; provider 401s surface
  inline (`358cda3`).

## 2026-10-08 — Docker, dashboard polish, budget engine
- Distroless image + home-box Traefik overlay; explicit router priorities and a dedicated
  `/admin/login` limiter (`3f46f00`, `3155b76`).
- `POST /admin/chat` behind the real routing path; Chat page, Token Economics, budget controls
  (`d0f8407`, `d8d0f68`).
- Budget stage with its own context-token accounting (schema v5) (`39cbdb8`).
- Combo retry with exponential backoff before failover; `EZLLM_*` env overrides restored for
  containers; per-combo enable toggle; enabled combos advertised in `/v1/models`.
- Provider add-in-one-dialog (key validated on save) with a Claude Platform preset; lm-eval
  canary proving traffic goes through the gateway.
- MIT license.

## 2026-10-07 — compression, rules, live surface
- Compression: M5 pipeline (engines, profile CRUD, inference wiring); headroom + lite engines
  (M6.5 phase 2); caveman lossy prose condensation, gated by construction (M7).
- Rules eligibility engine — token caps + allowed-use windows; dashboard rules editor.
- Live recent-requests feed (SSE over fetch); stats explorer (granularity × range); Requests
  page with token dissection.
- Dashboard password + plaintext provider keys (explicit operator choice).

## 2026-10-06 — core: M1–M4
- M1: skeleton + health.
- M2: store + field crypto + three-surface passthrough (`/v1/chat/completions`, `/v1/messages`,
  `/v1/responses`), usage normalized into the SQLite ledger.
- M3: admin API + embedded dashboard; registration with capped key-test protocol sweep; live
  model-catalog probes; adapters (`opencode-go`, `openai-compatible`, `anthropic-compatible`).
- M4: combos resolve to ordered candidates + strategies; proxy attempt/commit split (failover
  that never leaks a failed hop); admin persistence (accounts, provider keys, combos, tokens).
