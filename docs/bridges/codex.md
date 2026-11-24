# Turn on ChatGPT (Codex)

You need Linux, and ezllm already running ([quick start](../../README.md#quick-start)).
Run every command on that computer, as **yourself, not root**.

## 1. Install

Use this installer, not npm: npm gives you a script the container
cannot run.

```sh
curl -fsSL https://chatgpt.com/codex/install.sh | sh
```

## 2. Copy

It copies the whole folder. The last line prints the version.

```sh
REL=$(readlink -f ~/.codex/packages/standalone/current)
V=$("$REL/bin/codex" --version | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)
DIR=~/.local/share/codex-bridge
mkdir -p "$DIR" ~/.codex-sub && chmod 700 ~/.codex-sub
[ -e "$DIR/$V" ] || { cp -a "$REL" "$DIR/$V" && chmod -R a-w "$DIR/$V"; }
ln -sfn "$V" "$DIR/current"
"$DIR/current/bin/codex" --version
```

## 3. Sign in

It prints a web address and a code: open the address, enter the code.

```sh
CODEX_HOME=~/.codex-sub ~/.local/share/codex-bridge/current/bin/codex login --device-auth
```

Sign in fresh, as above. Never copy your own `~/.codex/auth.json`: the two copies log
each other out.

## 4. Switch it on

Pick the way you run ezllm:

- **[Docker Compose](#with-docker-compose)**: you have a `compose.yml`.
- **[`docker run`](#with-docker-run)**: Docker, without a `compose.yml`.
- **[The ezllm binary](#with-the-ezllm-binary)**: no Docker.

### With Docker Compose

In `compose.yml`, find this line and change `"off"` to `"on"`. If it is not there, add
it under `environment:`. Never have it twice.

```yaml
      EZLLM_CODEX_BRIDGE: "on"
```

Under `volumes:`, add these. Change `/home/you` to your home folder (`echo $HOME`
prints it).

```yaml
      - /home/you/.local/share/codex-bridge:/opt/codex-bridge:ro
      - /home/you/.codex-sub:/codex-sub
```

`id -u` and `id -g` should both print `1000`. If not, put your numbers in a file named
`.env` next to `compose.yml`: one line `UID=1001`, one line `GID=1001`.

Start it:

```sh
docker compose up -d --force-recreate ezllm
docker compose logs ezllm | grep -i "codex bridge"
```

You should see `codex bridge listening`. Go to [step 5](#5-add-it-in-the-dashboard).

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
  -e EZLLM_CODEX_BRIDGE=on
  -v "$HOME/.local/share/codex-bridge:/opt/codex-bridge:ro"
  -v "$HOME/.codex-sub:/codex-sub"
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

You should see `codex bridge listening`. If so, remove the old one with `docker rm
ezllm-old`. If not,
[go back to the old one](troubleshooting.md#get-ezllm-back).

### With the ezllm binary

This is for the [systemd user service](../install-binary.md#linux--systemd-user-unit)
with ezllm in `~/ezllm`. If yours is elsewhere, change `%h/ezllm` (`%h` is your home
folder). Paste it all:

```sh
mkdir -p ~/.config/systemd/user/ezllm.service.d
cat > ~/.config/systemd/user/ezllm.service.d/codex-bridge.conf <<'EOF'
[Service]
Environment=EZLLM_CODEX_BRIDGE=on
Environment=EZLLM_CODEX_BIN=%h/.local/share/codex-bridge/current/bin/codex
Environment=EZLLM_CODEX_HOME=%h/.codex-sub
Environment=EZLLM_CODEX_BRIDGE_TOKEN_FILE=%h/ezllm/data/codex-bridge.token
EOF
systemctl --user daemon-reload
systemctl --user restart ezllm
sleep 10
journalctl --user -u ezllm --since -1min | grep -i "codex bridge"
```

You should see `codex bridge listening`.

Starting ezllm by hand instead? Run this first, in the same terminal, then start ezllm as
usual.

```sh
export EZLLM_CODEX_BRIDGE=on
export EZLLM_CODEX_BIN="$HOME/.local/share/codex-bridge/current/bin/codex"
export EZLLM_CODEX_HOME="$HOME/.codex-sub"
export EZLLM_CODEX_BRIDGE_TOKEN_FILE="$HOME/ezllm/data/codex-bridge.token"
```

## 5. Add it in the dashboard

Open the dashboard → **Providers** → **Add provider**, pick **ChatGPT Plus/Pro (bridge)**, then **Save &
validate key**. There is no key to paste. The check sends a small message and uses a
little of your plan.

`release="unqualified …"` in the log is fine. It means nobody has recorded a live test
of that exact CLI version.

---

**Not working?** [Troubleshooting](troubleshooting.md) ·
**How it works, limits and caps:** [reference](../codex-bridge-runbook.md)
