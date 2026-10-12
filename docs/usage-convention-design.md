# Usage convention design — one meaning for `tokens_in`

Sign-off doc for Phase 1 item 1 of the Claude Code breakage. Status: **D1 shipped** (schema
v13: the per-account fact, its validation, PATCH semantics and the read projections — see
"Implementation status" below). **D2 (ledger `NormalizeUsageShape`), D4 (verification probe)
and D5 (historical backfill) remain open** and are deliberately NOT in that patch.

## The problem

`tokens_in` means different things depending on the surface the request arrived on:

| surface | what ezllm stores | result |
|---|---|---|
| openai / responses | `prompt_tokens - cached_tokens` | **cold input only** |
| anthropic | `input_tokens` (raw) | whatever the vendor put there |

For the real Anthropic API that is correct — Anthropic EXCLUDES cache from `input_tokens`.
For a **vendor synthesizing an anthropic-shaped envelope** it is wrong, because they pass
their own total through.

Live consequence (2026-10-11): Claude Code's `IN` column climbed 46,768 → 76,956 over nine
turns and looked like runaway cost, while the cached-truth was 4,764 new tokens. Same
provider, same model, openai surface: the column *shrank* (886 / 7,756 / 3,728). Two trends,
one field, one bug. It also double-bills cached tokens in the Economics cost math.

## Evidence (controlled, not inferred)

Fixed 9,651-token prompt sent twice on `/v1/messages` to `tokenrouter/deepseek/deepseek-v4.1-flash`:

| call | marker | `input_tokens` | `cache_read_input_tokens` | `cache_creation` |
|---|---|---|---|---|
| A1 | none | 9,651 | 0 | 0 |
| A2 | none | **9,651** | **9,600** | 0 |
| B1 | `cache_control: ephemeral` | 9,658 | 9,600 | 0 |
| B2 | `cache_control: ephemeral` | 9,658 | 9,600 | 0 |

Conclusions, in order of importance:

1. **Automatic prefix caching is what works.** 9,600 tokens read from cache with *no* marker.
2. **`cache_control` markers are ignored and cost nothing** — `cache_creation` stayed 0 with a
   marker present. A translator may drop markers on this path. (B also proves prefix matching:
   a 7-token appendable change still hit the same 9,600-token prefix.)
3. **tokenrouter's anthropic `input_tokens` is INCLUSIVE of the cached part.** 9,651 reported
   *and* 9,600 cached, from a prompt that was ~9,651 tokens. Tell-tale key: `claude_cache_creation_1_h_tokens`
   / `_5_m_tokens` — real Anthropic nests under `cache_creation`.

**Kind is NOT a safe default.** Same probe family on `alibaba-token-plan`: `input_tokens:252`,
`cache_read_input_tokens:1,792` (total 2,044 = the openai-surface total). Alibaba is EXCLUSIVE,
tokenrouter is INCLUSIVE, and both are `openai-compatible`. So the convention is a per-account
fact that must be recorded, never derived from the adapter kind.

## Decision

**D1. Add `accounts.usage_convention TEXT NOT NULL DEFAULT 'auto'`.** Values:

