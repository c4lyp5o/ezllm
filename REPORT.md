# Open audit findings

Updated: 2026-10-23

Reviewed commit: `4059482` (`fix/audit-findings`, v1.1.1)

Three accounting integrity findings remain open: two high and one medium.
Previously resolved findings remain removed. No production fixes or version
changes were made during this sweep.

## Accounting integrity sweep

Traced upstream usage normalization, JSON/SSE translation and tapping, retry
and failover attribution, ledger writes, client/model meters, reservations,
and effective-dated cost reporting. Reproductions used mock upstreams and
disposable SQLite databases, with no live provider calls or credentials.

### AI1 — High: translated client envelopes are a lossy accounting source

**Locations:** `internal/proxy/proxy.go:1260`,
`internal/proxy/proxy.go:1362`, `internal/translate/response_codec.go:397`,
`internal/translate/response_codec.go:409`, and
`internal/translate/stream_codec.go:706`.

The proxy translates the upstream response before tapping its usage. The
ledger therefore depends on what the client codec elects to expose, rather
than on the upstream's reported counters. The legacy Responses-to-Chat
response drops cache-read details. Other conversions drop cache-write
counters, and reasoning-token details are lost or synthesized as zero.
The store then treats omitted cache portions as cold input. The `raw_usage`
column also receives the synthesized usage instead of the original upstream
usage, removing the evidence needed to rederive the correct split later.

**Reproduction:** `TestAccountingAuditMatrix` exercises all nine wire pairs
with JSON and SSE: 18 cases. Each upstream reports normalized cold=100,
output=42, cache-read=900, cache-write=50, reasoning=7. The Messages fixture
uses the account's declared inclusive convention. Native routes preserve
all five counters. Translated routes produce the following identical results
in JSON and SSE (Chat means OpenAI Chat Completions):

| Client → upstream | Cold input | Output | Cache read | Cache write | Reasoning |
| --- | ---: | ---: | ---: | ---: | ---: |
| Any native pair | 100 | 42 | 900 | 50 | 7 |
| Chat → Responses | 1050 | 42 | 0 | 0 | 0 |
| Chat → Messages | 150 | 42 | 900 | 0 | 0 |
| Messages → Chat | 150 | 42 | 900 | 0 | 0 |
| Messages → Responses | 150 | 42 | 900 | 0 | 0 |
| Responses → Chat | 150 | 42 | 900 | 0 | 0 |
| Responses → Messages | 150 | 42 | 900 | 0 | 0 |

The fixture includes gateway-supported cache-write details on the Chat and
Responses wires and thinking-token details on Messages; these extensions are
not claimed to be universal provider fields. Standard Messages
`cache_creation_input_tokens` is also lost on the two strict reverse paths.

**Impact:** derived cost can use the wrong rate bucket, cache metrics and
reasoning metrics lose reported information, and caps based on cold input
plus output can reject traffic prematurely. For example, the Chat client on
a Responses hop counts 1092 tokens instead of the documented cap quantity of
142. Reasoning tokens already belong to output; losing their breakdown does
not mean they should be charged a second time.

**Suggested fix:** normalize and preserve upstream usage before client
projection, using the effective upstream wire and its account convention.
For SSE, observe source frames without buffering the entire response. Keep
source accounting distinct from client-facing usage, and retain original
usage evidence. Add end-to-end matrix tests with nonzero cache buckets and
reasoning details, including failed response conversion.

### AI2 — High: a metering failure releases budget without counting the spend

**Locations:** `internal/store/ledger.go:507`,
`internal/store/ledger.go:525`, `internal/store/client_budget.go:176`, and
`internal/store/client_budget.go:94`.

`insertCalls` deletes reservations before entering the best-effort meter
savepoint. If a meter update fails, it rolls back the meter but commits both
the ledger row and reservation deletion. An existing client counter remains
stale; `ClientBudgetStatus` backfills only a missing counter. There is no
automatic replay of the missed batch. The `UnmeteredRows` counter exposes the
failure but does not restore enforcement after the database fault clears.

