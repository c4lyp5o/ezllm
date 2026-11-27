// THE NEIGHBOURHOOD CITY — the data mapping. Design: docs/design/neighbourhood-design.md.
//
// One building per provider account, and one encoding per question:
//
//   height          tokens per minute, LINEAR against a ceiling (a ruler is drawn)
//   window colour   lit = a call that worked, red = a call that failed, dark = no calls
//   red sign, beacon, rain   failing now: 3+ calls and a quarter or more failed
//   short, grey, boarded     the account or every key is switched off
//   cars            one per live request (SSE ledger feed); red = it failed
//
// Placement follows state, never a random seed: failing providers stand in the
// front row, then serving, then quiet, then switched off. Nothing here invents
// activity: `usage === null` is `unknown`, drawn low and dark.
import type { Account, UsageResp } from './api';

/** Minutes of ledger the usage figures cover — the page's polling window. */
export const WINDOW_MIN = 5;
export const POLL_MS = 10_000;
/** The lowest ceiling the busiest-provider scale uses, in tokens per minute. */
export const MIN_CEILING = 1_000;
/** Storeys of the tallest building, at the ceiling. */
export const MAX_FLOORS = 24;

/** The look-back the page can show. Usage is averaged over it into tokens per
 *  minute, so a longer window smooths a provider that was busy a while ago
 *  instead of reading it as quiet. */
export const WINDOWS = [
  { min: WINDOW_MIN, label: '5 min', words: 'five minutes' },
  { min: 60, label: '1 hour', words: 'hour' },
  { min: 1440, label: '24 hours', words: '24 hours' },
] as const;

export type State = 'live' | 'idle' | 'off' | 'unknown';
/** What the scene shows a building as. `failing` wins over `live`. */
export type Health = 'failing' | 'serving' | 'quiet' | 'off' | 'unknown';

export type CityKey = {
  id: string; label: string; hint: string; enabled: boolean;
  tpm: number; calls: number; errors: number;
};

export type Building = {
  id: string; name: string; namespace: string; kind: string;
  enabled: boolean; state: State; health: Health;
  tpm: number; calls: number; errors: number;
  /** 3+ calls and a quarter or more failed. */
  storm: boolean;
  keys: CityKey[];
};

const isStorm = (calls: number, errors: number) => calls >= 3 && errors / calls >= 0.25;

/** `windowMin` is the look-back `usage` was fetched for: tokens are averaged over it.
 *  `usage` is grouped by account (the ledger records the namespace) and gives each
 *  building its totals; `keyUsage`, grouped by key hint, splits them per key. A hint
 *  is not unique (two accounts can both show "••••"), so a key reads its hint's row
 *  only when no other key shares it; a single-key account's key takes the account's
 *  totals. */
export function buildBuildings(accounts: Account[], usage: UsageResp | null, windowMin: number = WINDOW_MIN, keyUsage: UsageResp | null = null): Building[] {
  const byAccount = new Map((usage?.groups ?? []).map((g) => [g.k, g]));
  const byHint = new Map((keyUsage?.groups ?? []).map((g) => [g.k, g]));
  const hintCount = new Map<string, number>();
  for (const a of accounts) for (const k of a.keys) hintCount.set(k.hint, (hintCount.get(k.hint) ?? 0) + 1);
  return accounts.map((a) => {
    const total = byAccount.get(a.namespace);
    const keys: CityKey[] = a.keys.map((k) => {
      const g = a.keys.length === 1 ? total : hintCount.get(k.hint) === 1 ? byHint.get(k.hint) : undefined;
      const enabled = a.enabled && k.enabled;
      return {
        id: String(k.id), label: k.label || k.hint, hint: k.hint, enabled,
        tpm: enabled && g ? (g.tin + g.tout) / windowMin : 0,
        calls: enabled ? g?.calls ?? 0 : 0,
        errors: enabled ? g?.errors ?? 0 : 0,
      };
    });
    const enabled = a.enabled && keys.some((k) => k.enabled);
    const tpm = enabled && total ? (total.tin + total.tout) / windowMin : 0;
    const calls = enabled ? total?.calls ?? 0 : 0;
    const errors = enabled ? total?.errors ?? 0 : 0;
    const state: State = !enabled ? 'off' : usage === null ? 'unknown' : tpm > 0 ? 'live' : 'idle';
    const storm = enabled && usage !== null && isStorm(calls, errors);
    const health: Health = state === 'off' ? 'off' : state === 'unknown' ? 'unknown' : storm ? 'failing' : state === 'live' ? 'serving' : 'quiet';
    return { id: String(a.id), name: a.name, namespace: a.namespace, kind: a.kind, enabled, state, health, tpm, calls, errors, storm, keys };
  });
}

// ── scale ─────────────────────────────────────────────────────────────────

/** Rounds up to 1, 2, 2.5 or 5 times a power of ten: a ceiling a ruler can label. */
export function niceCeil(v: number): number {
  if (!(v > 0)) return MIN_CEILING;
  const e = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 2, 2.5, 5, 10]) if (m * e >= v - 1e-9) return m * e;
  return 10 * e;
}

