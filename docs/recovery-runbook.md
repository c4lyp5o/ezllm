# Recovery and upgrades

Back up the SQLite database and its active master key before upgrading. Keep
the old binary/image and the verified pair until the upgraded instance passes
health, account access, and representative routing checks. A newer database
schema is not necessarily readable by the old binary; rollback uses the old
binary with the pre-upgrade database/key pair in a separate stopped instance.

## Interrupted legacy combo rebuild

The pre-v9 combo rebuild now runs atomically on a dedicated SQLite connection.
A failed copy or validation rolls back the rebuild and restores foreign-key
checking. Reopening handles the two remnants left by older binaries:

- Original legacy `combos` still present: it remains authoritative; rebuild
  discards the partial staging copy inside the transaction.
- Only `combos_v9` present: recover that copy and finish normalization before
  exposing the configuration.

If both a current-format `combos` and a staging table exist, startup refuses
rather than choose or delete data. Stop the service, preserve the entire
DB/key backup, and inspect both tables on a copy before reconciling. Do not
blindly delete the staged copy: it can contain the only surviving configuration
from an older interrupted upgrade. Retry the upgraded instance after repair.

## Verified database/key backup

Use the procedure in [master-key-runbook.md](master-key-runbook.md). The online
backup helper uses SQLite's backup API and does not copy a live main file or
assume WAL checkpoints have already run. Accept only a successful exit plus
`COMPLETE.json`; test restoration to a separate stopped instance before relying
on the backup. Never let a second gateway write the same restored data files.

## Configuration exports

`GET /admin/export` includes non-secret rules, prices, model capability/pin
records and service bindings as well as account/combo/token/profile summaries.
The versioned file is useful for inspection and manually re-creating operator
configuration. It is not a full database backup or a lossless restore format;
its `database_backup_required` marker makes that limitation explicit. Keep the
verified SQLite/key pair for full recovery and accounting history.
