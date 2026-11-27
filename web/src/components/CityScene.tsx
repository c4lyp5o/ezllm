// CITY SCENE — the Neighbourhood drawn as an isometric city that tells the truth.
// Design: docs/design/neighbourhood-design.md. Data mapping: utils/city.ts.
//
// Canvas 2D for the ground, buildings, windows, weather and cars; real HTML for
// the signs, so they stay sharp, readable by a screen reader, clickable, and
// can never be covered by a building. The cost is bounded: device pixel ratio
// capped at 2, 60 fps in Show mode and 30 in Status (and on small screens), no
// work while the tab is hidden, and one still frame when `still` is set.
//
// Layer order per frame: sky → ground and streets → cars → buildings (back to
// front) → weather → lamps → sign leaders. The signs sit in a DOM layer above.
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import {
  MAX_FLOORS, floorsOf, hash, hash3, iso, planRows, placeSigns, rulerTicks, signLine, windowMix,
  type Building,
} from '../utils/city';
import { subscribeLiveCalls } from '../utils/live';
import { formatTokens } from './ui';

export type CityMode = 'status' | 'show';

// CityLoading is the stage's loading state: an indeterminate bar (it does not
// pretend to know a percentage) and what is happening now.
export function CityLoading({ label }: { label: string }) {
  return (
    <div className="nbhd-loader" role="progressbar" aria-label={label} aria-busy="true">
      <span className="nbhd-loader-track" aria-hidden><span className="nbhd-loader-fill" /></span>
      <span className="nbhd-loader-label">{label}</span>
    </div>
  );
}

// ── geometry (tiles and world pixels) ───────────────────────────────────────
const LOT = 2.3;          // lot pitch, tiles
const BW = 1.35;          // building footprint, tiles
const FLOOR = 9;          // storey height, world px
const H_MAX = MAX_FLOORS * FLOOR;
const OFF_H = 14;         // a switched-off building, world px
const CAR_MS = 2600;      // one car's trip from the edge of town to the door
const MAX_CARS = 80;

type Palette = {
  sky0: string; sky1: string; ground: string; lot: string; road: string; line: string;
  wallL: string; wallR: string; roof: string; off: string; offR: string;
  win: string; winDark: string; bad: string; badDim: string; ruler: string; car: string; lamp: string;
};
const PALETTE: Record<CityMode, Palette> = {
  status: {
    sky0: '#121a28', sky1: '#121a28', ground: '#18222f', lot: '#1e2937', road: '#131a25', line: '#2b3646',
    wallL: '#3a4760', wallR: '#2e394e', roof: '#46546d', off: '#3b4048', offR: '#30343b',
    win: '#f2c46d', winDark: '#232d3d', bad: '#ef4f63', badDim: '#7a2a35', ruler: '#8b98aa', car: '#cfe3f7', lamp: '#ffd98a',
  },
  show: {
    sky0: '#0b1222', sky1: '#1b2a45', ground: '#1a2a2a', lot: '#223433', road: '#141c28', line: '#3c4a5c',
    wallL: '#33415a', wallR: '#283449', roof: '#3d4b63', off: '#3a3f47', offR: '#2f3339',
    win: '#f2c46d', winDark: '#1d2636', bad: '#ef4f63', badDim: '#7a2a35', ruler: '#8b98aa', car: '#cfe3f7', lamp: '#ffd98a',
  },
};

type Pt = { x: number; y: number };
type Placed = { b: Building; c: number; r: number; h: number; floors: number; seed: number; row: number };
type View = { s: number; ox: number; oy: number; w: number; h: number; dpr: number };
type Car = { id: string; start: number; fail: boolean; lane: number };
type Sign = { id: string; left: number; top: number; anchor: Pt; roof: Pt };
type Live = {
  placed: Placed[]; rows: number; perRow: number; view: View; mode: CityMode;
  hot: string | null; selected: string | null; signs: Sign[]; ceiling: number; still: boolean;
};

