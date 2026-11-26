# Back up and restore

The `data` folder holds everything ezllm knows: settings, history, and the master key
that unlocks your provider keys. A backup needs the database and that key together. One
without the other cannot be read.

Pick one:

- **[Encrypted backups](#encrypted-backups)**: ezllm makes them, checks them and keeps
  the last few, daily or when you ask.
- **[Copy by hand](#copy-by-hand)**: stop ezllm and copy the folder.

## Encrypted backups

You need a folder on another disk or a NAS, and a **backup secret**: a separate password,
kept in a file. The examples use `~/ezllm` for ezllm and `/mnt/backup/ezllm` for the
backups; change both to yours.

### 1. Make the secret

This makes a random secret in a file only you can read:

```sh
mkdir -p ~/ezllm-secrets && chmod 700 ~/ezllm-secrets
openssl rand -base64 32 > ~/ezllm-secrets/backup && chmod 600 ~/ezllm-secrets/backup
```

Keep a copy of this file somewhere other than the backups. Without it, no backup can be
opened.

### 2. Tell ezllm where they are

Make the backup folder, owned by you:

```sh
sudo mkdir -p /mnt/backup/ezllm && sudo chown "$(id -u):$(id -g)" /mnt/backup/ezllm
```

Pick the way you run ezllm:

- **Docker Compose:** in `compose.yml`, add these under `environment:`:

  ```yaml
        EZLLM_BACKUP_DIR: /backups
        EZLLM_BACKUP_SECRET_FILE: /run/secrets/ezllm-backup
  ```

  and these under `volumes:`. Change `/home/you` to your home folder (`echo $HOME`
  prints it).

  ```yaml
        - /mnt/backup/ezllm:/backups
        - /home/you/ezllm-secrets/backup:/run/secrets/ezllm-backup:ro
  ```

  Then run `docker compose up -d`.

- **The ezllm binary:** set these before you start ezllm, then restart it. For the
  systemd service, add them as `Environment=` lines like the
  [bridge pages](bridges/claude.md#with-the-ezllm-binary) do.

  ```sh
  export EZLLM_BACKUP_DIR=/mnt/backup/ezllm
  export EZLLM_BACKUP_SECRET_FILE="$HOME/ezllm-secrets/backup"
  ```

ezllm refuses to start if the secret file is missing. A secret shorter than 24 characters,
or the same as the master key, leaves backups off.

### 3. Turn them on

1. In the dashboard, open **Settings** → **Backup & Recovery**. You should see
   `Backup storage configured`. If it says `Backup storage is not configured`, check the
   folder and the secret.
2. Choose **Back up now**. The backup appears in the list when it has been checked.
3. To back up every day, switch on **Scheduled backups**. **Daily backup hour** (in your
   timezone) and **Verified backups to keep** start at 3 and 7. Choose **Save changes**.

Each backup is checked before it is kept: ezllm opens a copy, checks the database, and
checks that the key opens your stored provider keys. Old backups are deleted only after a
new one passes. A failed backup rings the bell and leaves the older ones alone.

### Check a backup

This opens a backup and checks it, without changing anything:

```sh
~/ezllm/bin/ezllm backup-verify --archive /mnt/backup/ezllm/ARCHIVE.tar.gz.age --secret-file ~/ezllm-secrets/backup
```

`ARCHIVE` is the backup's name, such as `ezllm-20260101T030000Z-1a2b3c4d`. With Docker,
run it from the image ezllm runs, so the versions match:

```sh
docker run --rm --user "$(id -u):$(id -g)" -v /mnt/backup/ezllm:/backups:ro \
  -v "$HOME/ezllm-secrets/backup:/secret:ro" --entrypoint /app/ezllm \
  "$(docker inspect -f '{{.Config.Image}}' ezllm)" backup-verify --archive /backups/ARCHIVE.tar.gz.age --secret-file /secret
```

You should see `backup verified` and the date it was made.

### Restore

1. Stop ezllm: `docker stop ezllm`, `systemctl --user stop ezllm`, or Ctrl+C.
2. Restore into a new folder next to `data`. The folder must not exist yet:

   ```sh
   ~/ezllm/bin/ezllm backup-restore --archive /mnt/backup/ezllm/ARCHIVE.tar.gz.age \
     --secret-file ~/ezllm-secrets/backup --output ~/ezllm/data-restored
   ```

   With Docker:

   ```sh
   docker run --rm --user "$(id -u):$(id -g)" -v /mnt/backup/ezllm:/backups:ro \
     -v "$HOME/ezllm-secrets/backup:/secret:ro" -v "$HOME/ezllm:/out" --entrypoint /app/ezllm \
     "$(docker inspect -f '{{.Config.Image}}' ezllm)" backup-restore --archive /backups/ARCHIVE.tar.gz.age \
     --secret-file /secret --output /out/data-restored
   ```

   You should see `backup restored`. If it fails, do not use that folder: fix the error
   and restore into another new one.
3. Swap the folders. The old one stays as `data-old` until you are sure:

   ```sh
   cd ~/ezllm && mv data data-old && mv data-restored data
   ```

4. Start ezllm. If you set `EZLLM_MASTER_KEY`, remove it: the restored key file must be
   the one in use.

Sign in with the dashboard password from the backup. Your API keys work as before.

A backup does not have `config.yaml`, `compose.yml` or your environment settings, the
bridge CLIs and their logins, or the backup secret. Keep copies of those yourself.

## Copy by hand

Stop ezllm, copy the whole `data` folder, then start it again:

```sh
cd ~/ezllm && cp -a data ~/ezllm-backup-$(date +%Y%m%d-%H%M)
```

The copy holds the master key, so keep it private. To copy while ezllm runs, use
`scripts/backup_sqlite.py` ([master key](master-key-runbook.md#back-it-up)).

## Before you upgrade

Back up first, and keep the old image or binary. A newer version can change the database
so that an older one cannot open it. To go back, start the old version with the backup
you made before the upgrade. What to change for each version: [upgrading](upgrading.md).

## Export your configuration

**Settings** → **Backup & Recovery** → **Export configuration** downloads your accounts,
combos, rules, prices and settings as a readable file, without any keys. It helps you
check or rebuild a setup by hand. It is not a backup: it cannot be restored.

---

**How it works:** [API.md](API.md) (`/admin/backups`, `/admin/export`) ·
**The master key:** [master key](master-key-runbook.md)
