# Running the released binary

`release.yml` publishes a standalone binary for every `v*` tag. This is the
path for a host without Docker and without a Go toolchain: no libc, no
runtime, no package manager. The dashboard SPA is embedded in the binary.

For the container path see the README; for every environment variable and its
default see [environment.md](environment.md). The Windows, Linux and macOS
service shapes below all run the same binary — only the supervisor differs.

## What a release contains

| Platform | Asset | Archive | Binary |
|---|---|---|---|
| Linux x86-64 | `ezllm_v<ver>_linux_amd64.tar.gz` | tar.gz | `ezllm` |
| Linux arm64 | `ezllm_v<ver>_linux_arm64.tar.gz` | tar.gz | `ezllm` |
| macOS Apple Silicon | `ezllm_v<ver>_darwin_arm64.tar.gz` | tar.gz | `ezllm` |
| macOS Intel | `ezllm_v<ver>_darwin_amd64.tar.gz` | tar.gz | `ezllm` |
| Windows x86-64 | `ezllm_v<ver>_windows_amd64.zip` | zip | `ezllm.exe` |

Each archive holds the binary, `config.example.yaml` and `README.md`.
`SHA256SUMS` covers every archive. Sizes are 17–18.5 MB depending on platform.

The build is `CGO_ENABLED=0` because SQLite is pure Go (`modernc.org/sqlite`),
so the binary has no shared-library dependencies at all: it runs on glibc and
musl distributions alike, and on Windows with no redistributable installed.

### What differs per platform

| | Linux | macOS | Windows |
|---|---|---|---|
| Service manager | systemd (user or system unit) | launchd agent | Task Scheduler |
| Installer script | — | `scripts/ezllm-mac.sh` | — |
| Log target | journald / stdout | file via launchd | file via redirection |
| Subscription bridges | supported | Codex/Antigravity refused | Codex/Antigravity refused |
| Code signing | not required | arm64 needs ≥ ad-hoc | not required |

The Claude, Codex and Antigravity bridges are **Linux-only** features: the
Codex and Antigravity bridges refuse to start off Linux with a boot error, and
the Claude bridge needs the vendor CLI plus a subscription login. Leave
`EZLLM_CLAUDE_BRIDGE`, `EZLLM_CODEX_BRIDGE` and `EZLLM_AGY_BRIDGE` at their
default `off` on macOS and Windows — otherwise the boot fails.

## 1. Install

### Linux and macOS

```bash
VER=1.3.2                    # or any tag from the releases page
OS=$(uname -s | tr 'A-Z' 'a-z')
case "$(uname -m)" in
  x86_64) ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) echo "unsupported CPU: $(uname -m)" >&2; exit 1 ;;
esac
BASE=https://github.com/c4lyp5o/ezllm/releases/download/v$VER
NAME=ezllm_v${VER}_${OS}_${ARCH}

curl -fsSLO "$BASE/$NAME.tar.gz"
curl -fsSLO "$BASE/SHA256SUMS"
# SHA256SUMS lists every platform, so verify only the archive you downloaded.
grep " $NAME.tar.gz\$" SHA256SUMS | sha256sum -c -          # Linux
grep " $NAME.tar.gz\$" SHA256SUMS | shasum -a 256 -c -     # macOS
tar xzf "$NAME.tar.gz"

mkdir -p ~/ezllm/bin ~/ezllm/data ~/ezllm/logs
install -m 0755 "$NAME/ezllm" ~/ezllm/bin/ezllm
cp "$NAME/config.example.yaml" ~/ezllm/config.yaml      # required, see below
```

`uname -m` reports `aarch64` on Linux arm64 and `arm64` on macOS; both take the
`arm64` asset. On a Raspberry Pi or an ARM server use `linux_arm64`; there is no
32-bit or armv7 build.

Do not run the binary as root. It binds port 20129 (above 1024), so no
capability is needed.

### Windows (PowerShell)

