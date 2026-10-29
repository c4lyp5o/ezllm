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
import { Fragment, useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { CartesianGrid, Line, LineChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts';
import {
  get, usage, usageHours, RANGE_HOURS, RANGE_LABEL,
  type Overview, type RangeKey, type RecentCall, type UsageRow,
} from '../utils/api';
import { useLiveCalls } from '../utils/live';
import { readRange } from '../utils/preferences';
import {
  BarList, ChartTooltip, CompositionBar, Panel, VIZ,
  axisProps, chartGrid, cursorFill,
} from '../components/charts';
import { CountUp, Stagger, StaggerItem } from '../components/motion';
import {
  Chip, CauseChip, EmptyState, HeaderAside, SectionLabel, Segmented, Skeleton, SkeletonRows, Spinner,
  btn, clockTime, cx, inputCls, relTime, fmtDuration, fmtMs, fmtUptime, formatTokens, nfmt, pct,
} from '../components/ui';

// The trend keeps its own 7-day window whatever range the totals show. The
// ledger cannot sub-divide a day, so the trend is only honest across several.
const TREND_RANGE: RangeKey = '7d';
const RANGES: RangeKey[] = ['24h', '7d', '30d', '365d'];
const RANGE_WORDS: Record<RangeKey, string> = { '24h': '24 hours', '7d': '7 days', '30d': '30 days', '365d': '365 days' };
// /admin/usage refuses a window past maxWindowSpan (400 days), so the
// prior-period comparison (a 2x window) is not available for the 1y view.
const MAX_SPAN_H = 400 * 24;

const HEARTBEAT_MS = 15_000; // overview
const SLOW_MS = 60_000;      // breakdowns

// failoverNote: the hops that hopped, from the served row's err trail. The
// server appends `failover: ns/model …` entries for every candidate skipped or
// failed before the one that served; this returns a compact summary (first
// entry, plus a count) or '' when the row served on the first try.
function failoverTrail(err?: string): string[] {
  if (!err) return [];
  const i = err.indexOf('failover:');
  if (i < 0) return [];
  // Split on the MARKER, never on ';' alone: provider JSON bodies contain
  // semicolons and splitting on those would miscount the trail.
  return err.slice(i).split(/;\s*failover:\s*/).map((x) => x.replace(/^failover:\s*/, '').trim());
}

function failoverNote(err?: string): string {
  const segs = failoverTrail(err);
  if (segs.length === 0) return '';
  return segs.length > 1 ? `${segs[0]} (+${segs.length - 1} more)` : segs[0];
}

// errReason: the upstream's own short words from the ledger err column —
// everything after the `upstream <status> [<cause>]: ` prefix, and before any
// failover trail (which the model cell shows separately). Provider bodies are
// usually JSON with the words under error.message, so surface those, not the
// braces. Clipped for the status cell's second row; full text on hover.
function errReason(err?: string): string {
  if (!err) return '';
  let s = err.split('; failover:')[0];
  const m = /^\S+\s+\d+\s+\[[a-z_]+\]:\s?/.exec(s);
  if (m) s = s.slice(m[0].length);
  if (s.startsWith('{')) {
    try {
      const o = JSON.parse(s.replace(/…$/, ''));
      const msg = (o.error ?? o)?.message;
      if (typeof msg === 'string') s = msg;
    } catch { /* truncated or odd shape — show it raw */ }
  }
  return s.replace(/\s+/g, ' ').trim();
}

function statusTone(s: number): string {
  return s >= 500 ? 'text-bad' : s >= 400 ? 'text-warn' : 'text-ok';
}

// ── health pills ──────────────────────────────────────────────────────────
// Status, not counts: the uptime pill (green while the gateway reports ok)
// plus warnings that only appear when something is wrong. They sit in the
// screen header beside the title — the counts of accounts, models and combos
// were inventory, not health, and live on their own screens.
function HealthPills({ o, children }: { o: Overview; children?: ReactNode }) {
  const h = o.health;
  return (
    <HeaderAside>
      <Chip tone={h.status === 'ok' ? 'ok' : 'warn'} title={`gateway ${h.status}, up since the last restart`}>
        <span className={cx('h-1.5 w-1.5 rounded-full', h.status === 'ok' ? 'bg-ok' : 'bg-warn')} />
        {h.status === 'ok' ? 'Up' : h.status} <span className="tnum">{fmtUptime(h.uptime_s)}</span>
      </Chip>
      {/* The ledger is asynchronous, so a write that could not be recorded is
          a real accounting gap, not a cosmetic one — it means the call
          happened and its tokens are not counted anywhere. */}
      {h.dropped_rows > 0 && (
        <Chip tone="warn"
          title="ledger rows shed under write load — the calls happened, their accounting did not land">
          {nfmt(h.dropped_rows)} ledger rows dropped
        </Chip>
      )}
      {(h.lost_rows ?? 0) > 0 && (
        <Chip tone="warn" title="Ledger writes failed after retry; usage and budgets omit these calls. Check server logs and storage.">
          {nfmt(h.lost_rows ?? 0)} ledger rows lost
        </Chip>
      )}
      {/* Recorded but not metered: the call is in the ledger, but model caps
          and token budgets did not count it, so they read low by this much. */}
      {(h.unmetered_rows ?? 0) > 0 && (
        <Chip tone="warn"
          title="ledger rows the usage meter failed to count — model caps and token budgets under-count by these calls">
          {nfmt(h.unmetered_rows ?? 0)} rows unmetered
        </Chip>
      )}
      {children}
    </HeaderAside>
  );
}

// ── headline strip ────────────────────────────────────────────────────────
// One container split by hairlines, label over value: no tile per figure, no
// sparkline. The call count leads (the screen's question is "is traffic
// flowing?"); a status colour appears only where it says something — on the
// failures, when there are any.
//
// The LEAD figure (the call count) tweens: it is a large changing total and
// the tween shows traffic arriving between two polls. The smaller figures do
// not — a failed count is an exact small number, and animating it through
// 612.4 would read as a rounded lie about a precise figure. So the lead
// carries the raw number plus its formatter, and the rest carry a ready
// string.
type HeadCell = {
  label: string;
  value: string;
  /** Raw number for the lead cell: enables the count-up tween. */
  count?: { n: number; format: (n: number) => string };
  sub?: string;
  lead?: boolean;
  tone?: string;
  /** Change against the previous equal window; tone only where it says something. */
  delta?: { text: string; tone?: string };
};

function Headline({ cells }: { cells: HeadCell[] }) {
  return (
    <div className="grid grid-cols-2 gap-px overflow-hidden rounded-xl border border-line bg-line sm:grid-cols-3 xl:grid-cols-5">
      {cells.map((c, i) => (
        <div key={c.label}
          className={cx('min-w-0 bg-surface px-5 py-4',
            // An odd last cell on a phone takes the full row: no orphan half.
            i === cells.length - 1 && cells.length % 2 === 1 && 'col-span-2 sm:col-span-1')}>
          <div className="text-[13px] text-mute">{c.label}</div>
          <div className={cx('mt-1 font-medium leading-tight tnum', c.lead ? 'text-[30px]' : 'text-[22px]', c.tone ?? 'text-ink')}>
            {c.count
              ? <CountUp value={c.count.n} format={c.count.format} />
              : c.value}
          </div>
          {c.sub && <div className="mt-1 text-[12px] text-mute">{c.sub}</div>}
          {c.delta && <div className={cx('tnum mt-0.5 text-[12px]', c.delta.tone ?? 'text-mute')}>{c.delta.text}</div>}
        </div>
      ))}
    </div>
  );
}

// ── ranked breakdown panel ────────────────────────────────────────────────
// `link` turns a row's name into a Requests URL (omitted where the ledger
// has no filter for that dimension, i.e. API keys).
function Breakdown({ label, rows, emptyHint, link }: { label: string; rows: UsageRow[]; emptyHint: string; link?: (k: string) => string }) {
  const total = rows.reduce((a, r) => a + r.tin + r.tout + r.cread + r.cwrite, 0);
  return (
    <Panel
      label={<SectionLabel>{label}</SectionLabel>}
      aside={<span className="tnum text-[12px] text-mute">{formatTokens(total)} total</span>}>
      <BarList
        rows={rows.map((r) => ({
          key: r.k,
          to: link?.(r.k),
          value: r.tin + r.tout + r.cread + r.cwrite,
          hint: `${nfmt(r.calls)} calls · p50 ttft ${fmtMs(r.p50_ttft_ms)}${r.errors > 0 ? ` · ${r.errors} errors` : ''}`,
        }))}
        emptyLabel={emptyHint}
        maxRows={6} />
    </Panel>
  );
}

// ── header status: live pulse + freshness ────────────────────────────────
// Calls per minute comes from the same SSE feed the Neighbourhood uses
// (/admin/stream), so it moves between polls; the freshness label is the
// honest counterpart for everything else on the page, which is polled.
function PulseChip({ perMinute, connected }: { perMinute: number; connected: boolean }) {
  return connected ? (
    <Chip tone="neutral" title="requests committed in the last 60 seconds, from the live stream">
      <span className="relative flex h-1.5 w-1.5">
        <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-ok opacity-60" />
        <span className="relative inline-flex h-1.5 w-1.5 rounded-full bg-ok" />
      </span>
      <span className="tnum">{nfmt(perMinute)}</span> calls/min
    </Chip>
  ) : (
    <Chip tone="warn" title="the live stream is not delivering; the page still refreshes by polling">
      live feed reconnecting
    </Chip>
  );
}

function Freshness({ at, failed, busy, onRefresh }: { at: number | null; failed: boolean; busy: boolean; onRefresh: () => void }) {
  const [, tick] = useState(0);
  useEffect(() => { const t = setInterval(() => tick((n) => n + 1), 1000); return () => clearInterval(t); }, []);
  return (
    <span className={cx('flex items-center gap-2 text-[12px]', failed ? 'text-warn' : 'text-mute')}
      title={failed ? 'The last refresh failed; showing the previous figures.' : 'The overview refreshes every 15 seconds, the breakdowns every 60.'}>
      {failed ? 'refresh failed · last ok ' : 'updated '}
      <span className="tnum">{at ? relTime(new Date(at).toISOString()) : '–'}</span>
      <button type="button" onClick={onRefresh} disabled={busy} aria-label="Refresh now" title="Refresh now"
        className="inline-flex h-6 w-6 items-center justify-center rounded text-dim transition-colors hover:bg-hover hover:text-ink disabled:opacity-60">
        {busy ? <Spinner className="h-3.5 w-3.5" /> : <span aria-hidden>↻</span>}
      </button>
    </span>
  );
}

// ── prior-period delta ────────────────────────────────────────────────────
// `badWhenUp` colours only the figures where a rise is a problem (failures);
// traffic going up or down is not good or bad by itself.
type Prior = { calls: number; errors: number; tin: number; tout: number; cread: number };
function delta(cur: number, prev: number | undefined, label: string, badWhenUp = false): HeadCell['delta'] {
  if (prev == null) return undefined;
  if (prev === 0) return cur > 0 ? { text: `new, none in the previous ${label}` } : undefined;
  const p = Math.round(((cur - prev) / prev) * 100);
  if (p === 0) return { text: `same as the previous ${label}` };
  const tone = !badWhenUp ? undefined : p > 0 ? 'text-bad' : 'text-ok';
  return { text: `${p > 0 ? '▲' : '▼'} ${Math.abs(p)}% vs previous ${label}`, tone };
}

// ── needs attention ───────────────────────────────────────────────────────
// Appears only when something is off, and every line links to the screen
// that fixes it. Built from the overview alone (accounts, keys, quota and the
// recent-calls window), so it costs no extra request. Thresholds are
// deliberately conservative: a strip that is always lit gets ignored.
type Attn = { id: string; tone: 'bad' | 'warn'; text: string; to: string; action: string };
const RECENT_MS = 15 * 60_000;
const MIN_CALLS_FOR_RATE = 5;

function attentionItems(o: Overview): Attn[] {
  const out: Attn[] = [];
  for (const a of o.accounts ?? []) {
    if (!a.enabled) continue; // disabled on purpose is not a fault
    const keys = a.keys ?? [];
    if (keys.length > 0 && keys.every((k) => !k.enabled)) {
      out.push({ id: `keys-${a.id}`, tone: 'bad', text: `${a.name}: every key is disabled, so it cannot serve`, to: '/providers', action: 'Open providers' });
    } else {
      const failing = keys.filter((k) => k.enabled && k.last_test_ok === false).length;
      if (failing > 0) {
        out.push({ id: `test-${a.id}`, tone: 'warn', text: `${a.name}: ${failing} ${failing === 1 ? 'key' : 'keys'} failed the last test`, to: '/providers', action: 'Open providers' });
      }
    }
    const q = a.quota;
    const pcts: [string, number][] = [];
    if (q?.percent_rolling != null) pcts.push(['rolling', q.percent_rolling]);
    if (q?.percent_weekly != null) pcts.push(['weekly', q.percent_weekly]);
    if (q?.percent_monthly != null) pcts.push(['monthly', q.percent_monthly]);
    const worst = pcts.sort((x, y) => y[1] - x[1])[0];
    if (worst && worst[1] >= 90) {
      out.push({ id: `quota-${a.id}`, tone: worst[1] >= 100 ? 'bad' : 'warn', text: `${a.name}: ${worst[0]} quota at ${Math.round(worst[1])}%`, to: '/providers', action: 'Open providers' });
    }
    if (a.management_key_expires_at) {
      const days = Math.ceil((Date.parse(a.management_key_expires_at) - Date.now()) / 86_400_000);
      if (!Number.isNaN(days) && days <= 3) {
        out.push({ id: `cookie-${a.id}`, tone: days < 0 ? 'bad' : 'warn', text: days < 0 ? `${a.name}: session cookie expired` : `${a.name}: session cookie expires in ${days} ${days === 1 ? 'day' : 'days'}`, to: '/providers', action: 'Open providers' });
      }
    }
  }
  const cut = Date.now() - RECENT_MS;
  const recent = (o.recent_calls ?? []).filter((r) => Date.parse(r.ts) >= cut);
  const failed = recent.filter((r) => r.status >= 400).length;
  if (recent.length >= MIN_CALLS_FOR_RATE && failed / recent.length >= 0.25) {
    out.push({ id: 'errrate', tone: 'bad', text: `${failed} of ${recent.length} calls in the last 15 minutes failed (${Math.round((failed / recent.length) * 100)}%)`, to: '/requests?range=24h', action: 'Open requests' });
  }
  const hops = new Map<string, number>();
  for (const r of recent) if (failoverNote(r.err)) hops.set(r.alias || r.model, (hops.get(r.alias || r.model) ?? 0) + 1);
  for (const [name, n] of hops) {
    if (n >= 3) out.push({ id: `failover-${name}`, tone: 'warn', text: `${name} failed over ${n} times in the last 15 minutes`, to: '/combos', action: 'Open combos' });
  }
  return out.sort((a, b) => (a.tone === b.tone ? 0 : a.tone === 'bad' ? -1 : 1));
}

function AttentionStrip({ o }: { o: Overview }) {
  const items = useMemo(() => attentionItems(o), [o]);
  if (items.length === 0) return null;
  const shown = items.slice(0, 5);
  return (
    <section aria-label="Needs attention" className="rounded-xl border border-line bg-surface">
      <div className="flex items-baseline justify-between border-b border-line px-5 py-2.5">
        <SectionLabel>Needs attention</SectionLabel>
        <span className="tnum text-[12px] text-mute">{items.length} {items.length === 1 ? 'item' : 'items'}</span>
      </div>
      <ul className="divide-y divide-line/50">
        {shown.map((it) => (
          <li key={it.id} className="flex items-center gap-3 px-5 py-2.5 text-[13px]">
            <span className={cx('h-1.5 w-1.5 shrink-0 rounded-full', it.tone === 'bad' ? 'bg-bad' : 'bg-warn')} aria-hidden />
            <span className="sr-only">{it.tone === 'bad' ? 'Problem:' : 'Warning:'}</span>
            <span className="min-w-0 flex-1 truncate text-ink" title={it.text}>{it.text}</span>
            <Link to={it.to} className="shrink-0 text-dim underline underline-offset-2 hover:text-ink">{it.action}</Link>
          </li>
        ))}
      </ul>
      {items.length > shown.length && (
        <p className="border-t border-line/50 px-5 py-2 text-[12px] text-mute">+{items.length - shown.length} more, most severe first</p>
      )}
    </section>
  );
}

// ── recent-call interactions ──────────────────────────────────────────────
type CallFilter = 'all' | 'errors' | 'failover' | 'translated';

// No request id reaches the browser, so a row is identified by what it
// carries. A genuine duplicate would expand together, which is harmless.
const rowId = (r: RecentCall) => `${r.ts}|${r.account}|${r.model}|${r.status}|${r.total_ms}`;

function inFilter(r: RecentCall, f: CallFilter): boolean {
  if (f === 'errors') return r.status >= 400;
  if (f === 'failover') return failoverNote(r.err) !== '';
  if (f === 'translated') return !!r.translated;
  return true;
}

function CopyButton({ text, label }: { text: string; label: string }) {
  const [done, setDone] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text);
      setDone(true);
      setTimeout(() => setDone(false), 1500);
    } catch { /* clipboard blocked (insecure origin or denied): the button simply does nothing */ }
  };
  return (
    <button type="button" onClick={() => void copy()} className={cx(btn.base, btn.ghost, '!h-7 !px-3 !text-[12px]')}>
      {done ? 'Copied' : label}
    </button>
  );
}

