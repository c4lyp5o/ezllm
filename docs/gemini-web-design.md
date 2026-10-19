# gemini-web Provider — Design (M12)

Status: implementation spec, DECISIONS locked below.
Reference: protocol digest `~/workspace/gemini-web-api-protocol-digest.md`
(compiled from HanaokaYuzu/Gemini-API, repo master Oct 2026, all facts
file/line-cited there). Risk posture chosen by the operator: per-account
opt-in on add, cookie rotator, test-account guidance, honest error
surfacing, no fallback to the official API.

## What this is

A new provider kind `gemini-web` that speaks Google Gemini's WEB protocol
(`gemini.google.com`, reverse-engineered). Personal Google accounts AND
Workspace/Enterprise accounts are the same protocol; the RPC that lists
models returns a different catalog + tier per account, so "enterprise
support" is not a second code path — it is the same path with an
enterprise-account cookie.

## Credentials (the "key")

- The key row holds the cookie pair, one per line, in this exact format:
  `__Secure-1PSID=<value>` then `__Secure-1PSIDTS=<value>`. `PSIDTS` may be
  absent (degraded rotation window); the initial scrape still works.
- Cookie material is live account access. It lives only in `provider_keys`
  (0600 file, never in config.yaml, never in logs — same rules as API
  keys; see skill "Secrets NEVER in config YAML").
- Rotation: `POST https://accounts.google.com/RotateCookies` every 600 s
  ± 15 s jitter, per account, from one background goroutine per account
  holding live sessions. On success the stored key is rewritten with the
  rotated cookies; on failure the session is marked stale and the next
  init re-scrapes. Two goroutines must never rotate the same account —
  the session registry owns the lock, keyed by account id.

## Session init (per account, lazily on first use)

1. GET `https://gemini.google.com/app` with the cookies; scrape
   `SNlM0e`, `cfb2h` (bl), `FdrFJe` (f.sid), `TuX5cc` (hl) from the page.
2. `GET_USER_STATUS` RPC via `batchexecute` (rpcid `otAQ7b`, payload `[]`)
   → status code (1000 = OK) + model descriptors (field 15) + tier fields.
3. Failure signatures map to named causes (digest §7.2), surfaced to the
   operator verbatim in the key-test step and to the client via the
   existing error class vocabulary where one fits:
   - no `SNlM0e` / non-1000 status → `bad_credentials` (cookies expired)
   - 1037 usage-limit / 1060 temporarily blocked → `rate_limited`
   - 1040/1042 TOS, 1054/1057 guardian, 1060 location → mapped causes
   - CAPTCHA / recaptcha seen during init → distinct, explicit message
   - (never retried on 401-adjacent codes — Cooldown semantics unchanged)

## Key test & registration

