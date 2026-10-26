// Provider keys form a miniature town. Only activity indicators encode usage;
// architecture is scenery. The exact ledger figures remain in the table.
import { useEffect, useMemo, useState } from 'react';
import { Link } from 'react-router-dom';
import { useReducedMotion } from '../utils/preferences';
import { apiFetch, type Account, type UsageResp } from '../utils/api';
import { Chip, EmptyState, SectionLabel, btn, cx, formatTokens } from '../components/ui';
import NeighbourhoodScene from '../components/NeighbourhoodScene';

const WINDOW_MIN = 5;
const POLL_MS = 10_000;
const MIN_REF_TPM = 2_000;
type State = 'live' | 'idle' | 'off' | 'unknown';
export type House = {
  id: string; hint: string; label: string; account: string;
  state: State; tpm: number; calls: number; errors: number; level: number; storm: boolean;
};
export function glowLevel(tpm: number, ref: number): number {
  if (tpm <= 0) return 0;
  return Math.min(1, Math.log10(1 + tpm) / Math.log10(1 + Math.max(ref, MIN_REF_TPM)));
}
export function buildHouses(accounts: Account[], usage: UsageResp | null): House[] {
  const byHint = new Map((usage?.groups ?? []).map(g => [g.k, g]));
  const raw = accounts.flatMap(a => a.keys.map(k => {
    const g = byHint.get(k.hint);
    const enabled = a.enabled && k.enabled;
    return { a, k, enabled, tpm: enabled && g ? (g.tin + g.tout) / WINDOW_MIN : 0,
      calls: enabled ? g?.calls ?? 0 : 0, errors: enabled ? g?.errors ?? 0 : 0 };
  }));
  const ref = Math.max(MIN_REF_TPM, ...raw.map(r => r.tpm));
  return raw.map(({ a, k, enabled, tpm, calls, errors }) => ({
    id: `${a.id}:${k.id}`, hint: k.hint, label: k.label || k.hint, account: a.name,
    state: !enabled ? 'off' : usage === null ? 'unknown' : tpm > 0 ? 'live' : 'idle',
    tpm, calls, errors, level: glowLevel(tpm, ref), storm: calls >= 3 && errors / calls >= 0.25,
  }));
}
function useNeighbourhood() {
  const [accounts, setAccounts] = useState<Account[] | null>(null);
  const [usage, setUsage] = useState<UsageResp | null>(null);
  const [stale, setStale] = useState(false);
  const [loading, setLoading] = useState(true);
  const [at, setAt] = useState<number | null>(null);
  const [attempt, setAttempt] = useState(0);
  useEffect(() => {
    let alive = true, inFlight = false;
    const controller = new AbortController();
    setLoading(true);
    const load = async () => {
      if (document.hidden || inFlight) return;
      inFlight = true;
      const from = new Date(Date.now() - WINDOW_MIN * 60_000).toISOString();
      const [a, u] = await Promise.allSettled([
        apiFetch<Account[]>('/admin/accounts', { method: 'GET', signal: controller.signal }),
        apiFetch<UsageResp>(`/admin/usage?from=${encodeURIComponent(from)}&group_by=key`, { method: 'GET', signal: controller.signal }),
      ]);
      inFlight = false;
      if (!alive) return;
      if (a.status === 'fulfilled') setAccounts(a.value);
      if (u.status === 'fulfilled') setUsage(u.value);
      const complete = a.status === 'fulfilled' && u.status === 'fulfilled';
      setStale(!complete); setLoading(false);
      if (complete) setAt(Date.now());
    };
    void load();
    const iv = setInterval(() => void load(), POLL_MS);
    const onVisible = () => { if (!document.hidden) void load(); };
    document.addEventListener('visibilitychange', onVisible);
    return () => { alive = false; controller.abort(); clearInterval(iv); document.removeEventListener('visibilitychange', onVisible); };
  }, [attempt]);
  const houses = useMemo(() => accounts ? buildHouses(accounts, usage) : null, [accounts, usage]);
  return { houses, stale, loading, at, retry: () => setAttempt(n => n + 1) };
}

function Glyph({ kind }: { kind: 'lit' | 'smoke' | 'storm' | 'off' }) {
  return (
    <svg width={18} height={18} viewBox="0 0 18 18" aria-hidden className="shrink-0">
      {kind === 'lit' && <><circle cx={9} cy={9} r={8} fill="var(--nb-glow)" opacity={0.35} /><rect x={4} y={4} width={10} height={10} rx={1.5} fill="var(--nb-window-on)" /></>}
      {kind === 'smoke' && <g fill="var(--nb-smoke)"><circle cx={6} cy={13} r={2.6} /><circle cx={9.5} cy={8.5} r={3.2} opacity={0.75} /><circle cx={13} cy={4} r={2.6} opacity={0.5} /></g>}
      {kind === 'storm' && <><g fill="var(--nb-cloud)"><circle cx={6} cy={7} r={4} /><circle cx={11} cy={5.5} r={4.5} /><rect x={2} y={7} width={14} height={4} rx={2} /></g><g stroke="var(--color-bad)" strokeWidth={1.4} strokeLinecap="round"><line x1={6} y1={13} x2={5} y2={16} /><line x1={11} y1={13} x2={10} y2={16} /></g></>}
      {kind === 'off' && <><rect x={4} y={4} width={10} height={10} rx={1.5} fill="var(--nb-window-off)" /><g stroke="var(--nb-wall-line)" strokeWidth={1.8} strokeLinecap="round"><line x1={3} y1={7} x2={15} y2={10} /><line x1={3} y1={11} x2={15} y2={8} /></g></>}
    </svg>
  );
}

