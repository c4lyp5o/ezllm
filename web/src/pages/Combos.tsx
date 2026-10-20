// COMBOS — list cards + create/edit modal with an ordered hops editor.
import { useCallback, useEffect, useState } from 'react';
import { ApiError, accountModels, del, get, patch, post, STRATEGY_HINTS, type Combo, type FreeInclude, type Hop } from '../utils/api';
import { CAUSE_TONES, Chip, ConfirmModal, Modal, Mono, SectionLabel, SkeletonCards, Spinner, Toggle, btn, cx, inputCls, labelCls } from '../components/ui';
import { Collapse, ReorderItem, Stagger, StaggerItem } from '../components/motion';

// Strategies offered by the combo form. least_used is deliberately absent: it
// is accepted by the backend (where it keeps position order, with a comment
// explaining why) but is NOT offered here, because a selectable option that
// silently equals another one is a knob that appears to work and does not.
const STRATEGIES = ['failover', 'block_round_robin', 'true_round_robin', 'sticky_last_good'] as const;

// AccountLite keeps each account's request budget alongside its name so the
// block-size field can warn when a block could not be served in full.
// base_url is the GROUPING KEY: there is no `provider` column (schema change
// deliberately deferred), and the upstream endpoint is the provider in practice
// — two Groq accounts both point at api.groq.com. That is what lets the hop's
// account select say "these two belong to the same provider" without a migration.
type AccountLite = { id: number; name: string; base_url?: string; rate_limit?: number; rate_window?: string | null };

// providerKey → providerLabel. Labels strip the generic prefixes and keep the
// host, so the group reads as the endpoint it is (honest) rather than a
// guessed brand name: api.groq.com → groq.com, internal://geminiweb → geminiweb.
function providerKey(base_url: string | undefined): string {
  const raw = (base_url ?? '').trim();
  if (!raw) return 'unknown';
  try {
    return new URL(raw).host || raw;
  } catch {
    return raw;
  }
}

function providerLabel(key: string): string {
  if (key === 'unknown') return 'unknown endpoint';
  return key.replace(/^(api|www|app)\./, '');
}

// Grouped once per render; order follows the first appearance so the select
// keeps the operator's own account ordering.
function groupAccounts(accounts: AccountLite[]): { key: string; label: string; accounts: AccountLite[] }[] {
  const groups: { key: string; label: string; accounts: AccountLite[] }[] = [];
  const byKey = new Map<string, number>();
  for (const a of accounts) {
    const key = providerKey(a.base_url);
    const at = byKey.get(key);
    if (at === undefined) {
      byKey.set(key, groups.length);
      groups.push({ key, label: providerLabel(key), accounts: [a] });
    } else {
      groups[at].accounts.push(a);
    }
  }
  return groups;
}

// HopRow carries one UI-only flag: whether the model id is typed by hand
// rather than picked from the account's catalog (doc #12). It never reaches
// the wire — submit maps each row onto the plain Hop shape.
type HopRow = Hop & { custom?: boolean };

const emptyHop = (account_id: number): HopRow => ({ account_id, model_id: '', weight: 1, enabled: true, hold_until_failure: false });

type Draft = {
  name: string; strategy: string; sticky_idle_s: number; enabled: boolean;
  compression_profile_id: number | null; context_size: number; hops: HopRow[];
  block_size: number;
  auto_free: boolean; auto_free_accounts: number[]; auto_free_include: FreeInclude[];
};

function newDraft(): Draft {
  return { name: '', strategy: 'failover', sticky_idle_s: 1800, context_size: 200000, enabled: true, compression_profile_id: null, hops: [], block_size: 5, auto_free: false, auto_free_accounts: [], auto_free_include: [] };
}

function HopArrow({ last }: { last: boolean }) {
  return last
    ? <span className="select-none font-mono text-[12px] text-mute">·</span>
    : <span className="select-none font-mono text-[12px] text-accent/60">→</span>;
}

