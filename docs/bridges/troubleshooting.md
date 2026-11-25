# A bridge does not work

## Read the reason

Docker:

```sh
docker logs --tail 50 ezllm
```

The ezllm binary, as a service:

```sh
journalctl --user -u ezllm -n 50
```

A bridge that cannot start stops all of ezllm, and the last line says why. In Docker, do
this first: starting ezllm again clears the log.

## Get ezllm back

- **Docker Compose:** set that bridge to `"off"` in `compose.yml` and run
  `docker compose up -d`.
- **`docker run`:** bring the old container back:

  ```sh
  docker rm -f ezllm
  docker rename ezllm-old ezllm
  docker start ezllm
  ```

- **The ezllm binary:** remove that bridge's file, here for Codex, and start again. The
  other files are `claude-bridge.conf`, `copilot-bridge.conf` and `agy-bridge.conf`.

  ```sh
  rm ~/.config/systemd/user/ezllm.service.d/codex-bridge.conf
  systemctl --user daemon-reload
  systemctl --user restart ezllm
  ```

## Find the reason

"Redo" means the step with that name on your plan's page.

| The log says | Do this |
|---|---|
| `no such file or directory` | Run the [checker](#the-checker) |
| `no ChatGPT login` or `no Antigravity login` | Redo **Sign in** |
| `login=logged-out` (Claude) | Redo **Sign in** |
| `is not a directory — mount the CLI's login home there` | Add the second `volumes:` line (`docker run`: the second `-v`) |
| `mapping key … already defined` | That line is in `compose.yml` twice. Keep one |
| `token dir: mkdir /data: permission denied` | ezllm binary: the `…_BRIDGE_TOKEN_FILE` line is missing |
| `is a Linux feature` | That bridge needs Linux. On Windows, [use WSL](../wsl.md). On macOS, run ezllm in a Linux container or VM |
| `Permission denied` | Compose: set `UID` and `GID`, as your plan's page says. `docker run`: keep the `--user` part |
| `channel unavailable` (Copilot) | Your ezllm is older than this page. Update ezllm, or read the page for [your version](../../README.md#documentation) |
| `The container name "/ezllm" is already in use` | You skipped "Set the old container aside" |
| `The container name "/ezllm-old" is already in use` | One is left over. If ezllm works, `docker rm ezllm-old` and repeat |

Signed in, but the key test says **not eligible** (Antigravity)? Redo **Sign in**, open
the link it prints and verify the account.

Antigravity answers **the turn timed out**? Google may have no capacity for that model
right now. Try later or use another model; the
[runbook](../agy-bridge-runbook.md#limits) says how to check.

## The checker

Paste it on the computer that runs ezllm. It only looks.

```sh
for f in claude-bridge/current/claude codex-bridge/current/bin/codex \
         copilot-bridge/current/copilot agy-bridge/current/agy; do
  b=${f%%/*}; d=~/.local/share/$b; p=~/.local/share/$f
  if [ ! -d "$d" ]; then echo "--   $b: not set up"
  elif [ ! -L "$d/current" ]; then echo "BAD  $b: no 'current' link (redo Copy)"
  elif case "$(readlink "$d/current")" in /*) true;; *) false;; esac; then echo "BAD  $b: 'current' is a full path (redo Copy)"
  elif [ ! -f "$p" ]; then echo "BAD  $b: $p is missing (redo Copy)"
  elif [ "$(head -c 4 "$p" | tail -c 3)" != ELF ]; then echo "BAD  $b: a script, not the real program (redo Install)"
  else echo "OK   $b $(readlink "$d/current")"; fi
done
```

- `OK` for your bridge, and ezllm still says `no such file or directory`? Then the
  `volumes:` line (`docker run`: the `-v` line) is missing or misspelled, or you set
  an `EZLLM_…_BIN`: remove it, the default is right. With the ezllm binary, the
  `EZLLM_…_BIN` line is missing or misspelled: there you do need it.
- `BAD`? Redo the step it names.
- Is the folder empty and owned by root? Docker made it because the path in
  `volumes:` or `-v` did not exist. Delete it and redo **Copy**.

## The sign-in command does not run

`No such file or directory` there means **Copy** did not finish, so sign-in never
started. In WSL, install the Linux CLI inside WSL: a Windows one under `/mnt/c` will not
work.