```powershell
$Ver  = "1.3.2"
$Name = "ezllm_v${Ver}_windows_amd64"
$Base = "https://github.com/c4lyp5o/ezllm/releases/download/v$Ver"

$Dir = "C:\ezllm"
New-Item -ItemType Directory -Force -Path "$Dir\bin", "$Dir\data", "$Dir\logs" | Out-Null

Invoke-WebRequest "$Base/$Name.zip"   -OutFile "$Name.zip"
Invoke-WebRequest "$Base/SHA256SUMS"  -OutFile "SHA256SUMS"

# SHA256SUMS lists every platform; compare only the archive you downloaded.
$want = (Select-String -Path .\SHA256SUMS -Pattern " $Name\.zip$").Line.Split()[0]
$got  = (Get-FileHash ".\$Name.zip" -Algorithm SHA256).Hash.ToLower()
if ($got -ne $want) { throw "checksum mismatch: expected $want, got $got" }

Expand-Archive ".\$Name.zip" -DestinationPath . -Force
Copy-Item ".\$Name\ezllm.exe" "$Dir\bin\ezllm.exe"
Copy-Item ".\$Name\config.example.yaml" "$Dir\config.yaml"
# Downloaded from the internet: this removes the mark-of-the-web so the
# binary runs without a SmartScreen prompt.
Unblock-File "$Dir\bin\ezllm.exe"
```

Windows has no `arm64` asset: it is x86-64 (PE32+ console) only, which also runs
under emulation on ARM64 Windows.

The executable carries **no Authenticode signature** (the release pipeline has no
signing key), which is what triggers SmartScreen on a browser download;
`Unblock-File` clears the mark-of-the-web. Execution does not require a
signature on Windows the way it does on Apple Silicon — a verified run of
v1.3.2 reports `SignatureStatus: NotSigned` and starts normally.

## 2. `config.yaml` is mandatory

There is no config-less mode: `config.Load` reads the file and fails boot on a
missing one.

```
ezllm: config: read config.yaml: open config.yaml: no such file or directory
```

Two flags, both optional:

- **`-config <path>`** (default `config.yaml`) — resolved against the **working
  directory**, not against the binary, so either run from the install directory
  or pass an absolute path (`-config /home/me/ezllm/config.yaml`,
  `-config C:\ezllm\config.yaml`).
- **`-addr <host:port>`** — overrides `EZLLM_ADDR`, which overrides
  `config.listen`.

On Windows a YAML value with backslashes needs quoting or forward slashes
(`data_dir: "C:/ezllm/data"`); the shipped example uses a relative `./data`,
which resolves against the working directory the launcher sets.

The shipped `config.example.yaml` is bootstrap metadata only — provider
credentials live in SQLite and are added from the dashboard.

**It seeds whatever `providers:` contains.** Every entry is upserted into SQLite
at boot, *enabled and with no key*, so an uncommented example appears in a fresh
instance's provider list and no model is exposed until a key is added — the
account is cosmetic until then, not a cost or security problem. That is also why
the boot log warns `model sync failed at boot … err="store: not found"` once per
entry: the account exists, no credential does, so there is no catalog to sync.

The example that ships with **v1.3.2 and earlier** has two entries uncommented,
so those installs start with two keyless accounts:

| name | namespace | kind |
|---|---|---|
| `opencode-main` | `opengo` | `opencode-go` |
| `ssn-gpt` | `super-ssn` | `openai-compatible` |

Newer versions ship both commented out: nothing is seeded and a fresh instance
starts empty (verified — healthy, zero accounts). Either way, an empty map is
valid, and a config entry you delete later does **not** delete the account —
remove those in the dashboard.

The two settings a new install usually changes:

```yaml
listen: "127.0.0.1:20129"     # loopback; 0.0.0.0:20129 to reach it from the LAN
data_dir: ./data              # relative to the working directory
```

## 3. What it writes

| Path | What | Notes |
|---|---|---|
| `<data_dir>/ezllm.sqlite` (+ `-wal`, `-shm`) | ledger, accounts, combos, tokens | WAL mode; copy all three sidecars together, never a bare `cp` |
| `EZLLM_MASTER_KEY_FILE` | AES key for provider keys at rest | default `~/.hermes/secrets/ezllm-master.key` (`%USERPROFILE%\.hermes\secrets\…` on Windows), created on first boot |
| stdout | the whole log | one line per record; `EZLLM_LOG_FORMAT=json` for a collector |

**Always set `EZLLM_MASTER_KEY_FILE` explicitly** and keep it beside the
database. The default path is home-global, so a second instance silently shares
the first one's key, and a backup of `data/` alone leaves the keys unreadable.
Losing the key is unrecoverable: [master-key-runbook.md](master-key-runbook.md).
On Windows the key file's 0600 mode is not meaningful — put it in a directory
whose ACL limits it to the account that runs the gateway.

## 4. First boot and the dashboard

Linux / macOS:

```bash
cd ~/ezllm
EZLLM_ADDR=127.0.0.1:20129 \
EZLLM_DATA_DIR=$PWD/data \
EZLLM_MASTER_KEY_FILE=$PWD/data/ezllm-master.key \
./bin/ezllm -config config.yaml
```

Windows (PowerShell):

```powershell
cd C:\ezllm
$env:EZLLM_ADDR            = "127.0.0.1:20129"
$env:EZLLM_DATA_DIR        = "C:\ezllm\data"
$env:EZLLM_MASTER_KEY_FILE = "C:\ezllm\data\ezllm-master.key"
.\bin\ezllm.exe -config C:\ezllm\config.yaml
```

The boot banner ends with `dashboard setup pending: open the dashboard and enter
the setup code above` and prints a one-time setup code. `http://127.0.0.1:20129`
shows only a set-password page until that code is entered. The code is re-minted
on every restart until setup completes. To skip the page, set
`EZLLM_DASHBOARD_PASSWORD` (≥12 characters) or `EZLLM_DASHBOARD_PASSWORD_FILE`
before the first boot.

On a fresh instance the boot log also carries one warning per provider listed
in `config.example.yaml`:

```
WARN  model sync failed at boot (routing still works; sync later via admin) provider=opencode-main err="store: not found"
```

That is expected: the bootstrap config names providers, but no account or key
exists yet, so there is no catalog to sync. It disappears once you add a key in
the dashboard.

Verify (any OS; `curl.exe` on Windows):

```bash
curl -s localhost:20129/healthz                      # {"status":"ok","version":"1.3.2"}
curl -s -o /dev/null -w '%{http_code}\n' localhost:20129/v1/models   # 401 without a token
```

## 5. Throwaway instance (someone else's machine, or a scratch test)

Do not leave a gateway with your provider keys on a borrowed machine. Run it in
the foreground against a throwaway data directory and delete the directory
afterwards.

Linux / macOS:

```bash
D=$(mktemp -d)
EZLLM_DATA_DIR=$D/data EZLLM_MASTER_KEY_FILE=$D/master.key \
EZLLM_ADDR=127.0.0.1:20188 EZLLM_DASHBOARD_PASSWORD='<12+ chars>' \
  ./bin/ezllm -config config.yaml        # Ctrl-C to stop
rm -rf "$D"                              # removes the ledger AND the key
```

Windows (PowerShell):

```powershell
$d = Join-Path $env:TEMP ("ezllm-" + [guid]::NewGuid().ToString("N").Substring(0,8))
$env:EZLLM_DATA_DIR        = "$d\data"
$env:EZLLM_MASTER_KEY_FILE = "$d\master.key"
$env:EZLLM_ADDR            = "127.0.0.1:20188"
$env:EZLLM_DASHBOARD_PASSWORD = "<12+ chars>"
.\bin\ezllm.exe -config C:\ezllm\config.yaml    # Ctrl-C to stop
Remove-Item -Recurse -Force $d                  # removes the ledger AND the key
```

Nothing outside the temp directory is touched. A scratch instance must never
point at a production data directory or the production master key.

## 6. Run it as a service

### Linux — systemd user unit

`~/.config/systemd/user/ezllm.service`:

```ini
[Unit]
Description=ezllm gateway
After=network-online.target

[Service]
Type=simple
WorkingDirectory=%h/ezllm
ExecStart=%h/ezllm/bin/ezllm -config %h/ezllm/config.yaml
Environment=EZLLM_ADDR=127.0.0.1:20129
Environment=EZLLM_DATA_DIR=%h/ezllm/data
Environment=EZLLM_MASTER_KEY_FILE=%h/ezllm/data/ezllm-master.key
Environment=EZLLM_USAGE_TZ=Asia/Kuala_Lumpur
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
```

```bash
systemctl --user daemon-reload && systemctl --user enable --now ezllm
systemctl --user status ezllm
curl -s localhost:20129/healthz
```

Two things that bite a headless box:

- **A user unit stops when the last session ends.** Enable lingering so it
  survives logout: `sudo loginctl enable-linger "$USER"`.
- **Logs go to journald, not a file:** `journalctl --user -u ezllm -f` for the
  live log, `journalctl --user -u ezllm --since -1h` for the last hour.

### Linux — system unit (root-owned install, dedicated user)

For a machine where nobody logs in, install under `/opt` and run as a service
account. `/etc/systemd/system/ezllm.service`:

```ini
[Unit]
Description=ezllm gateway
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=ezllm
Group=ezllm
WorkingDirectory=/opt/ezllm
ExecStart=/opt/ezllm/bin/ezllm -config /opt/ezllm/config.yaml
Environment=EZLLM_ADDR=0.0.0.0:20129
Environment=EZLLM_DATA_DIR=/opt/ezllm/data
Environment=EZLLM_MASTER_KEY_FILE=/opt/ezllm/data/ezllm-master.key
Environment=EZLLM_USAGE_TZ=Asia/Kuala_Lumpur
Restart=on-failure
RestartSec=10
# hardening: the binary only needs its own directory
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/opt/ezllm/data
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

```bash
sudo useradd --system --home /opt/ezllm --shell /usr/sbin/nologin ezllm
sudo install -d -o ezllm -g ezllm /opt/ezllm/{bin,data,logs}
# copy the binary + config.yaml into /opt/ezllm, chown to ezllm
sudo systemctl daemon-reload && sudo systemctl enable --now ezllm
sudo systemctl status ezllm && journalctl -u ezllm -f
```

Reaching it from the LAN means binding `0.0.0.0` **and** opening the port:

```bash
sudo ufw allow from 192.168.0.0/16 to any port 20129 proto tcp    # Debian/Ubuntu
sudo firewall-cmd --permanent --add-port=20129/tcp && sudo firewall-cmd --reload   # Fedora/RHEL
```

Keep the dashboard password long. Nothing restricts `/admin` by source address
out of the box — the same warning applies to the container path in the README:
put a per-source rate limit on `/admin/login` and `/admin/setup` in front of it.

### macOS — launchd user agent

`scripts/ezllm-mac.sh` (repo root) does the install, the checksum, the layout
and the agent:

```bash
./scripts/ezllm-mac.sh install     # ~/ezllm + ~/Library/LaunchAgents/com.ezllm.gateway.plist
./scripts/ezllm-mac.sh start       # load and wait for /healthz
./scripts/ezllm-mac.sh status      # launchd state, health, last log lines
```

It also takes `stop`, `restart`, `logs`, and `uninstall [--purge]`. Override
`EZLLM_VERSION`, `EZLLM_PREFIX`, `EZLLM_ADDR`, `EZLLM_USAGE_TZ` or
`EZLLM_LOG_LEVEL` from the environment.

macOS specifics:

- The arm64 build carries an **ad-hoc code signature** (Go's linker adds it), so
  Apple Silicon executes it without further steps. An unsigned or modified copy
  is killed instantly (`Killed: 9`); re-sign with `codesign --force --sign - ezllm`.
- A browser download carries the quarantine bit and Gatekeeper will refuse it:
  `xattr -d com.apple.quarantine ezllm`, or download with `curl`.
- Binding `0.0.0.0` triggers the macOS firewall prompt — allow it.
- A LaunchAgent runs while you are **logged in**, not at boot, and stops on
  logout. For a headless always-on Mac use auto-login, or convert the plist into
  a LaunchDaemon in `/Library/LaunchDaemons` (root-owned).
- A sleeping laptop is an offline gateway.

### Windows — Task Scheduler

`ezllm.exe` is a **console application, not a Windows service**. Do not try
`New-Service` or `sc create`: the Service Control Manager accepts the
registration (the service appears, Stopped) and then refuses to start it —
`[SC] StartService FAILED 1053: The service did not respond to the start or
control request in a timely fashion` — because a console app never calls
`StartServiceCtrlDispatcher`. Measured on Windows 11 / Server 2025 build 26300.
Schedule it instead.

Task Scheduler has no environment block, so launch it through a small wrapper
that sets the variables — `C:\ezllm\ezllm-run.cmd`:

```bat
@echo off
set EZLLM_ADDR=127.0.0.1:20129
set EZLLM_DATA_DIR=C:\ezllm\data
set EZLLM_MASTER_KEY_FILE=C:\ezllm\data\ezllm-master.key
set EZLLM_USAGE_TZ=Asia/Kuala_Lumpur
cd /d C:\ezllm
"C:\ezllm\bin\ezllm.exe" -config C:\ezllm\config.yaml >> C:\ezllm\logs\ezllm.log 2>&1
```

Register it (PowerShell, run as the account that should own the data):

```powershell
$action  = New-ScheduledTaskAction -Execute 'C:\ezllm\ezllm-run.cmd'
$trigger = New-ScheduledTaskTrigger -AtLogOn
Register-ScheduledTask -TaskName ezllm -Action $action -Trigger $trigger `
  -RunLevel Highest -Force
Start-ScheduledTask -TaskName ezllm
```

