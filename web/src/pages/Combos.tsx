// COMBOS — list cards + create/edit modal with an ordered hops editor.
import { useCallback, useEffect, useState } from 'react';
import { ApiError, accountModels, del, get, patch, post, STRATEGY_HINTS, type Combo, type Hop } from '../api';
import { Chip, Modal, Mono, SectionLabel, SkeletonCards, Spinner, Toggle, btn, cx, inputCls, labelCls } from '../ui';

const STRATEGIES = ['failover', 'true_round_robin', 'strict_round_robin', 'sticky_last_good', 'least_used'] as const;

// HopRow carries one UI-only flag: whether the model id is typed by hand
// rather than picked from the account's catalog (doc #12). It never reaches
// the wire — submit maps each row onto the plain Hop shape.
type HopRow = Hop & { custom?: boolean };

const emptyHop = (account_id: number): HopRow => ({ account_id, model_id: '', weight: 1, enabled: true });

type Draft = {
  name: string; strategy: string; sticky_idle_s: number; enabled: boolean;
  compression_profile_id: number | null; context_size: number; hops: HopRow[];
};

function newDraft(): Draft {
  return { name: '', strategy: 'failover', sticky_idle_s: 1800, context_size: 200000, enabled: true, compression_profile_id: null, hops: [] };
}

function HopArrow({ last }: { last: boolean }) {
  return last
    ? <span className="select-none font-mono text-[11px] text-mute">·</span>
    : <span className="select-none font-mono text-[11px] text-accent/60">→</span>;
}

function HopsEditor({ hops, setHops, accounts }: {
  hops: HopRow[]; setHops: (h: HopRow[]) => void; accounts: { id: number; name: string }[];
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
        <button type="button" className={cx(btn.base, btn.ghost, 'px-2.5 py-1 text-[11.5px]')}
          onClick={() => setHops([...hops, emptyHop(accounts[0]?.id ?? 0)])}>
          ＋ Add hop
        </button>
      </div>
      {hops.length === 0 && (
        <p className="rounded-lg border border-dashed border-line px-4 py-5 text-center text-[12px] text-mute">
          No hops yet — add the first one.
        </p>
      )}
      {hops.map((h, i) => {
        const list = catalog[h.account_id];
        const models = list && list.length > 0 ? list : null;
        // Picked from the catalog by default; the checkbox below flips a row to
        // a free-typed id (an alias or a model the catalog has not synced yet).
        const custom = h.custom ?? !(models && models.includes(h.model_id));
        return (
          <div key={i} className="flex flex-wrap items-center gap-2 rounded-lg border border-line bg-raised/60 px-3 py-2.5">
            <span className="w-5 text-center font-mono text-[11px] text-mute">{i + 1}</span>
            <select className={cx(inputCls, 'w-auto min-w-40 flex-1 py-1.5 text-[12px]')} value={h.account_id}
              onChange={(e) => set(i, { account_id: Number(e.target.value), custom: undefined })}>
              {accounts.map((a) => <option key={a.id} value={a.id}>{a.name}</option>)}
              {accounts.length === 0 && <option value={0}>— no accounts —</option>}
            </select>
            {custom || !models ? (
              <input className={cx(inputCls, 'min-w-36 flex-[2] py-1.5 font-mono text-[12px]')} placeholder="model id · e.g. gpt-6-luna"
                value={h.model_id} onChange={(e) => set(i, { model_id: e.target.value.trim() })} spellCheck={false} />
            ) : (
              <select className={cx(inputCls, 'min-w-36 flex-[2] py-1.5 text-[12px]')} value={h.model_id}
                title="models synced from this account" onChange={(e) => set(i, { model_id: e.target.value })}>
                {!models.includes(h.model_id) && <option value={h.model_id}>{h.model_id} · not in catalog</option>}
                {models.map((m) => <option key={m} value={m}>{m}</option>)}
              </select>
            )}
            <label className="flex items-center gap-1.5 text-[11px] text-mute"
              title="type a model id instead of picking from the account catalog">
              <input type="checkbox" checked={custom} onChange={(e) => set(i, { custom: e.target.checked })} />
              custom model id
            </label>
            <label className="flex items-center gap-1.5 text-[11px] text-mute">
              w
              <input type="number" min={1} className={cx(inputCls, 'w-16 py-1.5 font-mono text-[12px] tabular-nums')}
                value={h.weight} onChange={(e) => set(i, { weight: Math.max(1, Number(e.target.value) || 1) })} />
            </label>
            <Toggle checked={h.enabled} onChange={(v) => set(i, { enabled: v })} label="hop enabled" />
            <div className="flex items-center gap-0.5">
              <button type="button" title="Move up" disabled={i === 0} className="rounded p-1 text-mute hover:bg-hover hover:text-ink disabled:opacity-30"
                onClick={() => move(i, -1)}>↑</button>
              <button type="button" title="Move down" disabled={i === hops.length - 1} className="rounded p-1 text-mute hover:bg-hover hover:text-ink disabled:opacity-30"
                onClick={() => move(i, 1)}>↓</button>
              <button type="button" title="Remove" className="rounded p-1 text-mute hover:bg-hover hover:text-bad"
                onClick={() => setHops(hops.filter((_, k) => k !== i))}>✕</button>
            </div>
            {i < hops.length - 1 && <HopArrow last={false} />}
          </div>
        );
      })}
    </div>
  );
}

