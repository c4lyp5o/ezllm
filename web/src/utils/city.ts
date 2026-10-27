// THE NEIGHBOURHOOD CITY — model, procedural layout, isometric projection.
//
// One building per provider account. The city is GENERATED from the provider
// list (seeded, so a given set of providers always grows the same streets), and
// then the data drives it:
//
//   data    height and window light   tokens per minute (5-minute window)
//           weather                   error share of recent calls
//           unlit / compacted         account or every key disabled
//           roof flare + taxi         one live request (SSE ledger feed)
//           traffic density           how busy the last minute was
//   scenery street grid, roof plant, antennas, trees, parks, lamps, people
//
// A provider that has never served is a low quiet building, never a lit one.
// Nothing here invents activity: `usage === null` is `unknown`, and unknown
// means no glow at all.
import type { Account, UsageResp } from './api';

/** Minutes of ledger the usage figures cover — the page's polling window. */
export const WINDOW_MIN = 5;
/** Full-glow floor: the busiest provider sets the scale, but never below this. */
export const MIN_REF_TPM = 2_000;
export const POLL_MS = 10_000;

export type State = 'live' | 'idle' | 'off' | 'unknown';

export type CityKey = {
  id: string; label: string; hint: string; enabled: boolean;
  tpm: number; calls: number; errors: number;
};

export type Building = {
  id: string; name: string; short: string; namespace: string; kind: string;
  enabled: boolean; state: State;
  tpm: number; calls: number; errors: number;
  /** 0..1 share of the busiest provider's tokens per minute. */
  level: number; storm: boolean;
  keys: CityKey[];
};

/** Log-scaled glow: full at the busiest provider, never below MIN_REF_TPM. */
export function glowLevel(tpm: number, ref: number): number {
  if (tpm <= 0) return 0;
  return Math.min(1, Math.log10(1 + tpm) / Math.log10(1 + Math.max(ref, MIN_REF_TPM)));
}

/** Two-word sign name: long account names stay readable on a small facade. */
export function signName(name: string): string {
  const words = name.trim().split(/\s+/);
  const two = words.slice(0, 2).join(' ');
  if (two.length <= 16) return two;
  return words[0].length <= 16 ? words[0] : `${words[0].slice(0, 15)}…`;
}

export function buildBuildings(accounts: Account[], usage: UsageResp | null): Building[] {
  const byHint = new Map((usage?.groups ?? []).map((g) => [g.k, g]));
  const raw = accounts.map((a) => {
    const keys: CityKey[] = a.keys.map((k) => {
      const g = byHint.get(k.hint);
      const enabled = a.enabled && k.enabled;
      return {
        id: String(k.id), label: k.label || k.hint, hint: k.hint, enabled,
        tpm: enabled && g ? (g.tin + g.tout) / WINDOW_MIN : 0,
        calls: enabled ? g?.calls ?? 0 : 0,
        errors: enabled ? g?.errors ?? 0 : 0,
      };
    });
    const enabled = a.enabled && keys.some((k) => k.enabled);
    const tpm = keys.reduce((n, k) => n + k.tpm, 0);
    const calls = keys.reduce((n, k) => n + k.calls, 0);
    const errors = keys.reduce((n, k) => n + k.errors, 0);
    return {
      id: String(a.id), name: a.name, short: signName(a.name), namespace: a.namespace,
      kind: a.kind, enabled, tpm, calls, errors, keys,
      state: (!enabled ? 'off' : usage === null ? 'unknown' : tpm > 0 ? 'live' : 'idle') as State,
      storm: calls >= 3 && errors / calls >= 0.25,
    };
  });
  const ref = Math.max(MIN_REF_TPM, ...raw.map((r) => r.tpm));
  return raw.map((r) => ({ ...r, level: glowLevel(r.tpm, ref), storm: r.enabled && r.storm }));
}

// ── procedural street grid ────────────────────────────────────────────────
// Blocks are square, streets run between them, and the whole plan is one
// integer-seeded 2D lattice — cheap to place, cheap to draw, no WebGL.
const BLOCK = 7.4;   // block side, tiles
const ROAD = 2.8;    // street width, tiles
const PITCH = BLOCK + ROAD;
const SETBACK = 0.8; // sidewalk + frontage inside the block

export type Archetype = 'tower' | 'deco' | 'block' | 'glass' | 'hall' | 'warehouse';

export type Slot = {
  id: string;              // provider account id
  x: number; y: number;    // footprint, tiles
  w: number; d: number;
  archetype: Archetype;
  tint: number;            // 0..3 palette index
  seed: number;
  block: { x: number; y: number; w: number; d: number };
};

export type Park = { x: number; y: number; w: number; d: number; seed: number; pond: boolean };

export type Tree = { x: number; y: number; seed: number };

export type CityPlan = {
  seed: number;
  w: number; d: number;                  // city extent, tiles
  roadX: number[]; roadY: number[];      // street centre lines
  blocks: { x: number; y: number; w: number; d: number }[];
  slots: Slot[];
  parks: Park[];
  trees: Tree[];
};

/** Deterministic 32-bit hash of a string (FNV-1a), used for every seed. */
export function hash(value: string): number {
  let h = 0x811c9dc5;
  for (let i = 0; i < value.length; i++) {
    h ^= value.charCodeAt(i);
    h = Math.imul(h, 0x01000193);
  }
  return h >>> 0;
}

