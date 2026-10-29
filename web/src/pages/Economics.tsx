import { useRange } from '../utils/preferences';
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
import { useCallback, useEffect, useMemo, useRef, useState, type FormEvent } from 'react';
import { Link } from 'react-router-dom';
import { Area, AreaChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts';
import { fmtMoney, pctWhole } from '../utils/lib';
import {
  cost, normalizeFeedRow, requests, RANGE_HOURS, RANGE_LABEL,
  type CostRow, type CostTotals, type FeedRow, type RangeKey,
} from '../utils/api';
import {
  BarList, ChartTooltip, CompositionBar, Panel, StatCard, VIZ, axisProps, chartGrid, cursorFill,
} from '../components/charts';
import { Stagger, StaggerItem } from '../components/motion';
import {
  btn, cx, EmptyState, formatTokens, inputCls, nfmt, pct, SectionLabel, Segmented,
} from '../components/ui';

const RANGES: RangeKey[] = ['24h', '7d', '30d', '365d'];

const usd = (n: number) => fmtMoney(n);

function Metric({ label, value, note, tone }: { label: string; value: string; note?: string; tone?: string }) {
  return (
    <div className="rounded-xl border border-line bg-surface px-4 py-3.5">
      <SectionLabel>{label}</SectionLabel>
      <div className={cx('mt-1.5 font-mono text-[22px] font-semibold tracking-tight tnum', tone ?? 'text-ink')}>{value}</div>
      {note && <div className="mt-0.5 text-[12px] text-mute">{note}</div>}
    </div>
  );
}

export default function Economics() {
  const [range, setRange] = useRange();
  const [rows, setRows] = useState<FeedRow[]>([]);
  const [summary, setSummary] = useState<any>(null);
  const [costRows, setCostRows] = useState<CostRow[]>([]);
  const [costTotals, setCostTotals] = useState<CostTotals | null>(null);
  const [minInput, setMinInput] = useState('');
  const [appliedMin, setAppliedMin] = useState<number | undefined>();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [costError, setCostError] = useState(false);

  // Only the newest load may write state: an older selection's response that
  // lands late is dropped instead of overwriting the current one.
  const loadGen = useRef(0);
  const load = useCallback(async () => {
    const gen = ++loadGen.current;
    setLoading(true); setError('');
    try {
      // Token flow and money are two endpoints; fetched together so one range
      // control drives both. A cost failure must not blank the token view, so
      // it is caught separately and leaves the money section empty.
      const [r, c] = await Promise.all([
        requests({
          range,
          sort: 'tin_desc',
          minTin: appliedMin,
          limit: 100,
        }),
        cost(range).catch(() => null),
      ]);
      if (gen !== loadGen.current) return;
      setRows(r.rows.map(normalizeFeedRow));
      setSummary(r.summary);
      setCostError(c === null);
      setCostRows(c?.rows ?? []);
      setCostTotals(c?.totals ?? null);
    } catch (e) {
      if (gen === loadGen.current) setError(e instanceof Error ? e.message : 'could not load token economics');
    } finally {
      if (gen === loadGen.current) setLoading(false);
    }
  }, [range, appliedMin]);

  useEffect(() => { void load(); }, [load]);

  // ── derived from the token summary ──
  // The savings headline is applied_saved ONLY: the raw compression_saved
  // column sweeps legacy declined rows in (~21k tokens from before the
  // applied/notional split), and adding context_saved on top double-counts
  // the budget-stage component (budget attribution is a COMPONENT of total
  // reduction, not an additional total).
  const saved = summary?.applied_saved ?? 0;
  const input = summary?.tin ?? 0;
  const output = summary?.tout ?? 0;
  const cached = summary?.cread ?? 0;
  const cwrite = summary?.cwrite ?? 0;
  const reasoning = summary?.reasoning ?? 0;
  // Input composition must be DISJOINT — and the ledger already guarantees
  // that (store/ledger.go): tokens_in = COLD (non-cached) input, cread =
  // served from cache, cwrite = written to cache. The previous version
  // assumed the cache was a SUBSET of input and computed
  // cold = input - cached - cwrite, which double-subtracted it and clamped
  // cold to 0 for ANY window where caching existed — i.e. all of them.
  // inputTotal is the honest denominator: everything that crossed the wire.
  const cold = input;
  const inputTotal = input + cached + cwrite;
  const visibleOut = Math.max(0, output - reasoning);
  // The counterfactual: `input` is what was ACTUALLY sent cold
  // (post-compression) and cached reads happen either way, so the
  // pre-optimization estimate is input + saved — cold tokens only.
  // Approximate by construction — labeled as such wherever it is shown.
  const before = input + saved;
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

  // Models whose calls had no rate on their date: named, so the operator can
  // go and price them instead of reading a bare count. Whole cost set, same
  // reasoning as above.
  const unpricedModels = useMemo(() => {
    const m = new Map<string, { key: string; calls: number }>();
    for (const r of costRows) {
      if (r.unpriced_calls <= 0) continue;
      const key = `${r.account} · ${r.model}`;
      const cur = m.get(key) ?? { key, calls: 0 };
      cur.calls += r.unpriced_calls;
      m.set(key, cur);
    }
    return [...m.values()].sort((a, b) => b.calls - a.calls);
  }, [costRows]);

  // Run-rate: the window's priced spend spread over its days. The window is a
  // rolling look-back of exactly RANGE_HOURS, so the divisor is exact. Unpriced
  // calls contribute $0, which makes this (like the total) a floor.
  const windowDays = RANGE_HOURS[range] / 24;
  const perDay = costTotals ? costTotals.usd / windowDays : 0;

  const submitMin = (e: FormEvent) => {
    e.preventDefault();
    const raw = minInput.trim();
    const parsed = raw === '' ? undefined : Number(raw);
    if (parsed !== undefined && (!Number.isSafeInteger(parsed) || parsed < 0)) {
      setError('Minimum input tokens must be a non-negative whole number.'); return;
    }
    setError('');
    if (parsed === appliedMin) void load(); else setAppliedMin(parsed);
  };

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
      <form onSubmit={submitMin} className="rounded-xl border border-line bg-surface px-5 py-4">
        <SectionLabel>Large-request filter</SectionLabel>
        <div className="mt-3 flex flex-wrap items-end gap-3">
          <label className="text-[12px] text-mute">
            Minimum actual input tokens
            <input value={minInput} onChange={(e) => setMinInput(e.target.value)} inputMode="numeric"
              placeholder="all" className={cx(inputCls, 'mt-1.5 w-44 font-mono tnum')} />
          </label>
          <button type="submit" className={cx(btn.base, btn.primary)}>Analyze</button>
          <span className="text-[12px] text-mute">
            Sorted largest first. Use Requests for full bounds and per-row inspection.
          </span>
        </div>
      </form>

      {error && (
        <div role="alert" className="rounded-xl border border-bad/30 bg-bad-dim px-5 py-4 text-[13px] text-bad">{error}</div>
      )}

      {loading && !summary ? (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
          {Array.from({ length: 4 }).map((_, i) => <StatCard key={i} label="loading" value="…" />)}
        </div>
      ) : (
        <>
          {/* Four cards: the context-budget card was removed with the budget
              stage itself — a card whose value can only ever be 0 is a lie
              dressed as a stat. The compression card reads applied_saved
              (shipped rewrites only); context_saved is its own component and
              is never added into the headline. If a budget stage ever ships
              again it gets its OWN card, not an arithmetic resurrection. */}
          <Stagger className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
            <StaggerItem>
              <Metric label="Actual input" value={formatTokens(input)} note={`${nfmt(matching)} matching calls · cold, non-cached`} />
            </StaggerItem>
            <StaggerItem>
              <Metric label="Output" value={formatTokens(output)} note={`${pct(output, inputTotal + output)} of volume`} />
            </StaggerItem>
            <StaggerItem>
              <Metric label="Cached read" value={formatTokens(cached)} note={`${pct(cached, inputTotal)} of input re-read from cache`} />
            </StaggerItem>
            <StaggerItem>
              <Metric label="Compression saved" value={formatTokens(saved)}
                tone={saved > 0 ? 'text-ok' : undefined}
                note={`${saved > 0 ? pctWhole(saved, before) : '0%'} approx. reduction (shipped rewrites)`} />
            </StaggerItem>
          </Stagger>

          <div className="grid gap-4 lg:grid-cols-[minmax(0,1.2fr)_minmax(0,1fr)]">
            <Panel label={<SectionLabel>What the tokens were · {RANGE_LABEL[range]}</SectionLabel>}
              aside={<span className="text-[12px] text-mute">{formatTokens(before)} before optimisation</span>}>
              {/* Input composition (disjoint): the ledger's three counters
                  are already exclusive — tokens_in is cold input, so the
                  three parts sum to inputTotal (cold + read + write), not to
                  the cold column. */}
              <div className="mb-1 text-[12px] text-mute">Input composition</div>
              <CompositionBar height={12} parts={[
                { key: 'cold', label: 'Cold input', value: cold, color: VIZ.in },
                ...(cached > 0 ? [{ key: 'cached', label: 'Cached read', value: cached, color: VIZ.cached }] : []),
                ...(cwrite > 0 ? [{ key: 'cwrite', label: 'Cached write', value: cwrite, color: VIZ.budget }] : []),
              ]} />
              <ul className="mt-3 space-y-1.5">
                {[
                  { key: 'cold', label: 'Cold input', value: cold, color: VIZ.in },
                  ...(cached > 0 ? [{ key: 'cached', label: 'Cached read', value: cached, color: VIZ.cached }] : []),
                  ...(cwrite > 0 ? [{ key: 'cwrite', label: 'Cached write', value: cwrite, color: VIZ.budget }] : []),
                ].map((p) => (
                  <li key={p.key} className="flex items-center gap-2 text-[13px]">
                    <span className="h-2 w-2 shrink-0 rounded-full" style={{ background: p.color }} />
                    <span className="text-dim">{p.label}</span>
                    <span className="tnum ml-auto font-mono text-ink">{formatTokens(p.value)}</span>
                    <span className="tnum w-12 text-right font-mono text-mute">{pct(p.value, inputTotal)}</span>
                  </li>
                ))}
              </ul>
              {/* Output composition (disjoint): reasoning is a SUBSET of
                  output — visible output is the remainder. */}
              <div className="mb-1 mt-5 text-[12px] text-mute">Output composition</div>
              <CompositionBar height={12} parts={[
                { key: 'vis', label: 'Visible output', value: visibleOut, color: VIZ.out },
                { key: 'reason', label: 'Reasoning', value: reasoning, color: VIZ.reasoning },
              ]} />
              <ul className="mt-3 space-y-1.5">
                {[
                  { key: 'vis', label: 'Visible output', value: visibleOut, color: VIZ.out },
                  { key: 'reason', label: 'Reasoning', value: reasoning, color: VIZ.reasoning },
                ].map((p) => (
                  <li key={p.key} className="flex items-center gap-2 text-[13px]">
                    <span className="h-2 w-2 shrink-0 rounded-full" style={{ background: p.color }} />
                    <span className="text-dim">{p.label}</span>
                    <span className="tnum ml-auto font-mono text-ink">{formatTokens(p.value)}</span>
                    <span className="tnum w-12 text-right font-mono text-mute">{pct(p.value, output)}</span>
                  </li>
                ))}
              </ul>
              {saved > 0 && (
                <div className="mt-3 border-t border-line pt-3">
                  <div className="flex items-center gap-2 text-[13px]">
                    <span className="h-2 w-2 shrink-0 rounded-full bg-ok" />
                    <span className="text-dim">Removed before forwarding</span>
                    <span className="tnum ml-auto font-mono text-ok">−{formatTokens(saved)}</span>
                    <span className="w-12 text-right font-mono text-mute">{pctWhole(saved, before)}</span>
                  </div>
                </div>
              )}
            </Panel>

            <Panel label={<SectionLabel>Reasoning · the tokens you never see</SectionLabel>}>
              <Metric label="Reasoning output" value={formatTokens(reasoning)}
                tone={reasoning > 0 ? 'text-warn' : undefined}
                note={`${pct(reasoning, output)} of tokens out`} />
              <p className="mt-3 text-[13px] leading-relaxed text-dim">
                Reasoning tokens are billed as output and never rendered in the reply. This is the single
                number that explains an invoice the call count does not: a short answer from a thinking
                model can cost more than a long answer from a fast one.
              </p>
              <div className="mt-4 border-t border-line pt-4">
                <Metric label="Estimated pre-optimization" value={formatTokens(before)}
                  note="approx. — what would have shipped without ezllm" />
                <p className="mt-3 text-[13px] leading-relaxed text-dim">
                  Tokens in plus everything compression removed before forwarding:
                  the prompt your clients sent, as they sent it.
                </p>
              </div>
            </Panel>
          </div>

          {/* ── money ── */}
          <Panel
            label={<SectionLabel>Spend (derived from model_prices)</SectionLabel>}
            aside={
              <>
                {appliedMin !== undefined && (
                  <span className="text-[12px] text-warn" title="The min-input filter scopes the token view above; the spend rows are the whole range.">
                    min-input filter: tokens only — spend is the whole range
                  </span>
                )}
                {costTotals && costTotals.unpriced_calls > 0 ? (
                  <span className="text-[12px] text-warn">
                    {nfmt(costTotals.unpriced_calls)} calls unpriced
                  </span>
                ) : costTotals?.pricing_complete && costRows.length > 0 ? (
                  <span className="text-[12px] text-ok">all calls priced</span>
                ) : null}
              </>
            }>
            {costError ? (
              <p role="alert" className="text-sm text-warn">Could not load the cost report. <button type="button" className="underline" onClick={() => void load()}>Retry cost report</button></p>
            ) : costRows.length === 0 ? (
              <EmptyState title="No priced traffic yet"
                hint="Cost is computed from the rates entered under Providers → account → Pricing. Until a rate exists for an account+model, those calls cost 0." />
            ) : (
              <div className="space-y-5">
                <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
                  <Metric label="Spend" value={usd(costTotals?.usd ?? 0)} note={`${nfmt(costTotals?.calls ?? 0)} calls`} />
                  <Metric label="Per day" value={usd(perDay)}
                    note={`${usd(perDay * 30)} over 30 days at this rate${costTotals && costTotals.unpriced_calls > 0 ? ' (a floor: unpriced calls count $0)' : ''}`} />
                  <Metric label="Priced" value={nfmt((costTotals?.calls ?? 0) - (costTotals?.unpriced_calls ?? 0))}
                    note="calls with a rate effective on their date" />
                  <Metric label="Unpriced" value={nfmt(costTotals?.unpriced_calls ?? 0)}
                    tone={costTotals && costTotals.unpriced_calls > 0 ? 'text-warn' : undefined}
                    note="contributing $0" />
                </div>

                {unpricedModels.length > 0 && (
                  <div className="rounded-lg border border-warn/30 bg-warn-dim px-4 py-3">
                    <div className="flex flex-wrap items-baseline justify-between gap-2">
                      <span className="text-[13px] text-ink">Counted as $0 until priced</span>
                      <Link to="/providers" className="text-[13px] text-dim underline underline-offset-2 hover:text-ink"
                        title="Open the provider, then Pricing, and enter a rate for the model">Add a price in Providers</Link>
                    </div>
                    <ul className="mt-2 grid gap-x-6 gap-y-1 text-[13px] sm:grid-cols-2">
                      {unpricedModels.slice(0, 8).map((u) => (
                        <li key={u.key} className="flex items-baseline gap-2">
                          <span className="min-w-0 flex-1 truncate text-dim" title={u.key}>{u.key}</span>
                          <span className="tnum text-warn">{nfmt(u.calls)} calls</span>
                        </li>
                      ))}
                    </ul>
                    {unpricedModels.length > 8 && <p className="mt-1.5 text-[12px] text-mute">+{unpricedModels.length - 8} more</p>}
                  </div>
                )}

                {byDay.length > 1 && (
                  <div>
                    <div className="mb-2 flex items-baseline justify-between">
                      <SectionLabel>Spend by day</SectionLabel>
                      <span className="text-[12px] text-mute">re-grouped from the priced rows below</span>
                    </div>
                    <div className="h-48">
                      <ResponsiveContainer width="100%" height="100%">
                        <AreaChart data={byDay} margin={{ top: 4, right: 8, bottom: 0, left: -8 }}>
                          <CartesianGrid {...chartGrid} vertical={false} />
                          <XAxis dataKey="day" {...axisProps} />
                          <YAxis {...axisProps} width={88} tickFormatter={(v: number) => usd(v)} />
                          <Tooltip content={<ChartTooltip format={(n) => usd(n)} />} cursor={cursorFill} />
                          <Area type="linear" dataKey="usd" name="spend" stroke={VIZ.in} strokeWidth={2}
                            fill={VIZ.in} fillOpacity={0.08} dot={false} activeDot={{ r: 3, strokeWidth: 0 }} />
                        </AreaChart>
                      </ResponsiveContainer>
                    </div>
                  </div>
                )}

                <div className="grid gap-5 lg:grid-cols-2">
                  <div className="min-w-0">
                    <SectionLabel>Most expensive · provider + model</SectionLabel>
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
                      <table className="w-full min-w-[620px] text-[13px]">
                        <thead className="sticky top-0 bg-surface">
                          <tr className="border-b border-line text-left text-[11px] text-mute">
                            <th className="px-4 py-2 font-medium">Day</th>
                            <th className="px-3 py-2 font-medium">Provider</th>
                            <th className="px-3 py-2 font-medium">Model</th>
                            <th className="px-3 py-2 text-right font-medium">Calls</th>
                            <th className="px-3 py-2 text-right font-medium">In / cached</th>
                            <th className="px-3 py-2 text-right font-medium">Out</th>
                            <th className="px-4 py-2 text-right font-medium">USD</th>
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
                    <p className="mt-2 text-[12px] leading-relaxed text-mute">
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
            aside={<span className="text-[12px] text-mute">top {nfmt(rows.length)} of {nfmt(matching)} by actual input, largest first</span>}
            bodyClass="p-0">
            {rows.length === 0 ? (
              <EmptyState title="No matching calls in this range"
                hint="Raise the minimum input or widen the range. This is where the outliers answer whether the client is sending too much history, or the model is burning it on output." />
            ) : (
              <div className="overflow-x-auto">
                <table className="w-full min-w-[1000px] text-[13px]">
                  <thead>
                    <tr className="border-b border-line text-left text-[11px] text-mute">
                      <th className="px-5 py-2.5 font-medium">When</th>
                      <th className="px-3 py-2.5 font-medium">Model</th>
                      <th className="px-3 py-2.5 font-medium">Client</th>
                      <th className="px-3 py-2.5 text-right font-medium">Actual in</th>
                      <th className="px-3 py-2.5 text-right font-medium">Compression</th>
                      <th className="px-3 py-2.5 text-right font-medium">Out</th>
                      <th className="px-3 py-2.5 text-right font-medium" title="tokens the model spent thinking — billed as output, never shown">Reasoning</th>
                      <th className="px-3 py-2.5 text-right font-medium">Status</th>
                      <th className="px-5 py-2.5 text-right font-medium"><span className="sr-only">Open</span></th>
                    </tr>
                  </thead>
                  <tbody className="font-mono tnum">
                    {rows.map((r, i) => (
                      <tr key={`${r.ts}-${i}`} className="border-b border-line/50 transition-colors last:border-0 hover:bg-wash">
                        <td className="whitespace-nowrap px-5 py-2 text-mute" title={r.ts}>
                          {new Date(r.ts).toLocaleString('en-GB', { day: '2-digit', month: 'short', hour: '2-digit', minute: '2-digit', hour12: false })}
                        </td>
                        <td className="max-w-[300px] px-3 py-2 font-sans">
                          <div className="truncate text-ink" title={r.model}>{r.model}</div>
                          {r.account && <div className="truncate text-[11px] text-mute" title={[r.account, r.alias].filter(Boolean).join(' · ')}>{r.account}{r.alias ? ` · ${r.alias}` : ''}</div>}
                        </td>
                        <td className="px-3 py-2 font-sans text-dim">{r.client || '–'}</td>
                        <td className="px-3 py-2 text-right text-ink">{nfmt(r.tin)}</td>
                        <td className="px-3 py-2 text-right text-ok">{formatTokens(r.saved)}</td>
                        <td className="px-3 py-2 text-right text-dim">{nfmt(r.tout)}</td>
                        <td className={cx('px-3 py-2 text-right', (r.reasoning ?? 0) > 0 ? 'text-warn' : 'text-mute')}>
                          {nfmt(r.reasoning ?? 0)}
                        </td>
                        <td className={cx('px-3 py-2 text-right', r.status >= 400 ? 'text-warn' : 'text-dim')}>{r.status}</td>
                        <td className="px-5 py-2 text-right font-sans">
                          <Link to={`/requests?${new URLSearchParams({ account: r.account, model: r.model, range })}`}
                            className="text-dim underline underline-offset-2 hover:text-ink" title="open this provider and model in Requests">Requests</Link>
                        </td>
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
