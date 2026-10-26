import { useReducedMotion } from '../utils/preferences';
// Shared data-viz primitives for every analytics surface.
//
// Two rules this module exists to enforce:
//   1. ONE colour per meaning. `--color-viz-*` in index.css is the only source;
//      no page may hardcode a hex for a series (Stats used #60a5fa and #4ade80
//      for colours Dashboard spelled #38bdf8 and #34d399 — the same "saved"
//      series rendered as two different colours on two screens).
//   2. Aggregates arrive pre-aggregated from SQL. Nothing here re-sums raw
//      rows — these are presentation layers over /admin/usage groups.
import { motion,} from 'motion/react';
import type { CSSProperties, ReactNode } from 'react';
import { cx, formatTokens, pct } from './ui';

// ── palette ───────────────────────────────────────────────────────────────
// Read through CSS vars so a token rename re-skins every chart and recharts
// (which wants a literal fill) still gets a resolved colour.
const v = (name: string) => `var(${name})`;

export const VIZ = {
  in: v('--color-viz-in'),
  out: v('--color-viz-out'),
  cached: v('--color-viz-cached'),
  reasoning: v('--color-viz-reasoning'),
  saved: v('--color-viz-saved'),
  error: v('--color-viz-error'),
  latency: v('--color-viz-latency'),
  budget: v('--color-viz-budget'),
} as const;

export const CAT = Array.from({ length: 8 }, (_, i) => v(`--color-viz-cat-${i + 1}`));
export const catColor = (i: number) => CAT[i % CAT.length];

// ── shared recharts furniture ──────────────────────────────────────────────
// Every chart in the app passes these, so axis/grid/tooltip/crosshair cannot
// drift between pages (Dashboard used the tokens, Stats hardcoded #111/#333).
export const chartGrid = { stroke: v('--color-viz-grid') } as const;

export const axisProps = {
  tick: { fill: v('--color-viz-axis'), fontSize: 12, fontFamily: 'var(--font-sans)' },
  tickLine: false,
  axisLine: false,
} as const;

export const axisPropsCompact = {
  tick: { fill: v('--color-viz-axis'), fontSize: 11, fontFamily: 'var(--font-sans)' },
  tickLine: false,
  axisLine: false,
} as const;

export const cursorFill = { fill: v('--color-viz-crosshair') } as const;

export const tooltipBox: CSSProperties = {
  background: v('--color-raised'),
  border: `1px solid ${v('--color-line-strong')}`,
  borderRadius: 6,
  fontSize: 12,
  fontFamily: 'var(--font-sans)',
  fontVariantNumeric: 'tabular-nums',
  color: v('--color-ink'),
  padding: '8px 10px',
  boxShadow: '0 8px 24px rgba(0,0,0,0.45)',
};

// ── custom tooltip ─────────────────────────────────────────────────────────
// Recharts' default tooltip renders a coloured square per series and a
// right-aligned row, which fights the mono/tabular look everywhere else.
// This keeps one shape: label on top, then name … value rows.
type TipRow = { name?: string; value?: number | string; color?: string; dataKey?: string | number };
type TipProps = { active?: boolean; payload?: TipRow[]; label?: string; format?: (n: number) => string };

export function ChartTooltip({ active, payload, label, format = formatTokens }: TipProps) {
  if (!active || !payload?.length) return null;
  return (
    <div style={tooltipBox}>
      {label != null && label !== '' && (
        <div className="mb-1.5 text-[12px] font-semibold text-ink">{label}</div>
      )}
      <div className="space-y-1">
        {payload.map((row, i) => (
          <div key={`${row.dataKey ?? i}`} className="flex items-center gap-3">
            <span className="flex items-center gap-1.5 text-[12px] text-dim">
              <span className="h-1.5 w-1.5 rounded-full" style={{ background: row.color }} />
              {row.name}
            </span>
            <span className="ml-auto tnum text-[12px] text-ink">
              {typeof row.value === 'number' ? format(row.value) : String(row.value ?? '–')}
            </span>
          </div>
        ))}
      </div>
    </div>
  );
}

