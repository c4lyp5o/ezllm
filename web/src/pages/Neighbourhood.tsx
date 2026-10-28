// NEIGHBOURHOOD — every provider is a building in a small city.
//
// The scene is the mood, the table is the record. What the city encodes:
//   height, window light   tokens per minute over the last five minutes
//   rain                   a quarter or more of recent calls failed
//   boarded / dark         the account or all of its keys are disabled
//   a flare + a car        one live request, replayed from the SSE ledger feed
//   traffic density        how busy the last minute was
// Roof plant, antennas, trees, parks, lamps and people are scenery, not
// metrics. Missing usage is `unknown` — never a quiet zero.
import { useEffect, useMemo, useRef, useState } from 'react';
import { Link } from 'react-router-dom';
import { useReducedMotion } from '../utils/preferences';
import { apiFetch, type Account, type UsageResp } from '../utils/api';
import { POLL_MS, WINDOW_MIN, MIN_REF_TPM, buildBuildings, planCity } from '../utils/city';
import { useLiveCalls } from '../utils/live';
import { Chip, EmptyState, SectionLabel, btn, cx, formatTokens } from '../components/ui';
import CityScene from '../components/CityScene';

const SEED_KEY = 'ezllm.city.seed';
const readSeed = (): number => {
  try {
    const raw = Number(localStorage.getItem(SEED_KEY));
    if (Number.isFinite(raw) && raw > 0) return raw;
  } catch { /* storage blocked: fall through to a fresh city */ }
  return 1 + Math.floor(Math.random() * 100_000);
};

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
  const buildings = useMemo(() => (accounts ? buildBuildings(accounts, usage) : null), [accounts, usage]);
  return { buildings, stale, loading, at, retry: () => setAttempt((n) => n + 1) };
}

function Ago({ at }: { at: number | null }) {
  const [, tick] = useState(0);
  useEffect(() => { const iv = setInterval(() => tick((n) => n + 1), 1000); return () => clearInterval(iv); }, []);
  if (at === null) return null;
  const s = Math.max(0, Math.round((Date.now() - at) / 1000));
  return <span className="tnum">updated {s < 2 ? 'just now' : `${s}s ago`}</span>;
}

