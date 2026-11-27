// NEIGHBOURHOOD — every provider is a building in a small city.
// Design: docs/design/neighbourhood-design.md.
//
// One encoding per question: height is tokens per minute on a labelled linear
// scale, red windows are failed calls, a red sign with a beacon and rain is a
// provider failing now, a short grey building is switched off, and each live
// request is a car. Failing providers stand in the front row. Status mode is
// calm and readable; Show mode adds atmosphere over the same numbers.
// Missing usage is `unknown`, never a quiet zero.
import { ExtraIntro, useIntroHidden } from '../components/ExtraIntro';
import { useEffect, useMemo, useRef, useState } from 'react';
import { Link, useSearchParams } from 'react-router-dom';
import { useReducedMotion } from '../utils/preferences';
import { apiFetch, type Account, type UsageResp } from '../utils/api';
import { POLL_MS, WINDOWS, buildBuildings, scaleCeiling, share, type Building, type Health } from '../utils/city';
import { useLiveCalls } from '../utils/live';
import { accountIssues, providerLink } from '../utils/health';
import { pct } from '../utils/lib';
import { Chip, EmptyState, Segmented, SectionLabel, btn, cx, formatTokens, inputCls } from '../components/ui';
import CityScene, { CityLoading, type CityMode } from '../components/CityScene';

// The view choices that survive a visit: mode, scale, pause and the look-back.
const PREF = { mode: 'ezllm.city.mode', scale: 'ezllm.city.scale', paused: 'ezllm.city.paused', window: 'ezllm.city.window' } as const;
const readPref = (key: string): string | null => { try { return localStorage.getItem(key); } catch { return null; } };
const writePref = (key: string, value: string) => { try { localStorage.setItem(key, value); } catch { /* storage blocked: the choice still applies for this visit */ } };
const readWindow = (): number => { const n = Number(readPref(PREF.window)); return WINDOWS.some((w) => w.min === n) ? n : WINDOWS[0].min; };
const readMode = (): CityMode => (readPref(PREF.mode) === 'show' ? 'show' : 'status');
/** null = scale to the busiest provider; a number = the operator's fixed ceiling. */
const readScale = (): number | null => { const n = Number(readPref(PREF.scale)); return Number.isFinite(n) && n > 0 ? n : null; };
const FIXED_DEFAULT = 100_000;