// ── sparkline ─────────────────────────────────────────────────────────────
// A trend at card scale. Hand-rolled SVG, not recharts: a recharts chart per
// stat card costs a ResponsiveContainer plus its resize observer, and this
// dashboard already ships one full recharts bundle.
export function Sparkline({
  data, color = VIZ.in, height = 28, filled = true, className,
}: {
  data: number[]; color?: string; height?: number; filled?: boolean; className?: string;
}) {
  const points = data.filter((n) => Number.isFinite(n));
  if (points.length < 2) {
    // One point is not a trend. A flat line for a single value would read as
    // "stable" when it is really "unknown".
    return <div className={cx('h-7', className)} aria-hidden />;
  }
  const w = 100;
  const h = height;
  const max = Math.max(...points);
  const min = Math.min(...points);
  const span = max - min;
  const pad = 2;
  const x = (i: number) => (i / (points.length - 1)) * w;
  const y = (n: number) => (span === 0 ? h / 2 : pad + (1 - (n - min) / span) * (h - pad * 2));
  const line = points.map((n, i) => `${i === 0 ? 'M' : 'L'}${x(i).toFixed(2)},${y(n).toFixed(2)}`).join(' ');
  // A line, no fill: a gradient under a trend is decoration, and the flat
  // 14% tint the house rules allow adds nothing at this height.
  void filled;
  return (
    <svg viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none" className={cx('w-full', className)}
      style={{ height }} aria-hidden focusable="false">
      <path d={line} fill="none" stroke={color} strokeWidth="1.5" strokeLinejoin="round" strokeLinecap="round" vectorEffect="non-scaling-stroke" />
    </svg>
  );
}

// ── bar list ──────────────────────────────────────────────────────────────
// The workhorse for "rank N named things": models, clients, aliases, accounts.
// A pie chart is unreadable past five slices and a recharts bar chart costs a
// container per card; this is a labelled bar, the share, and the value.
export function BarList({
  rows, total, format = formatTokens, maxRows, emptyLabel = 'nothing in this range', order = 'desc',
}: {
  rows: { key: string; value: number; color?: string; hint?: string }[];
  total?: number;
  format?: (n: number) => string;
  maxRows?: number;
  emptyLabel?: string;
  // 'asc' is the latency reading: the selection still keeps the biggest-N
  // rows (the ones worth showing), but they list lowest value first — the
  // fastest first token at the top, the slow one at the bottom.
  order?: 'asc' | 'desc';
}) {
  const reduce = useReducedMotion();
  // Ranked HERE rather than trusted from the caller: every bar chart in the
  // dashboard answers "biggest first" without each page remembering to sort,
  // and maxRows therefore caps at the N biggest rows, which is the point of
  // a cap.
  const rankedRows = [...rows].sort((a, b) => b.value - a.value);
  const shown = maxRows ? rankedRows.slice(0, maxRows) : rankedRows;
  const display = order === 'asc' ? [...shown].reverse() : shown;
  const sum = total ?? rows.reduce((a, r) => a + r.value, 0);
  const max = Math.max(...shown.map((r) => r.value), 0);
  if (display.length === 0) return <p className="px-5 py-8 text-center text-[13px] text-mute">{emptyLabel}</p>;
  return (
    <ul className="space-y-2.5">
      {display.map((r, i) => {
        const share = pct(r.value, sum);
        return (
          <li key={`${r.key}-${i}`} className="group">
            <div className="flex items-baseline gap-3">
              <span className="min-w-0 flex-1 truncate text-[13px] text-dim" title={r.hint ?? r.key}>{r.key}</span>
              <span className="tnum shrink-0 font-mono text-[13px] text-ink">{format(r.value)}</span>
              {/* Share of the total, not of the bar: the bar is scaled to the
                  largest row, so a bar's length and its share only agree when
                  one row dominates. Both numbers are useful and they are not
                  the same number. */}
              <span className="tnum w-11 shrink-0 text-right font-mono text-[12px] text-mute">{share}</span>
            </div>
            {/* A 2px neutral bar: the ranking is seen, not only read, without
                a hue per row. The leader is set in ink, the rest in grey —
                row colours from callers are ignored on purpose. */}
            <div className="mt-1 h-0.5 overflow-hidden bg-shade">
              <motion.div
                className="h-full"
                style={{ background: i === 0 && order === 'desc' ? 'var(--color-ink)' : 'var(--color-mute)' }}
                initial={reduce ? false : { width: 0 }}
                animate={{ width: `${max > 0 ? (r.value / max) * 100 : 0}%` }}
                transition={{ duration: 0.45, ease: [0.22, 1, 0.36, 1], delay: reduce ? 0 : Math.min(i * 0.035, 0.25) }}
              />
            </div>
          </li>
        );
      })}
    </ul>
  );
}

