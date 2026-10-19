# VERSION

Version history for ezllm. One version per change date — all patches committed
on the same date share one version. The patch component increments by one per
date (`v0.0.1`, `v0.0.2`, …).

The current version is served by `GET /healthz` (`version` field), shown in the
dashboard health strip, and asserted against this file by
`internal/server/version_test.go` — bump `internal/server/version.go` and this
file in the same commit, or the test fails.

**Current: v0.0.17**

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
