# Install the ezllm binary

ezllm is one file. You need no Docker, no Go and nothing else installed. Run every
command as **yourself, not root**.

Pick your system:

- **[Linux](#linux)**
- **[macOS](#macos)**
- **[Windows](#windows)**

Everything goes in one folder, `ezllm` in your home folder: `bin` is the program, `data`
is your settings, keys and history, and `config.yaml` is the configuration.

## Linux

### 1. Install on Linux

This gets the latest release, checks it and unpacks it. It keeps a `config.yaml` you
already have.

```sh
VER=$(curl -fsSLI -o /dev/null -w '%{url_effective}' https://github.com/c4lyp5o/ezllm/releases/latest | sed 's|.*/v||')
ARCH=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
NAME=ezllm_v${VER}_linux_$ARCH
URL=https://github.com/c4lyp5o/ezllm/releases/download/v$VER
mkdir -p ~/ezllm/bin ~/ezllm/data && chmod 700 ~/ezllm/data && cd ~/ezllm
curl -fsSLO "$URL/$NAME.tar.gz" && curl -fsSLO "$URL/SHA256SUMS" &&
  grep " $NAME.tar.gz\$" SHA256SUMS | sha256sum -c - &&
  tar xzf "$NAME.tar.gz" &&
  install -m 0755 "$NAME/ezllm" bin/ezllm &&
  { [ -e config.yaml ] || cp "$NAME/config.example.yaml" config.yaml; } &&
  rm -r "$NAME" "$NAME.tar.gz" SHA256SUMS
```

You should see a line ending in `OK`.

### 2. Start on Linux

```sh
cd ~/ezllm
EZLLM_MASTER_KEY_FILE=$HOME/ezllm/data/ezllm-master.key ./bin/ezllm -config config.yaml
```

You should see a `setup code`. Open [http://127.0.0.1:20129](http://127.0.0.1:20129),
enter the code and choose a password.

Press `Ctrl-C` to stop it. To leave it on, do the next step.

### Keep it running on Linux

Stop the one you started by hand first. This makes ezllm a service that starts by itself
and comes back after a crash:

```sh
mkdir -p ~/.config/systemd/user
cat > ~/.config/systemd/user/ezllm.service <<'UNIT'
[Unit]
Description=ezllm gateway

[Service]
WorkingDirectory=%h/ezllm
ExecStart=%h/ezllm/bin/ezllm -config %h/ezllm/config.yaml
Environment=EZLLM_MASTER_KEY_FILE=%h/ezllm/data/ezllm-master.key
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
UNIT
systemctl --user daemon-reload
systemctl --user enable --now ezllm
sleep 5
curl -s http://127.0.0.1:20129/healthz
```

You should see `"status":"ok"`.

A service of yours stops when you log out. This keeps it on:

```sh
sudo loginctl enable-linger "$USER"
```

Read the log with `journalctl --user -u ezllm -f`.

### Update on Linux

Read [upgrading](upgrading.md) for what changes. Then stop ezllm and copy its folder. The
copy holds your master key, so keep it private.

```sh
systemctl --user stop ezllm
cp -a ~/ezllm ~/ezllm-backup-$(date +%Y%m%d-%H%M)
```

Do [step 1](#1-install-on-linux) again, then start it:

```sh
systemctl --user start ezllm
sleep 5
curl -s http://127.0.0.1:20129/healthz
```

You should see the new version. If not, [go back](#go-back-to-the-old-version).

## macOS

### 1. Install on macOS

This gets the install script, then the latest release, and starts it. The script checks
the download.

```sh
mkdir -p ~/ezllm && cd ~/ezllm
curl -fsSLO https://raw.githubusercontent.com/c4lyp5o/ezllm/master/scripts/ezllm-mac.sh
bash ezllm-mac.sh install && bash ezllm-mac.sh start
```

You should see `"status":"ok"`. macOS now starts ezllm each time you log in, and stops it
when you log out. ezllm does not answer while the Mac sleeps.

### 2. Sign in on macOS

Get the setup code:

```sh
grep "setup code" ~/ezllm/logs/ezllm.log | tail -2
```

Open [http://127.0.0.1:20129](http://127.0.0.1:20129), enter the code and choose a
password.

Other commands: `bash ezllm-mac.sh status`, `logs`, `stop`, `restart` and `uninstall`.

### Update on macOS

Read [upgrading](upgrading.md) for what changes. Then stop ezllm and copy its folder. The
copy holds your master key, so keep it private.

```sh
cd ~/ezllm
bash ezllm-mac.sh stop
cp -a ~/ezllm ~/ezllm-backup-$(date +%Y%m%d-%H%M) &&
  curl -fsSLO https://raw.githubusercontent.com/c4lyp5o/ezllm/master/scripts/ezllm-mac.sh &&
  bash ezllm-mac.sh install && bash ezllm-mac.sh start
```

You should see the new version. If not, [go back](#go-back-to-the-old-version).

## Windows

You need 64-bit Windows 10 or 11.

### 1. Install on Windows

Open **PowerShell** and paste this. It gets the latest release, checks it and unpacks it.
It keeps a `config.yaml` you already have.

```powershell
$Ver = (curl.exe -fsSLI -o NUL -w '%{url_effective}' https://github.com/c4lyp5o/ezllm/releases/latest) -replace '.*/v', ''
$Name = "ezllm_v${Ver}_windows_amd64"
$Url = "https://github.com/c4lyp5o/ezllm/releases/download/v$Ver"
New-Item -ItemType Directory -Force "$HOME\ezllm\bin", "$HOME\ezllm\data", "$HOME\ezllm\logs" | Out-Null
cd "$HOME\ezllm"
curl.exe -fsSLO "$Url/$Name.zip"
curl.exe -fsSLO "$Url/SHA256SUMS"
$Want = (Select-String -SimpleMatch "$Name.zip" SHA256SUMS).Line.Split()[0]
if ((Get-FileHash "$Name.zip").Hash -eq $Want) {
  Expand-Archive "$Name.zip" . -Force
  if (-not (Test-Path config.yaml)) { Copy-Item "$Name\config.example.yaml" config.yaml }
  Copy-Item "$Name\ezllm.exe" bin\ezllm.exe -Force
  if ($?) { "Installed ezllm $Ver" } else { "Not installed. Stop ezllm, then paste this again." }
  Remove-Item -Recurse $Name, "$Name.zip", SHA256SUMS
} else { "The checksum does not match. Nothing was installed." }
```

You should see `Installed ezllm`.

### 2. Start on Windows

```powershell
cd "$HOME\ezllm"
$env:EZLLM_MASTER_KEY_FILE = "$HOME\ezllm\data\ezllm-master.key"
.\bin\ezllm.exe -config config.yaml
```

You should see a `setup code`. Open [http://127.0.0.1:20129](http://127.0.0.1:20129),
enter the code and choose a password.

Press `Ctrl-C` to stop it. To leave it on, do the next step.

### Keep it running on Windows

Stop the one you started by hand first. Open **PowerShell as administrator**, signed in as
the same user, and paste this. It starts ezllm each time you log in.

```powershell
@'
@echo off
cd /d "%USERPROFILE%\ezllm"
set "EZLLM_MASTER_KEY_FILE=%USERPROFILE%\ezllm\data\ezllm-master.key"
bin\ezllm.exe -config config.yaml >> logs\ezllm.log 2>&1
'@ | Set-Content -Encoding ascii "$HOME\ezllm\ezllm-run.cmd"
$Action = New-ScheduledTaskAction -Execute "$HOME\ezllm\ezllm-run.cmd"
$Trigger = New-ScheduledTaskTrigger -AtLogOn -User (whoami)
$Settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
Register-ScheduledTask -TaskName ezllm -Action $Action -Trigger $Trigger -Settings $Settings -Force | Out-Null
Start-ScheduledTask ezllm
Start-Sleep 5
curl.exe -s http://127.0.0.1:20129/healthz
```

You should see `"status":"ok"`. A black window opens with it. Closing that window stops
ezllm. So does `Stop-Process -Name ezllm`.

Read the log with `Get-Content "$HOME\ezllm\logs\ezllm.log" -Tail 20 -Wait`.

### Update on Windows

Read [upgrading](upgrading.md) for what changes. Then stop ezllm and copy its folder. The
copy holds your master key, so keep it private.

```powershell
Stop-Process -Name ezllm
Start-Sleep 3
Copy-Item -Recurse "$HOME\ezllm" "$HOME\ezllm-backup-$(Get-Date -Format yyyyMMdd-HHmm)"
```

Do [step 1](#1-install-on-windows) again, then start it:

```powershell
Start-ScheduledTask ezllm
Start-Sleep 5
curl.exe -s http://127.0.0.1:20129/healthz
```

You should see the new version. If not, [go back](#go-back-to-the-old-version).

## Go back to the old version

An old ezllm cannot open data a newer one has changed, so the program and the data go back
together. Anything that happened since the copy is lost.

1. Stop ezllm: `systemctl --user stop ezllm`, `bash ezllm-mac.sh stop` or
   `Stop-Process -Name ezllm`.
2. Rename the `ezllm` folder to `ezllm-new`.
3. Rename your `ezllm-backup-…` folder to `ezllm`.
4. Start ezllm: `systemctl --user start ezllm`, `bash ezllm-mac.sh start` or
   `Start-ScheduledTask ezllm`.

## Not working?

| You see | Do this |
|---|---|
| `read config.yaml: open config.yaml` | Start it from the `ezllm` folder |
| `address already in use`, or `Only one usage of each socket address` | ezllm is already running, or another program has the port. Stop it, or change `listen` in `config.yaml` |
| No setup code, and the page asks for a password | Setup is already done. Sign in with your password |
| The service stops when you log out (Linux) | Run `sudo loginctl enable-linger "$USER"` |
| `Killed: 9` (macOS) | The file was changed or cut short. Install it again |
| `cannot be opened because the developer cannot be verified` (macOS) | Run `xattr -d com.apple.quarantine ~/ezllm/bin/ezllm` |
| `Windows protected your PC` | Run `Unblock-File "$HOME\ezllm\bin\ezllm.exe"` |
| `The process cannot access the file` (Windows) | ezllm is still running. Run `Stop-Process -Name ezllm`, then try again |
| `StartService FAILED 1053` (Windows) | ezllm is not a Windows service. [Use Task Scheduler](#keep-it-running-on-windows) |
| A log line that names a bridge (macOS, Windows) | Turn that bridge `off`. Codex and Antigravity run on Linux only. On Windows, [use WSL](wsl.md) |
| `Install Command Line Developer Tools` opens (macOS) | A bridge CLI asked for Apple's tools. ezllm does not need them: press **Cancel** |
| ezllm stops answering after a while (macOS) | The Mac went to sleep. Wake it, or keep it awake in **System Settings** |

The log says why ezllm did not start.

---

## Reference

### What a release has

| System | File |
|---|---|
| Linux x86-64 | `ezllm_v<version>_linux_amd64.tar.gz` |
| Linux arm64 | `ezllm_v<version>_linux_arm64.tar.gz` |
| macOS Apple Silicon | `ezllm_v<version>_darwin_arm64.tar.gz` |
| macOS Intel | `ezllm_v<version>_darwin_amd64.tar.gz` |
| Windows x86-64 | `ezllm_v<version>_windows_amd64.zip` |

Each has the program, `config.example.yaml` and `README.md`. `SHA256SUMS` covers them all.
There is no 32-bit build. Windows on ARM runs the x86-64 one. The Windows and
macOS Intel programs are not signed. The Apple Silicon one is signed ad hoc, not notarized.
The downloads above use `curl`, so neither system blocks them.

[Subscription bridges](bridge-setup.md) work with the Linux binary. On Windows, use
[WSL](wsl.md). On macOS (Intel), a Copilot request worked; Claude starts but
cannot read its login. The bridge pages have no macOS steps.

### Settings

ezllm needs a `config.yaml`. `-config` names it, from the folder you are in. These change
where ezllm listens and keeps its data:

| What | `config.yaml` | Environment |
|---|---|---|
| Address | `listen: "127.0.0.1:20129"` | `EZLLM_ADDR`, or the `-addr` flag |
| Data folder | `data_dir: ./data` | `EZLLM_DATA_DIR` |
| Master key | none | `EZLLM_MASTER_KEY_FILE` |

Always set `EZLLM_MASTER_KEY_FILE`, and keep the key with the data. Without it your stored
provider keys cannot be read ([master key](master-key-runbook.md)). On Windows, use `/`
in `config.yaml` paths. Every setting: [environment](environment.md).

Each provider you uncomment in `config.yaml` is added at start, without a key. It logs
`model sync failed at boot` until you add one. Removing it from the file later does not
remove it from ezllm.

To copy the data while ezllm is running, use an
[online backup](recovery-runbook.md#verified-databasekey-backup). A plain copy of a running
database can miss recent changes.

### Reach it from other computers

Set `listen: "0.0.0.0:20129"` in `config.yaml`, restart, and open port 20129 in your
firewall. Use a long dashboard password: anyone who reaches the port can try to sign in.
Put a rate limit on `/admin/login` and `/admin/setup` in your
[reverse proxy](../README.md#configuration).

macOS asks to allow the connection. On Windows, run this as administrator:

```powershell
New-NetFirewallRule -DisplayName ezllm -Direction Inbound -Protocol TCP -LocalPort 20129 -Action Allow -Profile Private
```

### A Linux server where nobody logs in

This runs ezllm as its own user, from `/opt/ezllm`. Do [step 1](#1-install-on-linux) first.

```sh
sudo useradd --system --home /opt/ezllm --shell /usr/sbin/nologin ezllm
sudo install -d -o ezllm -g ezllm /opt/ezllm /opt/ezllm/bin
sudo install -d -m 0700 -o ezllm -g ezllm /opt/ezllm/data
sudo install -m 0755 ~/ezllm/bin/ezllm /opt/ezllm/bin/ezllm
sudo install -m 0644 ~/ezllm/config.yaml /opt/ezllm/config.yaml
sudo tee /etc/systemd/system/ezllm.service >/dev/null <<'UNIT'
[Unit]
Description=ezllm gateway
After=network-online.target
Wants=network-online.target

[Service]
User=ezllm
Group=ezllm
WorkingDirectory=/opt/ezllm
ExecStart=/opt/ezllm/bin/ezllm -config /opt/ezllm/config.yaml
Environment=EZLLM_MASTER_KEY_FILE=/opt/ezllm/data/ezllm-master.key
Restart=on-failure
RestartSec=10
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/opt/ezllm/data
PrivateTmp=true

[Install]
WantedBy=multi-user.target
UNIT
sudo systemctl daemon-reload
sudo systemctl enable --now ezllm
sleep 5
curl -s http://127.0.0.1:20129/healthz
```

You should see `"status":"ok"`. Read the log with `sudo journalctl -u ezllm -f`.

### Try it without keeping anything

This runs a second ezllm on another port, in a temporary folder. Press `Ctrl-C` when you
are done.

```sh
D=$(mktemp -d)
EZLLM_ADDR=127.0.0.1:20188 EZLLM_DATA_DIR=$D/data EZLLM_MASTER_KEY_FILE=$D/master.key ~/ezllm/bin/ezllm -config ~/ezllm/config.yaml
```

Then delete it:

```sh
rm -r "$D"
```

To try a new version on your own data first, copy the whole `data` folder while ezllm is
stopped, and point `EZLLM_DATA_DIR` and `EZLLM_MASTER_KEY_FILE` at the copy. Never run two
ezllms on the same data.

### Service notes

- **macOS:** a launch agent runs while you are logged in. For a Mac where nobody logs in,
  turn the file `~/Library/LaunchAgents/com.ezllm.gateway.plist` into a launch daemon.
- **macOS:** `EZLLM_VERSION`, `EZLLM_PREFIX`, `EZLLM_ADDR`, `EZLLM_USAGE_TZ` and
  `EZLLM_LOG_LEVEL` change what `ezllm-mac.sh install` sets up.
- **Windows:** Task Scheduler ends a task after three days unless told otherwise. The step
  above turns that limit off.
- **Windows:** stop ezllm with `Stop-Process -Name ezllm`. `Stop-ScheduledTask` ends only
  the window around it and leaves ezllm running. The database recovers at the next start.