function Ago({ at }: { at: number | null }) {
  const [, tick] = useState(0);
  useEffect(() => { const iv = setInterval(() => tick((n) => n + 1), 1000); return () => clearInterval(iv); }, []);
  if (at === null) return null;
  const s = Math.max(0, Math.round((Date.now() - at) / 1000));
  return <span className="tnum">updated {s < 2 ? 'just now' : `${s}s ago`}</span>;
}

export default function Neighbourhood() {
  const { houses, stale, loading, at, retry } = useNeighbourhood();
  const reduce = useReducedMotion();
  const [paused, setPaused] = useState(false);
  const [hidden, setHidden] = useState(document.hidden);
  const [hot, setHot] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  useEffect(() => { const update = () => setHidden(document.hidden); document.addEventListener('visibilitychange', update); return () => document.removeEventListener('visibilitychange', update); }, []);
  useEffect(() => { if (selected && houses && !houses.some(h => h.id === selected)) setSelected(null); }, [houses, selected]);
  const ranked = useMemo(() => [...(houses ?? [])].sort((a, b) => b.tpm - a.tpm || a.label.localeCompare(b.label)), [houses]);
  const detail = houses?.find(h => h.id === (hot ?? selected));
  const live = houses?.filter(h => h.state === 'live').length ?? 0;
  const idle = houses?.filter(h => h.state === 'idle').length ?? 0;
  const disabled = houses?.filter(h => h.state === 'off').length ?? 0;
  const total = houses?.reduce((n, h) => n + h.tpm, 0) ?? 0;
  const unknown = houses?.some(h => h.state === 'unknown');
  const still = reduce || paused || hidden;
  const select = (id: string) => setSelected(previous => previous === id ? null : id);
  const ref = Math.max(MIN_REF_TPM, ...(houses ?? []).map(h => h.tpm));

  if (houses === null) return <div className="nbhd nbhd-loading flex min-h-[360px] flex-col items-center justify-center gap-4 rounded-2xl border border-line bg-surface px-6 text-center">
    <span className="text-3xl text-accent" aria-hidden>⌂</span>
    {loading ? <p role="status" className="text-sm text-dim">Lighting the lamps…</p> : <><p role="alert" className="text-sm text-dim">Could not load the neighbourhood.</p><button className={cx(btn.base, btn.ghost, 'min-h-10')} onClick={retry}>Try again</button></>}
  </div>;
  if (!houses.length) return <div className="space-y-4"><EmptyState title="No keys yet." hint="Add a provider key under Providers and its house appears here." /><Link className={cx(btn.base, btn.primary, 'min-h-10')} to="/providers">Add a provider</Link></div>;

  return <div className="nbhd space-y-4">
    <div className="nbhd-toolbar flex flex-wrap items-center justify-between gap-4">
      <div className="flex flex-wrap items-center gap-x-5 gap-y-2 text-xs text-dim" aria-label="Neighbourhood summary">
        <span><strong className="tnum font-medium text-ink">{houses.length}</strong> houses</span>
        <span className="flex items-center gap-1.5"><span className="nbhd-status-light" /><strong className="tnum font-medium text-ink">{unknown ? '—' : live}</strong> serving</span>
        <span><strong className="tnum font-medium text-ink">{unknown ? '—' : idle}</strong> quiet</span>
        <span><strong className="tnum font-medium text-ink">{disabled}</strong> disabled</span>
        <span className="font-mono text-ink">{unknown ? 'Usage unavailable' : `${formatTokens(total)} tokens / min`}</span>
      </div>
      <button type="button" className={cx(btn.base, btn.ghost, 'min-h-10')} disabled={reduce} aria-pressed={paused || reduce}
        onClick={() => setPaused(p => !p)} title={reduce ? 'Reduced motion is enabled in your browser or Settings.' : 'Pause the scenery; usage updates continue.'}>
        <span aria-hidden>{still ? '▷' : 'Ⅱ'}</span>{reduce ? 'Reduced motion' : paused ? 'Resume scene' : 'Pause scene'}
      </button>
    </div>
    {stale && <div role="status" className="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-warn/30 bg-warn-dim px-4 py-3 text-xs text-warn">
      <span>{unknown ? 'Usage could not be loaded. Houses are shown without activity figures.' : 'Last update failed — showing the latest available scene.'}</span>
      <button className="min-h-10 px-2 font-medium underline underline-offset-4" onClick={retry}>Try again</button>
    </div>}
    <NeighbourhoodScene houses={houses} hot={hot} selected={selected} still={still} onHover={setHot} onSelect={select} />
    <div className="nbhd-inspector flex min-h-[76px] flex-wrap items-center justify-between gap-3 rounded-xl border border-line bg-surface px-4 py-3" aria-label="Selected house" aria-live="polite">
      {detail ? <>
        <div className="min-w-0"><p className="break-words text-sm font-medium text-ink">{detail.label}</p><p className="mt-0.5 text-xs text-mute">{detail.account} · {detail.state === 'off' ? 'disabled' : detail.state === 'unknown' ? 'usage unavailable' : detail.storm ? 'stormy' : detail.state === 'live' ? 'serving traffic' : 'quiet'}</p></div>
        <div className="flex flex-wrap gap-x-5 gap-y-1 font-mono text-xs text-dim"><span>{detail.state === 'unknown' ? '—' : formatTokens(detail.tpm)} tokens / min</span><span>{detail.state === 'unknown' ? '—' : formatTokens(detail.calls)} calls</span><span>{detail.state === 'unknown' ? '—' : formatTokens(detail.errors)} errors</span></div>
      </> : <><p className="text-sm text-dim">Every key has a place here.</p><p className="text-xs text-mute">Hover, focus or select a house to look inside.</p></>}
    </div>
    <div className="space-y-2">
      <ul className="flex flex-wrap items-center gap-x-5 gap-y-2 text-xs text-dim">
        <li className="flex items-center gap-1.5"><Glyph kind="lit" />Brighter = more tokens per minute</li>
        <li className="flex items-center gap-1.5"><Glyph kind="smoke" />Recent traffic</li>
        <li className="flex items-center gap-1.5"><Glyph kind="storm" />A quarter or more of calls failed</li>
        <li className="flex items-center gap-1.5"><Glyph kind="off" />Disabled</li>
        <li className="ml-auto text-mute"><Ago at={at} /> · last {WINDOW_MIN} minutes</li>
      </ul>
      <p className="text-xs text-mute">Window glow follows the last five minutes of usage. Full glow is never less than {formatTokens(MIN_REF_TPM)} tokens a minute.</p>
    </div>
    <div className="rounded-xl border border-line bg-surface">
      <div className="px-4 pt-3"><SectionLabel>Every house, busiest first</SectionLabel></div>
      <div className="overflow-x-auto">
        <table className="w-full min-w-[640px] text-[13px]">
          <thead><tr className="text-left text-[11px] text-mute">
            <th className="px-4 py-2 font-medium">Key</th><th className="px-3 py-2 font-medium">Account</th><th className="px-3 py-2 font-medium">Glow</th>
            <th className="px-3 py-2 text-right font-medium">Tokens / min</th><th className="px-3 py-2 text-right font-medium">Calls</th><th className="px-3 py-2 text-right font-medium">Errors</th><th className="px-4 py-2 font-medium">State</th>
          </tr></thead>
          <tbody>{ranked.map(h => <tr key={h.id} data-row={h.id} className={cx('border-t border-line transition-colors', (hot === h.id || selected === h.id) && 'bg-hover')}
            onMouseEnter={() => setHot(h.id)} onMouseLeave={() => setHot(null)}>
            <td className="px-4 py-2"><button className="min-h-10 text-left font-mono underline-offset-4 hover:underline focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent" onClick={() => select(h.id)} aria-pressed={selected === h.id} onFocus={() => setHot(h.id)} onBlur={() => setHot(null)}>{h.label}</button></td>
            <td className="px-3 py-2 text-dim">{h.account}</td>
            <td className="px-3 py-2"><div className="h-1.5 w-20 overflow-hidden rounded-full bg-line" title={h.state === 'unknown' ? 'Usage unavailable' : `${Math.round(h.level * 100)}% of full glow`}><div className="h-full rounded-full" style={{ width: `${Math.round(h.level * 100)}%`, background: 'var(--nb-window-on)' }} /></div></td>
            <td className="tnum px-3 py-2 text-right font-mono">{h.state === 'unknown' ? '—' : formatTokens(h.tpm)}</td>
            <td className="tnum px-3 py-2 text-right font-mono">{h.state === 'unknown' ? '—' : formatTokens(h.calls)}</td>
            <td className={cx('tnum px-3 py-2 text-right font-mono', h.errors > 0 && 'text-bad')}>{h.state === 'unknown' ? '—' : formatTokens(h.errors)}</td>
            <td className="px-4 py-2"><Chip tone={h.state === 'unknown' ? 'warn' : h.state === 'off' ? 'neutral' : h.storm ? 'bad' : h.state === 'live' ? 'ok' : 'neutral'}>{h.state === 'unknown' ? 'unavailable' : h.state === 'off' ? 'disabled' : h.storm ? 'stormy' : h.state === 'live' ? 'lit' : 'quiet'}</Chip></td>
          </tr>)}</tbody>
        </table>
      </div>
      <p className="px-4 pb-3 pt-2 text-xs text-mute">{unknown ? 'Activity figures will appear when usage is available.' : `Full glow is ${formatTokens(ref)} tokens per minute right now.`}</p>
    </div>
  </div>;
}