export default function Neighbourhood() {
  const { buildings, stale, loading, at, retry } = useNeighbourhood();
  const { perMinute, connected } = useLiveCalls();
  const reduce = useReducedMotion();
  const [paused, setPaused] = useState(false);
  const [sky, setSky] = useState<'auto' | 'rain' | 'clear'>('auto');
  const [hidden, setHidden] = useState(document.hidden);
  const [hot, setHot] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [seed, setSeed] = useState(readSeed);

  useEffect(() => {
    const update = () => setHidden(document.hidden);
    document.addEventListener('visibilitychange', update);
    return () => document.removeEventListener('visibilitychange', update);
  }, []);
  useEffect(() => { if (selected && buildings && !buildings.some((b) => b.id === selected)) setSelected(null); }, [buildings, selected]);

  const ids = useMemo(() => (buildings ?? []).map((b) => b.id), [buildings]);
  const plan = useMemo(() => planCity(ids, seed), [ids.join('|'), seed]); // eslint-disable-line react-hooks/exhaustive-deps

  const ranked = useMemo(() => [...(buildings ?? [])].sort((a, b) => b.tpm - a.tpm || a.name.localeCompare(b.name)), [buildings]);
  const detail = buildings?.find((b) => b.id === (hot ?? selected));
  const live = buildings?.filter((b) => b.state === 'live').length ?? 0;
  const quiet = buildings?.filter((b) => b.state === 'idle').length ?? 0;
  const disabled = buildings?.filter((b) => b.state === 'off').length ?? 0;
  const total = buildings?.reduce((n, b) => n + b.tpm, 0) ?? 0;
  const unknown = buildings?.some((b) => b.state === 'unknown') ?? false;
  const still = reduce || paused || hidden;
  const select = (id: string) => setSelected((previous) => (previous === id ? null : id));
  const ref = Math.max(MIN_REF_TPM, ...(buildings ?? []).map((b) => b.tpm));

  // What moved since the previous update: the table shows the direction, the
  // inspector names it. A provider seen for the first time has no trend.
  const before = useRef<Map<string, number>>(new Map());
  const trend = (id: string, tpm: number) => {
    const was = before.current.get(id);
    return was === undefined ? 0 : tpm - was;
  };
  useEffect(() => {
    if (!buildings) return;
    const next = new Map<string, number>();
    for (const b of buildings) next.set(b.id, b.tpm);
    before.current = next;
  }, [buildings]);
  const trends = useMemo(() => {
    const map = new Map<string, number>();
    for (const b of buildings ?? []) map.set(b.id, trend(b.id, b.tpm));
    return map;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [buildings]);

  const reroll = () => {
    const next = 1 + Math.floor(Math.random() * 100_000);
    setSeed(next);
    try { localStorage.setItem(SEED_KEY, String(next)); } catch { /* storage blocked: the seed still applies for this visit */ }
  };

  if (buildings === null) return <div className="nbhd nbhd-loading flex min-h-[360px] flex-col items-center justify-center gap-4 rounded-2xl border border-line bg-surface px-6 text-center">
    <span className="text-3xl text-accent" aria-hidden>⌂</span>
    {loading ? <p role="status" className="text-sm text-dim">Laying out the streets…</p> : <><p role="alert" className="text-sm text-dim">Could not load the neighbourhood.</p><button className={cx(btn.base, btn.ghost, 'min-h-10')} onClick={retry}>Try again</button></>}
  </div>;
  if (!buildings.length) return <div className="space-y-4"><EmptyState title="No providers yet." hint="Add a provider under Providers and its building appears here." /><Link className={cx(btn.base, btn.primary, 'min-h-10')} to="/providers">Add a provider</Link></div>;

  return <div className="nbhd space-y-4">
    <div className="nbhd-toolbar flex flex-wrap items-center justify-between gap-4">
      <div className="flex flex-wrap items-center gap-x-5 gap-y-2 text-xs text-dim" aria-label="Neighbourhood summary">
        <span><strong className="tnum font-medium text-ink">{buildings.length}</strong> buildings</span>
        <span className="flex items-center gap-1.5"><span className="nbhd-status-light" /><strong className="tnum font-medium text-ink">{unknown ? '—' : live}</strong> serving</span>
        <span><strong className="tnum font-medium text-ink">{unknown ? '—' : quiet}</strong> quiet</span>
        <span><strong className="tnum font-medium text-ink">{disabled}</strong> disabled</span>
        <span className="font-mono text-ink">{unknown ? 'Usage unavailable' : `${formatTokens(total)} tokens / min`}</span>
        <span className="flex items-center gap-1.5" title={connected ? 'Reading the live ledger feed' : 'Live feed not connected — figures still update every 10 s'}>
          <span className="nbhd-live-dot" data-live={connected ? 'true' : 'false'} aria-hidden />
          <span className="tnum font-mono text-ink">{formatTokens(perMinute)}</span><span>requests / min (live)</span>
        </span>
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <div className="flex items-center rounded-lg border border-line" role="group" aria-label="Weather over the city">
          {(['auto', 'rain', 'clear'] as const).map((mode) => (
            <button key={mode} type="button" data-wx={mode} aria-pressed={sky === mode}
              className="min-h-10 px-2.5 text-xs capitalize text-dim aria-pressed:bg-hover aria-pressed:text-ink"
              onClick={() => setSky(mode)}
              title={mode === 'auto' ? 'Rain drifts in on its own' : mode === 'rain' ? 'Rain over the whole city' : 'Hold the sky clear'}>
              {mode === 'auto' ? 'Auto sky' : mode}
            </button>
          ))}
        </div>
        <button type="button" className={cx(btn.base, btn.ghost, 'min-h-10')} disabled={reduce} aria-pressed={paused || reduce}
          onClick={() => setPaused((p) => !p)} title={reduce ? 'Reduced motion is enabled in your browser or Settings.' : 'Pause the traffic; usage updates continue.'}>
          <span aria-hidden>{still ? '▷' : 'Ⅱ'}</span>{reduce ? 'Reduced motion' : paused ? 'Resume traffic' : 'Pause traffic'}
        </button>
      </div>
    </div>
    {stale && <div role="status" className="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-warn/30 bg-warn-dim px-4 py-3 text-xs text-warn">
      <span>{unknown ? 'Usage could not be loaded. Buildings are shown without activity figures.' : 'Last update failed — showing the latest available city.'}</span>
      <button className="min-h-10 px-2 font-medium underline underline-offset-4" onClick={retry}>Try again</button>
    </div>}
    <CityScene plan={plan} buildings={buildings} hot={hot} selected={selected} still={still} rate={perMinute} sky={sky}
      onHover={setHot} onSelect={select} onReroll={reroll} />
    <div className="nbhd-inspector flex min-h-[76px] flex-wrap items-center justify-between gap-3 rounded-xl border border-line bg-surface px-4 py-3" aria-label="Selected building" aria-live="polite">
      {detail ? <>
        <div className="min-w-0">
          <p className="break-words text-sm font-medium text-ink">{detail.name}</p>
          <p className="mt-0.5 text-xs text-mute">{detail.namespace} · {detail.keys.length} {detail.keys.length === 1 ? 'key' : 'keys'} · {detail.state === 'off' ? 'disabled' : detail.state === 'unknown' ? 'usage unavailable' : detail.storm ? 'stormy' : detail.state === 'live' ? 'serving traffic' : 'quiet'}</p>
          {detail.keys.length > 1 && <p className="mt-1 break-words text-xs text-mute">{detail.keys.map((k) => `${k.label}${k.enabled ? '' : ' (disabled)'} ${formatTokens(k.tpm)}/min`).join(' · ')}</p>}
        </div>
        <div className="flex flex-wrap gap-x-5 gap-y-1 font-mono text-xs text-dim">
          <span>{detail.state === 'unknown' ? '—' : `${formatTokens(detail.tpm)} tokens / min`}</span>
          <span>{detail.state === 'unknown' ? '—' : `${formatTokens(detail.calls)} calls`}</span>
          <span className={cx(detail.errors > 0 && 'text-bad')}>{detail.state === 'unknown' ? '—' : `${formatTokens(detail.errors)} errors`}</span>
          {detail.state !== 'unknown' && (() => {
            const d = trends.get(detail.id) ?? 0;
            return Math.abs(d) < 0.5
              ? <span className="text-mute">steady since the last update</span>
              : <span className={d > 0 ? 'text-ok' : 'text-warn'}><span aria-hidden>{d > 0 ? '▲' : '▼'}</span> {formatTokens(Math.abs(Math.round(d)))} {d > 0 ? 'up' : 'down'} since the last update</span>;
          })()}
        </div>
      </> : <><p className="text-sm text-dim">Every provider has a building here.</p><p className="text-xs text-mute">Hover, focus or select a building to look inside.</p></>}
    </div>
    <div className="space-y-2">
      <ul className="flex flex-wrap items-center gap-x-5 gap-y-2 text-xs text-dim">
        <li className="flex items-center gap-1.5"><span className="nbhd-key nbhd-key-lit" aria-hidden />Brighter = more tokens per minute</li>
        <li className="flex items-center gap-1.5"><span className="nbhd-key nbhd-key-flare" aria-hidden />A flare is one live request</li>
        <li className="flex items-center gap-1.5"><span className="nbhd-key nbhd-key-rain" aria-hidden />Rain = a quarter or more of calls failed</li>
        <li className="flex items-center gap-1.5"><span className="nbhd-key nbhd-key-off" aria-hidden />Boarded up = disabled</li>
        <li className="ml-auto text-mute"><Ago at={at} /> · last {WINDOW_MIN} minutes</li>
      </ul>
      <p className="text-xs text-mute">Window light follows the last five minutes of usage. Full glow is never less than {formatTokens(MIN_REF_TPM)} tokens a minute.</p>
    </div>
    <div className="rounded-xl border border-line bg-surface">
      <div className="px-4 pt-3"><SectionLabel>Every building, busiest first</SectionLabel></div>
      <div className="overflow-x-auto">
        <table className="w-full min-w-[640px] text-[13px]">
          <thead><tr className="text-left text-[11px] text-mute">
            <th className="px-4 py-2 font-medium">Provider</th><th className="px-3 py-2 font-medium">Keys</th><th className="px-3 py-2 font-medium">Glow</th>
            <th className="px-3 py-2 text-right font-medium">Tokens / min</th><th className="px-3 py-2 text-right font-medium">Calls</th><th className="px-3 py-2 text-right font-medium">Errors</th><th className="px-4 py-2 font-medium">State</th>
          </tr></thead>
          <tbody>{ranked.map((b) => <tr key={b.id} data-row={b.id} className={cx('border-t border-line transition-colors', (hot === b.id || selected === b.id) && 'bg-hover')}
            onMouseEnter={() => setHot(b.id)} onMouseLeave={() => setHot(null)}>
            <td className="px-4 py-2"><button className="min-h-10 text-left underline-offset-4 hover:underline focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent" onClick={() => select(b.id)} aria-pressed={selected === b.id} onFocus={() => setHot(b.id)} onBlur={() => setHot(null)}>{b.name}</button></td>
            <td className="tnum px-3 py-2 text-dim">{b.keys.length}</td>
            <td className="px-3 py-2"><div className="h-1.5 w-20 overflow-hidden rounded-full bg-line" title={b.state === 'unknown' ? 'Usage unavailable' : `${Math.round(b.level * 100)}% of full glow`}><div className="h-full rounded-full" style={{ width: `${Math.round(b.level * 100)}%`, background: 'var(--nb-window-on)' }} /></div></td>
            <td className="tnum px-3 py-2 text-right font-mono">{b.state === 'unknown' ? '—' : <>
              {(() => {
                const d = trends.get(b.id) ?? 0;
                return Math.abs(d) < 0.5
                  ? <span className="text-mute" aria-hidden>·</span>
                  : <span className={d > 0 ? 'text-ok' : 'text-warn'} title={`${d > 0 ? '+' : '−'}${formatTokens(Math.abs(Math.round(d)))} tokens/min since the last update`}>
                    <span aria-hidden>{d > 0 ? '▲' : '▼'}</span>
                    <span className="sr-only">{d > 0 ? 'up' : 'down'} since the last update</span>
                  </span>;
              })()}{' '}{formatTokens(b.tpm)}</>}</td>
            <td className="tnum px-3 py-2 text-right font-mono">{b.state === 'unknown' ? '—' : formatTokens(b.calls)}</td>
            <td className={cx('tnum px-3 py-2 text-right font-mono', b.errors > 0 && 'text-bad')}>{b.state === 'unknown' ? '—' : formatTokens(b.errors)}</td>
            <td className="px-4 py-2"><Chip tone={b.state === 'unknown' ? 'warn' : b.state === 'off' ? 'neutral' : b.storm ? 'bad' : b.state === 'live' ? 'ok' : 'neutral'}>{b.state === 'unknown' ? 'unavailable' : b.state === 'off' ? 'disabled' : b.storm ? 'stormy' : b.state === 'live' ? 'lit' : 'quiet'}</Chip></td>
          </tr>)}</tbody>
        </table>
      </div>
      <p className="px-4 pb-3 pt-2 text-xs text-mute">{unknown ? 'Activity figures will appear when usage is available.' : `Full glow is ${formatTokens(ref)} tokens per minute right now.`}</p>
    </div>
  </div>;
}
