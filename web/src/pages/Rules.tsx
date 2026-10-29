// RULES — per-model eligibility: usage caps + allowed-use windows.
//
// The router runs every combo candidate through internal/rules before a hop:
// outside the window (or over the ledger cap) the candidate is skipped, and
// when nothing is eligible the request is refused with 429 rule_refusal.
// The open/closed status chip stays off this page on purpose: the window
// column's times are the same data, and what the router decides right now is
// behaviour that belongs inside ezllm.
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useSearchParams } from 'react-router-dom';
import {
  ApiError, accountModels, createModelRule, deleteModelRule, get, modelRules, updateModelRule,
  type Account, type ModelRow, type ModelRule,
} from '../utils/api';
import {
  Chip, ConfirmModal, EmptyState, Modal, Segmented, SectionLabel, SkeletonRows, Toggle, btn, cx, inputCls, labelCls,
} from '../components/ui';
import { nfmt } from '../utils/lib';
import {
  CAP_QUICK, CAP_WINDOWS, DAY_NAMES, WINDOW_PRESETS, activePreset, capLabel, capPhrase, daysCsv, groupIssues, groupRules,
  hasErrors, parseDays, ruleSentence, tzPhrase, validateDraft, windowPhrase,
  type Draft, type RuleGroup,
} from '../utils/rules';

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

const snapshot = (d: Draft) => JSON.stringify({ ...d, model_ids: [...d.model_ids].sort() });

// ── the editor ──────────────────────────────────────────────────────────────

type EditorTarget = { open: boolean; editing: RuleGroup | null; template: RuleGroup | null; account?: number };
const CLOSED: EditorTarget = { open: false, editing: null, template: null };

function SubHead({ n, title, hint }: { n: number; title: string; hint?: string }) {
  return (
    <div className="mb-2.5 flex flex-wrap items-baseline gap-x-3">
      <span className="font-mono text-[12px] text-mute">{n}</span>
      <SectionLabel>{title}</SectionLabel>
      {hint && <span className="text-[12px] text-mute">{hint}</span>}
    </div>
  );
}

