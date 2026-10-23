# Open audit findings

Updated: 2026-10-23

Branch: `fix/audit-findings`

Application version: v1.1.1 (unchanged)

No open findings remain from the completed sweeps. All five accounting
integrity findings have verified fixes, and resolved entries have been removed.
This tracks the audited findings; it does not claim the absence of other defects.

## Verification

Permanent regression coverage checks:

- Source usage and raw evidence across all nine wire pairs, JSON and SSE,
  plus usage retained when client conversion fails.
- Native multiline SSE usage, CRLF, bounded events, and recovery after an
  oversized event, with byte-exact forwarding.
- Durable meter reconciliation across restart, model/client counters,
  fail-closed capped admission, uncapped operator access, and exactly-once
  reservation release. Migration from schema v27 to v28 preserves ledger rows.
- Same-hop retries, JSON/SSE failover, final-error fallback, attempt-budget
  exhaustion, account/key attribution, and total client spend. Errors without
  reported usage do not receive estimated charges.
- USD-only price validation and legacy non-USD rates treated as unpriced
  without silently falling back to older USD rates.

Checks passed on the completed fixes:

- `make test-short` — all packages; output:
  `/tmp/ezllm-accounting-fix-functional-final.txt`.
- `go test -race -short -count=1 -timeout 3m ./internal/proxy ./internal/store ./internal/translate`
  — no race warnings; output: `/tmp/ezllm-accounting-fix-race-final.txt`.
- Focused server accounting, pricing, reservation, health, and version race
  checks — passed; output: `/tmp/ezllm-accounting-fix-server-race.txt`.
- Final attempt-accounting race regressions — passed; output:
  `/tmp/ezllm-accounting-attempt-final.txt`.
- `go vet ./...`, gateway build, formatting, and diff checks — passed.

The application version remains v1.1.1. The database schema advances to v28
for durable pending-meter debits, with migration coverage. Usage reported by
discarded attempts now has separate ledger rows, so ledger call counts can
exceed client request counts. Unknown usage is not estimated. Existing lost
usage history is not rewritten by these changes.

Validation used mock upstreams and disposable databases, without live provider
calls, credentials, or paid quota. The full performance harness was not run;
the repository's memory-safe functional gate was used. The preexisting Python
cache remains untouched.
