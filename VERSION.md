# VERSION

Version history for ezllm. One version per change date — all patches committed
on the same date share one version. The patch component increments by one per
date (`v0.0.1`, `v0.0.2`, …).

The current version is served by `GET /healthz` (`version` field), shown in the
dashboard health strip, and asserted against this file by
`internal/server/version_test.go` — bump `internal/server/version.go` and this
file in the same commit, or the test fails.

**Current: v0.0.20**

---

## v0.0.20 — 2026-10-20

The compression stack grows to ten engines, wire translation becomes the
default, and the dashboard gets a data-terminal redesign.

- **Five new compression engines**, all on one wire-neutral text walker so a
  request is transformed identically whether it arrives on chat, messages or
  responses: `ansi` and `fold` (terminal output), `jsonmin` (whitespace-free
  JSON tool output), `tool_ref` (repeated tool results become references) and
  `blobs` (base64 runs and data URIs become markers). The `budget` engine is
  **deleted** — it needed a known `max_tokens` and silently did nothing
  without one.
- **The pipeline is now ordered by meaning.** A per-profile `tool_classes`
  table says what each tool is (file read, edit, diff, …); a file read now
  ships byte-identical so the agent's next edit still matches, and an unknown
  tool is its own class — never truncated, only tidied by the lossless
  engines. `fresh_tool_output` decides whether a final message that is purely
  tool output may be trimmed on its first send (schema v24).
- **A committed-prefix memo** per request, so the cached prefix is exactly
  what was committed; when dedup drops a segment the commit is exact rather
  than approximate.
- **Universal wire translation is default-on** (no yaml needed; an
  anthropic-only account is pre-marked so the universal matrix translates
  instead of refusing). One translation path in `commit`, a `gatewayFault`
  helper, and the `zz_` files renamed.
- **The proxy bills every input token into one bucket**, honours the account
  usage convention, and splices the hop model into the client bytes instead
  of re-marshalling them. A hop stays parked after a transport error, and an
  oversized body is a 413.
- **Provider and registration honesty**: quota and catalog reads honour
  custom headers, pagination and no-endpoint statuses; the management key
  stays on tokenrouter; retries honour http statuses and sweeps keep proven
  verdicts; a claude-web key keeps its wire.
- **Perf**: one catalog read per hop, an in-memory routing catalog snapshot
  invalidated by `catalog_gen` (schema v23), a throttled reservation prune
  and an allocation-free header strip.
- **The dashboard is a data terminal** — exact numbers (never abbreviated),
  motion.dev throughout, and a screen-by-screen polish pass verified in a
  browser. The build is now reproducible: `web/src/index.css` excludes
  `web/dist/` from Tailwind's content scan, because `dist/` is a tracked
  build output and Tailwind was reading the previous build's class names
  back into the next one.
- `unmetered_rows` on health, overview, live feed and dashboard; the tz day
  report's `applied_saved`; the meter is all-or-nothing per batch; web
  bridges now require a process secret.
- **CI actions move off deprecated Node 20.** Eight pins were still on Node
  20 targets that the runners have deprecated and were force-shimming onto
  Node 24: `actions/checkout@v4`, `actions/setup-go@v5`,
  `actions/setup-node@v4`, `docker/setup-qemu-action@v3`,
  `docker/setup-buildx-action@v3`, `docker/login-action@v3`,
  `docker/build-push-action@v5` and `softprops/action-gh-release@v2`. They
  are now `checkout@v5`, `setup-go@v6`, `setup-node@v6`,
  `setup-qemu-action@v4`, `setup-buildx-action@v4`, `login-action@v4`,
  `build-push-action@v7` and `action-gh-release@v3` — every pin resolves to
  a Node 24 action natively, and a pin audit asserts it.
  `setup-go@v7` and `setup-node@v7` were skipped on purpose: both are ESM
  migrations with breaking changes, and v6 already carries the
  `@actions/cache` and `undici`/`fast-xml-parser` security updates.
  `build-push-action` had no such choice — v5 and v6 are both still Node 20,
  so v7 is the only Node 24 line, and its breaking changes (the ESM switch
  and dropping two deprecated env vars this repo never set) do not touch the
  inputs used here. No product behaviour changes; the gate and the image
  build are otherwise identical.
