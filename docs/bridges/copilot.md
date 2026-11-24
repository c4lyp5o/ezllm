# Turn on GitHub Copilot

You need Linux, and ezllm already running ([quick start](../../README.md#quick-start)).
Run every command on that computer, as **yourself, not root**.

## 1 and 2. Install and copy

One block does both. It leaves any Copilot you already
use alone. On an ARM computer, write `ASSET=copilot-linux-arm64.tar.gz`.

```sh
V=1.0.114
ASSET=copilot-linux-x64.tar.gz
URL="https://github.com/github/copilot-cli/releases/download/v$V"
DIR=~/.local/share/copilot-bridge
mkdir -p "$DIR/$V" ~/.copilot-sub && chmod 700 ~/.copilot-sub
cd "$(mktemp -d)"
curl -fLO "$URL/$ASSET" && curl -fLO "$URL/SHA256SUMS.txt" &&
  sha256sum -c --ignore-missing SHA256SUMS.txt &&
  tar xzf "$ASSET" -C "$DIR/$V" &&
  ln -sfn "$V" "$DIR/current"
"$DIR/current/copilot" --no-auto-update --version
```

## 3. Sign in

It prints a web address and a code: open the address, enter the code.

```sh
env -u DBUS_SESSION_BUS_ADDRESS COPILOT_HOME=~/.copilot-sub \
  ~/.local/share/copilot-bridge/current/copilot login --device-code
```

## 4. Switch it on

Pick the way you run ezllm:

- **[Docker Compose](#with-docker-compose)**: you have a `compose.yml`.
- **[`docker run`](#with-docker-run)**: Docker, without a `compose.yml`.
- **[The ezllm binary](#with-the-ezllm-binary)**: no Docker.

### With Docker Compose

In `compose.yml`, find this line and change `"off"` to `"on"`. If it is not there, add
it under `environment:`. Never have it twice.

```yaml
      EZLLM_COPILOT_BRIDGE: "on"
```

Also under `environment:`, add:

```yaml
      XDG_CACHE_HOME: /data/copilot-cache
```

Under `volumes:`, add these. Change `/home/you` to your home folder (`echo $HOME`
prints it).

```yaml
      - /home/you/.local/share/copilot-bridge:/opt/copilot-bridge:ro
      - /home/you/.copilot-sub:/copilot-sub
```

`id -u` and `id -g` should both print `1000`. If not, put your numbers in a file named
`.env` next to `compose.yml`: one line `UID=1001`, one line `GID=1001`.

Start it:

```sh
docker compose up -d --force-recreate ezllm
docker compose logs ezllm | grep -i "copilot bridge"
```

You should see `copilot bridge listening`, and **no** line saying `channel unavailable`.
Go to [step 5](#5-add-it-in-the-dashboard).

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
  -e EZLLM_COPILOT_BRIDGE=on -e XDG_CACHE_HOME=/data/copilot-cache
  -v "$HOME/.local/share/copilot-bridge:/opt/copilot-bridge:ro"
  -v "$HOME/.copilot-sub:/copilot-sub"
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

You should see `copilot bridge listening`, and **no** line saying `channel unavailable`.
If so, remove the old one with `docker rm ezllm-old`. If not,
[go back to the old one](troubleshooting.md#get-ezllm-back).

### With the ezllm binary

This is for the [systemd user service](../install-binary.md#linux--systemd-user-unit)
with ezllm in `~/ezllm`. If yours is elsewhere, change `%h/ezllm` (`%h` is your home
folder). Paste it all:

```sh
mkdir -p ~/.config/systemd/user/ezllm.service.d
cat > ~/.config/systemd/user/ezllm.service.d/copilot-bridge.conf <<'EOF'
[Service]
Environment=EZLLM_COPILOT_BRIDGE=on
Environment=EZLLM_COPILOT_BIN=%h/.local/share/copilot-bridge/current/copilot
Environment=EZLLM_COPILOT_HOME=%h/.copilot-sub
Environment=EZLLM_COPILOT_BRIDGE_TOKEN_FILE=%h/ezllm/data/copilot-bridge.token
Environment=XDG_CACHE_HOME=%h/ezllm/data/copilot-cache
EOF
systemctl --user daemon-reload
systemctl --user restart ezllm
sleep 10
journalctl --user -u ezllm --since -1min | grep -i "copilot bridge"
```

You should see `copilot bridge listening`, and **no** line saying `channel unavailable`.

Starting ezllm by hand instead? Run this first, in the same terminal, then start ezllm as
usual.

```sh
export EZLLM_COPILOT_BRIDGE=on
export EZLLM_COPILOT_BIN="$HOME/.local/share/copilot-bridge/current/copilot"
export EZLLM_COPILOT_HOME="$HOME/.copilot-sub"
export EZLLM_COPILOT_BRIDGE_TOKEN_FILE="$HOME/ezllm/data/copilot-bridge.token"
export XDG_CACHE_HOME="$HOME/ezllm/data/copilot-cache"
```

## 5. Add it in the dashboard

Open the dashboard → **Providers** → **Add provider**, pick **GitHub Copilot (bridge)**, then **Save &
validate key**. There is no key to paste. The check sends a small message and uses a
little of your plan.

`release="unqualified …"` in the log is fine. It means nobody has recorded a live test
of that exact CLI version.

---

**Not working?** [Troubleshooting](troubleshooting.md) ·
**How it works, limits and caps:** [reference](../copilot-bridge-runbook.md)
