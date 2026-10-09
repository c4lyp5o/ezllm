// RULES — per-model eligibility editor: usage caps + allowed-use windows.
//
// The router runs every combo candidate through internal/rules before a hop:
// outside the window (or over the ledger cap) the candidate is skipped, and
// when nothing is eligible the request is refused with 429 rule_refusal.
// The status chip uses windowState() from api.ts — a faithful mirror of
// Window.InWindow — so "open now" here is what the router decides right now.
import { useCallback, useEffect, useState } from 'react';
import {
  ApiError, accountModels, createModelRule, deleteModelRule, formatDays, get, modelRules,
  updateModelRule, windowState,
  type Account, type ModelRow, type ModelRule,
} from '../api';
import { Chip, ConfirmModal, Modal, SectionLabel, SkeletonRows, Toggle, btn, cx, formatTokens, inputCls, labelCls } from '../ui';

const CAP_WINDOWS = ['5h', 'daily', 'weekly', 'monthly'] as const;
const DAY_NAMES = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
const TZ_HINTS = ['Asia/Kuala_Lumpur', 'UTC', '+08:00', 'Asia/Jakarta'];

type Draft = {
  account_id: number; model_id: string;
  cap_tokens: number; cap_window: string;
  win_start: string; win_end: string; win_days: string; win_tz: string;
  enabled: boolean; note: string;
};

const newDraft = (accountID: number): Draft => ({
  account_id: accountID, model_id: '', cap_tokens: 0, cap_window: 'monthly',
  win_start: '', win_end: '', win_days: '', win_tz: 'Asia/Kuala_Lumpur',
  enabled: true, note: '',
});

const draftOf = (r: ModelRule): Draft => ({
  account_id: r.account_id, model_id: r.model_id,
  cap_tokens: r.cap_tokens, cap_window: r.cap_window,
  win_start: r.win_start ?? '', win_end: r.win_end ?? '',
  win_days: r.win_days ?? '', win_tz: r.win_tz,
  enabled: r.enabled, note: r.note ?? '',
});

const errText = (e: unknown) =>
  e instanceof ApiError
    ? `${e.detail.message}${e.detail.hint ? ` — ${e.detail.hint}` : ''}`
    : e instanceof Error ? e.message : String(e);

// write-side validation mirror (store.ValidateModelRule): windows are
// both-or-neither and start!=end — reject before the round-trip.
function windowDraftError(d: Draft): string {
  const { win_start: s, win_end: e } = d;
  if (!!s !== !!e) return 'window needs BOTH start and end (or neither)';
  if (s && s === e) return 'start and end cannot be equal';
  if (d.cap_tokens < 0) return 'cap cannot be negative';
  return '';
}

function capLabel(r: ModelRule): string {
  if (!r.cap_tokens) return 'unlimited';
  return `${formatTokens(r.cap_tokens)} / ${r.cap_window}`;
}

function windowLabel(r: ModelRule): string {
  if (!r.win_start && !r.win_end) return 'always';
  const days = formatDays(r.win_days);
  return `${r.win_start}–${r.win_end} · ${days === 'any day' ? 'daily' : days} · ${r.win_tz}`;
}

function StatusChip({ rule }: { rule: ModelRule }) {
  if (!rule.enabled) return <Chip tone="neutral">disabled</Chip>;
  const w = windowState(rule);
  if (!w.open) return <Chip tone="warn" title={`window: ${w.label}`}>closed now</Chip>;
  return w.label === 'always'
    ? <Chip tone="ok">always open</Chip>
    : <Chip tone="ok" title={`window: ${w.label}`}>open now</Chip>;
}

