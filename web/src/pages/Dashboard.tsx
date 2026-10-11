// DASHBOARD — the landing screen: what is the gateway doing right now.
//
// One GET /admin/overview still paints the headline (health, per-account
// usage, per-surface usage, recent calls, quota), but the rich dimensions the
// backend has always served are fetched alongside it so they are ON SCREEN
// rather than one toggle away:
//
//   provider  -> already inside /admin/overview (usage is grouped by account)
//   model     -> /admin/usage?group_by=model
//   key       -> /admin/usage?group_by=key
//   day       -> /admin/usage?group_by=day  (the only time bucketing there is)
//
// Those are SQL aggregates over the ledger, not client-side sums of rows.
// Refresh cadence is deliberately split: overview is the fast pulse (15s, as
// before), the breakdowns are slower (60s) because a token total twiddling
// every 15 seconds is noise, not signal. The live call feed is SSE and costs
// nothing, so it does not need a timer at all.
//
// Day is the FINEST time bucket the ledger supports (substr(ts,1,10)), so a
// 24h window yields one or two points. That is why the trend panel asks for
// 7d — a two-dot "trend" is worse than no chart — and why the panel says so
// rather than implying a resolution the data does not have.
import { useCallback, useEffect, useMemo, useState } from 'react';
import { Area, AreaChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts';
import { get, usage, RANGE_LABEL, type Overview, type RangeKey, type UsageRow } from '../api';
import {
  BarList, CAT, ChartTooltip, CompositionBar, Panel, StatCard, VIZ,
  axisProps, catColor, chartGrid, cursorFill,
} from '../charts';
import { Reveal, Stagger, StaggerItem } from '../motion';
import {
  Chip, EmptyState, Mono, SectionLabel, SkeletonCards, SkeletonRows, StaleBadge,
  clockTime, cx, fmtDuration, fmtMs, fmtUptime, formatTokens, nfmt, pct,
} from '../ui';
import RecentFeed from './RecentFeed';

// Two ranges on purpose: 24h for the headline totals, 7d for the trend. The
// ledger cannot sub-divide a day, so the trend is only honest across several.
const TOTAL_RANGE: RangeKey = '24h';
const TREND_RANGE: RangeKey = '7d';

const HEARTBEAT_MS = 15_000; // overview
const SLOW_MS = 60_000;      // breakdowns

function statusTone(s: number): string {
  return s >= 500 ? 'text-bad' : s >= 400 ? 'text-warn' : 'text-ok';
}

// ── health strip ──────────────────────────────────────────────────────────
function HealthStrip({ o }: { o: Overview }) {
  const h = o.health;
  const items: Array<[string, string]> = [
    ['accounts', String(h.accounts)],
    ['models', String(h.models)],
    ['combos', String(h.combos)],
    ['ledger', nfmt(h.ledger_rows)],
    ['uptime', fmtUptime(h.uptime_s)],
  ];
  return (
    <div className="flex flex-wrap gap-2">
      {items.map(([k, v]) => (
        <Chip key={k} className="gap-2 px-2.5 py-1">
          <span className="text-mute">{k}</span> <Mono className="tnum text-[11px] text-ink">{v}</Mono>
        </Chip>
      ))}
      {/* The ledger is asynchronous, so a write that could not be recorded is
          a real accounting gap, not a cosmetic one — it means the call
          happened and its tokens are not counted anywhere. */}
      {h.dropped_rows > 0 && (
        <Chip tone="warn" className="gap-2 px-2.5 py-1"
          title="ledger rows shed under write load — the calls happened, their accounting did not land">
          dropped <Mono className="tnum text-[11px]">{nfmt(h.dropped_rows)}</Mono>
        </Chip>
      )}
      <Chip tone={h.status === 'ok' ? 'ok' : 'warn'} className="gap-1.5 px-2.5 py-1">
        <span className={cx('h-1.5 w-1.5 rounded-full', h.status === 'ok' ? 'bg-ok' : 'bg-warn')} />
        {h.status}
      </Chip>
    </div>
  );
}

// ── ranked breakdown panel ────────────────────────────────────────────────
function Breakdown({ label, rows, emptyHint }: { label: string; rows: UsageRow[]; emptyHint: string }) {
  const total = rows.reduce((a, r) => a + r.tin + r.tout + r.cread + r.cwrite, 0);
  return (
    <Panel
      label={<SectionLabel>{label}</SectionLabel>}
      aside={<span className="text-[11px] text-mute">{formatTokens(total)} total</span>}>
      <BarList
        rows={rows.map((r, i) => ({
          key: r.k,
          value: r.tin + r.tout + r.cread + r.cwrite,
          color: catColor(i),
          hint: `${nfmt(r.calls)} calls · p50 ttft ${fmtMs(r.p50_ttft_ms)}${r.errors > 0 ? ` · ${r.errors} errors` : ''}`,
        }))}
        emptyLabel={emptyHint}
        maxRows={6} />
    </Panel>
  );
}

// ── latency card ──────────────────────────────────────────────────────────
// TTFT only exists on a streaming call: a non-streaming request completes as
// one block and has no first token to time. Reporting 0 for those would be
// worse than reporting nothing, so the panel is explicit about which it is
// showing.
function LatencyCard({ rows }: { rows: UsageRow[] }) {
  const measured = rows.filter((r) => r.p50_ttft_ms > 0);
  if (measured.length === 0) {
    return (
      <Panel label={<SectionLabel>Latency · p50 time to first token</SectionLabel>}>
        <EmptyState
          title="No streaming calls measured yet"
          hint="Time-to-first-token only exists on a streaming request. Non-streaming calls are rendered as “–” everywhere, on purpose — 0 would be a lie." />
      </Panel>
    );
  }
  const worst = [...measured].sort((a, b) => b.p50_ttft_ms - a.p50_ttft_ms)[0];
  return (
    <Panel label={<SectionLabel>Latency · p50 time to first token</SectionLabel>}>
      <BarList
        rows={measured.map((r) => ({ key: r.k, value: r.p50_ttft_ms, color: VIZ.latency, hint: `${nfmt(r.calls)} calls` }))}
        format={fmtMs} maxRows={5} />
      <p className="mt-3 border-t border-line pt-3 text-[11.5px] leading-relaxed text-mute">
        Slowest first token: <span className="font-mono text-warn">{fmtMs(worst.p50_ttft_ms)}</span> on{' '}
        <span className="text-dim">{worst.k}</span>. Computed in SQL per group — the upper of the two middle
        values on an even count, the convention the backend has always used.
      </p>
    </Panel>
  );
}

// ── recent calls ──────────────────────────────────────────────────────────
function RecentCalls({ o }: { o: Overview }) {
  const rows = o.recent_calls ?? [];
  return (
    <Panel
      label={<SectionLabel>Recent calls</SectionLabel>}
      aside={<span className="text-[11px] text-mute">{rows.length} shown · newest first</span>}
      bodyClass="p-0">
      {rows.length === 0 ? (
        <EmptyState title="No calls yet"
          hint="Route something through a combo and it lands here — the live feed under it starts the moment it happens." />
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full min-w-[980px] text-[12.5px]">
            <thead>
              <tr className="border-b border-line text-left text-[10.5px] uppercase tracking-[0.1em] text-mute">
                <th className="px-5 py-2.5 font-medium">time</th>
                <th className="px-3 py-2.5 font-medium">client</th>
                <th className="px-3 py-2.5 font-medium">surface</th>
                <th className="px-3 py-2.5 font-medium">account</th>
                <th className="px-3 py-2.5 font-medium">model</th>
                <th className="px-3 py-2.5 text-right font-medium">status</th>
                <th className="px-3 py-2.5 text-right font-medium" title="time to first token — only meaningful on a stream">ttft</th>
                <th className="px-3 py-2.5 text-right font-medium">total</th>
                <th className="px-3 py-2.5 text-right font-medium">in</th>
                <th className="px-3 py-2.5 text-right font-medium">out</th>
                <th className="px-3 py-2.5 text-right font-medium">saved</th>
                <th className="px-5 py-2.5 text-right font-medium">compression</th>
              </tr>
            </thead>
            <tbody className="font-mono tnum">
              {rows.map((r, i) => (
                <tr key={i} className="border-b border-line/50 transition-colors last:border-0 hover:bg-wash">
                  <td className="whitespace-nowrap px-5 py-2 text-mute">{clockTime(r.ts)}</td>
                  <td className="px-3 py-2 text-dim">{r.client || '–'}</td>
                  <td className="px-3 py-2 text-dim">{r.surface}</td>
                  <td className="px-3 py-2 text-dim">{r.account}</td>
                  <td className="max-w-[220px] truncate px-3 py-2 font-sans text-ink" title={r.model}>{r.model}</td>
                  <td className={cx('px-3 py-2 text-right', statusTone(r.status))}>{r.status}</td>
                  <td className="px-3 py-2 text-right text-dim">{fmtMs(r.ttft_ms)}</td>
                  <td className="px-3 py-2 text-right text-dim">{fmtDuration(r.total_ms)}</td>
                  <td className="px-3 py-2 text-right text-dim">{nfmt(r.tin)}</td>
                  <td className="px-3 py-2 text-right text-dim">{nfmt(r.tout)}</td>
                  <td className="px-3 py-2 text-right">
                    {r.saved > 0 ? <span className="text-ok">−{formatTokens(r.saved)}</span> : <span className="text-mute">–</span>}
                  </td>
                  <td className="px-5 py-2 text-right font-sans">
                    {r.compression ? (
                      <span
                        title={r.applied ? `compressed by ${r.compression}` : `asked ${r.compression}, no change`}
                        className={cx('rounded px-1 py-0.5 font-mono text-[10px]',
                          r.applied ? 'bg-ok-dim text-ok' : 'bg-shade text-mute')}>
                        {r.compression === 'off' ? 'off' : r.compression}
                      </span>
                    ) : <span className="text-mute">–</span>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Panel>
  );
}

// ── quota ─────────────────────────────────────────────────────────────────
function QuotaBars({ quota }: { quota: NonNullable<Overview['accounts'][number]['quota']> }) {
  const rows: Array<[string, number]> = [];
  if (quota.percent_rolling != null) rows.push(['rolling', quota.percent_rolling]);
  if (quota.percent_weekly != null) rows.push(['weekly', quota.percent_weekly]);
  if (quota.percent_monthly != null) rows.push(['monthly', quota.percent_monthly]);
  if (rows.length > 0 && rows.every(([, v]) => typeof v === 'number')) {
    return (
      <div className="space-y-2">
        {rows.map(([k, v]) => (
          <div key={k} className="flex items-center gap-2.5">
            <span className="w-14 text-[10.5px] uppercase tracking-wide text-mute">{k}</span>
            <div className="h-1.5 min-w-16 flex-1 overflow-hidden rounded-full bg-shade">
              <div className={cx('h-full rounded-full', v >= 90 ? 'bg-bad' : v >= 70 ? 'bg-warn' : 'bg-ok')}
                style={{ width: `${Math.min(100, Math.max(0, v))}%` }} />
            </div>
            <span className="tnum w-10 text-right font-mono text-[11px] text-dim">{v}%</span>
          </div>
        ))}
      </div>
    );
  }
  if (quota.kind === 'money' && quota.balance != null) {
    return (
      <div className="font-mono text-[12px]">
        <span className="text-dim">{quota.balance}</span>
        {quota.unit ? <span className="ml-1 text-mute">{quota.unit}</span> : null}
        {quota.plan ? <div className="mt-0.5 text-[10.5px] text-mute">{quota.plan}</div> : null}
      </div>
    );
  }
  return <span className="text-mute">–</span>;
}

function stale(quota: { read_at?: string | null } | null | undefined): boolean {
  if (!quota?.read_at) return false;
  const t = Date.parse(quota.read_at);
  return !Number.isNaN(t) && Date.now() - t > 5 * 60 * 1000;
}

function QuotaPanel({ o }: { o: Overview }) {
  const accounts = o.accounts ?? [];
  return (
    <Panel label={<SectionLabel>Per-account quota</SectionLabel>}
      aside={<span className="text-[11px] text-mute">probe snapshots · {accounts.length} accounts</span>}
      bodyClass="p-0">
      {accounts.length === 0 ? (
        <EmptyState title="No accounts yet"
          hint="Add a provider and its key counts, models and quota snapshot appear here." />
      ) : (
        <div className="divide-y divide-line/60">
          {accounts.map((a) => (
            <div key={a.id} className="flex flex-wrap items-center gap-x-4 gap-y-2 px-5 py-3">
              <div className="w-44 shrink-0">
                <span className="text-[13px] font-medium">{a.name}</span>
                <Mono className="ml-2 hidden text-mute sm:inline">{a.namespace}</Mono>
              </div>
              <div className="min-w-40 flex-1">
                {!a.quota || a.quota.kind === 'none' || a.quota.kind === 'unknown'
                  ? <span className="text-[12.5px] text-mute">–</span>
                  : <QuotaBars quota={a.quota} />}
              </div>
              <div className="flex items-center gap-2">
                <Chip tone="neutral"><span className="font-mono">{a.quota?.kind ?? 'none'}</span></Chip>
                <StaleBadge stale={stale(a.quota)} />
              </div>
            </div>
          ))}
        </div>
      )}
    </Panel>
  );
}

// ── the dashboard ────────────────────────────────────────────────────────
export default function Dashboard() {
  const [o, setO] = useState<Overview | null>(null);
  const [byModel, setByModel] = useState<UsageRow[]>([]);
  const [byKey, setByKey] = useState<UsageRow[]>([]);
  const [trend, setTrend] = useState<UsageRow[]>([]);
  const [err, setErr] = useState<string | null>(null);

  // Overview is the fast pulse; the breakdowns refresh on their own cadence
  // (see SLOW_MS). Splitting the timers means a token total does not twiddle
  // every 15s on a tab left open, while the call feed still feels live.
  const loadFast = useCallback(async (silent: boolean) => {
    if (!silent) setErr(null);
    try {
      setO(await get<Overview>('/admin/overview'));
      setErr(null);
    } catch (e) {
      setErr(e instanceof Error ? e.message : 'failed to load');
    }
  }, []);

  const loadSlow = useCallback(async () => {
    // Three independent SQL aggregates in parallel. A failure in one must not
    // blank the other two, so each is caught separately and simply leaves its
    // own panel empty — an empty panel says "no data", which is true and
    // useful, whereas a thrown error would take the whole page down.
    const [m, k, d] = await Promise.all([
      usage('model', TOTAL_RANGE).catch(() => null),
      usage('key', TOTAL_RANGE).catch(() => null),
      usage('day', TREND_RANGE).catch(() => null),
    ]);
    if (m) setByModel(m.groups);
    if (k) setByKey(k.groups);
    if (d) setTrend(d.groups);
  }, []);

  useEffect(() => {
    void loadFast(false);
    void loadSlow();
    const fast = setInterval(() => void loadFast(true), HEARTBEAT_MS);
    const slow = setInterval(() => void loadSlow(), SLOW_MS);
    return () => { clearInterval(fast); clearInterval(slow); };
  }, [loadFast, loadSlow]);

  const series = useMemo(() => [...trend].reverse().map((r) => ({
    k: r.k,
    in: r.tin,
    out: r.tout,
  })), [trend]);
  const seriesIn = useMemo(() => series.map((s) => s.in), [series]);

  const totals = useMemo(() => {
    if (!o) return null;
    const sum = (f: (u: Overview['usage'][number]) => number) => o.usage.reduce((a, u) => a + f(u), 0);
    return {
      tin: sum((u) => u.tin), tout: sum((u) => u.tout), cread: sum((u) => u.cread), cwrite: sum((u) => u.cwrite),
      calls: sum((u) => u.calls), errors: sum((u) => u.errors),
      saved: sum((u) => u.saved), contextSaved: sum((u) => u.context_saved),
    };
  }, [o]);

  const byProvider = o?.usage ?? [];
  const bySurface = o?.usage_by_surface ?? [];

  if (err && !o) {
    return (
      <div className="rounded-xl border border-bad/30 bg-bad-dim px-5 py-4 text-[13px] text-bad">
        {err}
        <button onClick={() => void loadFast(false)} className="ml-3 underline underline-offset-2 hover:text-ink">retry</button>
      </div>
    );
  }

  if (!o || !totals) {
    return (
      <div className="space-y-4">
        <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-5"><SkeletonCards n={5} /></div>
        <div className="grid gap-4 xl:grid-cols-3"><SkeletonCards n={3} /></div>
        <div className="rounded-xl border border-line bg-surface p-5"><SkeletonRows rows={6} /></div>
      </div>
    );
  }

  const errRate = totals.calls > 0 ? (totals.errors / totals.calls) * 100 : 0;
  const compTotal = totals.tin + totals.tout + totals.cread;

  return (
    <Stagger className="space-y-5">
      <StaggerItem><HealthStrip o={o} /></StaggerItem>

      {/* Headline totals. Only tokens-in carries a sparkline: it is the one
          figure where the 7-day shape is the answer to "is this growing", and
          a sparkline on every card is decoration rather than information. */}
      <StaggerItem>
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-5">
          <StatCard label={`Calls · ${RANGE_LABEL[TOTAL_RANGE]}`} value={nfmt(totals.calls)} />
          <StatCard label={`Tokens in · ${RANGE_LABEL[TOTAL_RANGE]}`} value={formatTokens(totals.tin)}
            spark={seriesIn} sparkColor={VIZ.in}
            sub={series.length > 1 ? 'trailing 7 days' : undefined} />
          <StatCard label={`Tokens out · ${RANGE_LABEL[TOTAL_RANGE]}`} value={formatTokens(totals.tout)}
            sub={`${pct(totals.tout, totals.tin + totals.tout)} of volume`} />
          <StatCard label={`Cached read · ${RANGE_LABEL[TOTAL_RANGE]}`} value={formatTokens(totals.cread)}
            tone="text-accent" sub={`${pct(totals.cread, totals.tin)} of input served from cache`} />
          <StatCard label={`Errors · ${RANGE_LABEL[TOTAL_RANGE]}`} value={nfmt(totals.errors)}
            tone={totals.errors > 0 ? 'text-warn' : 'text-ok'}
            sub={totals.errors > 0 ? `${errRate.toFixed(errRate < 1 ? 2 : 1)}% of calls` : 'clean'} />
        </div>
      </StaggerItem>

      {/* Traffic over time. Day is the only bucketing there is, so this is
          deliberately a 7-day window: a 24h one is two dots. */}
      <StaggerItem>
        <Panel
          label={<SectionLabel>Traffic · last {RANGE_LABEL[TREND_RANGE]} by day</SectionLabel>}
          aside={<span className="text-[11px] text-mute">the ledger's finest bucket is a calendar day</span>}>
          {series.length === 0 ? (
            <EmptyState title="No traffic in the last 7 days"
              hint="Nothing has been routed through a combo in this window. The live feed below starts the moment it happens." />
          ) : (
            <>
              <div className="h-64">
                <ResponsiveContainer width="100%" height="100%">
                  <AreaChart data={series} margin={{ top: 4, right: 8, bottom: 0, left: -8 }}>
                    <defs>
                      <linearGradient id="dash-in" x1="0" y1="0" x2="0" y2="1">
                        <stop offset="0%" stopColor={VIZ.in} stopOpacity={0.3} />
                        <stop offset="100%" stopColor={VIZ.in} stopOpacity={0} />
                      </linearGradient>
                      <linearGradient id="dash-out" x1="0" y1="0" x2="0" y2="1">
                        <stop offset="0%" stopColor={VIZ.out} stopOpacity={0.28} />
                        <stop offset="100%" stopColor={VIZ.out} stopOpacity={0} />
                      </linearGradient>
                    </defs>
                    <CartesianGrid {...chartGrid} vertical={false} />
                    <XAxis dataKey="k" {...axisProps} />
                    <YAxis {...axisProps} width={58} tickFormatter={(v: number) => formatTokens(v)} />
                    <Tooltip content={<ChartTooltip />} cursor={cursorFill} />
                    <Area type="monotone" dataKey="in" name="tokens in" stroke={VIZ.in} strokeWidth={2}
                      fill="url(#dash-in)" dot={false} activeDot={{ r: 3, strokeWidth: 0 }} />
                    <Area type="monotone" dataKey="out" name="tokens out" stroke={VIZ.out} strokeWidth={2}
                      fill="url(#dash-out)" dot={false} activeDot={{ r: 3, strokeWidth: 0 }} />
                  </AreaChart>
                </ResponsiveContainer>
              </div>
              <p className="mt-2 text-[11px] text-mute">
                One point per calendar day — the ledger's finest grouping. Out sits inside in by design;
                both share the scale, so the gap between them is the ratio, not the difference.
              </p>
            </>
          )}
        </Panel>
      </StaggerItem>

      {/* The breakdown trio — the three dimensions that answer "who is
          spending", permanently visible rather than behind a toggle. */}
      <div className="grid gap-4 xl:grid-cols-3">
        <StaggerItem>
          <Breakdown label="Tokens by provider" rows={byProvider}
            emptyHint="No provider traffic in the last 24h." />
        </StaggerItem>
        <StaggerItem>
          <Breakdown label="Tokens by model" rows={byModel}
            emptyHint="No model traffic in the last 24h." />
        </StaggerItem>
        <StaggerItem>
          <Breakdown label="Tokens by API key" rows={byKey}
            emptyHint="No keyed calls in the last 24h — unkeyed traffic reports as “(none)”." />
        </StaggerItem>
      </div>

      {/* Composition, latency and surface, so the breakdown is not only
          per-entity: "what were the tokens" and "how fast did they arrive". */}
      <div className="grid gap-4 xl:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_minmax(0,0.9fr)]">
        <StaggerItem>
          <Panel label={<SectionLabel>Token composition · {RANGE_LABEL[TOTAL_RANGE]}</SectionLabel>}
            aside={<span className="text-[11px] text-mute">{formatTokens(compTotal)} total</span>}>
            <CompositionBar height={12} parts={[
              { key: 'tin', label: 'tokens in', value: totals.tin, color: VIZ.in },
              { key: 'tout', label: 'tokens out', value: totals.tout, color: VIZ.out },
              { key: 'cread', label: 'cached read', value: totals.cread, color: VIZ.cached },
            ]} />
            <ul className="mt-4 space-y-1.5">
              {[
                { key: 'tin', label: 'tokens in', value: totals.tin, color: VIZ.in },
                { key: 'tout', label: 'tokens out', value: totals.tout, color: VIZ.out },
                { key: 'cread', label: 'cached read', value: totals.cread, color: VIZ.cached },
              ].map((p) => (
                <li key={p.key} className="flex items-center gap-2 text-[12px]">
                  <span className="h-2 w-2 shrink-0 rounded-full" style={{ background: p.color }} />
                  <span className="text-dim">{p.label}</span>
                  <span className="tnum ml-auto font-mono text-ink">{formatTokens(p.value)}</span>
                  <span className="tnum w-12 text-right font-mono text-mute">{pct(p.value, compTotal)}</span>
                </li>
              ))}
              {totals.saved > 0 && (
                <li className="mt-2 flex items-center gap-2 border-t border-line pt-2 text-[12px]">
                  <span className="h-2 w-2 shrink-0 rounded-full bg-ok" />
                  <span className="text-dim">compression saved</span>
                  <span className="tnum ml-auto font-mono text-ok">−{formatTokens(totals.saved)}</span>
                  <span className="w-12 text-right font-mono text-mute">
                    {totals.contextSaved > 0 ? `+${formatTokens(totals.contextSaved)} ctx` : ''}
                  </span>
                </li>
              )}
            </ul>
          </Panel>
        </StaggerItem>

        <StaggerItem>
          <LatencyCard rows={byModel} />
        </StaggerItem>

        <StaggerItem>
          <Panel label={<SectionLabel>Calls by surface</SectionLabel>}
            aside={<span className="text-[11px] text-mute">{nfmt(bySurface.reduce((a, s) => a + s.calls, 0))} calls</span>}>
            <BarList
              rows={bySurface.map((s, i) => ({
                key: s.k, value: s.calls, color: CAT[i % CAT.length],
                hint: `${formatTokens(s.tin)} tokens in`,
              }))}
              format={nfmt}
              emptyLabel="No surface usage yet." />
            <p className="mt-3 border-t border-line pt-3 text-[11.5px] leading-relaxed text-mute">
              Which endpoint clients actually call. A surface with traffic but nothing pinned to it is worth
              a look — it means every call is paying for a protocol probe.
            </p>
          </Panel>
        </StaggerItem>
      </div>

      <Reveal><RecentCalls o={o} /></Reveal>
      <Reveal><QuotaPanel o={o} /></Reveal>

      {/* The live pulse. SSE, so it costs no timer and no request budget, and
          it is the only thing on this page that moves between refreshes.
          RecentFeed owns its own connection lifecycle; mounting it here adds
          the second consumer alongside the Analytics page. */}
      <Reveal>
        <RecentFeed />
      </Reveal>
    </Stagger>
  );
}
