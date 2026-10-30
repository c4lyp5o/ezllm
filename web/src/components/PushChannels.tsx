import { useEffect, useMemo, useState } from 'react';
import {
  createChannel, deleteChannel, listChannels, testChannel, updateChannel,
  type ChannelBody, type ChannelKind, type ChannelsView, type NotificationChannel, type Severity,
} from '../utils/api';
import { Chip, ConfirmModal, Modal, SecretInput, SectionLabel, Segmented, Spinner, Toggle, btn, cx, inputCls, labelCls, relTime } from './ui';

// Push channels (Settings → Notifications): where bell events go besides the
// dashboard. The server is the authority on every bound; the checks here
// mirror internal/notify Validate so a bad value is refused at the field.

const KIND_LABEL: Record<ChannelKind, string> = { ntfy: 'ntfy', discord: 'Discord', telegram: 'Telegram', webhook: 'Webhook' };
const KINDS: ChannelKind[] = ['ntfy', 'discord', 'telegram', 'webhook'];

// Each field as the operator reads it: label, placeholder, help.
const FIELD: Record<string, { label: string; placeholder: string; help: string }> = {
  server: { label: 'Server', placeholder: 'https://ntfy.sh', help: 'Leave blank for ntfy.sh, or name your own ntfy server.' },
  topic: { label: 'Topic', placeholder: 'ezllm-alerts-7f3k', help: 'Subscribe to this topic in the ntfy app. On ntfy.sh anyone who knows the topic can read it: pick one that is hard to guess, or use an access token.' },
  token: { label: 'Access token', placeholder: 'tk_…', help: 'Optional: for a topic protected on the server.' },
  webhook_url: { label: 'Webhook URL', placeholder: 'https://discord.com/api/webhooks/…', help: 'Server Settings → Integrations → Webhooks → Copy Webhook URL.' },
  chat_id: { label: 'Chat id', placeholder: '-1001234567890 or @your_channel', help: 'The chat, group or channel the bot posts to. The bot must be a member.' },
  bot_token: { label: 'Bot token', placeholder: '123456789:AA…', help: 'The token BotFather gave you.' },
  url: { label: 'URL', placeholder: 'https://hooks.example.com/ezllm', help: 'Receives a JSON POST: kind, severity, title, body, created_at.' },
  bearer: { label: 'Bearer token', placeholder: 'optional', help: 'Optional: sent as Authorization: Bearer ….' },
};

const SEVERITY_READOUT: Record<Severity, string> = {
  info: 'Every bell event: information, warnings and errors.',
  warn: 'Warnings and errors (a held-back release, a failed backup). Information stays on the bell.',
  error: 'Errors only (a rollback, a failed backup).',
};

const ntfyTopic = /^[A-Za-z0-9_-]{1,64}$/;
const telegramChat = /^(-?[0-9]{1,20}|@[A-Za-z0-9_]{5,32})$/;
const telegramToken = /^[0-9]{5,15}:[A-Za-z0-9_-]{30,64}$/;

function urlProblem(raw: string, secret: boolean): string | null {
  let u: URL;
  try { u = new URL(raw); } catch { return 'Enter a full URL starting with https://'; }
  if (u.protocol !== 'https:' && u.protocol !== 'http:') return 'Use an http or https URL.';
  if (!secret && (u.username || u.password || u.search)) return 'Put credentials in the access token field, not in the URL.';
  return null;
}

type Draft = { kind: ChannelKind; name: string; enabled: boolean; min_severity: Severity; config: Record<string, string>; secrets: Record<string, string>; clear: Record<string, boolean> };

const emptyDraft = (): Draft => ({ kind: 'ntfy', name: '', enabled: true, min_severity: 'warn', config: {}, secrets: {}, clear: {} });
const draftOf = (c: NotificationChannel): Draft => ({ kind: c.kind, name: c.name, enabled: c.enabled, min_severity: c.min_severity, config: { ...c.config }, secrets: {}, clear: {} });

