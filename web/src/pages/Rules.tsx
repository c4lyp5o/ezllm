// RULES — per-model eligibility editor: usage caps + allowed-use windows.
//
// The router runs every combo candidate through internal/rules before a hop:
// outside the window (or over the ledger cap) the candidate is skipped, and
// when nothing is eligible the request is refused with 429 rule_refusal.
// The open/closed status chip was removed from the table (it duplicated what
// the window column already says, one column wide at 8+ columns) — the
// window column's times are the same data the chip summarised.
import { useCallback, useEffect, useState } from 'react';
import { motion } from 'motion/react';
import {
  ApiError, accountModels, createModelRule, deleteModelRule, formatDays, get, modelRules,
  updateModelRule,
  type Account, type ModelRow, type ModelRule,
} from '../utils/api';
import { Chip, ConfirmModal, Modal, SectionLabel, SkeletonRows, Toggle, btn, cx, formatTokens, inputCls, labelCls } from '../components/ui';
import { Stagger, staggerRow } from '../components/motion';

const CAP_WINDOWS = ['5h', 'daily', 'weekly', 'monthly'] as const;
const DAY_NAMES = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
// The platform's own timezone list; a hand-picked 4-entry datalist let
// "+08:00" masquerade as a zone (no DST, silently wrong half the year).
const TZS: string[] = (() => {
  try {
    const list = Intl.supportedValuesOf('timeZone');
    return list.includes('Asia/Kuala_Lumpur') ? list : ['Asia/Kuala_Lumpur', ...list];
  } catch {
    return ['Asia/Kuala_Lumpur', 'UTC', 'Asia/Jakarta', 'Asia/Singapore'];
  }
})();

type Draft = {
  account_id: number; model_ids: string[];
  cap_tokens: number; cap_window: string;
  win_start: string; win_end: string; win_days: string; win_tz: string;
  enabled: boolean; note: string;
};

const newDraft = (accountID: number): Draft => ({
  account_id: accountID, model_ids: [], cap_tokens: 0, cap_window: 'monthly',
  win_start: '', win_end: '', win_days: '', win_tz: 'instance',
  enabled: true, note: '',
});

const draftOf = (r: ModelRule): Draft => ({
  account_id: r.account_id, model_ids: [r.model_id],
  cap_tokens: r.cap_tokens, cap_window: r.cap_window,
  win_start: r.win_start ?? '', win_end: r.win_end ?? '',
  win_days: r.win_days ?? '', win_tz: r.win_tz,
  enabled: r.enabled, note: r.note ?? '',
});

const errText = (e: unknown) =>
  e instanceof ApiError
    ? `${e.detail.message}${e.detail.hint ? ` — ${e.detail.hint}` : ''}`
    : e instanceof Error ? e.message : String(e);

// A grouped action is one request per rule, and a failure partway leaves the
// earlier writes stored. Runs the steps in order; on a failure the error says
// which rules already changed, which failed and which were not tried, and the
// caller reloads so the page shows what is actually persisted.
type RuleStep = { model: string; run: () => Promise<unknown> };
async function runRuleSteps(steps: RuleStep[]) {
  for (let i = 0; i < steps.length; i++) {
    try { await steps[i].run(); } catch (e) {
      const list = (xs: RuleStep[]) => xs.map((s) => s.model).join(', ');
      const done = steps.slice(0, i), rest = steps.slice(i + 1);
      throw new Error(`${steps[i].model}: ${errText(e)}`
        + (done.length ? ` — already changed: ${list(done)}` : '')
        + (rest.length ? ` — not attempted: ${list(rest)}` : ''));
    }
  }
}

// write-side validation mirror (store.ValidateModelRule): windows are
// both-or-neither and start!=end — reject before the round-trip.
// Draft.model_ids is UI state; the wire is ONE model_id per rule
// (UNIQUE(account_id, model_id)) and decodeBody runs DisallowUnknownFields, so
// a stray model_ids in a body is a hard 400, not a warning.
function wireOf(d: Draft, modelID: string) {
  return {
    account_id: d.account_id, model_id: modelID,
    cap_tokens: d.cap_tokens, cap_window: d.cap_window,
    win_start: d.win_start, win_end: d.win_end,
    win_days: d.win_days, win_tz: d.win_tz,
    enabled: d.enabled, note: d.note,
  };
}

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

