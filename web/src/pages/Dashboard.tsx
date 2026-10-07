// DASHBOARD — one GET /admin/overview paints everything.
import { useCallback, useEffect, useMemo, useState } from 'react';
import {
  BarChart, Bar, XAxis, YAxis, CartesianGrid, Tooltip, ResponsiveContainer, PieChart, Pie, Cell, Legend,
} from 'recharts';
import { get, type Overview } from '../api';
import { Chip, SectionLabel, SkeletonCards, SkeletonRows, cx, formatTokens, Mono, clockTime, StaleBadge } from '../ui';

const fmtMs = (n: number) => (n >= 1000 ? `${(n / 1000).toFixed(n >= 10000 ? 0 : 1)}s` : `${n}ms`);

function statusTone(s: number): string {
  return s >= 500 ? 'text-bad' : s >= 400 ? 'text-warn' : 'text-ok';
}

function StatCard({ label, value, sub, tone }: { label: string; value: string; sub?: string; tone?: string }) {
  return (
    <div className="rounded-xl border border-line bg-surface px-4 py-3.5">
      <SectionLabel>{label}</SectionLabel>
      <div className={cx('mt-1.5 font-mono text-[22px] font-semibold tabular-nums tracking-tight', tone ?? 'text-ink')}>{value}</div>
      {sub && <div className="mt-0.5 text-[11px] text-mute">{sub}</div>}
    </div>
  );
}

function HealthStrip({ o }: { o: Overview }) {
  const h = o.health;
  const items: Array<[string, string]> = [
    ['accounts', String(h.accounts)],
    ['models', String(h.models)],
    ['combos', String(h.combos)],
    ['ledger rows', formatTokens(h.ledger_rows)],
    ['dropped', String(h.dropped_rows)],
    ['uptime', fmtUptime(h.uptime_s)],
  ];
  return (
    <div className="flex flex-wrap gap-2">
      {items.map(([k, v]) => (
        <Chip key={k} className="gap-2 px-2.5 py-1">
          <span className="text-mute">{k}</span> <Mono className="text-[11px] text-ink">{v}</Mono>
        </Chip>
      ))}
      <Chip tone={h.dropped_rows > 0 ? 'warn' : 'ok'} className="gap-1.5 px-2.5 py-1">
        <span className={cx('h-1.5 w-1.5 rounded-full', h.status === 'ok' ? 'bg-ok' : 'bg-warn')} />
        {h.status}
        <span className="text-mute">· schema v{h.schema_version}</span>
      </Chip>
    </div>
  );
}

function fmtUptime(s: number): string {
  const d = Math.floor(s / 86400); const h = Math.floor((s % 86400) / 3600); const m = Math.floor((s % 3600) / 60);
  return d > 0 ? `${d}d${h}h` : h > 0 ? `${h}h${m}m` : m > 0 ? `${m}m` : `${s}s`;
}

const chartAxis = { stroke: 'var(--color-mute)', fontSize: 10.5, fontFamily: 'var(--font-mono)' } as const;
const tooltipStyle = {
  contentStyle: {
    background: 'var(--color-raised)', border: '1px solid var(--color-line-strong)', borderRadius: 8,
    fontSize: 12, fontFamily: 'var(--font-mono)', color: 'var(--color-ink)',
  },
  itemStyle: { color: 'var(--color-dim)' },
  labelStyle: { color: 'var(--color-ink)', fontWeight: 600 },
} as const;

function AccountChart({ o }: { o: Overview }) {
  const data = o.usage.map((u) => ({ name: u.k, in: u.tin, out: u.tout, cached: u.cread }));
  return (
    <div className="rounded-xl border border-line bg-surface p-5">
      <div className="flex items-baseline justify-between">
        <SectionLabel>Tokens by account · 24h</SectionLabel>
        <span className="text-[11px] text-mute">in vs out</span>
      </div>
      <div className="mt-4 h-56">
        {data.length === 0
          ? <div className="flex h-full items-center justify-center text-[12.5px] text-mute">no traffic in the last 24h</div>
          : (
            <ResponsiveContainer width="100%" height="100%">
              <BarChart data={data} margin={{ top: 4, right: 8, bottom: 0, left: -8 }} barCategoryGap="28%">
                <CartesianGrid strokeDasharray="3 3" stroke="rgba(255,255,255,0.06)" vertical={false} />
                <XAxis dataKey="name" tick={chartAxis} tickLine={false} axisLine={false} />
                <YAxis tick={chartAxis} tickLine={false} axisLine={false} tickFormatter={(v: number) => formatTokens(v)} width={58} />
                <Tooltip {...tooltipStyle} formatter={(v) => formatTokens(v as number)} cursor={{ fill: 'rgba(255,255,255,0.04)' }} />
                <Legend iconType="circle" iconSize={8} formatter={(v) => <span style={{ color: 'var(--color-dim)', fontSize: 11 }}>{v}</span>} />
                <Bar dataKey="in" stackId="a" fill="var(--color-accent)" radius={[0, 0, 2, 2]} maxBarSize={34} />
                <Bar dataKey="out" stackId="a" fill="rgba(56,189,248,0.35)" radius={[2, 2, 0, 0]} maxBarSize={34} />
              </BarChart>
            </ResponsiveContainer>
          )}
      </div>
    </div>
  );
}