/** mulberry32 — small, fast, repeatable given a seed. */
export function rng(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

function archetypeFor(seed: number): Archetype {
  const roll = seed % 100;
  if (roll < 26) return 'tower';
  if (roll < 44) return 'deco';
  if (roll < 66) return 'block';
  if (roll < 81) return 'glass';
  if (roll < 92) return 'hall';
  return 'warehouse';
}

/** Footprint span and height multiplier per archetype. */
export const ARCHETYPE: Record<Archetype, { w: number; d: number; rise: number; floors: number }> = {
  tower: { w: 4.1, d: 4.1, rise: 1.15, floors: 1.05 },
  deco: { w: 3.5, d: 3.5, rise: 1.28, floors: 1.0 },
  block: { w: 5.2, d: 4.3, rise: 0.78, floors: 1.0 },
  glass: { w: 4.4, d: 4.4, rise: 1.0, floors: 0.95 },
  hall: { w: 5.4, d: 4.6, rise: 0.46, floors: 1.15 },
  warehouse: { w: 5.6, d: 3.6, rise: 0.3, floors: 1.2 },
};

/**
 * planCity lays providers out on the street grid. Placement comes from a hash
 * of the provider id, NOT from live traffic: a building must not move when a
 * request lands. Heights are computed at draw time from `level`.
 */
export function planCity(ids: string[], seed: number): CityPlan {
  const n = Math.max(1, ids.length);
  const cols = Math.max(1, Math.min(7, Math.ceil(Math.sqrt(n * 1.35))));
  const rows = Math.max(1, Math.ceil(n / cols));
  const w = cols * BLOCK + (cols + 1) * ROAD;
  const d = rows * BLOCK + (rows + 1) * ROAD;

  const roadX: number[] = [];
  const roadY: number[] = [];
  for (let c = 0; c <= cols; c++) roadX.push(ROAD / 2 + c * PITCH);
  for (let r = 0; r <= rows; r++) roadY.push(ROAD / 2 + r * PITCH);

  const blocks: CityPlan['blocks'] = [];
  for (let r = 0; r < rows; r++) {
    for (let c = 0; c < cols; c++) {
      blocks.push({ x: ROAD + c * PITCH, y: ROAD + r * PITCH, w: BLOCK, d: BLOCK });
    }
  }

  // Stable order: the provider list sorted by id, so two renders of the same
  // keys grow the same city whatever order the API returned the accounts in.
  // Blocks are filled in a seeded order, which leaves the spare blocks (parks)
  // scattered instead of strung along the last row.
  const order = [...ids].sort();
  const filling = blocks
    .map((_, i) => i)
    .sort((a, b) => hash(`${seed}:block:${a}`) - hash(`${seed}:block:${b}`))
    .slice(0, Math.min(order.length, blocks.length));
  const taken = new Set(filling);
  const slots: Slot[] = [];
  for (let i = 0; i < blocks.length; i++) {
    if (!taken.has(i)) continue;
    const block = blocks[i];
    const id = order[filling.indexOf(i)];
    const s = hash(`${id}:${seed}`);
    const pick = rng(s);
    const kind = archetypeFor(s);
    const arch = ARCHETYPE[kind];
    const jitter = 0.55;
    const availW = BLOCK - 2 * SETBACK;
    const availD = BLOCK - 2 * SETBACK;
    const bw = Math.min(availW, arch.w + (pick() - 0.5) * jitter);
    const bd = Math.min(availD, arch.d + (pick() - 0.5) * jitter);
    slots.push({
      id, block,
      x: block.x + SETBACK + (availW - bw) * pick(),
      y: block.y + SETBACK + (availD - bd) * pick(),
      w: bw, d: bd,
      archetype: kind, tint: Math.floor(pick() * 4), seed: s,
    });
  }

  // Blocks with no provider become parks. More than three and the outskirts
  // read as empty land, which is honest — those blocks hold no keys.
  const used = new Set(slots.map((s) => `${s.block.x}:${s.block.y}`));
  const spare = blocks.filter((b) => !used.has(`${b.x}:${b.y}`));
  const parks: Park[] = spare.map((b) => ({
    x: b.x, y: b.y, w: b.w, d: b.d,
    seed: hash(`${b.x}:${b.y}:${seed}`), pond: hash(`${b.x}:${b.y}`) % 3 === 0,
  }));

  const trees: Tree[] = [];
  for (const p of parks) {
    const pick = rng(p.seed);
    for (let i = 0; i < 7; i++) {
      trees.push({ x: p.x + 0.9 + pick() * (p.w - 1.8), y: p.y + 0.9 + pick() * (p.d - 1.8), seed: i });
    }
  }
  // Two street trees per occupied block keep the sidewalks alive.
  for (const s of slots) {
    const pick = rng(s.seed ^ 0x9e37);
    trees.push({ x: s.block.x + 0.6, y: s.block.y + s.block.d - 0.6, seed: 1 });
    trees.push({ x: s.block.x + s.block.w - 0.6, y: s.block.y + 0.6, seed: 2 });
    if (pick() > 0.6) trees.push({ x: s.x + s.w + 0.5, y: s.y + 0.4, seed: 3 });
  }

  return { seed, w, d, roadX, roadY, blocks, slots, parks, trees };
}

// ── isometric projection ──────────────────────────────────────────────────
// screen.x = (x - y) * TU/2      screen.y = (x + y) * TU/4 - z * Z
export const TU = 40;          // tile unit: one tile step is TU/2 across
export const Z = 17;           // one height unit, in world pixels

export function isoX(x: number, y: number): number { return (x - y) * (TU / 2); }
export function isoY(x: number, y: number, z = 0): number { return (x + y) * (TU / 4) - z * Z; }

/** Building height in world units for a provider at a given traffic level. */
export function buildingHeight(level: number, state: State, rise: number): number {
  if (state === 'off') return 2.2;
  const base = 3.4 + level * 16.5 * rise;
  return state === 'unknown' ? Math.min(base, 5.4) : base;
}
