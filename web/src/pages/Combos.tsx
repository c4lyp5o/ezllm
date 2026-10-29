// COMBOS — routing chains. Cards say whether a chain can serve, how it is doing
// and which hop is next; the editor guards unsaved work, refuses bad drafts
// with a reason, and previews what the chosen strategy will do.
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { Link, useSearchParams } from 'react-router-dom';
import {
  ApiError, RANGE_LABEL, accountModels, del, get, patch, post, STRATEGY_HINTS, usage,
  type Account, type BlockCursor, type Combo, type FreeInclude, type Hop, type RangeKey, type UsageRow,
} from '../utils/api';
import {
  CAUSE_TONES, Chip, Combobox, ConfirmModal, EmptyState, Modal, Mono, Segmented, SectionLabel, SkeletonCards, Spinner, Toggle,
  btn, cx, inputCls, labelCls, relTime,
} from '../components/ui';
import { Collapse, ReorderItem, Stagger, StaggerItem } from '../components/motion';
import { useRange } from '../utils/preferences';
import { nfmt, pct } from '../utils/lib';
import {
  PRESETS, cannotServe, comboHealth, copyName, describePlan, hasErrors, hopState, validateDraft,
  type ComboHealth, type Preset,
} from '../utils/combos';

// Strategies offered by the combo form — every one the backend implements.
const STRATEGIES = ['failover', 'block_round_robin', 'true_round_robin', 'sticky_last_good', 'least_used'] as const;
const STRATEGY_LABEL: Record<string, string> = {
  failover: 'Failover', block_round_robin: 'Block round robin', true_round_robin: 'Round robin',
  sticky_last_good: 'Sticky', least_used: 'Least used',
};
const RANGES: RangeKey[] = ['24h', '7d', '30d', '365d'];

const errText = (e: unknown) => (e instanceof ApiError ? `${e.detail.message}${e.detail.hint ? ` — ${e.detail.hint}` : ''}` : e instanceof Error ? e.message : String(e));

// base_url is the GROUPING KEY: there is no `provider` column, and the upstream
// endpoint is the provider in practice — two Groq accounts both point at
// api.groq.com. That is what lets the hop's account select say "these two
// belong to the same provider" without a schema change.
function providerKey(base_url: string | undefined): string {
  const raw = (base_url ?? '').trim();
  if (!raw) return 'unknown';
  try { return new URL(raw).host || raw; } catch { return raw; }
}
function providerLabel(key: string): string {
  if (key === 'unknown') return 'unknown endpoint';
  return key.replace(/^(api|www|app)\./, '');
}
function groupAccounts(accounts: Account[]): { key: string; label: string; accounts: Account[] }[] {
  const groups: { key: string; label: string; accounts: Account[] }[] = [];
  const byKey = new Map<string, number>();
  for (const a of accounts) {
    const key = providerKey(a.base_url);
    const at = byKey.get(key);
    if (at === undefined) { byKey.set(key, groups.length); groups.push({ key, label: providerLabel(key), accounts: [a] }); }
    else groups[at].accounts.push(a);
  }
  return groups;
}

// Model catalogs, one fetch per account, shared by the cards (is the model
// still listed?) and the editor (the model picker, the preview). null = the
// catalog did not load, so nothing is claimed about it.
function useCatalogs() {
  const [catalogs, setCatalogs] = useState<Record<number, string[] | null>>({});
  const asked = useRef(new Set<number>());
  const ensure = useCallback((ids: number[]) => {
    for (const id of ids) {
      if (id <= 0 || asked.current.has(id)) continue;
      asked.current.add(id);
      accountModels(id)
        .then((ms) => setCatalogs((p) => ({ ...p, [id]: ms.map((m) => m.id).sort() })))
        .catch(() => setCatalogs((p) => ({ ...p, [id]: null })));
    }
  }, []);
  return { catalogs, ensure };
}
type Catalogs = Record<number, string[] | null>;

// A hop row carries two UI-only fields that never reach the wire: `custom`
// (the model id is typed by hand rather than picked) and `uid` (a stable key,
// so a reordered row slides to its new place instead of re-rendering in place).
type HopRow = Hop & { custom?: boolean; uid: number };
let uidSeq = 0;
const withUid = (h: Hop): HopRow => ({ ...h, uid: ++uidSeq });
const emptyHop = (account_id: number): HopRow => withUid({ account_id, model_id: '', weight: 1, enabled: true, hold_until_failure: false });

type Draft = {
  name: string; strategy: string; sticky_idle_s: number; enabled: boolean;
  compression_profile_id: number | null; context_size: number; hops: HopRow[];
  block_size: number;
  auto_free: boolean; auto_free_accounts: number[]; auto_free_include: FreeInclude[];
};

function newDraft(p?: Preset): Draft {
  return {
    name: '', strategy: p?.strategy ?? 'failover', sticky_idle_s: 1800, context_size: 200000, enabled: true,
    compression_profile_id: null, hops: [], block_size: p?.block_size ?? 5,
    auto_free: p?.auto_free === true, auto_free_accounts: [], auto_free_include: [],
  };
}
function draftFrom(c: Combo, name = c.name): Draft {
  return {
    name, strategy: c.strategy, sticky_idle_s: c.sticky_idle_s ?? 1800,
    enabled: c.enabled, compression_profile_id: c.compression_profile_id ?? null,
    context_size: c.context_size ?? 200000, block_size: c.block_size ?? 1,
    hops: (c.hops ?? []).map(withUid),
    auto_free: c.auto_free === true,
    auto_free_accounts: c.auto_free_accounts ?? [],
    auto_free_include: c.auto_free_include ?? [],
  };
}
// What "unsaved changes" means: the draft minus the row keys.
const snapshot = (d: Draft) => JSON.stringify({ ...d, hops: d.hops.map(({ uid: _u, ...h }) => h) });

function HopArrow() {
  return <span className="select-none font-mono text-[12px] text-accent/60" aria-hidden>→</span>;
}

// ── the editor ──────────────────────────────────────────────────────────────