function HopsEditor({ hops, setHops, accounts, strategy }: {
  hops: HopRow[]; setHops: (h: HopRow[]) => void; accounts: AccountLite[]; strategy: string;
}) {
  const move = (i: number, dir: -1 | 1) => {
    const j = i + dir;
    if (j < 0 || j >= hops.length) return;
    const next = [...hops];
    [next[i], next[j]] = [next[j], next[i]];
    setHops(next);
  };
  const set = (i: number, p: Partial<HopRow>) => setHops(hops.map((h, k) => (k === i ? { ...h, ...p } : h)));

  // One model catalog per referenced account, fetched once each. null = the
  // catalog didn't load, so that hop falls back to a typed model id.
  const [catalog, setCatalog] = useState<Record<number, string[] | null>>({});
  useEffect(() => {
    let alive = true;
    const ids = [...new Set(hops.map((h) => h.account_id))].filter((id) => id > 0);
    for (const id of ids) {
      if (catalog[id] !== undefined) continue;
      accountModels(id)
        .then((ms) => { if (alive) setCatalog((p) => ({ ...p, [id]: ms.map((m) => m.id) })); })
        .catch(() => { if (alive) setCatalog((p) => ({ ...p, [id]: null })); });
    }
    return () => { alive = false; };
  }, [hops, catalog]);

  return (
    <div className="space-y-2">
      <div className="flex items-center justify-between">
        <SectionLabel>Hops — ordered, positions = array order</SectionLabel>
        <button type="button" className={cx(btn.base, btn.ghost, 'px-2.5 py-1 text-[12px]')}
          onClick={() => setHops([...hops, emptyHop(accounts[0]?.id ?? 0)])}>
          ＋ Add hop
        </button>
      </div>
      {hops.length === 0 && (
        <p className="rounded-lg border border-dashed border-line px-4 py-5 text-center text-[13px] text-mute">
          No hops yet — add the first one.
        </p>
      )}
      {hops.map((h, i) => {
        const list = catalog[h.account_id];
        const models = list && list.length > 0 ? list : null;
        // Picked from the catalog by default; the checkbox below flips a row to
        // a free-typed id (an alias or a model the catalog has not synced yet).
        // Default unchecked (catalog pick). A row with no catalog yet shows the
        // free-text input, so the checkbox reads checked to match what is shown.
        const custom = h.custom ?? !models;
        return (
          // `layout` makes the row SLIDE to its new position when the operator
          // reorders the hops. In a combo, order IS the routing priority, so
          // this is the one animation in the app that carries information:
          // you see which hop moved where instead of re-reading the list.
          <ReorderItem key={i} className="flex flex-wrap items-center gap-2 rounded-lg border border-line bg-raised/60 px-3 py-2.5">
            <span className="w-5 text-center font-mono text-[12px] text-mute">{i + 1}</span>
            {/* Grouped by provider (base_url): a provider with MORE THAN ONE
                account gets the count in its group label — that count is what
                tells the operator the account choice below is meaningful.
                A single-account group stays bare, and the control stays enabled
                either way, because it is also how the provider is chosen. */}
            <select className={cx(inputCls, 'w-auto min-w-40 flex-1 py-1.5 text-[13px]')} value={h.account_id}
              onChange={(e) => set(i, { account_id: Number(e.target.value), custom: undefined })}>
              {groupAccounts(accounts).map((g) => (
                <optgroup key={g.key} label={g.accounts.length > 1 ? `${g.label} · ${g.accounts.length} accounts` : g.label}>
                  {g.accounts.map((a) => <option key={a.id} value={a.id}>{a.name}</option>)}
                </optgroup>
              ))}
              {accounts.length === 0 && <option value={0}>— no accounts —</option>}
            </select>
            {custom || !models ? (
              <input className={cx(inputCls, 'min-w-36 flex-[2] py-1.5 font-mono text-[13px]')} placeholder="model id · e.g. gpt-6-luna"
                value={h.model_id} onChange={(e) => set(i, { model_id: e.target.value.trim() })} spellCheck={false} />
            ) : (
              <select className={cx(inputCls, 'min-w-36 flex-[2] py-1.5 text-[13px]')} value={h.model_id}
                title="models synced from this account" onChange={(e) => set(i, { model_id: e.target.value })}>
                {!models.includes(h.model_id) && <option value={h.model_id}>{h.model_id} · not in catalog</option>}
                {models.map((m) => <option key={m} value={m}>{m}</option>)}
              </select>
            )}
            <label className="flex items-center gap-1.5 text-[12px] text-mute"
              title="type a model id instead of picking from the account catalog">
              <input type="checkbox" checked={custom} onChange={(e) => set(i, { custom: e.target.checked })} />
              custom model id
            </label>
            <Toggle checked={h.enabled} onChange={(v) => set(i, { enabled: v })} label="hop enabled" />
            {strategy === 'block_round_robin' && (
              <label className="flex items-center gap-1.5 text-[12px] text-mute"
                title="hold the block while this hop keeps succeeding; release it on a hop-attributable failure (429/5xx/transport/failed stream). A 4xx is the request's fault and keeps the hold.">
                <input type="checkbox" checked={h.hold_until_failure === true}
                  onChange={(e) => set(i, { hold_until_failure: e.target.checked })} />
                hold until fail
              </label>
            )}
            <div className="flex items-center gap-0.5">
              <button type="button" title="Move up" disabled={i === 0} className="rounded p-1 text-mute hover:bg-hover hover:text-ink disabled:opacity-30"
                onClick={() => move(i, -1)}>↑</button>
              <button type="button" title="Move down" disabled={i === hops.length - 1} className="rounded p-1 text-mute hover:bg-hover hover:text-ink disabled:opacity-30"
                onClick={() => move(i, 1)}>↓</button>
              <button type="button" title="Remove" className="rounded p-1 text-mute hover:bg-hover hover:text-bad"
                onClick={() => setHops(hops.filter((_, k) => k !== i))}>✕</button>
            </div>
            {i < hops.length - 1 && <HopArrow last={false} />}
          </ReorderItem>
        );
      })}
    </div>
  );
}