function RuleModal({ open, editing, accounts, models, draft, setDraft,
  onClose, onSave, saving, err }: {
  open: boolean; editing: ModelRule | null; accounts: Account[]; models: ModelRow[];
  draft: Draft | null; setDraft: (d: Draft) => void;
  onClose: () => void; onSave: () => void; saving: boolean; err: string;
}) {
  if (!draft) return null;
  const d = draft;
  const set = (p: Partial<Draft>) => setDraft({ ...d, ...p });
  const wErr = windowDraftError(d);
  const wState = windowState(d);
  const account = accounts.find((a) => a.id === d.account_id);

  const toggleDay = (i: number) => {
    const cur = d.win_days.split(',').map((x) => x.trim()).filter(Boolean).map(Number);
    const next = cur.includes(i) ? cur.filter((x) => x !== i) : [...cur, i].sort((a, b) => a - b);
    set({ win_days: next.join(',') });
  };

  return (
    <Modal
      open={open} onClose={onClose} wide
      title={editing ? `Edit ${editing.model_id}` : 'New rule'}
      subtitle={editing ? `PATCH /admin/model-rules/${editing.id}` : 'POST /admin/model-rules'}
      footer={
        <div className="flex items-center justify-between gap-3">
          <span className="text-[12px] text-bad">{err}</span>
          <div className="flex gap-2">
            <button type="button" className={cx(btn.base, btn.ghost)} onClick={onClose}>Cancel</button>
            <button type="button" className={cx(btn.base, btn.primary)} disabled={saving || !!wErr} onClick={onSave}>
              {saving ? 'Saving…' : 'Save rule'}
            </button>
          </div>
        </div>
      }
    >
      <div className="grid gap-4 sm:grid-cols-2">
        <div>
          <label className={labelCls} htmlFor="rl-account">account</label>
          <select id="rl-account" className={inputCls} value={d.account_id}
            onChange={(e) => set({ account_id: Number(e.target.value) })}>
            {accounts.map((a) => (
              <option key={a.id} value={a.id}>{a.namespace} · {a.name}</option>
            ))}
          </select>
        </div>
        <div>
          <label className={labelCls} htmlFor="rl-model">model id</label>
          <input id="rl-model" className={cx(inputCls, 'font-mono')} list="rl-models"
            placeholder={account ? `${account.namespace}/model` : 'model id'}
            value={d.model_id} onChange={(e) => set({ model_id: e.target.value })} />
          <datalist id="rl-models">
            {models.map((m) => <option key={m.id} value={m.id} />)}
          </datalist>
          <p className="mt-1 text-[11px] text-mute">bare id inside this account's namespace; router keys on (account, model)</p>
        </div>

        <div>
          <label className={labelCls} htmlFor="rl-cap">cap tokens (0 = unlimited)</label>
          <div className="flex gap-2">
            <input id="rl-cap" type="number" min={0} step={1000}
              className={cx(inputCls, 'w-40 font-mono tabular-nums')}
              value={d.cap_tokens} onChange={(e) => set({ cap_tokens: Number(e.target.value) || 0 })} />
            <select className={cx(inputCls, 'w-36')} value={d.cap_window}
              onChange={(e) => set({ cap_window: e.target.value })}>
              {CAP_WINDOWS.map((w) => <option key={w} value={w}>{w}</option>)}
            </select>
          </div>
        </div>
        <div>
          <label className={labelCls} htmlFor="rl-tz">timezone</label>
          <input id="rl-tz" className={cx(inputCls, 'font-mono')} list="rl-tzs"
            value={d.win_tz} onChange={(e) => set({ win_tz: e.target.value })} />
          <datalist id="rl-tzs">
            {TZ_HINTS.map((t) => <option key={t} value={t} />)}
          </datalist>
        </div>

        <div>
          <label className={labelCls} htmlFor="rl-start">window start — end</label>
          <div className="flex items-center gap-2">
            <input id="rl-start" type="time" className={cx(inputCls, 'w-32 font-mono')}
              value={d.win_start} onChange={(e) => set({ win_start: e.target.value })} />
            <span className="text-mute">→</span>
            <input aria-label="window end" type="time" className={cx(inputCls, 'w-32 font-mono')}
              value={d.win_end} onChange={(e) => set({ win_end: e.target.value })} />
          </div>
          <p className="mt-1 text-[11px] text-mute">end &lt; start wraps midnight; leave both empty for always-open</p>
        </div>
        <div>
          <label className={labelCls}>allowed days</label>
          <div className="flex flex-wrap gap-1.5">
            {DAY_NAMES.map((name, i) => {
              const on = d.win_days.split(',').map((x) => x.trim()).filter(Boolean).map(Number).includes(i);
              return (
                <button key={name} type="button" onClick={() => toggleDay(i)}
                  className={cx('rounded-md border px-2 py-1 font-mono text-[11px] transition-colors',
                    on ? 'border-accent bg-accent-dim text-accent' : 'border-line bg-raised text-mute hover:border-line-strong')}>
                  {name}
                </button>
              );
            })}
          </div>
          <p className="mt-1 text-[11px] text-mute">none selected = every day</p>
        </div>

        <div className="sm:col-span-2 flex flex-wrap items-center gap-4 border-t border-line pt-4">
          <Toggle checked={d.enabled} onChange={(v) => set({ enabled: v })} label="rule enabled" />
          <span className="text-[12px] text-dim">{d.enabled ? 'rule enforces' : 'rule ignored by router'}</span>
          <span className="ml-auto">
            {wErr
              ? <Chip tone="bad">{wErr}</Chip>
              : <Chip tone={wState.open ? 'ok' : 'warn'}>
                  {d.enabled ? (wState.open ? '▸ would allow now' : '▸ would refuse now') : '▸ not evaluated (disabled)'}
                </Chip>}
          </span>
        </div>
        <div className="sm:col-span-2">
          <label className={labelCls} htmlFor="rl-note">note</label>
          <input id="rl-note" className={inputCls} placeholder="e.g. day model, night fallback"
            value={d.note} onChange={(e) => set({ note: e.target.value })} />
        </div>
      </div>
    </Modal>
  );
}

