# Open audit findings

Updated: 2026-10-23

Branch: `fix/audit-findings`

Application version: v1.1.1 (unchanged); database schema: v28 (unchanged)

No open findings remain from the completed sweeps. All five recovery and
upgrade findings have verified fixes, and their resolved entries are removed.
This tracks the audited findings, not a guarantee that no other defects exist.

## Verification

Permanent regressions cover:

- Preserving unreadable/empty master-key files, exclusive concurrent first
  boot, and private key-file permissions.
- Transactional legacy combo rebuild, interrupted staging before/after DROP,
  rollback on failed validation, restored foreign-key checks, and repeat open.
  Ambiguous current/staged copies are preserved and startup refuses to choose.
- A real online SQLite backup of committed WAL rows, the active key copy,
  integrity/completion metadata, a matching database checksum, and refusal to
  overwrite existing backup output.
- Versioned non-secret exports including model rules, prices, capability/pin
  records, service bindings, and explicit database-backup requirements.
- Export preflight query failures, midstream scan errors with incomplete
  manifests, successful empty/non-ledger completion, and client disconnects
  without a false success marker. Existing 12,000-row streaming coverage and
  credential-exclusion assertions remain intact.

Checks passed on the final code:

- `make test-short` — all packages; output:
  `/tmp/ezllm-recovery-fix-functional.txt`.
- Focused race checks for master keys, recovery, migrations, exports, meter
  restart recovery, and shutdown across store/server/command packages —
  passed without race warnings; output: `/tmp/ezllm-recovery-fix-race.txt`.
- `go vet ./...` and native gateway build — passed.
- Windows amd64 static cross-build — passed; binary:
  `/tmp/ezllm-recovery-fix-windows.exe`.
- Formatting and diff checks — clean.

The optional online backup tool requires Python 3.11+. Configuration exports
are an inspection/manual-configuration aid, not full database restore images.
Full recovery uses a verified SQLite/key backup and separately preserved
runtime configuration/subscription data; the recovery and master-key runbooks
include the backup/restore and rollback procedure. Unknown ambiguous migration
remnants require manual reconciliation on a copy rather than automatic deletion.

All verification used disposable databases, dummy keys, local processes, and
mock upstreams. No real production backup was restored, no deployment occurred,
and no provider credentials or paid quota were used. The full performance
harness was skipped per the repository's memory constraint. The preexisting
Python cache remains untouched.
