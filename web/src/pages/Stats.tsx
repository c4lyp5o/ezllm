// ANALYTICS EXPLORER — the /admin/usage report, exposed properly.
//
// The old design was ONE toggle called "granularity" that folded two
// independent questions into one enum: which DIMENSION do I split by, and
// which COLUMN do I care about. Choosing "Cache" silently meant "group by
// account" — you could not see cache spend per model, and you could not see
// token volume per client. Both axes are separate here:
//
//   split → group_by. Eight choices; the server has supported all eight
//           (store.usageGroupColumn) since M3 and the dashboard used three.
//   focus → which column the chart and the ranking emphasise. Free: the row
//           already carries every column, so a focus change re-reads the data
//           already in hand — no second request.
//   range → 24h / 7d / 30d / 1y.
//
// Every combination is still ONE /admin/usage query. Nothing here aggregates
// raw ledger rows in the browser; the SQL does it and this renders it.
import { useCallback, useEffect, useMemo, useState } from 'react';
import {
  BarChart, Bar, XAxis, YAxis, CartesianGrid, Tooltip, ResponsiveContainer, AreaChart, Area,
} from 'recharts';
import {
  usage, FOCUS_HINT, FOCUS_LABEL, SPLIT_HINT, SPLIT_LABEL,
  focusFormat, focusValue, RANGE_LABEL,
  type FocusKey, type RangeKey, type SplitKey, type UsageResp, type UsageRow,
} from '../utils/api';
import {
  ChartTooltip, BarList, CompositionBar, Panel, Sparkline, StatCard, VIZ, axisProps,
  catColor, chartGrid, cursorFill,
} from '../components/charts';
import { Reveal } from '../components/motion';
import {
  Chip, EmptyState, Mono, SectionLabel, Segmented, cx, fmtMs, formatTokens, nfmt, pct,
} from '../components/ui';
import RecentFeed from './RecentFeed';

const SPLITS: SplitKey[] = ['global', 'day', 'account', 'model', 'client', 'alias', 'key', 'surface'];
const FOCI: FocusKey[] = ['tokens', 'cache', 'savings', 'reasoning', 'latency', 'errors'];
const RANGES: RangeKey[] = ['24h', '7d', '30d', '365d'];

// Which series a focus draws on a multi-series chart. `global` and named
// splits both read this; single-valued focuses (latency, errors, reasoning)
// draw one series rather than meaningless ones. Keys are LEDGER FIELDS —
// the chart reads row[s.key], so a key that is not a UsageRow field draws
// nothing (that is exactly how 'compression'/'savings'/'latency' drew no
// bars for their whole lives: the key was the focus NAME, not the column).
function seriesOf(focus: FocusKey): { key: keyof UsageRow | string; label: string; color: string }[] {
  switch (focus) {
    case 'cache':
      return [
        { key: 'cread', label: 'cached read', color: VIZ.cached },
        { key: 'cwrite', label: 'cached write', color: VIZ.out },
      ];
    case 'tokens':
      return [
        { key: 'tin', label: 'tokens in', color: VIZ.in },
        { key: 'tout', label: 'tokens out', color: VIZ.out },
      ];
    case 'savings':
      // Two mechanisms, two bars, two colours: compression actually rewrote
      // (and shipped a shorter prompt); the context-budget stage kept history
      // out. Either can be 0 — the zero-filter below drops those rows.
      return [
        { key: 'saved', label: 'compression', color: VIZ.saved },
        { key: 'context_saved', label: 'context budget', color: VIZ.budget },
      ];
    case 'latency':
      return [{ key: 'p50_ttft_ms', label: 'p50 ttft', color: VIZ.latency }];
    case 'reasoning':
      return [{ key: 'reasoning', label: 'reasoning', color: VIZ.reasoning }];
    case 'errors':
      return [{ key: 'errors', label: 'errors', color: VIZ.error }];
  }
}

function focusColor(focus: FocusKey): string {
  switch (focus) {
    case 'cache': return VIZ.cached;
    case 'savings': return VIZ.saved;
    case 'reasoning': return VIZ.reasoning;
    case 'latency': return VIZ.latency;
    case 'errors': return VIZ.error;
    default: return VIZ.in;
  }
}

