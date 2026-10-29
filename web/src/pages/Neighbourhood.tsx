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
import { Link, useSearchParams } from 'react-router-dom';
import { useReducedMotion } from '../utils/preferences';
import { apiFetch, type Account, type UsageResp } from '../utils/api';
import { POLL_MS, WINDOWS, MIN_REF_TPM, buildBuildings, planCity, type Building } from '../utils/city';
import { useLiveCalls } from '../utils/live';
import { accountIssues, providerLink } from '../utils/health';
import { pct } from '../utils/lib';
import { Chip, EmptyState, Segmented, SectionLabel, btn, cx, formatTokens, inputCls } from '../components/ui';
import CityScene from '../components/CityScene';

const SEED_KEY = 'ezllm.city.seed';
const readSeed = (): number => {
  try {
    const raw = Number(localStorage.getItem(SEED_KEY));
    if (Number.isFinite(raw) && raw > 0) return raw;
  } catch { /* storage blocked: fall through to a fresh city */ }
  return 1 + Math.floor(Math.random() * 100_000);
};

// The view choices that should survive a visit: sky, pause and the look-back.
const PREF = { sky: 'ezllm.city.sky', paused: 'ezllm.city.paused', window: 'ezllm.city.window' } as const;
const readPref = (key: string): string | null => { try { return localStorage.getItem(key); } catch { return null; } };
const writePref = (key: string, value: string) => { try { localStorage.setItem(key, value); } catch { /* storage blocked: the choice still applies for this visit */ } };
const readWindow = (): number => { const n = Number(readPref(PREF.window)); return WINDOWS.some((w) => w.min === n) ? n : WINDOWS[0].min; };
const readSky = (): 'auto' | 'rain' | 'clear' => { const v = readPref(PREF.sky); return v === 'rain' || v === 'clear' ? v : 'auto'; };

function useNeighbourhood(win: number) {
  const [accounts, setAccounts] = useState<Account[] | null>(null);
  // The usage is kept with the look-back it was fetched for: switching the
  // window must not average the old window's tokens over the new one's minutes.
  const [usage, setUsage] = useState<{ resp: UsageResp; win: number } | null>(null);
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
      const from = new Date(Date.now() - win * 60_000).toISOString();
      const [a, u] = await Promise.allSettled([
        apiFetch<Account[]>('/admin/accounts', { method: 'GET', signal: controller.signal }),
        apiFetch<UsageResp>(`/admin/usage?from=${encodeURIComponent(from)}&group_by=key`, { method: 'GET', signal: controller.signal }),
      ]);
      inFlight = false;
      if (!alive) return;
      if (a.status === 'fulfilled') setAccounts(a.value);
      if (u.status === 'fulfilled') setUsage({ resp: u.value, win });
      const complete = a.status === 'fulfilled' && u.status === 'fulfilled';
      setStale(!complete); setLoading(false);
      if (complete) setAt(Date.now());
    };
    void load();
    const iv = setInterval(() => void load(), POLL_MS);
    const onVisible = () => { if (!document.hidden) void load(); };
    document.addEventListener('visibilitychange', onVisible);
    return () => { alive = false; controller.abort(); clearInterval(iv); document.removeEventListener('visibilitychange', onVisible); };
  }, [attempt, win]);
  const buildings = useMemo(() => (accounts ? buildBuildings(accounts, usage?.resp ?? null, usage?.win) : null), [accounts, usage]);
  return { buildings, accounts, usageWin: usage?.win ?? win, stale, loading, at, retry: () => setAttempt((n) => n + 1) };
}

function Ago({ at }: { at: number | null }) {
  const [, tick] = useState(0);
  useEffect(() => { const iv = setInterval(() => tick((n) => n + 1), 1000); return () => clearInterval(iv); }, []);
  if (at === null) return null;
  const s = Math.max(0, Math.round((Date.now() - at) / 1000));
  return <span className="tnum">updated {s < 2 ? 'just now' : `${s}s ago`}</span>;
}

type SortKey = 'name' | 'tpm' | 'calls' | 'errors' | 'rate';
type StateFilter = 'all' | 'serving' | 'quiet' | 'disabled' | 'stormy';

