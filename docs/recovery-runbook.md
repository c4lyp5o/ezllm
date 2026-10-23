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