export default function Stats() {
  const [split, setSplit] = useState<SplitKey>('account');
  const [focus, setFocus] = useState<FocusKey>('tokens');
  const [range, setRange] = useState<RangeKey>('24h');
  const [data, setData] = useState<UsageResp | null>(null);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    setErr(null);
    try {
      setData(await usage(split, range));
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, [split, range]);

  useEffect(() => { load(); }, [load]);

  const rows = data?.groups ?? [];
  const totals = data?.totals;
  const fmt = focusFormat(focus);
  const series = seriesOf(focus);
  const isTime = split === 'day';

  // The Combo split shows ONLY combos. Direct model routes are recorded with
  // an alias of "ns/model" (contains '/') or the '(direct)' placeholder, and
  // combo names can never contain '/' — so the exact filter is defenseless.
  // Direct traffic stays visible in the totals cards and the Every-column
  // table is combo-scoped too; the ranked panel aside says what share of the
  // total the displayed rows are.
  const rawRows = useMemo(() => {
    if (split !== 'alias') return rows;
    return rows.filter((r) => !r.k.includes('/') && r.k !== '(direct)');
  }, [rows, split]);

  // Ranked rows. Split day → latest date first (a time listing reads newest
  // at top; the chart below reverses this into chronological order). Every
  // other split → focus-metric descending, biggest first.
  const ranked = useMemo(() => {
    const copy = [...rawRows];
    if (isTime) return copy.sort((a, b) => b.k.localeCompare(a.k));
    return copy.sort((a, b) => focusValue(focus, b) - focusValue(focus, a));
  }, [rawRows, focus, isTime]);

  // Bars only exist when there is something to measure: a group whose focus
  // value is 0 draws no bar and no list row. (The Every-column table keeps
  // every row — zeros there render as '–', which is data.)
  const barRows = useMemo(() => ranked.filter((r) => focusValue(focus, r) > 0), [ranked, focus]);

  // Chart rows. Bar charts read barRows (zero groups dropped). The day area
  // chart trims only LEADING and TRAILING zero days: an interior zero day
  // must stay on a time axis, because dropping it compresses the line and
  // lies about when the neighbours happened.
  const chartRows = useMemo(() => {
    if (!isTime) return barRows;
    let start = 0;
    let end = ranked.length; // ranked is newest-first; trim both ends
    while (start < end && focusValue(focus, ranked[start]) === 0) start++;
    while (end > start && focusValue(focus, ranked[end - 1]) === 0) end--;
    return ranked.slice(start, end);
  }, [ranked, barRows, focus, isTime]);

  // The ranked panel's aside said "of total" next to a fixed 100% — the
  // share of the SUM of displayed rows (post-filter) vs the window total,
  // which is the number that actually moves when a filter applies.
  const rankedFocusSum = ranked.reduce((a, r) => a + focusValue(focus, r), 0);
  const totalFocus = totals ? focusValue(focus, totals) : 0;
  const focusSeries = ranked.map((r) => focusValue(focus, r));
  const isExpanded = data?.group_by != null && data.group_by !== '';

  return (
    <div className="space-y-5">
      {/* ── controls ─────────────────────────────────────────────────────── */}
      <div className="flex flex-col gap-3 rounded-xl border border-line bg-surface px-5 py-4">
        <div className="flex flex-wrap items-end justify-between gap-x-6 gap-y-3">
          <div>
            <SectionLabel>Split by</SectionLabel>
            <div className="mt-1.5">
              <Segmented
                label="Split the report by"
                options={SPLITS.map((s) => ({ key: s, label: SPLIT_LABEL[s], title: SPLIT_HINT[s] }))}
                value={split} onChange={setSplit} />
            </div>
          </div>
          <div>
            <SectionLabel>Focus</SectionLabel>
            <div className="mt-1.5">
              <Segmented
                label="Which metric to emphasise"
                options={FOCI.map((f) => ({ key: f, label: FOCUS_LABEL[f], title: FOCUS_HINT[f] }))}
                value={focus} onChange={setFocus} />
            </div>
          </div>
          <div>
            <SectionLabel>Range</SectionLabel>
            <div className="mt-1.5">
              <Segmented
                label="Time range"
                options={RANGES.map((r) => ({ key: r, label: RANGE_LABEL[r] }))}
                value={range} onChange={setRange} />
            </div>
          </div>
        </div>
      </div>

      {err && (
        <div role="alert" className="rounded-xl border border-bad/30 bg-bad-dim px-5 py-4 text-[13px] text-bad">
          {err}
          <button onClick={() => void load()} className="ml-3 underline underline-offset-2 hover:text-ink">retry</button>
        </div>
      )}

      {/* ── totals ──────────────────────────────────────────────────────── */}
      {totals && (
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-4 xl:grid-cols-7">
          <StatCard label="calls" value={nfmt(totals.calls)} sub={`${RANGE_LABEL[range]} window`} />
          <StatCard label="tokens in" value={formatTokens(totals.tin)} />
          <StatCard label="tokens out" value={formatTokens(totals.tout)} sub={`${pct(totals.tout, totals.tin + totals.tout)} of volume`} />
          <StatCard label="cached read" value={formatTokens(totals.cread)}
            sub={`${pct(totals.cread, totals.tin)} of input re-read from cache`} />
          <StatCard label="reasoning" value={formatTokens(totals.reasoning)}
            tone={totals.reasoning > 0 ? 'text-warn' : undefined}
            sub="billed, never shown" />
          <StatCard label="saved" value={formatTokens(totals.saved)}
            tone={totals.saved > 0 ? 'text-ok' : undefined}
            sub={totals.context_saved > 0 ? `+${formatTokens(totals.context_saved)} context budget` : 'compression only'} />
          <StatCard
            label="p50 ttft"
            value={fmtMs(totals.p50_ttft_ms)}
            tone={totals.p50_ttft_ms > 0 ? 'text-[color:var(--color-viz-latency)]' : 'text-mute'}
            sub={totals.p50_ttft_ms > 0 ? 'median first token' : 'not measured yet'} />
        </div>
      )}

      {/* ── chart ───────────────────────────────────────────────────────── */}
      {loading && !data && (
        <Panel label={<SectionLabel>loading…</SectionLabel>}>
          <div className="h-72 animate-pulse-soft rounded-lg bg-inset" />
        </Panel>
      )}

      {totals && data && !isExpanded && (
        <Panel
          label={<SectionLabel>Everything · last {RANGE_LABEL[range]}</SectionLabel>}
          aside={<Chip><Mono className="text-[10px]">totals only</Mono></Chip>}>
          <div className="grid gap-5 lg:grid-cols-[minmax(0,1fr)_minmax(0,1.1fr)]">
            <div>
              <p className="text-[12.5px] leading-relaxed text-dim">
                The range totals above are the whole ledger view. Pick a dimension
                under <span className="text-ink">Split by</span> to break it down.
              </p>
              <div className="mt-5">
                <SectionLabel>Token composition</SectionLabel>
                <div className="mt-2.5">
                  <CompositionBar parts={[
                    { key: 'tin', label: 'tokens in', value: totals.tin, color: VIZ.in },
                    { key: 'tout', label: 'tokens out', value: totals.tout, color: VIZ.out },
                    { key: 'cread', label: 'cached read', value: totals.cread, color: VIZ.cached },
                    { key: 'reasoning', label: 'reasoning', value: totals.reasoning, color: VIZ.reasoning },
                  ]} height={12} />
                </div>
                <ul className="mt-3 space-y-1.5">
                  {[
                    { key: 'tin', label: 'tokens in', value: totals.tin, color: VIZ.in },
                    { key: 'tout', label: 'tokens out', value: totals.tout, color: VIZ.out },
                    { key: 'cread', label: 'cached read', value: totals.cread, color: VIZ.cached },
                    { key: 'reasoning', label: 'reasoning', value: totals.reasoning, color: VIZ.reasoning },
                  ].map((p) => (
                    <li key={p.key} className="flex items-center gap-2 text-[12px]">
                      <span className="h-2 w-2 shrink-0 rounded-full" style={{ background: p.color }} />
                      <span className="text-dim">{p.label}</span>
                      <span className="tnum ml-auto font-mono text-ink">{formatTokens(p.value)}</span>
                      <span className="tnum w-12 text-right font-mono text-mute">
                        {pct(p.value, totals.tin + totals.tout + totals.cread + totals.reasoning)}
                      </span>
                    </li>
                  ))}
                </ul>
              </div>
            </div>
            <div>
              <SectionLabel>Trend · {FOCUS_LABEL[focus]}</SectionLabel>
              <div className="mt-2.5"><Sparkline data={focusSeries} color={focusColor(focus)} height={64} /></div>
              <p className="mt-2 text-[11.5px] leading-relaxed text-mute">
                Ranked order over {nfmt(rows.length)} group{rows.length === 1 ? '' : 's'}, not a time
                series — the ledger's only time bucketing is the calendar day,
                so switch <span className="text-dim">Split by</span> to Day for the trend.
              </p>
            </div>
          </div>
        </Panel>
      )}

      {data && isExpanded && ranked.length === 0 && (
        <Panel label={<SectionLabel>{SPLIT_LABEL[split]} · last {RANGE_LABEL[range]}</SectionLabel>}>
          {rows.length > 0 ? (
            // Rows exist for this split — the combo filter removed them all.
            <EmptyState
              title="No combo-routed calls in this window."
              hint="Every excluded row is a DIRECT model call (an alias holding '/', or no alias at all) — combos and direct routes can't share a name, so nothing was hidden by accident. Split by account or surface to see the direct traffic." />
          ) : (
            <EmptyState
              title={`No calls in the last ${RANGE_LABEL[range]}.`}
              hint="The ledger is empty for this window and dimension. Widen the range, or route a request through a combo and it appears here within a second." />
          )}
        </Panel>
      )}

      {totals && data && isExpanded && ranked.length > 0 && (
        <>
          <Panel
            label={
              <SectionLabel>
                {FOCUS_LABEL[focus]} by {SPLIT_LABEL[split].toLowerCase()} · last {RANGE_LABEL[range]}
              </SectionLabel>
            }
            aside={<Chip><Mono className="text-[10px]">{data.group_by}</Mono></Chip>}>
            <div className="h-72">
              {chartRows.length === 0 ? (
                <div className="flex h-full items-center justify-center px-6">
                  <p className="max-w-sm text-center text-[12.5px] leading-relaxed text-mute">
                    {FOCUS_LABEL[focus]} measured 0 across every {SPLIT_LABEL[split].toLowerCase()} group in this
                    window — nothing to plot. Switch the Focus, or widen the range.
                  </p>
                </div>
              ) : (
                <ResponsiveContainer width="100%" height="100%">
                  {isTime ? (
                    <AreaChart data={[...chartRows].reverse()} margin={{ top: 4, right: 8, bottom: 0, left: -8 }}>
                      <defs>
                        {series.map((s) => (
                          <linearGradient key={String(s.key)} id={`fill-${String(s.key)}`} x1="0" y1="0" x2="0" y2="1">
                            <stop offset="0%" stopColor={s.color} stopOpacity={0.3} />
                            <stop offset="100%" stopColor={s.color} stopOpacity={0} />
                          </linearGradient>
                        ))}
                      </defs>
                      <CartesianGrid {...chartGrid} vertical={false} />
                      <XAxis dataKey="k" {...axisProps} />
                      <YAxis {...axisProps} width={58} tickFormatter={(v: number) => fmt(v)} />
                      <Tooltip content={<ChartTooltip format={fmt} />} cursor={cursorFill} />
                      {series.map((s) => (
                        <Area
                          key={String(s.key)}
                          type="monotone" dataKey={s.key as string} name={s.label}
                          stroke={s.color} strokeWidth={2} fill={`url(#fill-${String(s.key)})`}
                          dot={false} activeDot={{ r: 3, strokeWidth: 0 }} />
                      ))}
                    </AreaChart>
                  ) : (
                    <BarChart data={chartRows} margin={{ top: 4, right: 8, bottom: 0, left: -8 }} barCategoryGap="26%">
                      <CartesianGrid {...chartGrid} vertical={false} />
                      <XAxis dataKey="k" {...axisProps} interval={0}
                        tickFormatter={(v: string) => (v.length > 12 ? `${v.slice(0, 11)}…` : v)} />
                      <YAxis {...axisProps} width={58} tickFormatter={(v: number) => fmt(v)} />
                      <Tooltip content={<ChartTooltip format={fmt} />} cursor={cursorFill} />
                      {series.map((s, i) => (
                        <Bar
                          key={String(s.key)}
                          dataKey={s.key as string} name={s.label}
                          fill={s.color} maxBarSize={38}
                          radius={i === series.length - 1 ? [3, 3, 0, 0] : undefined} />
                      ))}
                    </BarChart>
                  )}
                </ResponsiveContainer>
              )}
            </div>
            {isTime && (
              <p className="mt-2 text-[11px] text-mute">
                One bucket per calendar day — the ledger's finest time grouping. A 24h
                range therefore shows one or two points; that is the data, not a rendering gap.
              </p>
            )}
          </Panel>

          {/* Ranked list and the raw rows behind it, side by side: the bar
              chart answers "who is biggest", the list gives the exact share,
              the table gives every column. Zero-value groups appear in the
              table (as '–') but draw no bar and no list row. */}
          <div className="grid gap-4 xl:grid-cols-[minmax(0,0.85fr)_minmax(0,1.15fr)]">
            <Panel
              label={<SectionLabel>Ranked · {FOCUS_LABEL[focus]}</SectionLabel>}
              aside={<span className="text-[11px] text-mute">{pct(rankedFocusSum, totalFocus)} of total</span>}>
              <BarList
                rows={barRows.map((r, i) => ({
                  key: r.k,
                  value: focusValue(focus, r),
                  // A categorical ramp per position is stable within a render
                  // but NOT across splits — the legend below the chart is the
                  // mapping of record; here the hue just separates neighbours.
                  color: series.length > 1 ? series[0].color : catColor(i),
                  hint: `${nfmt(r.calls)} calls · p50 ttft ${fmtMs(r.p50_ttft_ms)}`,
                }))}
                format={fmt} maxRows={12}
                emptyLabel={`No ${FOCUS_LABEL[focus].toLowerCase()} recorded in this window.`} />
            </Panel>

            <Panel label={<SectionLabel>Every column · {RANGE_LABEL[range]}</SectionLabel>} bodyClass="p-0">
              <div className="overflow-x-auto">
                <table className="w-full min-w-[720px] text-[12px]">
                  <thead>
                    <tr className="border-b border-line text-left text-[10.5px] uppercase tracking-[0.1em] text-mute">
                      <th className="px-5 py-2.5 font-medium">{SPLIT_LABEL[split]}</th>
                      <th className="px-3 py-2.5 text-right font-medium">calls</th>
                      <th className="px-3 py-2.5 text-right font-medium">in</th>
                      <th className="px-3 py-2.5 text-right font-medium">out</th>
                      <th className="px-3 py-2.5 text-right font-medium">cached</th>
                      <th className="px-3 py-2.5 text-right font-medium">reason</th>
                      <th className="px-3 py-2.5 text-right font-medium">saved</th>
                      <th className="px-3 py-2.5 text-right font-medium" title="median time to first token">p50</th>
                      <th className="px-5 py-2.5 text-right font-medium">err</th>
                    </tr>
                  </thead>
                  <tbody className="font-mono tnum">
                    {ranked.map((r) => (
                      <tr key={r.k} className="border-b border-line/50 transition-colors last:border-0 hover:bg-wash">
                        <td className="max-w-[240px] truncate px-5 py-2 font-sans text-ink" title={r.k}>{r.k}</td>
                        <td className="px-3 py-2 text-right text-ink">{nfmt(r.calls)}</td>
                        <td className="px-3 py-2 text-right text-dim">{formatTokens(r.tin)}</td>
                        <td className="px-3 py-2 text-right text-dim">{formatTokens(r.tout)}</td>
                        <td className="px-3 py-2 text-right text-dim">{formatTokens(r.cread + r.cwrite)}</td>
                        <td className={cx('px-3 py-2 text-right', r.reasoning > 0 ? 'text-warn' : 'text-mute')}>
                          {r.reasoning > 0 ? formatTokens(r.reasoning) : '–'}
                        </td>
                        <td className={cx('px-3 py-2 text-right', r.saved > 0 ? 'text-ok' : 'text-mute')}>
                          {r.saved > 0 ? formatTokens(r.saved) : '–'}
                        </td>
                        <td className={cx('px-3 py-2 text-right', r.p50_ttft_ms > 0 ? 'text-[color:var(--color-viz-latency)]' : 'text-mute')}>
                          {fmtMs(r.p50_ttft_ms)}
                        </td>
                        <td className={cx('px-5 py-2 text-right', r.errors > 0 ? 'text-warn' : 'text-mute')}>
                          {r.errors > 0 ? nfmt(r.errors) : '–'}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </Panel>
          </div>
        </>
      )}

      <Reveal>
        <RecentFeed />
      </Reveal>
    </div>
  );
}