- `'auto'` — EXCLUSIVE (today's behaviour). Safe default: no silent regression.
- `'anthropic'` — EXCLUSIVE, stated explicitly (real Anthropic accounts).
- `'total'` — INCLUSIVE: subtract the cached part on the anthropic surface.

**D2. The truth lives in `store.NormalizeUsage`, not in the proxy.** New
`NormalizeUsageShape(surface, raw, usageConvention)`; the two tap helpers and the
`UsageTapper` carry the account's convention so no call site can silently default it back.

**D3. An arithmetic guard overrides a wrong setting.** An inclusive envelope can never report
`input_tokens < cache_read_input_tokens` (a total cannot be smaller than its cached part).
When that combination appears, normalize as EXCLUSIVE regardless of the stored flag. This is
what makes a mis-set flag under-report instead of producing a nonsense negative, and it is
why the setting can be trusted to be one of exactly two behaviours.

**D4. Per-account verification, not a probe sweep.** A "verify usage convention" action sends
the same fixed-size prompt twice and sets the column from what comes back. Two calls of ~10k
prompt tokens per account, operator-triggered, recorded with the verdict and timestamp. It is
NOT added to `ProtocolSweep` — that sweep aborts on auth error and already spends per model;
this is a per-account, one-time, explicitly-invoked measurement. Token spend requires the
operator's approval (see skill: no token-spending probes without it).

**D5. Existing rows are corrected by an explicit backfill, never by a migration.**
`UPDATE calls SET tokens_in = MAX(tokens_in - tokens_cached_read, 0) WHERE surface='anthropic'
AND account_id=? AND tokens_cached_read > 0 AND ...` on a **backup**, one account at a time,
after the run is confirmed. A migration must not rewrite ledger history silently; the ledger is
the audit trail.

## Trace path (required for a persisted field)

Fresh schema **and** forward migration (`schema.sql`, `schemaVersion` bump, guarded
`ALTER TABLE ... ADD COLUMN` in BOTH migrate branches, `ALTER` before `schema.sql` re-applies);
`provider.Account` + the store's `Account` read path (select order and NULL handling);
`NormalizeUsageShape` + every tap call site; `api.ts` account type + the account PATCH decoder
(absent keeps / `null` clears / a value validates — `json.RawMessage` for this field); the
Providers page representation; `/admin/accounts/{id}` responses; a regression test that inserts a
row and reads it back through `RecentCalls`; docs. A fresh-database test does NOT prove the
upgrade migration — rehearse it on a COPY of the live sqlite (online backup API, not `cp`).

## Deployment

Ships as its own commit after items 2–3, because it needs the migration rehearsal above and a
prod redeploy to take effect. Items 2–3 are process-local and need neither.

## Implementation status (2026-10-13, schema v13)

- **D1 landed** as `accounts.usage_convention TEXT NOT NULL DEFAULT 'auto'` with values
  exactly as decided. Fresh schema + guarded forward migration in both migrate branches;
  `provider.Account` and both read paths carry it; `GET|PATCH /admin/accounts/{id}` expose it;
  the catalog/registration upsert deliberately does not touch it (refresh survival).
  Absent PATCH keeps, `auto` is the documented reset, an invalid value is `422` naming the
  field and nothing is written. Rehearsed on an online-backup copy of the live DB:
  v11 → v13 with every table count preserved and all rows at `auto`; the copy was deleted.
- **Meaning of `auto`** is "unstated", which is why it is both today's ledger behaviour
  (D1) and a *refusal* for cross-wire conversions onto a Messages upstream (the wire-matrix
  prerequisite: an unstated convention must never be guessed). `anthropic`/`total` are the
  only values a cross-wire Messages destination accepts.
- **D3's arithmetic guard** exists in the codec layer (`translate.NormalizeMessagesUsage`):
  a declared `total` whose `input_tokens < cache_read_input_tokens` is read as exclusive
  instead of producing a negative cache split. The ledger side of D3 rides with D2.
- The universal wire matrix consumes the fact via `translate.UsageConventionOf` — the guard
  that refuses a Messages destination on an unstated account.

## Related

- `internal/store/ledger.go` `NormalizeUsage` (the fix site)
- `docs/API.md` token-accounting section (states the two conventions — extend with D3)
- `references/token-economics-and-canary.md`, `references/implementation-pitfalls.md` (NULL-into-string)

## Appendix — translated rows are the documented inclusive exception (schema v12)

The cold-vs-inclusive rule above is about the WIRE the usage came from. A
wire-translated hop (anthropic client -> openai-only hop) breaks it on purpose:
`translate.Response` / the SSE synthesizer build an anthropic usage envelope out
of OpenAI's `prompt_tokens`, which counts the cached prefix, so the tap reads an
INCLUSIVE number on a row whose ledger surface is `anthropic`. `calls.translated`
(schema v12) is the marker that makes it countable, and it carries the
convention with it: for such a row `tokens_in` is inclusive and
`tokens_cached_read` is 0. Pinned by `TestTranslatedUsageStaysInclusive`; the
usage meter sums `tokens_in`, so translated traffic counts its cached prefix
against caps (conservative on the free/prepaid pool). Normalizing translated
rows into the cold convention later means teaching the translator to carry
`prompt_tokens_details.cached_tokens` — a translator-contract change, not a
ledger one.