- **The image build stops pretending to be multi-arch.** The container was
  built for `linux/amd64,linux/arm64`, but the arm64 half ran the whole
  `golang:1.27-alpine` + distroless + Go toolchain under QEMU emulation on
  an amd64 runner — 7 of the 8 minutes of every push — and no host in the
  fleet is arm64 (calypsocloud-1, Media-Server, GAME-PC and the OVH VPS are
  all x86_64). The image is now `linux/amd64` only, the QEMU setup step is
  gone, and the build uses the GitHub Actions layer cache
  (`cache-from`/`cache-to: type=gha,mode=max`) so the Go module download and
  the compile are cached between pushes instead of redone from scratch every
  time. `ignore-error=true` on the export: a cache write can fail after the
  image is already pushed, and the worst acceptable outcome is a cold cache,
  not a red build. arm64 *binaries* are unchanged — `release.yml` still
  ships linux/arm64 and darwin/arm64, cross-compiled natively by Go.

---

## v0.0.19 — 2026-10-19

Claude subscription surface: **one container**, no sidecar.

- **Design pivot.** The `claude-bridge` sidecar (M1's first shape) is deleted:
  a second image, a second container, an inter-service token and a published
  port for what is fundamentally "spawn this binary". The surface now runs
  **in-process**, on a loopback-only listener (`127.0.0.1:8790`), and an ordinary
  `anthropic-compatible` account dials it — no new provider kind, no router
  change, no inter-service credential.
- **The CLI is bind-mounted, not baked.** `~/.local/share/claude/versions/2.1.299`
  mounts read-only at `/opt/claude/claude`; the subscription login mounts
  read-write at `/claude-sub` (the CLI refreshes its own token there). The
  account's shared secret auto-generates into `/data` on first boot, like the
  master key.
- **Runtime base: `distroless/base-debian12`** (was `/static`). Measured: the
  Claude Code CLI is a glibc-dynamic ELF (`ld-linux`, libc/librt/libpthread/
  libdl/libm), and on `distroless/static` the exec fails with "no such file or
  directory" because the loader is absent — that failure was observed live.
  Alpine is smaller (7.81 MB) but musl, so it cannot run this binary at all.
  `base` is 20.8 MB and the smallest maintained base that runs it: glibc + CA
  certs + NSS (DNS needs `libnss_dns`, invisible to `ldd`), still no shell. The
  ezllm binary itself remains static.
- Same rails as M1: text-only (`chat_tools_unsupported` before any spawn),
  `CLAUDE_CODE_EXTRA_BODY` body injection, `shouldQuery:false` history replay,
  native error codes mapped to honest statuses, boot asserts the CLI version and
  flag surface (`--max-turns`/`--system-prompt-file` are accepted but absent from
  `--help` on 2.1.299).
- **Probe budget (the NIM lesson, again).** The key-test generation probe and the
  protocol-sweep surface probe budgeted **8–16 output tokens**. Measured: a model
  with mandatory thinking (Claude Haiku 5.5) cannot answer in that and returns
  `max_output_tokens`, so a perfectly good key failed the inference step — and the
  sweep would misread a Messages-only upstream. Probes now budget 256 tokens
  (`internal/registration/probe_body.go`), with a test that fails if anyone
  shrinks it.
- `EZLLM_CLAUDE_BRIDGE=on` enables it; without a usable CLI or login the boot
  fails loudly. Loopback is enforced — the surface refuses a non-loopback bind.

## v0.0.18 — 2026-10-19

Claude subscription sidecar (`claude-bridge`) — milestone M1, text-only.

- **New `cmd/claude-bridge` + `internal/claudebridge`**: an Anthropic Messages
  surface that drives the official Claude Code CLI against a Claude subscription
  login. The CLI is the only thing that speaks to the subscription — the
  credential never leaves its own login store. ezllm dials the bridge as a plain
  `anthropic-compatible` account, so no router kind, surface or translation cell
  changes.
- **Upstream body injection**: the client's Messages body reaches upstream through
  `CLAUDE_CODE_EXTRA_BODY` (the official plugin's mechanism), with `system` held by
  `--system-prompt-file` and stdin frames marked `shouldQuery:false` on replay.
  Measured on CLI 2.1.299: the body drives the answer (a body saying "seven" beats
  trigger frames saying "eight"), one generation per call, and the CLI's own
  ~20k-token harness overhead disappears (170 input tokens for a three-message
  body, vs ~20k on the naive `-p` path).
- **M1 is text-only and says so**: tool-bearing requests are refused with
  `chat_tools_unsupported` before a process is spawned.
- Error mapping keeps the CLI's words and names the cause: `rate_limit`→429,
  `billing_error`→402, `authentication_failed`→401, `overloaded`→529,
  `max_output_tokens`→400 (a client-fixable ceiling, not a retryable 5xx).
- Live proof `tests/prove_claude_bridge.py`: 13/13 against the real CLI and a real
  Pro subscription — streaming SSE, native usage fields, refusal, and history
  fidelity.
- `Dockerfile.claude-bridge` + `compose.claude-bridge.yml`: distroless-static
  image; the CLI binary (265 MB, read-only) and the login directory are mounted
  and pinned by mount path; no published ports.

## v0.0.17 — 2026-10-19

Same-day batch: image content across all three wire surfaces (`f13ffdc`);
geminiweb re-scrapes the SNlM0e access token after PSIDTS rotation and reverts on
failure (`ce5c2dc`).

---

## v0.0.16 — 2026-10-19

Claude (web cookies) multi-turn conversations.

- **Follow-ups chain the server's assistant uuid**: the assistant message
  uuid is assigned by Claude in the `message_start` SSE event, not by the
  client. Chaining the locally generated placeholder returned an empty-body
  400 on turn 2. `consumeSSE` now captures `message.uuid` and commits it as
  the next turn's `parent_message_uuid`.
- **Follow-ups carry a fresh `human_message_uuid`**: the payload previously
  sent only `conversation_id` + parent on continuation turns; the live wire
  requires the human uuid too (also an empty-body 400).
- Non-2xx errors now include a bounded upstream body snippet, so a bare
  "status 400" is diagnosable.
- Tests: `TestFollowUpPayloadCarriesAllThreeUUIDs`,
  `TestConsumeSSECapturesServerAssistUUID` (both bite).

## v0.0.15 — 2026-10-19

Claude (web cookies) completions work against the live wire.

- **Full cookie header is required**: Claude/Cloudflare answers a
  sessionKey-only request with a synthetic `429 rate_limit_error` in ~30ms;
  the same completion carrying the browser's `cf_clearance` / `__cf_bm` /
  `anthropic-device-id` / `sessionKeyV3` cookies succeeds (measured 30ms 429
  vs 1.25s 200). `NormalizeSessionCookie` now preserves the operator's whole
  header (still accepting a bare token) and the bridge sends
  `anthropic-device-id` from it.
- **Streaming timeout fix**: `TLSFetcher.Fetch` cancelled its request context
  on return, so every SSE read died with `context canceled` and 429 bodies
  were misreported as transport errors. The cancel is now tied to
  `Body.Close()`; `TestCancelOnCloseKeepsBodyAlive` guards it.

## v0.0.14 — 2026-10-19

Gemini (web cookies) generation is live.

- **geminiweb generate**: the request now matches the reference client's wire
  identity — `_reqid` seeds a random 5-digit value (bumped by 100000 per turn),
  the turn uuid is shared between payload slot 59 and the
  `x-goog-ext-525005358-jspb` header, and the model header always carries a
  per-client session uuid. `TestGenerateWireIdentity` guards all three.
- **geminiweb stream framing**: strip the `)]}'` anti-XSSI prefix before the
  length-prefixed frames, unwrap the one-level `[["wrb.fr",…]]` envelope, and
  count the declared frame length the way Google does (it includes the
  separator newline). `TestGenerateRealWireShape` resumes the live shape.
- **Local test ergonomics**: `make test-quiet` pins the suite to cores 0–1 with
  `nice`/`ionice`, and `make check` refuses to run unbounded locally unless
  `EZLLM_ALLOW_FULL_CHECK=1`. CI remains the full gate.

## v0.0.13 — 2026-10-18

Native image/audio surfaces and the playground dashboard pass.

- **Native image adapters**: Cloudflare Workers AI image models now use
  `/ai/run/<model>` (flux-2 multipart, other models JSON `prompt`/`seed`)
  instead of the OpenAI `/images/generations` path.
- **Native audio adapters**: Cloudflare TTS/STT use `/ai/run/<model>` with
  MeloTTS `{"prompt": ...}` translation; Google AI Studio TTS/STT translate
  OpenAI audio bodies to native `generateContent` and back (PCM wrapped as WAV,
  STT text in OpenAI shape). Tests in `internal/proxy/gemini_audio_test.go`.
- **Dashboard**: Linear-inspired palette and sidebar icons; Providers models
  render as a compact 4-column status grid with per-row test indicators;
  Services use a two-column card grid with surface badges.
- **Chat is a playground**: 1–3 side-by-side columns, each with its own model
  selector and transcript, one detached composer broadcasting to every
  selected model. Added columns carry the remove control; the first does not.
- **Dashboard**: new routing-rescue panel surfaces recent calls that failed
  over and the hop trail that saved them.
- **Security (Milestone A)**: provider API keys are encrypted at rest with
  AES-256-GCM. The master key comes from `EZLLM_MASTER_KEY`, else
  `~/.hermes/secrets/ezllm-master.key` (auto-generated 0600 on first boot).
  Existing plaintext rows are rewritten in place on boot, idempotently;
  `PickKey` decrypts only for the outbound hop. Tests in
  `internal/store/crypto_integration_test.go`. **Back up the master key with
  the DB — losing it loses every provider key.**
- **Provider quota**: added TokenRouter management-key balance oracle
  (`/api/management/self/wallet`), parsed into a money quota. The management key
  is an optional per-account credential (`management_key`, schema v19), stored
  encrypted at rest and entered in the Add/Edit provider form; the API exposes
  only `management_key_set`. Tests in `internal/store/crypto_integration_test.go`
  and `internal/provider/quota_tokenrouter_test.go`.
- **Security (Milestone B)**: `/admin/login` now also sets an HttpOnly,
  SameSite=Strict `ezllm_session` cookie (Secure on HTTPS). The admin
  middleware accepts that cookie, so the dashboard no longer stores the admin
  token in `localStorage`; it sends `credentials: include`. Five failed
  password attempts per IP in fifteen minutes answer 429 (empty-password
  probes are exempt). Tests in `internal/server/dashboard_auth_test.go`.
- **Price catalog API**: `GET /v1/prices?model=&account_id=&at=` returns the
  current/historical rate per account+model; `POST /admin/prices/import` bulk-upserts
  snapshots; `POST /admin/prices/refresh?source=litellm` pulls LiteLLM's public
  price map and matches by exact model-id suffix. Prices carry `source` and
  `fetched_at`; schema v22. The refresh runs **in-process** (~45s after boot,
  then daily at 03:00 in `EZLLM_USAGE_TZ`), so a self-hosted instance needs no
  external cron.
- **Fix**: key retest and provider catalog refresh decrypt the stored key before use;
  encrypted keys no longer reach providers as `enc:v1:`.
- **Per-key budgets enforced**: client token `cap_tokens`/`cap_window` meter in
  `client_usage_counters` (schema v20) inside the ledger transaction; a spent
  key returns `429` before dispatch. Caps set after usage backfill the current
  bucket from `calls` once. `max_tokens` is reserved before dispatch and released
  on return; reservations expire after 15 minutes. Schema v21 adds
  `allowed_models` (glob allowlist, `403 model_not_allowed`) and `expires_at`
  (`401 token expired`). Tests in `internal/store/client_budget_test.go`,
  `internal/store/client_policy_test.go`, `internal/server/client_budget_test.go`,
  and `internal/server/client_policy_test.go`.
- **Web providers (claude-web/gemini-web)**: the bridge now serializes
  generation per account (one in-flight turn per cached client) and moves
  Gemini cookie rotation under the same lock; the Claude client decompresses
  zstd/br/gzip responses and accepts the live API's numeric organization id.
  Live-verified: org discovery resolves on a real session.
- **Streaming**: a mid-stream upstream connection drop now ends the stream
  with a terminal SSE error frame in the client's dialect (OpenAI, Anthropic
  or Responses) instead of a silent EOF; the ledger still records the
  truncation.
- **Dashboard chat**: `chatStream` works with the httpOnly cookie session
  (no admin token in localStorage required) and sends `credentials: include`,
  fixing the red "unlocked" box with zero network activity.

---

## v0.0.12 — 2026-10-17

Repo housekeeping and version plumbing.

- **Versioning**: new `VERSION.md` (this file); `internal/server/version.go`
  single source of truth; `GET /healthz` now reports `{"status":"ok","version":…}`;
  `/admin/overview` health object carries `version`; dashboard health strip shows it.
- **Docs**: README rewritten (professional, no personal deploy details);
  CONTRIBUTING.md and AGENTS.md added; EXPLORING.md/API.md/CHANGELOG synced
  to shipped state; stale landmarks corrected.
- **Tests relocated**: live proof scripts `scripts/prove_*.py` → `tests/`;
  Playwright drivers `web/shot-*.mjs`, `web/verify-*.mjs` → `web/tests/`.
  Go `_test.go` files stay in-package (Go toolchain requirement — see CONTRIBUTING).
- **Makefile**: test targets carry CI's memory envelope (`GOMEMLIMIT=3GiB`,
  `GOGC=50`), `test` runs `-count=1 -timeout 15m` (CI parity; cached results
  were masking changes), new `test-short` for constrained environments.