function HopsEditor({ hops, setHops, accounts, strategy, catalogs, ensure, errors }: {
  hops: HopRow[]; setHops: (h: HopRow[]) => void; accounts: Account[]; strategy: string;
  catalogs: Catalogs; ensure: (ids: number[]) => void; errors: Record<number, string>;
}) {
  const move = (i: number, to: number) => {
    if (to < 0 || to >= hops.length || to === i) return;
    const next = [...hops];
    const [h] = next.splice(i, 1);
    next.splice(to, 0, h);
    setHops(next);
  };
  const set = (i: number, p: Partial<HopRow>) => setHops(hops.map((h, k) => (k === i ? { ...h, ...p } : h)));
  useEffect(() => { ensure(hops.map((h) => h.account_id)); }, [hops, ensure]);
  const groups = useMemo(() => groupAccounts(accounts), [accounts]);
  const acctById = new Map(accounts.map((a) => [a.id, a]));

  return (
    <div className="space-y-2">
      <div className="flex items-center justify-between gap-3">
        <SectionLabel>Hops <span className="font-normal text-mute">· tried top to bottom</span></SectionLabel>
        <button type="button" className={cx(btn.base, btn.ghost, 'px-2.5 py-1 text-[12px]')}
          onClick={() => setHops([...hops, emptyHop(accounts[0]?.id ?? 0)])}>
          ＋ Add hop
        </button>
      </div>
      {hops.length === 0 && (
        <p className="rounded-lg border border-dashed border-line px-4 py-5 text-center text-[13px] text-mute">
          No hops yet. Add the first one.
        </p>
      )}
      {hops.map((h, i) => {
        const list = catalogs[h.account_id];
        const models = list && list.length > 0 ? list : null;
        // Picked from the catalog by default; the checkbox flips a row to a
        // typed id (an alias, or a model the catalog has not synced yet). A row
        // with no catalog shows the text input, so the box reads checked to match.
        const custom = h.custom ?? !models;
        const st = hopState(h, acctById.get(h.account_id), catalogs[h.account_id]);
        const problem = errors[i];
        return (
          // Order IS the routing priority, so this is the one animation in the
          // app that carries information: you see which hop moved where.
          <ReorderItem key={h.uid} className={cx('rounded-lg border bg-raised/60 px-3 py-2.5', problem ? 'border-bad/50' : 'border-line')}>
            <div className="flex flex-wrap items-center gap-2">
              <span className="w-5 text-center font-mono text-[12px] text-mute">{i + 1}</span>
              <select aria-label={`Hop ${i + 1} provider`} className={cx(inputCls, 'w-auto min-w-40 flex-1 py-1.5 text-[13px]')} value={h.account_id}
                onChange={(e) => set(i, { account_id: Number(e.target.value), custom: undefined })}>
                {groups.map((g) => (
                  <optgroup key={g.key} label={g.accounts.length > 1 ? `${g.label} · ${g.accounts.length} accounts` : g.label}>
                    {g.accounts.map((a) => <option key={a.id} value={a.id}>{a.name}{a.enabled ? '' : ' (disabled)'}</option>)}
                  </optgroup>
                ))}
                {accounts.length === 0 && <option value={0}>No providers</option>}
              </select>
              <div className="min-w-48 flex-[2]">
                {custom || !models ? (
                  <input aria-label={`Hop ${i + 1} model`} className={cx(inputCls, 'py-1.5 font-mono text-[13px]')} placeholder="Model id, e.g. gpt-6-luna"
                    value={h.model_id} onChange={(e) => set(i, { model_id: e.target.value.trim() })} spellCheck={false} />
                ) : (
                  <Combobox label={`Hop ${i + 1} model`} idPrefix={`hop-${h.uid}`} loadingText="Loading models…" placeholder="Search models…"
                    value={h.model_id}
                    options={[
                      ...(h.model_id && !models.includes(h.model_id) ? [{ value: h.model_id, label: `${h.model_id} · not in catalog` }] : []),
                      ...models.map((m) => ({ value: m, label: m })),
                    ]}
                    onChange={(v) => set(i, { model_id: v })} />
                )}
              </div>
              <div className="flex items-center gap-0.5">
                <button type="button" aria-label={`Move hop ${i + 1} up`} title="Move up" disabled={i === 0}
                  className="rounded p-1 text-mute hover:bg-hover hover:text-ink disabled:opacity-30" onClick={() => move(i, i - 1)}>↑</button>
                <button type="button" aria-label={`Move hop ${i + 1} down`} title="Move down" disabled={i === hops.length - 1}
                  className="rounded p-1 text-mute hover:bg-hover hover:text-ink disabled:opacity-30" onClick={() => move(i, i + 1)}>↓</button>
                <button type="button" aria-label={`Remove hop ${i + 1}`} title="Remove"
                  className="rounded p-1 text-mute hover:bg-hover hover:text-bad" onClick={() => setHops(hops.filter((_, k) => k !== i))}>✕</button>
              </div>
            </div>
            <div className="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1.5 pl-7 text-[12px] text-mute">
              <label className="flex items-center gap-2">
                <Toggle checked={h.enabled} onChange={(v) => set(i, { enabled: v })} label={`Hop ${i + 1} enabled`} />
                {h.enabled ? 'On' : 'Off'}
              </label>
              {models && (
                <label className="flex items-center gap-1.5" title="type a model id instead of picking from the provider's catalog">
                  <input type="checkbox" checked={custom} onChange={(e) => set(i, { custom: e.target.checked })} />
                  Type the model id
                </label>
              )}
              {strategy === 'block_round_robin' && (
                <label className="flex items-center gap-1.5"
                  title="Hold the block while this hop keeps succeeding; release it on a hop-attributable failure (429, 5xx, transport error, failed stream). A 4xx is the request's fault and keeps the hold.">
                  <input type="checkbox" checked={h.hold_until_failure === true}
                    onChange={(e) => set(i, { hold_until_failure: e.target.checked })} />
                  Hold until it fails
                </label>
              )}
              <span className="ml-auto flex items-center gap-3">
                {i > 1 && <button type="button" className="hover:text-ink" onClick={() => move(i, 0)}>Move to top</button>}
                <button type="button" className="hover:text-ink"
                  onClick={() => setHops([...hops.slice(0, i + 1), withUid(h), ...hops.slice(i + 1)])}>Duplicate</button>
              </span>
            </div>
            {(problem || st.issues.length > 0) && (
              <div className="mt-1.5 space-y-0.5 pl-7 text-[12px]">
                {problem && <p className="text-bad">{problem}</p>}
                {st.issues.map((x) => <p key={x.text} className={x.tone === 'bad' ? 'text-bad' : 'text-warn'}>{x.text}</p>)}
              </div>
            )}
          </ReorderItem>
        );
      })}
    </div>
  );
}