function ComboModal({ open, editing, accounts, profiles, onClose, onSaved }: {
  open: boolean; editing: Combo | null;
  accounts: { id: number; name: string }[];
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
        hops: (editing.hops ?? []).map((h) => ({ ...h })),
      });
    } else setD(newDraft());
  }, [open, editing]);

  const setHops = (hops: HopRow[]) => setD((p) => ({ ...p, hops }));

  const submit = async () => {
    setBusy(true); setErr(null);
    const body: Record<string, unknown> = {
      name: d.name.trim(), strategy: d.strategy, sticky_idle_s: d.strategy === 'sticky_last_good' ? d.sticky_idle_s : null,
      enabled: d.enabled, compression_profile_id: d.compression_profile_id, context_size: d.context_size,
      // strip the UI-only `custom` flag: the wire shape is the plain Hop.
      hops: d.hops.map((h) => ({ account_id: h.account_id, model_id: h.model_id, weight: h.weight, enabled: h.enabled })),
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

  const valid = d.name.trim().length > 0 && d.hops.length > 0 && d.hops.every((h) => h.model_id.trim().length > 0 && h.account_id > 0);

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
            <p className="mt-1 text-[11px] text-mute">Clients such as Hermes use this metadata. ezllm does not enforce the context limit; direct models keep their provider metadata.</p>
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
                <span className="block font-mono text-[12.5px] font-medium">{s}</span>
                <span className={cx('mt-0.5 block text-[11px] leading-snug', d.strategy === s ? 'text-accent/80' : 'text-mute')}>
                  {STRATEGY_HINTS[s]}
                </span>
              </button>
            ))}
          </div>
          {d.strategy === 'sticky_last_good' && (
            <div className="mt-2 flex items-center gap-3">
              <label className="text-[11px] uppercase tracking-wide text-mute" htmlFor="cb-idle">sticky idle</label>
              <input id="cb-idle" type="number" min={0} className={cx(inputCls, 'w-24 py-1.5 font-mono text-[12px] tabular-nums')}
                value={d.sticky_idle_s} onChange={(e) => setD((p) => ({ ...p, sticky_idle_s: Number(e.target.value) || 0 }))} />
              <span className="text-[11px] text-mute">seconds — move on after this idle</span>
            </div>
          )}
        </div>

        <HopsEditor hops={d.hops} setHops={setHops} accounts={accounts} />

        <div className="flex items-center gap-3">
          <Toggle checked={d.enabled} onChange={(v) => setD((p) => ({ ...p, enabled: v }))} label="combo enabled" />
          <span className="text-[12.5px] text-dim">Enabled</span>
        </div>

        {err && <div className="rounded-lg border border-bad/30 bg-[rgba(248,113,113,0.06)] px-4 py-3 text-[12.5px] text-bad">{err}</div>}
      </div>
    </Modal>
  );
}

