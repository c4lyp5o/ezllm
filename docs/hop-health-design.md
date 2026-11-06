# Hop health from stream outcomes — design

Status: **implementing** (branch `melissa/hop-truncation-health`, off development
7e05aad). Scope: measurement + operator visibility of streams that FINISH with a
status but do not deliver a whole answer. Routing action (parking a hop on a
rolling truncation rate) is deliberately a separate, knob-gated commit.

## Why

Measured on the live ledger, 8 days to 2026-11-05: 18 genuine mid-stream
`connection reset by peer` events, **16 of them on one hop** —
`command-code · xiaomi/mimo-v2.6-pro`, 16/771 = **2.1% of everything routed
there**, while the SAME model on `opencode-go` was 0/754. Each one is a 200
header plus a half answer, after a 6–9 s TTFT and a 250–600 KB prompt, so the
client eats a truncated reply and re-sends the whole prompt.

Nothing in the product shows that. The hop looks healthy: it commits, it returns
200, it writes a ledger row. The only trace was prose in `err`
(`stream truncated: …`), which cannot be told apart from the far more common
*client hung up after the last byte* artefact (442 such rows in the same window,
all complete calls) without splitting on `tokens_out` — a reading-comprehension
exercise that has already produced one wrong report.

## Locked DECISIONS

1. **`calls.stream_outcome` TEXT NOT NULL DEFAULT '' is the record.** Values:
   `''` (not a stream), `completed`, `truncated`, `client_gone`, `failed`.
   Written once in `finishDispatch` from `clientGone`, `res.StreamCompleted()`
   and `res.StreamOutcome`. `err` stays human-readable text and is NEVER parsed
   for this classification again.
2. **`client_gone` is a distinct value, not a failure.** A caller that hangs up
   after the final usage chunk is a complete call; counting it as truncation is
   the mislabel that hid the real 18.
3. **The rollup excludes `client_gone` from the denominator and numerator.**
   `truncation_rate = truncated / (streams − client_gone)`.
4. **`/admin/health` gains `hop_stream_health`** — per `(namespace, model)` over
   a window (`?stream_window=`, default 24h, max 30d): streams, completed,
   truncated, failed, client_gone, truncation_rate. Rows below 1 stream are
   omitted; the window is inclusive of its start.
5. **No routing change in this slice.** The rate-based park needs a measured
   baseline first (the M2 lesson: a hot-path rewrite justified by an unmeasured
   number deleted a whole client's tool catalog). It lands next as its own
   commit with one self-sufficient knob.
6. **Historical rows keep `''`** — the column is not backfilled from prose.
   A dashboard that wants history re-derives it from `err` with the
   `tokens_out` split, and says so.
7. **Same schema/migration discipline as every ledger column**: fresh schema +
   idempotent ALTER in BOTH migrate branches, applied BEFORE `schema.sql`
   re-applies, `schemaVersion` 33 → 34, tests on a fresh DB AND one stamped at
   33.

## Threading (the ledger-column recipe)

`schema.sql` → `store.schemaVersion` → `alterCallsStreamOutcome` (both migrate
branches) → `store.Call` → prepared INSERT (`?` + arg) → `RecentCalls`
SELECT + `Scan` → `server.callsCols`/`scanCalls` if the requests explorer
projects it → `store/events.go` `CallEvent` + `EventFromCall` (the SSE live feed
is a second projection; a field missing there is silently dropped by the TS
normalizer) → `web/src/utils/api.ts` type + normalizer → the page.

## Proof plan

- store: insert → `RecentCalls` round-trip carries the value (a placeholder/arg
  mismatch fails immediately).
- migration: a database stamped at 33 gains the column with every row `''`.
- server: a truncated stream records `truncated`; a caller that cancels records
  `client_gone`; a clean stream records `completed`; a non-stream request
  records `''`.
- health: the rollup's rate ignores `client_gone` and the window boundary.
- bite: delete the producer assignment, the dispatch test must go red.
