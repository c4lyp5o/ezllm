// CITY SCENE — the Neighbourhood drawn as a procedurally generated city.
//
// Canvas 2D, not SVG and not WebGL. One affine matrix per building face makes
// thousands of window panes cheap, and one depth-sorted pass gives correct
// occlusion for the traffic in the streets. The cost is bounded: device pixel
// ratio capped at 2, a 60 fps desktop / 30 fps small-screen budget, no work
// while the tab is hidden, no work when the scene is paused, and one still
// frame (same data, no motion) for reduced motion.
//
// Layer order per frame: land → blocks → streets → lamp pools → depth-sorted
// entities (trees, cars, people, buildings) → weather → labels.
//
// The render closures are recreated on every React render and read through a
// ref, so a poll that changes a figure repaints with the new number without
// restarting the animation (cars keep their place on the road).
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import {
  ARCHETYPE, TU, Z, buildingHeight, isoX, isoY, rng,
  type Building, type CityPlan, type Slot,
} from '../utils/city';
import { subscribeLiveCalls, type LiveCall } from '../utils/live';

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

const DUR = 1500;      // request pulse, ms
const LANE = 0.62;     // lane offset from a street's centre line
const MAX_PULSES = 6;  // per building; older pulses drop

type Style = {
  night: boolean;
  ground: string; road: string; kerb: string; line: string; land: string;
  hillFar: string; hillNear: string;
  wall: string[]; side: string[]; roof: string; roofTop: string; roofEdge: string;
  windowOff: string; windowOn: string; glow: string; tail: string;
  shadow: string; tree: string; treeDark: string; trunk: string;
  smoke: string; label: string; cloud: string; car: string[]; carWindow: string;
  door: string; accent: string; bad: string; ink: string;
};

const readStyle = (el: HTMLElement): Style => {
  const cs = getComputedStyle(el);
  const v = (name: string, fallback: string) => cs.getPropertyValue(name).trim() || fallback;
  return {
    night: v('--nb-night', '1') === '1',
    ground: v('--nb-ground', '#263b35'),
    road: v('--nb-road', '#182332'),
    kerb: v('--nb-pavement', '#526576'),
    line: v('--nb-road-line', '#7e8d9a'),
    land: v('--nb-land', '#1d2f2a'),
    hillFar: v('--nb-hill-far', '#2a3f42'),
    hillNear: v('--nb-hill-near', '#22352f'),
    wall: [v('--nb-sage', '#425d56'), v('--nb-clay', '#725b59'), v('--nb-blue', '#4b6281'), v('--nb-steel', '#5a6672')],
    side: [v('--nb-sage-side', '#2b4443'), v('--nb-clay-side', '#50434a'), v('--nb-blue-side', '#334962'), v('--nb-steel-side', '#3d4651')],
    roof: v('--nb-roof', '#40384d'), roofTop: v('--nb-roof-top', '#71617c'), roofEdge: v('--nb-roof-edge', '#8c7c92'),
    windowOff: v('--nb-window-off', '#233146'), windowOn: v('--nb-window-on', '#ffcd7c'),
    glow: v('--nb-glow', '#ffba64'), tail: v('--nb-tail', '#e2564f'),
    shadow: v('--nb-shadow', '#080e1b'),
    tree: v('--nb-tree', '#3d695a'), treeDark: v('--nb-tree-dark', '#2d5149'), trunk: v('--nb-trunk', '#665b56'),
    smoke: v('--nb-smoke', '#94a6bb'), label: v('--nb-label', '#c0cfdb'), cloud: v('--nb-cloud', '#7f93a8'),
    car: [v('--nb-car', '#d8987b'), v('--nb-car-1', '#8fb2c9'), v('--nb-car-2', '#c9c3a8'), v('--nb-car-3', '#a898bd')],
    carWindow: v('--nb-car-window', '#a4c7d8'),
    door: v('--nb-door', '#2c3547'), accent: v('--color-accent', '#38bdf8'),
    bad: v('--color-bad', '#c4242a'), ink: v('--color-ink', '#f2f2f3'),
  };
};

type View = { scale: number; ox: number; oy: number; w: number; h: number; dpr: number };
type Rect = { left: number; top: number; width: number; height: number };
type Car = {
  axis: 0 | 1; pos: number; dir: 1 | -1; lane: number;
  speed: number; wish: number; tint: number; taxi: boolean; life: number;
  fx: number; fy: number;
};
type Ped = { block: number; t: number; speed: number; dir: 1 | -1; phase: number; tint: number };
type Pulse = { start: number; power: number };
type Spark = { x: number; y: number; z: number; start: number; life: number };
/** A plane crossing the sky: screen-space scenery, never a metric. */
type Plane = { x: number; y: number; speed: number; dir: 1 | -1; trail: number[][]; tick: number };
type Weather = { rain: boolean; until: number };

const clamp = (v: number, lo: number, hi: number) => Math.min(hi, Math.max(lo, v));
const ease = (t: number) => 1 - (1 - clamp(t, 0, 1)) ** 3;
/** Stable 0..1 per window pane, from the building seed and pane index. */
const shade = (seed: number, i: number) => ((Math.imul(seed ^ (i * 2654435761), 2654435761) >>> 0) % 1000) / 1000;

/** A soft radial light, cached per colour: lamps, headlights, roof flares. */
const glowCache = new Map<string, HTMLCanvasElement>();
function glowAt(ctx: CanvasRenderingContext2D, color: string, x: number, y: number, r: number, alpha: number) {
  let sprite = glowCache.get(color);
  if (!sprite) {
    sprite = document.createElement('canvas');
    sprite.width = 64; sprite.height = 64;
    const c = sprite.getContext('2d');
    if (c) {
      const g = c.createRadialGradient(32, 32, 0, 32, 32, 32);
      g.addColorStop(0, color);
      g.addColorStop(0.45, `${color}55`);
      g.addColorStop(1, `${color}00`);
      c.fillStyle = g;
      c.fillRect(0, 0, 64, 64);
    }
    glowCache.set(color, sprite);
  }
  const prev = ctx.globalAlpha;
  ctx.globalAlpha = alpha;
  ctx.drawImage(sprite, x - r, y - r, r * 2, r * 2);
  ctx.globalAlpha = prev;
}

/** Seeded speckle tile, cached per palette: grass and asphalt grain. */
const patternCache = new Map<string, CanvasPattern | null>();
function grain(ctx: CanvasRenderingContext2D, dark: string, light: string, seed: number, count: number): CanvasPattern | null {
  const key = `${dark}|${light}|${seed}`;
  if (patternCache.has(key)) return patternCache.get(key) ?? null;
  const tile = document.createElement('canvas');
  tile.width = 96; tile.height = 96;
  const c = tile.getContext('2d');
  let pattern: CanvasPattern | null = null;
  if (c) {
    const r = rng(seed);
    for (let i = 0; i < count; i++) {
      c.globalAlpha = 0.1 + r() * 0.16;
      c.fillStyle = r() > 0.45 ? dark : light;
      c.fillRect(Math.floor(r() * 96), Math.floor(r() * 96), 1 + Math.floor(r() * 2), 1 + Math.floor(r() * 2));
    }
    pattern = ctx.createPattern(tile, 'repeat');
  }
  patternCache.set(key, pattern);
  return pattern;
}

const quadPath = (ctx: CanvasRenderingContext2D, x: number, y: number, w: number, d: number) => {
  ctx.beginPath();
  ctx.moveTo(isoX(x, y), isoY(x, y));
  ctx.lineTo(isoX(x + w, y), isoY(x + w, y));
  ctx.lineTo(isoX(x + w, y + d), isoY(x + w, y + d));
  ctx.lineTo(isoX(x, y + d), isoY(x, y + d));
  ctx.closePath();
};