- **CI**: `release.yml` tag builds gated by the reusable `test.yml`
  (`needs: test`) — a red suite never produces a release asset either.
- **Provider credential entry (both web protocols)**: GitHub device-flow stall
  fixed — `slow_down` is now surfaced (`GHDeviceStatus.slow_down`) and the
  dashboard raises its poll interval (RFC 8628 §3.5) instead of folding it into
  plain pending forever; poll/mint run on a detached context so a tab reload
  cannot cancel an already-granted authorization; 5xx/429/context errors are
  retryable instead of terminal (one blip used to poison a live handle); the
  pending panel now renders `detail`, every status failure reaches `ErrBox`
  (non-ApiError was silently swallowed), and the flow logs (handle prefix only).
- **gemini-web cookies**: the key field is a textarea (`multilineKey` preset
  flag) and `ParseKey` accepts a WHOLE cookie header (`Cookie: a=b; c=d`,
  `document.cookie`, or one per line), keeping only `__Secure-1PSID`/TS by
  EXACT name — the old line-wise split stored the entire rest of the header as
  the PSID value. Guards: 4 new ParseKey tests + `slow_down`/classification
  tests, all three bite-proven red without their fix.
- **Combos hop account select**: options grouped by provider (`<optgroup>` from
  `base_url`, count shown only when a provider has >1 account) so multi-account
  providers are visible without a schema change.

