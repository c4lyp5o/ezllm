# Use bridges on Windows (WSL)

WSL runs Linux inside Windows. [Bridges](bridge-setup.md) need Linux, so install ezllm in
WSL instead of on Windows. Use one or the other: both want the same port.

You need Windows 11 and WSL 2 with Debian or Ubuntu. No WSL yet? Run `wsl --install` in
PowerShell as administrator and restart Windows.

## 1. Check WSL

In the Linux terminal:

```sh
ps -p 1 -o comm=
```

You should see `systemd`. If not, see [Not working?](#not-working)

## 2. Install ezllm

In the Linux terminal, follow [Linux](install-binary.md#linux), up to and including
**Keep it running on Linux**.

On Windows, open [http://127.0.0.1:20129](http://127.0.0.1:20129).

## 3. Keep WSL running

Windows stops WSL soon after you close the last Linux terminal, and ezllm stops with it.
This starts WSL each time you log in to Windows and keeps it on.

Open **PowerShell as administrator**, signed in as the same user, and paste this:

```powershell
$Action = New-ScheduledTaskAction -Execute "powershell.exe" -Argument '-NoProfile -WindowStyle Hidden -Command "wsl.exe --exec sleep infinity"'
$Trigger = New-ScheduledTaskTrigger -AtLogOn -User (whoami)
$Settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
Register-ScheduledTask -TaskName ezllm-wsl -Action $Action -Trigger $Trigger -Settings $Settings -Force | Out-Null
Start-ScheduledTask ezllm-wsl
Start-Sleep 15
curl.exe -s http://127.0.0.1:20129/healthz
```

You should see `"status":"ok"`. Close every Linux terminal, wait a minute and run the last
line again: it should still answer.

WSL uses some of the computer's memory while it is on. ezllm stops when you log out of
Windows.

## 4. Turn on a bridge

Open the page for [your plan](bridge-setup.md) and run its commands in the Linux terminal.
In step 4 of that page, pick **The ezllm binary**.

## Stop it

This stops ezllm and everything else in WSL:

```powershell
wsl.exe --shutdown
```

To start it again, run `Start-ScheduledTask ezllm-wsl`. To stop it starting by itself, run
`Unregister-ScheduledTask ezllm-wsl -Confirm:$false`.

## Not working?

| You see | Do this |
|---|---|
| Step 1 does not print `systemd` | In Linux, add the lines `[boot]` and `systemd=true` to `/etc/wsl.conf`. Run `wsl.exe --shutdown` in PowerShell, then open Linux again |
| `localhost:20129` hangs | Use `127.0.0.1:20129` |
| ezllm stopped after `wsl --shutdown` | Run `Start-ScheduledTask ezllm-wsl` |
| A command finds a CLI under `/mnt/c` | That is the Windows one. Install the CLI inside WSL |
| `cat /sys/kernel/security/lsm` says `No such file` | That alone is not a fault. The ezllm log says whether Codex has [Landlock](codex-bridge-runbook.md#platform-requirements--read-this-first) |
