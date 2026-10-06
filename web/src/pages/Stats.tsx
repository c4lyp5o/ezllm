// STATS — the granularity + range explorer.
//
// Two selectors drive ONE view (the locked design): a granularity toggle
// (global / provider / model / cache / compression / savings) and a range
// toggle (24h / 7d / 30d / 1y). Every combination resolves to a single
// /admin/usage query whose group_by and highlighted columns follow the chosen
// granularity — so it is one data path, not six.
//
// The reactive recent-requests feed (SSE) lands separately; this is the
// stats-first pass.
import { useCallback, useEffect, useState } from 'react';
import {
  BarChart, Bar, XAxis, YAxis, CartesianGrid, Tooltip, ResponsiveContainer, LineChart, Line,
} from 'recharts';
import {
  usage, type UsageResp, type UsageRow, type Granularity, type RangeKey,
  RANGE_LABEL, GRANULARITY_GROUP,
} from '../api';
import { Chip, SectionLabel, SkeletonCards, SkeletonRows, cx, formatTokens, Mono } from '../ui';
import RecentFeed from './RecentFeed';

const GRAN: { key: Granularity; label: string }[] = [
  { key: 'global', label: 'Global' },
  { key: 'provider', label: 'Provider' },
  { key: 'model', label: 'Model' },
  { key: 'cache', label: 'Cache' },
  { key: 'compression', label: 'Compression' },
  { key: 'savings', label: 'Savings' },
];
const RANGES: RangeKey[] = ['24h', '7d', '30d', '365d'];

function Toggle({ options, value, onChange, labelFor }: {
  options: string[]; value: string; onChange: (v: string) => void; labelFor?: (v: string) => string;
}) {
  return (
    <div className="inline-flex flex-wrap gap-1 rounded-lg border border-line bg-surface p-1">
      {options.map((o) => (
        <button
          key={o}
          onClick={() => onChange(o)}
          className={cx(
            'rounded-md px-2.5 py-1 text-[12px] font-medium transition-colors',
            o === value ? 'bg-accent text-white' : 'text-mute hover:text-ink',
          )}
        >
          {labelFor ? labelFor(o) : o}
        </button>
      ))}
    </div>
  );
}

function Card({ label, value, sub, tone }: { label: string; value: string; sub?: string; tone?: string }) {
  return (
    <div className="rounded-xl border border-line bg-surface px-4 py-3.5">
      <SectionLabel>{label}</SectionLabel>
      <div className={cx('mt-1.5 font-mono text-[22px] font-semibold tabular-nums tracking-tight', tone ?? 'text-ink')}>{value}</div>
      {sub && <div className="mt-0.5 text-[11px] text-mute">{sub}</div>}
    </div>
  );
}

// Group rows into chart points. For time-series granularities (cache,
// compression, savings) group_by=day gives YYYY-MM-DD keys → a line. For
// entity granularities (provider, model) the key is a name → a bar.
function isTimeSeries(gran: Granularity): boolean {
  return gran === 'cache' || gran === 'compression' || gran === 'savings';
}