// FreePool — the editor body of an auto_free combo: which providers feed it,
// which unmarked models are added by hand, and the live pool (read-only: the
// server derives it; only a hop's enabled flag survives a sync).
function FreePool({ d, setD, accounts }: {
  d: Draft; setD: (f: (p: Draft) => Draft) => void; accounts: Account[];
}) {
  const nameOf = (id: number) => accounts.find((a) => a.id === id)?.name ?? `#${id}`;
  const [pickAcct, setPickAcct] = useState<number>(0);
  const [pickModel, setPickModel] = useState('');
  const [models, setModels] = useState<string[] | null>(null);

  useEffect(() => {
    setPickModel('');
    if (pickAcct <= 0) { setModels(null); return; }
    let live = true;
    accountModels(pickAcct).then((rows) => { if (live) setModels(rows.map((r) => r.id).sort()); }).catch(() => { if (live) setModels([]); });
    return () => { live = false; };
  }, [pickAcct]);

  const toggleAcct = (id: number) => setD((p) => ({
    ...p,
    auto_free_accounts: p.auto_free_accounts.includes(id) ? p.auto_free_accounts.filter((x) => x !== id) : [...p.auto_free_accounts, id],
  }));
  const addInclude = () => {
    if (pickAcct <= 0 || !pickModel) return;
    setD((p) => p.auto_free_include.some((i) => i.account_id === pickAcct && i.model_id === pickModel)
      ? p
      : { ...p, auto_free_include: [...p.auto_free_include, { account_id: pickAcct, model_id: pickModel }] });
    setPickModel('');
  };
  const includeKey = new Set(d.auto_free_include.map((i) => `${i.account_id}/${i.model_id}`));
  const setHopEnabled = (i: number, enabled: boolean) =>
    setD((p) => ({ ...p, hops: p.hops.map((h, k) => (k === i ? { ...h, enabled } : h)) }));

  return (
    <div className="space-y-5">
      <div>
        <div className="mb-2 flex items-baseline justify-between gap-3">
          <SectionLabel>Draw from</SectionLabel>
          <span className="text-[12px] text-mute">All providers when none are picked</span>
        </div>
        <div className="flex flex-wrap gap-2">
          {accounts.map((a) => {
            const on = d.auto_free_accounts.includes(a.id);
            return (
              <button key={a.id} type="button" aria-pressed={on} onClick={() => toggleAcct(a.id)}
                className={cx('rounded-md border px-2.5 py-1.5 text-[13px]',
                  on ? 'border-accent/60 bg-accent-dim text-accent' : 'border-line text-mute hover:border-line-strong hover:text-ink')}>
                {a.name}
              </button>
            );
          })}
        </div>
      </div>

      <div>
        <SectionLabel className="mb-2">Also include <span className="font-normal text-mute">· models with no free marker in their name</span></SectionLabel>
        <div className="flex flex-wrap items-center gap-2">
          {d.auto_free_include.map((i) => (
            <span key={`${i.account_id}/${i.model_id}`} className="inline-flex items-center gap-2 rounded-md border border-line-strong px-2.5 py-1.5 text-[13px]">
              {nameOf(i.account_id)} · <span className="font-mono">{i.model_id}</span>
              <button type="button" aria-label={`Remove ${i.model_id}`} className="text-mute hover:text-ink"
                onClick={() => setD((p) => ({ ...p, auto_free_include: p.auto_free_include.filter((x) => x !== i) }))}>✕</button>
            </span>
          ))}
        </div>
        <div className="mt-2 flex flex-wrap items-center gap-2">
          <div className="min-w-[10rem] flex-1">
            <select aria-label="Provider" className={inputCls} value={pickAcct}
              onChange={(e) => setPickAcct(Number(e.target.value))}>
              <option value={0}>Provider…</option>
              {accounts.map((a) => <option key={a.id} value={a.id}>{a.name}</option>)}
            </select>
          </div>
          <div className="min-w-[12rem] flex-[2]">
            <select aria-label="Model" className={inputCls} value={pickModel} disabled={!models?.length}
              onChange={(e) => setPickModel(e.target.value)}>
              <option value="">{pickAcct > 0 && models === null ? 'Loading…' : 'Model…'}</option>
              {(models ?? []).map((m) => <option key={m} value={m}>{m}</option>)}
            </select>
          </div>
          <button type="button" className={cx(btn.base, btn.ghost)} disabled={!pickModel} onClick={addInclude}>Add a model</button>
        </div>
      </div>

      <div className="overflow-hidden rounded-lg border border-line">
        <div className="flex flex-wrap items-baseline justify-between gap-2 border-b border-line bg-raised/40 px-4 py-3">
          <span className="text-[13px] font-semibold">Pool · {d.hops.length.toLocaleString()} model{d.hops.length === 1 ? '' : 's'}</span>
          <span className="text-[12px] text-mute">Order follows the strategy. Switch a model off to keep it out.</span>
        </div>
        {d.hops.length === 0 ? (
          <p className="px-4 py-4 text-[13px] text-mute">
            Nothing yet. The pool is filled from your providers' catalogs when you save, and again whenever a catalog changes.
          </p>
        ) : (
          <div className="max-h-64 divide-y divide-line overflow-y-auto">
            {d.hops.map((h, i) => (
              <div key={h.uid} className="flex items-center gap-3 px-4 py-2 text-[13px]">
                <span className={cx('min-w-0 flex-1 truncate font-mono', h.enabled ? 'text-ink' : 'text-mute')}>{h.model_id}</span>
                <span className="text-mute">{nameOf(h.account_id)}</span>
                <span className={cx('w-28 text-right text-[12px]', includeKey.has(`${h.account_id}/${h.model_id}`) ? 'text-ink' : 'text-accent')}>
                  {includeKey.has(`${h.account_id}/${h.model_id}`) ? 'added by hand' : 'free marker'}
                </span>
                <Toggle checked={h.enabled !== false} onChange={(v) => setHopEnabled(i, v)} label={`${h.model_id} enabled`} />
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

// What the chosen strategy would do right now, from the hops as drafted.
function PlanPreview({ d, accounts, catalogs }: { d: Draft; accounts: Account[]; catalogs: Catalogs }) {
  const byId = new Map(accounts.map((a) => [a.id, a]));
  const hops = d.hops.map((h) => {
    const a = byId.get(h.account_id);
    const st = hopState(h, a, catalogs[h.account_id], !d.auto_free);
    const bad = st.issues.find((i) => i.tone === 'bad');
    return {
      label: `${a?.name ?? `#${h.account_id}`} / ${h.model_id || '…'}`,
      usable: st.usable && h.model_id.trim() !== '',
      why: h.enabled === false ? 'it is switched off' : bad?.text ?? (h.model_id.trim() === '' ? 'no model chosen yet' : 'it cannot serve'),
    };
  });
  const lines = describePlan(d.strategy, hops, { blockSize: d.block_size, idleS: d.sticky_idle_s, hold: d.hops.map((h) => h.hold_until_failure === true) });
  const dead = hops.length > 0 && hops.every((h) => !h.usable);
  return (
    <div className={cx('rounded-lg border px-4 py-3', dead ? 'border-bad/40 bg-bad-dim' : 'border-line bg-raised/40')}>
      <div className="text-[13px] font-semibold">What happens next</div>
      <ul className="mt-1.5 space-y-1 text-[13px] leading-snug text-dim">
        {lines.map((l) => <li key={l} className={dead ? 'text-bad' : undefined}>{l}</li>)}
      </ul>
    </div>
  );
}

type EditorTarget = { open: boolean; editing: Combo | null; template: Combo | null };
const CLOSED: EditorTarget = { open: false, editing: null, template: null };

function ComboModal({ target, accounts, profiles, combos, catalogs, ensure, onClose, onSaved }: {
  target: EditorTarget; accounts: Account[]; profiles: { id: number; name: string }[]; combos: Combo[];
  catalogs: Catalogs; ensure: (ids: number[]) => void;
  onClose: () => void; onSaved: (name: string) => void;
}) {
  const { open, editing, template } = target;
  const [d, setD] = useState<Draft>(newDraft());
  const initial = useRef('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [showAll, setShowAll] = useState(false);
  const [preset, setPreset] = useState<string | null>(null);

  useEffect(() => {
    if (!open) return;
    setErr(null); setBusy(false); setShowAll(false); setPreset(null);
    const next = editing ? draftFrom(editing) : template ? draftFrom(template, copyName(template.name, combos.map((c) => c.name))) : newDraft();
    setD(next);
    initial.current = snapshot(next);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, editing, template]);

  const setHops = (hops: HopRow[]) => setD((p) => ({ ...p, hops }));
  const others = combos.filter((c) => c.id !== editing?.id).map((c) => c.name);
  const errors = validateDraft(d, others);
  const dirty = open && snapshot(d) !== initial.current;

  const applyPreset = (p: Preset) => {
    setPreset(p.key);
    setD((cur) => ({ ...cur, strategy: p.strategy, block_size: p.block_size ?? cur.block_size, auto_free: p.auto_free === true }));
  };

  const submit = async () => {
    if (busy) return;
    if (hasErrors(errors)) { setShowAll(true); return; }
    setBusy(true); setErr(null);
    const body: Record<string, unknown> = {
      name: d.name.trim(), strategy: d.strategy, sticky_idle_s: d.strategy === 'sticky_last_good' ? d.sticky_idle_s : null,
      enabled: d.enabled, compression_profile_id: d.compression_profile_id, context_size: d.context_size,
      // Always sent: the server keeps the stored value when this is 0, and 1 is
      // the smallest meaningful block, so a real value always wins.
      block_size: d.strategy === 'block_round_robin' ? d.block_size : 1,
      auto_free: d.auto_free, auto_free_accounts: d.auto_free_accounts, auto_free_include: d.auto_free_include,
      hops: d.hops.map((h) => ({
        account_id: h.account_id, model_id: h.model_id.trim(), weight: h.weight, enabled: h.enabled,
        hold_until_failure: h.hold_until_failure === true,
      })),
    };
    try {
      if (editing) await patch<Combo>(`/admin/combos/${editing.id}`, body);
      else await post<Combo>('/admin/combos', body);
      onSaved(d.name.trim());
    } catch (e) {
      setErr(errText(e));
    } finally { setBusy(false); }
  };

  const nameShown = errors.name && (showAll || d.name.trim() !== '');
  const tight = d.strategy === 'block_round_robin'
    ? d.hops.map((h) => accounts.find((a) => a.id === h.account_id)).filter((a): a is Account => !!a && (a.rate_limit ?? 0) > 0 && (a.rate_limit ?? 0) < d.block_size)
    : [];

  return (
    <Modal open={open} onClose={onClose} wide busy={busy} dirty={dirty} onSubmit={() => void submit()}
      title={editing ? `Edit ${editing.name}` : template ? `Duplicate ${template.name}` : 'New combo'}
      subtitle="Clients pick a combo by its name, like a model id"
      footer={(close) => <>
        {hasErrors(errors) && showAll && <span className="mr-auto text-[12px] text-bad">Fix the fields marked in red to save.</span>}
        <button type="button" className={cx(btn.base, btn.ghost)} onClick={close}>Cancel</button>
        <button type="submit" disabled={busy || (!!editing && !dirty)} className={cx(btn.base, btn.primary)}>
          {busy ? <Spinner /> : null} {editing ? 'Save changes' : 'Create combo'}
        </button>
      </>}>
      <div className="space-y-5">
        {!editing && !template && (
          <div>
            <SectionLabel className="mb-2">Start from</SectionLabel>
            <div className="grid gap-2 sm:grid-cols-3">
              {PRESETS.map((p) => (
                <button key={p.key} type="button" aria-pressed={preset === p.key} onClick={() => applyPreset(p)}
                  className={cx('rounded-lg border px-3 py-2 text-left transition-colors',
                    preset === p.key ? 'border-accent/60 bg-accent-dim' : 'border-line bg-raised/60 hover:border-line-strong')}>
                  <span className="block text-[13px] font-medium">{p.label}</span>
                  <span className="mt-0.5 block text-[12px] leading-snug text-mute">{p.hint}</span>
                </button>
              ))}
            </div>
          </div>
        )}

        <div className="grid gap-4 sm:grid-cols-[minmax(0,1.3fr)_minmax(0,1fr)_minmax(0,1fr)]">
          <div>
            <label className={labelCls} htmlFor="cb-name">Name</label>
            <input id="cb-name" className={cx(inputCls, nameShown && 'border-bad/60')} placeholder="daily" value={d.name} spellCheck={false}
              aria-invalid={!!nameShown} onChange={(e) => setD((p) => ({ ...p, name: e.target.value }))} />
            {nameShown && <p className="mt-1 text-[12px] text-bad">{errors.name}</p>}
          </div>
          <div>
            <label className={labelCls} htmlFor="cb-cp">Compression profile</label>
            <select id="cb-cp" className={cx(inputCls, profiles.length === 0 && 'opacity-60')} disabled={profiles.length === 0}
              value={d.compression_profile_id ?? ''}
              onChange={(e) => setD((p) => ({ ...p, compression_profile_id: e.target.value === '' ? null : Number(e.target.value) }))}>
              <option value="">{profiles.length === 0 ? 'None defined yet' : 'None'}</option>
              {profiles.map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
            </select>
          </div>
          <div>
            <label className={labelCls} htmlFor="cb-context-size">Context size (tokens)</label>
            <input id="cb-context-size" className={cx(inputCls, 'font-mono tabular-nums', errors.context && 'border-bad/60')} type="number" min={1} max={10000000} step={1000}
              value={d.context_size} onChange={(e) => setD((p) => ({ ...p, context_size: Number(e.target.value) }))} />
            {errors.context
              ? <p className="mt-1 text-[12px] text-bad">{errors.context}</p>
              : <p className="mt-1 text-[12px] text-mute">Advertised to clients such as Hermes. Not enforced.</p>}
          </div>
        </div>

        <div>
          <SectionLabel className="mb-2">Strategy</SectionLabel>
          <Segmented<string> label="Strategy" value={d.strategy} onChange={(s) => setD((p) => ({ ...p, strategy: s }))}
            options={STRATEGIES.map((s) => ({ key: s, label: STRATEGY_LABEL[s], title: s }))} />
          <p className="mt-2 text-[13px] leading-snug text-mute">
            <Mono>{d.strategy}</Mono> — {STRATEGY_HINTS[d.strategy]}
          </p>
          {/* The strategy's own fields COLLAPSE in: picking a strategy changes
              how much the form asks for, and a height animation shows the new
              fields arriving instead of the dialog resizing under the hand. */}
          <Collapse open={d.strategy === 'sticky_last_good'}>
            <div className="mt-3 flex flex-wrap items-center gap-3">
              <label className="text-[13px] font-medium" htmlFor="cb-idle">Sticky idle</label>
              <input id="cb-idle" type="number" min={0} className={cx(inputCls, '!w-28 py-1.5 font-mono text-[13px] tabular-nums', errors.idle && 'border-bad/60')}
                value={d.sticky_idle_s} onChange={(e) => setD((p) => ({ ...p, sticky_idle_s: Number(e.target.value) || 0 }))} />
              <span className="text-[12px] text-mute">seconds of quiet before it moves on</span>
              {errors.idle && <span className="text-[12px] text-bad">{errors.idle}</span>}
            </div>
          </Collapse>
          <Collapse open={d.strategy === 'block_round_robin'}>
            <div className="mt-3 rounded-lg border border-line bg-raised/60 p-3.5">
              <div className="flex flex-wrap items-center gap-3">
                <label className="text-[13px] font-medium" htmlFor="cb-block">Block size</label>
                <input id="cb-block" type="number" min={1} max={1000} className={cx(inputCls, '!w-28 py-1.5 font-mono text-[13px] tabular-nums', errors.block && 'border-bad/60')}
                  value={d.block_size} onChange={(e) => setD((p) => ({ ...p, block_size: Number(e.target.value) || 1 }))} />
                <span className="text-[12px] text-mute">requests per hop before the rotation moves on. Every hop shares this size.</span>
                {errors.block && <span className="text-[12px] text-bad">{errors.block}</span>}
              </div>
              {tight.length > 0 && (
                <p className="mt-2 text-[12px] leading-snug text-warn">
                  Block size {d.block_size} exceeds the request budget of{' '}
                  {tight.map((a) => `${a.name} (${a.rate_limit} per ${a.rate_window})`).join(', ')}. Those hops will drop out
                  mid-block and the rotation will advance early. Set the block size to the smallest hop budget, or raise the budget.
                </p>
              )}
              <details className="mt-2 text-[12px] text-mute">
                <summary className="cursor-pointer select-none text-dim hover:text-ink">Why blocks?</summary>
                <p className="mt-1.5 leading-snug">
                  Each account caches your prompt prefix, so a run of consecutive requests to the same account keeps that
                  cache warm: call <Mono>k</Mono> reads the prefix call <Mono>k-1</Mono> just sent, and the marginal cost
                  collapses. Rotating on every request throws that away, because the next hop has never seen the prefix and
                  pays a full cold read. Load is still split evenly, in blocks instead of alternating.
                </p>
              </details>
            </div>
          </Collapse>
        </div>

        <div className="flex items-center gap-4 rounded-lg border border-line bg-raised/40 px-4 py-3">
          <div className="min-w-0 flex-1">
            <div className="text-[13px] font-semibold">Fill with free models</div>
            <p className="mt-0.5 text-[12px] leading-snug text-mute">
              Hops come from every free model your providers list, and stay in sync as catalogs change.
            </p>
          </div>
          <Toggle checked={d.auto_free} onChange={(v) => setD((p) => ({ ...p, auto_free: v }))} label="fill with free models" />
        </div>

        {d.auto_free
          ? <FreePool d={d} setD={setD} accounts={accounts} />
          : <>
            <HopsEditor hops={d.hops} setHops={setHops} accounts={accounts} strategy={d.strategy} catalogs={catalogs} ensure={ensure}
              errors={showAll || d.hops.length > 0 ? errors.hop : {}} />
            {errors.hops && showAll && <p className="text-[12px] text-bad">{errors.hops}</p>}
          </>}

        <PlanPreview d={d} accounts={accounts} catalogs={catalogs} />

        <div className="flex items-center gap-3">
          <Toggle checked={d.enabled} onChange={(v) => setD((p) => ({ ...p, enabled: v }))} label="combo enabled" />
          <span className="text-[13px] text-dim">{d.enabled ? 'Enabled: clients can use it' : 'Disabled: requests to it are refused'}</span>
        </div>

        {err && <div className="rounded-lg border border-bad/30 bg-bad-dim px-4 py-3 text-[13px] text-bad">{err}</div>}
      </div>
    </Modal>
  );
}

// ── testing ─────────────────────────────────────────────────────────────────

// Hop test results — POST /admin/combos/{id}/test. Each probe is one REAL
// upstream call (tiny: max_tokens 16) and is NOT ledgered, so what the modal
// shows is the dispatcher's genuine verdict, parks included.
type TestHop = {
  index?: number; account_id: number; namespace?: string; model: string; enabled: boolean;
  status: number; ms: number; err?: string; cause?: string;
  parked?: boolean; cooldown_s?: number;
};
type TestResult = { combo: string; surface: string; hops: TestHop[] };
/** What the page remembers of a combo's last probes, by hop position. */
type TestRecord = { at: number; hops: Record<number, TestHop> };
type TestTarget = { combo: Combo; hop?: number };

const hopOk = (h: TestHop) => h.status >= 200 && h.status < 300;

function TestComboModal({ target, onClose, onResult }: {
  target: TestTarget | null; onClose: () => void; onResult: (c: Combo, hops: TestHop[], whole: boolean) => void;
}) {
  const combo = target?.combo ?? null;
  const [surface, setSurface] = useState<'chat' | 'anthropic'>('chat');
  const [running, setRunning] = useState(false);
  const [rows, setRows] = useState<Record<number, TestHop>>({});
  const [pending, setPending] = useState<number | 'all' | null>(null);
  const [err, setErr] = useState<string | null>(null);

  const run = useCallback(async (c: Combo, s: 'chat' | 'anthropic', hop?: number) => {
    setRunning(true); setErr(null); setPending(hop ?? 'all');
    try {
      const r = await post<TestResult>(`/admin/combos/${c.id}/test`, hop === undefined ? { surface: s } : { surface: s, hop });
      // Old servers do not number the hops: a whole-chain answer is positional.
      const numbered = r.hops.map((h, i) => ({ ...h, index: h.index ?? (hop ?? i) }));
      setRows((p) => {
        const next = hop === undefined ? {} as Record<number, TestHop> : { ...p };
        for (const h of numbered) next[h.index as number] = h;
        return next;
      });
      onResult(c, numbered, hop === undefined);
    } catch (e) { setErr(errText(e)); }
    finally { setRunning(false); setPending(null); }
  }, [onResult]);

  // Run on open; the surface selector + Run again re-run explicitly.
  useEffect(() => {
    if (target) { setRows({}); void run(target.combo, surface, target.hop); }
    // `surface` deliberately not a dep: changing it must not fire a paid
    // probe behind the operator's hand.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [target]);

  const shown = Object.values(rows).sort((a, b) => (a.index ?? 0) - (b.index ?? 0));
  const ok = shown.filter(hopOk).length;
  return (
    <Modal open={target != null} onClose={onClose} wide busy={running}
      title={combo ? `Test ${combo.name}` : 'Test combo'}
      subtitle="One real request per hop: tiny (16 tokens out) and not recorded in the ledger"
      footer={(close) => <>
        <span className="mr-auto flex items-center gap-2 text-[13px] text-mute">
          Surface
          <select aria-label="Surface" className={cx(inputCls, 'w-auto py-1 text-[13px]')} value={surface}
            onChange={(e) => setSurface(e.target.value as 'chat' | 'anthropic')}>
            <option value="chat">chat</option>
            <option value="anthropic">anthropic</option>
          </select>
        </span>
        <button type="button" className={cx(btn.base, btn.ghost)} onClick={close}>Close</button>
        <button type="button" disabled={running || !combo} className={cx(btn.base, btn.primary)}
          onClick={() => { if (combo) void run(combo, surface); }}>
          {running && pending === 'all' ? <Spinner /> : null} Test all hops
        </button>
      </>}>
      {err && <div className="mb-3 rounded-lg border border-bad/30 bg-bad-dim px-4 py-3 text-[13px] text-bad">{err}</div>}
      {running && shown.length === 0 && (
        <div className="flex items-center gap-2 px-2 py-6 text-[13px] text-mute">
          <Spinner /> Probing {target?.hop === undefined ? `${combo?.hops.length ?? 0} hop${combo?.hops.length === 1 ? '' : 's'}` : `hop ${target.hop + 1}`}…
        </div>
      )}
      {shown.length > 0 && (
        <div className="space-y-2">
          <p className="text-[13px] text-dim">
            {ok} of {shown.length} {shown.length === 1 ? 'hop' : 'hops'} answered{target?.hop !== undefined && shown.length === 1 ? ' (one hop tested)' : ''}.
          </p>
          {shown.map((h) => (
            <div key={h.index} className="rounded-lg border border-line bg-raised/40 px-4 py-3">
              <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5">
                <span className="font-mono text-[12px] text-mute">{(h.index ?? 0) + 1}</span>
                <span className="font-mono text-[13px] text-ink">{h.namespace ?? `#${h.account_id}`}/{h.model}</span>
                {!h.enabled && <Chip tone="neutral">hop disabled</Chip>}
                {h.status === 200 && <Chip tone="ok">200</Chip>}
                {h.status >= 400 && (
                  <Chip tone={CAUSE_TONES[h.cause ?? ''] ?? 'bad'} title={h.err}>
                    {h.status}{h.cause ? ` · ${h.cause.replace(/_/g, ' ')}` : ''}
                  </Chip>
                )}
                {h.status === 0 && <Chip tone="bad">not dialed</Chip>}
                {h.parked && <Chip tone="warn" title="the probe parked this hop exactly as a production 429 would">parked {h.cooldown_s}s</Chip>}
                <span className="ml-auto flex items-center gap-3">
                  <span className="font-mono text-[12px] text-mute">{h.ms} ms</span>
                  <button type="button" disabled={running} className="text-[12px] text-mute hover:text-ink disabled:opacity-50"
                    aria-label={`Test hop ${(h.index ?? 0) + 1} again`}
                    onClick={() => { if (combo) void run(combo, surface, h.index); }}>
                    {running && pending === h.index ? 'Testing…' : 'Test again'}
                  </button>
                </span>
              </div>
              {h.err && (
                <div className="mt-1.5 break-words font-mono text-[12px] leading-relaxed text-mute" title={h.err}>
                  {h.err.length > 220 ? h.err.slice(0, 220) + '…' : h.err}
                </div>
              )}
            </div>
          ))}
          <p className="pt-1 text-[12px] leading-relaxed text-mute">
            Probes run through the real dispatcher: a hop that earns a parkable failure stays parked, because
            it failed for real. No eligibility, capability or pin filtering. Disabled and skipped hops are
            probed too, which is how you learn why the router avoids them.
          </p>
        </div>
      )}
    </Modal>
  );
}

// ── the cards ───────────────────────────────────────────────────────────────

// FreeSummary — an auto_free card shows the pool as counts, not as dozens of
// hop chips: how many models, from how many providers, and the split.
function FreeSummary({ combo, accounts }: { combo: Combo; accounts: Account[] }) {
  const live = (combo.hops ?? []).filter((h) => h.enabled !== false);
  if (live.length === 0) {
    return (
      <div className="mt-3 border-t border-line pt-3 text-[13px] text-mute">
        No free models yet. Add a provider and its free models appear here on their own. Requests to this combo fail until there is one.
      </div>
    );
  }
  const by = new Map<number, number>();
  for (const h of live) by.set(h.account_id, (by.get(h.account_id) ?? 0) + 1);
  const rows = [...by.entries()].sort((a, b) => b[1] - a[1]);
  return (
    <div className="mt-3 space-y-3">
      <div className="flex items-baseline gap-2">
        <span className="text-[22px] font-semibold">{live.length.toLocaleString()}</span>
        <span className="text-[13px] text-mute">
          free model{live.length === 1 ? '' : 's'} from {rows.length.toLocaleString()} provider{rows.length === 1 ? '' : 's'}
        </span>
      </div>
      <div className="flex flex-wrap gap-2">
        {rows.map(([id, n]) => (
          <span key={id} className="flex gap-2 rounded-md border border-line px-2.5 py-1.5 text-[13px]">
            <span>{accounts.find((a) => a.id === id)?.name ?? `#${id}`}</span>
            <span className="text-mute">{n.toLocaleString()}</span>
          </span>
        ))}
      </div>
    </div>
  );
}

// The name copies on click: it is what clients put in the "model" field.
function CopyName({ name }: { name: string }) {
  const [done, setDone] = useState(false);
  return (
    <button type="button" title="Copy the model name clients use" aria-label={`Copy name ${name}`}
      className="group flex items-center gap-1.5 text-[14px] font-semibold hover:text-accent"
      onClick={() => {
        void navigator.clipboard?.writeText(name).then(() => { setDone(true); setTimeout(() => setDone(false), 1500); }).catch(() => {});
      }}>
      {name}
      <span className={cx('text-[11px] font-normal', done ? 'text-ok' : 'text-transparent group-hover:text-mute group-focus-visible:text-mute')}>{done ? 'Copied' : 'Copy'}</span>
    </button>
  );
}

function ComboCard({ c, accounts, health, stat, statsReady, range, cursor, test, profileName, busy, onToggle, onEdit, onDuplicate, onDelete, onTest }: {
  c: Combo; accounts: Account[]; health: ComboHealth; stat: UsageRow | undefined; statsReady: boolean; range: RangeKey;
  cursor: BlockCursor | undefined; test: TestRecord | undefined; profileName: string | undefined; busy: boolean;
  onToggle: (v: boolean) => void; onEdit: () => void; onDuplicate: () => void; onDelete: () => void; onTest: (hop?: number) => void;
}) {
  const nameOf = (id: number) => accounts.find((a) => a.id === id)?.name ?? `#${id}`;
  const hops = c.hops ?? [];
  const dead = cannotServe(c, health);
  const problems = hops
    .map((h, i) => ({ h, i, st: health.hops[i] }))
    .filter((x) => x.st && x.st.issues.length > 0 && x.h.enabled !== false);
  const calls = stat?.calls ?? 0;
  const tested = test ? Object.values(test.hops) : [];
  const testedOk = tested.filter(hopOk).length;
  const cursorHop = (i: number) => {
    if (!cursor) return false;
    const a = accounts.find((x) => x.id === hops[i].account_id);
    return !!a && cursor.hop === `${a.namespace}/${hops[i].model_id}`;
  };

  return (
    <div className={cx('rounded-xl border bg-surface px-5 py-4', dead ? 'border-bad/40' : 'border-line', !c.enabled && 'opacity-90')}>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <CopyName name={c.name} />
        <Chip title={STRATEGY_HINTS[c.strategy]}><span className="font-mono">{c.strategy}</span></Chip>
        {c.auto_free && <Chip tone="accent" title="hops are derived from your providers' free models and kept in sync">Auto</Chip>}
        {!!c.compression_profile_id && (
          <Chip tone="accent" title="compression profile applied to requests routed through this combo">
            <span className="font-mono">{profileName ?? `profile #${c.compression_profile_id}`}</span>
          </Chip>
        )}
        <span className="text-[12px] text-mute" title="the context size advertised to clients">{nfmt(c.context_size ?? 0)} context</span>
        <span className="ml-auto flex flex-wrap items-center gap-3">
          <label className="flex items-center gap-2 text-[13px] text-dim">
            <Toggle checked={c.enabled} disabled={busy}
              label={`${c.enabled ? 'Disable' : 'Enable'} combo ${c.name}`} onChange={onToggle} />
            <span>{c.enabled ? 'Enabled' : 'Disabled'}</span>
          </label>
          <span className="flex items-center gap-2">
            <button type="button" className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[13px]')}
              title="probe every hop with one tiny real request" onClick={() => onTest()}>Test</button>
            <button type="button" className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[13px]')} onClick={onEdit}>Edit</button>
            <button type="button" className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[13px]')} title="start a new combo from this one" onClick={onDuplicate}>Duplicate</button>
            <button type="button" className={cx(btn.base, btn.danger, 'px-3 py-1.5 text-[13px]')} onClick={onDelete}>Delete</button>
          </span>
        </span>
      </div>

      {/* Can it serve, and how is it doing — before the hop list, because that
          is what the operator opened the page to find out. */}
      <div className="mt-2.5 flex flex-wrap items-center gap-x-4 gap-y-1.5 text-[12px] text-mute">
        {dead
          ? <Chip tone="bad">Cannot serve: no usable hop</Chip>
          : health.total > 0 && health.usable < health.total
            ? <Chip tone={health.issues > 0 ? 'warn' : 'neutral'}>{health.usable} of {health.total} hops usable</Chip>
            : health.total > 0 && <Chip tone="ok">{health.total === 1 ? '1 hop usable' : `All ${health.total} hops usable`}</Chip>}
        <span>
          {!statsReady ? 'Loading traffic…'
            : calls === 0 ? `No requests in ${RANGE_LABEL[range]}`
              : <>
                <span className="text-dim">{nfmt(calls)} request{calls === 1 ? '' : 's'}</span>
                {' · '}
                <span className={(stat?.errors ?? 0) > 0 && (stat!.errors / calls) >= 0.1 ? 'text-bad' : undefined}>{pct(stat?.errors ?? 0, calls)} errors</span>
                {' in '}{RANGE_LABEL[range]}
              </>}
        </span>
        {calls > 0 && (
          <Link to={`/requests?${new URLSearchParams({ alias: c.name, range })}`} className="text-accent hover:underline">View requests</Link>
        )}
        {test && tested.length > 0 && (
          <span title="from this session's probes">
            Tested {relTime(new Date(test.at).toISOString())} · {testedOk} of {tested.length} answered
          </span>
        )}
      </div>

      {c.auto_free && <FreeSummary combo={c} accounts={accounts} />}
      {!c.auto_free && <div className="mt-3 flex flex-wrap items-center gap-2 font-mono text-[12px]">
        {hops.length === 0
          ? <span className="font-sans text-[13px] text-mute">No hops configured</span>
          : hops.map((h, i) => {
            const st = health.hops[i];
            const worst = st?.issues.some((x) => x.tone === 'bad') ? 'bad' : st?.issues.length ? 'warn' : null;
            const t = test?.hops[i];
            const here = cursorHop(i);
            return (
              <span key={i} className="flex items-center gap-2">
                {i > 0 && <HopArrow />}
                <button type="button" onClick={() => onTest(i)}
                  title={[`Test hop ${i + 1}`, ...(st?.issues.map((x) => x.text) ?? [])].join(' · ')}
                  className={cx('rounded-md border px-2 py-1 text-left transition-colors hover:border-line-strong',
                    h.enabled ? 'bg-raised' : 'bg-transparent opacity-45',
                    here ? 'border-accent' : worst === 'bad' ? 'border-bad/50' : worst === 'warn' ? 'border-warn/50' : 'border-line')}>
                  {nameOf(h.account_id)}
                  <span className="mx-1 text-mute">/</span>
                  <span className={h.enabled ? 'text-ink' : 'text-mute'}>{h.model_id}</span>
                  {h.hold_until_failure && (
                    <span className="ml-1.5 rounded border border-accent/40 px-1 text-[11px] text-accent"
                      title="holds the block while it succeeds; releases on a hop-attributable failure">hold</span>
                  )}
                  {here && cursor && (
                    <span className="ml-1.5 rounded border border-accent/40 px-1 text-[11px] text-accent"
                      title={`serving now: ${cursor.remaining} of ${cursor.block_size} requests left in this block${cursor.reason ? ` (${cursor.reason})` : ''}`}>
                      ▮ {cursor.remaining} left
                    </span>
                  )}
                  {t && <span className={cx('ml-1.5 text-[11px]', hopOk(t) ? 'text-ok' : 'text-bad')}
                    title={hopOk(t) ? 'answered the last probe' : `last probe: ${t.status}${t.cause ? ` ${t.cause.replace(/_/g, ' ')}` : ''}`}>
                    {hopOk(t) ? '✓' : '✕'}
                  </span>}
                </button>
              </span>
            );
          })}
      </div>}

      {problems.length > 0 && (
        <ul className="mt-2.5 space-y-0.5 text-[12px]">
          {problems.slice(0, 3).flatMap((p) => p.st.issues.map((x) => (
            <li key={`${p.i}-${x.text}`} className={x.tone === 'bad' ? 'text-bad' : 'text-warn'}>Hop {p.i + 1}: {x.text}</li>
          ))).slice(0, 4)}
          {problems.length > 3 && <li className="text-mute">and {problems.length - 3} more hop{problems.length - 3 === 1 ? '' : 's'} with problems</li>}
        </ul>
      )}
      {c.strategy === 'sticky_last_good' && (
        <div className="mt-2 text-[12px] text-mute">Sticky idle <Mono>{c.sticky_idle_s ?? '–'}s</Mono></div>
      )}
      {c.strategy === 'block_round_robin' && (
        <div className="mt-2 text-[12px] text-mute">
          Block size <Mono>{c.block_size ?? 1}</Mono>: one hop serves {c.block_size ?? 1} request
          {(c.block_size ?? 1) === 1 ? '' : 's'} in a row before rotating
        </div>
      )}
    </div>
  );
}

type Filter = 'all' | 'enabled' | 'disabled' | 'attention';

export default function Combos() {
  const [combos, setCombos] = useState<Combo[] | null>(null);
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [profiles, setProfiles] = useState<{ id: number; name: string }[]>([]);
  const [editor, setEditor] = useState<EditorTarget>(CLOSED);
  const [pendingDelete, setPendingDelete] = useState<Combo | null>(null);
  const [testing, setTesting] = useState<TestTarget | null>(null);
  const [tests, setTests] = useState<Record<number, TestRecord>>({});
  const [err, setErr] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [busyID, setBusyID] = useState<number | null>(null);
  const [deleting, setDeleting] = useState(false);
  const [filter, setFilter] = useState<Filter>('all');
  const [query, setQuery] = useState('');
  const [range, setRange] = useRange();
  const [usageRows, setUsageRows] = useState<Record<string, UsageRow> | null>(null);
  const [cursors, setCursors] = useState<BlockCursor[]>([]);
  const { catalogs, ensure } = useCatalogs();
  const [params, setParams] = useSearchParams();

  const load = useCallback(() => {
    get<Combo[]>('/admin/combos').then((c) => { setCombos(c); setErr(null); }).catch((e) => setErr(e instanceof Error ? e.message : 'failed to load'));
    get<Account[]>('/admin/accounts').then(setAccounts).catch(() => { /* non-fatal */ });
    get<{ id: number; name: string }[]>('/admin/compression-profiles')
      .then((p) => setProfiles(p.map((x) => ({ id: x.id, name: String((x as { name?: unknown }).name ?? x.id) }))))
      .catch(() => setProfiles([]));
  }, []);
  useEffect(() => { load(); }, [load]);

  // Traffic per combo (the ledger alias), refreshed with the range and every
  // half minute; the live block cursors on a faster beat, only when a block
  // combo exists to have one.
  useEffect(() => {
    let live = true;
    const pull = () => usage('alias', range)
      .then((u) => { if (live) setUsageRows(Object.fromEntries((u.groups ?? []).map((g) => [g.k, g]))); })
      .catch(() => { if (live) setUsageRows((p) => p ?? {}); });
    setUsageRows(null);
    void pull();
    const t = setInterval(() => void pull(), 30_000);
    return () => { live = false; clearInterval(t); };
  }, [range]);
  const hasBlock = (combos ?? []).some((c) => c.enabled && c.strategy === 'block_round_robin');
  useEffect(() => {
    if (!hasBlock) { setCursors([]); return; }
    let live = true;
    const pull = () => get<{ block_cursors?: BlockCursor[] }>('/admin/overview')
      .then((o) => { if (live) setCursors(o.block_cursors ?? []); }).catch(() => {});
    void pull();
    const t = setInterval(() => void pull(), 15_000);
    return () => { live = false; clearInterval(t); };
  }, [hasBlock]);

  useEffect(() => { if (combos) ensure(combos.filter((c) => !c.auto_free).flatMap((c) => (c.hops ?? []).map((h) => h.account_id))); }, [combos, ensure]);

  // Deep links: /combos?edit=<id> and /combos?test=<id>, so another page can
  // send the operator straight to the combo it is talking about.
  useEffect(() => {
    if (!combos) return;
    const edit = Number(params.get('edit'));
    const testId = Number(params.get('test'));
    if (!edit && !testId && params.get('new') !== '1') return;
    const next = new URLSearchParams(params);
    next.delete('edit'); next.delete('test'); next.delete('new');
    setParams(next, { replace: true });
    if (params.get('new') === '1') setEditor({ open: true, editing: null, template: null });
    const e = combos.find((c) => c.id === edit);
    if (e) setEditor({ open: true, editing: e, template: null });
    const t = combos.find((c) => c.id === testId);
    if (t) setTesting({ combo: t });
  }, [combos, params, setParams]);

  const healths = useMemo(() => new Map((combos ?? []).map((c) => [c.id, comboHealth(c, accounts, catalogs)])), [combos, accounts, catalogs]);
  const profileName = (id: number | null | undefined) => profiles.find((p) => p.id === id)?.name;

  const remove = async (c: Combo) => {
    setErr(null); setDeleting(true);
    try { await del(`/admin/combos/${c.id}`); setNotice(`Deleted ${c.name}.`); load(); }
    catch (e) { setErr(errText(e)); }
    finally { setPendingDelete(null); setDeleting(false); }
  };

  // One-field PATCH: the server merges the rest, so this cannot clobber hops or
  // strategy. Optimistic so the switch answers the tap immediately; the reload
  // in `finally` puts the server's truth back on screen either way.
  const setEnabled = async (c: Combo, next: boolean) => {
    setErr(null); setBusyID(c.id);
    setCombos((prev) => prev?.map((x) => (x.id === c.id ? { ...x, enabled: next } : x)) ?? prev);
    try { await patch<Combo>(`/admin/combos/${c.id}`, { enabled: next }); }
    catch (e) { setErr(errText(e)); }
    finally { setBusyID(null); load(); }
  };

  const recordTest = useCallback((c: Combo, hops: TestHop[], whole: boolean) => {
    setTests((p) => {
      const base = whole ? {} : (p[c.id]?.hops ?? {});
      const merged = { ...base };
      for (const h of hops) merged[h.index ?? 0] = h;
      return { ...p, [c.id]: { at: Date.now(), hops: merged } };
    });
  }, []);

  const q = query.trim().toLowerCase();
  const matches = (c: Combo) => {
    if (!q) return true;
    const hay = [c.name, c.strategy, ...(c.hops ?? []).flatMap((h) => [h.model_id, accounts.find((a) => a.id === h.account_id)?.name ?? ''])];
    return hay.some((s) => s.toLowerCase().includes(q));
  };
  const needsAttention = (c: Combo) => {
    const h = healths.get(c.id);
    return !!h && c.enabled && (cannotServe(c, h) || h.issues > 0);
  };
  const shown = (combos ?? [])
    .filter(matches)
    .filter((c) => filter === 'all' || (filter === 'enabled' ? c.enabled : filter === 'disabled' ? !c.enabled : needsAttention(c)))
    // A chain that cannot serve is the thing to see first; the rest keep the
    // order the operator knows.
    .map((c, i) => ({ c, i, dead: cannotServe(c, healths.get(c.id) ?? { hops: [], usable: 1, total: 1, issues: 0 }) }))
    .sort((a, b) => Number(b.dead) - Number(a.dead) || a.i - b.i)
    .map((x) => x.c);

  const total = combos?.length ?? 0;
  const enabledN = (combos ?? []).filter((c) => c.enabled).length;
  const deadN = (combos ?? []).filter((c) => { const h = healths.get(c.id); return !!h && cannotServe(c, h); }).length;
  const attentionN = (combos ?? []).filter(needsAttention).length;
  const openNew = () => setEditor({ open: true, editing: null, template: null });

  return (
    // No page-root fade: the route transition in App.tsx already owns the
    // page entrance. A second entrance stacked on it reads as a stutter.
    <div>
      <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
        <div className="text-[13px] text-mute" aria-live="polite">
          {combos === null ? 'Loading combos…' : (
            <>
              <span className="text-ink">{nfmt(total)}</span> combo{total === 1 ? '' : 's'} · {nfmt(enabledN)} enabled
              {deadN > 0 && <> · <span className="text-bad">{nfmt(deadN)} cannot serve</span></>}
              {deadN === 0 && attentionN > 0 && <> · <span className="text-warn">{nfmt(attentionN)} need{attentionN === 1 ? 's' : ''} attention</span></>}
            </>
          )}
        </div>
        <button type="button" className={cx(btn.base, btn.primary)} onClick={openNew}>＋ New combo</button>
      </div>

      {total > 0 && (
        <div className="mb-4 flex flex-wrap items-center gap-x-5 gap-y-2">
          <input aria-label="Search combos" type="search" placeholder="Search by name, model or provider" value={query}
            onChange={(e) => setQuery(e.target.value)} className={cx(inputCls, 'w-full sm:!w-64 py-1.5')} />
          <Segmented<Filter> size="sm" label="Show" value={filter} onChange={setFilter}
            options={[
              { key: 'all', label: 'All' }, { key: 'enabled', label: 'Enabled' }, { key: 'disabled', label: 'Disabled' },
              { key: 'attention', label: attentionN > 0 ? `Needs attention (${attentionN})` : 'Needs attention' },
            ]} />
          <span className="ml-auto flex items-center gap-2 text-[12px] text-mute">
            Traffic
            <Segmented<RangeKey> size="sm" label="Traffic range" value={range} onChange={setRange}
              options={RANGES.map((k) => ({ key: k, label: RANGE_LABEL[k] }))} />
          </span>
        </div>
      )}

      {err && <div className="mb-4 rounded-lg border border-bad/30 bg-bad-dim px-4 py-3 text-[13px] text-bad">{err}</div>}
      {notice && !err && (
        <div className="mb-4 flex items-center justify-between gap-3 rounded-lg border border-line bg-raised/60 px-4 py-2.5 text-[13px] text-dim" role="status">
          <span>{notice}</span>
          <button type="button" className="text-mute hover:text-ink" aria-label="Dismiss" onClick={() => setNotice(null)}>✕</button>
        </div>
      )}

      {combos === null ? (
        <SkeletonCards n={2} />
      ) : combos.length === 0 ? (
        <div className="rounded-xl border border-line bg-surface px-6 py-14 text-center">
          <p className="text-[14px] font-medium">No combos yet</p>
          <p className="mt-1 text-[13px] text-mute">A combo is an ordered chain of hops. Clients pick one by name and it tries the hops according to its strategy.</p>
          <button type="button" className={cx(btn.base, btn.primary, 'mt-5')} onClick={openNew}>＋ Create your first combo</button>
        </div>
      ) : shown.length === 0 ? (
        <EmptyState title="No combo matches" hint="Nothing fits the search and filter together."
          action={<button type="button" className={cx(btn.base, btn.ghost)} onClick={() => { setQuery(''); setFilter('all'); }}>Clear search and filter</button>} />
      ) : (
        <Stagger className="space-y-3">
          {shown.map((c) => (
            <StaggerItem key={c.id}>
              <ComboCard c={c} accounts={accounts} health={healths.get(c.id) ?? { hops: [], usable: 0, total: 0, issues: 0 }}
                stat={usageRows?.[c.name]} statsReady={usageRows !== null} range={range}
                cursor={c.strategy === 'block_round_robin' ? cursors.find((k) => k.combo === c.name && !!k.hop) : undefined}
                test={tests[c.id]} profileName={profileName(c.compression_profile_id)} busy={busyID === c.id}
                onToggle={(v) => void setEnabled(c, v)}
                onEdit={() => setEditor({ open: true, editing: c, template: null })}
                onDuplicate={() => setEditor({ open: true, editing: null, template: c })}
                onDelete={() => setPendingDelete(c)}
                onTest={(hop) => setTesting({ combo: c, hop })} />
            </StaggerItem>
          ))}
        </Stagger>
      )}

      <ComboModal target={editor} accounts={accounts} profiles={profiles} combos={combos ?? []} catalogs={catalogs} ensure={ensure}
        onClose={() => setEditor(CLOSED)}
        onSaved={(name) => {
          // Probes of the old hop list no longer describe this combo.
          if (editor.editing) setTests((p) => { const n = { ...p }; delete n[editor.editing!.id]; return n; });
          setNotice(editor.editing ? `Saved ${name}.` : `Created ${name}.`);
          setEditor(CLOSED); load();
        }} />
      <TestComboModal target={testing} onClose={() => setTesting(null)} onResult={recordTest} />
      <ConfirmModal
        open={pendingDelete != null}
        title={pendingDelete ? `Delete combo ${pendingDelete.name}?` : 'Delete combo?'}
        body={pendingDelete && (() => {
          const calls = usageRows?.[pendingDelete.name]?.calls ?? 0;
          return (
            <>
              {pendingDelete.auto_free ? 'Its free-model pool' : `Its ${nfmt((pendingDelete.hops ?? []).length)} hop${(pendingDelete.hops ?? []).length === 1 ? '' : 's'}`} will be removed with it.
              {calls > 0 && <> It served {nfmt(calls)} request{calls === 1 ? '' : 's'} in the last {RANGE_LABEL[range]}, so clients are using it.</>}
              {' '}Clients pointing at it get errors until you recreate it or change their model.
            </>
          );
        })()}
        busy={deleting}
        onClose={() => setPendingDelete(null)}
        onConfirm={() => { if (pendingDelete) void remove(pendingDelete); }} />
    </div>
  );
}
