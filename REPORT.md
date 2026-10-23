# Open audit findings

Updated: 2026-10-23

Reviewed commit: `19489a2` (`fix/audit-findings`, application v1.1.1, schema v28)

Two recovery and upgrade findings remain open, both medium.
Previously resolved findings remain removed. This sweep changed no production
code, release version, database schema, or deployment.

## Recovery and upgrades sweep

Reviewed startup/key handling, schema migration and restart behavior, WAL
backup instructions, export completeness and errors, shutdown ownership,
durable meter recovery, and release/container workflows. Local probes used
only disposable databases, dummy secrets, and local processes.

### RU4 — Medium: disaster-recovery export omits non-secret configuration

**Locations:** `internal/store/admin.go:1633`,
`internal/store/admin.go:1689`, `internal/server/admin.go:2069`, and
`docs/API.md:517`.

The export is documented as a file that can re-seed a fresh instance minus
credentials, but it contains only account summaries, combos, token summaries,
compression profiles, quota summaries, and optional projected ledger rows.
It does not include per-model rules, effective-dated prices, or individual
model capability/pin records. Account-level counts and protocol counts do not
reconstruct those records. These omissions are distinct from the deliberate
exclusion of secrets.

**Reproduction:** `TestRecoveryAuditConfigExportOmitsState` stores a model rule
with a daily cap and a model price, then downloads the export. The valid
response has no representation for those configured rules or price records,
and models are represented only by aggregate account counts. Source inspection
confirms the projection has no corresponding collections.

**Impact:** restoring from this file cannot reproduce enforcement policy,
price history, or operator routing pins. Re-entering provider credentials or
rediscovering the catalog does not recover those operator decisions.

**Suggested fix:** define a versioned recovery format covering non-secret
persistent configuration, with a tested re-seed/restore procedure and explicit
handling of identifiers and omitted secrets. If this endpoint remains a
partial diagnostic export, label its limitations and require a database-plus-
key backup for disaster recovery. Add round-trip recovery coverage.

### RU5 — Medium: export read failures become successful-looking empty backups

**Locations:** `internal/server/admin.go:2090`,
`internal/server/admin.go:2096`, and `internal/server/admin.go:2122`.

The handler commits HTTP 200 before opening the ledger cursor. If the ledger
query fails, it logs the error and closes a valid JSON document with
`ledger:[]`. Row scan or iteration failures likewise close the document after
the rows already emitted. The download has no error, completeness flag,
expected-row count, or completion manifest to distinguish a failed export
from a genuinely empty/smaller ledger. The documentation explicitly favors
valid JSON on failure, but supplies no in-file failure signal.

**Reproduction:** `TestRecoveryAuditExportLooksSuccessfulWithoutLedger` seeds
a ledger row, then renames one projected column in the disposable database to
inject a query error while leaving config reads intact. The export returns
HTTP 200 and valid JSON containing `ledger:[]`, with no error marker. The
server log is the only indication the ledger was not exported.

**Impact:** backup automation that checks HTTP success and JSON validity can
accept an incomplete recovery artifact and discover the missing history only
at restore time. Comparing with a separate live health count is not an atomic
completion check for this specific export.

**Suggested fix:** validate/open required cursors before committing headers,
and make streaming failures machine-detectable in the artifact. Use an
explicit final completeness manifest or fail the document/transport rather
than representing an error as a complete empty ledger. Exercise initial query,
mid-stream scan, iteration, and client-disconnect failures.

## Verification and limits

- `make test-short` — passed across all packages; output:
  `/tmp/ezllm-recovery-functional.txt`.
- `go test -race -count=1 -run '^TestRecoveryAudit' -v ./internal/store ./internal/server`
  — all database/export reproduction probes passed without race warnings;
  output: `/tmp/ezllm-recovery-probes.txt`.
- `go test -count=1 -run '^TestRecoveryAudit' -v ./cmd/ezllm`
  — the master-key overwrite probe passed as a non-root user; output:
  `/tmp/ezllm-recovery-masterkey-probe.txt`.
- `go test -race -short -count=1 -timeout 3m -run 'Test(Migrate.*|Open.*|PickKeyFailsWithWrongMasterKey|AccountingMeter.*|Export.*|DrainWaits.*)$' ./internal/store ./internal/server ./cmd/ezllm`
  — existing migration, encryption, export, meter recovery, and shutdown
  checks passed without race warnings; output:
  `/tmp/ezllm-recovery-existing-race.txt`.

Probe success means the stated failure was reproduced, not that the behavior
is correct. Temporary probe sources were removed and preserved in
`/tmp/ezllm-recovery-audit/` as:

- `store_recovery_audit_temp_test.go.txt`
- `server_recovery_audit_temp_test.go.txt`
- `cmd_recovery_audit_temp_test.go.txt`

Restore the matching file as
`recovery_audit_temp_test.go` in its original package to rerun. The command
probe uses `/tmp/ezllm-accounting-fix`, the gateway binary built from the same
production code during the preceding verified fix turn; rebuild that path
before reproducing elsewhere.

This audit did not restore a real production backup, interrupt a live process,
run a deployment, publish artifacts, or use provider credentials. Release
workflows were inspected locally; CI and container builds were not executed.
Newer-schema refusal, successful legacy migrations, clean shutdown ownership,
and recent durable meter recovery were reviewed without relisting their
resolved issues. The performance harness was skipped per the repository's
memory constraint. Application version remains v1.1.1 and schema remains v28.
