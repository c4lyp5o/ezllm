# Turn on Google Antigravity

You need Linux, and ezllm already running in Docker
([quick start](../../README.md#quick-start)). Run every command on that computer, as
**yourself, not root**.

## 1. Install

```sh
curl -fsSL https://antigravity.google/cli/install.sh | bash
```

Close the terminal and open a new one.

## 2. Copy

The last line prints the version.

```sh
CLI=$(readlink -f "$(command -v agy)")
V=$("$CLI" --version | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)
DIR=~/.local/share/agy-bridge
mkdir -p "$DIR" ~/.agy-sub && chmod 700 ~/.agy-sub
[ -e "$DIR/$V" ] || { mkdir "$DIR/$V" && cp "$CLI" "$DIR/$V/agy" && chmod -R a-w "$DIR/$V"; }
ln -sfn "$V" "$DIR/current"
"$DIR/current/agy" --version
```

## 3. Sign in

This opens the app. Sign in, send any message, then type `/exit`.

```sh
env -u DBUS_SESSION_BUS_ADDRESS HOME=$HOME/.agy-sub ~/.local/share/agy-bridge/current/agy
```

If it says your account is *not eligible*, open the link it prints, verify the
account, and send a message again.

## 4. Switch it on

Pick the way you started ezllm: **[Docker Compose](#with-docker-compose)** (you have a
`compose.yml`) or **[`docker run`](#with-docker-run)** (you do not).

### With Docker Compose

In `compose.yml`, find this line and change `"off"` to `"on"`. If it is not there, add
it under `environment:`. Never have it twice.

```yaml
      EZLLM_AGY_BRIDGE: "on"
```

Under `volumes:`, add these. Change `/home/you` to your home folder (`echo $HOME`
prints it).

```yaml
      - /home/you/.local/share/agy-bridge:/opt/agy-bridge:ro
      - /home/you/.agy-sub:/agy-sub
```

`id -u` and `id -g` should both print `1000`. If not, put your numbers in a file named
`.env` next to `compose.yml`: one line `UID=1001`, one line `GID=1001`.

Start it:

```sh
docker compose up -d --force-recreate ezllm
docker compose logs ezllm | grep -i "agy bridge"
```

You should see `agy bridge listening`. Go to [step 5](#5-add-it-in-the-dashboard).

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
  -e EZLLM_AGY_BRIDGE=on
  -v "$HOME/.local/share/agy-bridge:/opt/agy-bridge:ro"
  -v "$HOME/.agy-sub:/agy-sub"
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

You should see `agy bridge listening`. If so, remove the old one with `docker rm
ezllm-old`. If not,
[go back to the old one](troubleshooting.md#get-ezllm-back).

## 5. Add it in the dashboard

Open the dashboard → **Providers** → **Add provider**, pick **Google Antigravity (bridge)**, then **Save &
validate key**. There is no key to paste. The check sends a small message and uses a
little of your plan.

`release="unqualified …"` in the log is fine. It means nobody has recorded a live test
of that exact CLI version.

---

**Not working?** [Troubleshooting](troubleshooting.md) ·
**How it works, limits and caps:** [reference](../agy-bridge-runbook.md)
