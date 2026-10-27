import { getSettings, saveSettings, getDashboardSessions, revokeDashboardSession, revokeAllDashboardSessions, getBackupStatus, runBackup, getDiagnostics, type SettingsView, type DashboardSession, type BackupStatus, type SystemDiagnostics } from '../utils/api';
import { readTheme, setTheme } from '../utils/theme';
import { readMotion, readRange, savePreference, subscribePreferences } from '../utils/preferences';
import { useEffect, useRef, useState } from 'react';
import { ApiError, changeDashboardPassword, clearToken } from '../utils/api';
import { Chip, SectionLabel, btn, cx, inputCls } from '../components/ui';
import PasswordStrength from '../components/PasswordStrength';
import { MIN_PASSWORD } from '../utils/passwordStrength';

function PasswordSettings() {
  const [currentPassword, setCurrentPassword] = useState('');
  const [newPassword, setNewPassword] = useState('');
  const [confirmPassword, setConfirmPassword] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [saved, setSaved] = useState(false);

  const submit = async (event: React.FormEvent) => {
    event.preventDefault();
    setError('');
    setSaved(false);
    if (newPassword.length < MIN_PASSWORD) { setError(`Use at least ${MIN_PASSWORD} characters.`); return; }
    if (newPassword !== confirmPassword) { setError('The new passwords do not match.'); return; }
    setBusy(true);
    try {
      await changeDashboardPassword(currentPassword, newPassword);
      setSaved(true);
      setCurrentPassword(''); setNewPassword(''); setConfirmPassword('');
      // Password change revokes every session, including this browser's.
      window.setTimeout(() => { clearToken(); window.location.reload(); }, 900);
    } catch (cause) {
      setError(cause instanceof ApiError ? cause.message : cause instanceof Error ? cause.message : 'Could not update password.');
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="max-w-2xl space-y-6">
      <section className="rounded-xl border border-line bg-surface p-5 sm:p-6">
        <SectionLabel>Dashboard password</SectionLabel>
        <p className="mt-2 text-sm text-mute">Choose a new password of at least {MIN_PASSWORD} characters. Your active dashboard sessions will be signed out after the change.</p>
        <form className="mt-6 max-w-md space-y-4" onSubmit={submit}>
          <label className="block text-xs font-medium text-dim">Current password
            <input className={cx(inputCls, 'mt-1.5')} type="password" autoComplete="current-password" value={currentPassword} onChange={(e) => setCurrentPassword(e.target.value)} required />
          </label>
          <label className="block text-xs font-medium text-dim">New password
            <input className={cx(inputCls, 'mt-1.5')} type="password" autoComplete="new-password" value={newPassword} onChange={(e) => setNewPassword(e.target.value)} minLength={MIN_PASSWORD} required />
            <PasswordStrength password={newPassword} />
          </label>
          <label className="block text-xs font-medium text-dim">Confirm new password
            <input className={cx(inputCls, 'mt-1.5')} type="password" autoComplete="new-password" value={confirmPassword} onChange={(e) => setConfirmPassword(e.target.value)} minLength={MIN_PASSWORD} required />
          </label>
          {error && <p className="text-sm text-bad" role="alert">{error}</p>}
          {saved && <p className="text-sm text-good" role="status">Password updated. Signing out this session…</p>}
          <button disabled={busy} className={cx(btn.base, btn.primary)} type="submit">{busy ? 'Saving…' : 'Change password'}</button>
        </form>
      </section>
      <div className="flex items-start gap-3 rounded-xl border border-warn/30 bg-warn-dim p-4">
        <Chip tone="warn">Security</Chip>
        <p className="text-xs leading-relaxed text-mute">This password guards provider keys and client tokens, and there is no second factor. Keep it long and unique — the strength bar should read Good or Strong — especially if the dashboard is reachable beyond a trusted network.</p>
      </div>
    </div>
  );
}

type Tab = 'General' | 'Security' | 'Backup & Recovery' | 'Gateway' | 'Notifications' | 'System';
const TABS: Tab[] = ['General', 'Security', 'Backup & Recovery', 'Gateway', 'Notifications', 'System'];
const DETAILS: Record<string, [string, string]> = {
  usage_timezone: ['Usage timezone', 'IANA name, for example Asia/Kuala_Lumpur. Reports and rules set to “Instance timezone” follow this value. Explicit rule timezones and existing client-token budgets retain their own boundaries.'],
  translation_enabled: ['Protocol translation', 'Allow proven cross-protocol routes. Surface pins still apply.'],
  automatic_probing: ['Automatic probing', 'Probe untested combo hops and newly discovered free models.'],
  catalog_interval: ['Catalog refresh interval', 'Duration of at least 1m, for example 12h.'],
  price_refresh: ['Automatic price refresh', 'Refresh after startup and daily at 03:00 in the instance timezone.'],
  retries: ['Retries per hop', '0–9 retries for transient failures. Zero disables same-hop retries.'],
  attempt_budget: ['Total request attempt budget', '0–64 attempts across all hops and retries. Zero means unlimited.'],
  dispatch_deadline: ['Dispatch deadline', 'At least 1s, or leave empty for unlimited. Bounds selection and retry waits; active streams continue.'],
  cooldown_429: ['Rate-limit cooldown', 'Fallback duration when a provider does not supply Retry-After.'],
  cooldown_5xx: ['Server-error cooldown', 'Duration before a failed endpoint is eligible again.'],
  cooldown_strikes: ['Failures before cooldown', 'Positive failure count. Existing cooldown expiry times are preserved.'],
  backup_enabled: ['Scheduled backups', 'Requires a mounted backup directory and a separate encryption secret configured on the host.'],
  backup_hour: ['Daily backup hour', '0–23, in the instance timezone.'],
  backup_keep: ['Verified backups to keep', '1–365. Older completed backups are removed only after a new backup verifies successfully.'],
  notify_free_models_added: ['Free models added', 'New free models discovered in a provider catalog.'],
  notify_free_models_removed: ['Free models removed', 'Free models removed from a provider catalog.'],
  notify_free_model_broken: ['Broken free models', 'Free models that fail every protocol probe.'],
  notify_catalog_refresh_failed: ['Catalog refresh failures', 'A provider catalog could not be refreshed.'],
  notify_client_budget_reached: ['Client budget reached', 'A client token has exhausted its budget.'],
  notify_price_refresh_failed: ['Price refresh failures', 'The price catalog could not be refreshed.'],
  notify_backup_failed: ['Backup failures', 'A backup did not complete or verification failed.'],
};
const GROUPS: Partial<Record<Tab, string[]>> = {
  General: ['usage_timezone'],
  Gateway: ['translation_enabled', 'automatic_probing', 'catalog_interval', 'price_refresh', 'retries', 'attempt_budget', 'dispatch_deadline', 'cooldown_429', 'cooldown_5xx', 'cooldown_strikes'],
  'Backup & Recovery': ['backup_enabled', 'backup_hour', 'backup_keep'],
  Notifications: Object.keys(DETAILS).filter(k => k.startsWith('notify_')),
};
const panel = 'rounded-xl border border-line bg-surface p-5 sm:p-6';
const errorMessage = (e: unknown) => e instanceof Error ? e.message : 'The operation could not be completed.';

function BrowserPreferences() {
  const [theme, themeState] = useState(readTheme);
  const [motion, motionState] = useState(readMotion);
  const [range, rangeState] = useState(readRange);
  useEffect(() => subscribePreferences(() => { themeState(readTheme()); motionState(readMotion()); rangeState(readRange()); }), []);
  return <section className={panel}>
    <SectionLabel>This browser</SectionLabel>
    <p className="mt-2 text-sm text-mute">Display preferences apply to this browser. They do not change gateway behavior.</p>
    <div className="mt-5 grid gap-4 sm:grid-cols-3">
      <div className="text-sm"><label htmlFor="pref-theme">Theme</label><select id="pref-theme" className={cx(inputCls, 'mt-2')} value={theme} onChange={e => { setTheme(e.target.value as 'dark' | 'light'); themeState(readTheme()); }}><option value="dark">Dark</option><option value="light">Light</option></select></div>
      <div className="text-sm"><label htmlFor="pref-motion">Motion</label><select id="pref-motion" className={cx(inputCls, 'mt-2')} value={motion} onChange={e => { savePreference('ezllm.motion', e.target.value); motionState(readMotion()); }}><option value="system">Follow system</option><option value="reduce">Reduce motion</option></select></div>
      <div className="text-sm"><label htmlFor="pref-range">Default analytics range</label><select id="pref-range" className={cx(inputCls, 'mt-2')} value={range} onChange={e => { savePreference('ezllm.range', e.target.value); rangeState(readRange()); }}><option value="24h">24 hours</option><option value="7d">7 days</option><option value="30d">30 days</option><option value="365d">1 year</option></select></div>
    </div>
  </section>;
}

function SettingsGroup({ tab, view, updated }: { tab: Tab; view: SettingsView; updated: (v: SettingsView) => void }) {
  const keys = GROUPS[tab] ?? [];
  const [draft, setDraft] = useState<Record<string, unknown>>({});
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [saved, setSaved] = useState(false);
  const [acknowledged, acknowledge] = useState(false);
  useEffect(() => { setDraft({}); acknowledge(false); }, [view.revision, tab]);
  if (!keys.length) return null;
  const timezoneChanged = 'usage_timezone' in draft;
  const submit = async (event: React.FormEvent) => {
    event.preventDefault(); setBusy(true); setError(''); setSaved(false);
    try { updated(await saveSettings(view.revision, draft)); setDraft({}); setSaved(true); }
    catch (e) { setError(errorMessage(e)); }
    finally { setBusy(false); }
  };
  return <form className={panel} onSubmit={submit}>
    <SectionLabel>{tab === 'General' ? 'Instance defaults' : tab}</SectionLabel>
    <p className="mt-2 text-sm text-mute">Saved changes apply to new requests immediately. Environment and flag overrides are managed on the host.</p>
    <fieldset disabled={busy} className="mt-5 space-y-5">
      {keys.map(key => {
        const field = view.fields[key]; if (!field) return null;
        const value = key in draft && draft[key] !== null ? draft[key] : field.value;
        const [label, hint] = DETAILS[key];
        return <div key={key} className="border-b border-line pb-4 last:border-0">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <label htmlFor={`setting-${key}`} className="text-sm font-medium">{label}</label>
            <div className="flex items-center gap-2"><Chip>{field.source}</Chip>{field.saved && field.editable && <button type="button" className="text-xs text-mute underline" onClick={() => { setDraft(d => ({ ...d, [key]: null })); setSaved(false); }}>Reset to deployment default</button>}</div>
          </div>
          <p id={`hint-${key}`} className="mt-1 text-xs leading-relaxed text-mute">{hint}</p>
          {typeof field.value === 'boolean'
            ? <input id={`setting-${key}`} aria-describedby={`hint-${key}`} className="mt-3 h-4 w-4" type="checkbox" disabled={!field.editable} checked={!!value} onChange={e => { setDraft(d => ({ ...d, [key]: e.target.checked })); setSaved(false); }} />
            : <input id={`setting-${key}`} aria-describedby={`hint-${key}`} className={cx(inputCls, 'mt-2 max-w-sm')} type={typeof field.value === 'number' ? 'number' : 'text'} disabled={!field.editable} value={String(value)} onChange={e => { setDraft(d => ({ ...d, [key]: typeof field.value === 'number' ? Number(e.target.value) : e.target.value })); setSaved(false); }} />}
          {draft[key] === null && <p className="mt-1 text-xs text-warn">Deployment/default value will be restored when saved.</p>}
          {!field.editable && <p className="mt-1 text-xs text-mute">Managed by {field.source}; change the deployment override to unlock this control.</p>}
        </div>;
      })}
      {timezoneChanged && <label className="flex items-start gap-2 text-sm text-warn"><input type="checkbox" checked={acknowledged} onChange={e => acknowledge(e.target.checked)} />I understand this changes report days and inherited rule boundaries, and may change available daily budget immediately.</label>}
      {error && <p role="alert" className="text-sm text-bad">{error} Reload Settings if another operator changed it.</p>}
      {saved && <p role="status" className="text-sm text-good">Settings saved.</p>}
      <button className={cx(btn.base, btn.primary)} disabled={busy || !Object.keys(draft).length || (timezoneChanged && !acknowledged)}>{busy ? 'Saving…' : 'Save changes'}</button>
    </fieldset>
  </form>;
}

function Sessions() {
  const [rows, setRows] = useState<DashboardSession[]>([]);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const load = () => getDashboardSessions().then(setRows).catch(e => setError(errorMessage(e)));
  useEffect(() => { void load(); }, []);
  const revoke = async (session?: DashboardSession) => {
    if (!window.confirm(session ? `Sign out ${session.current ? 'this browser' : 'this session'}?` : 'Sign out all dashboard sessions, including this browser?')) return;
    setBusy(true); setError('');
    try { if (session) await revokeDashboardSession(session.id); else await revokeAllDashboardSessions();
      if (!session || session.current) { clearToken(); window.location.reload(); return; } await load();
    } catch (e) { setError(errorMessage(e)); } finally { setBusy(false); }
  };
  return <section className={panel}><SectionLabel>Active sessions</SectionLabel><p className="mt-2 text-sm text-mute">Browser descriptions are supplied by the browser. Older sessions may have no description.</p>
    <ul className="mt-4 space-y-3">{rows.map(s => <li key={s.id} className="flex flex-wrap items-center justify-between gap-3 border-b border-line pb-3"><div className="min-w-0"><p className="break-words text-sm">{s.current ? 'This browser · ' : ''}{s.browser || 'Browser description unavailable'}</p><p className="mt-1 text-xs text-mute">Created {new Date(s.created_at).toLocaleString()} · expires {new Date(s.expires_at).toLocaleString()}</p></div><button className={cx(btn.base, btn.ghost)} disabled={busy} onClick={() => void revoke(s)}>Sign out</button></li>)}</ul>
    {error && <p role="alert" className="mt-3 text-sm text-bad">{error}</p>}<button className={cx(btn.base, btn.ghost, 'mt-4')} disabled={busy || !rows.length} onClick={() => void revoke()}>Sign out all devices</button>
  </section>;
}
function Backups() {
  const [status, setStatus] = useState<BackupStatus | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const loading = useRef(false);
  const load = async () => {
    if (loading.current) return;
    loading.current = true;
    try { setStatus(await getBackupStatus()); setError(''); }
    catch (e) { setError(errorMessage(e)); }
    finally { loading.current = false; }
  };
  useEffect(() => {
    const refresh = () => { if (!document.hidden) void load(); };
    refresh();
    const timer = window.setInterval(refresh, 5000);
    document.addEventListener('visibilitychange', refresh);
    return () => { window.clearInterval(timer); document.removeEventListener('visibilitychange', refresh); };
  }, []);
  const run = async () => { setBusy(true); setError(''); try { await runBackup(); await load(); } catch (e) { setError(errorMessage(e)); } finally { setBusy(false); } };
  return <><section className={panel}><SectionLabel>Exports</SectionLabel><p className="mt-2 text-sm text-mute">Exports contain configuration and optional usage history, with no provider credentials. They require a separate database and master-key backup for full recovery. A completed export ends with a successful completion manifest.</p><div className="mt-4 flex flex-wrap gap-3"><a className={cx(btn.base, btn.ghost)} href="/admin/export" download>Export configuration</a><a className={cx(btn.base, btn.ghost)} href="/admin/export?include_ledger=true" download>Export with ledger</a></div></section>
    <section className={panel}><SectionLabel>Encrypted backups</SectionLabel><p className="mt-2 text-sm text-mute">Each backup contains a verified database snapshot and its active master key, encrypted with a separate backup secret. Keep that secret separately. Configure EZLLM_BACKUP_DIR and EZLLM_BACKUP_SECRET_FILE on the host; vendor CLI logins and deployment configuration need separate backups.</p>
      <p className="mt-4 text-sm">{status?.running ? 'Backup running…' : status?.ready ? 'Backup storage configured' : 'Backup storage is not configured'}</p>
      {status?.error && <p role="alert" className="mt-2 text-sm text-bad">{status.error}</p>}
      {status?.last_outcome && <p className="mt-1 text-xs text-mute">Last job: {status.last_outcome} {status.last_finished && new Date(status.last_finished).toLocaleString()}</p>}
      <button className={cx(btn.base, btn.primary, 'mt-4')} disabled={!status?.ready || status.running || busy} onClick={() => void run()}>{busy || status?.running ? 'Backing up…' : 'Back up now'}</button>
      {error && <p role="alert" className="mt-3 text-sm text-bad">{error}</p>}
      <ul className="mt-4 space-y-2">{status?.backups.map(b => <li key={b.id} className="flex flex-wrap justify-between gap-2 text-xs"><span>{new Date(b.created_at).toLocaleString()} · {(b.bytes / 1048576).toFixed(2)} MiB</span><Chip tone="good">Verified</Chip></li>)}</ul>
      <p className="mt-5 text-xs text-mute">Recovery is offline into a new directory. Verify with <code>ezllm backup-verify --archive BACKUP --secret-file SECRET</code>, then restore with <code>ezllm backup-restore --archive BACKUP --secret-file SECRET --output NEW_DIRECTORY</code>. Restored dashboard sessions are revoked.</p>
    </section></>;
}
function System() {
  const [info, setInfo] = useState<SystemDiagnostics | null>(null);
  const [error, setError] = useState('');
  useEffect(() => { getDiagnostics().then(setInfo).catch(e => setError(errorMessage(e))); }, []);
  return <section className={panel}><SectionLabel>System</SectionLabel>{error && <p role="alert" className="mt-3 text-bad">{error}</p>}{info && <>
    <dl className="mt-4 grid grid-cols-2 gap-3 text-sm"><dt>Version</dt><dd>{info.version}</dd><dt>Uptime</dt><dd>{Math.floor(info.uptime_seconds / 60)} minutes</dd><dt>Database</dt><dd>{(info.database_bytes / 1048576).toFixed(2)} MiB</dd></dl>
    {info.deployment && <dl className="mt-5 grid grid-cols-2 gap-3 text-sm"><dt>Listen address</dt><dd className="break-words">{info.deployment.listen_address} · {info.deployment.listen_source}</dd><dt>Storage</dt><dd>{info.deployment.storage}</dd><dt>Master-key source</dt><dd>{info.deployment.master_key_source}</dd><dt>Trusted proxy entries</dt><dd>{info.deployment.trusted_proxy_count}</dd></dl>}
    <div className="mt-5 space-y-2">{info.bridges.map(b => <p key={b.id} className="flex justify-between text-sm"><span>{b.id}</span><span>{b.enabled ? 'Ready' : 'Off'}</span></p>)}</div>
    <div className="mt-5 space-y-2">{Object.entries(info.jobs).map(([name, j]) => <div key={name} className="text-sm"><p>{name}: {j.running ? 'Running' : 'Idle'}</p><p className="text-xs text-mute">Last success: {j.last_success ? new Date(j.last_success).toLocaleString() : 'No recorded success'} · last failure: {j.last_failure ? new Date(j.last_failure).toLocaleString() : 'None recorded'}</p></div>)}</div>
    <p className="mt-5 text-xs text-mute">Network binding, data storage, trusted proxies, master-key source, bridge paths and backup paths are deployment-managed.</p>
    <a className={cx(btn.base, btn.ghost, 'mt-4')} href="/admin/settings/diagnostics?download=true" download>Download sanitized diagnostics</a>
  </>}</section>;
}
export default function Settings() {
  const [tab, setTab] = useState<Tab>('General');
  const [view, setView] = useState<SettingsView | null>(null);
  const [error, setError] = useState('');
  const load = () => { setError(''); getSettings().then(setView).catch(e => setError(errorMessage(e))); };
  useEffect(load, []);
  return <div className="max-w-4xl space-y-5">
    <nav aria-label="Settings sections" className="flex flex-wrap gap-2">{TABS.map(t => <button key={t} aria-current={tab === t ? 'page' : undefined} className={cx(btn.base, tab === t ? btn.primary : btn.ghost)} onClick={() => setTab(t)}>{t}</button>)}</nav>
    {error && <div role="alert" className="text-sm text-bad">{error} <button className="underline" onClick={load}>Reload Settings</button></div>}
    {tab === 'General' && <BrowserPreferences />}
    {view && <SettingsGroup key={tab} tab={tab} view={view} updated={setView} />}
    {tab === 'Security' && <><PasswordSettings /><Sessions /></>}
    {tab === 'Backup & Recovery' && <Backups />}
    {tab === 'Notifications' && <p className="text-xs text-mute">Preferences affect future notifications in this instance’s dashboard bell. Existing history is retained.</p>}
    {tab === 'System' && <><System />{view && <section className={panel}><SectionLabel>Effective configuration</SectionLabel><div className="mt-4 overflow-x-auto"><table className="w-full text-left text-xs"><thead><tr><th className="py-2">Setting</th><th>Effective value</th><th>Source</th></tr></thead><tbody>{Object.entries(view.fields).map(([k, f]) => <tr key={k} className="border-t border-line"><td className="py-2 pr-3">{DETAILS[k]?.[0] ?? k}</td><td className="pr-3">{String(f.value) || 'Unlimited'}</td><td>{f.source}</td></tr>)}</tbody></table></div></section>}</>}
  </div>;
}
