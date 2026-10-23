# Open audit findings

Updated: 2026-10-23

Reviewed commit: `4059482` (`fix/audit-findings`, v1.1.1)

One accounting integrity finding remains open: medium-priority attempt accounting.
Previously resolved findings remain removed. No production fixes or version
changes were made during this sweep.

## Accounting integrity sweep

Traced upstream usage normalization, JSON/SSE translation and tapping, retry
and failover attribution, ledger writes, client/model meters, reservations,
and effective-dated cost reporting. Reproductions used mock upstreams and
disposable SQLite databases, with no live provider calls or credentials.

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
