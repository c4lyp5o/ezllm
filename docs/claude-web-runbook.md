# claude-web provider — operator runbook (M14)

Claude.ai's **web** protocol as a provider (sessionKey cookie, SSE
completion). Ported from omniroute's production implementation; design:
`docs/claude-web-design.md`; the stale community digest
(`~/workspace/claude-ai-re-client-protocol-digest.md`, last verified
2026-04-25) is cross-reference only.

## Risk posture (accepted by Calypso, 2026-10-16)

- **Cookie material = live account access.** The stored `sessionKey` row is
  the account. Encrypted key store only; never chat/logs/YAML.
- **Automation risk.** Scripted use of the consumer web app can get the
  account flagged or locked. Use a test account.
- **Cloudflare.** claude.ai fingerprints TLS. The client impersonates
  Chrome's handshake (uTLS `chrome_146` profile, ported from omniroute).
  A challenge surfaces as a typed 403 (`cloudflare_challenge`) — the
  browser fallback omniroute uses is NOT implemented; retry guidance only.
- **No fallback.** Expired/challenged sessions fail honestly (401/403); no
  official-API degradation.

## Adding an account

1. Sign into claude.ai in a browser with the **test** account.
2. DevTools → Application → Cookies → `https://claude.ai`: copy the value of
   `sessionKey`.
3. Register the account with kind `claude-web` and base URL the sentinel
   `internal://claudeweb` (the router rewrites it to the in-process bridge).
4. Store the cookie as the account key: `sessionKey=sk-ant-…` (the bare
   token is also accepted).
5. The registration key test runs org discovery (`GET /api/organizations`
   — one request, zero quota) through the bridge: 200 = live cookie.
6. Models: this protocol has **no discovery**. The router seeds a bootstrap
   catalog from the ids the reference implementation pins (`claude-opus-5`,
   `claude-sonnet-4-6`) plus hyphenated-convention siblings — a starting
   point, NOT entitlement. The bridge passes ANY model id through; the
   upstream answer reveals what the account can serve. If you need an id
   that isn't in the seed, it still routes — the first error is the answer.

## How turns work

- **One conversation per account-client.** The upstream state machine
  chains `conversation_id` + `parent_message_uuid` inside it. A cut stream
  (no `message_stop`) commits NOTHING — no phantom parent state.
- History is flattened into one prompt (user/tool turns joined with blank
  lines) — the web protocol has no message array. Fine for chat, lossy for
  agent scaffolding.
- **Tools pass through natively** (Anthropic shape `{name, description,
  input_schema}`) — this surface DOES carry tool use.
- **No structured output** (`response_format` gets a typed 400
  `unsupported_feature`) and **no image input**.

## Failure cards

| Symptom | Meaning | Action |
|---|---|---|
| 401 authentication_error | Cookie expired / signed out | Re-export sessionKey, replace the key row |
| 403 cloudflare_challenge | TLS fingerprint or bot gate rejected | Retry; the profile version may need a bump (`chromeMajor` in tlsclient.go) |
| 403 authentication_error (plain) | Account forbidden (not a challenge) | Check the account state in the browser |
| 429 | Rate limited | Wait; lower concurrency |
| 502 stream ended before message_stop | Upstream cut mid-turn | Nothing committed; retry the turn |

## Version pins worth remembering

- TLS profile: `chrome_146` (constant in `internal/claudeweb/tlsclient.go`)
  — bump when Cloudflare starts rejecting the profile, same spirit as the
  Copilot CLI version floor.
- The prompt flattening + uuid-chaining + payload shapes are ported
  slot-for-slot from the compiled reference; the design doc lists every
  ported index.