## v0.0.11 — 2026-10-16 · 30 commits (02b1c40..75a46e5)

The four web-protocol providers, compression addenda, and the CI gate.

- **M11 gap closure**: 1 MiB percentiles, fuzz moat, cancel-latency certificate.
- **M12 gemini-web**: wrb.fr frame parser, cookie auth, GET_USER_STATUS oracle,
  StreamGenerate + incremental frames, provider kind + in-process bridge; runbook.
- **M13 github-copilot**: device flow, mint chain, routing, quirks, catalog,
  adapter + `ezllm github-login`; Copilot third-party OAccess policy evidence.
- **M14 claude-web**: uTLS Chrome transport, session auth, SSE conversation
  state machine, provider adapter + loopback bridge; runbook.
- **Compression addenda** #2–#7: hop compatibility groups + context-size
  filtering, request-wide attempt budget + selection deadline, immutable
  profile versions, absolute floor + CPU cap + persisted decision versions,
  per-engine attribution + sampled shadow mode, rtk tool-class filters.
- **Correctness**: caveman §11 rule safety, one authoritative applied-savings
  aggregate (WS-A), disjoint input/output economics (WS-B), committed-prefix
  stability (WS-C).
- **CI**: `test.yml` gate (gofmt + vet + test + web build) now blocks
  `docker-publish` via `needs: test`.
