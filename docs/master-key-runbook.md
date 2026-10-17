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

```bash
cp data/ezllm.sqlite        ~/backups/ezllm/<date>/ezllm.sqlite
cp data/ezllm-master.key    ~/backups/ezllm/<date>/ezllm-master.key
chmod 600 ~/backups/ezllm/<date>/*
```

Losing the master key makes every stored provider key unrecoverable. There is no
recovery path; the only fix is re-entering every key in the dashboard.

## Rotation

There is no in-place re-encrypt command yet. To rotate:

1. Stop the service.
2. Back up the DB and the current key.
3. Set the new key (env or file).
4. Start the service — the migration sees ciphertext it cannot decrypt and
   **fails fast** instead of returning ciphertext as plaintext.
5. Re-enter every provider key in the dashboard (they will be encrypted with the
   new key on write).

A future `ezllm rotate-master-key` command can decrypt with the old key and
re-encrypt with the new one; not shipped.

## Failure modes

- **Wrong key / lost key**: boot fails with
  `store: cannot decrypt provider key N — wrong master key?`.
- **No key in a container**: set `EZLLM_MASTER_KEY_FILE=/data/ezllm-master.key`
  (the compose default) and make sure `/data` is writable by the container user.
- **Key backed up with the DB**: that is intentional; the pair is the unit of
  restore. Store the backup somewhere encrypted.
