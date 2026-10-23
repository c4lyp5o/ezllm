# Claude Web provider — design (M14)

Status: DESIGN LOCKED 2026-10-16. Primary source: omniroute's production
implementation (`claude-web.ts` executor + `claudeTlsClient.ts`, extracted from
the omniroute image 2026-10-16). Cross-reference: the community digest
`~/workspace/claude-ai-re-client-protocol-digest.md` (repo Adithyan-Defender/
claude-ai-re-client — **stale, last commit 2026-04-25, single dev, no tests**;
used only for corroboration, never as the source of truth).

## Why omniroute's implementation is the source

- Active maintenance inside a production gateway vs an unmaintained 3-commit
  research repo whose protocol notes date to April 2026.
- omniroute's version carries strictly more: TLS impersonation (the reverse
  repo needs curl_cffi; omniroute solved it server-side), tools
  (`{name, description, input_schema}` — Anthropic-native shape), effort and
  thinking_mode, conversation state with uuid turn markers, Cloudflare
  challenge detection with browser fallback, retry-after forwarding.
- Both agree on the core wire: `sessionKey` cookie, `GET /api/organizations`
  for org id, conversation create + `/completion` SSE, `[DONE]`/`message_stop`
  termination, failure signatures (401 expired / 403 challenge / 429 rate).

## DECISIONS (locked — no open questions)

- **Provider kind `claude-web`**, model prefix `claude-web/<model>`.
- **Credential = provider_keys row**: the full `sessionKey=<value>` cookie
  (plus optional `anthropic-device-id`). Cookie material is live account
  access — 0600 storage only, never YAML, never logged.
- **Transport = TLS-fingerprinted HTTPS** (port of omniroute's wreq-js setup,
  profile `chrome_146`, OS linux): Go implementation via uTLS
  (`refraction-networking/utls`, Chrome Hello) behind a small
  `tlsclient` interface so the profile is swappable when Chrome moves. Plain
  net/http is NOT acceptable — claude.ai's Cloudflare distinguishes the Go
  JA3 fingerprint. Documented as the one external dependency; if uTLS proves
  insufficient in live proof, the fallback is shelling out to a Chrome
  fingerprint binary (decision deferred to evidence, not vibes).
- **Org discovery**: `GET https://claude.ai/api/organizations` with the full
  browser header set; take `[0].uuid ?? [0].id`. Failure map (exact):
  - 401 → `errSessionExpired` (typed)
  - 403 + Cloudflare challenge markers → `errCloudflareChallenge` (typed)
  - 403 otherwise → `errForbidden`
  - other non-200 → `errUnavailable`
- **Conversation**: one upstream conversation per bridge request
  (create-on-first-turn via `create_conversation_params`). The prompt is the
  caller's whole flattened history, so chaining requests into one
  conversation added nothing and leaked one caller's turns into another's
  context (2026-10-23). The client still chains conversation id + parent
  message uuids for follow-ups within a conversation.
- **Routing**: `POST /api/organizations/{org}/chat_conversations/{conv}/completion`,
  SSE.
- **Payload** (exact ports): prompt = OpenAI messages flattened to text (user
  and tool roles joined with `\n\n`; system prompt prepended) — a documented
  loss vs the official API, matching both implementations; model id passed
  through; timezone = server-local IANA (default UTC); locale `en-US`;
  `personalized_styles` = the single Default block verbatim; tools mapped to
  `{name, description, input_schema}`; effort/thinking_mode map:
  `claude-opus-5` → `{effort:"high", thinking_mode:"auto"}`; explicit
  thinking requested → `{effort:<req>, thinking_mode:"extended"}`; else
  `{effort:"low", thinking_mode:"off"}`; `turn_message_uuids`
  (human/assistant fresh uuid4 per turn, parent chained after the first);
  `attachments: []`, `files: []`, `sync_sources: []`;
  `rendering_mode: "messages"`; new-conversation turns carry
  `create_conversation_params` verbatim.
- **Headers** (exact, from omniroute's `getBrowserHeaders` + fingerprint
  config): Accept `text/event-stream`, Accept-Encoding gzip/deflate/br/zstd,
  Accept-Language `en-US,en;q=0.9`, Cache-Control no-cache,
  Content-Type application/json, Origin https://claude.ai, Pragma no-cache,
  Priority `u=1, i`, Referer `https://claude.ai/new`, Sec-Ch-Ua +
  Sec-Ch-Ua-Mobile `?0` + Sec-Ch-Ua-Platform + Sec-Fetch-Dest empty +
  Sec-Fetch-Mode cors + Sec-Fetch-Site same-origin, User-Agent = fingerprint
  UA, `anthropic-client-platform: web_claude_ai`, optional
  `anthropic-device-id`.
- **Streaming**: standard SSE `data: {...}` frames; text deltas from
  `content_block_delta`/`text_delta`; turn ends at `message_stop`, stream at
  `[DONE]`; parser is strict — state commits (assistant uuid becomes next
  turn's parent) only after `message_stop` is actually observed; mid-stream
  failure invalidates the turn (never commits a phantom parent uuid).
- **Key test (zero-quota oracle)**: `GET /api/organizations` — 200 = live
  session; the same call doubles as org discovery. No message is spent.
- **Scope fences (typed refusals above the package)**: no image input
  (attachments/files always empty), no artifacts/downloads, no model
  discovery (ids passed through; forward-compat by construction). Model
  switching mid-conversation starts a new conversation (web limitation).
- **Risks (accepted per Calypso 2026-10-16)**: Cloudflare challenge possible
  (typed error + operator-visible message; browser fallback is omniroute's
  answer, ours is a clear `errCloudflareChallenge` + retry guidance), session
  expiry (typed error, operator re-exports the cookie), rate limits
  (429 → forward Retry-After after validating it), account ban risk on
  automation, protocol drift (both sources' pin dates recorded in the
  runbook).
- **Bridge admission is a process secret, not the account header** —
  the bridge routes share ezllm's public listener, so an account id alone
  is never enough: each request must carry the process-local
  `X-Ezllm-Bridge-Secret` (random, generated at start, never persisted),
  which only the adapter stamps and the bridge checks in constant time.
  Without it a caller who could reach the port drove the account's STORED
  cookie with no ezllm token (2026-10-19). The secret is stripped from every
  upstream request.
  The cached session client is reused only for the cookie it was built
  from; a replaced key gets a fresh session.

## Files

- `internal/claudeweb/tlsclient.go` — uTLS Chrome-146 transport (interface +
  default impl; timeouts: 60s request + 10s grace; first-byte unbounded like
  omniroute's config).
- `internal/claudeweb/client.go` — cookie normalization, org discovery,
  conversation create/reuse/delete, completion POST, SSE parse → deltas.
- Wire into provider registry + key store (same pattern as gemini-web).
- Runbook `docs/claude-web-runbook.md`: cookie export procedure, org
  discovery semantics, challenge/expiry/rate failure cards, pin-date table
  (omniroute verified 2026-10; repo digest 2026-04-25).

## Acceptance

- TLS: transport dials the mock with the Chrome profile (fingerprint
  assertion on the ClientHello via the mock server).
- Org discovery: 200 → id; 401/403-challenge/403/500 → the four typed errors.
- SSE: multi-delta stream, `message_stop` commit semantics (parent-uuid
  chaining), mid-stream failure invalidates (bite-proven).
- Payload parity: golden JSON test vs the exact port (including
  create_conversation_params on first turn and the effort map).
- Live proof with a real account is operator-gated, same posture as
  gemini-web: no live call until Calypso says go.
