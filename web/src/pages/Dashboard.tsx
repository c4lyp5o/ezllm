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
import { CartesianGrid, Line, LineChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts';
import { get, usage, RANGE_LABEL, type Overview, type RangeKey, type UsageRow } from '../utils/api';
import {
  BarList, ChartTooltip, CompositionBar, Panel, VIZ,
  axisProps, chartGrid, cursorFill,
} from '../components/charts';
import { CountUp, Stagger, StaggerItem } from '../components/motion';
import {
  Chip, CauseChip, EmptyState, SectionLabel, Skeleton, SkeletonRows,
  clockTime, cx, fmtDuration, fmtMs, fmtUptime, formatTokens, nfmt, pct,
} from '../components/ui';

// Two ranges on purpose: 24h for the headline totals, 7d for the trend. The
// ledger cannot sub-divide a day, so the trend is only honest across several.
const TOTAL_RANGE: RangeKey = '24h';
const TREND_RANGE: RangeKey = '7d';

const HEARTBEAT_MS = 15_000; // overview
const SLOW_MS = 60_000;      // breakdowns

// failoverNote: the hops that hopped, from the served row's err trail. The
// server appends `failover: ns/model …` entries for every candidate skipped or
// failed before the one that served; this returns a compact summary (first
// entry, plus a count) or '' when the row served on the first try.
function failoverNote(err?: string): string {
  if (!err) return '';
  const i = err.indexOf('failover:');
  if (i < 0) return '';
  // Split on the MARKER, never on ';' alone: provider JSON bodies contain
  // semicolons and splitting on those would miscount the trail.
  const segs = err.slice(i).split(/;\s*failover:\s*/);
  const first = segs[0].replace(/^failover:\s*/, '').trim();
  return segs.length > 1 ? `${first} (+${segs.length - 1} more)` : first;
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

// ── health strip ──────────────────────────────────────────────────────────
function HealthStrip({ o }: { o: Overview }) {
  const h = o.health;
  return (
    <div className="flex flex-wrap items-center gap-x-4 gap-y-2 text-[13px] text-mute">
      <span>v{h.version}</span>
      <span><span className="tnum text-dim">{nfmt(h.accounts)}</span> accounts</span>
      <span><span className="tnum text-dim">{nfmt(h.models)}</span> models</span>
      <span><span className="tnum text-dim">{nfmt(h.combos)}</span> combos</span>
      <span>up <span className="tnum text-dim">{fmtUptime(h.uptime_s)}</span></span>
      {/* The ledger is asynchronous, so a write that could not be recorded is
          a real accounting gap, not a cosmetic one — it means the call
          happened and its tokens are not counted anywhere. */}
      {h.dropped_rows > 0 && (
        <Chip tone="warn"
          title="ledger rows shed under write load — the calls happened, their accounting did not land">
          {nfmt(h.dropped_rows)} ledger rows dropped
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
      {h.status !== 'ok' && <Chip tone="warn">{h.status}</Chip>}
    </div>
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
        </div>
      ))}
    </div>
  );
}

// ── ranked breakdown panel ────────────────────────────────────────────────
function Breakdown({ label, rows, emptyHint }: { label: string; rows: UsageRow[]; emptyHint: string }) {
  const total = rows.reduce((a, r) => a + r.tin + r.tout + r.cread + r.cwrite, 0);
  return (
    <Panel
      label={<SectionLabel>{label}</SectionLabel>}
      aside={<span className="tnum text-[12px] text-mute">{formatTokens(total)} total</span>}>
      <BarList
        rows={rows.map((r) => ({
          key: r.k,
          value: r.tin + r.tout + r.cread + r.cwrite,
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
  const rows = o.recent_calls ?? [];
  const cursors = new Map((o.block_cursors ?? []).map((c) => [c.combo, c]));
  return (
    <Panel
      label={<SectionLabel>Recent calls</SectionLabel>}
      aside={(
        <span className="flex items-center gap-2 text-[12px] text-mute">
          <span className="relative flex h-1.5 w-1.5">
            <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-ok opacity-60" />
            <span className="relative inline-flex h-1.5 w-1.5 rounded-full bg-ok" />
          </span>
          live · {rows.length} shown · newest first
        </span>
      )}
      bodyClass="p-0">
      {rows.length === 0 ? (
        <EmptyState title="No calls yet."
          hint="Route something through a combo and it lands here; this table refreshes itself every few seconds." />
      ) : (
        <div className="overflow-x-auto">
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
                return (
                  <tr key={i} className="border-b border-line/50 transition-colors last:border-0 hover:bg-wash">
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
                          title="translated hop: this hop's wire differs from the client's surface (anthropic client -> openai-only hop, or a hop curated for /v1/responses) — and the answer was converted back. Tokens are recorded in ezllm's own convention like any other row.">
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
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </Panel>
  );
}

// Quota presentation moved to pages/Providers.tsx — quota data is slotted
// under the provider it describes; the stale pill became a reload action.
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

  // The API ranks day groups by call count (`ORDER BY calls DESC` — right for
  // the ranked breakdowns), so a time series must be sorted by the day key
  // before charting: old → new, flowing to the right. Reversing was not
  // enough — the input order was not temporal at all.
  const series = useMemo(() => [...trend]
    .sort((a, b) => a.k.localeCompare(b.k))
    .map((r) => ({ k: r.k, in: r.tin, out: r.tout })), [trend]);

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
    // Inside the frame, one sentence and a retry; never a full-screen banner.
    return (
      <div className="rounded-xl border border-line bg-surface px-5 py-4 text-[13px] text-dim">
        <span className="text-bad">Couldn't load the overview.</span> {err}
        <button onClick={() => void loadFast(false)} className="ml-3 text-ink underline underline-offset-2">Retry</button>
      </div>
    );
  }

  if (!o || !totals) {
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

  const errRate = totals.calls > 0 ? (totals.errors / totals.calls) * 100 : 0;
  // Cache share of ALL prompt tokens: tokens in (not cached) + cache reads +
  // cache writes. (The old card divided cache reads by tokens in alone and
  // printed "3076% of input served from cache".)
  const promptTotal = totals.tin + totals.cread + totals.cwrite;
  const cacheShare = promptTotal > 0 ? (totals.cread / promptTotal) * 100 : 0;
  const compTotal = totals.tin + totals.tout + totals.cread;
  const fmtPct = (n: number) => `${n.toFixed(n < 10 ? 1 : 0)}%`;

  // The finding, first: the one sentence an operator opening this screen
  // needs, built from the same totals as the strip below.
  const finding = totals.calls === 0
    ? 'No calls in the last 24 hours.'
    : `${nfmt(totals.calls)} calls in the last 24 hours; ${totals.errors > 0 ? `${nfmt(totals.errors)} failed (${fmtPct(errRate)})` : 'none failed'}.`
      + (promptTotal > 0 ? ` ${fmtPct(cacheShare)} of prompt tokens were served from cache.` : '');

  return (
    <div className="space-y-6">
      <div className="space-y-2">
        <p className="text-[15px] text-ink">{finding}</p>
        <HealthStrip o={o} />
      </div>

      <Headline cells={[
        {
          label: `Calls, ${RANGE_LABEL[TOTAL_RANGE]}`,
          value: nfmt(totals.calls),
          count: { n: totals.calls, format: nfmt },
          lead: true,
        },
        {
          label: 'Failed', value: nfmt(totals.errors),
          sub: totals.calls > 0 ? `${fmtPct(errRate)} of calls` : undefined,
          tone: totals.errors > 0 ? 'text-bad' : undefined,
        },
        { label: 'Tokens in', value: formatTokens(totals.tin), sub: 'not served from cache' },
        { label: 'Tokens out', value: formatTokens(totals.tout), sub: `${fmtPct(totals.tin + totals.tout > 0 ? (totals.tout / (totals.tin + totals.tout)) * 100 : 0)} of uncached volume` },
        { label: 'Cache reads', value: formatTokens(totals.cread), sub: `${fmtPct(cacheShare)} of prompt tokens` },
      ]} />

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

      {/* The breakdown trio — the three dimensions that answer "who is
          spending", permanently visible rather than behind a toggle.
          One <Stagger> orchestrates all three so they arrive as a sequence:
          three orphaned <StaggerItem>s (no parent orchestrator) resolved no
          variant at all and sat at their final state from the first paint. */}
      <Stagger className="grid items-start gap-6 xl:grid-cols-3">
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
      </Stagger>

      {/* Composition, latency and surface, so the breakdown is not only
          per-entity: "what were the tokens" and "how fast did they arrive". */}
      <div className="grid items-start gap-6 xl:grid-cols-2">
        <Panel label={<SectionLabel>What the tokens were, {RANGE_LABEL[TOTAL_RANGE]}</SectionLabel>}
          aside={<span className="tnum text-[12px] text-mute">{formatTokens(compTotal)} total</span>}>
          <CompositionBar height={8} parts={[
            { key: 'tin', label: 'tokens in', value: totals.tin, color: VIZ.in },
            { key: 'tout', label: 'tokens out', value: totals.tout, color: VIZ.out },
            { key: 'cread', label: 'cache reads', value: totals.cread, color: VIZ.cached },
          ]} />
          <ul className="mt-4 space-y-1.5">
            {[
              { key: 'tin', label: 'tokens in', value: totals.tin, color: VIZ.in },
              { key: 'tout', label: 'tokens out', value: totals.tout, color: VIZ.out },
              { key: 'cread', label: 'cache reads', value: totals.cread, color: VIZ.cached },
            ].map((p) => (
              <li key={p.key} className="flex items-center gap-2 text-[13px]">
                <span className="h-2 w-2 shrink-0 rounded-full" style={{ background: p.color }} />
                <span className="text-dim">{p.label}</span>
                <span className="tnum ml-auto text-ink">{formatTokens(p.value)}</span>
                <span className="tnum w-12 text-right text-mute">{pct(p.value, compTotal)}</span>
              </li>
            ))}
            {totals.saved > 0 && (
              <li className="flex items-center gap-2 text-[13px]">
                <span className="h-2 w-2 shrink-0 rounded-full bg-ok" />
                <span className="text-dim">removed by compression</span>
                <span className="tnum ml-auto text-ok">−{formatTokens(totals.saved)}</span>
                <span className="tnum w-12 text-right text-mute">
                  {totals.contextSaved > 0 ? `+${formatTokens(totals.contextSaved)} ctx` : ''}
                </span>
              </li>
            )}
          </ul>
          {/* The surface split rides in the same card: two or three rows do
              not earn a card of their own beside a five-row latency list. */}
          <div className="mt-5 flex items-baseline justify-between">
            <span className="text-[13px] font-semibold text-ink">Calls by client wire</span>
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

      <RescuedCalls o={o} />
      <RecentCalls o={o} />
    </div>
  );
}