// One table row per SETTING, not per model: rules on the same provider with
// identical cap, window, state and note are one group with several models.
// The wire stays one rule per (account, model) — a group is a view, and
// saving it fans out to its members.
type RuleGroup = { key: string; account_id: number; rules: ModelRule[] };

function groupKey(r: ModelRule): string {
  return [r.account_id, r.cap_tokens, r.cap_tokens ? r.cap_window : '', r.win_start ?? '', r.win_end ?? '',
    r.win_days ?? '', r.win_tz, r.enabled, r.note ?? ''].join('|');
}

function groupRules(rules: ModelRule[]): RuleGroup[] {
  const out = new Map<string, RuleGroup>();
  for (const r of rules) {
    const k = groupKey(r);
    const g = out.get(k);
    if (g) g.rules.push(r);
    else out.set(k, { key: k, account_id: r.account_id, rules: [r] });
  }
  return [...out.values()];
}

function RuleModal({ open, editing, accounts, models, draft, setDraft,
  onClose, onSave, saving, err, existingRuleID }: {
  open: boolean; editing: RuleGroup | null; accounts: Account[]; models: ModelRow[];
  draft: Draft | null; setDraft: (d: Draft) => void;
  onClose: () => void; onSave: () => void; saving: boolean; err: string;
  existingRuleID: (accountID: number, modelID: string) => number | undefined;
}) {
  const [modelQuery, setModelQuery] = useState('');
  // Provider swap invalidates the filter: results are provider-scoped, so a
  // leftover query would silently hide everything and look like a bug.
  useEffect(() => { setModelQuery(''); }, [draft?.account_id]);

  if (!draft) return null;
  const d = draft;
  const set = (p: Partial<Draft>) => setDraft({ ...d, ...p });
  const wErr = windowDraftError(d);
  // Hundreds of models is normal (Vercel AI Gateway ships ~700), so the list
  // is always filtered before it is rendered, and the DOM is capped at the
  // first 300 matches — selected models stay visible even when filtered out,
  // so ticking something can never make it vanish from the list.
  const q = modelQuery.trim().toLowerCase();
  const matches = q
    ? models.filter((m) => m.id.toLowerCase().includes(q) || (m.display ?? '').toLowerCase().includes(q))
    : models;
  // The render list is ALL matches, selected rows included (checked), plus any
  // selected model that the current query filters OUT — otherwise ticking
  // everything under a filter empties the list and you cannot untick it.
  const filteredOut = d.model_ids
    .filter((id) => !matches.some((m) => m.id === id))
    .map((id) => models.find((m) => m.id === id))
    .filter((m): m is ModelRow => !!m);
  const matchedModels = [...filteredOut, ...matches];

  const toggleDay = (i: number) => {
    const cur = d.win_days.split(',').map((x) => x.trim()).filter(Boolean).map(Number);
    const next = cur.includes(i) ? cur.filter((x) => x !== i) : [...cur, i].sort((a, b) => a - b);
    set({ win_days: next.join(',') });
  };

  return (
    <Modal
      open={open} onClose={onClose} wide
      title={editing ? 'Edit rule' : 'New rule'}
      footer={
        <div className="flex items-center justify-between gap-3">
          <span className="text-[13px] text-bad">{err}</span>
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
          <label className={labelCls} htmlFor="rl-account">Provider</label>
          {/* Fixed while editing: the group's rules belong to this provider. */}
          <select id="rl-account" className={inputCls} value={d.account_id} disabled={!!editing}
            onChange={(e) => set({ account_id: Number(e.target.value) })}>
            {accounts.map((a) => (
              <option key={a.id} value={a.id}>{a.namespace} · {a.name}</option>
            ))}
          </select>
        </div>
        <div>
          <label className={labelCls} htmlFor="rl-model">Model</label>
          {models.length > 0 && (
            <input id="rl-model" type="search" className={cx(inputCls, 'mb-1.5 font-mono')}
              placeholder={`search ${models.length} model${models.length === 1 ? '' : 's'}…`}
              value={modelQuery} onChange={(e) => setModelQuery(e.target.value)} />
          )}
          <div id="rl-model-list" role="group" aria-label="models"
            className="max-h-44 space-y-0.5 overflow-y-auto rounded-lg border border-line bg-raised p-1.5">
            {models.length === 0 ? (
              <p className="px-2 py-1.5 text-[12px] text-mute">No models on this provider — sync it on Providers first.</p>
            ) : matchedModels.length === 0 ? (
              <p className="px-2 py-1.5 text-[12px] text-mute">No model matches “{modelQuery.trim()}”.</p>
            ) : matchedModels.slice(0, 300).map((m) => {
              const on = d.model_ids.includes(m.id);
              const inGroup = editing?.rules.some((r) => r.model_id === m.id) ?? false;
              const hasRule = !inGroup && existingRuleID(d.account_id, m.id) != null;
              return (
                <label key={m.id} htmlFor={`rl-model-${m.id}`} className={cx(
                  'flex items-center gap-2 rounded-md px-2 py-1.5 font-mono text-[12px] transition-colors',
                  'cursor-pointer hover:bg-wash', on ? 'bg-accent-dim text-accent' : 'text-dim')}>
                  <input id={`rl-model-${m.id}`} type="checkbox" checked={on}
                    onChange={() => set({ model_ids: on ? d.model_ids.filter((x) => x !== m.id) : [...d.model_ids, m.id] })}
                    className="h-3.5 w-3.5 accent-accent" />
                  <span className="min-w-0 flex-1 truncate">{m.id}</span>
                  {hasRule && <span className="text-[11px] text-mute">has rule</span>}
                </label>
              );
            })}
          </div>
          {models.length > 0 && (
            <div className="mt-1 flex flex-wrap items-center justify-between gap-2 text-[11px] text-mute">
              <span>
                {d.model_ids.length} selected
                {matchedModels.length > 300 && <> · showing 300 of {matchedModels.length} matches</>}
              </span>
              {matches.length > 0 && (
                <button type="button"
                  className="font-medium text-dim underline-offset-2 hover:text-ink hover:underline"
                  onClick={() => set({
                    model_ids: [...new Set([...d.model_ids, ...matches.map((m) => m.id)])],
                  })}>
                  select all {matches.length} match{matches.length === 1 ? '' : 'es'}
                </button>
              )}
            </div>
          )}
        </div>

        <div>
          <label className={labelCls} htmlFor="rl-cap">Cap tokens (0 = unlimited)</label>
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
          <label className={labelCls} htmlFor="rl-tz">Timezone</label>
          <select id="rl-tz" className={cx(inputCls, 'font-mono')} value={d.win_tz}
            onChange={(e) => set({ win_tz: e.target.value })}>
            {/* The browser's own IANA list, so an exotic zone can't be typed
                into a typo the router would silently treat as UTC. */}
            {!TZS.includes(d.win_tz) && d.win_tz && d.win_tz !== 'instance' && <option value={d.win_tz}>{d.win_tz}</option>}
            <option value="instance">Instance timezone</option>
            {TZS.map((t) => <option key={t} value={t}>{t}</option>)}
          </select>
        </div>

        <div>
          <label className={labelCls} htmlFor="rl-start">Window start — end</label>
          <div className="flex items-center gap-2">
            <input id="rl-start" type="time" className={cx(inputCls, 'w-32 font-mono')}
              value={d.win_start} onChange={(e) => set({ win_start: e.target.value })} />
            <span className="text-mute">→</span>
            <input aria-label="window end" type="time" className={cx(inputCls, 'w-32 font-mono')}
              value={d.win_end} onChange={(e) => set({ win_end: e.target.value })} />
          </div>
          <p className="mt-1 text-[12px] text-mute">end &lt; start wraps midnight; leave both empty for always-open</p>
        </div>
        <div>
          <label className={labelCls}>Allowed days</label>
          {/* The day chips are a MULTI-select, so the house selection mark (a
              1px accent line under a label) does not apply here — a filled
              chip is the honest affordance for a toggle. They do get the
              global press squeeze, and aria-pressed so a screen reader
              announces the state. */}
          <div className="flex flex-wrap gap-1.5">
            {DAY_NAMES.map((name, i) => {
              const on = d.win_days.split(',').map((x) => x.trim()).filter(Boolean).map(Number).includes(i);
              return (
                <button key={name} type="button" onClick={() => toggleDay(i)} aria-pressed={on}
                  className={cx('rounded-md border px-2 py-1 font-mono text-[12px] transition-colors',
                    on ? 'border-accent bg-accent-dim text-accent' : 'border-line bg-raised text-mute hover:border-line-strong')}>
                  {name}
                </button>
              );
            })}
          </div>
          <p className="mt-1 text-[12px] text-mute">none selected = every day</p>
        </div>

        <div className="sm:col-span-2 flex flex-wrap items-center gap-4 border-t border-line pt-4">
          <Toggle checked={d.enabled} onChange={(v) => set({ enabled: v })} label="Enabled" />
          {/* No routing reveal here: the live "would allow/would refuse" chip
              told the operator exactly what the router decides right now, which
              is the sort of behaviour detail that belongs inside ezllm. Window
              validity still surfaces as a blocking error chip. */}
          <span className="ml-auto">{wErr && <Chip tone="bad">{wErr}</Chip>}</span>
        </div>
        <div className="sm:col-span-2">
          <label className={labelCls} htmlFor="rl-note">Note</label>
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
  const [editing, setEditing] = useState<RuleGroup | null>(null);
  const [pendingDelete, setPendingDelete] = useState<RuleGroup | null>(null);
  const [draft, setDraft] = useState<Draft | null>(null);
  const [models, setModels] = useState<ModelRow[]>([]);
  const [saving, setSaving] = useState(false);
  const [modalErr, setModalErr] = useState('');
  const [busy, setBusy] = useState<string | null>(null);

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
  const startEdit = (g: RuleGroup) => {
    setEditing(g);
    setDraft({ ...draftOf(g.rules[0]), model_ids: g.rules.map((r) => r.model_id) });
    setModalErr('');
    setOpen(true);
  };

  const save = async () => {
    if (!draft) return;
    const modelIDs = [...new Set(draft.model_ids.map((m) => m.trim()).filter(Boolean))];
    if (modelIDs.length === 0) { setModalErr('select at least one model'); return; }
    // Models unticked from the group being edited lose their rule.
    const dropped = editing?.rules.filter((r) => !modelIDs.includes(r.model_id)) ?? [];
    // One request per model: honest about a large fan-out instead of hanging
    // silently while it fires hundreds of PATCH/POST calls.
    if (modelIDs.length + dropped.length > 20 && !window.confirm(
      `This will change ${modelIDs.length + dropped.length} rules (one request each). Continue?`)) {
      return;
    }
    setSaving(true); setModalErr('');
    try {
      // Each checked model is its own rule. A model that already has one is
      // PATCHed with these settings instead of re-POSTed (that is one row per
      // (account, model) by schema), so multi-select also acts as apply-to-many.
      await runRuleSteps([
        ...modelIDs.map((modelID) => ({ model: modelID, run: () => {
          const existing = rules?.find((r) => r.account_id === draft.account_id && r.model_id === modelID)?.id;
          return existing != null ? updateModelRule(existing, wireOf(draft, modelID)) : createModelRule(wireOf(draft, modelID));
        } })),
        ...dropped.map((r) => ({ model: r.model_id, run: () => deleteModelRule(r.id) })),
      ]);
      setOpen(false);
      await load();
    } catch (e) {
      // Reload first: a retry then PATCHes the rules this attempt created.
      await load();
      setModalErr(errText(e));
    }
    finally { setSaving(false); }
  };

  // row toggle: send the FULL row — a PATCH without window fields clears them
  const setEnabled = async (g: RuleGroup, enabled: boolean) => {
    setBusy(g.key);
    try {
      await runRuleSteps(g.rules.map((r) => ({ model: r.model_id, run: () => updateModelRule(r.id, { ...wireOf(draftOf(r), r.model_id), enabled }) })));
      await load();
    }
    catch (e) { await load(); setErr(errText(e)); }
    finally { setBusy(null); }
  };

  const remove = async (g: RuleGroup) => {
    setBusy(g.key);
    try { await runRuleSteps(g.rules.map((r) => ({ model: r.model_id, run: () => deleteModelRule(r.id) }))); await load(); }
    catch (e) { await load(); setErr(errText(e)); }
    finally { setBusy(null); }
  };

  const accountName = (id: number) => accounts?.find((a) => a.id === id)?.namespace ?? `#${id}`;

  return (
    <div className="space-y-5">
      <div className="flex items-center justify-between gap-3">
        <div>
          <SectionLabel>Model rules</SectionLabel>
          <p className="mt-1 max-w-2xl text-[13px] text-mute">
            Eligibility per (provider, model): a ledger usage cap plus an allowed-use window in the rule's timezone.
            A candidate outside its rule is skipped by the router — if no candidate is eligible the request is
            refused with <span className="font-mono text-dim">429 rule_refusal</span>.
          </p>
        </div>
        <button type="button" className={cx(btn.base, btn.primary)} onClick={startCreate}>+ New rule</button>
      </div>

      {err && <div className="rounded-lg border border-bad/40 bg-bad-dim px-4 py-2.5 text-[13px] text-bad">{err}</div>}

      {!rules || !accounts ? (
        <SkeletonRows rows={4} cols={6} />
      ) : rules.length === 0 ? (
        <div className="rounded-xl border border-line bg-surface px-6 py-12 text-center text-[13px] text-mute">
          No rules yet — add one to cap a model or hold it to daytime (or nighttime) hours.
        </div>
      ) : (
        <div className="overflow-x-auto rounded-xl border border-line bg-surface">
          <Stagger>
          <table className="w-full min-w-[900px] text-[13px]">
            <thead>
              <tr className="text-left text-[11px] text-mute">
                <th className="px-5 py-2.5 font-medium">Enabled</th>
                <th className="px-3 py-2.5 font-medium">Provider</th>
                <th className="px-3 py-2.5 font-medium">Models</th>
                <th className="px-3 py-2.5 font-medium">Cap</th>
                <th className="px-3 py-2.5 font-medium">Window</th>
                <th className="px-3 py-2.5 font-medium">Note</th>
                <th className="px-5 py-2.5 text-right font-medium">Actions</th>
              </tr>
            </thead>
            <tbody>
              {groupRules(rules).map((g) => { const r = g.rules[0]; return (
                // motion.tr: the rows fade in as a sequence. Only opacity
                // animates — a translate on a table row detaches it from its
                // own columns (see staggerRow).
                <motion.tr key={g.key} variants={staggerRow}
                  className="border-t border-line/60 transition-colors hover:bg-wash">
                  <td className="px-5 py-2.5">
                    <Toggle checked={r.enabled} disabled={busy === g.key}
                      onChange={(v) => void setEnabled(g, v)} label={`enable ${g.rules.map((x) => x.model_id).join(', ')}`} />
                  </td>
                  <td className="px-3 py-2.5 text-dim">{accountName(r.account_id)}</td>
                  <td className="px-3 py-2.5">
                    <div className="flex max-w-[360px] flex-wrap gap-1.5">
                      {g.rules.map((x) => <Chip key={x.id} className="font-code text-ink">{x.model_id}</Chip>)}
                    </div>
                  </td>
                  <td className={cx('px-3 py-2.5 font-mono tabular-nums', r.cap_tokens ? 'text-dim' : 'text-mute')}>{capLabel(r)}</td>
                  <td className="px-3 py-2.5 font-mono text-[12px] text-dim">{windowLabel(r)}</td>
                  <td className="max-w-[200px] truncate px-3 py-2.5 text-mute" title={r.note}>{r.note || '—'}</td>
                  <td className="whitespace-nowrap px-5 py-2.5 text-right">
                    <button type="button" className={cx(btn.base, btn.ghost, 'mr-2 px-3 py-1.5 text-[13px]')}
                      onClick={() => startEdit(g)}>Edit</button>
                    <button type="button" className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[13px] text-bad')}
                      onClick={() => setPendingDelete(g)}>Delete</button>
                  </td>
                </motion.tr>
              ); })}
            </tbody>
          </table>
          </Stagger>
        </div>
      )}

      <RuleModal
        open={open} editing={editing} accounts={accounts ?? []} models={models}
        draft={draft} setDraft={setDraft} onClose={() => setOpen(false)}
        onSave={() => void save()} saving={saving} err={modalErr}
        existingRuleID={(a, m) => rules?.find((r) => r.account_id === a && r.model_id === m)?.id}
      />
      <ConfirmModal
        open={pendingDelete != null}
        title={pendingDelete
          ? pendingDelete.rules.length === 1 ? `Delete rule for ${pendingDelete.rules[0].model_id}?` : `Delete rule for ${pendingDelete.rules.length} models?`
          : 'Delete rule?'}
        body="The router stops enforcing this rule immediately."
        busy={pendingDelete != null && busy === pendingDelete.key}
        onClose={() => setPendingDelete(null)}
        onConfirm={() => { if (pendingDelete) void remove(pendingDelete).then(() => setPendingDelete(null)); }} />
    </div>
  );
}