function CallDetail({ r }: { r: RecentCall }) {
  const trail = failoverTrail(r.err);
  const body = (r.err ?? '').split('; failover:')[0];
  const meta: [string, string][] = [
    ['time', r.ts], ['client', r.client || '–'], ['surface', r.surface],
    ['provider · combo', [r.account, r.alias].filter(Boolean).join(' · ') || '–'],
    ['caller address', r.ip || '–'],
  ];
  const link = `/requests?${new URLSearchParams({ account: r.account, model: r.model, range: '24h' })}`;
  return (
    <div className="font-sans">
      <dl className="grid gap-x-8 gap-y-1.5 text-[12px] sm:grid-cols-2 lg:grid-cols-3">
        {meta.map(([k, v]) => (
          <div key={k} className="flex gap-2"><dt className="w-28 shrink-0 text-mute">{k}</dt><dd className="min-w-0 break-all text-dim">{v}</dd></div>
        ))}
      </dl>
      {body && (
        <div className="mt-3">
          <div className="text-[12px] text-mute">upstream error</div>
          <pre className="font-code mt-1 max-h-40 overflow-auto whitespace-pre-wrap break-words rounded bg-shade px-3 py-2 text-[12px] text-dim">{body}</pre>
        </div>
      )}
      {trail.length > 0 && (
        <div className="mt-3">
          <div className="text-[12px] text-mute">failover trail, in order</div>
          <ol className="mt-1 list-decimal space-y-0.5 pl-5 text-[12px] text-warn">
            {trail.map((t, i) => <li key={i} className="break-words">{t}</li>)}
          </ol>
        </div>
      )}
      <div className="mt-3 flex flex-wrap items-center gap-2">
        {r.err && <CopyButton text={r.err} label="Copy error" />}
        <CopyButton text={JSON.stringify(r, null, 2)} label="Copy row as JSON" />
        <Link to={link} className={cx(btn.base, btn.ghost, '!h-7 !px-3 !text-[12px]')}>Same model in Requests</Link>
      </div>
    </div>
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
      {/* The one bar list that reads ASCENDING (fastest on top): latency is
          the metric where "smallest is best", so biggest-first would lead
          with the worst number. The footer still names the slowest. */}
      <BarList
        rows={measured.map((r) => ({ key: r.k, value: r.p50_ttft_ms, hint: `${nfmt(r.calls)} calls` }))}
        format={fmtMs} maxRows={5} order="asc" />
      <p className="mt-3 text-[12px] leading-relaxed text-mute">
        Slowest first token: <span className="tnum text-ink">{fmtMs(worst.p50_ttft_ms)}</span> on{' '}
        <span className="text-dim">{worst.k}</span>.
      </p>
    </Panel>
  );
}

