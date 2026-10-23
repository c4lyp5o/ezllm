# Master key runbook

ezllm encrypts provider API keys and per-account management keys at rest with
AES-256-GCM. The master key is never stored in the database.

## Where it comes from (first match wins)

1. `EZLLM_MASTER_KEY` — the raw secret (base64 is fine; any string works).
2. `EZLLM_MASTER_KEY_FILE` — path to a file containing the secret.
3. `~/.hermes/secrets/ezllm-master.key` — the default outside containers.
4. `$EZLLM_DATA_DIR/.hermes/secrets/ezllm-master.key` when the home directory is
   unusable (containers).

If none exists, ezllm generates 32 random bytes, writes the file mode 0600, and
uses it. The Docker compose file defaults to `/data/ezllm-master.key` so the
key lives on the data volume and is backed up with the SQLite ledger.

## Backup (do this with every DB backup)

Do not copy a running database's main file with `cp`: committed pages can
still be in `-wal`. Do not copy live DB/WAL files separately as a snapshot.
Use the online tool from the repository (Python 3.11+, standard library only):

```bash
python3 scripts/backup_sqlite.py --db data/ezllm.sqlite \
  --key data/ezllm-master.key --output /backups/ezllm/new-backup
```

The output directory must not exist. The tool includes committed WAL data,
closes and verifies the SQLite copy, copies the key without changing it, and
writes `COMPLETE.json` last with the database checksum. Require both exit
status zero and the completion marker; a failed/incomplete directory is not a
backup. The directory is private and its files are mode 0600. Keep the pair
encrypted off-host. This captures committed rows, not usage still queued in a
running gateway; cleanly stop the service first when every accepted call must
be included. Do not rotate/change the master key during backup.

Use the **active** key, honoring the precedence above. If `EZLLM_MASTER_KEY`
overrides the file, put that same secret in a protected file for the tool;
backing up an ignored key file is not enough. Also preserve `config.yaml` and
any separately mounted subscription login/CLI data needed by your deployment.

For an offline copy instead, cleanly stop the service, confirm it exited and
closed its ledger, then copy the database and active key as a pair into a new
private directory. Restart only after the copy finishes. Never run that
procedure against a live writer.

Restore into a separate stopped instance: verify `COMPLETE.json`, recompute
and compare `database_sha256` before opening the copy, check SQLite
`PRAGMA integrity_check`, then copy both files to its data directory and set
`EZLLM_MASTER_KEY_FILE` to the copied key. Start on a different local port,
verify account access and representative recent rows, and only then switch
traffic. Keep the original backup immutable; restore from a copy. An older
binary may refuse a newer schema, so rollback uses a matching pre-upgrade
backup. See [recovery-runbook.md](recovery-runbook.md).

Losing the master key makes every stored provider key unrecoverable. There is no
recovery path; the only fix is re-entering every key in the dashboard.

## Rotation

In-place master-key rotation is not implemented. Do not replace the secret on
an existing database: startup correctly refuses credentials encrypted with a
different key, so the dashboard cannot be used to repair that mismatch.

To move the key, stop the service, copy the existing key without changing its
contents, set `EZLLM_MASTER_KEY_FILE` to the new location, then restart and
verify access. Retain the original database/key backup until verification.

If changing the secret is required, retain a verified backup and prepare a
separate fresh database with the new secret. Re-create non-secret configuration
from the configuration snapshot and re-enter provider/management credentials.
Verify the replacement instance before switching traffic; keep the original
instance and key available for rollback. This is a replacement-instance
procedure, not in-place re-encryption or a lossless ledger restore.

## Failure modes

- **Wrong key / lost key**: boot fails with
  `store: cannot decrypt provider key N — wrong master key?`.
- **Unreadable or empty key file**: startup fails without modifying the file.
  Correct its permissions/content; do not replace an existing encryption key.
- **No key in a container**: set `EZLLM_MASTER_KEY_FILE=/data/ezllm-master.key`
  (the compose default) and make sure `/data` is writable by the container user.
- **Key backed up with the DB**: that is intentional; the pair is the unit of
  restore. Store the backup somewhere encrypted.