function useNeighbourhood(win: number) {
  const [accounts, setAccounts] = useState<Account[] | null>(null);
  // The usage is kept with the look-back it was fetched for: switching the
  // window must not average the old window's tokens over the new one's minutes.
  const [usage, setUsage] = useState<{ resp: UsageResp; keys: UsageResp | null; win: number } | null>(null);
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
      const [a, u, k] = await Promise.allSettled([
        apiFetch<Account[]>('/admin/accounts', { method: 'GET', signal: controller.signal }),
        apiFetch<UsageResp>(`/admin/usage?from=${encodeURIComponent(from)}&group_by=account`, { method: 'GET', signal: controller.signal }),
        apiFetch<UsageResp>(`/admin/usage?from=${encodeURIComponent(from)}&group_by=key`, { method: 'GET', signal: controller.signal }),
      ]);
      inFlight = false;
      if (!alive) return;
      if (a.status === 'fulfilled') setAccounts(a.value);
      if (u.status === 'fulfilled') setUsage({ resp: u.value, keys: k.status === 'fulfilled' ? k.value : null, win });
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
  const buildings = useMemo(() => (accounts ? buildBuildings(accounts, usage?.resp ?? null, usage?.win, usage?.keys ?? null) : null), [accounts, usage]);
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
type StateFilter = 'all' | Health;

const rateOf = (b: Building) => (b.calls > 0 ? b.errors / b.calls : 0);
const HEALTH_WORDS: Record<Health, string> = { failing: 'failing', serving: 'serving', quiet: 'quiet', off: 'switched off', unknown: 'usage unavailable' };
const HEALTH_TONE: Record<Health, 'bad' | 'ok' | 'neutral' | 'warn'> = { failing: 'bad', serving: 'ok', quiet: 'neutral', off: 'neutral', unknown: 'warn' };

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

function Trend({ d }: { d: number }) {
  if (Math.abs(d) < 0.5) return <span className="text-mute" aria-hidden>·</span>;
  return <span className={d > 0 ? 'text-ok' : 'text-warn'} title={`${d > 0 ? '+' : '−'}${formatTokens(Math.abs(Math.round(d)))} tokens/min since the last update`}>
    <span aria-hidden>{d > 0 ? '▲' : '▼'}</span><span className="sr-only">{d > 0 ? 'up' : 'down'} since the last update</span>
  </span>;
}

export default function Neighbourhood() {
  const [win, setWin] = useState(readWindow);
  const { buildings, accounts, usageWin, stale, loading, at, retry } = useNeighbourhood(win);
  const { perMinute, connected } = useLiveCalls();
  const [introHidden, hideIntro] = useIntroHidden('neighbourhood');
  const reduce = useReducedMotion();
  const [mode, setMode] = useState<CityMode>(readMode);
  const [fixed, setFixed] = useState<number | null>(readScale);
  const [fixedDraft, setFixedDraft] = useState(() => String(readScale() ?? FIXED_DEFAULT));
  const [paused, setPaused] = useState(() => readPref(PREF.paused) === '1');
  const [hidden, setHidden] = useState(document.hidden);
  const [hotScene, setHotScene] = useState<string | null>(null);
  const [hotRow, setHotRow] = useState<string | null>(null);
  const hot = hotScene ?? hotRow;
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
  const details = useRef<HTMLDivElement | null>(null);
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

  const wordsOf = WINDOWS.find((w) => w.min === usageWin)?.words ?? 'five minutes';
  const detail = buildings?.find((b) => b.id === selected);
  const detailAccount = detail ? accounts?.find((a) => String(a.id) === detail.id) : undefined;
  const count = (h: Health) => buildings?.filter((b) => b.health === h).length ?? 0;
  const failing = (buildings ?? []).filter((b) => b.health === 'failing');
  const total = buildings?.reduce((n, b) => n + b.tpm, 0) ?? 0;
  const totalCalls = buildings?.reduce((n, b) => n + b.calls, 0) ?? 0;
  const totalErrors = buildings?.reduce((n, b) => n + b.errors, 0) ?? 0;
  const unknown = buildings?.some((b) => b.health === 'unknown') ?? false;
  const still = reduce || paused || hidden;
  const select = (id: string) => setSelected(selected === id ? null : id);
  const ceiling = useMemo(() => scaleCeiling(buildings ?? [], fixed), [buildings, fixed]);

  // What moved since the previous update. A provider seen for the first time has
  // no trend, and a change of window starts the comparison over.
  const before = useRef<Map<string, number>>(new Map());
  const trends = useMemo(() => {
    const map = new Map<string, number>();
    for (const b of buildings ?? []) {
      const was = before.current.get(`${usageWin}:${b.id}`);
      map.set(b.id, was === undefined ? 0 : b.tpm - was);
    }
    return map;
  }, [buildings]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => {
    if (!buildings) return;
    const next = new Map<string, number>();
    for (const b of buildings) next.set(`${usageWin}:${b.id}`, b.tpm);
    before.current = next;
  }, [buildings, usageWin]);

  const changeWindow = (min: number) => { setWin(min); writePref(PREF.window, String(min)); };
  const changeMode = (m: CityMode) => { setMode(m); writePref(PREF.mode, m); };
  const useBusiest = () => { setFixed(null); writePref(PREF.scale, ''); };
  const applyFixed = () => {
    const n = Math.round(Number(fixedDraft));
    if (!Number.isFinite(n) || n < 1) { setFixedDraft(String(fixed ?? FIXED_DEFAULT)); return; }
    setFixed(n); setFixedDraft(String(n)); writePref(PREF.scale, String(n));
  };
  const togglePause = () => setPaused((p) => { writePref(PREF.paused, p ? '0' : '1'); return !p; });
  const toggleFull = () => {
    if (document.fullscreenElement) void document.exitFullscreen();
    else void stage.current?.requestFullscreen?.().catch(() => { /* refused (iframe, policy): the page just stays as it is */ });
  };
  const onSort = (key: SortKey) => setSort((cur) => (cur.key === key ? { key, dir: (cur.dir * -1) as 1 | -1 } : { key, dir: key === 'name' ? 1 : -1 }));
  // From the table the details card may be off screen, most of all on a phone:
  // bring it into view, and only as far as needed.
  const selectFromList = (id: string) => {
    const opening = selected !== id;
    select(id);
    requestAnimationFrame(() => {
      const el = details.current;
      if (!opening || !el) return;
      const r = el.getBoundingClientRect();
      if (r.bottom < 0 || r.top > window.innerHeight) el.scrollIntoView({ block: 'nearest', behavior: reduce ? 'auto' : 'smooth' });
    });
  };

  const q = query.trim().toLowerCase();
  const ranked = useMemo(() => {
    const matches = (b: Building) => (!q || b.name.toLowerCase().includes(q) || b.namespace.toLowerCase().includes(q)) && (filter === 'all' || b.health === filter);
    const val = (b: Building) => (sort.key === 'name' ? b.name.toLowerCase() : sort.key === 'tpm' ? b.tpm : sort.key === 'calls' ? b.calls : sort.key === 'errors' ? b.errors : rateOf(b));
    return [...(buildings ?? [])].filter(matches).sort((a, b) => {
      const x = val(a), y = val(b);
      const c = typeof x === 'string' ? x.localeCompare(y as string) : (x as number) - (y as number);
      return c * sort.dir || b.tpm - a.tpm || a.name.localeCompare(b.name);
    });
  }, [buildings, sort, q, filter]);

  if (buildings === null) return <div className="flex min-h-[360px] flex-col items-center justify-center gap-4 rounded-2xl border border-line bg-surface px-6 text-center">
    {loading ? <CityLoading label="Laying out the streets…" /> : <><p role="alert" className="text-sm text-dim">Could not load the neighbourhood.</p><button className={cx(btn.base, btn.ghost, 'min-h-10')} onClick={retry}>Try again</button></>}
  </div>;
  if (!buildings.length) return <div className="space-y-4"><EmptyState title="No providers yet." hint="Add a provider to see its building here." /><Link className={cx(btn.base, btn.primary, 'min-h-10')} to="/providers">Add a provider</Link></div>;

  const issues = detailAccount ? accountIssues(detailAccount) : [];
  const verdict = unknown ? 'Usage didn\'t load' : failing.length === 0 ? 'Everything is working'
    : `${failing.map((b) => b.name).join(', ')} ${failing.length === 1 ? 'is' : 'are'} failing`;
  const shown = (b: Building, s: string) => (b.health === 'unknown' ? '—' : s);

  return <div className="nbhd space-y-4">
    {!introHidden && (
      <ExtraIntro title="Neighbourhood" onClose={hideIntro}>
        <p>Each provider is a building. Taller means more tokens per minute, red windows are failed calls, and a provider that's failing stands in the front row with a red sign. Status mode is for leaving open on a screen; Show mode adds the night sky.</p>
      </ExtraIntro>
    )}

    <div className="flex flex-wrap items-center gap-x-6 gap-y-2 rounded-xl border border-line bg-surface px-4 py-3 text-[13px]" aria-label="Neighbourhood summary">
      <span className="flex items-center gap-2 font-semibold text-ink" data-verdict={failing.length ? 'failing' : unknown ? 'unknown' : 'ok'}>
        <span className="nbhd-verdict-dot" data-bad={failing.length > 0 || unknown ? 'true' : 'false'} aria-hidden />{verdict}
      </span>
      {!unknown && <span className="text-dim"><strong className="tnum font-semibold text-ink">{formatTokens(Math.round(total))}</strong> tokens / min</span>}
      {!unknown && <span className="text-dim"><strong className="tnum font-semibold text-ink">{formatTokens(totalCalls)}</strong> calls{totalCalls > 0 && <> · <strong className={cx('tnum font-semibold', totalErrors > 0 ? 'text-bad' : 'text-ink')}>{pct(totalErrors, totalCalls)}</strong> failed</>}</span>}
      <span className="text-dim">{count('serving')} serving · {count('quiet')} quiet · {count('off')} switched off</span>
      <span className="flex items-center gap-1.5 text-dim" title={connected ? 'Reading the live ledger feed' : 'Live updates aren\'t connected. Numbers still refresh every 10 seconds.'}>
        <span className="nbhd-live-dot" data-live={connected ? 'true' : 'false'} aria-hidden />
        {connected ? <><strong className="tnum font-semibold text-ink">{formatTokens(perMinute)}</strong> requests / min live</> : 'live cars off'}
      </span>
      <span className="text-mute"><Ago at={at} /> · last {wordsOf}</span>
    </div>

    <div className="flex flex-wrap items-center gap-2">
      <Segmented<CityMode> size="sm" label="City mode" value={mode} onChange={changeMode}
        options={[{ key: 'status', label: 'Status', title: 'Plain and readable, for a screen left open' }, { key: 'show', label: 'Show', title: 'Night sky, lamps and more traffic, over the same numbers' }]} />
      <Segmented<string> size="sm" label="Usage window" value={String(win)} onChange={(v) => changeWindow(Number(v))}
        options={WINDOWS.map((w) => ({ key: String(w.min), label: w.label, title: `Average the last ${w.words} of usage` }))} />
      <div className="flex items-center gap-2 rounded-lg border border-line px-2 py-1 text-xs text-dim" role="group" aria-label="Height scale">
        <span>Full height</span>
        <button type="button" aria-pressed={fixed === null} onClick={useBusiest}
          className="min-h-8 rounded-md px-2 aria-pressed:bg-hover aria-pressed:text-ink" title="The busiest provider sets full height, rounded up">busiest</button>
        <form className="flex items-center gap-1" onSubmit={(e) => { e.preventDefault(); applyFixed(); }}>
          <input aria-label="Fixed full height, tokens per minute" inputMode="numeric" value={fixedDraft} onChange={(e) => setFixedDraft(e.target.value.replace(/[^0-9]/g, ''))}
            onBlur={() => { if (fixed !== null) applyFixed(); }}
            className={cx(inputCls, '!w-24 !py-1 tnum', fixed !== null && 'border-accent')} />
          <button type="submit" aria-pressed={fixed !== null} className="min-h-8 rounded-md px-2 aria-pressed:bg-hover aria-pressed:text-ink" title="A fixed ceiling, so a quiet day looks quiet">tokens / min</button>
        </form>
      </div>
      <button type="button" className={cx(btn.base, btn.ghost, 'min-h-10')} disabled={reduce} aria-pressed={paused || reduce}
        onClick={togglePause} title={reduce ? 'Reduced motion is on in your browser or Settings.' : 'Pause the animation. Numbers keep updating.'}>
        {reduce ? 'Reduced motion' : paused ? 'Resume' : 'Pause'}
      </button>
      {typeof document !== 'undefined' && document.fullscreenEnabled && (
        <button type="button" className={cx(btn.base, btn.ghost, 'min-h-10')} onClick={toggleFull} aria-pressed={full} title="Show the city full screen, for a wall display">
          {full ? 'Exit full screen' : 'Full screen'}
        </button>
      )}
    </div>

    {stale && <div role="status" className="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-warn/30 bg-warn-dim px-4 py-3 text-xs text-warn">
      <span>{unknown ? 'Usage didn\'t load, so the buildings have no activity numbers.' : 'The last update failed. This is the most recent city.'}</span>
      <button className="min-h-10 px-2 font-medium underline underline-offset-4" onClick={retry}>Try again</button>
    </div>}

    <div className="nbhd-stage" ref={stage}>
      <CityScene buildings={buildings} ceiling={ceiling} mode={mode} still={still} hot={hot} selected={selected}
        onHover={setHotScene} onSelect={select} />
      {full && <button type="button" className="nbhd-exit-full" onClick={toggleFull}>Exit full screen</button>}
    </div>

    {detail && <div ref={details} className="flex flex-wrap items-start justify-between gap-4 rounded-xl border border-line bg-surface px-4 py-3" aria-label="Selected building" aria-live="polite">
      <div className="min-w-0 space-y-1">
        <p className="flex flex-wrap items-center gap-2 break-words text-sm font-semibold text-ink"><span>{detail.name}</span><Chip tone={HEALTH_TONE[detail.health]}>{HEALTH_WORDS[detail.health]}</Chip></p>
        <p className="text-xs text-mute">{detail.namespace} · {detail.keys.length} {detail.keys.length === 1 ? 'key' : 'keys'}</p>
        {detail.keys.length > 1 && <p className="break-words text-xs text-mute">{detail.keys.map((k) => `${k.label}${k.enabled ? '' : ' (switched off)'} ${formatTokens(Math.round(k.tpm))}/min`).join(' · ')}</p>}
        {detail.health === 'failing' && <p className="text-xs text-bad">A quarter or more of the calls in the last {wordsOf} failed.</p>}
        {issues.map((i) => (
          <p key={i.id} className={cx('text-xs', i.tone === 'bad' ? 'text-bad' : 'text-warn')}>
            {i.text}{' '}<Link className="underline underline-offset-2 hover:text-ink" to={providerLink(Number(detail.id), i.fix)}>Fix</Link>
          </p>
        ))}
        <p className="flex flex-wrap items-center gap-x-4 gap-y-1 pt-1 text-xs">
          <Link className="text-accent hover:underline" to={providerLink(Number(detail.id), 'open')}>Open provider</Link>
          <Link className="text-accent hover:underline" to={`/requests?${new URLSearchParams({ account: detail.namespace, range: '24h' })}`}>View requests</Link>
          <Link className="text-accent hover:underline" to={providerLink(Number(detail.id), 'keys')}>Edit keys</Link>
        </p>
      </div>
      <dl className="grid grid-cols-[auto_auto] gap-x-4 gap-y-0.5 text-xs">
        <dt className="text-mute">Tokens / min</dt><dd className="tnum text-right text-ink">{shown(detail, formatTokens(Math.round(detail.tpm)))}</dd>
        <dt className="text-mute">Calls</dt><dd className="tnum text-right text-ink">{shown(detail, formatTokens(detail.calls))}</dd>
        <dt className="text-mute">Failed</dt><dd className={cx('tnum text-right', detail.errors > 0 ? 'text-bad' : 'text-ink')}>{shown(detail, `${formatTokens(detail.errors)}${detail.calls > 0 ? ` · ${pct(detail.errors, detail.calls)}` : ''}`)}</dd>
        <dt className="text-mute">Since last update</dt><dd className="tnum text-right text-dim">{(() => { const d = trends.get(detail.id) ?? 0; return Math.abs(d) < 0.5 ? 'steady' : `${d > 0 ? '+' : '−'}${formatTokens(Math.abs(Math.round(d)))}`; })()}</dd>
      </dl>
      <button type="button" className="text-xs text-mute underline underline-offset-4 hover:text-ink" onClick={() => setSelected(null)}>Close</button>
    </div>}

    <div className="rounded-xl border border-line bg-surface">
      <div className="flex flex-wrap items-center justify-between gap-3 px-4 pt-3">
        <SectionLabel>Every building</SectionLabel>
        <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
          <input aria-label="Search buildings" type="search" placeholder="Search providers" value={query} onChange={(e) => setQuery(e.target.value)} className={cx(inputCls, '!w-44 py-1')} />
          <Segmented<StateFilter> size="sm" label="Show buildings" value={filter} onChange={setFilter}
            options={[
              { key: 'all', label: 'All' },
              { key: 'failing', label: failing.length ? `Failing (${failing.length})` : 'Failing' },
              { key: 'serving', label: 'Serving' }, { key: 'quiet', label: 'Quiet' }, { key: 'off', label: 'Switched off' },
            ]} />
        </div>
      </div>
      {/* Desktop: the table. relative: the sr-only labels inside are absolutely
          positioned and would otherwise escape this scroller and widen the page. */}
      <div className="relative hidden overflow-x-auto md:block">
        <table className="w-full text-[13px]">
          <thead><tr className="text-left text-[11px] text-mute">
            <SortTh label="Provider" k="name" sort={sort} onSort={onSort} className="px-4" /><th className="px-3 py-2 font-medium">Keys</th><th className="px-3 py-2 font-medium">Height</th>
            <SortTh label="Tokens / min" k="tpm" sort={sort} onSort={onSort} className="px-3 text-right" /><SortTh label="Calls" k="calls" sort={sort} onSort={onSort} className="px-3 text-right" />
            <SortTh label="Failed" k="errors" sort={sort} onSort={onSort} className="px-3 text-right" /><SortTh label="Failure rate" k="rate" sort={sort} onSort={onSort} className="px-3 text-right" />
            <th className="px-4 py-2 font-medium">State</th><th className="px-3 py-2 font-medium"><span className="sr-only">Links</span></th>
          </tr></thead>
          <tbody>{ranked.map((b) => <tr key={b.id} data-row={b.id} className={cx('border-t border-line transition-colors', (hot === b.id || selected === b.id) && 'bg-hover')}
            onMouseEnter={() => setHotRow(b.id)} onMouseLeave={() => setHotRow(null)}>
            <td className="px-4 py-2"><button className="min-h-10 text-left underline-offset-4 hover:underline focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent" onClick={() => selectFromList(b.id)} aria-pressed={selected === b.id} onFocus={() => setHotRow(b.id)} onBlur={() => setHotRow(null)}>{b.name}</button></td>
            <td className="tnum px-3 py-2 text-dim">{b.keys.length}</td>
            <td className="px-3 py-2"><div className="h-1.5 w-20 overflow-hidden rounded-full bg-line" title={b.health === 'unknown' ? 'Usage unavailable' : `${Math.round(share(b.tpm, ceiling) * 100)}% of full height (${formatTokens(ceiling)} tokens / min)`}><div className="h-full rounded-full bg-accent" style={{ width: `${Math.round(share(b.tpm, ceiling) * 100)}%` }} /></div></td>
            <td className="tnum px-3 py-2 text-right">{shown(b, '')}{b.health !== 'unknown' && <><Trend d={trends.get(b.id) ?? 0} />{' '}{formatTokens(Math.round(b.tpm))}</>}</td>
            <td className="tnum px-3 py-2 text-right">{shown(b, formatTokens(b.calls))}</td>
            <td className={cx('tnum px-3 py-2 text-right', b.errors > 0 && 'text-bad')}>{shown(b, formatTokens(b.errors))}</td>
            <td className={cx('tnum px-3 py-2 text-right', b.health === 'failing' && 'text-bad')}>{shown(b, pct(b.errors, b.calls))}</td>
            <td className="px-4 py-2"><Chip tone={HEALTH_TONE[b.health]}>{HEALTH_WORDS[b.health]}</Chip></td>
            <td className="whitespace-nowrap px-3 py-2 text-[12px]">
              <Link className="text-mute hover:text-ink" to={providerLink(Number(b.id), 'open')} aria-label={`Open ${b.name}`}>Provider</Link>
              <span className="mx-1.5 text-mute/50" aria-hidden>·</span>
              <Link className="text-mute hover:text-ink" to={`/requests?${new URLSearchParams({ account: b.namespace, range: '24h' })}`} aria-label={`Requests for ${b.name}`}>Requests</Link>
            </td>
          </tr>)}
          {ranked.length === 0 && <tr><td colSpan={9} className="px-4 py-6 text-center text-mute">No building matches. <button type="button" className="underline" onClick={() => { setQuery(''); setFilter('all'); }}>Clear search and filter</button></td></tr>}</tbody>
        </table>
      </div>
      {/* Phone: one card per building. */}
      <ul className="divide-y divide-line md:hidden">
        {ranked.map((b) => <li key={b.id} data-card={b.id} className={cx('px-4 py-3', selected === b.id && 'bg-hover')}>
          <div className="flex items-center justify-between gap-3">
            <button className="min-h-10 text-left text-sm font-medium text-ink" onClick={() => selectFromList(b.id)} aria-pressed={selected === b.id}>{b.name}</button>
            <Chip tone={HEALTH_TONE[b.health]}>{HEALTH_WORDS[b.health]}</Chip>
          </div>
          <p className="tnum text-xs text-dim">{shown(b, `${formatTokens(Math.round(b.tpm))} tokens / min · ${formatTokens(b.calls)} calls · ${formatTokens(b.errors)} failed${b.calls ? ` (${pct(b.errors, b.calls)})` : ''}`)}</p>
          <p className="mt-1 flex gap-4 text-xs">
            <Link className="text-accent" to={providerLink(Number(b.id), 'open')}>Provider</Link>
            <Link className="text-accent" to={`/requests?${new URLSearchParams({ account: b.namespace, range: '24h' })}`}>Requests</Link>
          </p>
        </li>)}
        {ranked.length === 0 && <li className="px-4 py-6 text-center text-sm text-mute">No building matches.</li>}
      </ul>
      <p className="px-4 pb-3 pt-2 text-xs text-mute">{unknown ? 'Activity numbers will show up once usage loads.' : `Full height means ${formatTokens(ceiling)} tokens per minute${fixed === null ? ', set by the busiest provider' : ''}.`}{' '}<button type="button" className="underline underline-offset-4 hover:text-ink" onClick={retry}>Refresh now</button></p>
    </div>
  </div>;
}
