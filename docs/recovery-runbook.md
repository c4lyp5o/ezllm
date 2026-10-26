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

The manual Python procedure in [master-key-runbook.md](master-key-runbook.md)
and the encrypted Settings workflow below both use SQLite's online backup API.
Neither relies on copying a live main file or assuming WAL was checkpointed.
For the Python format require a successful exit plus `COMPLETE.json`; for the
encrypted format require successful verification of the `.tar.gz.age` archive.
Their completion formats and restore procedures are different. Test restoration
to a separate stopped instance before relying on either format, and never let a
second gateway write the same restored data files.

## Configuration exports

`GET /admin/export` includes non-secret rules, prices, model capability/pin
records, service bindings and saved instance settings as well as
account/combo/token/profile summaries.
The versioned file is useful for inspection and manually re-creating operator
configuration. It is not a full database backup or a lossless restore format;
its `database_backup_required` marker makes that limitation explicit. Keep the
verified SQLite/key pair for full recovery and accounting history.

Require the configuration export's final `completion.complete:true` marker as
well as successful HTTP and JSON parsing. An absent/false completion marker is
not a successful export; a false marker includes an error and emitted row count.
Never interpret a failed ledger read as an empty ledger. The SQLite backup
helper separately uses its `COMPLETE.json` completion file.

## Automated encrypted backups

Configure an absolute mounted `EZLLM_BACKUP_DIR` and a separate
`EZLLM_BACKUP_SECRET_FILE` before starting ezllm. The secret must contain at least
24 bytes after trimming whitespace and must differ from the active
field-encryption master key.
Protect this file and keep a recovery copy separately from the backup archives.
Settings → Backup & Recovery provides manual runs, scheduling and verified status.
Automation starts disabled; enabling defaults to daily at 03:00 in the instance
timezone with seven verified backups retained.

`backup.go` `BackupTo` uses SQLite's online backup API after a ledger flush, so
committed WAL data is included without a Python dependency. Live traffic after
the snapshot belongs to a later backup. Do not rotate deployment secrets during
a backup. The archive includes the active master secret, honoring environment
overrides; it does not rely on an ignored key file.

Each `.tar.gz.age` archive contains `ezllm.sqlite`, `ezllm-master.key`,
`manifest.json` and `RECOVERY.txt`. Before publication, ezllm decrypts a temporary
copy, verifies the database SHA-256 and SQLite integrity, checks schema support,
and proves the key decrypts stored provider/management credentials. Temporary
plaintext has private directory/file permissions and is removed after the job.
The companion JSON record marks archives verified at creation and published.
Retention starts only after publication and removes recognized completed pairs.
A snapshot or verification failure preserves earlier backups; cleanup errors can
leave extra older archives. Listing an archive does not reverify its contents.

Verify an archive offline:

```sh
ezllm backup-verify --archive /backups/ARCHIVE.tar.gz.age --secret-file /run/secrets/backup
```

Restore into a new directory with the destination instance stopped:

```sh
ezllm backup-restore --archive /backups/ARCHIVE.tar.gz.age --secret-file /run/secrets/backup --output /data/recovered
```

The output directory must not exist, even if empty. Restore verifies the archive
before creating the output and revokes all restored dashboard sessions. Configure
the recovered database location and `EZLLM_MASTER_KEY_FILE` pointing to the
restored key. Remove any `EZLLM_MASTER_KEY` override, or set it to that same
restored secret: it still takes precedence over the file. Start the same or a
compatible newer binary on a separate port.
Confirm account access and ledger history before switching traffic. Password and
client tokens are recovered from the database; browser sessions must log in again.
The original archive is unchanged.

Deployment YAML/environment, bridge CLI binaries, vendor login directories and
the backup encryption secret are not in the archive; preserve them separately.
The mounted directory may be a NAS or another disk; this feature does not upload
to S3. A failed verification, cancellation or insufficient storage must be resolved
before treating the latest job as successful. Keep enough destination capacity
for the database snapshot, encrypted archive and decrypted verification copy.
CLI verification uses a private system temporary directory; CLI restore stages
in the output directory's parent. Automated verification stages on the configured
backup volume.
