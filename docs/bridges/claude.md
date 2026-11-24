# Turn on Claude

You need Linux, and ezllm already running in Docker
([quick start](../../README.md#quick-start)). Run every command on that computer, as
**yourself, not root**.

## 1. Install

```sh
curl -fsSL https://claude.ai/install.sh | bash
```

Close the terminal and open a new one.

## 2. Copy

The last line prints the version.

```sh
CLI=$(readlink -f "$(command -v claude)")
V=$("$CLI" --version | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)
DIR=~/.local/share/claude-bridge
mkdir -p "$DIR" ~/.claude-sub && chmod 700 ~/.claude-sub
[ -e "$DIR/$V" ] || { mkdir "$DIR/$V" && cp "$CLI" "$DIR/$V/claude" && chmod -R a-w "$DIR/$V"; }
ln -sfn "$V" "$DIR/current"
"$DIR/current/claude" --version
```

## 3. Sign in

```sh
CLAUDE_CONFIG_DIR=~/.claude-sub ~/.local/share/claude-bridge/current/claude auth login
```

## 4. Switch it on

Pick the way you started ezllm: **[Docker Compose](#with-docker-compose)** (you have a
`compose.yml`) or **[`docker run`](#with-docker-run)** (you do not).

### With Docker Compose

In `compose.yml`, find this line and change `"off"` to `"on"`. If it is not there, add
it under `environment:`. Never have it twice.

```yaml
      EZLLM_CLAUDE_BRIDGE: "on"
```

Also under `environment:`, add (your login name; `whoami` prints it):

```yaml
      USER: you
      LOGNAME: you
```

Under `volumes:`, add these. Change `/home/you` to your home folder (`echo $HOME`
prints it).

```yaml
      - /home/you/.local/share/claude-bridge:/opt/claude-bridge:ro
      - /home/you/.claude-sub:/claude-sub
```

`id -u` and `id -g` should both print `1000`. If not, put your numbers in a file named
`.env` next to `compose.yml`: one line `UID=1001`, one line `GID=1001`.

Start it:

```sh
docker compose up -d --force-recreate ezllm
docker compose logs ezllm | grep -i "claude bridge"
```

You should see `claude bridge listening`, with `login=logged-in` on the same line. Go to
[step 5](#5-add-it-in-the-dashboard).

### With docker run

You stop the container and start a new one. Your settings, keys and history are in the
`data` folder, so the new one has them all.

Go to the folder with `config.yaml` and `data` in it. If yours is not `~/ezllm`, change
the first line. Stay in this terminal until the end.

```sh
cd ~/ezllm
ls -d config.yaml data
BRIDGES=()
```

Paste this. If another bridge is already on, paste the same block from its page too, or
it will be off.

```sh
BRIDGES+=(
  -e EZLLM_CLAUDE_BRIDGE=on -e USER="$(whoami)" -e LOGNAME="$(whoami)"
  -v "$HOME/.local/share/claude-bridge:/opt/claude-bridge:ro"
  -v "$HOME/.claude-sub:/claude-sub"
)
```

Set the old container aside:

```sh
docker stop ezllm
docker rename ezllm ezllm-old
```

Start the new one. If you started ezllm with options of your own, add them here too.

```sh
docker run -d --name ezllm --user "$(id -u):$(id -g)" \
  -p 127.0.0.1:20129:20129 -v "$(pwd)/data:/data" \
  -v "$(pwd)/config.yaml:/app/config.yaml:ro" \
  -e EZLLM_MASTER_KEY_FILE=/data/ezllm-master.key \
  "${BRIDGES[@]}" \
  "$(docker inspect -f '{{.Config.Image}}' ezllm-old)"
sleep 10
docker logs ezllm 2>&1 | grep -i "bridge"
```

You should see `claude bridge listening`, with `login=logged-in` on the same line. If
so, remove the old one with `docker rm ezllm-old`. If not,
[go back to the old one](troubleshooting.md#get-ezllm-back).

## 5. Add it in the dashboard

Open the dashboard → **Providers** → **Add provider**, pick **Claude Pro/Max (bridge)**, then **Save &
validate key**. There is no key to paste. The check sends a small message and uses a
little of your plan.

`release="unqualified …"` in the log is fine. It means nobody has recorded a live test
of that exact CLI version.

---

**Not working?** [Troubleshooting](troubleshooting.md) ·
**How it works, limits and caps:** [reference](../claude-bridge-runbook.md)
