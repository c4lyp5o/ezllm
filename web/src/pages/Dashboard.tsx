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
import { get, usage, RANGE_LABEL, type Overview, type RangeKey, type UsageRow } from '../utils/api';
import {
  BarList, CAT, ChartTooltip, CompositionBar, Panel, StatCard, VIZ,
  axisProps, catColor, chartGrid, cursorFill,
} from '../components/charts';
import { Reveal, Stagger, StaggerItem } from '../components/motion';
import {
  Chip, CauseChip, EmptyState, Mono, SectionLabel, SkeletonCards, SkeletonRows,
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
  const items: Array<[string, string]> = [
    ['accounts', String(h.accounts)],
    ['models', String(h.models)],
    ['combos', String(h.combos)],
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
      {/* A green "ok" pill signified nothing — the page responding IS ok. The
          pill earns its pixels only when the collector is actually degraded. */}
      {h.status !== 'ok' && (
        <Chip tone="warn" className="gap-1.5 px-2.5 py-1">
          <span className="h-1.5 w-1.5 rounded-full bg-warn" />
          {h.status}
        </Chip>
      )}
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
      {/* The one bar list that reads ASCENDING (fastest on top): latency is
          the metric where "smallest is best", so biggest-first would lead
          with the worst number. The footer still names the slowest. */}
      <BarList
        rows={measured.map((r) => ({ key: r.k, value: r.p50_ttft_ms, color: VIZ.latency, hint: `${nfmt(r.calls)} calls` }))}
        format={fmtMs} maxRows={5} order="asc" />
      <p className="mt-3 border-t border-line pt-3 text-[11.5px] leading-relaxed text-mute">
        Slowest first token: <span className="font-mono text-warn">{fmtMs(worst.p50_ttft_ms)}</span> on{' '}
        <span className="text-dim">{worst.k}</span>.
      </p>
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
        <span className="flex items-center gap-2 text-[11px] text-mute">
          <span className="relative flex h-1.5 w-1.5">
            <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-ok opacity-60" />
            <span className="relative inline-flex h-1.5 w-1.5 rounded-full bg-ok" />
          </span>
          live · {rows.length} shown · newest first
        </span>
      )}
      bodyClass="p-0">
      {rows.length === 0 ? (
        <EmptyState title="No calls yet"
          hint="Route something through a combo and it lands here — this table refreshes itself every few seconds." />
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full min-w-[860px] text-[12.5px]">
            <thead>
              <tr className="border-b border-line text-left text-[10.5px] uppercase tracking-[0.1em] text-mute">
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
                        <div className="text-[10px] text-mute/70" title={`caller address: ${r.ip}`}>{r.ip}</div>
                      )}
                    </td>
                    <td className="px-3 py-2 text-dim">{r.client || '–'}</td>
                    <td className="max-w-[260px] px-3 py-2 font-sans">
                      <div className="truncate text-ink" title={r.model}>{r.model}</div>
                      {/* Identity of the serve, parked under the model: the
                          surface it rode, then who served it (provider ·
                          combo). Direct routes carry no combo, so the dash
                          stays for those. */}
                      <div className="mt-0.5 flex items-center gap-1.5 text-[10.5px] text-mute"
                        title={[r.surface, r.account, r.alias].filter(Boolean).join(' · ')}>
                        <span className="shrink-0 rounded bg-shade px-1 py-0.5 font-mono text-[10px] text-dim">{r.surface}</span>
                        <span className="truncate">{r.account}{r.alias ? ` · ${r.alias}` : ''}</span>
                      </div>
                      {/* Row 3: this dispatch's position in its block — the
                          "N requests finished" tag. Size comes from the live
                          cursor when the combo has one; a pos longer than the
                          size was never possible, so a fallback run that
                          overruns N is visible row by row. */}
                      {(r.block_pos ?? 0) > 0 && (
                        <div className="mt-0.5 text-[10px] text-mute"
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
                        <div className="mt-0.5 truncate text-[10px] text-warn" title={r.err}>
                          ⚠ {failoverNote(r.err)}
                        </div>
                      )}
                      {/* Wire-translated hop: the row is attributable, and its
                          in-tokens are the vendor's INCLUSIVE total. */}
                      {r.translated && (
                        <div className="mt-0.5 text-[10px] text-accent"
                          title="translated hop: this hop's wire differs from the client's surface (anthropic client -> openai-only hop, or a hop curated for /v1/responses) — so the in-tokens below are the vendor's INCLUSIVE total (cache counted inside), not ezllm's cold-token convention">
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
                            <span className="max-w-[180px] truncate text-[10px] text-mute" title={r.err}>
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
                        <span className="text-[10px] uppercase tracking-wide text-mute">ttft </span>
                        {fmtMs(r.ttft_ms)}
                      </div>
                      <div className="mt-0.5">
                        <span className="text-[10px] uppercase tracking-wide text-mute">total </span>
                        {fmtDuration(r.total_ms)}
                      </div>
                    </td>
                    <td className="px-3 py-2 text-right">
                      {/* ONE tokens cell, in ↑ first and out ↓ second — the
                          same shape as the requests explorer. Cached reads
                          ride inside tokens_in by the anthropic convention,
                          so they stay a greyed sub-line of the in figure. */}
                      <div className="text-dim"
                        title={`tokens in ${nfmt(r.tin)} (↑, prompt with cache inside) · tokens out ${nfmt(r.tout)} (↓, completion)`}>
                        <span title="tokens in — prompt, cache counted inside">↑{nfmt(r.tin)}</span>{' '}
                        <span className="text-mute" title="tokens out — completion">↓{nfmt(r.tout)}</span>
                      </div>
                      {(r.cread ?? 0) > 0 && (
                        <div className="mt-0.5 text-[10px] text-mute" title="of which served from the prompt cache (counted inside tokens in)">
                          {nfmt(r.cread ?? 0)} cached
                        </div>
                      )}
                    </td>
                    <td className="px-5 py-2 text-right font-sans">
                      {r.compression ? (
                        <>
                          <span
                            title={r.applied ? `compressed by ${r.compression}` : `asked ${r.compression}, no change`}
                            className={cx('rounded px-1 py-0.5 font-mono text-[10px]',
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
                            <div className="mt-0.5 text-[10px]">
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
    .map((r) => ({
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
          label={<SectionLabel>Traffic · last {RANGE_LABEL[TREND_RANGE]} by day</SectionLabel>}>
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
          </Panel>
        </StaggerItem>
      </div>

      <Reveal><RecentCalls o={o} /></Reveal>
    </Stagger>
  );
}
