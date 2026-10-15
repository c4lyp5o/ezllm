// Dependency-free formatting + helper leaf.
//
// This module imports NOTHING, and nothing may import from ui.tsx/motion.tsx
// into it. That is the whole point: api.ts needs formatTokens for a tooltip
// default, ui.tsx needs it for tables, motion.tsx needs cx. Importing any of
// them from a shared place created a cycle (ui -> motion -> ui), so the pure
// helpers live here and every other module points at this one.

export const cx = (...xs: Array<string | false | null | undefined>) => xs.filter(Boolean).join(' ');

/**
 * Compact token count. The k/M/B steps are rounded to one decimal with a
 * trailing ".0" stripped, so 1,000,000 reads "1M" and 1,250,000 reads "1.3M".
 * Small numbers stay exact — a count of 612 must never render as "0.6k".
 */
export function formatTokens(n: number): string {
  if (!Number.isFinite(n)) return '–';
  const a = Math.abs(n);
  if (a >= 1e9) return `${(n / 1e9).toFixed(1).replace(/\.0$/, '')}B`;
  if (a >= 1e6) return `${(n / 1e6).toFixed(1).replace(/\.0$/, '')}M`;
  if (a >= 1e3) return `${(n / 1e3).toFixed(1).replace(/\.0$/, '')}k`;
  return `${n}`;
}

/**
 * ttft_ms is null on non-streaming calls — there is no first token to time.
 * Render the en-dash "not measured" cue, never the raw `null` ("nullms").
 */
export function fmtMs(n: number | null | undefined): string {
  if (n == null) return '–';
  if (n >= 1000) return `${(n / 1000).toFixed(n >= 10_000 ? 0 : 1)}s`;
  return `${n}ms`;
}

export function fmtDuration(ms: number | null | undefined): string {
  if (ms == null) return '–';
  if (ms < 1000) return `${ms}ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(s < 10 ? 2 : 1)}s`;
  const m = Math.floor(s / 60);
  return `${m}m${Math.round(s % 60)}s`;
}

export function fmtUptime(s: number): string {
  const d = Math.floor(s / 86400); const h = Math.floor((s % 86400) / 3600); const m = Math.floor((s % 3600) / 60);
  return d > 0 ? `${d}d${h}h` : h > 0 ? `${h}h${m}m` : m > 0 ? `${m}m` : `${s}s`;
}

/** Whole-number locale string, for exact counts that must not be abbreviated. */
export const nfmt = (v: number) => (Number.isFinite(v) ? v.toLocaleString('en-US') : '–');

/** Share of a total as a one-decimal percent that drops to whole above 10%. */
export function pct(part: number, total: number): string {
  if (!Number.isFinite(part) || !Number.isFinite(total) || total <= 0) return '–';
  const p = (part / total) * 100;
  return p >= 9.95 ? `${p.toFixed(0)}%` : `${p.toFixed(1)}%`;
}