// ── composition bar ───────────────────────────────────────────────────────
// One stacked bar that answers "what were these tokens?". The point is the
// RATIO between parts, so it renders the same at any width.
export function CompositionBar({
  parts, format = formatTokens, height = 10, className,
}: {
  parts: { key: string; label: string; value: number; color: string }[];
  format?: (n: number) => string;
  height?: number;
  className?: string;
}) {
  const reduce = useReducedMotion();
  const sum = parts.reduce((a, p) => a + Math.max(0, p.value), 0);
  if (sum <= 0) {
    return <div className={cx('rounded-sm bg-inset', className)} style={{ height }} />;
  }
  return (
    <div className={cx('flex gap-[2px] overflow-hidden rounded-sm bg-inset', className)}
      style={{ height }} title={parts.map((p) => `${p.label} ${format(p.value)}`).join(' · ')}>
      {parts.filter((p) => p.value > 0).map((p, i) => (
        <motion.div
          key={p.key}
          className="h-full"
          style={{ background: p.color }}
          initial={reduce ? false : { width: 0 }}
          animate={{ width: `${(p.value / sum) * 100}%` }}
          transition={{ duration: 0.5, ease: [0.22, 1, 0.36, 1], delay: reduce ? 0 : i * 0.05 }}
        />
      ))}
    </div>
  );
}

// ── delta / trend ─────────────────────────────────────────────────────────
// Direction with an honest guard: a previous of 0 has no meaningful percent,
// so it reports the absolute move instead of dividing by zero and printing
// "∞%" or "NaN%".
export function Delta({ current, previous, className }: { current: number; previous: number; className?: string }) {
  if (!Number.isFinite(current) || !Number.isFinite(previous) || previous <= 0) return null;
  const pct = ((current - previous) / previous) * 100;
  const up = pct > 0;
  const flat = Math.abs(pct) < 0.05;
  if (flat) return <span className={cx('text-[12px] text-mute', className)}>flat</span>;
  return (
    <span className={cx('tnum inline-flex items-center gap-0.5 text-[12px] font-medium', up ? 'text-warn' : 'text-ok', className)}
      title={`${formatTokens(current)} vs ${formatTokens(previous)} previously`}>
      <span aria-hidden>{up ? '▲' : '▼'}</span>
      {Math.abs(pct) >= 100 ? `${Math.abs(pct).toFixed(0)}%` : `${Math.abs(pct).toFixed(1)}%`}
    </span>
  );
}

// ── card shell ────────────────────────────────────────────────────────────
// Every analytics card uses this so padding, radius, border and header
// spacing are identical; pages only supply the label and the extra.
export function Panel({
  label, aside, children, className, bodyClass,
}: {
  label?: ReactNode; aside?: ReactNode; children: ReactNode; className?: string; bodyClass?: string;
}) {
  return (
    <section className={cx('flex min-w-0 flex-col rounded-xl border border-line bg-surface', className)}>
      {(label || aside) && (
        <header className="flex items-baseline justify-between gap-3 px-5 pb-1 pt-4">
          {label}
          {aside}
        </header>
      )}
      <div className={cx('min-w-0 flex-1 px-5 pb-5 pt-3', bodyClass)}>{children}</div>
    </section>
  );
}

// ── stat card ─────────────────────────────────────────────────────────────
// Dashboard's four cards, Stats' seven cards and Economics' metrics were
// three near-identical components. This is the one. The sparkline is what
// makes it worth unifying: a trend costs no recharts container, so a card can
// show direction at the same weight as the number.
export function StatCard({
  label, value, sub, tone, footer,
}: {
  label: string;
  value: string;
  sub?: string;
  tone?: string;
  /** Accepted for old call sites and ignored: a KPI carries no sparkline. */
  spark?: number[];
  sparkColor?: string;
  footer?: ReactNode;
}) {
  return (
    <div className="flex min-w-0 flex-col rounded-xl border border-line bg-surface px-4 py-3.5">
      <div className="text-[12px] text-mute">{label}</div>
      <div className={cx('mt-1 text-[22px] font-medium leading-tight tnum', tone ?? 'text-ink')}>
        {value}
      </div>
      {sub && <div className="mt-1 text-[12px] text-mute">{sub}</div>}
      {footer && <div className="mt-2 border-t border-line pt-2.5">{footer}</div>}
    </div>
  );
}

// Label + value in the same compact shape the panels use, for card footers.
export function StatLabel({ children }: { children: ReactNode }) {
  return <div className="text-[12px] text-mute">{children}</div>;
}