- **Web**: provider presets for the three web-protocol providers, Copilot
  device-flow login, bar-chart ordering, railway backdrop restraint, token
  economics cold-input fix.

## v0.0.10 — 2026-10-15 · 11 commits (485ffc7..e5033eb)

Performance baseline and the single-parse dispatch.

- **M1 perf baseline**: phase benchmarks, latency percentile harness,
  cancellation / slow-consumer / retained-memory tests (no leak: ~1.3–2 KB/req).
- **M10 single-parse dispatch**: one JSON decode per request through
  extractModel → Apply → captureShape → prepareHopBody (−25% e2e non-stream,
  −26% stream at 1 MiB; design `docs/single-parse-dispatch-design.md`).
- **Providers**: capacity-shaped generation failure passes with a proven key,
  sweep registry can no longer wedge, real NDJSON step chain, transient
  probe-status badge, drawer retest-all + live status.
- **Web**: login page (first-run hint, railway backdrop), dashboard polish
  (analytics, economics, rules, requests, compression), repo file sort.

## v0.0.9 — 2026-10-14 · 10 commits (abd130b..44b9901)

Service-level endpoint defaults and host-scoped quota.

- **Schema v15**: `service_bindings` + `service_probes` for per-service
  endpoint defaults (embeddings / images / audio TTS / audio STT).
