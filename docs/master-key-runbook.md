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

ezllm cannot re-encrypt with a new key. A new key on the same database stops ezllm from
starting, because it cannot read the stored keys.

To change it anyway, start a new ezllm with a new data folder and a new key. Rebuild the
setup using the [configuration export](recovery-runbook.md#export-your-configuration) as
a guide, enter every provider key again, and keep the old instance until the new one
works. History stays in the old database.

## Not working?

| The log says | Do this |
|---|---|
| `cannot decrypt provider key N — wrong master key?` | ezllm has a different key from the one that saved the provider keys. Find the right key file, or restore the key with its database |
| `read master key` or `master key file is empty` | The key file is unreadable or empty. Fix its owner or mode, or restore it. Do not replace it with a new key |
| `found the master key at its pre-2.0.0 default` | Move the key as the message says. See [upgrading](upgrading.md) |
| In Docker, ezllm cannot write the key | Set `EZLLM_MASTER_KEY_FILE=/data/ezllm-master.key` and check that `data` belongs to the user ezllm runs as |