**Reproduction:** `TestAccountingAuditReservationMeterFailure` creates a
100-token cap with one token already counted, then reserves 99. A temporary
SQLite trigger injects a client-meter error while the corresponding 99-token
ledger row is inserted. After removing the trigger:

- Durable ledger usage is 100.
- The budget counter still reports 1.
- No reservation remains for the spent 99 tokens.
- A fresh 99-token reservation is admitted.

This is a deterministic database-failure injection, not an assertion that the
production schema contains such a trigger. It establishes the ordering and
recovery behavior when the existing best-effort failure path executes.

**Impact:** a transient meter fault can leave client/model caps understated
for the rest of their bucket. The recent reservation fix protects the normal
flush path, but its unreserved-and-uncounted gap remains on this failure path.
Preserving ledger rows on meter failure is deliberate; losing enforcement
without reconciliation is the remaining problem.

**Suggested fix:** retain an enforceable pending debit or durable reconciliation
record until the meter catches up, or rebuild dirty counters from the ledger
before admission. Recovery must avoid charging the same row twice and must
not discard the durable call to preserve counter consistency.

### AI4 — Medium: failover omits usage explicitly reported by earlier hops

**Locations:** `internal/proxy/proxy.go:1802`,
`internal/proxy/proxy.go:1810`, and `internal/server/dispatch.go:119`.

When another hop succeeds, the earlier error response is closed without
normalizing its usage. Only the committed hop's usage enters `Result.Usage`
and the single final call row. Attempt diagnostics carry namespace/model,
status, and text, but no structured usage to meter against the earlier
account. An error snippet can happen to contain some usage text; it does not
feed token counters or cost aggregation.

**Reproduction:** `TestAccountingAuditDiscardedAttemptUsage` uses a first hop
returning HTTP 503 with reported prompt=100/output=42, followed by a successful
hop reporting prompt=10/output=5. Both upstreams were called. The returned
accounting is only 10/5, while the first hop appears only in diagnostics. Total
reported usage across the two attempts was 110/47.

**Impact:** the ledger and account/client meters cannot reconcile all reported
upstream work on a failover request. This finding does not assume every 503 or
429 is billable; it is limited to attempts that explicitly report usage. The
current one-row-per-committed-request policy explains the omission but does
not make that row a complete upstream-spend record.

**Suggested fix:** record per-attempt usage with its actual account, key,
model, status, and source wire, separately from the client delivery row.
Avoid adding the earlier account's tokens to the successful account's row.
Apply retries and final fallback through the same accounting path, and define
how attempt charges reconcile with the request's reservation exactly once.

## Verification and limits

- `make test-short` — passed across all packages; output:
  `/tmp/ezllm-accounting-functional.txt`.
- `go test -race -count=1 -run '^TestAccountingAudit' -v ./internal/proxy ./internal/store`
  — all reproduction probes passed with no race warnings; output:
  `/tmp/ezllm-accounting-probes.txt`. These probes assert that the defects
  reproduce, so passing does not establish correct accounting.
- `go test -race -short -count=1 -timeout 3m ./internal/store ./internal/proxy ./internal/translate`
  — passed with no race warnings after removing the temporary probes; output:
  `/tmp/ezllm-accounting-race.txt`.

Temporary probe sources were removed from the repository and preserved as:

- `/tmp/ezllm-accounting-audit/proxy_accounting_audit_temp_test.go.txt`
- `/tmp/ezllm-accounting-audit/store_accounting_audit_temp_test.go.txt`

Restore each as `accounting_audit_temp_test.go` in its original package to
reproduce. All local upstreams and disposable databases were cleaned up.
The preexisting Python cache remains untouched.

Existing effective-date pricing, unpriced-call reporting, usage conventions,
ordinary reservation handoff, and meter-savepoint tests were reviewed. The
meter's deliberate cache exclusions and nonblocking ledger shedding policy
were not relabeled as newly discovered bugs. Unreported usage after a client
cancels cannot be established from these fixtures and was not estimated.
No full performance harness, live invoice reconciliation, exchange-rate
lookup, or credentialed provider verification was performed. The functional
gate follows the repository's memory constraint.