export default function Stats() {
  const [gran, setGran] = useState<Granularity>('global');
  const [range, setRange] = useState<RangeKey>('24h');
  const [data, setData] = useState<UsageResp | null>(null);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    setErr(null);
    try {
      const r = await usage(range, gran);
      setData(r);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, [range, gran]);

  useEffect(() => { load(); }, [load]);

  const t = data?.totals;
  // Which column the selected granularity cares about most.
  const primaryOf = (r: UsageRow): number => {
    switch (gran) {
      case 'cache': return r.cread + r.cwrite;
      case 'compression': return r.saved;
      case 'savings': return r.saved;
      case 'provider': case 'model': return r.tin + r.tout;
      default: return r.tin + r.tout;
    }
  };
  const primaryLabel = (): string => {
    switch (gran) {
      case 'cache': return 'cached tokens';
      case 'compression': case 'savings': return 'tokens saved';
      default: return 'tokens';
    }
  };

  const rows = data?.groups ?? [];
  const chartable = gran !== 'global' && rows.length > 0;

  return (
    <div className="space-y-5">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-lg font-semibold text-ink">Stats</h1>
          <p className="text-[12px] text-mute">Granularity × time range — one query, one view.</p>
        </div>
      </div>

      {/* Selectors */}
      <div className="flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex flex-wrap gap-2">
          <Toggle options={GRAN.map((g) => g.key)} value={gran} onChange={(v) => setGran(v as Granularity)}
            labelFor={(v) => GRAN.find((g) => g.key === v)?.label ?? v} />
        </div>
        <Toggle options={RANGES} value={range} onChange={(v) => setRange(v as RangeKey)}
          labelFor={(v) => RANGE_LABEL[v as RangeKey]} />
      </div>

      {err && (
        <div className="rounded-xl border border-bad/40 bg-bad/10 px-4 py-3 text-[13px] text-bad">{err}</div>
      )}

      {loading && !data && (
        <div className="space-y-3"><SkeletonCards n={4} /><SkeletonRows rows={5} /></div>
      )}

      {data && (
        <>
          {/* Totals strip — always visible, ground truth for the range */}
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-6">
            <Card label="calls" value={formatTokens(t!.calls)} />
            <Card label="tokens in" value={formatTokens(t!.tin)} />
            <Card label="tokens out" value={formatTokens(t!.tout)} />
            <Card label={primaryLabel()} value={formatTokens(primaryOf(t!))}
              tone={gran === 'cache' || gran === 'savings' || gran === 'compression' ? 'text-ok' : undefined} />
            <Card label="cached read" value={formatTokens(t!.cread)} />
            <Card label="errors" value={formatTokens(t!.errors)} tone={t!.errors > 0 ? 'text-warn' : 'text-ok'} />
          </div>

          {/* Chart for entity / time-series breakdowns */}
          {chartable && (
            <div className="rounded-xl border border-line bg-surface p-4">
              <div className="mb-3 flex items-center justify-between">
                <SectionLabel>
                  {GRAN.find((g) => g.key === gran)?.label} — {primaryLabel()} · {RANGE_LABEL[range]}
                </SectionLabel>
                <Chip className="px-2 py-0.5"><Mono className="text-[10px] text-mute">{data.group_by}</Mono></Chip>
              </div>
              <div className="h-64">
                <ResponsiveContainer width="100%" height="100%">
                  {isTimeSeries(gran) ? (
                    <LineChart data={[...rows].reverse().map((r) => ({ ...r, cached: r.cread + r.cwrite }))}>
                      <CartesianGrid strokeDasharray="3 3" stroke="currentColor" className="text-line" />
                      <XAxis dataKey="k" tick={{ fontSize: 10 }} stroke="currentColor" className="text-mute" />
                      <YAxis tick={{ fontSize: 10 }} stroke="currentColor" className="text-mute"
                        tickFormatter={(v) => formatTokens(v as number)} width={56} />
                      <Tooltip
                        contentStyle={{ background: '#111', border: '1px solid #333', borderRadius: 8, fontSize: 12 }}
                        formatter={(v: number) => [formatTokens(v), primaryLabel()]}
                      />
                      <Line type="monotone"
                        dataKey={gran === 'cache' ? 'cached' : 'saved'}
                        stroke="#4ade80" strokeWidth={2} dot={false}
                        name={primaryLabel()} />
                    </LineChart>
                  ) : (
                    <BarChart data={rows}>
                      <CartesianGrid strokeDasharray="3 3" stroke="currentColor" className="text-line" />
                      <XAxis dataKey="k" tick={{ fontSize: 10 }} stroke="currentColor" className="text-mute" />
                      <YAxis tick={{ fontSize: 10 }} stroke="currentColor" className="text-mute"
                        tickFormatter={(v) => formatTokens(v as number)} width={56} />
                      <Tooltip
                        contentStyle={{ background: '#111', border: '1px solid #333', borderRadius: 8, fontSize: 12 }}
                        formatter={(v: number) => [formatTokens(v), primaryLabel()]}
                      />
                      <Bar dataKey={(r: UsageRow) => primaryOf(r)} fill="#60a5fa" radius={[3, 3, 0, 0]} name={primaryLabel()} />
                    </BarChart>
                  )}
                </ResponsiveContainer>
              </div>
            </div>
          )}

          {/* Breakdown table — the raw rows behind the chart */}
          {gran !== 'global' && (
            <div className="rounded-xl border border-line bg-surface">
              <div className="border-b border-line px-4 py-2.5">
                <SectionLabel>Breakdown · {RANGE_LABEL[range]} · grouped by {data.group_by}</SectionLabel>
              </div>
              <div className="overflow-x-auto">
                <table className="w-full text-[12px]">
                  <thead>
                    <tr className="text-left text-mute">
                      <th className="px-4 py-2 font-medium">key</th>
                      <th className="px-4 py-2 text-right font-medium">calls</th>
                      <th className="px-4 py-2 text-right font-medium">in</th>
                      <th className="px-4 py-2 text-right font-medium">out</th>
                      <th className="px-4 py-2 text-right font-medium">cached</th>
                      <th className="px-4 py-2 text-right font-medium">saved</th>
                      <th className="px-4 py-2 text-right font-medium">errors</th>
                    </tr>
                  </thead>
                  <tbody>
                    {rows.map((r) => (
                      <tr key={r.k} className="border-t border-line/60">
                        <td className="px-4 py-1.5 font-mono text-ink">{r.k}</td>
                        <td className="px-4 py-1.5 text-right tabular-nums text-ink">{formatTokens(r.calls)}</td>
                        <td className="px-4 py-1.5 text-right tabular-nums text-mute">{formatTokens(r.tin)}</td>
                        <td className="px-4 py-1.5 text-right tabular-nums text-mute">{formatTokens(r.tout)}</td>
                        <td className="px-4 py-1.5 text-right tabular-nums text-mute">{formatTokens(r.cread + r.cwrite)}</td>
                        <td className="px-4 py-1.5 text-right tabular-nums text-ok">{formatTokens(r.saved)}</td>
                        <td className={cx('px-4 py-1.5 text-right tabular-nums', r.errors > 0 ? 'text-warn' : 'text-mute')}>{formatTokens(r.errors)}</td>
                      </tr>
                    ))}
                    {rows.length === 0 && (
                      <tr><td colSpan={7} className="px-4 py-6 text-center text-mute">No usage in this range.</td></tr>
                    )}
                  </tbody>
                </table>
              </div>
            </div>
          )}

          {gran === 'global' && rows.length > 0 && (
            <div className="rounded-xl border border-line bg-surface p-4 text-[12px] text-mute">
              Showing range totals. Pick a granularity above for a per-{''}
              {GRANULARITY_GROUP[gran] ?? 'account'} breakdown.
            </div>
          )}
        </>
      )}

      <RecentFeed />
    </div>
  );
}