function RuleModal({ target, accounts, rules, onClose, onSave }: {
  target: EditorTarget; accounts: Account[]; rules: ModelRule[];
  onClose: () => void; onSave: (d: Draft, editing: RuleGroup | null) => Promise<void>;
}) {
  const { open, editing, template } = target;
  const [d, setD] = useState<Draft>(newDraft(1));
  const initial = useRef('');
  const [models, setModels] = useState<ModelRow[] | null>(null);
  const [modelQuery, setModelQuery] = useState('');
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState('');
  const [showAll, setShowAll] = useState(false);

  useEffect(() => {
    if (!open) return;
    setErr(''); setSaving(false); setShowAll(false); setModelQuery('');
    const first = target.account ?? accounts[0]?.id ?? 1;
    const next = editing ? { ...draftOf(editing.rules[0]), model_ids: editing.rules.map((r) => r.model_id) }
      : template ? { ...draftOf(template.rules[0]), model_ids: [] }
        : newDraft(first);
    setD(next);
    initial.current = snapshot(next);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, editing, template]);

  // The model list follows the chosen provider.
  useEffect(() => {
    if (!open) return;
    let dead = false;
    setModels(null);
    accountModels(d.account_id).then((m) => { if (!dead) setModels(m); }).catch(() => { if (!dead) setModels([]); });
    return () => { dead = true; };
  }, [open, d.account_id]);
  // A provider swap invalidates the filter: results are provider-scoped, so a
  // leftover query would silently hide everything and look like a bug.
  useEffect(() => { setModelQuery(''); }, [d.account_id]);

  const set = (p: Partial<Draft>) => setD((cur) => ({ ...cur, ...p }));
  const errors = validateDraft(d);
  const dirty = open && snapshot(d) !== initial.current;
  const list = models ?? [];
  const existing = (id: string) => rules.some((r) => r.account_id === d.account_id && r.model_id === id);
  const providerName = accounts.find((a) => a.id === d.account_id)?.name ?? `#${d.account_id}`;

  // Hundreds of models is normal (Vercel AI Gateway ships ~700), so the list is
  // always filtered before it is rendered and capped at 300 rows. Selected
  // models stay visible even when filtered out, so ticking something can never
  // make it vanish from the list.
  const q = modelQuery.trim().toLowerCase();
  const matches = q ? list.filter((m) => m.id.toLowerCase().includes(q) || (m.display ?? '').toLowerCase().includes(q)) : list;
  // A selected model the provider no longer lists still gets a row (marked), so
  // a stale rule can be seen and unticked, which is how it gets deleted.
  const missing = models === null ? [] : d.model_ids.filter((id) => !list.some((m) => m.id === id));
  const filteredOut = d.model_ids.filter((id) => !matches.some((m) => m.id === id)).map((id) => list.find((m) => m.id === id)).filter((m): m is ModelRow => !!m);
  const shown: ModelRow[] = [...missing.map((id) => ({ id })), ...filteredOut, ...matches];

  const days = parseDays(d.win_days);
  const toggleDay = (i: number) => set({ win_days: daysCsv(days.includes(i) ? days.filter((x) => x !== i) : [...days, i]) });
  const preset = activePreset(d);

  const submit = async () => {
    if (saving) return;
    if (hasErrors(errors)) { setShowAll(true); return; }
    setSaving(true); setErr('');
    try { await onSave(d, editing); } catch (e) { setErr(errText(e)); } finally { setSaving(false); }
  };

  return (
    <Modal open={open} onClose={onClose} wide busy={saving} dirty={dirty} onSubmit={() => void submit()}
      title={editing ? 'Edit rule' : template ? 'Duplicate rule' : 'New rule'}
      subtitle={template ? 'The settings are copied. Choose the models to apply them to.' : 'A cap and a window on the models you pick'}
      footer={(close) => <>
        {showAll && hasErrors(errors) && <span className="mr-auto text-[12px] text-bad">Fix the fields marked in red to save.</span>}
        <button type="button" className={cx(btn.base, btn.ghost)} onClick={close}>Cancel</button>
        <button type="submit" className={cx(btn.base, btn.primary)} disabled={saving || (!!editing && !dirty)}>
          {saving ? 'Saving…' : 'Save rule'}
        </button>
      </>}>
      <div className="space-y-6">
        <section>
          <SubHead n={1} title="Models" hint="Each ticked model gets its own rule with the settings below." />
          <div className="mb-3 max-w-sm">
            <label className={labelCls} htmlFor="rl-account">Provider</label>
            {/* Fixed while editing: the group's rules belong to this provider. */}
            <select id="rl-account" className={inputCls} value={d.account_id} disabled={!!editing}
              onChange={(e) => set({ account_id: Number(e.target.value), model_ids: [] })}>
              {accounts.map((a) => <option key={a.id} value={a.id}>{a.name} · {a.namespace}</option>)}
            </select>
          </div>
          {list.length > 0 && (
            <input id="rl-model" type="search" aria-label="Search models" className={cx(inputCls, 'mb-1.5 font-mono')}
              placeholder={`Search ${nfmt(list.length)} model${list.length === 1 ? '' : 's'}…`}
              value={modelQuery} onChange={(e) => setModelQuery(e.target.value)} />
          )}
          <div id="rl-model-list" role="group" aria-label="models"
            className={cx('max-h-48 space-y-0.5 overflow-y-auto rounded-lg border bg-raised p-1.5', showAll && errors.models ? 'border-bad/60' : 'border-line')}>
            {models === null ? (
              <p className="px-2 py-1.5 text-[12px] text-mute">Loading models…</p>
            ) : list.length === 0 && shown.length === 0 ? (
              <p className="px-2 py-1.5 text-[12px] text-mute">No models on this provider. Sync it on Providers first.</p>
            ) : shown.length === 0 ? (
              <p className="px-2 py-1.5 text-[12px] text-mute">No model matches “{modelQuery.trim()}”.</p>
            ) : shown.slice(0, 300).map((m) => {
              const on = d.model_ids.includes(m.id);
              const inGroup = editing?.rules.some((r) => r.model_id === m.id) ?? false;
              const hasRule = !inGroup && existing(m.id);
              return (
                <label key={m.id} htmlFor={`rl-model-${m.id}`} className={cx(
                  'flex items-center gap-2 rounded-md px-2 py-1.5 font-mono text-[12px] transition-colors',
                  'cursor-pointer hover:bg-wash', on ? 'bg-accent-dim text-accent' : 'text-dim')}>
                  <input id={`rl-model-${m.id}`} type="checkbox" checked={on}
                    onChange={() => set({ model_ids: on ? d.model_ids.filter((x) => x !== m.id) : [...d.model_ids, m.id] })}
                    className="h-3.5 w-3.5 accent-accent" />
                  <span className="min-w-0 flex-1 truncate">{m.id}</span>
                  {missing.includes(m.id) && <span className="text-[11px] text-warn">not in catalog</span>}
                  {hasRule && <span className="text-[11px] text-mute" title="saving replaces its current rule with these settings">has a rule</span>}
                </label>
              );
            })}
          </div>
          <div className="mt-1.5 flex flex-wrap items-center justify-between gap-2 text-[12px] text-mute">
            <span className={showAll && errors.models ? 'text-bad' : undefined}>
              {showAll && errors.models ? errors.models : `${nfmt(d.model_ids.length)} selected`}
              {shown.length > 300 && <> · showing 300 of {nfmt(shown.length)} matches</>}
            </span>
            <span className="flex items-center gap-3">
              {d.model_ids.length > 0 && (
                <button type="button" className="font-medium text-dim underline-offset-2 hover:text-ink hover:underline"
                  onClick={() => set({ model_ids: [] })}>Clear selection</button>
              )}
              {matches.length > 0 && (
                <button type="button" className="font-medium text-dim underline-offset-2 hover:text-ink hover:underline"
                  onClick={() => set({ model_ids: [...new Set([...d.model_ids, ...matches.map((m) => m.id)])] })}>
                  Select all {nfmt(matches.length)} match{matches.length === 1 ? '' : 'es'}
                </button>
              )}
            </span>
          </div>
        </section>

        <section>
          <SubHead n={2} title="Cap" hint="Stop using these models after this many tokens. 0 means no cap." />
          <div className="flex flex-wrap items-center gap-3">
            <input id="rl-cap" aria-label="Cap tokens" type="number" min={0} step={1000}
              className={cx(inputCls, '!w-44 font-mono tabular-nums', errors.cap && 'border-bad/60')}
              value={d.cap_tokens} onChange={(e) => set({ cap_tokens: Math.trunc(Number(e.target.value)) || 0 })} />
            <span className="flex items-center gap-1.5">
              {CAP_QUICK.map((n) => (
                <button key={n} type="button" onClick={() => set({ cap_tokens: n })}
                  className="rounded-md border border-line px-2 py-1 font-mono text-[12px] text-mute hover:border-line-strong hover:text-ink">
                  {nfmt(n)}
                </button>
              ))}
            </span>
          </div>
          {errors.cap && <p className="mt-1 text-[12px] text-bad">{errors.cap}</p>}
          {d.cap_tokens > 0 ? (
            <div className="mt-2.5 flex flex-wrap items-center gap-3">
              <span className="text-[12px] text-mute">Counted per</span>
              <Segmented<string> size="sm" label="Cap period" value={d.cap_window} onChange={(w) => set({ cap_window: w })}
                options={CAP_WINDOWS.map((w) => ({ key: w, label: w === '5h' ? '5 hours' : w === 'daily' ? 'Day' : w === 'weekly' ? 'Week' : 'Month' }))} />
              <span className="font-mono text-[12px] text-dim">{capPhrase(d.cap_tokens, d.cap_window)}</span>
            </div>
          ) : (
            <p className="mt-2 text-[12px] text-mute">No cap: only the window below (if any) limits these models.</p>
          )}
        </section>

        <section>
          <SubHead n={3} title="Window" hint="When these models may be used." />
          <div className="flex flex-wrap items-center gap-1.5">
            {WINDOW_PRESETS.map((p) => (
              <button key={p.key} type="button" title={p.title} aria-pressed={preset === p.key}
                onClick={() => set({ win_start: p.win_start, win_end: p.win_end, win_days: p.win_days })}
                className={cx('rounded-md border px-2.5 py-1 text-[12px] transition-colors',
                  preset === p.key ? 'border-accent bg-accent-dim text-accent' : 'border-line text-mute hover:border-line-strong hover:text-ink')}>
                {p.label}
              </button>
            ))}
          </div>
          <div className="mt-3 grid gap-4 sm:grid-cols-[auto_minmax(0,1fr)]">
            <div>
              <span className={labelCls}>From and to</span>
              <div className="flex items-center gap-2">
                <input id="rl-start" aria-label="Window start" type="time" className={cx(inputCls, '!w-32 font-mono', errors.window && 'border-bad/60')}
                  value={d.win_start} onChange={(e) => set({ win_start: e.target.value })} />
                <span className="text-mute">→</span>
                <input aria-label="Window end" type="time" className={cx(inputCls, '!w-32 font-mono', errors.window && 'border-bad/60')}
                  value={d.win_end} onChange={(e) => set({ win_end: e.target.value })} />
              </div>
              <p className="mt-1 text-[12px] text-mute">An end before the start wraps midnight.</p>
            </div>
            <div>
              <label className={labelCls} htmlFor="rl-tz">Timezone</label>
              <select id="rl-tz" className={cx(inputCls, 'max-w-xs font-mono')} value={d.win_tz} onChange={(e) => set({ win_tz: e.target.value })}>
                {/* The browser's own IANA list, so an exotic zone can't be typed
                    into a typo the router would silently treat as UTC. */}
                {!TZS.includes(d.win_tz) && d.win_tz && d.win_tz !== 'instance' && <option value={d.win_tz}>{d.win_tz}</option>}
                <option value="instance">Instance timezone</option>
                {TZS.map((t) => <option key={t} value={t}>{t}</option>)}
              </select>
            </div>
          </div>
          {errors.window && <p className="mt-1.5 text-[12px] text-bad">{errors.window}</p>}
          <div className="mt-3">
            <div className="mb-1.5 flex flex-wrap items-center gap-x-3 text-[12px]">
              <span className="font-medium text-mute">Days</span>
              <button type="button" className="text-mute hover:text-ink" onClick={() => set({ win_days: '1,2,3,4,5' })}>Weekdays</button>
              <button type="button" className="text-mute hover:text-ink" onClick={() => set({ win_days: '0,6' })}>Weekend</button>
              <button type="button" className="text-mute hover:text-ink" onClick={() => set({ win_days: '' })}>Every day</button>
            </div>
            {/* The day chips are a MULTI-select, so the house selection mark (a
                1px accent line under a label) does not apply: a filled chip is
                the honest affordance for a toggle. aria-pressed announces it. */}
            <div className="flex flex-wrap gap-1.5">
              {DAY_NAMES.map((name, i) => {
                const on = days.includes(i);
                return (
                  <button key={name} type="button" onClick={() => toggleDay(i)} aria-pressed={on}
                    className={cx('rounded-md border px-2 py-1 font-mono text-[12px] transition-colors',
                      on ? 'border-accent bg-accent-dim text-accent' : 'border-line bg-raised text-mute hover:border-line-strong')}>
                    {name}
                  </button>
                );
              })}
            </div>
            <p className="mt-1 text-[12px] text-mute">None selected means every day.</p>
          </div>
        </section>

        <section>
          <SubHead n={4} title="Details" />
          <div className="grid gap-4 sm:grid-cols-[minmax(0,1fr)_auto] sm:items-end">
            <div>
              <label className={labelCls} htmlFor="rl-note">Note</label>
              <input id="rl-note" className={inputCls} placeholder="e.g. day model, night fallback"
                value={d.note} onChange={(e) => set({ note: e.target.value })} />
            </div>
            <label className="flex items-center gap-2.5 pb-2 text-[13px] text-dim">
              <Toggle checked={d.enabled} onChange={(v) => set({ enabled: v })} label="Enabled" />
              {d.enabled ? 'Enabled' : 'Disabled'}
            </label>
          </div>
        </section>

        <div className="rounded-lg border border-line bg-raised/40 px-4 py-3">
          <div className="text-[12px] font-medium text-mute">This rule</div>
          <p className="mt-1 text-[13px] leading-snug text-dim" aria-live="polite">{ruleSentence(d, providerName)}</p>
        </div>

        {err && <div className="rounded-lg border border-bad/30 bg-bad-dim px-4 py-3 text-[13px] text-bad">{err}</div>}
      </div>
    </Modal>
  );
}

