# The master key

ezllm encrypts every provider key it stores with one **master key**. The key is a file
next to the database, never inside it. Lose the key and the stored provider keys cannot
be read: the only fix is to enter each one again.

## Where ezllm finds it

The first one that is set wins:

1. `EZLLM_MASTER_KEY`: the secret itself.
2. `EZLLM_MASTER_KEY_FILE`: a file that holds it.
3. `ezllm-master.key` in the data folder (`/data/ezllm-master.key` in Docker).

When there is none, ezllm makes one on its first start and saves it with mode `600`. The
quick start sets `EZLLM_MASTER_KEY_FILE` to the data folder, so the key is backed up with
the database.

## Back it up

Back up the key and the database together, always. The easiest way is
[encrypted backups](recovery-runbook.md#encrypted-backups), which include both.

To copy both while ezllm runs, use `scripts/backup_sqlite.py` (Python 3.11 or newer). A
plain `cp` of a running database can miss recent changes. Get the script if you have no
copy of the repository:

```sh
curl -fsSLO https://raw.githubusercontent.com/c4lyp5o/ezllm/master/scripts/backup_sqlite.py
```

Then, from the folder with `data` in it:

```sh
mkdir -p ../ezllm-backups && python3 backup_sqlite.py --db data/ezllm.sqlite \
  --key data/ezllm-master.key --output ../ezllm-backups/$(date +%Y%m%d-%H%M)
```

Use the path to the script if it is somewhere else, such as `scripts/backup_sqlite.py`.
You should see `backup complete`. The new folder holds `ezllm.sqlite`,
`ezllm-master.key` and `COMPLETE.json`, which is written last. A folder without
`COMPLETE.json` is not a backup.

If you set `EZLLM_MASTER_KEY`, the key file is not the one in use. Put that same secret in
a private file and pass it with `--key`.

Requests still being recorded when the copy runs are not in it. To include everything,
stop ezllm first.

To restore it, stop ezllm, copy `ezllm.sqlite` and `ezllm-master.key` into an empty data
folder, and start ezllm.

## Move the key

1. Stop ezllm.
2. Copy the key file to its new place without changing it.
3. Set `EZLLM_MASTER_KEY_FILE` to the new path.
4. Start ezllm and open a provider in the dashboard to check its key still works.

## Change the key

Change the key if the key file was exposed. `rotate-master-key` makes a new key,
re-encrypts every stored secret with it in one step, and puts it in the key file. If any
value fails, nothing changes.

1. [Back up](recovery-runbook.md) first.
2. Stop ezllm: `docker stop ezllm`, `systemctl --user stop ezllm`, or Ctrl+C.
3. Rotate. With the ezllm binary:

   ```sh
   ~/ezllm/bin/ezllm rotate-master-key --data-dir ~/ezllm/data
   ```

   With Docker, run it from the image ezllm runs:

   ```sh
   docker run --rm --user "$(id -u):$(id -g)" -v "$HOME/ezllm/data:/data" --entrypoint /app/ezllm \
     "$(docker inspect -f '{{.Config.Image}}' ezllm)" rotate-master-key --data-dir /data
   ```

   You should see `master key rotated` and how many values it re-encrypted. If the key
   lives somewhere else, add `--key-file` with its path. A key set in `EZLLM_MASTER_KEY`
   must be moved into a file first.
4. Start ezllm and open a provider in the dashboard to check its key still works.
5. Delete the old key, which the command kept as `ezllm-master.key.old-<time>`.

## Not working?

| The log says | Do this |
|---|---|
| `cannot decrypt provider key N — wrong master key?` | ezllm has a different key from the one that saved the provider keys. Find the right key file, or restore the key with its database |
| `read master key` or `master key file is empty` | The key file is unreadable or empty. Fix its owner or mode, or restore it. Do not replace it with a new key |
| `found the master key at its pre-2.0.0 default` | Move the key as the message says. See [upgrading](upgrading.md) |
| In Docker, ezllm cannot write the key | Set `EZLLM_MASTER_KEY_FILE=/data/ezllm-master.key` and check that `data` belongs to the user ezllm runs as |