/** The tokens/min a full-height building stands for: the operator's fixed
 *  ceiling, or the busiest provider rounded up (never below MIN_CEILING). */
export function scaleCeiling(buildings: Building[], fixed: number | null = null): number {
  if (fixed && fixed > 0) return fixed;
  return niceCeil(Math.max(MIN_CEILING, ...buildings.map((b) => b.tpm)));
}

/** Ruler ticks from 0 to the ceiling, in even steps of a round number. */
export function rulerTicks(ceiling: number): number[] {
  const mant = ceiling / Math.pow(10, Math.floor(Math.log10(ceiling)));
  const parts = Math.abs(mant - 2.5) < 1e-9 || Math.abs(mant - 5) < 1e-9 ? 5 : 4;
  return Array.from({ length: parts + 1 }, (_, i) => (ceiling * i) / parts);
}

/** Linear 0..1 share of the ceiling. Over the ceiling (a fixed scale) is 1. */
export function share(tpm: number, ceiling: number): number {
  return ceiling > 0 ? Math.min(1, Math.max(0, tpm / ceiling)) : 0;
}

/** Storeys to draw. Any traffic is at least one storey, so a small provider
 *  never vanishes (its sign carries the exact number). */
export function floorsOf(b: Building, ceiling: number): number {
  if (b.health === 'off' || b.health === 'unknown' || b.tpm <= 0) return 1;
  return Math.max(1, Math.round(share(b.tpm, ceiling) * MAX_FLOORS));
}

/** Share of windows lit (calls that worked) and red (calls that failed). The
 *  rest stay dark. A building with no calls is dark. */
export function windowMix(b: Building): { ok: number; failed: number } {
  if (b.health === 'off' || b.health === 'unknown' || b.calls <= 0) return { ok: 0, failed: 0 };
  const failed = Math.min(1, b.errors / b.calls);
  return { ok: 1 - failed, failed };
}

// ── layout ────────────────────────────────────────────────────────────────

const ROW_ORDER: Health[][] = [['failing'], ['serving'], ['quiet', 'unknown'], ['off']];

/** Rows of buildings, front row first: failing, serving, quiet (and unknown),
 *  switched off; each group by name, wrapped at `perRow`. */
export function planRows(buildings: Building[], perRow = 4): Building[][] {
  const rows: Building[][] = [];
  const n = Math.max(1, Math.floor(perRow));
  for (const group of ROW_ORDER) {
    const g = buildings.filter((b) => group.includes(b.health))
      .sort((a, b) => a.name.localeCompare(b.name) || a.id.localeCompare(b.id));
    for (let i = 0; i < g.length; i += n) rows.push(g.slice(i, i + n));
  }
  return rows;
}

export type SignBox = { id: string; x: number; y: number; w: number; h: number };

/** Signs in priority order (front row first) are nudged up, in `step` pixels,
 *  until none overlaps one already placed. Returns each sign's final top. */
export function placeSigns(signs: SignBox[], gap = 4, step = 4, maxLift = 400): Map<string, number> {
  const placed: SignBox[] = [];
  const out = new Map<string, number>();
  const hits = (s: SignBox) => placed.some((o) => s.x < o.x + o.w + gap && s.x + s.w + gap > o.x && s.y < o.y + o.h + gap && s.y + s.h + gap > o.y);
  for (const s of signs) {
    const box = { ...s };
    let lifted = 0;
    while (hits(box) && lifted < maxLift) { box.y -= step; lifted += step; }
    placed.push(box);
    out.set(s.id, box.y);
  }
  return out;
}

// ── isometric projection ──────────────────────────────────────────────────
// screen.x = (c - r) * TW/2      screen.y = (c + r) * TH/2 - z
export const TW = 64;
export const TH = 32;

export function iso(c: number, r: number): { x: number; y: number } {
  return { x: (c - r) * (TW / 2), y: (c + r) * (TH / 2) };
}

/** Deterministic 0..1 hash of three integers, for window patterns that do not
 *  flicker between frames. */
export function hash3(a: number, b: number, c: number): number {
  let h = (Math.imul(a, 374761393) + Math.imul(b, 668265263) + Math.imul(c, 2147483647)) | 0;
  h = Math.imul(h ^ (h >>> 13), 1274126177);
  return ((h ^ (h >>> 16)) >>> 0) / 4294967296;
}

/** Deterministic 32-bit hash of a string (FNV-1a). */
export function hash(value: string): number {
  let h = 0x811c9dc5;
  for (let i = 0; i < value.length; i++) {
    h ^= value.charCodeAt(i);
    h = Math.imul(h, 0x01000193);
  }
  return h >>> 0;
}

/** One line for a building's sign: its number, or its state in words. */
export function signLine(b: Building, short: (n: number) => string): string {
  if (b.health === 'off') return 'switched off';
  if (b.health === 'unknown') return 'usage unavailable';
  if (b.health === 'failing') return `${Math.round((b.errors / Math.max(1, b.calls)) * 100)}% failing`;
  if (b.health === 'quiet') return 'quiet';
  return `${short(Math.round(b.tpm))} tok/min`;
}
