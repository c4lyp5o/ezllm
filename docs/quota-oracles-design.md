# Quota oracles — same-key balance reads for providers without `/v1/usage`

Status: IMPLEMENTED 2026-10-14 (rev 1, supersedes nothing).
Scope approved by Calypso: "same-key batch first (DeepSeek, OpenRouter, Vercel, Ollama)".

## Problem

`adapter.ReadQuota` only probes `GET {base}/usage`. 12 of 14 live accounts fall
through to `QuotaNone` ("measured" = our own ledger), because their balance lives
somewhere else — but always at an endpoint the SAME inference key can read.
Live-probed 2026-10-14 with the operator's own keys (zero-token GETs, UA
`ezllm/1.0 (llm-router)`):

| provider | endpoint | same key? | live evidence |
|---|---|---|---|
| DeepSeek | `GET {base}/user/balance` | yes | 200 `is_available:true`, USD 0.09 + CNY 9.17 |
| OpenRouter | `GET {base}/key` | yes | 200 `limit_remaining 999.98` / `limit 1000` |
| Vercel AI Gateway | `GET {base}/credits` | yes | 200 `balance "4.30"`, `total_used "0.70"` (strings!) |
| Ollama Cloud | `GET https://ollama.com/api/usage?range=24h` | yes | 200 `totals.request_count 11` (origin-rooted, NOT under `/v1`) |

Non-batch findings (recorded, not built):
- **Command Code** — usage page `commandcode.ai/inqm7y1/settings/usage` is a
  sign-in SPA; `/api/*` paths answer app-level 404 with a browser UA, provider
  API has no usage route. **Cookie-gated class → phase 2** (opt-in credential). IMPLEMENTED 2026-10-20: the
  page's XHR is `GET api.commandcode.ai/internal/billing/credits`, authenticated by the
  `__Secure-commandcode_prod_.session_token` cookie alone (live-verified; `session_data` not
  needed). Cookie stored in `management_key`; parser `parseCommandCodeCredits` (percent shape).
- **TokenRouter** — balance needs the separate Management key
  (`/api/management/self/wallet`, live-verified `$3.69` top-up left). Phase 2.
- **Cloudflare billable-usage** — official API but the live token 403s it
  (needs Billing Read permission). Permission, not shape.
- **MiMo / Alibaba** — console cookie class, phase 2.
- **Anthropic / Groq / NVIDIA NIM** — no same-key endpoint exists (NIM retired
  credits Sep 2025; Anthropic standard key has none). Header-harvesting is a
  separate, supplement-only design.

## DECISIONS (locked)