function RescuedCalls({ o }: { o: Overview }) {
  const rescued = (o.recent_calls ?? []).filter((r) => failoverNote(r.err));
  if (rescued.length === 0) return null;
  return (
    <Panel label={<SectionLabel>Routing rescue · failover hops</SectionLabel>}
      aside={<span className="text-[12px] text-mute">{rescued.length} rescued</span>}>
      <ul className="space-y-1.5">
        {rescued.slice(0, 8).map((r, i) => (
          <li key={i} className="flex items-center gap-2 text-[13px]">
            <Chip tone="warn">failover</Chip>
            <span className="truncate text-dim">{r.model}</span>
            <span className="ml-auto truncate text-[11px] text-mute" title={r.err}>{failoverNote(r.err)}</span>
            <span className={cx('font-mono text-[11px]', statusTone(r.status))}>{r.status}</span>
          </li>
        ))}
      </ul>
    </Panel>
  );
}

// ── recent calls ──────────────────────────────────────────────────────────
// The single consolidated feed: recent-requests (the SSE RecentFeed) is gone
// from this page — it duplicated these rows. What this table gained:
//   - combo column (the ledger alias: combo name or "ns/model" direct route)
//   - block cursor under the model, joined by alias to the live cursor
//     snapshot (/admin/overview block_cursors) — block_round_robin state is
//     finally visible next to the traffic it governs
//   - error-cause class under the status, parsed from the ledger err marker
//   - compression cell holds profile + saved + held (cae3565 folded the old
//     standalone saved column in)
//   - tokens are ONE cell (in ↑ first, out ↓ second — the requests explorer's
//     shape), and the caller ip renders under the timestamp (01c7a54 captured
//     it; the dashboard was the one view left rendering neither)
function RecentCalls({ o }: { o: Overview }) {
  const live = o.recent_calls ?? [];
  const cursors = new Map((o.block_cursors ?? []).map((c) => [c.combo, c]));
  const [filter, setFilter] = useState<CallFilter>('all');
  const [q, setQ] = useState('');
  const [open, setOpen] = useState<string | null>(null);
  const [hover, setHover] = useState(false);

  // Hovering the table or holding a row open freezes it on the rows that were
  // there at that moment, so a 15s poll never shuffles the row under the
  // cursor. Moving away (and collapsing) lets the live rows back in.
  const paused = hover || open !== null;
  const snap = useRef(live);
  if (!paused) snap.current = live;
  const all = paused ? snap.current : live;

  const needle = q.trim().toLowerCase();
  const rows = all.filter((r) => inFilter(r, filter) && (needle === ''
    || [r.model, r.account, r.alias, r.client, r.ip, r.surface].some((v) => (v ?? '').toLowerCase().includes(needle))));
  const count = (f: CallFilter) => all.filter((r) => inFilter(r, f)).length;
  const toggle = (id: string) => setOpen((cur) => (cur === id ? null : id));

  return (
    <Panel
      label={<SectionLabel>Recent calls</SectionLabel>}
      aside={(
        <span className="flex items-center gap-2 text-[12px] text-mute">
          {paused ? (
            <span className="text-warn" title="Rows are held while you read; move the pointer away and collapse any open row to resume.">paused</span>
          ) : (
            <span className="relative flex h-1.5 w-1.5">
              <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-ok opacity-60" />
              <span className="relative inline-flex h-1.5 w-1.5 rounded-full bg-ok" />
            </span>
          )}
          {paused ? '· ' : 'live · '}{rows.length === all.length ? `${rows.length} shown` : `${rows.length} of ${all.length} shown`} · newest first
        </span>
      )}
      bodyClass="p-0">
      {live.length === 0 ? (
        <EmptyState title="No calls yet."
          hint="Route something through a combo and it lands here; this table refreshes itself every few seconds." />
      ) : (
        <>
          <div className="flex flex-wrap items-center gap-x-4 gap-y-2 border-b border-line px-5 py-2.5">
            <Segmented<CallFilter> size="sm" label="Filter recent calls"
              options={[
                { key: 'all', label: `All ${count('all')}` },
                { key: 'errors', label: `Errors ${count('errors')}`, title: 'status 400 and above' },
                { key: 'failover', label: `Failed over ${count('failover')}`, title: 'served after one or more hops were skipped or failed' },
                { key: 'translated', label: `Translated ${count('translated')}`, title: 'the hop spoke a different wire than the client' },
              ]}
              value={filter} onChange={setFilter} />
            <input type="search" value={q} onChange={(e) => setQ(e.target.value)}
              placeholder="Search model, provider, combo, client, address"
              aria-label="Search recent calls"
              className={cx(inputCls, 'ml-auto !w-72 max-w-full !py-1')} />
          </div>
          {rows.length === 0 ? (
            <EmptyState title="No recent call matches."
              hint="The filter and search apply to the rows shown here, not to the whole ledger. Requests searches the ledger." />
          ) : (
        <div className="overflow-x-auto" onMouseEnter={() => setHover(true)} onMouseLeave={() => setHover(false)}>
          <table className="w-full min-w-[860px] text-[13px]">
            <thead>
              <tr className="border-b border-line text-left text-[11px] text-mute">
                <th className="px-5 py-2.5 font-medium">time</th>
                <th className="px-3 py-2.5 font-medium">client</th>
                <th className="px-3 py-2.5 font-medium" title="the served model, with the surface it rode and who served it (provider · combo) parked underneath">model</th>
                <th className="px-3 py-2.5 text-right font-medium">status</th>
                <th className="px-3 py-2.5 text-right font-medium" title="time to first token on top, total duration below — ttft is only meaningful on a stream">duration</th>
                <th className="px-3 py-2.5 text-right font-medium" title="tokens in ↑ / tokens out ↓">tokens ↑↓</th>
                <th className="px-5 py-2.5 text-right font-medium" title="profile applied; saved = tokens a shipped rewrite removed; held = what a REJECTED rewrite would have saved (tuning data)">compression</th>
              </tr>
            </thead>
            <tbody className="font-mono tnum">
              {rows.map((r, i) => {
                const cursor = r.alias ? cursors.get(r.alias) : undefined;
                const id = rowId(r);
                const isOpen = open === id;
                return (
                  <Fragment key={`${id}#${i}`}>
                  <tr tabIndex={0} aria-expanded={isOpen} onClick={() => toggle(id)}
                    onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggle(id); } }}
                    className={cx('cursor-pointer border-b border-line/50 transition-colors last:border-0 hover:bg-wash focus-visible:bg-wash focus-visible:outline-none', isOpen && 'bg-wash')}>
                    <td className="whitespace-nowrap px-5 py-2 text-mute">
                      {clockTime(r.ts)}
                      {/* The caller address (ledger calls.ip, captured at
                          request time). Public traffic arrives through traefik
                          with a real client ip; a row from this box's own
                          loopback hits reads as the docker bridge peer. */}
                      {r.ip && (
                        <div className="text-[11px] text-mute/70" title={`caller address: ${r.ip}`}>{r.ip}</div>
                      )}
                    </td>
                    <td className="px-3 py-2 text-dim">{r.client || '–'}</td>
                    <td className="max-w-[260px] px-3 py-2 font-sans">
                      <div className="truncate text-ink" title={r.model}>{r.model}</div>
                      {/* Identity of the serve, parked under the model: the
                          surface it rode, then who served it (provider ·
                          combo). Direct routes carry no combo, so the dash
                          stays for those. */}
                      <div className="mt-0.5 flex items-center gap-1.5 text-[11px] text-mute"
                        title={[r.surface, r.account, r.alias].filter(Boolean).join(' · ')}>
                        <span className="shrink-0 rounded bg-shade px-1 py-0.5 font-mono text-[11px] text-dim">{r.surface}</span>
                        <span className="truncate">{r.account}{r.alias ? ` · ${r.alias}` : ''}</span>
                      </div>
                      {/* Row 3: this dispatch's position in its block — the
                          "N requests finished" tag. Size comes from the live
                          cursor when the combo has one; a pos longer than the
                          size was never possible, so a fallback run that
                          overruns N is visible row by row. */}
                      {(r.block_pos ?? 0) > 0 && (
                        <div className="mt-0.5 text-[11px] text-mute"
                          title={cursor
                            ? cursor.hop
                              ? `block_round_robin: dispatch ${r.block_pos} of ${cursor.block_size}; ${cursor.hop} owns the current block (${cursor.remaining} left)${cursor.reason ? ` — this block started: ${cursor.reason}` : ''}`
                              : `block_round_robin: dispatch ${r.block_pos} of ${cursor.block_size}; no live block yet (${cursor.reason || 'cold start'} — nothing has dispatched on this combo since the last restart)`
                            : `block_round_robin: dispatch ${r.block_pos} of its block`}>
                          ▮ {r.block_pos}{cursor ? `/${cursor.block_size}` : ''}
                        </div>
                      )}
                      {/* The hops that hopped: skipped or failed candidates,
                          from the served row's trail. */}
                      {failoverNote(r.err) && (
                        <div className="mt-0.5 truncate text-[11px] text-warn" title={r.err}>
                          failed over: {failoverNote(r.err)}
                        </div>
                      )}
                      {/* Wire-translated hop: the row is attributable, and its
                          in-tokens are the vendor's INCLUSIVE total. */}
                      {r.translated && (
                        <div className="mt-0.5 text-[11px] text-accent"
                          title="translated hop: this hop's wire differs from the client's surface (anthropic client -> chat-only hop, or a hop curated for /v1/responses) — and the answer was converted back. Tokens are recorded in ezllm's own convention like any other row.">
                          ⇄ translated
                        </div>
                      )}
                    </td>
                    <td className={cx('px-3 py-2 text-right', statusTone(r.status))}>
                      <div>{r.status}</div>
                      {r.status >= 400 && (
                        // Row 2 under the code: the classified cause AND the
                        // upstream's own short reason — the code says what
                        // happened, this says why. Full text on hover.
                        <div className="mt-0.5 flex items-center justify-end gap-1.5 font-sans">
                          <CauseChip err={r.err} status={r.status} />
                          {errReason(r.err) && (
                            <span className="max-w-[180px] truncate text-[11px] text-mute" title={r.err}>
                              {errReason(r.err)}
                            </span>
                          )}
                        </div>
                      )}
                    </td>
                    <td className="px-3 py-2 text-right text-dim">
                      {/* Duration, stacked like every other two-line cell:
                          ttft on top (stream-only meaningful), total below. */}
                      <div>
                        <span className="text-[11px] text-mute">ttft </span>
                        {fmtMs(r.ttft_ms)}
                      </div>
                      <div className="mt-0.5">
                        <span className="text-[11px] text-mute">total </span>
                        {fmtDuration(r.total_ms)}
                      </div>
                    </td>
                    <td className="px-3 py-2 text-right">
                      {/* ONE tokens cell, in ↑ first and out ↓ second — the
                          same shape as the requests explorer. Cached reads
                          ride inside tokens_in by the anthropic convention,
                          so they stay a greyed sub-line of the in figure. */}
                      <div className="text-dim"
                        title={`tokens in ${nfmt(r.tin)} (↑, prompt tokens not served from cache) · tokens out ${nfmt(r.tout)} (↓, completion)`}>
                        <span title="tokens in — prompt tokens NOT served from cache">↑{nfmt(r.tin)}</span>{' '}
                        <span className="text-mute" title="tokens out — completion">↓{nfmt(r.tout)}</span>
                      </div>
                      {(r.cread ?? 0) > 0 && (
                        <div className="mt-0.5 text-[11px] text-mute" title="prompt tokens served from the cache — billed at the cache price, not part of tokens in">
                          {nfmt(r.cread ?? 0)} cached
                        </div>
                      )}
                    </td>
                    <td className="px-5 py-2 text-right font-sans">
                      {r.compression ? (
                        <>
                          <span
                            title={r.applied ? `compressed by ${r.compression}` : `asked ${r.compression}, no change`}
                            className={cx('rounded px-1 py-0.5 font-mono text-[11px]',
                              r.applied ? 'bg-ok-dim text-ok' : 'bg-shade text-mute')}>
                            {r.compression === 'off' ? 'off' : r.compression}
                          </span>
                          {/* Row 2 of the same cell: saved + held, consolidated
                              from the old standalone Saved column. `saved` is a
                              rewrite that SHIPPED (real tokens removed); `held`
                              is what a REJECTED rewrite would have saved —
                              tuning data, never real savings. Saved keeps its
                              own status color, held stays amber. */}
                          {((r.applied && r.saved > 0) || (!r.applied && (r.saved_notional ?? 0) > 0)) && (
                            <div className="mt-0.5 text-[11px]">
                              {r.applied && r.saved > 0 && (
                                <span className="text-ok"
                                  title={`compressed by ${r.compression}: ${nfmt(r.saved)} tokens removed`}>
                                  saved {formatTokens(r.saved)}
                                </span>
                              )}
                              {!r.applied && (r.saved_notional ?? 0) > 0 && (
                                <span className="text-warn"
                                  title="held: a rewrite was rejected by the ratio floor — this is what it would have saved">
                                  {' '}held {formatTokens(r.saved_notional ?? 0)}
                                </span>
                              )}
                            </div>
                          )}
                        </>
                      ) : (
                        // No profile on this call — the savings column still
                        // earns its place: a saved-only row is real data.
                        r.saved > 0
                          ? <span className="text-ok" title={`${nfmt(r.saved)} tokens removed by compression`}>saved {formatTokens(r.saved)}</span>
                          : <span className="text-mute">–</span>
                      )}
                    </td>
                  </tr>
                  {isOpen && (
                    <tr className="border-b border-line/50 bg-wash">
                      <td colSpan={7} className="px-5 py-3"><CallDetail r={r} /></td>
                    </tr>
                  )}
                  </Fragment>
                );
              })}
            </tbody>
          </table>
        </div>
          )}
        </>
      )}
    </Panel>
  );
}

