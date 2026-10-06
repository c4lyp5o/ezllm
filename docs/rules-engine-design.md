# ezllm — Rules Engine Design (M5)

Status: APPROVED by Calypso 2026-10-06. All five open questions answered (§9). Ready to implement.
Author: m3lis5a · 2026-10-06

## 1. What we're building

Two per-model rules, both answering the same question the resolver already asks
at `router.go:257` — **"may this hop be used RIGHT NOW?"** — via the existing
`Eligible func(ctx, accountID int64, modelID string) error` seam:

1. **Usage cap** — "how much of this model can we use?" (Calypso's rough idea)
2. **Allowed-hours window** — "what time may this model be used?"
   (the night-discount case; omniroute has no equivalent)

One combo then holds a day model + a night model and the router picks whichever
is legal at the current hour. No separate day/night combos.

## 2. Decisions already locked (Calypso, re-confirmed post-compaction)

| Decision | Answer | Consequence |
|---|---|---|
| Cap scope | **per (account + model)** | Counter key is exactly `(account_id, model_id)` — matches the existing `UNIQUE(account_id, model_id)` on `models` |
| `/v1/models` visibility | **always list everything** | Out-of-window/capped model is still advertised; calling it yields **429 with the rule + the window** in the body. Nothing vanishes mid-session. |
| Out-of-window in a combo | **skip to next hop** | Already the resolver's behavior (`continue` at the gate) — silent day/night switching |

## 3. Grounded findings from the code (these shape the design)

### 3.1 The seam is real but EMPTY
`grep -rn WithEligibility` → **zero call sites outside router.go itself**.
`cmd/ezllm/main.go` never wires a predicate, so `eligible()` currently always
passes. We define the first real implementation. No back-compat constraint.

### 3.2 `cap_tokens` / `cap_window` exist but are decorative
Schema has them on `accounts` (line 24-25) and `client_tokens` (line 132-133),
never read in the hot path. **Important:** those are *account-wide* and
*per-client-token* caps — NOT per-model. The user's feature is a third thing:
per-(account+model). Keep all three; they compose (see §7).

### 3.3 No server-side cache layer exists
`grep sync.RWMutex|refreshLoop|atomic.Value` in server+router → **nothing**.
Every resolve goes to SQLite (`ComboByName` is 2 indexed queries). So window
evaluation must be either (a) pure in-memory arithmetic over a cached rule set,
or (b) a cheap indexed lookup. **Per-request `SUM()` over `calls` is
forbidden** — that puts an aggregate on every inference.

### 3.4 The ledger write path is the only sane counter hook
`RecordCall` → channel → `ledgerLoop` batches → `insertCalls` (single writer,
FIFO, 20ms/4-row flush). This is where a usage counter can be incremented in
the **same transaction as the calls insert** — atomic, off the response path,
and it already has the full `Call` struct.

### 3.5 `Call` carries `Account string`, NOT an account ID
Denormalized *name*, "survives key/account deletion". But a name can be
re-pointed at a different ID (this exact class of bug broke boot:
`token_hash` UNIQUE). **So the counter must be keyed by ID, and the ID must be
carried on the Call** — adding `AccountID int64` to `Call` is required, not
optional. Route already has it (`Route.Account.ID`).

### 3.6 `calls` has no `account_id` column at all
Only the denormalized name. Adding the ID column to `calls` is a migration we
want anyway (usage reports group by name today; grouping by ID is more correct).

## 4. Schema

### 4.1 New table: `model_rules`
```sql
CREATE TABLE IF NOT EXISTS model_rules (
  id            INTEGER PRIMARY KEY,
  account_id    INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  model_id      TEXT    NOT NULL,
  -- usage cap
  cap_tokens    INTEGER NOT NULL DEFAULT 0,   -- 0 = unlimited
  cap_window    TEXT    NOT NULL DEFAULT 'monthly',  -- 5h|daily|weekly|monthly (matches store.ValidateAccount, admin.go:259)
  -- allowed-hours window (NULL/empty = always allowed)
  win_start     TEXT,   -- 'HH:MM' local to win_tz
  win_end       TEXT,   -- 'HH:MM'; end < start means it wraps midnight
  win_days      TEXT,   -- CSV of 0-6 (Sun=0), empty = every day
  win_tz        TEXT    NOT NULL DEFAULT 'Asia/Kuala_Lumpur',
  enabled       INTEGER NOT NULL DEFAULT 1,
  note          TEXT,
  UNIQUE(account_id, model_id)
);
```
`UNIQUE(account_id, model_id)` mirrors `models` — **one rule row per
(account, model)**. Multiple windows for one model = OR inside `win_days`, not
multiple rows (keeps the predicate a single lookup).

### 4.2 New table: `usage_counters` (the O(1) meter)
```sql
CREATE TABLE IF NOT EXISTS usage_counters (
  account_id  INTEGER NOT NULL,
  model_id    TEXT    NOT NULL,
  bucket      TEXT    NOT NULL,   -- window key, e.g. '2026-10-06' / '2026-W40' / '2026-10-06T14'
  tokens      INTEGER NOT NULL DEFAULT 0,
  calls       INTEGER NOT NULL DEFAULT 0,
  updated_at  TEXT    NOT NULL,
  PRIMARY KEY (account_id, model_id, bucket)
) WITHOUT ROWID;
```
- `WITHOUT ROWID` → the PK *is* the index; the increment is a single
  `INSERT ... ON CONFLICT DO UPDATE SET tokens = tokens + ?`. One row touch.
- **Bucket rotation is implicit**: a new window simply writes a new bucket key.
  No cron, no reset job, no "forgot to reset the counter" class of bug.
- Old buckets age out naturally; a periodic prune (or `DELETE WHERE bucket <`)
  is housekeeping, not correctness.
- Read for the gate = one PK lookup. That is the entire hot-path cost.

### 4.3 Migration on `calls`
```sql
ALTER TABLE calls ADD COLUMN account_id INTEGER;   -- NULL for historical rows
CREATE INDEX IF NOT EXISTS idx_calls_acct_model ON calls(account_id, model_id, ts);
```
Historical rows keep `account_id NULL`; the backfill is optional and can be
done by joining on the denormalized name (best-effort — names may have moved).
**Do not block the feature on backfilling.**

## 5. The predicate

```
eligible(ctx, accountID, modelID) error
  1. rule := rules.Get(accountID, modelID)      // cached, see §6
     if rule == nil || !rule.enabled → nil (allowed)
  2. WINDOW CHECK (pure arithmetic, no I/O):
     now := time.Now().In(rule.tz)
     if rule has a window and !inWindow(now, rule) →
        return &ErrRule{kind:"window", detail:"allowed 22:00-06:00 Asia/Kuala_Lumpur"}
  3. CAP CHECK (one PK lookup):
     if rule.cap_tokens > 0:
        used := counters.Get(accountID, modelID, bucketFor(now, rule.cap_window))
        if used >= rule.cap_tokens →
           return &ErrRule{kind:"cap", detail:"monthly cap 5,000,000 reached"}
  4. return nil
```

Order matters: **window before cap**. The window is free (no I/O); the cap
costs a lookup. Checking the cheap veto first is both faster and gives the
better error (a night-only model at noon should say "not until 22:00", not
"cap reached").

### 5.1 `inWindow` — the genuinely fiddly part
- **Overnight**: `win_end < win_start` (22:00→06:00) means
  `now >= start || now < end`.
- **Day attribution for an overnight window**: a request at 01:00 Tuesday
  belongs to *Monday's* window. Compute the window's **anchor day** as
  `now.Add(-start)` when wrapped, so the day filter matches the day the window
  *opened*, not the calendar day of the request. Getting this backwards makes
  "Mon-Fri nights" exclude Friday 01:00 — i.e. Saturday morning.
- **DST**: evaluate in the rule's `win_tz` via `time.LoadLocation`, never in
  UTC and never in the server's local zone. A 22:00-06:00 rule in
  `America/New_York` must flip at New York midnight, not Kuala Lumpur's.
  Malaysia has no DST (UTC+8 fixed), so *our* default tz is safe — but a vendor
  selling "US night discount" is not, hence per-rule tz.
- **`win_days` empty = every day.** Explicit list = only those days (0=Sun).

### 5.2 `bucketFor` — window → bucket key
| cap_window | bucket key | example |
|---|---|---|
| `5h` | rolling — see note | — |
| `daily` | `YYYY-MM-DD` | `2026-10-06` |
| `weekly` | `YYYY-Www` (ISO) | `2026-W41` |
| `monthly` | `YYYY-MM` | `2026-10` |

**Verified against the code**: `store.ValidateAccount` (`admin.go:259`) accepts
exactly `5h|daily|weekly|monthly` and rejects anything else. There is **no
`hourly`** — I had invented it in the first draft. `model_rules.cap_window`
must accept the same set so one validator covers both, and the `5h` question
below is therefore unavoidable: the enum already promises it.

**`5h` is DECIDED (§9.1): calendar-aligned blocks.** Bucket key is
`YYYY-MM-DDT<block>` where `block = hour / 5`, i.e. integer division of the
hour-of-day — **not** `H/5` cron syntax, which indexes differently. Blocks:
`0`=00:00-04:59, `1`=05:00-09:59, `2`=10:00-14:59, `3`=15:00-19:59,
`4`=20:00-23:59 (short block — the day has 24 hours, not 25; document this,
it is the one visibly uneven bucket). Example: 14:37 → hour 14 → `14/5 = 2` →
bucket `2026-10-06T2`. O(1) and reset-free — a new block simply writes a new
key. The block boundary is computed in the **rule's** `win_tz`, same as
windows, so a cap day does not straddle two timezones.
Accepted caveat: right after a block boundary the headroom is whatever is left
of the cap for that block, which can be small. Rolling-5h was rejected because
it needs either sub-buckets or a range scan over `calls` (forbidden here).

### 5.3 What counts toward the cap
`tokens_in + tokens_out`. **Not** cached_read (you didn't pay full price for
it), **not** reasoning separately (already inside out for most providers).
This must be stated because it's a policy choice, and it's the one number a
vendor's "you used X tokens" will disagree with. Make it a documented constant,
not a hidden sum.

## 6. Caching (the part that keeps this off the hot path)

New `internal/rules` package owning:
- **Rule set**: `atomic.Value` holding an immutable `map[ruleKey]Rule`,
  refreshed by a background goroutine on a ticker (30s) **and** invalidated
  immediately on any admin write to `model_rules`. Read = one atomic load +
  map hit. Zero I/O.
- **Counters**: read-through with a short TTL cache (1-2s) keyed by
  `(accountID, modelID, bucket)`. Rationale: the counter is written by the
  ledger loop asynchronously anyway (up to 20ms lag), so a 1s read cache
  changes nothing about correctness — it only removes a PK lookup from most
  requests. **Stale-by-1s on a monthly cap is irrelevant**; stale-by-1s on a
  5h cap near the boundary is the one case worth noting.
- **Increment**: happens in `insertCalls`, in the same tx, from the batch.
  Group the batch by `(account_id, model_id, bucket)` first so N calls collapse
  into ≤N counter upserts (usually far fewer).

This is the same "process-local, cheap, lose-on-restart-is-fine" philosophy as
`rrCounters`/`stickyLast` — except the counter itself is durable in SQLite,
only the *read cache* is ephemeral.

## 7. How the three cap layers compose

| Layer | Scope | Where | Status |
|---|---|---|---|
| `client_tokens.cap_tokens` | per API key (client) | not enforced | existing, decorative |
| `accounts.cap_tokens` | per account, all models | not enforced | existing, decorative |
| **`model_rules.cap_tokens`** | **per (account, model)** | **this design** | new |

Enforce **most-specific first**: model rule → account cap → client cap. All
three produce the same `ErrRule` shape so the 429 body is uniform. I'd implement
the model rule now and wire the other two in the same pass since the plumbing
(`ErrRule` → 429, counter read) is shared — **but only if you want it**; it's
scope creep otherwise.

## 8. Error surface

`ErrRule` implements `error` and carries `Kind` ("window"|"cap"), `Detail`, and
(for windows) `NextAllowed time.Time`. Mapping:
- **Direct `ns/model` call** → `429` with
  `{"error":{"type":"rate_limit_error","code":"model_unavailable","message":"...","rule":{"kind":"window","detail":"allowed 22:00-06:00 Asia/Kuala_Lumpur","next_allowed":"2026-10-06T22:00:00+08:00"}}}`
  (`next_allowed` is the genuinely useful bit — a client can sleep until then.)
- **Combo hop** → silently dropped (`continue`), exactly as today. If *every*
  hop is dropped, the combo 429s with the **first** hop's rule as the reason
  (already the `first != nil` path).
- **`/v1/models`** → unaffected. Lists everything (locked decision).

**Verified**: `errorStatus` (`server.go:424`) is a chain of `errors.As` against
*concrete pointer types* (`*router.ErrNotFound` → 404, `*router.ErrUnavailable`
→ 429) with a **502 default**. So an unwrapped `ErrRule` would fall through to
502 — the wrong status for a policy refusal. `ErrRule` must therefore either
embed/wrap `*ErrUnavailable` or be added to that switch ahead of the default.
**Add it to the switch** (explicit beats clever here) *and* keep `ErrRule`
self-describing so the handler can put `kind`/`next_allowed` in the body.

## 9. Locked decisions (Calypso, 2026-10-06)

1. **`5h` = calendar-aligned blocks.** O(1), reset-free. Documented caveat:
   headroom can be tiny immediately after a block reset — that is accepted, not
   a bug. No sub-bucket granularity, no range scans.
2. **Cap counting is post-response.** The counter increments on the ledger
   write, so the request that *crosses* the cap is served and the next one is
   refused. Chosen deliberately over pre-check reservation: it never
   over-refuses a request that would have fit. Consequence to remember: a cap
   is a soft ceiling, overshoot is bounded by one request's output.
3. **Model rules only this pass.** The existing `accounts.cap_tokens` and
   `client_tokens.cap_tokens` stay decorative and untouched — smallest diff,
   ships the requested feature. The `ErrRule` → 429 plumbing is built so those
   two layers can be wired later without reshaping the error surface.
4. **API + tests first, dashboard editor after.** Matches the M3.2 sequence.
5. **`least_used` gets implemented** — the counter table is exactly the usage
   data `router.go:302` was waiting for, so it is nearly free once §4.2 exists.
   Ordering by real usage replaces the current position-order fallback.

## 10. Build order (once signed off)

1. Migration + `model_rules`/`usage_counters` schema, store CRUD (+ tests)
2. `internal/rules` package: `inWindow` (with overnight/DST/tz table tests),
   `bucketFor`, cached rule set, counter read/increment
3. Wire `WithEligibility` in `cmd/ezllm/main.go` — the first real predicate
4. Counter increment inside `insertCalls` (same tx, batch-grouped)
5. `ErrRule` → 429 mapping + `next_allowed`; combo drop path unchanged
6. Tests: unit (window edge cases) + e2e (day/night combo actually flips hops
   when the clock crosses the boundary — **inject a clock**, don't sleep)
7. Admin API `/admin/model-rules` (+ tests)
8. Dashboard rules editor
9. Live proof: a night-only model 429s at "noon" and routes inside a combo at
   "night", with a faked clock

## 11. What I will NOT do

- No `SUM()` over `calls` on the hot path. Ever.
- No hiding models from `/v1/models` (locked decision).
- No multiple rule rows per (account, model) — one row, OR'd days.
- No silent cap semantics — the counted quantity is a documented constant.
- No commit without your gate; patches only, as always.