const PIE_COLORS = ['#38bdf8', '#818cf8', '#34d399', '#fbbf24', '#f87171', '#a78bfa'];

function SurfaceChart({ o }: { o: Overview }) {
  const data = o.usage_by_surface.map((u) => ({ name: u.k, value: u.calls }));
  const total = data.reduce((a, d) => a + d.value, 0);
  return (
    <div className="rounded-xl border border-line bg-surface p-5">
      <div className="flex items-baseline justify-between">
        <SectionLabel>Calls by surface</SectionLabel>
        <span className="text-[11px] text-mute mono tabular-nums">{formatTokens(total)} calls</span>
      </div>
      <div className="mt-4 h-56">
        {data.length === 0
          ? <div className="flex h-full items-center justify-center text-[12.5px] text-mute">no surface usage yet</div>
          : (
            <ResponsiveContainer width="100%" height="100%">
              <PieChart>
                <Pie data={data} dataKey="value" nameKey="name" innerRadius="58%" outerRadius="82%" paddingAngle={3} stroke="none">
                  {data.map((_, i) => <Cell key={i} fill={PIE_COLORS[i % PIE_COLORS.length]} />)}
                </Pie>
                <Tooltip {...tooltipStyle} formatter={(v, n) => [`${formatTokens(v as number)} calls`, String(n)]} />
                <Legend iconType="circle" iconSize={8} formatter={(v) => <span style={{ color: 'var(--color-dim)', fontSize: 11, fontFamily: 'var(--font-mono)' }}>{v}</span>} />
              </PieChart>
            </ResponsiveContainer>
          )}
      </div>
    </div>
  );
}

