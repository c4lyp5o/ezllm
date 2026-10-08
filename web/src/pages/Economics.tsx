import { useCallback, useEffect, useMemo, useState } from 'react';
import { requests, normalizeFeedRow, RANGE_LABEL, type FeedRow, type RangeKey } from '../api';
import { SectionLabel, SkeletonCards, btn, cx, formatTokens, inputCls } from '../ui';

const RANGES: RangeKey[] = ['24h', '7d', '30d', '365d'];

function Metric({ label, value, note }: { label: string; value: string; note?: string }) {
  return <div className="border border-line bg-surface px-4 py-4"><div className="text-[11px] uppercase tracking-wide text-mute">{label}</div><div className="mt-2 font-mono text-xl tabular-nums text-ink">{value}</div>{note && <div className="mt-1 text-[11px] text-mute">{note}</div>}</div>;
}

export default function Economics() {
  const [range, setRange] = useState<RangeKey>('24h');
  const [rows, setRows] = useState<FeedRow[]>([]);
  const [summary, setSummary] = useState<any>(null);
  const [minInput, setMinInput] = useState('');
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');

  const load = useCallback(async () => {
    setLoading(true); setError('');
    try {
      const r = await requests({ range, sort: 'tin_desc', minTin: minInput ? Number(minInput) : undefined, limit: 100 });
      setRows(r.rows.map(normalizeFeedRow)); setSummary(r.summary);
    } catch (e) { setError(e instanceof Error ? e.message : 'could not load token economics'); }
    finally { setLoading(false); }
  }, [range, minInput]);
  useEffect(() => { void load(); }, [load]);

  const totals = useMemo(() => ({
    context: summary?.context_saved ?? 0,
    compression: summary?.compression_saved ?? 0,
  }), [summary]);
  const input = summary?.tin ?? 0;
  const output = summary?.tout ?? 0;
  const cached = summary?.cread ?? 0;
  const reasoning = summary?.reasoning ?? 0;
  const saved = totals.context + totals.compression;
  const before = input + cached + saved;
  const pct = before > 0 ? ((saved / before) * 100).toFixed(1) : '0.0';

  return <div className="space-y-5">
    <div className="flex flex-wrap items-end justify-between gap-3">
      <div><h2 className="text-lg font-semibold text-ink">Token Economics</h2><p className="mt-1 text-[12px] text-mute">What arrived, what was cached, and what ezllm removed before forwarding.</p></div>
      <div className="flex flex-wrap gap-1 rounded-lg border border-line bg-surface p-1">{RANGES.map(r => <button key={r} onClick={() => setRange(r)} className={cx('rounded-md px-2.5 py-1 text-[12px]', r === range ? 'bg-accent text-white' : 'text-mute hover:text-ink')}>{RANGE_LABEL[r]}</button>)}</div>
    </div>
    <section className="border border-line bg-surface px-5 py-4"><SectionLabel>large-request filter</SectionLabel><div className="mt-3 flex flex-wrap items-end gap-3"><label className="text-[11px] uppercase tracking-wide text-mute">Minimum actual input tokens<input value={minInput} onChange={e => setMinInput(e.target.value)} inputMode="numeric" placeholder="all" className={cx(inputCls, 'mt-1 w-44 font-mono')} /></label><button onClick={() => void load()} className={cx(btn.base, btn.primary)}>Analyze</button><span className="text-[11px] text-mute">Sorted largest first; use Requests for full bounds and per-row inspection.</span></div></section>
    {error && <div className="border border-bad/30 bg-bad/5 px-4 py-3 text-sm text-bad">{error}</div>}
    {loading ? <SkeletonCards n={5} /> : <>
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-5"><Metric label="actual input" value={formatTokens(input)} /><Metric label="output" value={formatTokens(output)} /><Metric label="cached read" value={formatTokens(cached)} /><Metric label="context saved" value={formatTokens(totals.context)} /><Metric label="compression saved" value={formatTokens(totals.compression)} note={`${pct}% estimated reduction`} /></div>
      <div className="grid gap-3 sm:grid-cols-2"><Metric label="reasoning output" value={formatTokens(reasoning)} /><Metric label="estimated pre-optimization" value={formatTokens(before)} note={`${summary?.count ?? rows.length} matching calls`} /></div>
      <section className="border border-line bg-surface"><div className="border-b border-line px-5 py-3"><SectionLabel>largest requests</SectionLabel></div>{rows.length === 0 ? <div className="px-5 py-8 text-center text-sm text-mute">No matching calls in this range.</div> : <div className="overflow-x-auto"><table className="w-full min-w-[720px] text-[12px]"><thead><tr className="text-left text-[10px] uppercase tracking-wide text-mute"><th className="px-5 py-2">model</th><th className="px-3 py-2 text-right">actual in</th><th className="px-3 py-2 text-right">context saved</th><th className="px-3 py-2 text-right">compression</th><th className="px-3 py-2 text-right">out</th><th className="px-5 py-2 text-right">status</th></tr></thead><tbody className="font-mono tabular-nums">{rows.map((r, i) => <tr key={`${r.ts}-${i}`} className="border-t border-line/60"><td className="max-w-[280px] truncate px-5 py-2 text-ink">{r.model}</td><td className="px-3 py-2 text-right text-dim">{formatTokens(r.tin)}</td><td className="px-3 py-2 text-right text-sky-300/80">{formatTokens(r.contextSaved ?? 0)}</td><td className="px-3 py-2 text-right text-emerald-300/80">{formatTokens(r.saved)}</td><td className="px-3 py-2 text-right text-dim">{formatTokens(r.tout)}</td><td className="px-5 py-2 text-right text-dim">{r.status}</td></tr>)}</tbody></table></div>}</section>
    </>}
  </div>;
}