const rateOf = (b: Building) => (b.calls > 0 ? b.errors / b.calls : 0);
const stateWords = (b: Building) => (b.state === 'off' ? 'disabled' : b.state === 'unknown' ? 'usage unavailable' : b.storm ? 'stormy' : b.state === 'live' ? 'serving traffic' : 'quiet');

function SortTh({ label, k, sort, onSort, className }: { label: string; k: SortKey; sort: { key: SortKey; dir: 1 | -1 }; onSort: (k: SortKey) => void; className?: string }) {
  const on = sort.key === k;
  return (
    <th className={cx('py-2 font-medium', className)} aria-sort={on ? (sort.dir === 1 ? 'ascending' : 'descending') : 'none'}>
      <button type="button" className={cx('inline-flex items-center gap-1 hover:text-ink', on && 'text-ink')} onClick={() => onSort(k)}>
        {label}<span aria-hidden className="w-2 text-[10px]">{on ? (sort.dir === 1 ? '▲' : '▼') : ''}</span>
      </button>
    </th>
  );
}

export default function Neighbourhood() {
  const [win, setWin] = useState(readWindow);
  const { buildings, accounts, usageWin, stale, loading, at, retry } = useNeighbourhood(win);
  const { perMinute, connected } = useLiveCalls();
  const reduce = useReducedMotion();
  const [paused, setPaused] = useState(() => readPref(PREF.paused) === '1');
  const [sky, setSky] = useState<'auto' | 'rain' | 'clear'>(readSky);
  const [hidden, setHidden] = useState(document.hidden);
  // Two hover sources. Either lights the building and its row, but only the
  // scene's drives the inspector: the inspector sits above the table, so a table
  // hover that changed its height would shove the rows out from under the pointer.
  const [hotScene, setHotScene] = useState<string | null>(null);
  const [hotRow, setHotRow] = useState<string | null>(null);
  const hot = hotScene ?? hotRow;
  const [seed, setSeed] = useState(readSeed);
  const [params, setParams] = useSearchParams();
  // Local state answers the click at once; the URL follows it (and moves it back
  // when the URL changes from outside, e.g. the browser's back button).
  const [selected, setSel] = useState<string | null>(() => params.get('select'));
  const urlSelected = params.get('select');
  useEffect(() => { setSel(urlSelected); }, [urlSelected]);
  const [query, setQuery] = useState('');
  const [filter, setFilter] = useState<StateFilter>('all');
  const [sort, setSort] = useState<{ key: SortKey; dir: 1 | -1 }>({ key: 'tpm', dir: -1 });
  const stage = useRef<HTMLDivElement | null>(null);
  const inspector = useRef<HTMLDivElement | null>(null);
  const [full, setFull] = useState(false);

  useEffect(() => {
    const update = () => setHidden(document.hidden);
    document.addEventListener('visibilitychange', update);
    return () => document.removeEventListener('visibilitychange', update);
  }, []);
  useEffect(() => {
    const update = () => setFull(document.fullscreenElement === stage.current && !!stage.current);
    document.addEventListener('fullscreenchange', update);
    return () => document.removeEventListener('fullscreenchange', update);
  }, []);

  // The selected building lives in the URL, so a view can be linked to: a
  // select= that names no provider (deleted, mistyped) is dropped.
  const setSelected = (id: string | null) => {
    setSel(id);
    setParams((p) => {
      const n = new URLSearchParams(p);
      if (id) n.set('select', id); else n.delete('select');
      return n;
    }, { replace: true });
  };
  useEffect(() => { if (selected && buildings && !buildings.some((b) => b.id === selected)) setSelected(null); }, [buildings, selected]); // eslint-disable-line react-hooks/exhaustive-deps

  const ids = useMemo(() => (buildings ?? []).map((b) => b.id), [buildings]);
  const plan = useMemo(() => planCity(ids, seed), [ids.join('|'), seed]); // eslint-disable-line react-hooks/exhaustive-deps

  const wordsOf = WINDOWS.find((w) => w.min === usageWin)?.words ?? 'five minutes';
  const detail = buildings?.find((b) => b.id === (hotScene ?? selected));
  const detailAccount = detail ? accounts?.find((a) => String(a.id) === detail.id) : undefined;
  const live = buildings?.filter((b) => b.state === 'live').length ?? 0;
  const quiet = buildings?.filter((b) => b.state === 'idle').length ?? 0;
  const disabled = buildings?.filter((b) => b.state === 'off').length ?? 0;
  const stormy = buildings?.filter((b) => b.storm).length ?? 0;
  const total = buildings?.reduce((n, b) => n + b.tpm, 0) ?? 0;
  const totalCalls = buildings?.reduce((n, b) => n + b.calls, 0) ?? 0;
  const totalErrors = buildings?.reduce((n, b) => n + b.errors, 0) ?? 0;
  const unknown = buildings?.some((b) => b.state === 'unknown') ?? false;
  const still = reduce || paused || hidden;
  const select = (id: string) => setSelected(selected === id ? null : id);
  const ref = Math.max(MIN_REF_TPM, ...(buildings ?? []).map((b) => b.tpm));

  // What moved since the previous update: the table shows the direction, the
  // inspector names it. A provider seen for the first time has no trend, and a
  // change of window starts the comparison over (two windows are not comparable).
  const before = useRef<Map<string, number>>(new Map());
  const trend = (id: string, tpm: number) => {
    const was = before.current.get(`${usageWin}:${id}`);
    return was === undefined ? 0 : tpm - was;
  };
  useEffect(() => {
    if (!buildings) return;
    const next = new Map<string, number>();
    for (const b of buildings) next.set(`${usageWin}:${b.id}`, b.tpm);
    before.current = next;
  }, [buildings, usageWin]);
  const trends = useMemo(() => {
    const map = new Map<string, number>();
    for (const b of buildings ?? []) map.set(b.id, trend(b.id, b.tpm));
    return map;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [buildings]);

  const reroll = () => {
    const next = 1 + Math.floor(Math.random() * 100_000);
    setSeed(next);
    writePref(SEED_KEY, String(next));
  };
  const changeWindow = (min: number) => { setWin(min); writePref(PREF.window, String(min)); };
  const changeSky = (mode: 'auto' | 'rain' | 'clear') => { setSky(mode); writePref(PREF.sky, mode); };
  const togglePause = () => setPaused((p) => { writePref(PREF.paused, p ? '0' : '1'); return !p; });
  const toggleFull = () => {
    if (document.fullscreenElement) void document.exitFullscreen();
    else void stage.current?.requestFullscreen?.().catch(() => { /* refused (iframe, policy): the page just stays as it is */ });
  };
  const onSort = (key: SortKey) => setSort((cur) => (cur.key === key ? { key, dir: (cur.dir * -1) as 1 | -1 } : { key, dir: key === 'name' ? 1 : -1 }));
  // From a table row the inspector may be off screen, most of all on a phone:
  // bring it into view, and only as far as needed.
  const selectFromTable = (id: string) => {
    const opening = selected !== id;
    select(id);
    const el = inspector.current;
    if (opening && el) {
      const r = el.getBoundingClientRect();
      if (r.bottom < 0 || r.top > window.innerHeight) el.scrollIntoView({ block: 'nearest', behavior: reduce ? 'auto' : 'smooth' });
    }
  };

  const q = query.trim().toLowerCase();
  const matches = (b: Building) => (!q || b.name.toLowerCase().includes(q) || b.namespace.toLowerCase().includes(q))
    && (filter === 'all' || (filter === 'serving' ? b.state === 'live' : filter === 'quiet' ? b.state === 'idle' : filter === 'disabled' ? b.state === 'off' : b.storm));
  const ranked = useMemo(() => {
    const val = (b: Building) => (sort.key === 'name' ? b.name.toLowerCase() : sort.key === 'tpm' ? b.tpm : sort.key === 'calls' ? b.calls : sort.key === 'errors' ? b.errors : rateOf(b));
    return [...(buildings ?? [])].filter(matches).sort((a, b) => {
      const x = val(a), y = val(b);
      const c = typeof x === 'string' ? x.localeCompare(y as string) : (x as number) - (y as number);
      return c * sort.dir || b.tpm - a.tpm || a.name.localeCompare(b.name);
    });
  }, [buildings, sort, q, filter]); // eslint-disable-line react-hooks/exhaustive-deps

  if (buildings === null) return <div className="nbhd nbhd-loading flex min-h-[360px] flex-col items-center justify-center gap-4 rounded-2xl border border-line bg-surface px-6 text-center">
    <span className="text-3xl text-accent" aria-hidden>⌂</span>
    {loading ? <p role="status" className="text-sm text-dim">Laying out the streets…</p> : <><p role="alert" className="text-sm text-dim">Could not load the neighbourhood.</p><button className={cx(btn.base, btn.ghost, 'min-h-10')} onClick={retry}>Try again</button></>}
  </div>;
  if (!buildings.length) return <div className="space-y-4"><EmptyState title="No providers yet." hint="Add a provider under Providers and its building appears here." /><Link className={cx(btn.base, btn.primary, 'min-h-10')} to="/providers">Add a provider</Link></div>;

  const issues = detailAccount ? accountIssues(detailAccount) : [];
  const attentionN = stormy;

  return <div className="nbhd space-y-4">
    <div className="nbhd-toolbar flex flex-wrap items-center justify-between gap-4">
      <div className="flex flex-wrap items-center gap-x-5 gap-y-2 text-xs text-dim" aria-label="Neighbourhood summary">
        <span><strong className="tnum font-medium text-ink">{buildings.length}</strong> buildings</span>
        <span className="flex items-center gap-1.5"><span className="nbhd-status-light" /><strong className="tnum font-medium text-ink">{unknown ? '—' : live}</strong> serving</span>
        <span><strong className="tnum font-medium text-ink">{unknown ? '—' : quiet}</strong> quiet</span>
        <span><strong className="tnum font-medium text-ink">{disabled}</strong> disabled</span>
        {!unknown && stormy > 0 && <span className="text-bad"><strong className="tnum font-medium">{stormy}</strong> stormy</span>}
        <span className="font-mono text-ink">{unknown ? 'Usage unavailable' : `${formatTokens(total)} tokens / min`}</span>
        {!unknown && totalCalls > 0 && <span className={cx(totalErrors > 0 && totalErrors / totalCalls >= 0.1 && 'text-bad')}><strong className="tnum font-medium text-ink">{pct(totalErrors, totalCalls)}</strong> of calls failed</span>}
        <span className="flex items-center gap-1.5" title={connected ? 'Reading the live ledger feed' : 'Live feed not connected — figures still update every 10 s'}>
          <span className="nbhd-live-dot" data-live={connected ? 'true' : 'false'} aria-hidden />
          <span className="tnum font-mono text-ink">{formatTokens(perMinute)}</span><span>requests / min (live)</span>
          {!connected && <span className="text-warn">· live feed off</span>}
        </span>
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <Segmented<string> size="sm" label="Usage window" value={String(win)} onChange={(v) => changeWindow(Number(v))}
          options={WINDOWS.map((w) => ({ key: String(w.min), label: w.label, title: `Average the last ${w.words} of usage` }))} />
        <div className="flex items-center rounded-lg border border-line" role="group" aria-label="Weather over the city">
          {(['auto', 'rain', 'clear'] as const).map((mode) => (
            <button key={mode} type="button" data-wx={mode} aria-pressed={sky === mode}
              className="min-h-10 px-2.5 text-xs capitalize text-dim aria-pressed:bg-hover aria-pressed:text-ink"
              onClick={() => changeSky(mode)}
              title={mode === 'auto' ? 'Rain drifts in on its own' : mode === 'rain' ? 'Rain over the whole city' : 'Hold the sky clear'}>
              {mode === 'auto' ? 'Auto sky' : mode}
            </button>
          ))}
        </div>
        <button type="button" className={cx(btn.base, btn.ghost, 'min-h-10')} disabled={reduce} aria-pressed={paused || reduce}
          onClick={togglePause} title={reduce ? 'Reduced motion is enabled in your browser or Settings.' : 'Pause the traffic; usage updates continue.'}>
          <span aria-hidden>{still ? '▷' : 'Ⅱ'}</span>{reduce ? 'Reduced motion' : paused ? 'Resume traffic' : 'Pause traffic'}
        </button>
        {typeof document !== 'undefined' && document.fullscreenEnabled && (
          <button type="button" className={cx(btn.base, btn.ghost, 'min-h-10')} onClick={toggleFull} aria-pressed={full} title="Show the city full screen, for a wall display">
            {full ? 'Exit full screen' : 'Full screen'}
          </button>
        )}
      </div>
    </div>
    {stale && <div role="status" className="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-warn/30 bg-warn-dim px-4 py-3 text-xs text-warn">
      <span>{unknown ? 'Usage could not be loaded. Buildings are shown without activity figures.' : 'Last update failed — showing the latest available city.'}</span>
      <button className="min-h-10 px-2 font-medium underline underline-offset-4" onClick={retry}>Try again</button>
    </div>}
    <div className="nbhd-stage" ref={stage}>
      <CityScene plan={plan} buildings={buildings} hot={hot} selected={selected} still={still} rate={perMinute} sky={sky}
        onHover={setHotScene} onSelect={select} onReroll={reroll} />
      {full && (
        // Everything outside the stage is hidden in full screen, so the way out
        // has to live inside it (Esc works too).
        <button type="button" className="nbhd-exit-full" onClick={toggleFull}>Exit full screen</button>
      )}
    </div>
    <div ref={inspector} className="nbhd-inspector flex min-h-[132px] flex-wrap items-start justify-between gap-3 rounded-xl border border-line bg-surface px-4 py-3" aria-label="Selected building" aria-live="polite">
      {detail ? <>
        <div className="min-w-0">
          <p className="break-words text-sm font-medium text-ink">{detail.name}</p>
          <p className="mt-0.5 text-xs text-mute">{detail.namespace} · {detail.keys.length} {detail.keys.length === 1 ? 'key' : 'keys'} · {stateWords(detail)}</p>
          {detail.keys.length > 1 && <p className="mt-1 break-words text-xs text-mute">{detail.keys.map((k) => `${k.label}${k.enabled ? '' : ' (disabled)'} ${formatTokens(k.tpm)}/min`).join(' · ')}</p>}
          {detail.storm && <p className="mt-1 text-xs text-bad">A quarter or more of the calls in the last {wordsOf} failed.</p>}
          {issues.map((i) => (
            <p key={i.id} className={cx('mt-1 text-xs', i.tone === 'bad' ? 'text-bad' : 'text-warn')}>
              {i.text}{' '}<Link className="underline underline-offset-2 hover:text-ink" to={providerLink(Number(detail.id), i.fix)}>Fix</Link>
            </p>
          ))}
          <p className="mt-1.5 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs">
            <Link className="text-accent hover:underline" to={providerLink(Number(detail.id), 'open')}>Open provider</Link>
            <Link className="text-accent hover:underline" to={`/requests?${new URLSearchParams({ account: detail.name, range: '24h' })}`}>View requests</Link>
            <Link className="text-accent hover:underline" to={providerLink(Number(detail.id), 'keys')}>Edit keys</Link>
          </p>
        </div>
        <div className="flex flex-wrap gap-x-5 gap-y-1 font-mono text-xs text-dim">
          <span>{detail.state === 'unknown' ? '—' : `${formatTokens(detail.tpm)} tokens / min`}</span>
          <span>{detail.state === 'unknown' ? '—' : `${formatTokens(detail.calls)} calls`}</span>
          <span className={cx(detail.errors > 0 && 'text-bad')}>{detail.state === 'unknown' ? '—' : `${formatTokens(detail.errors)} errors${detail.calls > 0 ? ` · ${pct(detail.errors, detail.calls)}` : ''}`}</span>
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
      <ul className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-mute">
        <li><Ago at={at} /></li><li>· last {wordsOf}</li>
        <li><button type="button" className="text-dim underline underline-offset-4 hover:text-ink" onClick={retry}>Refresh now</button></li>
      </ul>
      <details className="text-xs text-dim">
        <summary className="cursor-pointer select-none hover:text-ink">How to read the city</summary>
        <ul className="mt-2 flex flex-wrap items-center gap-x-5 gap-y-2">
          <li className="flex items-center gap-1.5"><span className="nbhd-key nbhd-key-lit" aria-hidden />Brighter = more tokens per minute</li>
          <li className="flex items-center gap-1.5"><span className="nbhd-key nbhd-key-flare" aria-hidden />A flare is one live request</li>
          <li className="flex items-center gap-1.5"><span className="nbhd-key nbhd-key-rain" aria-hidden />Rain = a quarter or more of calls failed</li>
          <li className="flex items-center gap-1.5"><span className="nbhd-key nbhd-key-off" aria-hidden />Boarded up = disabled</li>
        </ul>
        <p className="mt-2 text-mute">Window light follows the last {wordsOf} of usage, averaged per minute. Full glow is never less than {formatTokens(MIN_REF_TPM)} tokens a minute.</p>
      </details>
    </div>
    <div className="rounded-xl border border-line bg-surface">
      <div className="flex flex-wrap items-center justify-between gap-3 px-4 pt-3">
        <SectionLabel>Every building</SectionLabel>
        <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
          <input aria-label="Search buildings" type="search" placeholder="Search providers" value={query} onChange={(e) => setQuery(e.target.value)} className={cx(inputCls, '!w-44 py-1')} />
          <Segmented<StateFilter> size="sm" label="Show buildings" value={filter} onChange={setFilter}
            options={[
              { key: 'all', label: 'All' }, { key: 'serving', label: 'Serving' }, { key: 'quiet', label: 'Quiet' }, { key: 'disabled', label: 'Disabled' },
              { key: 'stormy', label: attentionN > 0 ? `Stormy (${attentionN})` : 'Stormy' },
            ]} />
        </div>
      </div>
      {/* relative: the sr-only labels inside are absolutely positioned and would
          otherwise escape this scroller and widen the whole page. */}
      <div className="relative overflow-x-auto">
        <table className="w-full min-w-[760px] text-[13px]">
          <thead><tr className="text-left text-[11px] text-mute">
            <SortTh label="Provider" k="name" sort={sort} onSort={onSort} className="px-4" /><th className="px-3 py-2 font-medium">Keys</th><th className="px-3 py-2 font-medium">Glow</th>
            <SortTh label="Tokens / min" k="tpm" sort={sort} onSort={onSort} className="px-3 text-right" /><SortTh label="Calls" k="calls" sort={sort} onSort={onSort} className="px-3 text-right" />
            <SortTh label="Errors" k="errors" sort={sort} onSort={onSort} className="px-3 text-right" /><SortTh label="Error rate" k="rate" sort={sort} onSort={onSort} className="px-3 text-right" />
            <th className="px-4 py-2 font-medium">State</th><th className="px-3 py-2 font-medium"><span className="sr-only">Links</span></th>
          </tr></thead>
          <tbody>{ranked.map((b) => <tr key={b.id} data-row={b.id} className={cx('border-t border-line transition-colors', (hot === b.id || selected === b.id) && 'bg-hover')}
            onMouseEnter={() => setHotRow(b.id)} onMouseLeave={() => setHotRow(null)}>
            <td className="px-4 py-2"><button className="min-h-10 text-left underline-offset-4 hover:underline focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent" onClick={() => selectFromTable(b.id)} aria-pressed={selected === b.id} onFocus={() => setHotRow(b.id)} onBlur={() => setHotRow(null)}>{b.name}</button></td>
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
            <td className={cx('tnum px-3 py-2 text-right font-mono', b.storm && 'text-bad')}>{b.state === 'unknown' ? '—' : pct(b.errors, b.calls)}</td>
            <td className="px-4 py-2"><Chip tone={b.state === 'unknown' ? 'warn' : b.state === 'off' ? 'neutral' : b.storm ? 'bad' : b.state === 'live' ? 'ok' : 'neutral'}>{b.state === 'unknown' ? 'unavailable' : b.state === 'off' ? 'disabled' : b.storm ? 'stormy' : b.state === 'live' ? 'lit' : 'quiet'}</Chip></td>
            <td className="whitespace-nowrap px-3 py-2 text-[12px]">
              <Link className="text-mute hover:text-ink" to={providerLink(Number(b.id), 'open')} aria-label={`Open ${b.name}`}>Provider</Link>
              <span className="mx-1.5 text-mute/50" aria-hidden>·</span>
              <Link className="text-mute hover:text-ink" to={`/requests?${new URLSearchParams({ account: b.name, range: '24h' })}`} aria-label={`Requests for ${b.name}`}>Requests</Link>
            </td>
          </tr>)}
          {ranked.length === 0 && <tr><td colSpan={9} className="px-4 py-6 text-center text-mute">No building matches. <button type="button" className="underline" onClick={() => { setQuery(''); setFilter('all'); }}>Clear search and filter</button></td></tr>}</tbody>
        </table>
      </div>
      <p className="px-4 pb-3 pt-2 text-xs text-mute">{unknown ? 'Activity figures will appear when usage is available.' : `Full glow is ${formatTokens(ref)} tokens per minute right now.`}</p>
    </div>
  </div>;
}