```powershell
Get-ScheduledTask ezllm | Get-ScheduledTaskInfo     # last run / last result
Get-Content C:\ezllm\logs\ezllm.log -Tail 20 -Wait   # live log
Stop-ScheduledTask ezllm                             # stop
```

Variants and caveats:

- `-AtStartup` instead of `-AtLogOn` needs stored credentials (`-User` with a
  password) or `-User "SYSTEM"`. Running as SYSTEM means the data directory is
  owned by SYSTEM; that is fine for a headless box, awkward on a desktop.
- Stopping the task terminates the process rather than asking it to shut down.
  The ledger is SQLite WAL, so the next start recovers cleanly — but prefer
  `Ctrl-C` in a console window when you can.
- Reaching it from another machine means binding `0.0.0.0` **and** allowing the
  port (only on Private networks unless you mean otherwise):

  ```powershell
  New-NetFirewallRule -DisplayName ezllm -Direction Inbound -Protocol TCP `
    -LocalPort 20129 -Action Allow -Profile Private
  ```
- Windows can be configured to terminate console apps on logoff; the
  `-AtStartup`/SYSTEM form avoids that entirely.
- If you genuinely need a real Windows service, wrap the launcher with NSSM or
  WinSW — that is a third-party component, not part of a release.

## 7. Upgrade and roll back

Take a backup first — a bare `cp` / `Copy-Item` of the SQLite file is not one
(the WAL and shm sidecars carry committed rows). Use the online backup API
shipped in the repo:

```bash
python3 scripts/backup_sqlite.py \
  --db   ~/ezllm/data/ezllm.sqlite \
  --key  ~/ezllm/data/ezllm-master.key \
  --output ~/ezllm-backup-$(date +%Y%m%d)      # directory must not exist
```

Windows (PowerShell):

```powershell
python scripts\backup_sqlite.py --db C:\ezllm\data\ezllm.sqlite `
  --key C:\ezllm\data\ezllm-master.key --output C:\ezllm-backup-20261027
```

Then stop, replace the binary, start:

```bash
systemctl --user stop ezllm               # or: ./ezllm-mac.sh stop
# replace bin/ezllm with the new release binary, keep config.yaml and data/
systemctl --user start ezllm              # migration runs on boot
curl -s localhost:20129/healthz           # expect the new version string
```

```powershell
Stop-ScheduledTask ezllm
Copy-Item .\ezllm_v1.4.0_windows_amd64\ezllm.exe C:\ezllm\bin\ezllm.exe -Force
Start-ScheduledTask ezllm
curl.exe -s localhost:20129/healthz
```

Schema migrations run at boot and are one-way per release. Roll back with the
previous binary **and** the backup taken before the upgrade: an old binary
refuses a newer schema. Rehearse a migration before a production upgrade by
booting the new binary against a **copy** of the data directory
(`EZLLM_DATA_DIR=<copy>` on a scratch port) and comparing table counts and
`schema_version` before and after — see
[recovery-runbook.md](recovery-runbook.md) for the full procedure.

## Troubleshooting

| Symptom | Platform | Cause |
|---|---|---|
| `config: read config.yaml: no such file or directory` | all | run from the install dir, or pass `-config` an absolute path (quote backslash paths) |
| `/healthz` never answers | all | check the log first: config error, port already bound, or a firewall |
| `Killed: 9` | macOS arm64 | signature stripped by an edit/copy — `codesign --force --sign - ezllm` |
| "cannot be opened because the developer cannot be verified" | macOS | quarantine bit — `xattr -d com.apple.quarantine ezllm` |
| "Windows protected your PC" / SmartScreen | Windows | mark-of-the-web — `Unblock-File .\ezllm.exe` |
| `[SC] StartService FAILED 1053: The service did not respond…` | Windows | `New-Service`/`sc create` on a console app — the SCM registers it, then cannot start it. Use Task Scheduler |
| task registered but nothing listens | Windows | wrapper `cd /d` missing, or the task ran as an account that cannot read `config.yaml` |
| unit dies at logout | Linux | user unit without lingering — `sudo loginctl enable-linger "$USER"` |
| `bind: permission denied` | Linux | port < 1024; use the default 20129 or run behind a proxy |
| boot fails naming an unresolvable timezone | all | `EZLLM_USAGE_TZ` must be a real IANA name |
| boot fails naming a bridge | macOS / Windows | `EZLLM_*_BRIDGE=on` is Linux-only for Codex/Antigravity — keep it `off` |