export default function CityScene({
  plan, buildings, hot, selected, still, rate, sky = 'auto', onHover, onSelect, onReroll,
}: {
  plan: CityPlan; buildings: Building[]; hot: string | null; selected: string | null;
  still: boolean; rate: number; sky?: 'auto' | 'rain' | 'clear';
  onHover: (id: string | null) => void; onSelect: (id: string) => void;
  onReroll: () => void;
}) {
  const host = useRef<HTMLDivElement | null>(null);
  const canvas = useRef<HTMLCanvasElement | null>(null);
  // The loading bar stays up until a frame has been painted onto a sized
  // canvas: the first draw (ground layer, sprites, seeded streets) takes long
  // enough that a bare canvas read as a crash. `loading` then fades it out.
  const drawn = useRef(false);
  const [loading, setLoading] = useState<'drawing' | 'fading' | 'done'>('drawing');
  const markDrawn = () => {
    const el = canvas.current;
    if (drawn.current || !el || !el.width || !el.height) return;
    drawn.current = true;
    setLoading('fading');
  };
  useEffect(() => {
    if (loading !== 'fading') return;
    // A timer, not transitionend: a paused scene switches transitions off.
    const t = setTimeout(() => setLoading('done'), 320);
    return () => clearTimeout(t);
  }, [loading]);
  const [view, setView] = useState<View>({ scale: 1, ox: 0, oy: 0, w: 0, h: 0, dpr: 1 });
  const [boxes, setBoxes] = useState<Record<string, { box: Rect; clip: string; roof: [number, number] }>>({});

  const byId = useMemo(() => new Map(buildings.map((b) => [b.id, b])), [buildings]);
  const byName = useMemo(() => new Map(buildings.map((b) => [b.name.trim().toLowerCase(), b.id])), [buildings]);

  // Mutable scene state — never React state: cars must not be recreated by a
  // re-render, and a pulse is an animation, not a value to render.
  const pulses = useRef(new Map<string, Pulse[]>());
  const sparks = useRef<Spark[]>([]);
  const cars = useRef<Car[]>([]);
  const peds = useRef<Ped[]>([]);
  const clock = useRef(0);
  const style = useRef<Style | null>(null);
  const groundLayer = useRef<{ key: string; style: Style; canvas: HTMLCanvasElement } | null>(null);
  const pulsesSeen = useRef(0);
  // Requests per minute, mirrored into a ref: the street density follows the
  // real request rate, so a busy gateway visibly has a busy city.
  const rateRef = useRef(rate);
  rateRef.current = rate;
  const skyRef = useRef(sky);
  skyRef.current = sky;
  // Ambient weather and the plane are scenery: both are seeded from the city
  // seed, so "once in a while" is regular enough to see and stable per city.
  const weather = useRef<Weather>({ rain: false, until: 0 });
  const plane = useRef<Plane | null>(null);
  const planeAt = useRef(4 + (plan.seed % 9));
  const planesSeen = useRef(0);
  const reflected = useRef(0);

  // ── style: re-read when the theme class flips ────────────────────────────
  useEffect(() => {
    const el = host.current;
    if (!el) return;
    const read = () => { style.current = readStyle(el); };
    read();
    const mo = new MutationObserver(read);
    mo.observe(document.documentElement, { attributes: true, attributeFilter: ['class'] });
    return () => mo.disconnect();
  }, []);

  // ── heights + the world bounds the view fits into ────────────────────────
  const geometry = useMemo(() => {
    const heights = new Map<string, number>();
    let minX = Infinity, maxX = -Infinity, minY = Infinity, maxY = -Infinity;
    const grow = (x: number, y: number) => {
      minX = Math.min(minX, x); maxX = Math.max(maxX, x);
      minY = Math.min(minY, y); maxY = Math.max(maxY, y);
    };
    for (const s of plan.slots) {
      const b = byId.get(s.id);
      const h = buildingHeight(b?.level ?? 0, b?.state ?? 'unknown', ARCHETYPE[s.archetype].rise);
      heights.set(s.id, h);
      // Tall buildings carry an antenna 30% above the roof; the view must leave
      // room for it or the mast is cut off by the top of the stage.
      const top = (b?.state !== 'off' && h > 9) ? h * 1.3 : h;
      for (const [x, y] of [[s.x, s.y], [s.x + s.w, s.y], [s.x + s.w, s.y + s.d], [s.x, s.y + s.d]] as const) {
        grow(isoX(x, y), isoY(x, y));
        grow(isoX(x, y), isoY(x, y, top));
      }
    }
    for (const [x, y] of [[0, 0], [plan.w, 0], [plan.w, plan.d], [0, plan.d]] as const) grow(isoX(x, y), isoY(x, y));
    return { heights, bounds: { minX, maxX, minY, maxY } };
  }, [plan, byId]);

  const fit = useMemo(() => {
    const pad = 24;
    const { minX, maxX, minY, maxY } = geometry.bounds;
    if (!view.w || !view.h) return { scale: 1, ox: 0, oy: 0 };
    const scale = clamp(
      Math.min((view.w - pad * 2) / Math.max(1, maxX - minX), (view.h - pad * 2) / Math.max(1, maxY - minY)),
      0.14, 1.5,
    );
    return {
      scale,
      ox: (view.w - (maxX - minX) * scale) / 2 - minX * scale,
      oy: (view.h - (maxY - minY) * scale) / 2 - minY * scale,
    };
  }, [geometry, view.w, view.h]);

  // Hit targets: real buttons over the canvas, so pointer, touch, keyboard and
  // screen readers all reach a provider without touching the renderer. Each
  // target is clipped to the building's own silhouette (D_top → A_top → B_top →
  // B_ground → C_ground → D_ground), so a tall tower never covers the building
  // behind it and the hit area matches what the eye sees.
  useLayoutEffect(() => {
    const next: Record<string, { box: Rect; clip: string; roof: [number, number] }> = {};
    for (const s of plan.slots) {
      const h = geometry.heights.get(s.id) ?? 4;
      const x0 = s.x, y0 = s.y, x1 = s.x + s.w, y1 = s.y + s.d;
      const ring: [number, number][] = [
        [isoX(x0, y1), isoY(x0, y1, h)],
        [isoX(x0, y0), isoY(x0, y0, h)],
        [isoX(x1, y0), isoY(x1, y0, h)],
        [isoX(x1, y0), isoY(x1, y0)],
        [isoX(x1, y1), isoY(x1, y1)],
        [isoX(x0, y1), isoY(x0, y1)],
      ];
      const left = Math.min(...ring.map((p) => p[0])), right = Math.max(...ring.map((p) => p[0]));
      const top = Math.min(...ring.map((p) => p[1])), bottom = Math.max(...ring.map((p) => p[1]));
      const width = Math.max(6, (right - left) * fit.scale);
      const height = Math.max(6, (bottom - top) * fit.scale);
      const clip = `polygon(${ring.map(([x, y]) => `${(((x - left) * fit.scale) / width) * 100}% ${(((y - top) * fit.scale) / height) * 100}%`).join(', ')})`;
      next[s.id] = {
        box: { left: fit.ox + left * fit.scale, top: fit.oy + top * fit.scale, width, height },
        clip,
        // Screen point of the roof centre: the regression drivers sample the
        // canvas here to prove the tower is painted where the plan says.
        roof: [
          Math.round(fit.ox + isoX(s.x + s.w / 2, s.y + s.d / 2) * fit.scale),
          Math.round(fit.oy + isoY(s.x + s.w / 2, s.y + s.d / 2, h) * fit.scale),
        ] as [number, number],
      };
    }
    setBoxes(next);
  }, [plan, geometry, fit]);

  // ── element size ─────────────────────────────────────────────────────────
  useEffect(() => {
    const el = host.current;
    if (!el) return;
    const measure = () => {
      const r = el.getBoundingClientRect();
      const w = Math.max(1, Math.round(r.width));
      const h = Math.max(1, Math.round(r.height));
      const dpr = Math.min(2, window.devicePixelRatio || 1);
      setView((old) => (old.w === w && old.h === h && old.dpr === dpr ? old : { ...old, w, h, dpr }));
    };
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  useEffect(() => {
    const el = canvas.current;
    if (!el) return;
    const w = Math.round(view.w * view.dpr), h = Math.round(view.h * view.dpr);
    if (el.width !== w || el.height !== h) { el.width = w; el.height = h; }
  }, [view]);

  // ── traffic seeding ──────────────────────────────────────────────────────
  // Seeded once per city. The guard is the flag, not `cars.length`: a request
  // can dispatch a taxi before the first frame, and that must not stop the
  // city from seeding its streets and its people.
  const seeded = useRef(false);
  const ensureSeeded = () => {
    if (seeded.current) return;
    seeded.current = true;
    seedTraffic();
  };
  const seedTraffic = () => {
    const random = rng(plan.seed ^ 0x51ed);
    const total = clamp(6 + Math.round(plan.slots.length * 0.7), 6, 20);
    const next: Car[] = [];
    for (let i = 0; i < total; i++) {
      const axis: 0 | 1 = random() > 0.5 ? 0 : 1;
      const roads = axis === 0 ? plan.roadY : plan.roadX;
      const dir: 1 | -1 = random() > 0.5 ? 1 : -1;
      const extent = axis === 0 ? plan.w : plan.d;
      next.push({
        axis, dir, lane: roads[Math.floor(random() * roads.length)] + dir * LANE,
        pos: random() * extent, speed: 0, wish: 1.7 + random() * 2.6,
        tint: Math.floor(random() * 4), taxi: false, life: 0, fx: 0, fy: 0,
      });
    }
    cars.current = next;
    const people = rng(plan.seed ^ 0x2b1f);
    peds.current = plan.blocks.map((_, i) => ({
      block: i, t: people(), speed: 0.055 + people() * 0.075,
      dir: people() > 0.5 ? 1 : -1, phase: people() * 6.28,
      tint: Math.floor(people() * 4),
    }));
  };

  /** A dispatched car: it drives to its provider's street, then pulls in. */
  const dispatch = (slot: Slot, failed: boolean) => {
    if (cars.current.filter((c) => c.taxi).length >= 4) return;
    const alongX = slot.x + slot.w / 2 > plan.w / 2;
    const dir: 1 | -1 = failed ? -1 : slot.y + slot.d / 2 > plan.d / 2 ? -1 : 1;
    const roads = alongX ? plan.roadY : plan.roadX;
    const want = alongX ? slot.block.y + slot.block.d + 1.2 : slot.block.x + slot.block.w + 1.2;
    let road = roads[0];
    for (const r of roads) if (Math.abs(r - want) < Math.abs(road - want)) road = r;
    cars.current.push({
      axis: alongX ? 0 : 1, pos: (alongX ? slot.x : slot.y) - dir * 9, dir,
      lane: road + dir * LANE, speed: 6, wish: 6, tint: 2, taxi: true, life: 0,
      fx: slot.x + slot.w / 2, fy: slot.y + slot.d / 2,
    });
  };

  // A live ledger row flares that provider's building. The SSE feed is the
  // only per-request source the page has, so every flare is a committed call —
  // never a scheduled imitation of one.
  useEffect(() => subscribeLiveCalls((call: LiveCall) => {
    const id = byName.get(call.account.trim().toLowerCase());
    if (!id) return;
    const list = pulses.current.get(id) ?? [];
    list.push({ start: performance.now(), power: clamp(Math.log10(1 + call.tin + call.tout) / 6, 0.25, 1) });
    pulses.current.set(id, list.slice(-MAX_PULSES));
    pulsesSeen.current += 1;
    const slot = plan.slots.find((s) => s.id === id);
    if (slot) dispatch(slot, call.status >= 400);
  }), [byName, plan]);

  // ── ground layer ─────────────────────────────────────────────────────────
  const paintGround = (g: CanvasRenderingContext2D) => {
    const s = style.current ?? readStyle(host.current ?? document.body);
    const px = 1 / fit.scale;
    // Land: the city sits on a plateau, so the diamond is a place, not a chart.
    const pad = 3.2;
    g.beginPath();
    ([[-pad, -pad], [plan.w + pad, -pad], [plan.w + pad, plan.d + pad], [-pad, plan.d + pad]] as const)
      .forEach(([x, y], i) => (i ? g.lineTo(isoX(x, y), isoY(x, y)) : g.moveTo(isoX(x, y), isoY(x, y))));
    g.closePath();
    g.fillStyle = s.land;
    g.fill();

    const turf = grain(g, s.treeDark, s.windowOn, 11, 150);
    const tarmac = grain(g, s.shadow, s.line, 23, 90);
    for (const b of plan.blocks) {
      quadPath(g, b.x, b.y, b.w, b.d);
      g.fillStyle = s.ground;
      g.fill();
      if (turf) { g.fillStyle = turf; g.fill(); }
      quadPath(g, b.x + 0.16, b.y + 0.16, b.w - 0.32, b.d - 0.32);
      g.strokeStyle = s.kerb;
      g.globalAlpha = 0.32;
      g.lineWidth = Math.max(0.5, px * 0.7);
      g.stroke();
      g.globalAlpha = 1;
    }

    // Streets: one quad per road, full length, plus lane dashes.
    g.fillStyle = s.road;
    for (const rx of plan.roadX) { quadPath(g, rx - 1.4, 0, 2.8, plan.d); g.fill(); }
    for (const ry of plan.roadY) { quadPath(g, 0, ry - 1.4, plan.w, 2.8); g.fill(); }
    if (tarmac) {
      g.fillStyle = tarmac;
      for (const rx of plan.roadX) { quadPath(g, rx - 1.4, 0, 2.8, plan.d); g.fill(); }
      for (const ry of plan.roadY) { quadPath(g, 0, ry - 1.4, plan.w, 2.8); g.fill(); }
    }
    // Zebra crossings on every arm of every junction, one path, one fill.
    g.fillStyle = s.line;
    g.globalAlpha = 0.5;
    g.beginPath();
    const bar = (x: number, y: number, w: number, d: number) => {
      g.moveTo(isoX(x, y), isoY(x, y)); g.lineTo(isoX(x + w, y), isoY(x + w, y));
      g.lineTo(isoX(x + w, y + d), isoY(x + w, y + d)); g.lineTo(isoX(x, y + d), isoY(x, y + d)); g.closePath();
    };
    for (const rx of plan.roadX) {
      for (const ry of plan.roadY) {
        for (let k = -2; k <= 2; k++) {
          const o = k * 0.52;
          for (const e of [-1, 1]) {
            bar(rx + o - 0.14, ry + e * 1.95 - 0.28, 0.28, 0.56);
            bar(rx + e * 1.95 - 0.28, ry + o - 0.14, 0.56, 0.28);
          }
        }
      }
    }
    g.fill();
    g.globalAlpha = 1;
    g.strokeStyle = s.line;
    g.globalAlpha = 0.45;
    g.lineWidth = Math.max(0.4, px * 0.8);
    g.setLineDash([1.1, 1.9]);
    g.beginPath();
    for (const rx of plan.roadX) { g.moveTo(isoX(rx, 0), isoY(rx, 0)); g.lineTo(isoX(rx, plan.d), isoY(rx, plan.d)); }
    for (const ry of plan.roadY) { g.moveTo(isoX(0, ry), isoY(0, ry)); g.lineTo(isoX(plan.w, ry), isoY(plan.w, ry)); }
    g.stroke();
    g.setLineDash([]);
    g.globalAlpha = 1;
  };

  // ── the frame ────────────────────────────────────────────────────────────
  const draw = (ctx: CanvasRenderingContext2D, now: number) => {
    const s = style.current ?? readStyle(host.current ?? document.body);
    const { scale, ox, oy } = fit;
    const dpr = view.dpr;
    ctx.setTransform(1, 0, 0, 1, 0, 0);
    ctx.clearRect(0, 0, Math.round(view.w * dpr), Math.round(view.h * dpr));

    // Horizon first, in screen pixels: two hill bands behind the plateau turn
    // the empty triangles either side of the diamond into landscape, so the
    // city sits in a place instead of floating in the page.
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    const hz = view.h * 0.62;
    const band = (color: string, amp: number, phase: number) => {
      ctx.beginPath();
      ctx.moveTo(0, view.h);
      for (let x = 0; x <= view.w; x += 24) {
        ctx.lineTo(x, hz + Math.sin((x / view.w) * Math.PI * 2.2 + phase) * amp - amp * 0.4);
      }
      ctx.lineTo(view.w, view.h);
      ctx.closePath();
      const haze = ctx.createLinearGradient(0, hz - amp, 0, view.h);
      haze.addColorStop(0, `${color}`);
      haze.addColorStop(1, `${s.land}`);
      ctx.fillStyle = haze;
      ctx.fill();
    };
    band(s.hillFar, 26, 0.7);
    band(s.hillNear, 14, 2.3);
    // A plane crossing the sky is drawn behind the world, so a tower really
    // does occlude it.
    drawPlane(ctx, s);

    ctx.setTransform(dpr * scale, 0, 0, dpr * scale, dpr * ox, dpr * oy);
    const px = 1 / scale; // one CSS pixel in world units
    const heights = geometry.heights;

    // The ground never moves, so it is painted once into an offscreen layer
    // and blitted: textures and crossings cost nothing per frame. The key is
    // everything that changes it: size, fit, palette and city.
    const groundKey = `${view.w}x${view.h}@${dpr}|${scale}|${ox}|${oy}|${plan.seed}|${plan.blocks.length}`;
    let layer = groundLayer.current;
    if (!layer || layer.key !== groundKey || layer.style !== s) {
      const off = layer?.canvas ?? document.createElement('canvas');
      off.width = Math.round(view.w * dpr); off.height = Math.round(view.h * dpr);
      const g = off.getContext('2d');
      if (g) {
        g.setTransform(dpr * scale, 0, 0, dpr * scale, dpr * ox, dpr * oy);
        paintGround(g);
      }
      layer = { key: groundKey, style: s, canvas: off };
      groundLayer.current = layer;
    }
    ctx.setTransform(1, 0, 0, 1, 0, 0);
    if (layer.canvas.width && layer.canvas.height) ctx.drawImage(layer.canvas, 0, 0);
    ctx.setTransform(dpr * scale, 0, 0, dpr * scale, dpr * ox, dpr * oy);

    // Weather on the ground. Rain lays a sheen over the streets, and a lit
    // building throws its windows down the road in front of it at night — the
    // city's own light, never a number.
    reflected.current = 0;
    const wet = weather.current.rain;
    if (wet) {
      ctx.globalAlpha = 0.12;
      ctx.fillStyle = s.line;
      for (const rx of plan.roadX) { quadPath(ctx, rx - 1.4, 0, 2.8, plan.d); ctx.fill(); }
      for (const ry of plan.roadY) { quadPath(ctx, 0, ry - 1.4, plan.w, 2.8); ctx.fill(); }
      ctx.globalAlpha = 1;
    }
    if (s.night || wet) {
      for (const slot of plan.slots) {
        const b = byId.get(slot.id);
        const level = b?.level ?? 0;
        if (!b || b.state === 'off' || b.state === 'unknown' || level <= 0.03) continue;
        reflected.current += 1;
        const fx = isoX(slot.x + slot.w, slot.y + slot.d);
        const fy = isoY(slot.x + slot.w, slot.y + slot.d);
        const w2 = (slot.w + slot.d) * TU * 0.34;
        const tall = (10 + level * 22) * px * (wet ? 1.35 : 1);
        const grad = ctx.createLinearGradient(fx, fy, fx, fy + tall);
        grad.addColorStop(0, `${s.windowOn}55`);
        grad.addColorStop(1, `${s.windowOn}00`);
        ctx.globalAlpha = (0.1 + level * 0.3) * (s.night ? 1 : 0.5);
        ctx.fillStyle = grad;
        ctx.fillRect(fx - w2 / 2, fy, w2, tall);
        ctx.globalAlpha = 1;
      }
    }
    // Street lamps at every block's front corner: a post and a head.
    ctx.lineWidth = Math.max(0.5, px * 0.9);
    for (const b of plan.blocks) {
      const lx = isoX(b.x + b.w + 0.35, b.y + b.d + 0.35), ly = isoY(b.x + b.w + 0.35, b.y + b.d + 0.35);
      ctx.strokeStyle = s.roofEdge;
      ctx.beginPath(); ctx.moveTo(lx, ly); ctx.lineTo(lx, ly - 0.95 * Z); ctx.stroke();
      ctx.fillStyle = s.night ? s.windowOn : s.roofTop;
      ctx.beginPath(); ctx.arc(lx, ly - 0.98 * Z, Math.max(0.9, px * 1.3), 0, Math.PI * 2); ctx.fill();
    }
    if (s.night) {
      for (const slot of plan.slots) {
        const x = slot.block.x + slot.block.w + 0.35;
        const y = slot.block.y + slot.block.d + 0.35;
        glowAt(ctx, s.glow, isoX(x, y), isoY(x, y), 40 * px, 0.14);
      }
    }

    // One depth-sorted pass. Depth is x + y (isometric distance from the back
    // corner); a building sorts at its FRONT corner so traffic in front of its
    // facade draws after it and traffic behind draws before.
    type Item = { d: number; k: 0 | 1 | 2 | 3; i: number };
    const items: Item[] = [];
    plan.slots.forEach((slot, i) => items.push({ d: slot.x + slot.w + slot.y + slot.d, k: 0, i }));
    cars.current.forEach((_, i) => items.push({ d: carPoint(cars.current[i])[0] + carPoint(cars.current[i])[1], k: 1, i }));
    plan.trees.forEach((t, i) => items.push({ d: t.x + t.y, k: 2, i }));
    peds.current.forEach((p, i) => { const [x, y] = pedAt(p); items.push({ d: x + y, k: 3, i }); });
    items.sort((a, b) => a.d - b.d);

    for (const item of items) {
      if (item.k === 0) {
        const slot = plan.slots[item.i];
        drawBuilding(ctx, s, slot, byId.get(slot.id), heights.get(slot.id) ?? 4, now, px);
      } else if (item.k === 1) drawCar(ctx, s, cars.current[item.i], px);
      else if (item.k === 2) drawTree(ctx, s, plan.trees[item.i], px);
      else drawPed(ctx, s, peds.current[item.i], pedAt(peds.current[item.i]), px);
    }

    for (const slot of plan.slots) {
      if (byId.get(slot.id)?.storm) drawStorm(ctx, s, slot, heights.get(slot.id) ?? 4, px);
    }

    for (const spark of sparks.current) {
      const p = clamp((now - spark.start) / spark.life, 0, 1);
      ctx.globalAlpha = (1 - p) * 0.8;
      ctx.beginPath();
      ctx.arc(isoX(spark.x, spark.y), isoY(spark.x, spark.y, spark.z), (0.5 + p * 2.4) * TU, 0, Math.PI * 2);
      ctx.strokeStyle = s.glow;
      ctx.lineWidth = Math.max(0.6, px * (1.2 - p));
      ctx.stroke();
      ctx.globalAlpha = 1;
    }

    // Labels are screen-space text: crisp at any zoom, and they carry data (a
    // provider name), so they stay even in the still frame. Labels sit on the
    // street in front of their block and are dropped lowest-traffic-first when
    // two would collide — the busiest buildings always keep their name.
    ctx.font = `500 ${10.5 * px}px "Public Sans Variable", system-ui, sans-serif`;
    ctx.textAlign = 'center';
    const placed: { x0: number; x1: number; y0: number; y1: number }[] = [];
    const order = [...plan.slots].sort((a, b) => (byId.get(b.id)?.level ?? 0) - (byId.get(a.id)?.level ?? 0));
    for (const slot of order) {
      const b = byId.get(slot.id);
      if (!b) continue;
      const active = hot === slot.id || selected === slot.id;
      const sx = isoX(slot.x + slot.w / 2, slot.y + slot.d);
      const sy = isoY(slot.x + slot.w / 2, slot.y + slot.d) + 9 * px;
      const halfW = (b.short.length * 5.4) / 2;
      const box = { x0: sx - halfW, x1: sx + halfW, y0: sy - 9 * px, y1: sy + 3 * px };
      const hits = placed.some((p) => box.x0 < p.x1 && box.x1 > p.x0 && box.y0 < p.y1 && box.y1 > p.y0);
      if (hits && !active) continue;
      placed.push(box);
      ctx.globalAlpha = active ? 1 : 0.62;
      ctx.lineWidth = 3 * px;
      ctx.strokeStyle = s.land;
      ctx.strokeText(b.short, sx, sy);
      ctx.fillStyle = active ? s.ink : s.label;
      ctx.fillText(b.short, sx, sy);
      ctx.globalAlpha = 1;
    }

    // Rain falls over the whole city, in screen pixels so the streaks keep
    // their weight whatever the fit does to the world.
    if (weather.current.rain) {
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      drawRain(ctx, s, view.w, view.h, now);
    }
  };

  // ── geometry helpers used by the pass above ──────────────────────────────
  const carPoint = (car: Car): [number, number] => car.axis === 0
    ? [clamp(car.pos, -6, plan.w + 6), car.lane]
    : [car.lane, clamp(car.pos, -6, plan.d + 6)];

  const pedAt = (p: Ped): [number, number] => {
    const b = plan.blocks[p.block];
    const x0 = b.x + 0.55, y0 = b.y + 0.55, x1 = b.x + b.w - 0.55, y1 = b.y + b.d - 0.55;
    const w = x1 - x0, d = y1 - y0;
    let at = p.t * 2 * (w + d);
    if (at < w) return [x0 + at, y0];
    at -= w;
    if (at < d) return [x1, y0 + at];
    at -= d;
    if (at < w) return [x1 - at, y1];
    at -= w;
    return [x0, y1 - at];
  };

  function drawBuilding(
    ctx: CanvasRenderingContext2D, s: Style, slot: Slot, b: Building | undefined,
    height: number, now: number, px: number,
  ) {
    const state = b?.state ?? 'unknown';
    const active = hot === slot.id || selected === slot.id;
    const wall = s.wall[state === 'off' ? 3 : slot.tint];
    const side = s.side[state === 'off' ? 3 : slot.tint];
    const zTop = height * Z;

    const pB = [isoX(slot.x + slot.w, slot.y), isoY(slot.x + slot.w, slot.y)] as const;
    const pC = [isoX(slot.x + slot.w, slot.y + slot.d), isoY(slot.x + slot.w, slot.y + slot.d)] as const;
    const pD = [isoX(slot.x, slot.y + slot.d), isoY(slot.x, slot.y + slot.d)] as const;
    const cx = isoX(slot.x + slot.w / 2, slot.y + slot.d / 2);
    const cy = isoY(slot.x + slot.w / 2, slot.y + slot.d / 2);

    ctx.globalAlpha = 0.32;
    ctx.beginPath();
    ctx.ellipse(cx + 4 * px, cy + 3 * px, (slot.w * TU) / 2, (slot.d * TU) / 4.4, 0, 0, Math.PI * 2);
    ctx.fillStyle = s.shadow;
    ctx.fill();
    ctx.globalAlpha = 1;

    // A face is an affine space: local x runs along the facade, local y is
    // straight down it. Windows and bands are then plain fillRects.
    const face = (ox2: number, oy2: number, bx: number, by: number, width: number, fill: string) => {
      ctx.save();
      ctx.transform(bx / width, by / width, 0, 1, ox2, oy2);
      ctx.fillStyle = fill;
      ctx.fillRect(0, 0, width, zTop);
      // Light falls from the sky: a pale top, a dark base where the street
      // shades the wall, a cornice course under the roof and a plinth at grade.
      const g = ctx.createLinearGradient(0, 0, 0, zTop);
      g.addColorStop(0, 'rgba(255,255,255,0.2)');
      g.addColorStop(0.35, 'rgba(255,255,255,0)');
      g.addColorStop(1, `${s.shadow}66`);
      ctx.fillStyle = g;
      ctx.fillRect(0, 0, width, zTop);
      ctx.fillStyle = s.roofTop;
      ctx.globalAlpha = 0.5;
      ctx.fillRect(0, 0, width, Math.max(1.2, zTop * 0.035));
      ctx.fillStyle = s.shadow;
      ctx.globalAlpha = 0.3;
      ctx.fillRect(0, zTop - Math.max(1.2, zTop * 0.04), width, Math.max(1.2, zTop * 0.04));
      ctx.globalAlpha = 1;
    };

    face(pC[0], pC[1] - zTop, pB[0] - pC[0], pB[1] - pC[1], (slot.d * TU) / 2, side);
    windowsIn(ctx, s, slot, b, (slot.d * TU) / 2, zTop, height, now, 1, px);
    ctx.restore();

    face(pD[0], pD[1] - zTop, pC[0] - pD[0], pC[1] - pD[1], (slot.w * TU) / 2, wall);
    windowsIn(ctx, s, slot, b, (slot.w * TU) / 2, zTop, height, now, 2, px);
    ctx.restore();

    // The vertical corner catches the light.
    ctx.strokeStyle = '#ffffff';
    ctx.globalAlpha = s.night ? 0.14 : 0.45;
    ctx.lineWidth = Math.max(0.5, px);
    ctx.beginPath(); ctx.moveTo(pC[0], pC[1]); ctx.lineTo(pC[0], pC[1] - zTop); ctx.stroke();
    ctx.globalAlpha = 1;

    // Roof slab.
    const roof: [number, number][] = [
      [isoX(slot.x, slot.y), isoY(slot.x, slot.y, height)],
      [isoX(slot.x + slot.w, slot.y), isoY(slot.x + slot.w, slot.y, height)],
      [isoX(slot.x + slot.w, slot.y + slot.d), isoY(slot.x + slot.w, slot.y + slot.d, height)],
      [isoX(slot.x, slot.y + slot.d), isoY(slot.x, slot.y + slot.d, height)],
    ];
    ctx.beginPath();
    roof.forEach(([x, y], i) => (i ? ctx.lineTo(x, y) : ctx.moveTo(x, y)));
    ctx.closePath();
    ctx.fillStyle = s.roof;
    ctx.fill();
    ctx.strokeStyle = s.roofEdge;
    ctx.lineWidth = Math.max(0.4, px * 0.8);
    ctx.stroke();
    // Parapet: an inset deck a shade lighter than the rim.
    const rcx = (roof[0][0] + roof[2][0]) / 2, rcy = (roof[0][1] + roof[2][1]) / 2;
    ctx.beginPath();
    roof.forEach(([x, y], i) => {
      const ix = rcx + (x - rcx) * 0.82, iy = rcy + (y - rcy) * 0.82;
      if (i) ctx.lineTo(ix, iy); else ctx.moveTo(ix, iy);
    });
    ctx.closePath();
    ctx.fillStyle = s.roofTop;
    ctx.globalAlpha = 0.42;
    ctx.fill();
    ctx.globalAlpha = 1;

    // Roof plant: air-handling boxes and, on a tall building, an antenna with
    // a beacon that blinks at night.
    ctx.fillStyle = s.roofTop;
    const boxes = 1 + (slot.seed % 3);
    for (let i = 0; i < boxes; i++) {
      const gx = slot.x + slot.w * (0.3 + shade(slot.seed, i) * 0.4);
      const gy = slot.y + slot.d * (0.3 + shade(slot.seed, i + 9) * 0.4);
      const w = (0.34 + shade(slot.seed, i + 3) * 0.3) * TU;
      ctx.fillRect(isoX(gx, gy) - w / 2, isoY(gx, gy, height) - w / 4, w, w / 2);
    }
    const roofCx = isoX(slot.x + slot.w / 2, slot.y + slot.d / 2);
    const roofCy = isoY(slot.x + slot.w / 2, slot.y + slot.d / 2, height);
    if (height > 9 && state !== 'off') {
      ctx.strokeStyle = s.roofEdge;
      ctx.lineWidth = Math.max(0.4, px);
      ctx.beginPath();
      ctx.moveTo(roofCx, roofCy);
      ctx.lineTo(roofCx, roofCy - height * Z * 0.3);
      ctx.stroke();
      const blink = s.night && Math.sin(now / 520) > 0.3;
      ctx.beginPath();
      ctx.arc(roofCx, roofCy - height * Z * 0.3, Math.max(0.5, px * 1.3), 0, Math.PI * 2);
      ctx.globalAlpha = blink ? 0.95 : 0.4;
      ctx.fillStyle = blink ? s.bad : s.roofTop;
      ctx.fill();
      ctx.globalAlpha = 1;
    }

    // Request flare. The building is already lit by its own traffic, so the
    // pulse is deliberately brighter and short: one call, one struck match.
    const list = pulses.current.get(slot.id);
    if (list?.length) {
      for (const pulse of list) {
        const p = (now - pulse.start) / DUR;
        if (p < 0 || p > 1) continue;
        const fade = 1 - ease((p - 0.3) / 0.7);
        glowAt(ctx, s.glow, roofCx, roofCy, 30 * px * (0.7 + ease(p * 2)), fade * 0.42);
        if (pulse.power > 0.55) {
          const top = roofCy - 36 * (0.6 + pulse.power);
          const grad = ctx.createLinearGradient(roofCx, roofCy, roofCx, top);
          grad.addColorStop(0, `${s.glow}88`);
          grad.addColorStop(1, `${s.glow}00`);
          ctx.globalAlpha = fade * 0.3;
          ctx.fillStyle = grad;
          ctx.fillRect(roofCx - 5 * px, top, 10 * px, roofCy - top);
          ctx.globalAlpha = 1;
        }
      }
    }

    if (active) {
      ctx.beginPath();
      roof.forEach(([x, y], i) => (i ? ctx.lineTo(x, y) : ctx.moveTo(x, y)));
      ctx.closePath();
      ctx.strokeStyle = s.accent;
      ctx.globalAlpha = 0.8;
      ctx.lineWidth = Math.max(0.6, px * 1.2);
      ctx.stroke();
      quadPath(ctx, slot.x, slot.y, slot.w, slot.d);
      ctx.globalAlpha = 0.14;
      ctx.fillStyle = s.accent;
      ctx.fill();
      ctx.globalAlpha = 1;
    } else if (state === 'unknown') {
      ctx.setLineDash([4 * px, 3 * px]);
      quadPath(ctx, slot.x, slot.y, slot.w, slot.d);
      ctx.strokeStyle = s.label;
      ctx.globalAlpha = 0.45;
      ctx.lineWidth = Math.max(0.5, px);
      ctx.stroke();
      ctx.setLineDash([]);
      ctx.globalAlpha = 1;
    }
  }

  /** Window panes for one facade, in that facade's affine space. */
  function windowsIn(
    ctx: CanvasRenderingContext2D, s: Style, slot: Slot, b: Building | undefined,
    width: number, heightPx: number, heightUnits: number, now: number, face: number, px: number,
  ) {
    if (width < 8 || heightPx < 6) return;
    const state = b?.state ?? 'unknown';
    const level = b?.level ?? 0;
    const rows = clamp(Math.floor(heightUnits / ARCHETYPE[slot.archetype].floors), 1, 16);
    const cols = clamp(Math.floor(width / (13 * px)), 2, 8);
    const cellW = width / cols;
    const cellH = heightPx / rows;
    const winW = cellW * 0.56;
    const winH = cellH * 0.46;
    const dark = state === 'off' || state === 'unknown';
    const list = pulses.current.get(slot.id) ?? [];

    // Street level: a lobby band, lit when the provider is serving.
    if (!dark) {
      ctx.fillStyle = s.door;
      ctx.globalAlpha = 0.9;
      ctx.fillRect(0, heightPx - cellH * 0.95, width, cellH * 0.95);
      ctx.fillStyle = level > 0.03 ? s.windowOn : s.windowOff;
      ctx.globalAlpha = level > 0.03 ? 0.18 + level * 0.45 : 0.6;
      ctx.fillRect(width * 0.16, heightPx - cellH * 0.72, width * 0.68, cellH * 0.46);
      ctx.globalAlpha = 1;
    }

    for (let r = 0; r < rows; r++) {
      for (let c = 0; c < cols; c++) {
        const thr = shade(slot.seed + face * 977, r * 31 + c);
        const x = c * cellW + (cellW - winW) / 2;
        const y = r * cellH + (cellH - winH) / 2;
        let lit = !dark && level > 0 && thr < level * 0.95;
        let alpha = lit ? 0.4 + level * 0.5 : 0;
        for (const pulse of list) {
          const p = (now - pulse.start) / DUR;
          if (p < 0 || p > 1) continue;
          // The wave climbs from the pavement: the request arriving.
          const cascade = clamp(p / 0.32, 0, 1);
          if (1 - r / rows <= cascade) {
            lit = true;
            const decay = p < 0.32 ? 1 : 1 - ease((p - 0.32) / 0.68);
            alpha = Math.max(alpha, 0.3 + decay * 0.7 * pulse.power);
          }
        }
        if (lit) {
          const flicker = 0.92 + Math.sin(now / 900 + thr * 40) * 0.08;
          ctx.fillStyle = s.windowOn;
          ctx.globalAlpha = clamp(alpha * flicker, 0, 1);
        } else {
          ctx.fillStyle = s.windowOff;
          ctx.globalAlpha = dark ? 0.45 : 0.78;
        }
        ctx.fillRect(x, y, winW, winH);
        if (!lit && !dark) {
          // Sky reflected in the glass, upper half only.
          ctx.fillStyle = '#ffffff';
          ctx.globalAlpha = s.night ? 0.05 : 0.22;
          ctx.fillRect(x, y, winW, winH * 0.45);
        }
      }
    }
    ctx.globalAlpha = 1;

    // A disabled provider is boarded up, not merely dark.
    if (state === 'off' && rows > 1) {
      ctx.strokeStyle = s.treeDark;
      ctx.globalAlpha = 0.7;
      ctx.lineWidth = Math.max(0.7, px * 1.1);
      ctx.beginPath();
      for (let r = 0; r < rows; r += 2) {
        ctx.moveTo(width * 0.14, (r + 0.55) * cellH);
        ctx.lineTo(width * 0.86, (r + 0.55) * cellH + cellH * 0.3);
      }
      ctx.stroke();
      ctx.globalAlpha = 1;
    }
  }

  // Street furniture shares one scale with the buildings: a height unit is one
  // storey (~3 m, Z px), so a car is ~1.5 units long and 0.5 high, a person
  // ~0.55 high and a street tree ~2.2.
  const pt = (x: number, y: number, z = 0): [number, number] => [isoX(x, y), isoY(x, y, z)];
  const poly = (ctx: CanvasRenderingContext2D, pts: [number, number][], fill: string, shadeA = 0, shadeC = '') => {
    ctx.beginPath();
    pts.forEach(([x, y], i) => (i ? ctx.lineTo(x, y) : ctx.moveTo(x, y)));
    ctx.closePath();
    ctx.fillStyle = fill;
    ctx.fill();
    if (shadeA) { ctx.globalAlpha = shadeA; ctx.fillStyle = shadeC; ctx.fill(); ctx.globalAlpha = 1; }
  };
  /** An axis-aligned box in world space: top, the y=y1 wall and the x=x1 wall. */
  const box = (
    ctx: CanvasRenderingContext2D, s: Style, x0: number, y0: number, x1: number, y1: number,
    z0: number, z1: number, color: string, glass = false,
  ) => {
    const wall = glass ? s.carWindow : color;
    poly(ctx, [pt(x0, y1, z0), pt(x1, y1, z0), pt(x1, y1, z1), pt(x0, y1, z1)], wall, glass ? 0.25 : 0.08, s.shadow);
    poly(ctx, [pt(x1, y0, z0), pt(x1, y1, z0), pt(x1, y1, z1), pt(x1, y0, z1)], wall, glass ? 0.5 : 0.32, s.shadow);
    poly(ctx, [pt(x0, y0, z1), pt(x1, y0, z1), pt(x1, y1, z1), pt(x0, y1, z1)], color, 0.16, '#ffffff');
  };

  function drawCar(ctx: CanvasRenderingContext2D, s: Style, car: Car, px: number) {
    const [x, y] = carPoint(car);
    const body = car.taxi ? s.car[2] : s.car[car.tint];
    const along = car.axis === 0;
    const L = 0.95, W = 0.4; // half length / half width, world units
    const hx = along ? L : W, hy = along ? W : L;
    const x0 = x - hx, x1 = x + hx, y0 = y - hy, y1 = y + hy;

    ctx.globalAlpha = 0.3;
    poly(ctx, [pt(x0 + 0.1, y0 + 0.1), pt(x1 + 0.1, y0 + 0.1), pt(x1 + 0.1, y1 + 0.1), pt(x0 + 0.1, y1 + 0.1)], s.shadow);
    ctx.globalAlpha = 1;

    box(ctx, s, x0, y0, x1, y1, 0.07, 0.3, body);
    // Cabin: shorter than the body and set back from the nose.
    const sh = L * 0.34, back = L * 0.1, c = car.dir;
    const cx0 = along ? x - sh + (c > 0 ? -back : back) : x0 + 0.06;
    const cx1 = along ? x + sh + (c > 0 ? -back : back) : x1 - 0.06;
    const cy0 = along ? y0 + 0.06 : y - sh + (c > 0 ? -back : back);
    const cy1 = along ? y1 - 0.06 : y + sh + (c > 0 ? -back : back);
    box(ctx, s, cx0, cy0, cx1, cy1, 0.3, 0.5, body, true);
    poly(ctx, [pt(cx0, cy0, 0.5), pt(cx1, cy0, 0.5), pt(cx1, cy1, 0.5), pt(cx0, cy1, 0.5)], body, 0.16, '#ffffff');

    // Wheels: dark pads under the body on the two visible sides.
    ctx.fillStyle = s.ink;
    ctx.globalAlpha = 0.7;
    for (const t of [-0.5, 0.5]) {
      const wx = along ? x + t * L * 1.2 : x1, wy = along ? y1 : y + t * L * 1.2;
      const [qx, qy] = pt(wx, wy, 0.1);
      ctx.beginPath(); ctx.ellipse(qx, qy, 2.3, 2.3, 0, 0, Math.PI * 2); ctx.fill();
    }
    ctx.globalAlpha = 1;

    // Lamps at both ends, on the nose and tail faces' corners.
    const nose = along ? [x + c * hx, y + hy] : [x + hx, y + c * hy];
    const tail = along ? [x - c * hx, y + hy] : [x + hx, y - c * hy];
    const [nx, ny] = pt(nose[0], nose[1], 0.2);
    const [tx, ty] = pt(tail[0], tail[1], 0.2);
    if (s.night) glowAt(ctx, s.glow, nx, ny, 9 * px, 0.35);
    ctx.fillStyle = s.night ? s.windowOn : s.carWindow;
    ctx.fillRect(nx - 1, ny - 1, 2, 2);
    ctx.fillStyle = s.tail;
    ctx.fillRect(tx - 1, ty - 1, 2, 2);

    if (car.taxi) {
      // The dispatched car wears a lit roof sign: a request is recognisable by day.
      const [rx, ry] = pt(x, y, 0.62);
      ctx.fillStyle = s.windowOn;
      ctx.fillRect(rx - 3, ry - 1.5, 6, 3);
    }
  }

  /** A person on the pavement: ~0.55 of a storey, legs that swing, umbrella in rain. */
  function drawPed(
    ctx: CanvasRenderingContext2D, s: Style, p: Ped, at: [number, number], px: number,
  ) {
    const [x, y] = at;
    const sx = isoX(x, y), sy = isoY(x, y);
    const step = Math.sin(clock.current * 7 + p.phase);
    const skin = s.car[p.tint];
    const h = 0.66 * Z; // 1.7 m on a 3 m storey, nudged up to stay legible

    ctx.globalAlpha = 0.25;
    ctx.beginPath();
    ctx.ellipse(sx + 1, sy + 0.5, h * 0.3, h * 0.1, 0, 0, Math.PI * 2);
    ctx.fillStyle = s.shadow;
    ctx.fill();
    ctx.globalAlpha = 1;

    const hip = sy - h * 0.45;
    ctx.strokeStyle = s.ink;
    ctx.globalAlpha = 0.75;
    ctx.lineWidth = Math.max(0.6, px * 0.9);
    ctx.beginPath();
    ctx.moveTo(sx, hip); ctx.lineTo(sx + step * h * 0.14, sy);
    ctx.moveTo(sx, hip); ctx.lineTo(sx - step * h * 0.14, sy);
    ctx.stroke();
    ctx.globalAlpha = 1;
    ctx.fillStyle = skin;
    ctx.fillRect(sx - h * 0.15, sy - h * 0.86, h * 0.3, h * 0.44);
    ctx.beginPath();
    ctx.arc(sx, sy - h * 0.94, h * 0.12, 0, Math.PI * 2);
    ctx.fill();

    if (weather.current.rain) {
      ctx.fillStyle = s.bad;
      ctx.globalAlpha = 0.7;
      ctx.beginPath();
      ctx.arc(sx, sy - h * 1.02, h * 0.34, Math.PI, 0);
      ctx.closePath();
      ctx.fill();
      ctx.globalAlpha = 1;
    }
  }

  /** A street tree: ~2.2 storeys tall with a layered canopy. */
  function drawTree(ctx: CanvasRenderingContext2D, s: Style, t: { x: number; y: number; seed: number }, _px: number) {
    const sx = isoX(t.x, t.y), sy = isoY(t.x, t.y);
    const size = 0.85 + (t.seed % 4) * 0.12;
    const r = 0.42 * Z * size;
    const trunkH = 0.85 * Z * size;
    ctx.globalAlpha = 0.25;
    ctx.beginPath();
    ctx.ellipse(sx + r * 0.35, sy + r * 0.2, r * 1.2, r * 0.45, 0, 0, Math.PI * 2);
    ctx.fillStyle = s.shadow;
    ctx.fill();
    ctx.globalAlpha = 1;
    ctx.fillStyle = s.trunk;
    ctx.fillRect(sx - Math.max(0.8, r * 0.1), sy - trunkH, Math.max(1.6, r * 0.2), trunkH);
    const blob = (dx: number, dy: number, rr: number, color: string, a = 1) => {
      ctx.beginPath();
      ctx.arc(sx + dx * r, sy - trunkH - dy * r, rr * r, 0, Math.PI * 2);
      ctx.fillStyle = color;
      ctx.globalAlpha = a;
      ctx.fill();
      ctx.globalAlpha = 1;
    };
    blob(0, 0.2, 1, s.treeDark);
    blob(0.6, 0.0, 0.7, s.treeDark);
    blob(-0.55, 0.7, 0.8, s.tree);
    blob(0.25, 1.0, 0.7, s.tree);
    blob(-0.45, 1.25, 0.28, '#ffffff', s.night ? 0.05 : 0.2);
  }

  /** The plane in the sky: screen-space scenery with a fading contrail. */
  function drawPlane(ctx: CanvasRenderingContext2D, s: Style) {
    const p = plane.current;
    if (!p) return;
    const u = 17; // fuselage half-length, screen pixels
    if (p.trail.length > 1) {
      ctx.beginPath();
      p.trail.forEach(([tx, ty], i) => (i ? ctx.lineTo(tx, ty) : ctx.moveTo(tx, ty)));
      ctx.strokeStyle = s.cloud;
      ctx.globalAlpha = 0.2;
      ctx.lineWidth = 1.6;
      ctx.stroke();
      ctx.globalAlpha = 1;
    }
    ctx.save();
    ctx.translate(p.x, p.y);
    ctx.scale(p.dir, 1);
    ctx.fillStyle = s.night ? s.smoke : s.label;
    ctx.globalAlpha = 0.92;
    ctx.beginPath();
    ctx.ellipse(0, 0, u, u * 0.15, 0, 0, Math.PI * 2);
    ctx.fill();
    ctx.beginPath();
    ctx.moveTo(-u * 0.08, -u * 0.06); ctx.lineTo(-u * 0.62, -u * 0.58); ctx.lineTo(-u * 0.3, -u * 0.02); ctx.closePath();
    ctx.moveTo(-u * 0.08, u * 0.06); ctx.lineTo(-u * 0.62, u * 0.58); ctx.lineTo(-u * 0.3, u * 0.02); ctx.closePath();
    ctx.moveTo(-u * 0.8, -u * 0.04); ctx.lineTo(-u * 1.08, -u * 0.38); ctx.lineTo(-u * 0.94, -u * 0.02); ctx.closePath();
    ctx.fill();
    // A row of lit windows, and the beacon that blinks over the hull.
    ctx.fillStyle = s.windowOn;
    ctx.globalAlpha = s.night ? 0.85 : 0.45;
    for (let i = 0; i < 4; i++) ctx.fillRect(u * (0.06 + i * 0.17), -u * 0.055, u * 0.1, u * 0.11);
    ctx.globalAlpha = Math.sin(clock.current * 4) > 0 ? 0.95 : 0.22;
    ctx.beginPath();
    ctx.arc(0, -u * 0.2, 1.7, 0, Math.PI * 2);
    ctx.fillStyle = s.bad;
    ctx.fill();
    ctx.restore();
    ctx.globalAlpha = 1;
  }

  /** The whole-sky shower: two layers of streaks, steady rather than jittery. */
  function drawRain(ctx: CanvasRenderingContext2D, s: Style, w: number, h: number, now: number) {
    for (let layer = 0; layer < 2; layer++) {
      const count = layer ? 72 : 46;
      const speed = layer ? 640 : 430;
      ctx.strokeStyle = s.cloud;
      ctx.globalAlpha = layer ? 0.16 : 0.26;
      ctx.lineWidth = layer ? 1 : 1.7;
      ctx.beginPath();
      for (let i = 0; i < count; i++) {
        const seedX = ((i * 71 + layer * 37) % 997) / 997;
        const x = (seedX * (w + 80) - 40 + (now / 1000) * 24) % (w + 80);
        const y = ((i * 131 + (now / 1000) * speed) % (h + 60)) - 30;
        ctx.moveTo(x, y);
        ctx.lineTo(x - 3.4, y + 11);
      }
      ctx.stroke();
    }
    ctx.globalAlpha = 1;
  }

  function drawStorm(ctx: CanvasRenderingContext2D, s: Style, slot: Slot, height: number, px: number) {
    const t = clock.current;
    ctx.strokeStyle = s.bad;
    ctx.lineWidth = Math.max(0.4, px * 0.8);
    for (let i = 0; i < 8; i++) {
      const spread = (shade(slot.seed, i) - 0.5) * slot.w;
      const across = (shade(slot.seed, i + 40) - 0.5) * slot.d;
      const fall = ((t * (1.5 + shade(slot.seed, i + 7)) + i) % 2.6) / 2.6;
      const sx = isoX(slot.x + slot.w / 2 + spread, slot.y + slot.d / 2 + across);
      const sy = isoY(slot.x + slot.w / 2 + spread, slot.y + slot.d / 2 + across, height + 1.1 - fall * 2.4);
      ctx.globalAlpha = 0.16 + (1 - fall) * 0.38;
      ctx.beginPath();
      ctx.moveTo(sx, sy);
      ctx.lineTo(sx - 1.6 * px, sy + 4.5 * px);
      ctx.stroke();
    }
    ctx.globalAlpha = 1;
  }

  // ── simulation ───────────────────────────────────────────────────────────
  const advance = (dt: number) => {
    clock.current += dt;
    const lightPhase = (clock.current % 11) / 11;

    // Weather drifts in and out on the city's own schedule unless the visitor
    // has asked for a specific sky. Scenery only: rain here is not a metric.
    const wx = weather.current;
    const forced = skyRef.current;
    if (forced !== 'auto') {
      wx.rain = forced === 'rain';
      wx.until = 0; // returning to auto re-arms the occasional schedule
    } else if (clock.current > wx.until) {
      const roll = rng(((plan.seed * 7919) ^ Math.floor(clock.current)) >>> 0);
      if (wx.until === 0) {
        // First run of the scene: the sky starts clear and the first shower is
        // due some time from now, so rain stays the exception.
        wx.until = clock.current + 30 + roll() * 50;
      } else {
        wx.rain = !wx.rain;
        wx.until = clock.current + (wx.rain ? 22 + roll() * 26 : 55 + roll() * 85);
      }
    }

    // A plane crosses the sky every minute or two, and rests between flights.
    if (plane.current) {
      const p = plane.current;
      p.x += p.dir * p.speed * dt;
      // Sample the path eight times a second and keep only the last forty.
      const tick = Math.floor(clock.current * 8);
      if (p.tick !== tick) {
        p.tick = tick;
        p.trail.push([p.x + p.dir * 12, p.y]);
        if (p.trail.length > 40) p.trail.shift();
      }
      if (p.x < -140 || p.x > view.w + 140) {
        plane.current = null;
        planesSeen.current += 1;
        const rest = rng(((plan.seed * 104729) ^ planesSeen.current) >>> 0);
        planeAt.current = clock.current + 34 + rest() * 46;
      }
    } else if (clock.current > planeAt.current) {
      const roll = rng(((plan.seed * 15485863) ^ Math.floor(clock.current * 10)) >>> 0);
      const dir: 1 | -1 = roll() > 0.5 ? 1 : -1;
      plane.current = {
        x: dir > 0 ? -120 : view.w + 120,
        y: view.h * (0.12 + roll() * 0.14),
        speed: 26 + roll() * 16, dir, trail: [], tick: 0,
      };
    }

    // Street density follows the live request rate: the base fleet is scenery,
    // the extra cars are the gateway's actual traffic in the last minute.
    if (Math.floor(clock.current) !== Math.floor(clock.current - dt)) {
      const target = clamp(6 + Math.round(plan.slots.length * 0.7) + Math.min(16, Math.round(rateRef.current / 4)), 6, 40);
      const ambient = cars.current.filter((c) => !c.taxi).length;
      if (ambient < target) {
        const random = rng((clock.current * 1000) | 0);
        const axis: 0 | 1 = random() > 0.5 ? 0 : 1;
        const roads = axis === 0 ? plan.roadY : plan.roadX;
        const dir: 1 | -1 = random() > 0.5 ? 1 : -1;
        cars.current.push({
          axis, dir, lane: roads[Math.floor(random() * roads.length)] + dir * LANE,
          pos: (axis === 0 ? plan.w : plan.d) * random(), speed: 1, wish: 1.7 + random() * 2.6,
          tint: Math.floor(random() * 4), taxi: false, life: 0, fx: 0, fy: 0,
        });
      } else if (ambient > target + 3) {
        const drop = cars.current.findIndex((c) => !c.taxi);
        if (drop >= 0) cars.current.splice(drop, 1);
      }
    }
    for (const car of cars.current) {
      const extent = car.axis === 0 ? plan.w : plan.d;
      let blocked = false;
      for (const other of cars.current) {
        if (other === car || other.axis !== car.axis || other.dir !== car.dir) continue;
        if (Math.abs(other.lane - car.lane) > 0.35) continue;
        const gap = (other.pos - car.pos) * car.dir;
        if (gap > 0 && gap < 2.3) { blocked = true; break; }
      }
      if (car.taxi && car.life === 0) {
        const target = car.axis === 0 ? car.fx : car.fy;
        if ((target - car.pos) * car.dir <= 1.1) car.life = 1;
      }
      if (car.life > 0) {
        car.life -= dt;
        if (car.life <= 0) {
          sparks.current.push({ x: car.fx, y: car.fy, z: 1.4, start: performance.now(), life: 900 });
          car.taxi = false;
          car.life = 0;
          car.pos = car.dir > 0 ? -7 : extent + 7;
          car.wish = 1.8 + (car.tint % 3) * 0.4;
        }
        car.speed = 0;
      } else if (!car.taxi) {
        // Cross traffic holds the city on a light beat, so the streets move in
        // waves instead of one uniform drift.
        const ahead = car.pos + car.dir * 2.4;
        const atLight = (car.axis === 0 ? plan.roadX : plan.roadY).some((r) => Math.abs(r - ahead) < 1.3);
        const hold = atLight && (car.axis === 0 ? lightPhase < 0.16 : lightPhase > 0.5 && lightPhase < 0.66);
        if (hold) blocked = true;
      }
      const want = blocked || car.life > 0 ? 0 : car.wish;
      car.speed += (want - car.speed) * Math.min(1, dt * (want > car.speed ? 2.2 : 7));
      car.pos += car.dir * car.speed * dt;
      if (car.pos > extent + 8 || car.pos < -8) car.pos = car.pos > 0 ? -7 : extent + 7;
      if (!Number.isFinite(car.pos)) car.pos = 0;
    }

    for (const p of peds.current) {
      p.t += (p.dir * p.speed * dt) / 4;
      if (p.t > 1) p.t -= 1;
      if (p.t < 0) p.t += 1;
    }

    for (const [id, list] of pulses.current) {
      const live = list.filter((p) => performance.now() - p.start < DUR);
      if (live.length) pulses.current.set(id, live); else pulses.current.delete(id);
    }
    if (sparks.current.length) sparks.current = sparks.current.filter((sp) => performance.now() - sp.start < sp.life);
  };

  // The loop was started by an earlier render, so it must call the LATEST
  // closures: a poll that changes a figure repaints with the new number
  // without restarting the animation.
  const scene = useRef({ draw, advance });
  scene.current = { draw, advance };

  // ── loop control ─────────────────────────────────────────────────────────
  useEffect(() => {
    ensureSeeded();
    const ctx = canvas.current?.getContext('2d');
    if (!ctx) return;
    let raf = 0;
    let last = performance.now();
    let statsAt = 0;

    const loop = (now: number) => {
      const dt = Math.min(0.06, Math.max(0, (now - last) / 1000));
      last = now;
      scene.current.advance(dt);
      scene.current.draw(ctx, now);
      markDrawn();
      if (now - statsAt > 1000) {
        // Liveness counters for the regression drivers. Written once a second,
        // never per frame.
        statsAt = now;
        const el = canvas.current;
        if (el) {
          el.dataset.frames = String(Number(el.dataset.frames ?? '0') + 1);
          el.dataset.pulses = String(pulsesSeen.current);
          el.dataset.cars = String(cars.current.length);
          el.dataset.peds = String(peds.current.length);
          el.dataset.planes = String(planesSeen.current + (plane.current ? 1 : 0));
          el.dataset.weather = weather.current.rain ? 'rain' : 'clear';
          el.dataset.reflect = String(reflected.current);
        }
        if (host.current) host.current.dataset.weather = weather.current.rain ? 'rain' : 'clear';
      }
      raf = requestAnimationFrame(loop);
    };

    // Two frames before the first draw: the one in between is painted, so the
    // loading bar is on screen before the long first draw holds the thread.
    if (still) {
      raf = requestAnimationFrame(() => { raf = requestAnimationFrame((now) => { scene.current.draw(ctx, now); markDrawn(); }); });
      return () => cancelAnimationFrame(raf);
    }
    raf = requestAnimationFrame(() => { raf = requestAnimationFrame((now) => { last = now; loop(now); }); });
    return () => cancelAnimationFrame(raf);
    // `still` restarts the loop, so a paused scene stops scheduling frames.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [still, view.w, view.h, view.dpr, plan.seed]);

  // A paused or reduced-motion scene still has to show the current figures:
  // one still frame per data change.
  useEffect(() => {
    if (!still) return;
    ensureSeeded();
    const ctx = canvas.current?.getContext('2d');
    if (ctx) { scene.current.draw(ctx, performance.now()); markDrawn(); }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [still, buildings, plan, view.w, view.h]);

  const liveState = buildings.filter((b) => b.state === 'live').length;

  return (
    <div className="nbhd-city" ref={host} data-still={still}>
      <div className="nbhd-clouds" aria-hidden>
        <span /><span />
      </div>
      <canvas
        ref={canvas}
        className="nbhd-city-canvas"
        role="img"
        aria-label={`A city of ${buildings.length} providers, ${liveState} serving traffic right now.`}
      />
      {loading !== 'done' && (
        <div className="nbhd-city-loading" data-fading={loading === 'fading'}>
          <CityLoading label="Drawing the city…" />
        </div>
      )}
      <div className="nbhd-city-hits">
        {[...plan.slots].sort((a, b) => (a.x + a.w + a.y + a.d) - (b.x + b.w + b.y + b.d)).map((slot) => {
          const b = byId.get(slot.id);
          const hit = boxes[slot.id];
          if (!b || !hit) return null;
          const title = `${b.name}${b.state === 'off' ? ' · disabled'
            : b.state === 'unknown' ? ' · usage unavailable'
              : ` · ${Math.round(b.tpm).toLocaleString()} tokens/min · ${Math.round(b.calls).toLocaleString()} calls · ${Math.round(b.errors).toLocaleString()} errors`}`;
          return (
            <button
              key={slot.id}
              type="button"
              data-building={slot.id}
              data-state={b.state}
              data-level={b.level.toFixed(2)}
              data-tpm={Math.round(b.tpm)}
              data-calls={Math.round(b.calls)}
              data-storm={b.storm ? '1' : '0'}
              data-roof={`${hit.roof[0]},${hit.roof[1]}`}
              data-highlighted={hot === slot.id || selected === slot.id}
              aria-pressed={selected === slot.id}
              aria-label={title}
              title={title}
              className="nbhd-hit"
              style={{ ...hit.box, clipPath: hit.clip }}
              onMouseEnter={() => onHover(slot.id)}
              onMouseLeave={() => onHover(null)}
              onFocus={() => onHover(slot.id)}
              onBlur={() => onHover(null)}
              onClick={() => onSelect(slot.id)}
            />
          );
        })}
      </div>
      <button
        type="button"
        className="nbhd-reroll"
        onClick={onReroll}
        title="Grow a different arrangement of the same providers"
      >
        <span aria-hidden>⟳</span> New city
      </button>
    </div>
  );
}