- **Server**: per-service default bindings with honest typed refusals;
  multipart STT gets a missing model field injected.
- **Quota**: host-scoped same-key quota oracles, `ezllm` UA on own requests,
  `tokens_today` / `requests_today` wired end-to-end, count-shaped quota
  rendered on provider cards, token kind shown in key tests.
- **Web**: Services page (endpoint defaults, honest states, prompt tests),
  cache split carried onto every client wire, docx UI list.

## v0.0.8 — 2026-10-13 · 18 commits (f7ee36b..4232902)

Universal wire matrix, four new inference endpoints, and key-test hardening.

- **Universal wire matrix**: surface-neutral request IR, shared response/event
  codecs, per-account Messages usage convention (schema v13), translated hops
  as strategy citizens, single `translation.universal_enabled` switch.
- **Endpoints**: embeddings, image generation, audio TTS + STT (multipart),
  with per-surface default models (`/admin/services` contract).
- **Key tests**: NVIDIA NIM measured-fast probe models + timeout advances to
  the next candidate; Cloudflare Workers AI account-aware oracle with absolute
  probe URL and `@cf/…`-name catalog ids; plan-free model probe; retired-model
  and empty-balance survival.
- **Web**: searchable chat model picker, per-model Test column, 7-surface
  model-drawer pinning, NVIDIA NIM / Cloudflare / OpenRouter presets,
  caller IP + in/out token fold on recent calls.
- **Aliases**: VS Code tokenized aliases (OmniRoute parity).

## v0.0.7 — 2026-10-12 · 37 commits (8afa172..3a3ab3c)

The translation day: anthropic↔openai and chat→responses, probe-gated.

- **anthropic→openai translation**: wire-protocol fixtures, deterministic
  request translation (golden-tested), openai→anthropic response translation,
  SSE synthesis, config knob + hop eligibility; translated hops ordered after
  native.
- **responses-wire translation**: chat client → `/v1/responses` hop,
  curated responses-wire hops for chat clients; anthropic↔responses leg
  (Claude Code on responses-only hops).
- **Visibility**: hop-aware routing with visible failover, block-position
  tagging + failover trail, block-cursor advances logged, upstream error
  cause classification + cause chips, consolidated recent-calls table,
  combo hop-test modal, request IP.
- **Verdicts**: curated protocol verdicts (schema v11), whole-catalog protocol
  sweep ("test all"), wire identity + wire-first selection.
- **Web**: Vercel AI Gateway preset with per-provider quota, dashboard
  polish pass (10-item list), cause chips + current-state tooltip.

## v0.0.6 — 2026-10-11 · 12 commits (c5053c8..0044b00)

Routing honesty and dashboard rev 2.

- **Router**: true `true_round_robin` (stateless dice roll), success-driven
  `strict_round_robin`, `block_round_robin` block size, the dead success hook
  (`MarkHopGood`) wired for the first time; fail closed on store errors;
  never cache decrypted keys.
- **Registration**: per-test adapter resolution (not per process), plan-free
  probe so a plan refusal is not read as a bad key.
- **Proxy**: a status the upstream never sent is never written; hidden causes
  surfaced.
- **Store/ledger**: lost ledger batch made visible, p50 computed in SQL,
  409 on conflict; repeated user turn never dropped from history; timezone
  that will not load is refused instead of silently falling back to UTC.
- **Web**: dashboard UI rev 2 (analytics explorer, light/dark theme, chat
  upload), day alignment with the meter, disaster-recovery export stream,
  `least_used` dropped from the strategy picker.
- **Presets**: Ollama Cloud.

## v0.0.5 — 2026-10-10 · 21 commits (c099dd5..9d05fa6)

Surface pins, research schema, and honest economics (M8/M9).

- **Surface pin**: per-model `proto_pin` (schema v7) — enforced, never
  rerouted; wrong-surface calls refuse `400 pinned_surface` before any dial.