// validate returns a reason per field; `stored` names the secrets a saved
// channel already holds (a blank secret input keeps them).
function validate(d: Draft, view: ChannelsView, stored: string[]): Record<string, string> {
  const e: Record<string, string> = {};
  const name = d.name.trim();
  if (!name) e.name = 'Give the channel a name.';
  else if ([...name].length > 60) e.name = 'Use 60 characters or fewer.';
  for (const f of view.kinds[d.kind] ?? []) {
    const v = (f.secret ? d.secrets[f.name] : d.config[f.name])?.trim() ?? '';
    const kept = f.secret && !d.clear[f.name] && stored.includes(f.name);
    if (!v) { if (f.required && !kept) e[f.name] = `${FIELD[f.name]?.label ?? f.name} is required.`; continue; }
    if (f.name === 'server' || f.name === 'url') { const p = urlProblem(v, f.secret); if (p) e[f.name] = p; }
    if (f.name === 'topic' && !ntfyTopic.test(v)) e.topic = 'Use 1 to 64 letters, digits, - or _.';
    if (f.name === 'webhook_url' && !/^https:\/\/(discord|discordapp)\.com\/api\/webhooks\//.test(v)) e.webhook_url = 'Paste a Discord webhook URL (https://discord.com/api/webhooks/…).';
    if (f.name === 'chat_id' && !telegramChat.test(v)) e.chat_id = 'Use a numeric chat id or an @channel name.';
    if (f.name === 'bot_token' && !telegramToken.test(v)) e.bot_token = 'That is not a BotFather token (digits:letters).';
  }
  return e;
}

// body turns a draft into the request: on an edit only what changed, and a
// secret is sent only when typed (or cleared on purpose).
function body(d: Draft, base: NotificationChannel | null): ChannelBody {
  const config: Record<string, string> = {};
  for (const [k, v] of Object.entries(d.config)) if (v.trim()) config[k] = v.trim();
  const secrets: Record<string, string> = {};
  for (const [k, v] of Object.entries(d.secrets)) if (v.trim()) secrets[k] = v.trim();
  for (const [k, on] of Object.entries(d.clear)) if (on && !secrets[k]) secrets[k] = '';
  if (!base) return { kind: d.kind, name: d.name.trim(), enabled: d.enabled, min_severity: d.min_severity, config, secrets };
  const b: ChannelBody = {};
  if (d.name.trim() !== base.name) b.name = d.name.trim();
  if (d.enabled !== base.enabled) b.enabled = d.enabled;
  if (d.min_severity !== base.min_severity) b.min_severity = d.min_severity;
  if (sameEntries(config, base.config) === false) b.config = config;
  if (Object.keys(secrets).length) b.secrets = secrets;
  return b;
}

const isEmpty = (b: ChannelBody) => Object.keys(b).length === 0;
const sameEntries = (a: Record<string, string>, b: Record<string, string>) =>
  JSON.stringify(Object.entries(a).sort()) === JSON.stringify(Object.entries(b).sort());

function ChannelEditor({ view, target, open, onClose, onSaved }: {
  view: ChannelsView; target: NotificationChannel | null; open: boolean; onClose: () => void; onSaved: (c: NotificationChannel, created: boolean) => void;
}) {
  const [d, setD] = useState<Draft>(emptyDraft);
  const [busy, setBusy] = useState<'' | 'save' | 'test'>('');
  const [showAll, setShowAll] = useState(false);
  const [err, setErr] = useState('');
  const [test, setTest] = useState<{ ok: boolean; error?: string } | null>(null);
  useEffect(() => { if (open) { setD(target ? draftOf(target) : emptyDraft()); setShowAll(false); setErr(''); setTest(null); } }, [open, target]);

  const stored = target?.secrets_set ?? [];
  const errors = validate(d, view, stored);
  const req = body(d, target);
  const dirty = target ? !isEmpty(req) : (d.name.trim() !== '' || Object.values(d.config).some(Boolean) || Object.values(d.secrets).some(Boolean));
  // A malformed value shows at once; a missing one waits for the first save.
  const shown = (k: string) => {
    if (!errors[k]) return '';
    const typed = k === 'name' ? d.name.trim() !== '' : (d.config[k] ?? d.secrets[k] ?? '').trim() !== '';
    return showAll || typed ? errors[k] : '';
  };
  const set = (patch: Partial<Draft>) => { setD((x) => ({ ...x, ...patch })); setTest(null); };
  const fields = view.kinds[d.kind] ?? [];

  const save = async () => {
    setShowAll(true);
    if (Object.keys(errors).length) return;
    setBusy('save'); setErr('');
    try {
      const c = target ? await updateChannel(target.id, req) : await createChannel(req);
      onSaved(c, !target);
    } catch (e) { setErr(e instanceof Error ? e.message : 'The channel could not be saved.'); }
    finally { setBusy(''); }
  };
  const sendTest = async () => {
    setShowAll(true);
    if (Object.keys(errors).length) return;
    setBusy('test'); setErr(''); setTest(null);
    try { setTest(await testChannel(target ? { id: target.id, ...req } : req)); }
    catch (e) { setErr(e instanceof Error ? e.message : 'The test could not be sent.'); }
    finally { setBusy(''); }
  };

  return (
    <Modal open={open} onClose={onClose} busy={busy !== ''} dirty={dirty} onSubmit={() => void save()}
      title={target ? `Edit push channel — ${target.name}` : 'Add a push channel'}
      subtitle="Bell events that meet the minimum severity are sent here as they happen"
      footer={(close) => <>
        {showAll && Object.keys(errors).length > 0 && <span className="mr-auto text-[12px] text-bad">Fix the fields marked in red.</span>}
        <button type="button" disabled={busy !== ''} className={cx(btn.base, btn.ghost)} onClick={() => void sendTest()}>{busy === 'test' ? <Spinner /> : null}Send test</button>
        <button type="button" className={cx(btn.base, btn.ghost)} onClick={close}>Cancel</button>
        <button type="submit" disabled={busy !== '' || (!!target && !dirty)} className={cx(btn.base, btn.primary)}>{busy === 'save' ? <Spinner /> : null}{target ? 'Save' : 'Add channel'}</button>
      </>}>
      <div className="space-y-5">
        {!target && (
          <div>
            <span className={labelCls}>Service</span>
            <Segmented label="Service" options={KINDS.map((k) => ({ key: k, label: KIND_LABEL[k] }))} value={d.kind}
              onChange={(kind) => set({ kind, config: {}, secrets: {}, clear: {} })} />
          </div>
        )}
        <div>
          <label className={labelCls} htmlFor="pc-name">Name</label>
          <input id="pc-name" className={cx(inputCls, shown('name') && 'border-bad/60')} value={d.name} placeholder="My phone" spellCheck={false}
            aria-invalid={!!shown('name')} onChange={(e) => set({ name: e.target.value })} />
          {shown('name') ? <p className="mt-1 text-[12px] text-bad">{shown('name')}</p> : <p className="mt-1 text-[12px] text-mute">How this channel is listed here.</p>}
        </div>
        {fields.map((f) => {
          const meta = FIELD[f.name] ?? { label: f.name, placeholder: '', help: '' };
          const id = `pc-${f.name}`;
          const has = stored.includes(f.name) && !d.clear[f.name];
          const problem = shown(f.name);
          return (
            <div key={`${d.kind}-${f.name}`}>
              <label className={labelCls} htmlFor={id}>{meta.label}{!f.required && <span className="text-mute"> · optional</span>}</label>
              {f.secret
                ? <SecretInput id={id} value={d.secrets[f.name] ?? ''} placeholder={has ? 'Stored — leave blank to keep it' : meta.placeholder}
                    onChange={(v) => set({ secrets: { ...d.secrets, [f.name]: v }, clear: { ...d.clear, [f.name]: false } })} />
                : <input id={id} className={cx(inputCls, problem && 'border-bad/60')} value={d.config[f.name] ?? ''} placeholder={meta.placeholder} spellCheck={false}
                    aria-invalid={!!problem} onChange={(e) => set({ config: { ...d.config, [f.name]: e.target.value } })} />}
              {problem ? <p className="mt-1 text-[12px] text-bad">{problem}</p> : <p className="mt-1 text-[12px] text-mute">{meta.help}</p>}
              {f.secret && !f.required && stored.includes(f.name) && (
                <label className="mt-1.5 flex items-center gap-2 text-[12px] text-dim">
                  <input type="checkbox" checked={!!d.clear[f.name]} onChange={(e) => set({ clear: { ...d.clear, [f.name]: e.target.checked }, secrets: { ...d.secrets, [f.name]: '' } })} />
                  Remove the stored {meta.label.toLowerCase()}
                </label>
              )}
            </div>
          );
        })}
        <div>
          <span className={labelCls}>Send</span>
          <Segmented label="Minimum severity" options={[{ key: 'info', label: 'Everything' }, { key: 'warn', label: 'Warnings and errors' }, { key: 'error', label: 'Errors only' }]}
            value={d.min_severity} onChange={(min_severity) => set({ min_severity })} />
          <p className="mt-1.5 text-[12px] text-mute">{SEVERITY_READOUT[d.min_severity]}</p>
        </div>
        <div className="flex items-center justify-between gap-3">
          <span className="text-[13px] text-dim">Deliver to this channel</span>
          <Toggle checked={d.enabled} onChange={(enabled) => set({ enabled })} label="Enabled" />
        </div>
        {test && (test.ok
          ? <p role="status" className="text-[13px] text-ok">Test sent. Check {KIND_LABEL[d.kind]} for “ezllm test notification”.</p>
          : <p role="alert" className="text-[13px] text-bad">The test did not arrive: {test.error}</p>)}
        {err && <p role="alert" className="text-[13px] text-bad">{err}</p>}
      </div>
    </Modal>
  );
}

// A channel's one status, worst first.
function status(c: NotificationChannel): { label: string; tone: 'neutral' | 'ok' | 'bad'; title?: string } {
  if (!c.enabled) return { label: 'Off', tone: 'neutral' };
  if (c.last_status === 'failed') return { label: 'Failing', tone: 'bad', title: c.last_error };
  if (c.last_status === 'ok') return { label: `Delivered ${relTime(c.last_sent_at)}`, tone: 'ok' };
  return { label: 'Nothing sent yet', tone: 'neutral' };
}

const SEVERITY_SHORT: Record<Severity, string> = { info: 'everything', warn: 'warnings and errors', error: 'errors only' };

export default function PushChannels() {
  const [view, setView] = useState<ChannelsView | null>(null);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [editing, setEditing] = useState<NotificationChannel | null>(null);
  const [adding, setAdding] = useState(false);
  const [deleting, setDeleting] = useState<NotificationChannel | null>(null);
  const [rowBusy, setRowBusy] = useState<number | null>(null);
  // A response of the wrong shape is an error in this panel, never a crash
  // of the Settings page around it.
  const load = () => listChannels().then((v) => {
    if (!v || !Array.isArray(v.channels) || !v.kinds) throw new Error('Push channels could not be loaded: the server answered in an unexpected shape.');
    setView(v); setError('');
  }).catch((e) => setError(e instanceof Error ? e.message : 'Push channels could not be loaded.'));
  useEffect(() => { void load(); }, []);

  const failing = useMemo(() => view?.channels.filter((c) => c.enabled && c.last_status === 'failed').length ?? 0, [view]);
  const act = async (c: NotificationChannel, run: () => Promise<string>) => {
    setRowBusy(c.id); setNotice('');
    try { setNotice(await run()); await load(); }
    catch (e) { setNotice(e instanceof Error ? e.message : 'That did not work.'); }
    finally { setRowBusy(null); }
  };

  return (
    <section className="rounded-xl border border-line bg-surface p-5 sm:p-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <SectionLabel>Push channels</SectionLabel>
        <button type="button" className={cx(btn.base, btn.ghost)} disabled={!view || view.channels.length >= view.max}
          title={view && view.channels.length >= view.max ? `At most ${view.max} channels` : undefined} onClick={() => setAdding(true)}>Add channel</button>
      </div>
      <p className="mt-2 text-[13px] text-mute">
        Send bell events to your phone or a chat as they happen: ntfy, Discord, Telegram or any webhook. The toggles above still decide which events exist.
      </p>
      {error && <p role="alert" className="mt-3 text-[13px] text-bad">{error}</p>}
      {view && view.channels.length > 0 && (
        <p className="mt-3 text-[12px] text-dim">
          {view.channels.length === 1 ? '1 channel' : `${view.channels.length} channels`}{failing ? ` · ${failing === 1 ? '1 failing' : `${failing} failing`}` : ''}
        </p>
      )}
      {notice && (
        <div className="mt-3 flex items-center justify-between gap-3 rounded-lg border border-line bg-raised/60 px-4 py-2.5 text-[13px] text-dim" role="status">
          <span>{notice}</span>
          <button type="button" className="text-mute hover:text-ink" aria-label="Dismiss" onClick={() => setNotice('')}>✕</button>
        </div>
      )}
      {view && view.channels.length === 0 && <p className="mt-4 text-[13px] text-dim">No push channels yet. The bell in the dashboard still shows every event.</p>}
      {view && view.channels.length > 0 && (
        <ul className="mt-3 divide-y divide-line">
          {view.channels.map((c) => {
            const st = status(c);
            return (
              <li key={c.id} className="flex flex-col gap-2 py-3 sm:flex-row sm:items-center sm:gap-4">
                <div className="min-w-0 flex-1">
                  <p className="flex flex-wrap items-center gap-2 text-sm text-ink">{c.name}<Chip>{KIND_LABEL[c.kind]}</Chip><Chip tone={st.tone} title={st.title}>{st.label}</Chip></p>
                  <p data-target className="mt-0.5 break-words font-mono text-[12px] text-mute">{c.target} · {SEVERITY_SHORT[c.min_severity]}</p>
                  {c.enabled && c.last_status === 'failed' && c.last_error && <p className="mt-0.5 text-[12px] text-bad">{c.last_error}</p>}
                </div>
                <div className="flex flex-wrap items-center gap-2">
                  <Toggle checked={c.enabled} disabled={rowBusy === c.id} label={c.enabled ? 'On' : 'Off'}
                    onChange={(enabled) => void act(c, async () => { await updateChannel(c.id, { enabled }); return `${c.name} is ${enabled ? 'on' : 'off'}.`; })} />
                  <button type="button" className={cx(btn.base, btn.ghost)} disabled={rowBusy === c.id}
                    onClick={() => void act(c, async () => { const r = await testChannel({ id: c.id }); return r.ok ? `Test sent to ${c.name}.` : `The test to ${c.name} did not arrive: ${r.error}`; })}>
                    {rowBusy === c.id ? <Spinner /> : null}Send test
                  </button>
                  <button type="button" className={cx(btn.base, btn.ghost)} onClick={() => setEditing(c)}>Edit</button>
                  <button type="button" className={cx(btn.base, btn.ghost)} onClick={() => setDeleting(c)}>Delete</button>
                </div>
              </li>
            );
          })}
        </ul>
      )}
      {view && (
        <ChannelEditor view={view} open={adding || editing != null} target={editing}
          onClose={() => { setAdding(false); setEditing(null); }}
          onSaved={(c, created) => { setAdding(false); setEditing(null); setNotice(created ? `Added ${c.name}. Send a test to check it reaches you.` : `Saved ${c.name}.`); void load(); }} />
      )}
      <ConfirmModal open={deleting != null} title={`Delete ${deleting?.name ?? 'this channel'}?`} busy={rowBusy === deleting?.id}
        body="Bell events stop going to this channel. Its stored secrets are deleted with it."
        onClose={() => setDeleting(null)}
        onConfirm={() => { const c = deleting; if (!c) return; void act(c, async () => { await deleteChannel(c.id); return `Deleted ${c.name}.`; }).then(() => setDeleting(null)); }} />
    </section>
  );
}