export default function Rules() {
  const [rules, setRules] = useState<ModelRule[] | null>(null);
  const [accounts, setAccounts] = useState<Account[] | null>(null);
  const [err, setErr] = useState('');
  const [open, setOpen] = useState(false);
  const [editing, setEditing] = useState<ModelRule | null>(null);
  const [pendingDelete, setPendingDelete] = useState<ModelRule | null>(null);
  const [draft, setDraft] = useState<Draft | null>(null);
  const [models, setModels] = useState<ModelRow[]>([]);
  const [saving, setSaving] = useState(false);
  const [modalErr, setModalErr] = useState('');
  const [busy, setBusy] = useState<number | null>(null);

  const load = useCallback(async () => {
    try {
      const [r, a] = await Promise.all([modelRules(), get<Account[]>('/admin/accounts')]);
      setRules(r); setAccounts(a); setErr('');
    } catch (e) { setErr(errText(e)); }
  }, []);

  useEffect(() => { void load(); }, [load]);

  // models for the datalist follow the modal's account
  useEffect(() => {
    if (!open) return;
    let dead = false;
    accountModels(draft?.account_id ?? 0)
      .then((m) => { if (!dead) setModels(m); })
      .catch(() => { if (!dead) setModels([]); });
    return () => { dead = true; };
  }, [open, draft?.account_id]);

  const startCreate = () => {
    setEditing(null);
    setDraft(newDraft(accounts?.[0]?.id ?? 1));
    setModalErr('');
    setOpen(true);
  };
  const startEdit = (r: ModelRule) => {
    setEditing(r);
    setDraft(draftOf(r));
    setModalErr('');
    setOpen(true);
  };

  const save = async () => {
    if (!draft) return;
    const modelID = draft.model_id.trim();
    if (!modelID) { setModalErr('model id is required'); return; }
    const payload = { ...draft, model_id: modelID };
    setSaving(true); setModalErr('');
    try {
      if (editing) await updateModelRule(editing.id, payload);
      else await createModelRule(payload);
      setOpen(false);
      await load();
    } catch (e) { setModalErr(errText(e)); }
    finally { setSaving(false); }
  };

  // row toggle: send the FULL row — a PATCH without window fields clears them
  const setEnabled = async (r: ModelRule, enabled: boolean) => {
    setBusy(r.id);
    try { await updateModelRule(r.id, { ...draftOf(r), enabled }); await load(); }
    catch (e) { setErr(errText(e)); }
    finally { setBusy(null); }
  };

  const remove = async (r: ModelRule) => {
    setBusy(r.id);
    try { await deleteModelRule(r.id); await load(); }
    catch (e) { setErr(errText(e)); }
    finally { setBusy(null); }
  };

  const accountName = (id: number) => accounts?.find((a) => a.id === id)?.namespace ?? `#${id}`;

  return (
    <div className="space-y-5">
      <div className="flex items-center justify-between gap-3">
        <div>
          <SectionLabel>Model rules</SectionLabel>
          <p className="mt-1 max-w-2xl text-[12.5px] text-mute">
            Eligibility per (account, model): a ledger usage cap plus an allowed-use window in the rule's timezone.
            A candidate outside its rule is skipped by the router — if no candidate is eligible the request is
            refused with <span className="font-mono text-dim">429 rule_refusal</span>.
          </p>
        </div>
        <button type="button" className={cx(btn.base, btn.primary)} onClick={startCreate}>+ New rule</button>
      </div>

      {err && <div className="rounded-lg border border-bad/40 bg-[rgba(248,113,113,0.06)] px-4 py-2.5 text-[13px] text-bad">{err}</div>}

      {!rules || !accounts ? (
        <SkeletonRows rows={4} cols={6} />
      ) : rules.length === 0 ? (
        <div className="rounded-xl border border-line bg-surface px-6 py-12 text-center text-[13px] text-mute">
          No rules yet — add one to cap a model or hold it to daytime (or nighttime) hours.
        </div>
      ) : (
        <div className="overflow-x-auto rounded-xl border border-line bg-surface">
          <table className="w-full min-w-[900px] text-[12.5px]">
            <thead>
              <tr className="text-left text-[10.5px] uppercase tracking-[0.1em] text-mute">
                <th className="px-5 py-2.5 font-medium">status</th>
                <th className="px-3 py-2.5 font-medium">account</th>
                <th className="px-3 py-2.5 font-medium">model</th>
                <th className="px-3 py-2.5 font-medium">cap</th>
                <th className="px-3 py-2.5 font-medium">window</th>
                <th className="px-3 py-2.5 font-medium">note</th>
                <th className="px-3 py-2.5 text-right font-medium">enabled</th>
                <th className="px-5 py-2.5 text-right font-medium">actions</th>
              </tr>
            </thead>
            <tbody>
              {rules.map((r) => (
                <tr key={r.id} className="border-t border-line/60 transition-colors hover:bg-[rgba(255,255,255,0.03)]">
                  <td className="whitespace-nowrap px-5 py-2.5"><StatusChip rule={r} /></td>
                  <td className="px-3 py-2.5 text-dim">{accountName(r.account_id)}</td>
                  <td className="px-3 py-2.5 font-mono text-ink">{r.model_id}</td>
                  <td className={cx('px-3 py-2.5 font-mono tabular-nums', r.cap_tokens ? 'text-dim' : 'text-mute')}>{capLabel(r)}</td>
                  <td className="px-3 py-2.5 font-mono text-[11.5px] text-dim">{windowLabel(r)}</td>
                  <td className="max-w-[200px] truncate px-3 py-2.5 text-mute" title={r.note}>{r.note || '—'}</td>
                  <td className="px-3 py-2.5 text-right">
                    <span className="inline-flex justify-end">
                      <Toggle checked={r.enabled} disabled={busy === r.id}
                        onChange={(v) => void setEnabled(r, v)} label={`enable ${r.model_id}`} />
                    </span>
                  </td>
                  <td className="whitespace-nowrap px-5 py-2.5 text-right">
                    <button type="button" className={cx(btn.base, btn.ghost, 'mr-2 px-3 py-1.5 text-[12px]')}
                      onClick={() => startEdit(r)}>Edit</button>
                    <button type="button" className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[12px] text-bad')}
                      onClick={() => setPendingDelete(r)}>Delete</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <RuleModal
        open={open} editing={editing} accounts={accounts ?? []} models={models}
        draft={draft} setDraft={setDraft} onClose={() => setOpen(false)}
        onSave={() => void save()} saving={saving} err={modalErr}
      />
      <ConfirmModal
        open={pendingDelete != null}
        title={pendingDelete ? `Delete rule for ${pendingDelete.model_id}?` : 'Delete rule?'}
        body="The router stops enforcing this rule immediately."
        busy={pendingDelete != null && busy === pendingDelete.id}
        onClose={() => setPendingDelete(null)}
        onConfirm={() => { if (pendingDelete) void remove(pendingDelete).then(() => setPendingDelete(null)); }} />
    </div>
  );
}