function RecentCalls({ o }: { o: Overview }) {
  const rows = o.recent_calls ?? [];
  return (
    <div className="rounded-xl border border-line bg-surface">
      <div className="flex items-baseline justify-between border-b border-line px-5 py-3.5">
        <SectionLabel>Recent calls</SectionLabel>
        <span className="text-[11px] text-mute">{rows.length} shown · newest first</span>
      </div>
      {rows.length === 0 ? (
        <div className="px-5 py-10 text-center text-[13px] text-mute">No calls yet — route something through a combo and it lands here.</div>
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full min-w-[860px] text-[12.5px]">
            <thead>
              <tr className="text-left text-[10.5px] uppercase tracking-[0.1em] text-mute">
                <th className="px-5 py-2 font-medium">time</th>
                <th className="px-3 py-2 font-medium">client</th>
                <th className="px-3 py-2 font-medium">surface</th>
                <th className="px-3 py-2 font-medium">account</th>
                <th className="px-3 py-2 font-medium">model</th>
                <th className="px-3 py-2 text-right font-medium">status</th>
                <th className="px-3 py-2 text-right font-medium">ttft</th>
                <th className="px-3 py-2 text-right font-medium">total</th>
                <th className="px-3 py-2 text-right font-medium">in</th>
                <th className="px-3 py-2 text-right font-medium">out</th>
                <th className="px-3 py-2 text-right font-medium">saved</th>
                <th className="px-3 py-2 text-right font-medium">compression</th>
              </tr>
            </thead>
            <tbody className="font-mono tabular-nums">
              {rows.map((r, i) => (
                <tr key={i} className="border-t border-line/60 transition-colors hover:bg-[rgba(255,255,255,0.03)]">
                  <td className="whitespace-nowrap px-5 py-2 text-mute">{clockTime(r.ts)}</td>
                  <td className="px-3 py-2 text-dim">{r.client || '–'}</td>
                  <td className="px-3 py-2 text-dim">{r.surface}</td>
                  <td className="px-3 py-2 text-dim">{r.account}</td>
                  <td className="max-w-[220px] truncate px-3 py-2 text-ink" title={r.model}>{r.model}</td>
                  <td className={cx('px-3 py-2 text-right', statusTone(r.status))}>{r.status}</td>
                  <td className="px-3 py-2 text-right text-dim">{fmtMs(r.ttft_ms)}</td>
                  <td className="px-3 py-2 text-right text-dim">{fmtMs(r.total_ms)}</td>
                  <td className="px-3 py-2 text-right tabular-nums text-dim">{r.tin}</td>
                  <td className="px-3 py-2 text-right tabular-nums text-dim">{r.tout}</td>
                  <td className="px-3 py-2 text-right tabular-nums text-dim">
                    {r.saved > 0 ? <span className="text-emerald-300/80">−{formatTokens(r.saved)}</span> : '–'}
                  </td>
                  <td className="px-3 py-2 text-right">
                    {r.compression ? (
                      <span
                        title={r.applied ? `compressed by ${r.compression}` : `asked ${r.compression}, no change`}
                        className={cx(
                          'rounded px-1 py-0.5 font-mono text-[10px]',
                          r.applied ? 'bg-emerald-500/10 text-emerald-300/90' : 'bg-[rgba(255,255,255,0.06)] text-mute',
                        )}
                      >
                        {r.compression === 'off' ? 'off' : r.compression}
                      </span>
                    ) : (
                      <span className="text-mute">–</span>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

function QuotaBars({ quota }: { quota: NonNullable<Overview['accounts'][number]['quota']> }) {
  const rows: Array<[string, number]> = [];
  if (quota.percent_weekly != null) rows.push(['weekly', quota.percent_weekly]);
  if (quota.percent_monthly != null) rows.push(['monthly', quota.percent_monthly]);
  if (rows.length > 0 && rows.every(([, v]) => typeof v === 'number')) {
    return (
      <div className="space-y-2">
        {rows.map(([k, v]) => (
          <div key={k} className="flex items-center gap-2.5">
            <span className="w-14 text-[10.5px] uppercase tracking-wide text-mute">{k}</span>
            <div className="h-1.5 min-w-16 flex-1 overflow-hidden rounded-full bg-[rgba(255,255,255,0.07)]">
              <div className={cx('h-full rounded-full', v >= 90 ? 'bg-bad' : v >= 70 ? 'bg-warn' : 'bg-ok')} style={{ width: `${Math.min(100, Math.max(0, v))}%` }} />
            </div>
            <span className="w-10 text-right font-mono text-[11px] tabular-nums text-dim">{v}%</span>
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
    <div className="rounded-xl border border-line bg-surface">
      <div className="border-b border-line px-5 py-3.5"><SectionLabel>Per-account quota</SectionLabel></div>
      {accounts.length === 0 ? (
        <div className="px-5 py-8 text-center text-[13px] text-mute">No accounts yet.</div>
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
    </div>
  );
}

export default function Dashboard() {
  const [o, setO] = useState<Overview | null>(null);
  const [err, setErr] = useState<string | null>(null);

  const load = useCallback(async (silent: boolean) => {
    if (!silent) setErr(null);
    try {
      const d = await get<Overview>('/admin/overview');
      setO(d);
      setErr(null);
    } catch (e) {
      setErr(e instanceof Error ? e.message : 'failed to load');
    }
  }, []);

  useEffect(() => {
    void load(false);
    const t = setInterval(() => void load(true), 15000);
    return () => clearInterval(t);
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  const totals = useMemo(() => {
    if (!o) return null;
    const sum = (f: (u: Overview['usage'][number]) => number) => o.usage.reduce((a, u) => a + f(u), 0);
    return {
      tin: sum((u) => u.tin), tout: sum((u) => u.tout), cread: sum((u) => u.cread), cwrite: sum((u) => u.cwrite),
      calls: sum((u) => u.calls), errors: sum((u) => u.errors),
    };
  }, [o]);

  if (err && !o) {
    return (
      <div className="rounded-xl border border-bad/30 bg-[rgba(248,113,113,0.05)] px-5 py-4 text-[13px] text-bad">
        {err}
        <button onClick={() => void load(false)} className="ml-3 underline underline-offset-2 hover:text-ink">retry</button>
      </div>
    );
  }

  if (!o || !totals) {
    return (
      <div className="space-y-4">
        <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
          {Array.from({ length: 4 }).map((_, i) => (
            <div key={i} className="rounded-xl border border-line bg-surface px-4 py-3.5">
              <div className="h-2.5 w-20 animate-pulse-soft rounded bg-[rgba(255,255,255,0.05)]" />
              <div className="mt-3 h-7 w-24 animate-pulse-soft rounded bg-[rgba(255,255,255,0.05)]" />
            </div>
          ))}
        </div>
        <div className="grid gap-4 xl:grid-cols-3"><SkeletonCards n={2} /><SkeletonCards n={1} /></div>
        <div className="rounded-xl border border-line bg-surface p-5"><SkeletonRows rows={6} /></div>
      </div>
    );
  }

  return (
    <div className="space-y-4 animate-fade-up">
      <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
        <StatCard label="Tokens in · 24h" value={formatTokens(totals!.tin)} sub={`${formatTokens(totals!.calls)} calls`} />
        <StatCard label="Tokens out · 24h" value={formatTokens(totals!.tout)} sub={totals!.errors > 0 ? `${totals!.errors} errors` : 'clean'} />
        <StatCard label="Cached read · 24h" value={formatTokens(totals!.cread)} tone="text-accent" />
        <StatCard label="Cached write · 24h" value={formatTokens(totals!.cwrite)} sub="billed less on next read" />
      </div>

      <HealthStrip o={o} />

      <div className="grid gap-4 xl:grid-cols-3">
        <div className="xl:col-span-2"><AccountChart o={o} /></div>
        <SurfaceChart o={o} />
      </div>

      <RecentCalls o={o} />
      <QuotaPanel o={o} />
    </div>
  );
}
