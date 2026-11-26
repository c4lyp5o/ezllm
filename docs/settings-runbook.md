# Settings

**Settings** in the dashboard has six tabs. Providers, combos, API keys and compression
profiles have their own pages.

| Tab | What you set there |
|---|---|
| **General** | Your timezone, and whether ezllm checks for new releases |
| **Security** | The dashboard password and signed-in browsers |
| **Backup & Recovery** | Encrypted backups and configuration exports |
| **Gateway** | Translation, automatic probing, catalog and price refresh, retries and cooldowns |
| **Notifications** | Which events ring the bell, and push channels |
| **System** | Version, uptime, storage, bridges, and every effective setting with its source |

Choose **Save changes** after an edit. Changes apply to new requests straight away;
requests already running keep the settings they started with.

## Which value wins

A setting can come from several places. The first one that is set wins:

1. a command-line flag,
2. an `EZLLM_*` environment variable,
3. a value saved in Settings,
4. `config.yaml`,
5. the built-in default.

A setting fixed by a flag or an environment variable is locked in the dashboard: change it
on the host. **Reset to deployment default** removes the value saved in Settings, so
`config.yaml` or the default applies again. It never changes `config.yaml`.

**System** → **Effective configuration** shows each value and where it came from.

The listen address, file locations, trusted proxies, bridges and backup storage are set
only on the host, never in the dashboard.

If someone else saved Settings while you were editing, saving fails instead of overwriting
their change. Choose **Reload Settings** and make your change again.

## Timezone

**General** → **Usage timezone** takes a name such as `Europe/Berlin`. It decides:

- where a day starts and ends in reports,
- the windows of rules set to **Instance timezone**,
- the days and months of API key budgets,
- the time of daily price refreshes and backups.

Changing it moves those boundaries at once, so a rule can have more or less left for
today. Tick the box that says you understand, then save. Rules with their own timezone keep
it. Report rows already written are not changed.

`EZLLM_USAGE_TZ` sets it from the host instead. Nothing else sets ezllm's timezone.

## Release check

A minute after it starts, and then daily, ezllm asks GitHub for the latest release. When
there is a newer one, the version in the sidebar lights up, **System** shows it, and the
bell rings once. Nothing about your ezllm is sent and nothing is installed.

Turn it off in **General**, or with `EZLLM_UPDATE_CHECK=off`.

## Password and browsers

Changing the password signs out every browser. **Security** lists signed-in browsers; you
can sign out one, or all with **Sign out all devices**.

## Retries and cooldowns

| Setting | What it does |
|---|---|
| **Retries per hop** | Retries of a temporary failure on the same provider, 0 to 9. `0` turns them off |
| **Total request attempt budget** | Attempts across all providers and retries, 0 to 64. `0` means no limit |
| **Dispatch deadline** | The longest time spent choosing a provider and waiting to retry. Answers already streaming are not cut off |
| **Rate-limit cooldown** | How long a provider rests after a `429` without `Retry-After` |
| **Server-error cooldown** | How long a provider rests after server errors |
| **Failures before cooldown** | How many failures start a cooldown |

In `config.yaml`, `retry.retries: 0` means the default of 3. To turn retries off, save `0`
in Settings.

## Push channels

**Notifications** → **Push channels** sends bell events to ntfy, Discord, Telegram or any
JSON webhook as they happen. You can have up to ten.

1. Choose **Add channel** and fill it in.
2. Pick a **Minimum severity**: **Everything**, **Warnings and errors** (the default) or
   **Errors only**.
3. Choose **Send test**. It tests the channel before you save it.

The event switches above still decide which events exist. An event that repeats is pushed
at most once a day.

Tokens and webhook addresses are stored encrypted and never shown again. Leave the field
empty when you edit a channel to keep the saved one. A channel that keeps failing shows
**Failing** with the reason.

On ntfy.sh anyone who knows a topic can read it: pick one that is hard to guess, or use an
access token.

## Backups

**Backup & Recovery** has **Back up now**, **Scheduled backups** and exports. Setting up
the backup folder: [back up and restore](recovery-runbook.md).

## Diagnostics

**System** → **Download sanitized diagnostics** saves a file to share when you ask for
help. It has the version, uptime, database size, bridge status, settings and job times. It
has no keys, prompts, request bodies, environment or file paths.

Bridge status there means the bridge started; it does not send a test request.

## This browser

Theme, motion and the default analytics range are saved in each browser, not in ezllm.
Motion follows your system unless you choose **Reduce motion**.

---

**API:** [Settings and push channels in API.md](API.md)