const poly = (ctx: CanvasRenderingContext2D, pts: Pt[], fill: string) => {
  ctx.beginPath();
  pts.forEach((p, i) => (i ? ctx.lineTo(p.x, p.y) : ctx.moveTo(p.x, p.y)));
  ctx.closePath();
  ctx.fillStyle = fill;
  ctx.fill();
};
const up = (p: Pt, h: number): Pt => ({ x: p.x, y: p.y - h });
const toScreen = (v: View, p: Pt): Pt => ({ x: p.x * v.s + v.ox, y: p.y * v.s + v.oy });
const roofOf = (p: Placed): Pt => up(iso(p.c + BW / 2, p.r + BW / 2), p.h);

/** Lays the rows out on the ground: the front row nearest the viewer. */
function place(buildings: Building[], ceiling: number, perRow: number): { placed: Placed[]; rows: number } {
  const rows = planRows(buildings, perRow);
  const placed: Placed[] = [];
  rows.forEach((row, ri) => {
    const r = (rows.length - 1 - ri) * LOT + 1;
    const offset = ((perRow - row.length) * LOT) / 2;
    row.forEach((b, ci) => {
      const floors = floorsOf(b, ceiling);
      const h = b.health === 'off' ? OFF_H : floors * FLOOR;
      placed.push({ b, c: ci * LOT + offset + 1, r, h, floors, seed: hash(b.id) % 9973, row: ri });
    });
  });
  return { placed, rows: rows.length };
}

// Screen room kept for the ruler's labels (left) and around the plan.
const PAD_L = 62, PAD = 12;

/** The scale and offset that fit the whole plan and its ruler. `headroom` is
 *  extra screen space kept at the top for signs stacked above the roofs. */
function fit(placed: Placed[], rows: number, perRow: number, w: number, h: number, dpr: number, headroom: number): View {
  const pts: Pt[] = [];
  const c1 = perRow * LOT + 0.8, r1 = rows * LOT + 0.6;
  pts.push(iso(-0.6, -0.4), iso(c1, -0.4), iso(c1, r1), iso(-0.6, r1));
  for (const p of placed) pts.push(up(iso(p.c, p.r), p.h + 20));
  const rl = iso(-0.6, r1);
  pts.push({ x: rl.x - 14, y: rl.y - H_MAX - 6 });
  const minX = Math.min(...pts.map((p) => p.x)), maxX = Math.max(...pts.map((p) => p.x));
  const minY = Math.min(...pts.map((p) => p.y)), maxY = Math.max(...pts.map((p) => p.y)) + 10;
  const top = PAD + 22 + headroom; // 22: the ruler's "tokens / min" caption
  const availW = Math.max(40, w - PAD_L - PAD), availH = Math.max(40, h - top - PAD);
  const s = Math.max(0.15, Math.min(2.2, availW / (maxX - minX), availH / (maxY - minY)));
  return { s, ox: PAD_L + (availW - (maxX - minX) * s) / 2 - minX * s, oy: top + (availH - (maxY - minY) * s) / 2 - minY * s, w, h, dpr };
}

