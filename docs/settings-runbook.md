# Settings administration

Settings has six sections: General, Security, Backup & Recovery, Gateway,
Notifications and System. Provider accounts, combos, client tokens and compression
profiles remain on their existing pages.

## Configuration ownership

For supported controls, flags override environment variables, which override
saved SQLite settings, explicit YAML values, and built-in defaults in that order.
The effective configuration view shows each value's source. An environment or
flag override locks its dashboard control. Reset removes the saved value and
returns to deployment/default configuration. It never modifies YAML.

Existing installations retain their configuration until an operator saves an
override. A saved override survives restart and wins over later YAML edits.
Remove it with Reset when returning ownership to YAML. The deprecated translation
switch retains its existing environment compatibility behavior.

Settings changes validate and persist as one revision. Concurrent edits return
409 rather than overwriting another operator's work. Reload Settings after a
conflict and reapply the intended change. Persistence failure leaves the previous
runtime policy active.

YAML retains its existing validation/default behavior: a nonpositive
`retry.retries` normalizes to three. Saving `retries: 0` through Settings disables
same-hop retries; removing that saved override returns to the YAML/default policy.

## Live changes

Translation, retries, attempt budget, dispatch deadline and cooldown policy are
captured at HTTP request admission. Updates apply to subsequent requests; active
requests keep their admitted policy. Dispatch deadlines bound choosing/retrying,
not the winning stream. Existing cooldown expiry times are preserved.

Catalog refresh interval and price-refresh switches reschedule future work.
Already-running jobs may finish; scheduled jobs do not overlap another scheduled
run of the same kind. Automatic probing controls future automatic probe jobs.
Manual provider/price tools remain available.

Network binding, database/key locations, trusted proxies, bridge executables,
CLI logins and backup paths/secrets remain deployment-managed.

## Timezones and budgets

`usage_timezone` controls default report day bucketing, inherited model-rule
windows, and the daily price/backup schedules. An explicit report `tz` query still
wins. The dashboard requires an impact acknowledgement before a timezone change.

Model rules with `win_tz: instance` follow the admitted request's instance timezone.
New rules default to this mode. Explicit rule timezones remain independent;
existing empty rule timezones are migrated to explicit UTC, preserving their old
boundaries. Existing explicit values, including Asia/Kuala_Lumpur, are preserved.

Inherited rule usage is computed from committed ledger timestamps in the chosen
calendar range. This avoids mixing counters that share a bucket name but were
created under different timezone boundaries. Historical rows/counters are not
rewritten. An instance timezone change can immediately alter remaining inherited
budget. Client-token budgets retain their existing server-local boundaries and
reservation settlement contract.

## Security and diagnostics

Password changes still revoke every dashboard session. The session list shows
opaque management IDs, creation/expiry times, and bounded browser descriptions.
Descriptions come from the browser and are informational. Individual revocation
and sign-out-all take effect immediately; revoking the current browser signs it
out. Legacy descriptions may be unavailable.

Notification preferences apply to future bell events, including backup failures;
existing notification history remains. No external notification delivery is added.

System diagnostics use an explicit allowlist: version, uptime, database byte size,
bridge readiness, safe deployment metadata (listen address/source, storage kind,
master-key source and trusted proxy count), runtime settings/provenance, job
timestamps, and backup status. They contain no credentials, prompts, request bodies, raw environment dumps, or
filesystem paths. Readiness records bridge startup status, not an active vendor
inference probe. `/healthz` remains exactly `status` and `version`.

## Push channels

Notifications → Push channels sends bell events to ntfy, Discord, Telegram or
any JSON webhook as they happen. The per-event toggles still decide which
events exist; each channel then takes those at or above its minimum severity
(everything, warnings and errors, or errors only — warnings and errors by
default). A recurring event that folds into one unread bell row is pushed
again at most once a day.

Add a channel, then **Send test**: the editor tests the draft before it is
saved, and a saved row's test records the outcome as its status. Secrets (the
ntfy token, the Discord webhook URL, the Telegram bot token, the webhook URL
and bearer) are stored encrypted with the master key, never shown again, and
kept on an edit when their field is left blank. Delivery is in the background
— 10 s per attempt, three attempts for a network failure, 429 or 5xx, no
redirects followed — and a failing channel shows *Failing* with the reason; it
never raises a bell event about itself. At most ten channels. On ntfy.sh anyone
who knows a topic can read it: choose one that is hard to guess, or protect it
with an access token. API: docs/API.md § Push channels.

## Backup automation and daily schedules

Use the [recovery runbook](recovery-runbook.md) to configure the mounted
backup destination and separate encryption secret. Scheduling defaults to off;
when enabled, defaults are daily at 03:00 with seven verified backups retained.
A manual backup is available without enabling the schedule. A job verifies the
snapshot and credential decryption before publishing the encrypted archive;
retention begins only after successful publication.

Daily schedules use the current instance timezone. A skipped DST hour uses the
first valid minute after the gap, and a repeated hour uses its first occurrence.
Changing an hour/timezone reschedules future work; it does not cancel active jobs.

An unavailable destination does not prevent gateway startup or unrelated policy
edits. Enabling automation checks storage; each job checks again and reports
failures independently. A configured backup secret file must remain readable at
startup. Backup status/history is described in the recovery runbook.

## Browser preferences

Theme, reduced motion and default analytics range are local to each browser.
Dark theme and a 24-hour analytics range remain the defaults. Motion follows the
OS unless Reduce motion is selected; the app never overrides an OS request for
less motion. Changing an analytics page's range does not change the saved default.