- The ORACLE is `GET_USER_STATUS`: zero generation quota (real money ≠
  other providers' generation probes), returns auth verdict + tier.
  Therefore registration's probe draft = init + one batchexecute POST.
- Registration's 7-step flow unchanged; the adapter declares
  `HostedAuthOracle`-equivalent behavior through its own probe path
  (design mirrors the Cloudflare absolute-URL pattern; the endpoint is
  fixed here, so it is simply `AuthProbe` semantics with a batchexecute
  body and our cookie jar).

## Catalog sync

- Model list = `GET_USER_STATUS` field 15 (model_id hex, display name,
  capacity, tier flags). Upserted like every other `ListModels`.
- Model ids are exposed to clients as `gemini-web/<display>` (namespace
  chosen at account creation, like every kind).
- `proto_openai=1` / `proto_anthropic` capability flags are POSTURE, not
  facts: this hop is reachable from openai/anthropic clients through the
  normal translation legs, so we mark the flags per probe (a tiny real
  generate) and let the usual gates decide.

## Request path

- openai/anthropic/responses client → hop dial wires `gemini-web`:
  protocol request = form-encoded `at` + `f.req` (double-encoded inner
  list, streaming flag at index 7, model header
  `x-goog-ext-525001261-jspb` built from the model's registry entry —
  never hardcoded).
- Streaming response = wrb.fr length-prefixed frames (UTF-16-unit
  length prefix, JSPB JSON payload arrays). Our own incremental parser
  feeds the existing translate/SSE layer: per frame, take the candidate
  text and emit the APPENDED SUFFIX over the last emitted text (common
  prefix; on flicker/replace, emit the tail of the new text — never a
  full re-send as a new delta).
- Non-stream: dial with streaming on, accumulate the final text, then
  answer on the client's wire (the web protocol has no non-stream mode).
- System prompts: the web protocol has no inline system instruction; a
  Gem is the closest analogue. Documented loss: system prompts are
  prepended as the first user turn (best effort) — see Losses below.

## Capabilities & honest losses (baked into the code, not just docs)

- **No function calling** (no function_call parts in the web protocol).
  A gemini-web hop receiving a tools request must NOT pretend: the
  translate leg refuses with a typed error naming tool support, before
  any upstream call. (The natural home is the existing
  `chat_tools_unsupported`-adjacent refusal pattern; tool requests never
  get a silent wrong answer.)
- **No structured output** (no response_schema): same refusal pattern.
- Images-in / citations-out: text + inline image inputs pass through the
  upload path only if/when implemented — M12 scope is TEXT ONLY.
- No reasoning/thinking echo semantics: web `thoughts` are dropped
  (documented loss; no fabricated reasoning blocks).
- No usage metering from the web wire (no token counts): the ledger row
  records tokens as reported-by-none, `tokens_in/out` from our byte
  estimator at best — marked so the dashboard never shows provider-exact
  numbers it never received. Cost accounting notes the gap.
- Failover: transport/5xx/429/1060 → cooldown + next hop (existing
  semantics); a web-specific refusal (captcha, TOS, guardian) is
  hop-specific and fails over with the UPSTREAM's own words in the
  ledger err, like every other cause.

## Bounded resources

- Session registry is bounded by account count; each session holds one
  `http.Client` with IdleConnTimeout and MaxIdleConnsPerHost bounded.
- Rotator goroutines are per-account, started on first use, idempotent.
- No unbounded retry loops against the web endpoints: bounded retries
  inside the protocol package only (the 10-min rotator is a cadence, not
  a retry storm).
- Cookie rotation failure after N consecutive failures marks the account
  session dead (90s park) — the account is skipped gracefully, the
  operator sees the reason in health, and a fresh key test re-arms it.

## Tests (deterministic, no live Google calls)

- Mock `gemini.google.com` via httptest (loopback, per the mock-upstream
  convention): `/app` scrape fixture, `batchexecute` fixtures
  (status/model list/error codes), `StreamGenerate` wrb.fr fixture
  frames incl. UTF-16 astral chars (emoji = 2 units) and split frames.
- Frame-parser unit tests: whole frames, 8-byte fragments, astral
  payloads, garbage prefix, truncated tail (no panic, no hang).
- Delta emitter: append-only growth, flicker replacement, unicode.
- Rotator: mock `RotateCookies` rotation success/failure paths.
- One e2e-ish test: openai client → gemini-web hop → synthesized SSE
  (through the real path, ByteEqual against a golden anthropic/openai
  frame sequence, as the wire matrix does).

## Acceptance criteria

1. All gates green (gofmt/vet/build/`go test ./...`/race on touched).
2. Key test passes against a REAL Google account cookie pair the operator
   provides for a scratch instance (never on the live ledger) — the
   live proof is the gate for "credential-verifiable", per repo rule.
3. Catalog sync returns the account's real model list live.
4. One tiny real streamed call (16-token max) succeeded live on scratch.
5. Tool/structured requests refused with the typed error (bite-proven:
   remove the refusal → a tools-request test goes red).
6. Runbook `docs/gemini-web-runbook.md`: cookie export steps, rotation
   behavior, error-code table, account-risk posture, test-account
   guidance.

## Locked decisions

1. **Cookies, never OAuth, for M12** — this library's protocol is
   cookie-based; OAuth token minting for web endpoints is out of scope.
2. **`GET_USER_STATUS` is both key-test and catalog** — a zero-quota
   oracle beats a generation probe on cost AND latency; adopt it.
3. **Text-only in M12** — images/upload/deep-research/Gems are follow-up
   scope; the refusal pattern covers what's missing rather than faking.
4. **Tools/structured = typed refusal before dispatch**, never a silent
   upstream failure and never truncation.
5. **Personal and Workspace/enterprise are ONE code path** — tier
   differences are data from the status RPC, not branches.
6. **No dedicated kill flag** (operator's explicit choice): account
   enable/disable is already the switch; a `gemini-web` account that
   isn't added doesn't exist, and disabling it parks it exactly like any
   other kind.
7. **Errors carry the upstream's own words** through the existing
   `errclass` cause vocabulary; new causes only where the web protocol
   has genuinely new failure classes (captcha, TOS-pending, guardian).
8. **Bridge admission is a process secret, not the account header** —
   the bridge routes share ezllm's public listener, so an account id alone
   is never enough: each request must carry the process-local
   `X-Ezllm-Bridge-Secret` (random, generated at start, never persisted),
   which only the adapter stamps and the bridge checks in constant time.
   Without it a caller who could reach the port drove the account's STORED
   cookie with no ezllm token (2026-10-19). The secret is stripped from every
   upstream request.