// ── the list ────────────────────────────────────────────────────────────────

// Which weekdays a window allows, at a glance. Shown only when days are limited.
function DayStrip({ csv }: { csv: string }) {
  const on = parseDays(csv);
  if (on.length === 0 || on.length === 7) return null;
  return (
    <span className="mt-1 flex gap-0.5" role="img" aria-label={`Allowed days: ${dayList(on)}`}>
      {DAY_NAMES.map((n, i) => (
        <span key={n} title={n} className={cx('w-4 text-center font-mono text-[11px]', on.includes(i) ? 'text-ink' : 'text-mute/50')}>{n[0]}</span>
      ))}
    </span>
  );
}
const dayList = (d: number[]) => d.map((i) => DAY_NAMES[i]).join(', ');

const COLS = 'md:grid-cols-[3rem_9rem_minmax(0,1fr)_9.5rem_12rem_auto]';
const CHIPS_SHOWN = 4;

type Filter = 'all' | 'enabled' | 'disabled' | 'attention';

export default function Rules() {
  const [rules, setRules] = useState<ModelRule[] | null>(null);
  const [accounts, setAccounts] = useState<Account[] | null>(null);
  const [catalogs, setCatalogs] = useState<Record<number, string[] | null>>({});
  const asked = useRef(new Set<number>());
  const [err, setErr] = useState('');
  const [notice, setNotice] = useState('');
  const [editor, setEditor] = useState<EditorTarget>(CLOSED);
  const [pendingDelete, setPendingDelete] = useState<RuleGroup | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [query, setQuery] = useState('');
  const [providerFilter, setProviderFilter] = useState(0);
  const [filter, setFilter] = useState<Filter>('all');
  const [fan, setFan] = useState<{ count: number; resolve: (ok: boolean) => void } | null>(null);
  const [params, setParams] = useSearchParams();

  const load = useCallback(async () => {
    try {
      const [r, a] = await Promise.all([modelRules(), get<Account[]>('/admin/accounts')]);
      setRules(r); setAccounts(a); setErr('');
    } catch (e) { setErr(errText(e)); }
  }, []);
  useEffect(() => { void load(); }, [load]);

  // One catalog fetch per provider that has rules: is the model still listed?
  useEffect(() => {
    if (!rules || !accounts) return;
    for (const id of new Set(rules.map((r) => r.account_id))) {
      if (asked.current.has(id) || !accounts.some((a) => a.id === id)) continue;
      asked.current.add(id);
      accountModels(id)
        .then((ms) => setCatalogs((p) => ({ ...p, [id]: ms.map((m) => m.id) })))
        .catch(() => setCatalogs((p) => ({ ...p, [id]: null })));
    }
  }, [rules, accounts]);

  const groups = useMemo(() => groupRules(rules ?? []), [rules]);
  const acctById = useMemo(() => new Map((accounts ?? []).map((a) => [a.id, a])), [accounts]);
  const issuesOf = useCallback((g: RuleGroup) => groupIssues(g, acctById.get(g.account_id), catalogs[g.account_id]), [acctById, catalogs]);
  const accountName = (id: number) => acctById.get(id)?.name ?? `provider #${id}`;

  // Deep links: /rules?edit=<rule id> opens that rule's group, and
  // /rules?new=1&account=<id> starts a rule on that provider.
  useEffect(() => {
    if (!rules || !accounts) return;
    const edit = Number(params.get('edit'));
    const isNew = params.get('new') === '1';
    if (!edit && !isNew) return;
    const acct = Number(params.get('account')) || undefined;
    const next = new URLSearchParams(params);
    next.delete('edit'); next.delete('new'); next.delete('account');
    setParams(next, { replace: true });
    if (edit) {
      const g = groups.find((x) => x.rules.some((r) => r.id === edit));
      if (g) setEditor({ open: true, editing: g, template: null });
    } else setEditor({ open: true, editing: null, template: null, account: acct });
  }, [rules, accounts, groups, params, setParams]);

  // The editor stays open until the write succeeds, so a failure keeps the
  // operator's work. A large fan-out asks first, in a styled confirm.
  const askFan = (count: number) => new Promise<boolean>((resolve) => setFan({ count, resolve }));

  const save = async (d: Draft, editing: RuleGroup | null) => {
    const modelIDs = [...new Set(d.model_ids.map((m) => m.trim()).filter(Boolean))];
    // Models unticked from the group being edited lose their rule.
    const dropped = editing?.rules.filter((r) => !modelIDs.includes(r.model_id)) ?? [];
    // One request per model: honest about a large fan-out instead of hanging
    // silently while it fires hundreds of PATCH/POST calls.
    if (modelIDs.length + dropped.length > 20 && !(await askFan(modelIDs.length + dropped.length))) return;
    try {
      // Each checked model is its own rule. A model that already has one is
      // PATCHed with these settings instead of re-POSTed (one row per
      // (account, model) by schema), so multi-select also acts as apply-to-many.
      await runRuleSteps([
        ...modelIDs.map((modelID) => ({ model: modelID, run: () => {
          const existing = rules?.find((r) => r.account_id === d.account_id && r.model_id === modelID)?.id;
          return existing != null ? updateModelRule(existing, wireOf(d, modelID)) : createModelRule(wireOf(d, modelID));
        } })),
        ...dropped.map((r) => ({ model: r.model_id, run: () => deleteModelRule(r.id) })),
      ]);
    } catch (e) {
      // Reload first: a retry then PATCHes the rules this attempt created.
      await load();
      throw e;
    }
    setEditor(CLOSED);
    setNotice(`Saved ${modelIDs.length === 1 ? `the rule for ${modelIDs[0]}` : `${nfmt(modelIDs.length)} rules`}${dropped.length ? `, removed ${nfmt(dropped.length)}` : ''}.`);
    await load();
  };

  // Row toggle: send the FULL row — a PATCH without window fields clears them.
  const setEnabled = async (g: RuleGroup, enabled: boolean) => {
    setBusy(g.key); setNotice('');
    try {
      await runRuleSteps(g.rules.map((r) => ({ model: r.model_id, run: () => updateModelRule(r.id, { ...wireOf(draftOf(r), r.model_id), enabled }) })));
      setNotice(`${enabled ? 'Enabled' : 'Disabled'} ${g.rules.length === 1 ? `the rule for ${g.rules[0].model_id}` : `${nfmt(g.rules.length)} rules`}.`);
      await load();
    }
    catch (e) { await load(); setErr(errText(e)); }
    finally { setBusy(null); }
  };

  const remove = async (g: RuleGroup) => {
    setBusy(g.key); setNotice('');
    try {
      await runRuleSteps(g.rules.map((r) => ({ model: r.model_id, run: () => deleteModelRule(r.id) })));
      setNotice(`Deleted ${g.rules.length === 1 ? `the rule for ${g.rules[0].model_id}` : `${nfmt(g.rules.length)} rules`}.`);
      await load();
    }
    catch (e) { await load(); setErr(errText(e)); }
    finally { setBusy(null); }
  };

  // ── filtering ──
  const q = query.trim().toLowerCase();
  const matches = (g: RuleGroup) => !q || accountName(g.account_id).toLowerCase().includes(q)
    || (acctById.get(g.account_id)?.namespace ?? '').toLowerCase().includes(q)
    || g.rules.some((r) => r.model_id.toLowerCase().includes(q) || (r.note ?? '').toLowerCase().includes(q));
  const attention = (g: RuleGroup) => issuesOf(g).length > 0;
  const shown = groups
    .filter((g) => providerFilter === 0 || g.account_id === providerFilter)
    .filter(matches)
    .filter((g) => filter === 'all' || (filter === 'enabled' ? g.rules[0].enabled : filter === 'disabled' ? !g.rules[0].enabled : attention(g)));

  const total = rules?.length ?? 0;
  const disabledN = (rules ?? []).filter((r) => !r.enabled).length;
  const attentionN = groups.filter(attention).length;
  const providers = [...new Set((rules ?? []).map((r) => r.account_id))];
  const openNew = () => setEditor({ open: true, editing: null, template: null });

  return (
    <div className="space-y-5">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <SectionLabel>Model rules</SectionLabel>
          <p className="mt-1 max-w-2xl text-[13px] text-mute">
            Limit when and how much a model can be used: cap the tokens it may spend, or only allow it at certain hours.
            When a combo hop is outside its rule the router skips it and tries the next one. If nothing is allowed,
            the request is refused with <span className="font-mono text-dim">429 rule_refusal</span>.
          </p>
        </div>
        <button type="button" className={cx(btn.base, btn.primary)} onClick={openNew}>+ New rule</button>
      </div>

      {err && <div className="rounded-lg border border-bad/40 bg-bad-dim px-4 py-2.5 text-[13px] text-bad">{err}</div>}
      {notice && !err && (
        <div className="flex items-center justify-between gap-3 rounded-lg border border-line bg-raised/60 px-4 py-2.5 text-[13px] text-dim" role="status">
          <span>{notice}</span>
          <button type="button" className="text-mute hover:text-ink" aria-label="Dismiss" onClick={() => setNotice('')}>✕</button>
        </div>
      )}

      {rules && accounts && total > 0 && (
        <div className="space-y-3">
          <p className="text-[13px] text-mute" aria-live="polite">
            <span className="text-ink">{nfmt(total)}</span> rule{total === 1 ? '' : 's'} on {nfmt(providers.length)} provider{providers.length === 1 ? '' : 's'}
            {disabledN > 0 && <> · {nfmt(disabledN)} disabled</>}
            {attentionN > 0 && <> · <span className="text-warn">{nfmt(attentionN)} need{attentionN === 1 ? 's' : ''} attention</span></>}
          </p>
          <div className="flex flex-wrap items-center gap-x-5 gap-y-2">
            <input aria-label="Search rules" type="search" placeholder="Search by model, provider or note" value={query}
              onChange={(e) => setQuery(e.target.value)} className={cx(inputCls, 'w-full py-1.5 sm:!w-64')} />
            {providers.length > 1 && (
              <select aria-label="Provider" className={cx(inputCls, '!w-auto py-1.5')} value={providerFilter}
                onChange={(e) => setProviderFilter(Number(e.target.value))}>
                <option value={0}>All providers</option>
                {providers.map((id) => <option key={id} value={id}>{accountName(id)}</option>)}
              </select>
            )}
            <Segmented<Filter> size="sm" label="Show" value={filter} onChange={setFilter}
              options={[
                { key: 'all', label: 'All' }, { key: 'enabled', label: 'Enabled' }, { key: 'disabled', label: 'Disabled' },
                { key: 'attention', label: attentionN > 0 ? `Needs attention (${attentionN})` : 'Needs attention' },
              ]} />
          </div>
        </div>
      )}

      {!rules || !accounts ? (
        <SkeletonRows rows={4} cols={6} />
      ) : rules.length === 0 ? (
        <div className="rounded-xl border border-line bg-surface px-6 py-12 text-center">
          <p className="text-[14px] font-medium">No rules yet</p>
          <p className="mx-auto mt-1 max-w-md text-[13px] text-mute">Add one to cap how much a model can spend, or to keep it to daytime (or night-time) hours.</p>
          <button type="button" className={cx(btn.base, btn.primary, 'mt-5')} onClick={openNew}>+ Create your first rule</button>
        </div>
      ) : shown.length === 0 ? (
        <EmptyState title="No rule matches" hint="Nothing fits the search and filters together."
          action={<button type="button" className={cx(btn.base, btn.ghost)} onClick={() => { setQuery(''); setFilter('all'); setProviderFilter(0); }}>Clear search and filters</button>} />
      ) : (
        <div className="overflow-hidden rounded-xl border border-line bg-surface">
          <div className={cx('hidden gap-x-4 px-5 py-2.5 text-[12px] font-medium text-mute md:grid', COLS)}>
            <span>On</span><span>Provider</span><span>Models</span><span>Cap</span><span>Window</span><span className="text-right">Actions</span>
          </div>
          {shown.map((g) => {
            const r = g.rules[0];
            const issues = issuesOf(g);
            const open = expanded.has(g.key);
            const visible = open ? g.rules : g.rules.slice(0, CHIPS_SHOWN);
            const label = g.rules.map((x) => x.model_id).join(', ');
            return (
              <div key={g.key} className="border-t border-line/60 px-4 py-3 transition-colors first:border-t-0 hover:bg-wash md:px-5">
                <div className={cx('grid items-start gap-x-4 gap-y-2', COLS)}>
                  <div className="flex items-center gap-3 md:block">
                    <Toggle checked={r.enabled} disabled={busy === g.key}
                      onChange={(v) => void setEnabled(g, v)} label={`enable ${label}`} />
                    <span className="text-[13px] font-medium md:hidden">{accountName(g.account_id)}</span>
                  </div>
                  <span className="hidden text-[13px] text-dim md:block" title={acctById.get(g.account_id)?.namespace}>{accountName(g.account_id)}</span>
                  <div className="flex min-w-0 flex-wrap items-center gap-1.5">
                    {visible.map((x) => <Chip key={x.id} className="font-code text-ink">{x.model_id}</Chip>)}
                    {g.rules.length > CHIPS_SHOWN && (
                      <button type="button" className="text-[12px] text-mute hover:text-ink"
                        onClick={() => setExpanded((p) => { const n = new Set(p); if (n.has(g.key)) n.delete(g.key); else n.add(g.key); return n; })}>
                        {open ? 'Show fewer' : `+${nfmt(g.rules.length - CHIPS_SHOWN)} more`}
                      </button>
                    )}
                  </div>
                  <div className={cx('font-mono text-[13px] tabular-nums', r.cap_tokens ? 'text-dim' : 'text-mute')}>
                    <span className="mr-2 font-sans text-[12px] text-mute md:hidden">Cap</span>{capLabel(r)}
                  </div>
                  <div title={tzPhrase(r.win_tz)}>
                    <span className="mr-2 text-[12px] text-mute md:hidden">Window</span>
                    <span className={cx('text-[13px]', r.win_start || r.win_end ? 'text-dim' : 'text-mute')}>{windowPhrase(r)}</span>
                    {(r.win_start || r.win_end) && <span className="block text-[11px] text-mute">{tzPhrase(r.win_tz)}</span>}
                    <DayStrip csv={r.win_days ?? ''} />
                  </div>
                  <div className="flex items-center gap-2 md:justify-end">
                    <button type="button" className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[13px]')}
                      onClick={() => setEditor({ open: true, editing: g, template: null })}>Edit</button>
                    <button type="button" className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[13px]')} title="Copy these settings onto other models"
                      onClick={() => setEditor({ open: true, editing: null, template: g })}>Duplicate</button>
                    <button type="button" className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[13px] text-bad')}
                      onClick={() => setPendingDelete(g)}>Delete</button>
                  </div>
                </div>
                {(r.note || issues.length > 0) && (
                  <div className="mt-1.5 space-y-0.5 md:pl-[3.75rem]">
                    {r.note && <p className="truncate text-[12px] text-mute" title={r.note}>{r.note}</p>}
                    {issues.map((x) => (
                      <p key={x.text} className={cx('text-[12px]', x.tone === 'bad' ? 'text-bad' : 'text-warn')}>
                        {x.text}{' '}
                        <button type="button" className="underline underline-offset-2 hover:text-ink"
                          onClick={() => setEditor({ open: true, editing: g, template: null })}>Fix</button>
                      </p>
                    ))}
                  </div>
                )}
              </div>
            );
          })}
        </div>
      )}

      <RuleModal target={editor} accounts={accounts ?? []} rules={rules ?? []} onClose={() => setEditor(CLOSED)} onSave={save} />
      <ConfirmModal
        open={fan != null} tone="primary" confirmLabel="Continue"
        title={`Change ${fan ? nfmt(fan.count) : ''} rules?`}
        body="Each rule is its own request, so this makes one call per rule. If one fails, the earlier ones stay changed and the error says which."
        onClose={() => { fan?.resolve(false); setFan(null); }}
        onConfirm={() => { fan?.resolve(true); setFan(null); }} />
      <ConfirmModal
        open={pendingDelete != null}
        title={pendingDelete
          ? pendingDelete.rules.length === 1 ? `Delete rule for ${pendingDelete.rules[0].model_id}?` : `Delete rule for ${pendingDelete.rules.length} models?`
          : 'Delete rule?'}
        body={pendingDelete && (
          <>
            The router stops enforcing {pendingDelete.rules.length === 1 ? 'it' : 'them'} immediately.
            {pendingDelete.rules.length > 1 && <> This removes the rule from: <span className="font-mono text-dim">{pendingDelete.rules.map((x) => x.model_id).join(', ')}</span>.</>}
          </>
        )}
        busy={pendingDelete != null && busy === pendingDelete.key}
        onClose={() => setPendingDelete(null)}
        onConfirm={() => { if (pendingDelete) void remove(pendingDelete).then(() => setPendingDelete(null)); }} />
    </div>
  );
}
