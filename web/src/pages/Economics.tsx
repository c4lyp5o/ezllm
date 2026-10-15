// TOKEN ECONOMICS — what arrived, what was cached, what was removed, what it
// cost. Two endpoints, one range control:
//
//   /admin/requests  -> per-call token dissection. Its `summary` covers the
//                       WHOLE matching set, not just the returned page.
//   /admin/cost      -> derived money from model_prices, per day+account+model.
//
// The honesty rule that governs this page: `usd` counts PRICED calls only. An
// unpriced call contributes 0, so the total is a FLOOR, not the bill — and the
// page says so with the count behind it rather than quietly showing a number
// that looks complete. Rates are versioned by effective_from, so a price change
// does not retroactively rewrite history, which is also why "unpriced" is a
// per-row fact and not a global one.
import { useCallback, useEffect, useMemo, useState } from 'react';
import { Area, AreaChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts';
import {
  cost, normalizeFeedRow, requests, RANGE_LABEL,
  type CostRow, type CostTotals, type FeedRow, type RangeKey,
} from '../utils/api';
import {
  BarList, ChartTooltip, CompositionBar, Panel, StatCard, VIZ, axisProps, chartGrid, cursorFill,
} from '../components/charts';
import {
  btn, cx, EmptyState, formatTokens, inputCls, nfmt, pct, SectionLabel, Segmented,
} from '../components/ui';

const RANGES: RangeKey[] = ['24h', '7d', '30d', '365d'];

const usd = (n: number) => `$${n.toFixed(n >= 100 ? 2 : 4)}`;

function Metric({ label, value, note, tone }: { label: string; value: string; note?: string; tone?: string }) {
  return (
    <div className="rounded-xl border border-line bg-surface px-4 py-3.5">
      <SectionLabel>{label}</SectionLabel>
      <div className={cx('mt-1.5 font-mono text-[22px] font-semibold tracking-tight tnum', tone ?? 'text-ink')}>{value}</div>
      {note && <div className="mt-0.5 text-[11px] text-mute">{note}</div>}
    </div>
  );
}

export default function Economics() {
  const [range, setRange] = useState<RangeKey>('24h');
  const [rows, setRows] = useState<FeedRow[]>([]);
  const [summary, setSummary] = useState<any>(null);
  const [costRows, setCostRows] = useState<CostRow[]>([]);
  const [costTotals, setCostTotals] = useState<CostTotals | null>(null);
  const [minInput, setMinInput] = useState('');
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');

  const load = useCallback(async () => {
    setLoading(true); setError('');
    try {
      // Token flow and money are two endpoints; fetched together so one range
      // control drives both. A cost failure must not blank the token view, so
      // it is caught separately and leaves the money section empty.
      const [r, c] = await Promise.all([
        requests({
          range,
          sort: 'tin_desc',
          minTin: minInput ? Number(minInput) : undefined,
          limit: 100,
        }),
        cost(range).catch(() => null),
      ]);
      setRows(r.rows.map(normalizeFeedRow));
      setSummary(r.summary);
      setCostRows(c?.rows ?? []);
      setCostTotals(c?.totals ?? null);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'could not load token economics');
    } finally {
      setLoading(false);
    }
  }, [range, minInput]);

  useEffect(() => { void load(); }, [load]);

  // ── derived from the token summary ──
  const context = summary?.context_saved ?? 0;
  const compression = summary?.compression_saved ?? 0;
  const input = summary?.tin ?? 0;
  const output = summary?.tout ?? 0;
  const cached = summary?.cread ?? 0;
  const reasoning = summary?.reasoning ?? 0;
  const saved = context + compression;
  const before = input + cached + saved;
  const matching = summary?.count ?? rows.length;

  // ── derived from the cost rows ──
  // /admin/cost returns rows ALREADY grouped by (day, account, model) and has
  // no limit and no pagination. Re-grouping that complete set into per-day and
  // per-model totals here is not "summing a page", which is what the repo's
  // own dashboard rule forbids — it forbids summing a truncated row set. This
  // is the whole set.
  const byDay = useMemo(() => {
    const m = new Map<string, { day: string; usd: number; calls: number }>();
    for (const r of costRows) {
      const cur = m.get(r.day) ?? { day: r.day, usd: 0, calls: 0 };
      cur.usd += r.usd;
      cur.calls += r.calls;
      m.set(r.day, cur);
    }
    return [...m.values()].sort((a, b) => a.day.localeCompare(b.day));
  }, [costRows]);

  const byModelCost = useMemo(() => {
    const m = new Map<string, { key: string; usd: number; calls: number; unpriced: number }>();
    for (const r of costRows) {
      const key = `${r.account} · ${r.model}`;
      const cur = m.get(key) ?? { key, usd: 0, calls: 0, unpriced: 0 };
      cur.usd += r.usd;
      cur.calls += r.calls;
      cur.unpriced += r.unpriced_calls;
      m.set(key, cur);
    }
    return [...m.values()].sort((a, b) => b.usd - a.usd);
  }, [costRows]);

  return (
    <div className="space-y-5">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <Segmented label="Time range"
          options={RANGES.map((r) => ({ key: r, label: RANGE_LABEL[r] }))}
          value={range} onChange={setRange} />
      </div>

      {/* Large-request filter. It commits on Analyze rather than on change: a
          six-digit number fires six requests as it is typed. */}
      <section className="rounded-xl border border-line bg-surface px-5 py-4">
        <SectionLabel>large-request filter</SectionLabel>
        <div className="mt-3 flex flex-wrap items-end gap-3">
          <label className="text-[11px] uppercase tracking-[0.08em] text-mute">
            Minimum actual input tokens
            <input value={minInput} onChange={(e) => setMinInput(e.target.value)} inputMode="numeric"
              placeholder="all" className={cx(inputCls, 'mt-1.5 w-44 font-mono tnum')} />
          </label>
          <button onClick={() => void load()} className={cx(btn.base, btn.primary)}>Analyze</button>
          <span className="text-[11px] text-mute">
            Sorted largest first. Use Requests for full bounds and per-row inspection.
          </span>
        </div>
      </section>

      {error && (
        <div role="alert" className="rounded-xl border border-bad/30 bg-bad-dim px-5 py-4 text-[13px] text-bad">{error}</div>
      )}

      {loading && !summary ? (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-5">
          {Array.from({ length: 5 }).map((_, i) => <StatCard key={i} label="loading" value="…" />)}
        </div>
      ) : (
        <>
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-5">
            <Metric label="actual input" value={formatTokens(input)} note={`${nfmt(matching)} matching calls`} />
            <Metric label="output" value={formatTokens(output)} note={`${pct(output, input + output)} of volume`} />
            <Metric label="cached read" value={formatTokens(cached)} note={`${pct(cached, input)} of input re-read from cache`} />
            <Metric label="context saved" value={formatTokens(context)} note="context-budget stage" />
            <Metric label="compression saved" value={formatTokens(compression)}
              tone={saved > 0 ? 'text-ok' : undefined}
              note={`${saved > 0 ? pct(saved, before) : '0%'} estimated reduction`} />
          </div>

          <div className="grid gap-4 lg:grid-cols-[minmax(0,1.2fr)_minmax(0,1fr)]">
            <Panel label={<SectionLabel>What the tokens were · {RANGE_LABEL[range]}</SectionLabel>}
              aside={<span className="text-[11px] text-mute">{formatTokens(before)} before optimisation</span>}>
              <CompositionBar height={12} parts={[
                { key: 'in', label: 'actual input', value: input, color: VIZ.in },
                { key: 'cached', label: 'cached read', value: cached, color: VIZ.cached },
                { key: 'out', label: 'output', value: output, color: VIZ.out },
                { key: 'reason', label: 'reasoning', value: reasoning, color: VIZ.reasoning },
              ]} />
              <ul className="mt-4 space-y-1.5">
                {[
                  { key: 'in', label: 'actual input', value: input, color: VIZ.in },
                  { key: 'cached', label: 'cached read', value: cached, color: VIZ.cached },
                  { key: 'out', label: 'output', value: output, color: VIZ.out },
                  { key: 'reason', label: 'reasoning', value: reasoning, color: VIZ.reasoning },
                ].map((p) => (
                  <li key={p.key} className="flex items-center gap-2 text-[12px]">
                    <span className="h-2 w-2 shrink-0 rounded-full" style={{ background: p.color }} />
                    <span className="text-dim">{p.label}</span>
                    <span className="tnum ml-auto font-mono text-ink">{formatTokens(p.value)}</span>
                    <span className="tnum w-12 text-right font-mono text-mute">{pct(p.value, before)}</span>
                  </li>
                ))}
              </ul>
              {saved > 0 && (
                <div className="mt-3 border-t border-line pt-3">
                  <div className="flex items-center gap-2 text-[12px]">
                    <span className="h-2 w-2 shrink-0 rounded-full bg-ok" />
                    <span className="text-dim">removed before forwarding</span>
                    <span className="tnum ml-auto font-mono text-ok">−{formatTokens(saved)}</span>
                    <span className="w-12 text-right font-mono text-mute">{pct(saved, before)}</span>
                  </div>
                  <p className="mt-2 text-[11.5px] leading-relaxed text-mute">
                    {formatTokens(compression)} from compression stages and {formatTokens(context)} from the
                    context budget — both are what actually shipped shorter. A stage that DECLINED is not
                    counted here; that is the "held" figure on the Compression page.
                  </p>
                </div>
              )}
            </Panel>

            <Panel label={<SectionLabel>Reasoning · the tokens you never see</SectionLabel>}>
              <Metric label="reasoning output" value={formatTokens(reasoning)}
                tone={reasoning > 0 ? 'text-warn' : undefined}
                note={`${pct(reasoning, output)} of tokens out`} />
              <p className="mt-3 text-[12px] leading-relaxed text-dim">
                Reasoning tokens are billed as output and never rendered in the reply. This is the single
                number that explains an invoice the call count does not: a short answer from a thinking
                model can cost more than a long answer from a fast one.
              </p>
              <div className="mt-4 border-t border-line pt-4">
                <Metric label="estimated pre-optimization" value={formatTokens(before)}
                  note="what would have shipped without ezllm" />
              </div>
            </Panel>
          </div>

          {/* ── money ── */}
          <Panel
            label={<SectionLabel>Spend (derived from model_prices)</SectionLabel>}
            aside={
              costTotals && costTotals.unpriced_calls > 0 ? (
                <span className="text-[11px] text-warn">
                  {nfmt(costTotals.unpriced_calls)} calls unpriced — the total is a floor, not the bill
                </span>
              ) : costTotals?.pricing_complete && costRows.length > 0 ? (
                <span className="text-[11px] text-ok">all calls priced</span>
              ) : null
            }>
            {costRows.length === 0 ? (
              <EmptyState title="No priced traffic yet"
                hint="Cost is computed from the rates entered under Providers → account → Pricing. Until a rate exists for an account+model, those calls cost 0 and the total above is a floor rather than a bill." />
            ) : (
              <div className="space-y-5">
                <div className="grid gap-3 sm:grid-cols-3">
                  <Metric label="spend" value={usd(costTotals?.usd ?? 0)} note={`${nfmt(costTotals?.calls ?? 0)} calls`} />
                  <Metric label="priced" value={nfmt((costTotals?.calls ?? 0) - (costTotals?.unpriced_calls ?? 0))}
                    note="calls with a rate effective on their date" />
                  <Metric label="unpriced" value={nfmt(costTotals?.unpriced_calls ?? 0)}
                    tone={costTotals && costTotals.unpriced_calls > 0 ? 'text-warn' : undefined}
                    note="contributing $0 — a floor, not a bill" />
                </div>

                {byDay.length > 1 && (
                  <div>
                    <div className="mb-2 flex items-baseline justify-between">
                      <SectionLabel>Spend by day</SectionLabel>
                      <span className="text-[11px] text-mute">re-grouped from the priced rows below</span>
                    </div>
                    <div className="h-48">
                      <ResponsiveContainer width="100%" height="100%">
                        <AreaChart data={byDay} margin={{ top: 4, right: 8, bottom: 0, left: -8 }}>
                          <defs>
                            <linearGradient id="econ-spend" x1="0" y1="0" x2="0" y2="1">
                              <stop offset="0%" stopColor={VIZ.saved} stopOpacity={0.3} />
                              <stop offset="100%" stopColor={VIZ.saved} stopOpacity={0} />
                            </linearGradient>
                          </defs>
                          <CartesianGrid {...chartGrid} vertical={false} />
                          <XAxis dataKey="day" {...axisProps} />
                          <YAxis {...axisProps} width={58} tickFormatter={(v: number) => usd(v)} />
                          <Tooltip content={<ChartTooltip format={(n) => usd(n)} />} cursor={cursorFill} />
                          <Area type="monotone" dataKey="usd" name="spend" stroke={VIZ.saved} strokeWidth={2}
                            fill="url(#econ-spend)" dot={false} activeDot={{ r: 3, strokeWidth: 0 }} />
                        </AreaChart>
                      </ResponsiveContainer>
                    </div>
                  </div>
                )}

                <div className="grid gap-5 lg:grid-cols-2">
                  <div className="min-w-0">
                    <SectionLabel>Most expensive · account + model</SectionLabel>
                    <div className="mt-3">
                      <BarList
                        rows={byModelCost.slice(0, 8).map((r) => ({
                          key: r.key,
                          value: r.usd,
                          color: VIZ.saved,
                          hint: `${nfmt(r.calls)} calls${r.unpriced > 0 ? ` · ${nfmt(r.unpriced)} unpriced` : ''}`,
                        }))}
                        format={(n) => usd(n)}
                        emptyLabel="No priced traffic yet." />
                    </div>
                  </div>

                  <div className="min-w-0">
                    <SectionLabel>Every priced row</SectionLabel>
                    <div className="mt-3 max-h-80 overflow-y-auto rounded-lg border border-line">
                      <table className="w-full min-w-[620px] text-[12px]">
                        <thead className="sticky top-0 bg-surface">
                          <tr className="border-b border-line text-left text-[10.5px] uppercase tracking-[0.1em] text-mute">
                            <th className="px-4 py-2 font-medium">day</th>
                            <th className="px-3 py-2 font-medium">account</th>
                            <th className="px-3 py-2 font-medium">model</th>
                            <th className="px-3 py-2 text-right font-medium">calls</th>
                            <th className="px-3 py-2 text-right font-medium">in / cached</th>
                            <th className="px-3 py-2 text-right font-medium">out</th>
                            <th className="px-4 py-2 text-right font-medium">usd</th>
                          </tr>
                        </thead>
                        <tbody className="font-mono tnum">
                          {costRows.map((r, i) => (
                            <tr key={`${r.day}-${r.account_id}-${r.model}-${i}`}
                              className="border-b border-line/50 transition-colors last:border-0 hover:bg-wash">
                              <td className="px-4 py-2 text-dim">{r.day}</td>
                              <td className="px-3 py-2 font-sans text-ink">{r.account}</td>
                              <td className="max-w-[200px] truncate px-3 py-2 font-sans text-ink" title={r.model}>{r.model}</td>
                              <td className="px-3 py-2 text-right text-dim">
                                {nfmt(r.calls)}
                                {r.unpriced_calls > 0 && <span className="text-warn"> *{nfmt(r.unpriced_calls)}</span>}
                              </td>
                              <td className="px-3 py-2 text-right text-dim">
                                {formatTokens(r.tokens_in)} / {formatTokens(r.tokens_cached_read)}
                              </td>
                              <td className="px-3 py-2 text-right text-dim">{formatTokens(r.tokens_out)}</td>
                              <td className="px-4 py-2 text-right text-ink">{usd(r.usd)}</td>
                            </tr>
                          ))}
                        </tbody>
                      </table>
                    </div>
                    <p className="mt-2 text-[11px] leading-relaxed text-mute">
                      * rows with unpriced calls cost 0 until a rate exists for that account+model on that
                      date. Rates are versioned by effective_from, so changing a price does not
                      retroactively rewrite what past calls cost.
                    </p>
                  </div>
                </div>
              </div>
            )}
          </Panel>

          <Panel label={<SectionLabel>Largest requests · {RANGE_LABEL[range]}</SectionLabel>}
            aside={<span className="text-[11px] text-mute">sorted by actual input, largest first</span>}
            bodyClass="p-0">
            {rows.length === 0 ? (
              <EmptyState title="No matching calls in this range"
                hint="Raise the minimum input or widen the range. This is where the outliers answer whether the client is sending too much history, or the model is burning it on output." />
            ) : (
              <div className="overflow-x-auto">
                <table className="w-full min-w-[820px] text-[12px]">
                  <thead>
                    <tr className="border-b border-line text-left text-[10.5px] uppercase tracking-[0.1em] text-mute">
                      <th className="px-5 py-2.5 font-medium">model</th>
                      <th className="px-3 py-2.5 text-right font-medium">actual in</th>
                      <th className="px-3 py-2.5 text-right font-medium" title="tokens the context-budget stage kept out of the prompt">context saved</th>
                      <th className="px-3 py-2.5 text-right font-medium">compression</th>
                      <th className="px-3 py-2.5 text-right font-medium">out</th>
                      <th className="px-3 py-2.5 text-right font-medium" title="tokens the model spent thinking — billed as output, never shown">reasoning</th>
                      <th className="px-5 py-2.5 text-right font-medium">status</th>
                    </tr>
                  </thead>
                  <tbody className="font-mono tnum">
                    {rows.map((r, i) => (
                      <tr key={`${r.ts}-${i}`} className="border-b border-line/50 transition-colors last:border-0 hover:bg-wash">
                        <td className="max-w-[300px] truncate px-5 py-2 font-sans text-ink" title={r.model}>{r.model}</td>
                        <td className="px-3 py-2 text-right text-ink">{nfmt(r.tin)}</td>
                        <td className="px-3 py-2 text-right text-accent">{formatTokens(r.contextSaved ?? 0)}</td>
                        <td className="px-3 py-2 text-right text-ok">{formatTokens(r.saved)}</td>
                        <td className="px-3 py-2 text-right text-dim">{nfmt(r.tout)}</td>
                        <td className={cx('px-3 py-2 text-right', (r.reasoning ?? 0) > 0 ? 'text-warn' : 'text-mute')}>
                          {nfmt(r.reasoning ?? 0)}
                        </td>
                        <td className="px-5 py-2 text-right text-dim">{r.status}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </Panel>
        </>
      )}
    </div>
  );
}