1. **Dispatch shape** — one host-scoped table `quotaOracleFor(acct)` in
   `internal/provider/quota.go`, called at the TOP of
   `openAICompatible.ReadQuota`. Returns absolute URL + parser. `ok=false` →
   the existing generic `{base}/usage` path runs unchanged. Interface
   unchanged (no new adapter methods). Host scoping matches `isCloudflareAI`:
   `Kind == openai-compatible` AND `https` AND exact hostname. A spoofed
   account *name*/*namespace* or a user-supplied path can never trigger an
   oracle.
2. **URL construction** — DeepSeek/OpenRouter/Vercel: `base + "/path"` (their
   bases are stable: no `/v1`, `/api/v1`, `/v1` respectively). Ollama: derive
   `scheme://host` from the base and append `/api/usage?range=24h` — the
   endpoint is origin-rooted; appending to a `/v1` base would produce a wrong
   path.
3. **Parsers are per-shape, never generalized** — `parseDeepSeekBalance`
   (multi-currency `balance_infos[]`, **prefer USD**, else first entry; unit =
   that entry's currency), `parseOpenRouterKey` (`data.limit_remaining` →
   balance USD; `data.usage` → `cost_total`; `data.usage_daily` → `cost_today`;
   `data.limit_reset` → `resets_at`; nil-safe for unlimited keys),
   `parseVercelCredits` (**string numbers** — `jsonFloat` rejects strings, so
   a `parseFloatString` helper is required), `parseOllamaUsage`
   (`kind=tokens`, `requests_today = totals.request_count` for the 24h range).
   Every parser returns a zero `Quota{}` on shape mismatch → caller maps to
   `ErrQuotaUnsupported` (never a hard error).
4. **404/405 on an oracle ⇒ `ErrQuotaUnsupported`**, not a 502: a retired
   endpoint is "provider has no quota endpoint", the documented contract
   (`docs/API.md`). Other HTTP errors (401/429/5xx) propagate unchanged so a
   dead credential never overwrites a good snapshot with `kind=none`.
5. **UA policy (Calypso's explicit rule)** — ezllm's OWN requests
   (`getJSONStatus`: quota, catalog, Cloudflare search) always carry
   `agentUserAgent`. `PrepareRequest` (openai-compatible, anthropic-compatible)
   sets it **only when absent**: probes/quota/catalog have no client UA → ours;
   inference keeps the client's UA (existing `header_strip_test` contract),
   falling back to ours instead of Go's default. Operator `custom_headers`
   still wins (applied after). opencode-go's unconditional set is unchanged.
   Honest evidence (measured 2026-10-14, real Go program against live keys):
   the 403/1010 was **python-urllib** (my earlier probe), while
   `Go-http-client/1.1` answered **200** on both Groq and Command Code —
   consistent with probe N9's "Go was never affected". So this rule is
   operator-mandated hygiene + honest self-identification, **not** a live fix.
6. **`tokens_today` / `requests_today` get wired end-to-end** — the columns
   already exist in the live schema (verified `PRAGMA table_info`), but
   `InsertQuotaSnapshot` never wrote them and no projection read them (the
   ssn-gpt parser already fills them — they were silently dropped). Wired:
   INSERT → `LatestQuotas` → `hydrateAccount`/`QuotaSummary` → `api.ts`
   → `QuotaBars`. **No schema bump, no migration.**
7. **UI** — `QuotaBars` gains a counts branch AFTER percent/money: plain
   label+value lines, **no bar** (there is no ceiling to render a percentage
   against; a bar would be a fabricated limit). Percent bars and money balance
   render exactly as before. The quota button gate (`kind !== 'none'`) already
   admits `tokens`/`money` with no change.
8. **Not in this batch**: cookie-gated consoles (MiMo, Alibaba, Command Code),
   TokenRouter's management key, Cloudflare billable-usage (permission),
   response-header harvesting. Each needs either a second credential field or
   an opt-in consent flow — separate design.

## Test plan (all fixtures are verbatim live bodies)

- `quota_test.go`: oracle table host matrix (right URL per host; namespace
  spoof, http scheme, wrong kind, wrong host → `ok=false`); four parser tests
  against captured live JSON incl. multi-currency USD preference, string
  numbers, unlimited-key nil balance, garbage → zero `Quota{}`.
- `adapters_test.go`: `getJSONStatus` sends `agentUserAgent`; UA
  present-in-hdrs wins; `PrepareRequest` set-if-absent for both compat kinds;
  `ReadQuota` oracle path 404 → `ErrQuotaUnsupported`.
- `store`: snapshot round-trip asserting `tokens_today`/`requests_today`
  survive insert → `LatestQuotas` → `hydrateAccount`.
- `registration`: `quotaDetail` tokens case.
- Bite proofs: drop the balance assignment → parser test red; drop the INSERT
  column → store round-trip red; drop the UA line → UA test red.
- Live canary: copied-DB rig on a scratch port, `GET
  /admin/accounts/{12,13,14,9}/quota` → kinds + numbers must match the probe
  table above. Zero inference spend (all GET oracles).

## Addendum 2026-10-20 — session-credential oracles (OpenAI)

OpenAI's `GET api.openai.com/v1/dashboard/billing/credit_grants` answers `200` to a platform
session token (`sess-…`, copied from the dashboard's Authorization header) and `401` to an `sk-`
inference key; the `openai-organization` header is not required (live-verified). Stored in
`management_key`; without it the account stays "measured". `parseOpenAICreditGrants` maps
`total_available` → balance (can be negative) and `total_used` → cost_total; the older
`parseMoneyQuota` "classic" branch is NOT reused (it reports `total_granted` as the balance).
No expiry hint: the session token carries none.

## Addendum 2026-10-20 — Anthropic Console prepaid credits

`GET platform.claude.com/api/organizations/{org}/prepaid/credits` answers `200` to the
`sessionKey` cookie alone (no browser headers needed; live-verified). The org id lives in the
`lastActiveOrg` cookie, so the operator pastes the whole Cookie header into `management_key`;
`claudePlatformCreds` keeps those two cookies and refuses an org id that is not a bare UUID (it
is interpolated into a URL path). Scoped to kind `anthropic-compatible` + host `api.anthropic.com`
(other Anthropic-wire hosts never trigger it). `parseClaudePrepaidCredits`: balance =
`balance.credits` (else `.money`) `amount_minor / 10^exponent` — the raw `amount` is cents —
and cost_total = Σ tranche `granted` − balance. No expiry hint (the cookie carries none).