export default function CityScene({
  buildings, ceiling, mode, still, hot, selected, onHover, onSelect,
}: {
  buildings: Building[]; ceiling: number; mode: CityMode; still: boolean;
  hot: string | null; selected: string | null;
  onHover: (id: string | null) => void; onSelect: (id: string) => void;
}) {
  const host = useRef<HTMLDivElement | null>(null);
  const canvas = useRef<HTMLCanvasElement | null>(null);
  const [size, setSize] = useState({ w: 0, h: 0, dpr: 1 });
  const [drawn, setDrawn] = useState(false);
  const narrow = size.w > 0 && size.w < 560;
  const perRow = narrow ? 2 : 4;
  // Headroom belongs to one layout (building count, rows, size): a new layout
  // starts from none, without a reset effect racing the sign layout.
  const [room, setRoom] = useState<{ key: string; px: number }>({ key: '', px: 0 });
  const fitTries = useRef({ key: '', n: 0 });

  const { placed, rows } = useMemo(() => place(buildings, ceiling, perRow), [buildings, ceiling, perRow]);
  const roomKey = `${placed.length}|${rows}|${size.w}|${size.h}`;
  const headroom = room.key === roomKey ? room.px : 0;
  const view = useMemo(() => fit(placed, rows, perRow, size.w, size.h, size.dpr, headroom), [placed, rows, perRow, size, headroom]);

  // Signs: measured in the DOM, placed front row first, nudged clear of each other.
  const signRefs = useRef(new Map<string, HTMLButtonElement>());
  const [signs, setSigns] = useState<Sign[]>([]);
  const signKey = placed.map((p) => `${p.b.id}:${p.b.health}:${p.h}:${Math.round(p.b.tpm)}:${p.b.calls}:${p.b.errors}`).join('|');
  useLayoutEffect(() => {
    if (!size.w) return;
    const order = [...placed].sort((a, b) => (b.c + b.r) - (a.c + a.r));
    const boxes = order.map((p) => {
      const roof = toScreen(view, roofOf(p));
      const el = signRefs.current.get(p.b.id);
      const w = el?.offsetWidth ?? 90, h = el?.offsetHeight ?? 34;
      const lift = p.b.health === 'failing' ? 56 : 16;
      return { id: p.b.id, x: roof.x - w / 2, y: roof.y - lift - h, w, h, roof };
    });
    const tops = placeSigns(boxes, 4, 4);
    // A sign pushed past the top edge: keep more headroom and fit again
    // (bounded, so a crowded city shrinks rather than loops).
    const highest = Math.min(...boxes.map((bx) => tops.get(bx.id) ?? bx.y));
    // Each refit shrinks the plan, so the gap closes in steps; stop under a pixel
    // or after a few tries (a city too crowded to fit just keeps its last fit).
    const tries = fitTries.current.key === roomKey ? fitTries.current.n : 0;
    if (highest < PAD - 1 && headroom < size.h * 0.55 && tries < 8) {
      fitTries.current = { key: roomKey, n: tries + 1 };
      setRoom({ key: roomKey, px: Math.min(size.h * 0.55, headroom + (PAD - highest) + 2) });
      return;
    }
    setSigns(boxes.map((bx) => {
      const top = tops.get(bx.id) ?? bx.y;
      return { id: bx.id, left: bx.x, top, anchor: { x: bx.x + bx.w / 2, y: top + bx.h }, roof: bx.roof };
    }));
  }, [signKey, view, mode, hot, selected, narrow]); // eslint-disable-line react-hooks/exhaustive-deps

  // Size: follow the host box.
  useEffect(() => {
    const el = host.current;
    if (!el) return;
    const measure = () => {
      const r = el.getBoundingClientRect();
      setSize({ w: Math.round(r.width), h: Math.round(r.height), dpr: Math.min(2, window.devicePixelRatio || 1) });
    };
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  // Live calls → cars. The ledger names the account by namespace.
  const cars = useRef<Car[]>([]);
  const byNs = useMemo(() => new Map(buildings.map((b) => [b.namespace, b.id])), [buildings]);
  const byNsRef = useRef(byNs);
  byNsRef.current = byNs;
  useEffect(() => subscribeLiveCalls((call) => {
    const id = byNsRef.current.get(call.account);
    if (!id) return;
    cars.current.push({ id, start: performance.now(), fail: call.status >= 400, lane: cars.current.length % 2 });
    if (cars.current.length > MAX_CARS) cars.current.splice(0, cars.current.length - MAX_CARS);
  }), []);

  // Everything the frame reads, mirrored into a ref so the loop never restarts
  // on a re-render (cars keep their place on the road).
  const live = useRef<Live>({ placed, rows, perRow, view, mode, hot, selected, signs, ceiling, still });
  live.current = { placed, rows, perRow, view, mode, hot, selected, signs, ceiling, still };

  const stillKey = still ? `${signKey}|${mode}|${hot}|${selected}|${ceiling}|${signs.map((s) => `${s.left},${s.top}`).join(';')}` : '';
  useEffect(() => {
    const cv = canvas.current;
    if (!cv || !size.w || !size.h) return;
    cv.width = Math.round(size.w * size.dpr);
    cv.height = Math.round(size.h * size.dpr);
    const ctx = cv.getContext('2d');
    if (!ctx) return;
    let raf = 0, last = 0;
    const loop = (now: number) => {
      const st = live.current;
      const budget = st.mode === 'show' && size.w >= 560 ? 1000 / 60 : 1000 / 30;
      if (st.still || now - last >= budget - 1) {
        last = now;
        drawFrame(ctx, st, now);
        setDrawn(true);
      }
      if (!st.still && !document.hidden) raf = requestAnimationFrame(loop);
    };
    raf = requestAnimationFrame(loop);
    const onVisible = () => {
      if (!document.hidden && !live.current.still) { cancelAnimationFrame(raf); raf = requestAnimationFrame(loop); }
    };
    document.addEventListener('visibilitychange', onVisible);
    return () => { cancelAnimationFrame(raf); document.removeEventListener('visibilitychange', onVisible); };
    // A still scene redraws only when something it shows changes.
  }, [size, still, stillKey]); // eslint-disable-line react-hooks/exhaustive-deps

  // Hit test on the canvas: building bodies, front first.
  const pick = (clientX: number, clientY: number): string | null => {
    const el = canvas.current;
    if (!el) return null;
    const r = el.getBoundingClientRect();
    const x = clientX - r.left, y = clientY - r.top;
    const order = [...placed].sort((a, b) => (b.c + b.r) - (a.c + a.r));
    for (const p of order) {
      const L = toScreen(view, iso(p.c, p.r + BW)), R = toScreen(view, iso(p.c + BW, p.r));
      const bottom = toScreen(view, iso(p.c + BW, p.r + BW)).y, top = toScreen(view, up(iso(p.c, p.r), p.h)).y;
      if (x >= L.x && x <= R.x && y >= top && y <= bottom) return p.b.id;
    }
    return null;
  };

  const ticks = rulerTicks(ceiling);
  return (
    <div ref={host} className="nbhd-scene" data-mode={mode} data-ceiling={ceiling} data-headroom={Math.round(headroom)} data-still={still ? '1' : '0'}
      onMouseMove={(e) => { if (e.target === canvas.current) onHover(pick(e.clientX, e.clientY)); }}
      onMouseLeave={() => onHover(null)}>
      <canvas ref={canvas} className="nbhd-canvas" aria-hidden
        onClick={(e) => { const id = pick(e.clientX, e.clientY); if (id) onSelect(id); }} />
      {!drawn && <div className="nbhd-scene-loading"><CityLoading label="Laying out the streets…" /></div>}
      <div className="nbhd-signs" role="group"
        aria-label={`City of ${buildings.length} providers. Height scale up to ${formatTokens(ceiling)} tokens per minute, ruler at ${ticks.map((t) => formatTokens(t)).join(', ')}.`}>
        {placed.map((p) => {
          const s = signs.find((x) => x.id === p.b.id);
          const big = (mode === 'status' && !narrow) || p.b.health === 'failing' || hot === p.b.id || selected === p.b.id;
          const roof = toScreen(view, roofOf(p));
          const line = signLine(p.b, (n) => formatTokens(n));
          return (
            <button key={p.b.id} type="button"
              ref={(el) => { if (el) signRefs.current.set(p.b.id, el); else signRefs.current.delete(p.b.id); }}
              className="nbhd-sign" data-building={p.b.id} data-health={p.b.health} data-floors={p.floors} data-row={p.row}
              data-big={big ? '1' : '0'} data-roof={`${Math.round(roof.x)},${Math.round(roof.y)}`}
              aria-pressed={selected === p.b.id}
              aria-label={`${p.b.name}: ${line}`}
              style={{ left: s ? s.left : roof.x, top: s ? s.top : roof.y - 60, visibility: s ? 'visible' : 'hidden' }}
              onMouseEnter={() => onHover(p.b.id)} onMouseLeave={() => onHover(null)}
              onFocus={() => onHover(p.b.id)} onBlur={() => onHover(null)}
              onClick={() => onSelect(p.b.id)}>
              <span className="nbhd-sign-name">{p.b.name}</span>
              <span className="nbhd-sign-line">{line}</span>
            </button>
          );
        })}
      </div>
    </div>
  );

  // ── the frame ─────────────────────────────────────────────────────────────
  function drawFrame(ctx: CanvasRenderingContext2D, st: Live, now: number) {
    const C = PALETTE[st.mode];
    const v = st.view;
    const t = st.still ? 0 : now;
    ctx.setTransform(v.dpr, 0, 0, v.dpr, 0, 0);
    const g = ctx.createLinearGradient(0, 0, 0, v.h);
    g.addColorStop(0, C.sky0); g.addColorStop(1, C.sky1);
    ctx.fillStyle = g; ctx.fillRect(0, 0, v.w, v.h);
    if (st.mode === 'show') drawSky(ctx, v, t, st.still);

    ctx.setTransform(v.dpr * v.s, 0, 0, v.dpr * v.s, v.dpr * v.ox, v.dpr * v.oy);
    drawGround(ctx, st, C);
    drawRuler(ctx, st, C);
    if (!st.still) drawCars(ctx, st, C, now);
    const order = [...st.placed].sort((a, b) => (a.c + a.r) - (b.c + b.r));
    for (const p of order) drawBuilding(ctx, p, st, C, t);
    for (const p of order) if (p.b.health === 'failing') drawRain(ctx, p, t, st.still);
    if (st.mode === 'show') for (const p of st.placed) drawLamp(ctx, p, C);

    // Leaders from each sign down to its roof, in screen space.
    ctx.setTransform(v.dpr, 0, 0, v.dpr, 0, 0);
    for (const s of st.signs) {
      const bad = st.placed.find((p) => p.b.id === s.id)?.b.health === 'failing';
      ctx.strokeStyle = bad ? C.bad : '#ffffff40';
      ctx.lineWidth = 1;
      ctx.beginPath(); ctx.moveTo(s.anchor.x, s.anchor.y); ctx.lineTo(s.roof.x, s.roof.y - (bad ? 11 * v.s : 0)); ctx.stroke();
    }
  }

  function drawSky(ctx: CanvasRenderingContext2D, v: View, t: number, isStill: boolean) {
    for (let i = 0; i < 90; i++) {
      const a = hash3(i, 5, 6);
      ctx.globalAlpha = 0.25 + 0.5 * (isStill ? a : (Math.sin(t / 900 + a * 9) + 1) / 2);
      ctx.fillStyle = '#ffffff';
      ctx.fillRect(hash3(i, 1, 2) * v.w, hash3(i, 3, 4) * v.h * 0.5, 1.4, 1.4);
    }
    ctx.globalAlpha = 1;
    ctx.fillStyle = '#e9e3c9';
    ctx.beginPath(); ctx.arc(v.w - 80, 64, 16, 0, Math.PI * 2); ctx.fill();
    ctx.globalAlpha = 0.08;
    ctx.beginPath(); ctx.arc(v.w - 80, 64, 42, 0, Math.PI * 2); ctx.fill();
    ctx.globalAlpha = 1;
  }

  function drawGround(ctx: CanvasRenderingContext2D, st: Live, C: Palette) {
    const c0 = -0.6, c1 = st.perRow * LOT + 0.8, r0 = -0.4, r1 = st.rows * LOT + 0.6;
    poly(ctx, [iso(c0, r0), iso(c1, r0), iso(c1, r1), iso(c0, r1)], C.ground);
    for (let i = 0; i < st.rows; i++) {
      const ra = i * LOT + 1 + BW + 0.15, rb = i * LOT + 1 + BW + 0.8;
      poly(ctx, [iso(c0, ra), iso(c1, ra), iso(c1, rb), iso(c0, rb)], C.road);
      const a = iso(c0, (ra + rb) / 2), b = iso(c1, (ra + rb) / 2);
      ctx.strokeStyle = C.line; ctx.lineWidth = 1; ctx.setLineDash([6, 8]);
      ctx.beginPath(); ctx.moveTo(a.x, a.y); ctx.lineTo(b.x, b.y); ctx.stroke();
      ctx.setLineDash([]);
    }
    for (const p of st.placed) {
      poly(ctx, [iso(p.c - 0.2, p.r - 0.2), iso(p.c + BW + 0.2, p.r - 0.2), iso(p.c + BW + 0.2, p.r + BW + 0.2), iso(p.c - 0.2, p.r + BW + 0.2)], C.lot);
    }
  }

  function drawRuler(ctx: CanvasRenderingContext2D, st: Live, C: Palette) {
    const base = iso(-0.6, st.rows * LOT + 0.6);
    const x = base.x - 14, s = st.view.s;
    ctx.strokeStyle = C.ruler; ctx.fillStyle = C.ruler; ctx.lineWidth = 1 / s;
    ctx.beginPath(); ctx.moveTo(x, base.y); ctx.lineTo(x, base.y - H_MAX); ctx.stroke();
    ctx.font = `500 ${11 / s}px "Public Sans Variable", "Public Sans", system-ui, sans-serif`;
    ctx.textAlign = 'right'; ctx.textBaseline = 'middle';
    for (const tick of rulerTicks(st.ceiling)) {
      const y = base.y - (tick / st.ceiling) * H_MAX;
      ctx.beginPath(); ctx.moveTo(x - 5 / s, y); ctx.lineTo(x, y); ctx.stroke();
      ctx.fillText(tick === 0 ? '0' : formatTokens(tick), x - 8 / s, y);
    }
    ctx.textAlign = 'left'; ctx.textBaseline = 'alphabetic';
    ctx.fillText('tokens / min', x - 48 / s, base.y - H_MAX - 16 / s);
  }

  function drawBuilding(ctx: CanvasRenderingContext2D, p: Placed, st: Live, C: Palette, t: number) {
    const { c, r, h, b } = p;
    const A = iso(c, r), B = iso(c + BW, r), Cc = iso(c + BW, r + BW), D = iso(c, r + BW);
    const off = b.health === 'off';
    ctx.globalAlpha = 0.3;
    poly(ctx, [D, Cc, { x: Cc.x + h * 0.35, y: Cc.y + h * 0.12 }, { x: D.x + h * 0.35, y: D.y + h * 0.12 }], '#000000');
    ctx.globalAlpha = 1;
    poly(ctx, [D, Cc, up(Cc, h), up(D, h)], off ? C.off : C.wallL);
    poly(ctx, [B, Cc, up(Cc, h), up(B, h)], off ? C.offR : C.wallR);
    poly(ctx, [up(A, h), up(B, h), up(Cc, h), up(D, h)], off ? C.offR : C.roof);
    if (st.hot === b.id || st.selected === b.id) {
      ctx.strokeStyle = '#7dd3fc'; ctx.lineWidth = 2 / st.view.s;
      ctx.beginPath();
      [up(A, h), up(B, h), up(Cc, h), up(D, h)].forEach((q, i) => (i ? ctx.lineTo(q.x, q.y) : ctx.moveTo(q.x, q.y)));
      ctx.closePath(); ctx.stroke();
    }
    if (off) {
      ctx.strokeStyle = '#8a7258'; ctx.lineWidth = 2.5;
      ctx.beginPath();
      ctx.moveTo(D.x + 6, D.y - 3); ctx.lineTo(Cc.x - 6, Cc.y - h + 4);
      ctx.moveTo(D.x + 6, D.y - h + 4); ctx.lineTo(Cc.x - 6, Cc.y - 3);
      ctx.stroke();
      return;
    }
    // Windows: lit share = calls that worked, red share = calls that failed.
    const mix = windowMix(b);
    const cols = 3;
    for (const [face, L, R] of [[0, D, Cc], [1, Cc, B]] as const) {
      for (let f = 0; f < p.floors; f++) {
        for (let k = 0; k < cols; k++) {
          const u0 = (k + 0.22) / cols, u1 = (k + 0.78) / cols, v0 = (f + 0.25) * FLOOR, v1 = (f + 0.75) * FLOOR;
          const x0 = L.x + (R.x - L.x) * u0, y0 = L.y + (R.y - L.y) * u0, x1 = L.x + (R.x - L.x) * u1, y1 = L.y + (R.y - L.y) * u1;
          const roll = hash3(p.seed + face * 101, f, k);
          let col = C.winDark;
          if (roll < mix.failed) col = C.bad;
          else if (roll < mix.failed + mix.ok) col = C.win;
          if (col === C.bad && !st.still && Math.sin(t / 180 + f * 1.7 + k) < -0.2) col = C.badDim;
          poly(ctx, [{ x: x0, y: y0 - v0 }, { x: x1, y: y1 - v0 }, { x: x1, y: y1 - v1 }, { x: x0, y: y0 - v1 }], col);
        }
      }
    }
    if (b.health === 'failing') {
      const top = roofOf(p);
      const pulse = st.still ? 1 : 0.55 + 0.45 * Math.sin(t / 260);
      ctx.strokeStyle = C.bad; ctx.lineWidth = 1.5;
      ctx.beginPath(); ctx.moveTo(top.x, top.y); ctx.lineTo(top.x, top.y - 6); ctx.stroke();
      ctx.fillStyle = C.bad;
      ctx.beginPath(); ctx.arc(top.x, top.y - 7, 4, 0, Math.PI * 2); ctx.fill();
      ctx.globalAlpha = 0.25 * pulse;
      ctx.beginPath(); ctx.arc(top.x, top.y - 7, 16, 0, Math.PI * 2); ctx.fill();
      ctx.globalAlpha = 1;
    }
  }

  function drawRain(ctx: CanvasRenderingContext2D, p: Placed, t: number, isStill: boolean) {
    const top = iso(p.c + BW / 2, p.r + BW / 2);
    const cx = top.x, cy = top.y - p.h - 30;
    ctx.fillStyle = '#2b3345';
    for (const [dx, dy, rr] of [[-14, 2, 11], [0, -4, 14], [15, 2, 11], [6, 6, 10], [-6, 6, 10]]) {
      ctx.beginPath(); ctx.arc(cx + dx, cy + dy, rr, 0, Math.PI * 2); ctx.fill();
    }
    ctx.strokeStyle = '#8fb3d9aa'; ctx.lineWidth = 1;
    for (let i = 0; i < 14; i++) {
      const ph = isStill ? hash3(i, 2, 9) : ((t / 9 + i * 37) % 60) / 60;
      const x = cx - 22 + ((i * 3.3) % 44), y = cy + 10 + ph * (p.h + 20);
      ctx.beginPath(); ctx.moveTo(x, y); ctx.lineTo(x - 2, y + 7); ctx.stroke();
    }
  }

  function drawLamp(ctx: CanvasRenderingContext2D, p: Placed, C: Palette) {
    const q = iso(p.c - 0.1, p.r + BW + 0.3);
    ctx.fillStyle = '#ffd98a22';
    ctx.beginPath(); ctx.ellipse(q.x, q.y, 18, 8, 0, 0, Math.PI * 2); ctx.fill();
    ctx.fillStyle = C.lamp; ctx.fillRect(q.x - 1, q.y - 14, 2, 3);
  }

  function drawCars(ctx: CanvasRenderingContext2D, st: Live, C: Palette, now: number) {
    cars.current = cars.current.filter((k) => now - k.start < CAR_MS);
    for (const k of cars.current) {
      const p = st.placed.find((x) => x.b.id === k.id);
      if (!p) continue;
      const u = (now - k.start) / CAR_MS;
      const q = iso(-0.6 + u * (p.c + BW / 2 + 0.6), p.r + BW + 0.35 + k.lane * 0.22);
      ctx.globalAlpha = u > 0.9 ? (1 - u) * 10 : 1;
      ctx.fillStyle = k.fail ? C.bad : C.car;
      ctx.beginPath(); ctx.ellipse(q.x, q.y, 6, 3.2, 0, 0, Math.PI * 2); ctx.fill();
      if (st.mode === 'show') {
        ctx.fillStyle = k.fail ? '#ef4f6355' : '#fff3c455';
        ctx.beginPath(); ctx.arc(q.x + 7, q.y + 1, 5, 0, Math.PI * 2); ctx.fill();
      }
      ctx.globalAlpha = 1;
    }
  }
}