// FreePool — the editor body of an auto_free combo: which providers feed it,
// which unmarked models are added by hand, and the live pool (read-only: the
// server derives it; only a hop's enabled flag survives a sync, so that is the
// one thing editable here).
function FreePool({ d, setD, accounts }: {
  d: Draft; setD: (f: (p: Draft) => Draft) => void; accounts: AccountLite[];
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
        <SectionLabel className="mb-2">Also include — models with no free marker in their name</SectionLabel>
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
              <div key={`${h.account_id}/${h.model_id}`} className="flex items-center gap-3 px-4 py-2 text-[13px]">
                <span className={cx('min-w-0 flex-1 truncate font-mono', h.enabled ? 'text-ink' : 'text-mute')}>{h.model_id}</span>
                <span className="text-mute">{nameOf(h.account_id)}</span>
                <span className={cx('w-24 text-right text-[12px]', includeKey.has(`${h.account_id}/${h.model_id}`) ? 'text-ink' : 'text-accent')}>
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

function ComboModal({ open, editing, accounts, profiles, onClose, onSaved }: {
  open: boolean; editing: Combo | null;
  accounts: AccountLite[];
  profiles: { id: number; name: string }[];
  onClose: () => void; onSaved: () => void;
}) {
  const [d, setD] = useState<Draft>(newDraft());
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    if (!open) return;
    setErr(null); setBusy(false);
    if (editing) {
      setD({
        name: editing.name, strategy: editing.strategy, sticky_idle_s: editing.sticky_idle_s ?? 1800,
        enabled: editing.enabled, compression_profile_id: editing.compression_profile_id ?? null,
        context_size: editing.context_size ?? 200000,
        block_size: editing.block_size ?? 1,
        hops: (editing.hops ?? []).map((h) => ({ ...h })),
        auto_free: editing.auto_free === true,
        auto_free_accounts: editing.auto_free_accounts ?? [],
        auto_free_include: editing.auto_free_include ?? [],
      });
    } else setD(newDraft());
  }, [open, editing]);

  const setHops = (hops: HopRow[]) => setD((p) => ({ ...p, hops }));

  const submit = async () => {
    setBusy(true); setErr(null);
    const body: Record<string, unknown> = {
      name: d.name.trim(), strategy: d.strategy, sticky_idle_s: d.strategy === 'sticky_last_good' ? d.sticky_idle_s : null,
      enabled: d.enabled, compression_profile_id: d.compression_profile_id, context_size: d.context_size,
      // Always sent: the server keeps the stored value when this is 0, and 1 is
      // the smallest meaningful block, so a real value always wins.
      block_size: d.strategy === 'block_round_robin' ? d.block_size : 1,
      auto_free: d.auto_free, auto_free_accounts: d.auto_free_accounts, auto_free_include: d.auto_free_include,
      // strip the UI-only `custom` flag: the wire shape is the plain Hop.
      hops: d.hops.map((h) => ({
        account_id: h.account_id, model_id: h.model_id, weight: h.weight, enabled: h.enabled,
        hold_until_failure: h.hold_until_failure === true,
      })),
    };
    try {
      if (editing) await patch<Combo>(`/admin/combos/${editing.id}`, body);
      else await post<Combo>('/admin/combos', body);
      onSaved();
      onClose();
    } catch (e) {
      setErr(e instanceof ApiError ? `${e.detail.message}${e.detail.hint ? ` — ${e.detail.hint}` : ''}` : e instanceof Error ? e.message : String(e));
    } finally { setBusy(false); }
  };

  const valid = d.name.trim().length > 0 && (d.auto_free || d.hops.length > 0) && d.hops.every((h) => h.model_id.trim().length > 0 && h.account_id > 0);

  return (
    <Modal open={open} onClose={onClose} wide
      title={editing ? `Edit ${editing.name}` : 'New combo'}
      subtitle={editing ? 'PATCH replaces the whole hop list' : 'POST upserts by name'}
      footer={<>
        <button type="button" className={cx(btn.base, btn.ghost)} onClick={onClose}>Cancel</button>
        <button type="button" disabled={!valid || busy} className={cx(btn.base, btn.primary)} onClick={() => void submit()}>
          {busy ? <Spinner /> : null} {editing ? 'Save changes' : 'Create combo'}
        </button>
      </>}>
      <div className="space-y-5">
        <div className="grid gap-4 sm:grid-cols-2">
          <div>
            <label className={labelCls} htmlFor="cb-name">Name</label>
            <input id="cb-name" className={inputCls} placeholder="daily" value={d.name} spellCheck={false}
              onChange={(e) => setD((p) => ({ ...p, name: e.target.value }))} />
          </div>
          <div>
            <label className={labelCls} htmlFor="cb-cp">Compression profile</label>
            {profiles.length > 0 ? (
              <select id="cb-cp" className={inputCls} value={d.compression_profile_id ?? ''}
                onChange={(e) => setD((p) => ({ ...p, compression_profile_id: e.target.value === '' ? null : Number(e.target.value) }))}>
                <option value="">none</option>
                {profiles.map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
              </select>
            ) : (
              <select id="cb-cp" className={cx(inputCls, 'opacity-50')} disabled value="">
                <option value="">none — profiles arrive in M5</option>
              </select>
            )}
          </div>
          <div>
            <label className={labelCls} htmlFor="cb-context-size">Context size advertised to clients</label>
            <input id="cb-context-size" className={inputCls} type="number" min={1} max={10000000} step={1000}
              value={d.context_size} onChange={(e) => setD((p) => ({ ...p, context_size: Number(e.target.value) }))} />
            <p className="mt-1 text-[12px] text-mute">Clients such as Hermes use this metadata. ezllm does not enforce the context limit; direct models keep their provider metadata.</p>
          </div>
        </div>

        <div>
          <SectionLabel className="mb-2">Strategy</SectionLabel>
          <div className="grid gap-2 sm:grid-cols-2">
            {STRATEGIES.map((s) => (
              <button type="button" key={s}
                onClick={() => setD((p) => ({ ...p, strategy: s }))}
                className={cx('rounded-lg border px-3.5 py-2.5 text-left transition-colors',
                  d.strategy === s ? 'border-accent/60 bg-accent-dim' : 'border-line bg-raised/60 hover:border-line-strong')}>
                <span className="block font-mono text-[13px] font-medium">{s}</span>
                <span className={cx('mt-0.5 block text-[12px] leading-snug', d.strategy === s ? 'text-accent/80' : 'text-mute')}>
                  {STRATEGY_HINTS[s]}
                </span>
              </button>
            ))}
          </div>
          {/* The strategy's own extra fields COLLAPSE in: picking a strategy
              changes how much the form asks for, and a height animation shows
              the new fields arriving instead of the dialog silently
              resizing under the operator's hands. */}
          <Collapse open={d.strategy === 'sticky_last_good'}>
            <div className="mt-2 flex items-center gap-3">
              <label className="text-[12px] text-mute" htmlFor="cb-idle">sticky idle</label>
              <input id="cb-idle" type="number" min={0} className={cx(inputCls, 'w-24 py-1.5 font-mono text-[13px] tabular-nums')}
                value={d.sticky_idle_s} onChange={(e) => setD((p) => ({ ...p, sticky_idle_s: Number(e.target.value) || 0 }))} />
              <span className="text-[12px] text-mute">seconds — move on after this idle</span>
            </div>
          </Collapse>
          <Collapse open={d.strategy === 'block_round_robin'}>
            <div className="mt-3 rounded-lg border border-line bg-raised/60 p-3.5">
              <div className="flex items-center gap-3">
                <label className="text-[12px] text-mute" htmlFor="cb-block">block size</label>
                <input id="cb-block" type="number" min={1} max={1000} className={cx(inputCls, 'w-24 py-1.5 font-mono text-[13px] tabular-nums')}
                  value={d.block_size} onChange={(e) => setD((p) => ({ ...p, block_size: Number(e.target.value) || 1 }))} />
                <span className="text-[12px] text-mute">requests per hop before the rotation moves on</span>
              </div>
              <p className="mt-2 text-[12px] leading-snug text-mute">
                One hop serves this many requests in a row, then the next hop does the same, and so on.
                Because each account caches your prompt prefix, a run of consecutive requests to the same
                account keeps that cache warm — call <Mono>k</Mono> reads the prefix call <Mono>k-1</Mono> just
                sent, so the marginal cost collapses. Rotating on every request (the plain round robins)
                throws that away: the hop that receives the next call has never seen that prefix and pays a
                full cold read. Load is still split evenly — just in blocks instead of alternating.
                <span className="text-dim"> Every hop in the combo shares this size.</span>
              </p>
              {(() => {
                // A block cannot be served in full if it is longer than the
                // hop's own request budget — the budget gate will skip the hop
                // mid-block and the rotation will move on early.
                const tight = d.hops
                  .map((h) => accounts.find((a) => a.id === h.account_id))
                  .filter((a): a is AccountLite => !!a && (a.rate_limit ?? 0) > 0 && (a.rate_limit ?? 0) < d.block_size);
                if (tight.length === 0) return null;
                return (
                  <p className="mt-2 text-[12px] leading-snug text-warn">
                    Block size {d.block_size} exceeds the request budget of{' '}
                    {tight.map((a) => `${a.name} (${a.rate_limit} per ${a.rate_window})`).join(', ')} —
                    those hops will drop out mid-block and the rotation will advance early. Set the block
                    size to the smallest hop budget, or raise it.
                  </p>
                );
              })()}
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
          : <HopsEditor hops={d.hops} setHops={setHops} accounts={accounts} strategy={d.strategy} />}

        <div className="flex items-center gap-3">
          <Toggle checked={d.enabled} onChange={(v) => setD((p) => ({ ...p, enabled: v }))} label="combo enabled" />
          <span className="text-[13px] text-dim">Enabled</span>
        </div>

        {err && <div className="rounded-lg border border-bad/30 bg-bad-dim px-4 py-3 text-[13px] text-bad">{err}</div>}
      </div>
    </Modal>
  );
}