- **M9 research schema (v8)**: request-shape capture (`prefix_sha`,
  `session_id`, `req_bytes`), versioned model prices + derived cost, real vs
  notional savings split, TTFT measured from dispatch.
- **Admin API**: request-shape + pricing/cost endpoints, `GET /admin/requests`
  token filter, `POST /admin/chat`.
- **Routing**: per-account request budget, block round robin, honest strategy
  implementations; 429 is a failover signal, not a retry signal.
- **Registration**: 400 from the auth oracle defers instead of failing; no
  doubled HTTP prefix in oracle detail.
- **Web**: confirmation modals for destructive actions, cost panel +
  per-account pricing drawer, connect page rewrite, OpenAI preset,
  docs refresh to shipped state.

## v0.0.4 — 2026-10-09 · 15 commits (8c832ef..fb2a2fd)

First hardening pass and preset expansion.

- **Fixes**: client `Origin` stripped before forwarding (Anthropic CORS 401),
  dashboard session cleared only on own auth 401, routed account id persisted,
  duplicate token names rejected, combo config size advertised.
- **Router**: dead combo endpoints cool down; rules refusal returns 429, not
  502.
- **healthz reduced to liveness only** (`{"status":"ok"}`) — detail moved
  behind `/admin/*`.
- **Provider**: `gemini-openai` kind with scoped 400 auth rejection; key shape
  checks dropped — the probe chain is the sole validator.
- **Presets**: Command Code, DeepSeek, TokenRouter, Gemini, Groq.
- **Web**: 18-item dashboard UI pass.
- **Docs**: housekeeping to shipped state (M1–M7), API.md extended, CHANGELOG added.

## v0.0.3 — 2026-10-08 · 21 commits (0056710..e60b7fd)

Containerization, chat, and retry semantics.

- **Retry**: combo hop retried with exponential backoff before failover;
  global policy (3 retries/hop, transient-only) with config knobs.
- **Config**: credentials become SQLite-only; `EZLLM_*` env overrides restored
  for containers (deployment bug fix).
- **Budget**: compression budget stage with own context-token accounting
  (schema v5).
- **Web**: chat page behind the real routing path, Token Economics +
  budget-engine controls, add-provider dialog with key validation on save.
- **Deploy**: static distroless Docker image + home-box Traefik overlay;
  explicit router priorities + dedicated `/admin/login` limiter; MIT license.
- **Combos**: advertised in `/v1/models`, `enabled` honored in PATCH,
  per-combo enable toggle.
- **Presets**: Claude Platform; anthropic-compatible `/v1` join fix.

## v0.0.2 — 2026-10-07 · 18 commits (c10d422..18abe07)

Rules, compression, and the dashboard surfaces (M5–M7).

- **Rules**: eligibility engine — token caps + allowed-use windows, dashboard
  editor with live open/closed chips.
- **Compression**: M5 pipeline (engines, profiles, CRUD, inference wiring),
  headroom + lite engines, caveman (lossy prose condensation gated by
  construction), `rules_fired` attribution end to end, surface stamp on the feed.
- **Ledger**: `GET /admin/requests` filters by tokens in/out; Requests page.
- **Surfaces**: stats explorer (granularity × range), live recent-requests
  feed (SSE over fetch).
- **Auth**: dashboard password + plaintext keys stored in SQLite.
- **Fix**: combo refused by rules on every hop returns 429, not 502.
- **Test**: dashboard auth + plaintext schema coverage; Playwright render
  drivers.

## v0.0.1 — 2026-10-06 · 10 commits (d0247f8..fcabd35)

Initial release: the skeleton up to a working routed gateway.

- **M1**: skeleton + `/healthz`.
- **M2**: SQLite store, field crypto (AES-GCM, test-covered, unwired in v1),
  three-surface passthrough (chat / messages / responses).
- **Store**: admin persistence — accounts, provider keys, combos, client tokens.
- **Provider**: live model-catalog probe + adapter protocol surfaces.
- **Registration**: provider key testing with capped protocol sweep.
- **Proxy**: attempt/commit split — combo failover that never leaks a failed hop.
- **Router**: combo name → ordered candidate hops; strategies.
- **Admin**: admin API, embedded dashboard, combo inference wiring.
- **Docs**: API reference, key-test design record.