// Quota presentation moved to pages/Providers.tsx — quota data is slotted
// under the provider it describes; the stale pill became a reload action.
// ── the dashboard ────────────────────────────────────────────────────────
export default function Dashboard() {
  const [o, setO] = useState<Overview | null>(null);
  const [byAccount, setByAccount] = useState<UsageRow[]>([]);
  const [byModel, setByModel] = useState<UsageRow[]>([]);
  const [byKey, setByKey] = useState<UsageRow[]>([]);
  const [trend, setTrend] = useState<UsageRow[]>([]);
  const [whole, setWhole] = useState<UsageRow | null>(null);
  const [prior, setPrior] = useState<Prior | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [reportsStale, setReportsStale] = useState(false);
  const [range, setRange] = useState<RangeKey>(readRange);
  // Which range the slow reports on screen were fetched for: after a range
  // switch they are dimmed until the new ones land, never passed off as current.
  const [reportsFor, setReportsFor] = useState<RangeKey | null>(null);
  const [okAt, setOkAt] = useState<number | null>(null);
  const [refreshing, setRefreshing] = useState(false);
  const { perMinute, connected } = useLiveCalls();

  // Overview is the fast pulse; the breakdowns refresh on their own cadence
  // (see SLOW_MS). Splitting the timers means a token total does not twiddle
  // every 15s on a tab left open, while the call feed still feels live.
  // A tick that finds its previous request still running is skipped: on a
  // large ledger a slow report must not stack up behind itself.
  const fastBusy = useRef(false);
  const slowBusy = useRef<RangeKey | null>(null);
  const wantRange = useRef(range);
  wantRange.current = range;
  const loadFast = useCallback(async (silent: boolean) => {
    if (fastBusy.current) return;
    fastBusy.current = true;
    if (!silent) setErr(null);
    try {
      setO(await get<Overview>('/admin/overview'));
      setErr(null);
      setOkAt(Date.now());
    } catch (e) {
      setErr(e instanceof Error ? e.message : 'failed to load');
    } finally {
      fastBusy.current = false;
    }
  }, []);

  const loadSlow = useCallback(async () => {
    if (slowBusy.current === range) return;
    slowBusy.current = range;
    const hours = RANGE_HOURS[range];
    try {
      // Independent SQL aggregates in parallel. A failure in one must not
      // blank the others, so each is caught separately and simply leaves its
      // own panel empty — an empty panel says "no data", which is true and
      // useful, whereas a thrown error would take the whole page down.
      // The previous equal window is the 2x window minus this one (the ledger
      // has no from/to pair on this helper); a 2x window past the server's
      // 400-day cap is skipped, so the 1y view has no comparison.
      const [m, k, ac, w, w2, d] = await Promise.all([
        usage('model', range).catch(() => null),
        usage('key', range).catch(() => null),
        usage('account', range).catch(() => null),
        usage('global', range).catch(() => null),
        hours * 2 <= MAX_SPAN_H ? usageHours('global', hours * 2).catch(() => null) : Promise.resolve(null),
        usage('day', TREND_RANGE).catch(() => null),
      ]);
      if (wantRange.current !== range) return; // the operator moved on
      setReportsStale(!m || !k || !ac || !w || !d);
      if (m) setByModel(m.groups);
      if (k) setByKey(k.groups);
      if (ac) setByAccount(ac.groups);
      if (d) setTrend(d.groups);
      setWhole(w ? w.totals : null);
      setPrior(w && w2 ? {
        calls: Math.max(0, w2.totals.calls - w.totals.calls),
        errors: Math.max(0, w2.totals.errors - w.totals.errors),
        tin: Math.max(0, w2.totals.tin - w.totals.tin),
        tout: Math.max(0, w2.totals.tout - w.totals.tout),
        cread: Math.max(0, w2.totals.cread - w.totals.cread),
      } : null);
      setReportsFor(range);
    } finally {
      if (slowBusy.current === range) slowBusy.current = null;
    }
  }, [range]);

  const refreshNow = useCallback(async () => {
    setRefreshing(true);
    try { await Promise.all([loadFast(true), loadSlow()]); } finally { setRefreshing(false); }
  }, [loadFast, loadSlow]);

  // Polling runs only while the tab is visible: a background tab re-running
  // every report on the server's reader pool helps nobody. Coming back
  // refreshes at once, then resumes the timers. Changing the range re-runs
  // this effect (loadSlow is keyed on it), which fetches the new window now.
  useEffect(() => {
    void loadFast(false);
    void loadSlow();
    let fast: ReturnType<typeof setInterval> | undefined;
    let slow: ReturnType<typeof setInterval> | undefined;
    const stop = () => { clearInterval(fast); clearInterval(slow); fast = slow = undefined; };
    const start = () => {
      stop();
      fast = setInterval(() => void loadFast(true), HEARTBEAT_MS);
      slow = setInterval(() => void loadSlow(), SLOW_MS);
    };
    const onVisibility = () => {
      if (document.hidden) { stop(); return; }
      void loadFast(true);
      void loadSlow();
      start();
    };
    if (!document.hidden) start();
    document.addEventListener('visibilitychange', onVisibility);
    return () => { stop(); document.removeEventListener('visibilitychange', onVisibility); };
  }, [loadFast, loadSlow]);

  // The API ranks day groups by call count (`ORDER BY calls DESC` — right for
  // the ranked breakdowns), so a time series must be sorted by the day key
  // before charting: old → new, flowing to the right. Reversing was not
  // enough — the input order was not temporal at all.
  const series = useMemo(() => [...trend]
    .sort((a, b) => a.k.localeCompare(b.k))
    .map((r) => ({ k: r.k, in: r.tin, out: r.tout })), [trend]);

  const fresh = reportsFor === range;
  // 24h totals come from the 15-second overview (the live pulse); other
  // windows come from the ledger report for that window, and are unknown
  // until it lands.
  const totals = useMemo(() => {
    if (!o) return null;
    if (range === '24h') {
      const sum = (f: (u: Overview['usage'][number]) => number) => o.usage.reduce((a, u) => a + f(u), 0);
      return {
        tin: sum((u) => u.tin), tout: sum((u) => u.tout), cread: sum((u) => u.cread), cwrite: sum((u) => u.cwrite),
        calls: sum((u) => u.calls), errors: sum((u) => u.errors),
        saved: sum((u) => u.saved), contextSaved: sum((u) => u.context_saved),
      };
    }
    if (!whole || !fresh) return null;
    return {
      tin: whole.tin, tout: whole.tout, cread: whole.cread, cwrite: whole.cwrite,
      calls: whole.calls, errors: whole.errors, saved: whole.saved, contextSaved: whole.context_saved,
    };
  }, [o, range, whole, fresh]);

  const bySurface = o?.usage_by_surface ?? [];
  const byProvider = range === '24h' ? (o?.usage ?? []) : byAccount;
  const win = RANGE_WORDS[range];
  const reqLink = (field: 'account' | 'model') => (k: string) => `/requests?${new URLSearchParams({ [field]: k, range })}`;

  if (err && !o) {
    // Inside the frame, one sentence and a retry; never a full-screen banner.
    return (
      <div className="rounded-xl border border-line bg-surface px-5 py-4 text-[13px] text-dim">
        <span className="text-bad">Couldn't load the overview.</span> {err}
        <button onClick={() => void loadFast(false)} className="ml-3 text-ink underline underline-offset-2">Retry</button>
      </div>
    );
  }

  if (!o) {
    // The final layout with each value replaced by a flat block at its size.
    return (
      <div className="space-y-6">
        <Skeleton className="h-5 w-[28rem] max-w-full" />
        <div className="grid grid-cols-2 gap-px overflow-hidden rounded-xl border border-line bg-line sm:grid-cols-3 xl:grid-cols-5">
          {Array.from({ length: 5 }).map((_, i) => (
            <div key={i} className="bg-surface px-5 py-4"><Skeleton className="h-3.5 w-20" /><Skeleton className="mt-2 h-7 w-24" /></div>
          ))}
        </div>
        <div className="rounded-xl border border-line bg-surface p-5"><Skeleton className="h-56 w-full" /></div>
        <div className="rounded-xl border border-line bg-surface p-5"><SkeletonRows rows={6} /></div>
      </div>
    );
  }

  const known = totals !== null;
  const t = totals ?? { tin: 0, tout: 0, cread: 0, cwrite: 0, calls: 0, errors: 0, saved: 0, contextSaved: 0 };
  const errRate = t.calls > 0 ? (t.errors / t.calls) * 100 : 0;
  // Cache share of ALL prompt tokens: tokens in (not cached) + cache reads +
  // cache writes. (The old card divided cache reads by tokens in alone and
  // printed "3076% of input served from cache".)
  const promptTotal = t.tin + t.cread + t.cwrite;
  const cacheShare = promptTotal > 0 ? (t.cread / promptTotal) * 100 : 0;
  const compTotal = t.tin + t.tout + t.cread;
  const fmtPct = (n: number) => `${n.toFixed(n < 10 ? 1 : 0)}%`;
  const dash = (v: string) => (known ? v : '–');
  // The comparison needs this window's numbers from the same (slow) report as
  // the prior window's, so it only shows once both have landed.
  const cmp = (cur: number, prev: number | undefined, bad = false) => (known && fresh && prior ? delta(cur, prev, win, bad) : undefined);

  return (
    <div className="space-y-6">
      <HealthPills o={o}>
        <PulseChip perMinute={perMinute} connected={connected} />
        <Freshness at={okAt} failed={!!err} busy={refreshing} onRefresh={() => void refreshNow()} />
      </HealthPills>
      {reportsStale && <p role="alert" className="rounded-lg border border-warn/30 bg-warn-dim p-3 text-sm text-ink">Some usage reports could not refresh. Their figures may be unavailable or out of date; check the connection. <button type="button" className="underline" onClick={() => void loadSlow()}>Retry</button></p>}

      <AttentionStrip o={o} />

      {/* One statement of the window for the whole screen, instead of a
          suffix on the first tile only: the totals and every breakdown below
          share it; the trend panel names its own 7-day window. */}
      <div className="flex flex-wrap items-center justify-between gap-x-6 gap-y-2">
        <p className="text-[12px] text-mute">Showing the last {win} · the token trend covers the last 7 days</p>
        <Segmented<RangeKey> size="sm" label="Time range"
          options={RANGES.map((k) => ({ key: k, label: RANGE_LABEL[k] }))}
          value={range} onChange={setRange} />
      </div>

      <div className={cx('space-y-6 transition-opacity duration-200', !fresh && 'opacity-60')} aria-busy={!fresh}>
      <Headline cells={[
        {
          label: 'Calls',
          value: dash(nfmt(t.calls)),
          count: known ? { n: t.calls, format: nfmt } : undefined,
          lead: true,
          delta: cmp(t.calls, prior?.calls),
        },
        {
          label: 'Failed', value: dash(nfmt(t.errors)),
          sub: known && t.calls > 0 ? `${fmtPct(errRate)} of calls` : undefined,
          tone: known && t.errors > 0 ? 'text-bad' : undefined,
          delta: cmp(t.errors, prior?.errors, true),
        },
        { label: 'Tokens in', value: dash(formatTokens(t.tin)), sub: 'not served from cache', delta: cmp(t.tin, prior?.tin) },
        { label: 'Tokens out', value: dash(formatTokens(t.tout)), sub: known ? `${fmtPct(t.tin + t.tout > 0 ? (t.tout / (t.tin + t.tout)) * 100 : 0)} of uncached volume` : undefined, delta: cmp(t.tout, prior?.tout) },
        { label: 'Cache reads', value: dash(formatTokens(t.cread)), sub: known ? `${fmtPct(cacheShare)} of prompt tokens` : undefined, delta: cmp(t.cread, prior?.cread) },
      ]} />
      </div>

      {/* Traffic over time. Day is the only bucketing there is, so this is
          deliberately a 7-day window: a 24h one is two dots. */}
      <Panel
        label={<SectionLabel>Tokens per day, last {RANGE_LABEL[TREND_RANGE]}</SectionLabel>}
        aside={series.length > 0 ? (
          <span className="flex items-center gap-4 text-[12px] text-mute">
            <span className="flex items-center gap-1.5"><span className="h-2 w-2 rounded-full" style={{ background: VIZ.in }} />tokens in</span>
            <span className="flex items-center gap-1.5"><span className="h-2 w-2 rounded-full" style={{ background: VIZ.out }} />tokens out</span>
          </span>
        ) : undefined}>
        {series.length === 0 ? (
          <EmptyState title="No traffic in the last 7 days."
            hint="Nothing has been routed through a combo in this window. The table below fills the moment it happens." />
        ) : (
          <div className="h-56">
            <ResponsiveContainer width="100%" height="100%">
              <LineChart data={series} margin={{ top: 8, right: 12, bottom: 0, left: 0 }}>
                <CartesianGrid {...chartGrid} vertical={false} />
                <XAxis dataKey="k" {...axisProps} minTickGap={24} />
                <YAxis {...axisProps} width={88} tickFormatter={(v: number) => formatTokens(v)} />
                <Tooltip content={<ChartTooltip />} cursor={cursorFill} />
                <Line type="linear" dataKey="in" name="tokens in" stroke={VIZ.in} strokeWidth={2} dot={false} activeDot={{ r: 3, strokeWidth: 0 }} />
                <Line type="linear" dataKey="out" name="tokens out" stroke={VIZ.out} strokeWidth={1.5} dot={false} activeDot={{ r: 3, strokeWidth: 0 }} />
              </LineChart>
            </ResponsiveContainer>
          </div>
        )}
      </Panel>

      <div className={cx('space-y-6 transition-opacity duration-200', !fresh && 'opacity-60')} aria-busy={!fresh}>
      {/* The breakdown trio — the three dimensions that answer "who is
          spending", permanently visible rather than behind a toggle.
          One <Stagger> orchestrates all three so they arrive as a sequence:
          three orphaned <StaggerItem>s (no parent orchestrator) resolved no
          variant at all and sat at their final state from the first paint.
          Provider and model names open Requests filtered to that name; API
          keys do not, because the ledger has no key filter. */}
      <Stagger className="grid items-start gap-6 xl:grid-cols-3">
        <StaggerItem>
          <Breakdown label="Tokens by provider" rows={byProvider} link={reqLink('account')}
            emptyHint={`No provider traffic in the last ${win}.`} />
        </StaggerItem>
        <StaggerItem>
          <Breakdown label="Tokens by model" rows={byModel} link={reqLink('model')}
            emptyHint={`No model traffic in the last ${win}.`} />
        </StaggerItem>
        <StaggerItem>
          <Breakdown label="Tokens by API key" rows={byKey}
            emptyHint={`No keyed calls in the last ${win} — unkeyed traffic reports as “(none)”.`} />
        </StaggerItem>
      </Stagger>

      {/* Composition, latency and surface, so the breakdown is not only
          per-entity: "what were the tokens" and "how fast did they arrive". */}
      <div className="grid items-start gap-6 xl:grid-cols-2">
        <Panel label={<SectionLabel>What the tokens were, {RANGE_LABEL[range]}</SectionLabel>}
          aside={<span className="tnum text-[12px] text-mute">{known ? `${formatTokens(compTotal)} total` : '–'}</span>}>
          {!known ? (
            <Skeleton className="h-24 w-full" />
          ) : (
            <>
          <CompositionBar height={8} parts={[
            { key: 'tin', label: 'tokens in', value: t.tin, color: VIZ.in },
            { key: 'tout', label: 'tokens out', value: t.tout, color: VIZ.out },
            { key: 'cread', label: 'cache reads', value: t.cread, color: VIZ.cached },
          ]} />
          <ul className="mt-4 space-y-1.5">
            {[
              { key: 'tin', label: 'tokens in', value: t.tin, color: VIZ.in },
              { key: 'tout', label: 'tokens out', value: t.tout, color: VIZ.out },
              { key: 'cread', label: 'cache reads', value: t.cread, color: VIZ.cached },
            ].map((p) => (
              <li key={p.key} className="flex items-center gap-2 text-[13px]">
                <span className="h-2 w-2 shrink-0 rounded-full" style={{ background: p.color }} />
                <span className="text-dim">{p.label}</span>
                <span className="tnum ml-auto text-ink">{formatTokens(p.value)}</span>
                <span className="tnum w-12 text-right text-mute">{pct(p.value, compTotal)}</span>
              </li>
            ))}
            {t.saved > 0 && (
              <li className="flex items-center gap-2 text-[13px]">
                <span className="h-2 w-2 shrink-0 rounded-full bg-ok" />
                <span className="text-dim">removed by compression</span>
                <span className="tnum ml-auto text-ok">−{formatTokens(t.saved)}</span>
                <span className="tnum w-12 text-right text-mute">
                  {t.contextSaved > 0 ? `+${formatTokens(t.contextSaved)} ctx` : ''}
                </span>
              </li>
            )}
          </ul>
            </>
          )}
          {/* The surface split rides in the same card: two or three rows do
              not earn a card of their own beside a five-row latency list.
              It comes from the 24h overview whatever the range, so it names
              its window. */}
          <div className="mt-5 flex items-baseline justify-between">
            <span className="text-[13px] font-semibold text-ink">Calls by client wire · last 24 hours</span>
            <span className="tnum text-[12px] text-mute">{nfmt(bySurface.reduce((a, s) => a + s.calls, 0))} calls</span>
          </div>
          <div className="mt-3">
            <BarList
              rows={bySurface.map((s) => ({
                key: s.k, value: s.calls,
                hint: `${formatTokens(s.tin)} tokens in`,
              }))}
              format={nfmt}
              emptyLabel="No calls yet." />
          </div>
        </Panel>

        <LatencyCard rows={byModel} />
      </div>
      </div>

      <RescuedCalls o={o} />
      <RecentCalls o={o} />
    </div>
  );
}