// FreeSummary — an auto_free card shows the pool as counts, not as dozens of
// hop chips: how many models, from how many providers, and the split.
function FreeSummary({ combo, accounts }: { combo: Combo; accounts: AccountLite[] }) {
  const live = (combo.hops ?? []).filter((h) => h.enabled !== false);
  if (live.length === 0) {
    return (
      <div className="mt-3 border-t border-line pt-3 text-[13px] text-mute">
        <span className="mr-2 text-[22px] font-semibold text-mute">–</span>
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

// Hop test results — POST /admin/combos/{id}/test. Each probe is one REAL
// upstream call (tiny: max_tokens 16) and lands in the ledger as client
// "combo-test", so what the modal shows is the dispatcher's genuine verdict,
// parks included.
type TestHop = {
  account_id: number; namespace?: string; model: string; enabled: boolean;
  status: number; ms: number; err?: string; cause?: string;
  parked?: boolean; cooldown_s?: number;
};
type TestResult = { combo: string; surface: string; hops: TestHop[] };

function TestComboModal({ combo, onClose }: { combo: Combo | null; onClose: () => void }) {
  const [surface, setSurface] = useState<'openai' | 'anthropic'>('openai');
  const [running, setRunning] = useState(false);
  const [result, setResult] = useState<TestResult | null>(null);
  const [err, setErr] = useState<string | null>(null);

  const run = useCallback(async (c: Combo, s: 'openai' | 'anthropic') => {
    setRunning(true); setErr(null);
    try { setResult(await post<TestResult>(`/admin/combos/${c.id}/test`, { surface: s })); }
    catch (e) { setErr(e instanceof ApiError ? e.detail.message : e instanceof Error ? e.message : String(e)); }
    finally { setRunning(false); }
  }, []);

  // Run on open; the surface selector + Run again re-run explicitly.
  useEffect(() => {
    if (combo) { setResult(null); void run(combo, surface); }
    // `surface` deliberately not a dep: changing it must not fire a paid
    // probe behind the operator's hand.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [combo]);

  return (
    <Modal open={combo != null} onClose={onClose} wide
      title={combo ? `Test combo ${combo.name}` : 'Test combo'}
      subtitle="one real request per hop — tiny (16 tokens out), recorded in the ledger as combo-test"
      footer={<>
        <span className="mr-auto flex items-center gap-2 text-[13px] text-mute">
          surface
          <select className={cx(inputCls, 'w-auto py-1 text-[13px]')} value={surface}
            onChange={(e) => setSurface(e.target.value as 'openai' | 'anthropic')}>
            <option value="openai">openai</option>
            <option value="anthropic">anthropic</option>
          </select>
        </span>
        <button type="button" className={cx(btn.base, btn.ghost)} onClick={onClose}>Close</button>
        <button type="button" disabled={running || !combo} className={cx(btn.base, btn.primary)}
          onClick={() => { if (combo) void run(combo, surface); }}>
          {running ? <Spinner /> : null} Run again
        </button>
      </>}>
      {err && (
        <div className="mb-3 rounded-lg border border-bad/30 bg-bad-dim px-4 py-3 text-[13px] text-bad">{err}</div>
      )}
      {running && !result && (
        <div className="flex items-center gap-2 px-2 py-6 text-[13px] text-mute">
          <Spinner /> probing {combo?.hops.length ?? 0} hop{combo?.hops.length === 1 ? '' : 's'}…
        </div>
      )}
      {result && (
        <div className="space-y-2">
          {result.hops.map((h, i) => (
            <div key={i} className="rounded-lg border border-line bg-raised/40 px-4 py-3">
              <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5">
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
                <span className="ml-auto font-mono text-[12px] text-mute">{h.ms} ms</span>
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
            it failed for real. No eligibility, capability or pin filtering — disabled and skipped hops are
            probed too, which is how you learn why the router avoids them.
          </p>
        </div>
      )}
    </Modal>
  );
}

export default function Combos() {
  const [combos, setCombos] = useState<Combo[] | null>(null);
  const [accounts, setAccounts] = useState<AccountLite[]>([]);
  const [profiles, setProfiles] = useState<{ id: number; name: string }[]>([]);
  const [modal, setModal] = useState<{ open: boolean; editing: Combo | null }>({ open: false, editing: null });
  const [pendingDelete, setPendingDelete] = useState<Combo | null>(null);
  const [testing, setTesting] = useState<Combo | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [busyID, setBusyID] = useState<number | null>(null);

  const load = useCallback(() => {
    get<Combo[]>('/admin/combos').then((c) => { setCombos(c); setErr(null); }).catch((e) => setErr(e instanceof Error ? e.message : 'failed to load'));
    get<AccountLite[]>('/admin/accounts')
      .then((a) => setAccounts(a.map((x) => ({ id: x.id, name: x.name, base_url: x.base_url, rate_limit: x.rate_limit, rate_window: x.rate_window }))))
      .catch(() => { /* non-fatal */ });
    get<{ id: number; name: string }[]>('/admin/compression-profiles')
      .then((p) => setProfiles(p.map((x) => ({ id: x.id, name: String((x as { name?: unknown }).name ?? x.id) }))))
      .catch(() => setProfiles([]));
  }, []);

  useEffect(() => { load(); }, [load]);

  const remove = async (c: Combo) => {
    setErr(null);
    try { await del(`/admin/combos/${c.id}`); load(); }
    catch (e) { setErr(e instanceof ApiError ? e.detail.message : e instanceof Error ? e.message : String(e)); }
    finally { setPendingDelete(null); }
  };

  // One-field PATCH: the server merges the rest, so this cannot clobber hops or
  // strategy. Optimistic so the switch answers the tap immediately; the reload
  // in `finally` puts the server's truth back on screen either way.
  const setEnabled = async (c: Combo, next: boolean) => {
    setErr(null); setBusyID(c.id);
    setCombos((prev) => prev?.map((x) => (x.id === c.id ? { ...x, enabled: next } : x)) ?? prev);
    try { await patch<Combo>(`/admin/combos/${c.id}`, { enabled: next }); }
    catch (e) { setErr(e instanceof ApiError ? e.detail.message : e instanceof Error ? e.message : String(e)); }
    finally { setBusyID(null); load(); }
  };

  return (
    // No page-root fade: the route transition in App.tsx already owns the
    // page entrance. A second entrance stacked on it reads as a stutter.
    <div>
      <div className="mb-4 flex items-center justify-between">
        <SectionLabel>Routing chains</SectionLabel>
        <button type="button" className={cx(btn.base, btn.primary)} onClick={() => setModal({ open: true, editing: null })}>
          ＋ New combo
        </button>
      </div>

      {err && <div className="mb-4 rounded-lg border border-bad/30 bg-bad-dim px-4 py-3 text-[13px] text-bad">{err}</div>}

      {combos === null ? (
        <SkeletonCards n={2} />
      ) : combos.length === 0 ? (
        <div className="rounded-xl border border-line bg-surface px-6 py-14 text-center">
          <p className="text-[14px] font-medium">No combos yet</p>
          <p className="mt-1 text-[13px] text-mute">A combo is an ordered chain of hops — clients pick one by path alias and it tries hops per its strategy.</p>
          <button type="button" className={cx(btn.base, btn.primary, 'mt-5')} onClick={() => setModal({ open: true, editing: null })}>＋ Create your first combo</button>
        </div>
      ) : (
        <div className="space-y-3">
          <Stagger className="space-y-3">
            {combos.map((c) => (
              <StaggerItem key={c.id}>
                <div className="rounded-xl border border-line bg-surface px-5 py-4">
                  <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
                    <span className="text-[14px] font-semibold">{c.name}</span>
                    <Chip><span className="font-mono">{c.strategy}</span></Chip>
                    {c.auto_free && (
                      <Chip tone="accent" title="hops are derived from your providers' free models and kept in sync">Auto</Chip>
                    )}
                    {/* Item 10: the combo's compression profile, visible without
                        opening the editor. "none" renders nothing — an empty pill
                        on every card is noise. */}
                    {!!c.compression_profile_id && (
                      <Chip tone="accent" title="compression profile applied to requests routed through this combo">
                        <span className="font-mono">{profiles.find((p) => p.id === c.compression_profile_id)?.name ?? `profile #${c.compression_profile_id}`}</span>
                      </Chip>
                    )}
                    <span className="ml-auto flex items-center gap-3">
                      <label className="flex items-center gap-2 text-[13px] text-dim">
                        <Toggle checked={c.enabled} disabled={busyID === c.id}
                          label={`${c.enabled ? 'Disable' : 'Enable'} combo ${c.name}`}
                          onChange={(v) => void setEnabled(c, v)} />
                        <span>{c.enabled ? 'Enabled' : 'Disabled'}</span>
                      </label>
                      <span className="flex items-center gap-2">
                        <button type="button" className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[13px]')}
                          title="probe every hop with one tiny real request"
                          onClick={() => setTesting(c)}>Test</button>
                        <button type="button" className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[13px]')} onClick={() => setModal({ open: true, editing: c })}>Edit</button>
                        <button type="button" className={cx(btn.base, btn.danger, 'px-3 py-1.5 text-[13px]')} onClick={() => setPendingDelete(c)}>Delete</button>
                      </span>
                    </span>
                  </div>
                  {c.auto_free && <FreeSummary combo={c} accounts={accounts} />}
                  {!c.auto_free && <div className="mt-3 flex flex-wrap items-center gap-2 font-mono text-[12px]">
                    {(c.hops ?? []).length === 0
                      ? <span className="font-sans text-[13px] text-mute">no hops configured</span>
                      : (c.hops ?? []).map((h, i) => (
                        <span key={i} className="flex items-center gap-2">
                          {i > 0 && <span className="text-accent/50">→</span>}
                          <span className={cx('rounded-md border px-2 py-1', h.enabled ? 'border-line bg-raised' : 'border-line/50 bg-transparent opacity-40')}>
                            {accounts.find((a) => a.id === h.account_id)?.name ?? `#${h.account_id}`}
                            <span className="mx-1 text-mute">/</span>
                            <span className={h.enabled ? 'text-ink' : 'text-mute'}>{h.model_id}</span>
                            {h.hold_until_failure && (
                              <span className="ml-1.5 rounded border border-accent/40 px-1 text-[11px] text-accent"
                                title="holds the block while it succeeds; releases on a hop-attributable failure">
                                hold
                              </span>
                            )}
                          </span>
                        </span>
                      ))}
                  </div>}
                  {c.strategy === 'sticky_last_good' && (
                    <div className="mt-2 text-[12px] text-mute">sticky idle <Mono>{c.sticky_idle_s ?? '–'}s</Mono></div>
                  )}
                  {c.strategy === 'block_round_robin' && (
                    <div className="mt-2 text-[12px] text-mute">
                      block size <Mono>{c.block_size ?? 1}</Mono> — one hop serves {c.block_size ?? 1} request
                      {(c.block_size ?? 1) === 1 ? '' : 's'} in a row before rotating
                    </div>
                  )}
                </div>
              </StaggerItem>
            ))}
          </Stagger>
        </div>
      )}

      <ComboModal open={modal.open} editing={modal.editing} accounts={accounts} profiles={profiles}
        onClose={() => setModal({ open: false, editing: null })} onSaved={() => { setModal({ open: false, editing: null }); load(); }} />
      <TestComboModal combo={testing} onClose={() => setTesting(null)} />
      <ConfirmModal
        open={pendingDelete != null}
        title={pendingDelete ? `Delete combo ${pendingDelete.name}?` : 'Delete combo?'}
        body="Clients pointing at this combo will get errors until you recreate it or change their model."
        busy={false}
        onClose={() => setPendingDelete(null)}
        onConfirm={() => { if (pendingDelete) void remove(pendingDelete); }} />
    </div>
  );
}