export default function Combos() {
  const [combos, setCombos] = useState<Combo[] | null>(null);
  const [accounts, setAccounts] = useState<{ id: number; name: string }[]>([]);
  const [profiles, setProfiles] = useState<{ id: number; name: string }[]>([]);
  const [modal, setModal] = useState<{ open: boolean; editing: Combo | null }>({ open: false, editing: null });
  const [err, setErr] = useState<string | null>(null);
  const [busyID, setBusyID] = useState<number | null>(null);

  const load = useCallback(() => {
    get<Combo[]>('/admin/combos').then((c) => { setCombos(c); setErr(null); }).catch((e) => setErr(e instanceof Error ? e.message : 'failed to load'));
    get<{ id: number; name: string }[]>('/admin/accounts').then((a) => setAccounts(a.map((x) => ({ id: x.id, name: x.name })))).catch(() => { /* non-fatal */ });
    get<{ id: number; name: string }[]>('/admin/compression-profiles')
      .then((p) => setProfiles(p.map((x) => ({ id: x.id, name: String((x as { name?: unknown }).name ?? x.id) }))))
      .catch(() => setProfiles([]));
  }, []);

  useEffect(() => { load(); }, [load]);

  const remove = async (c: Combo) => {
    setErr(null);
    try { await del(`/admin/combos/${c.id}`); load(); }
    catch (e) { setErr(e instanceof ApiError ? e.detail.message : e instanceof Error ? e.message : String(e)); }
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
    <div className="animate-fade-up">
      <div className="mb-4 flex items-center justify-between">
        <SectionLabel>Routing chains</SectionLabel>
        <button type="button" className={cx(btn.base, btn.primary)} onClick={() => setModal({ open: true, editing: null })}>
          ＋ New combo
        </button>
      </div>

      {err && <div className="mb-4 rounded-lg border border-bad/30 bg-[rgba(248,113,113,0.06)] px-4 py-3 text-[12.5px] text-bad">{err}</div>}

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
          {combos.map((c) => (
            <div key={c.id} className="rounded-xl border border-line bg-surface px-5 py-4 animate-fade-up">
              <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
                <span className="text-[14px] font-semibold">{c.name}</span>
                <Chip><span className="font-mono">{c.strategy}</span></Chip>
                {c.compression_profile_id != null && <Chip tone="accent">profile #{c.compression_profile_id}</Chip>}
                <span className="ml-auto flex items-center gap-3">
                  <label className="flex items-center gap-2 text-[12px] text-dim">
                    <Toggle checked={c.enabled} disabled={busyID === c.id}
                      label={`${c.enabled ? 'Disable' : 'Enable'} combo ${c.name}`}
                      onChange={(v) => void setEnabled(c, v)} />
                    <span>{c.enabled ? 'Enabled' : 'Disabled'}</span>
                  </label>
                  <span className="flex items-center gap-2">
                    <button type="button" className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[12px]')} onClick={() => setModal({ open: true, editing: c })}>Edit</button>
                    <button type="button" className={cx(btn.base, btn.danger, 'px-3 py-1.5 text-[12px]')} onClick={() => void remove(c)}>Delete</button>
                  </span>
                </span>
              </div>
              <div className="mt-3 flex flex-wrap items-center gap-2 font-mono text-[11.5px]">
                {(c.hops ?? []).length === 0
                  ? <span className="font-sans text-[12px] text-mute">no hops configured</span>
                  : (c.hops ?? []).map((h, i) => (
                    <span key={i} className="flex items-center gap-2">
                      {i > 0 && <span className="text-accent/50">→</span>}
                      <span className={cx('rounded-md border px-2 py-1', h.enabled ? 'border-line bg-raised' : 'border-line/50 bg-transparent opacity-40')}>
                        {accounts.find((a) => a.id === h.account_id)?.name ?? `#${h.account_id}`}
                        <span className="mx-1 text-mute">/</span>
                        <span className={h.enabled ? 'text-ink' : 'text-mute'}>{h.model_id}</span>
                        {h.weight > 1 && <span className="ml-1.5 text-mute">×{h.weight}</span>}
                      </span>
                    </span>
                  ))}
              </div>
              {c.strategy === 'sticky_last_good' && (
                <div className="mt-2 text-[11px] text-mute">sticky idle <Mono>{c.sticky_idle_s ?? '–'}s</Mono></div>
              )}
            </div>
          ))}
        </div>
      )}

      <ComboModal open={modal.open} editing={modal.editing} accounts={accounts} profiles={profiles}
        onClose={() => setModal({ open: false, editing: null })} onSaved={() => { setModal({ open: false, editing: null }); load(); }} />
    </div>
  );
}
