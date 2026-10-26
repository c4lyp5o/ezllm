import { useId, type CSSProperties } from 'react';
import type { House } from '../pages/Neighbourhood';
import { formatTokens } from './ui';

const W = 88;
const GAP = 47;
const DEPTH = 20;
const GROUND = 174;
const HEIGHT = 278;
const hash = (value: string) => { let n = 0; for (const c of value) n = ((n << 5) - n + c.charCodeAt(0)) | 0; return Math.abs(n); };
const short = (value: string, n = 15) => value.length > n ? `${value.slice(0, n - 1)}…` : value;

function Window({ x, y, h, glow }: { x: number; y: number; h: House; glow: string }) {
  const lit = h.state === 'live' ? 0.3 + h.level * 0.7 : h.state === 'idle' ? 0.08 : 0;
  return <g>
    {h.state === 'live' && <circle cx={x} cy={y} r={25} fill={`url(#${glow})`} opacity={h.level * 0.65} />}
    <rect x={x - 7} y={y - 9} width={14} height={18} rx={1.5} fill="var(--nb-window-off)" />
    <rect className={h.state === 'live' ? 'nbhd-window-light' : ''} x={x - 7} y={y - 9} width={14} height={18} rx={1.5}
      fill="var(--nb-window-on)" opacity={lit} style={{ '--nb-window-lit': lit } as CSSProperties} />
    <path d={`M${x},${y - 9}v18 M${x - 7},${y}h14`} stroke="var(--nb-window-frame)" strokeWidth={1.6} />
    {h.state === 'off' && <g stroke="var(--nb-board)" strokeWidth={3} strokeLinecap="round">
      <line x1={x - 9} y1={y - 5} x2={x + 9} y2={y + 3} /><line x1={x - 9} y1={y + 5} x2={x + 9} y2={y - 3} />
    </g>}
  </g>;
}

function Tree({ x, y, small = false }: { x: number; y: number; small?: boolean }) {
  return <g transform={`translate(${x} ${y}) scale(${small ? 0.72 : 1})`} aria-hidden>
    <ellipse cy={2} rx={17} ry={4} fill="var(--nb-shadow)" opacity={0.35} />
    <path d="M0 0v-28" stroke="var(--nb-trunk)" strokeWidth={4} />
    <path d="M0-16-9-27M0-22 8-30" stroke="var(--nb-trunk)" strokeWidth={2} />
    <circle cx={-8} cy={-31} r={13} fill="var(--nb-tree-dark)" /><circle cx={7} cy={-37} r={14} fill="var(--nb-tree)" />
    <circle cx={-2} cy={-46} r={12} fill="var(--nb-tree-light)" />
  </g>;
}

function Lamp({ x, glow }: { x: number; glow: string }) {
  return <g transform={`translate(${x} ${GROUND + 23})`} aria-hidden>
    <ellipse cy={3} rx={23} ry={5} fill={`url(#${glow})`} opacity={0.3} />
    <path d="M0 0v-40q0-6 6-6h7" fill="none" stroke="var(--nb-lamp)" strokeWidth={2.5} />
    <circle cx={12} cy={-43} r={12} fill={`url(#${glow})`} opacity={0.6} />
    <path d="M6-45h12l-2 5H8Z" fill="var(--nb-window-on)" />
  </g>;
}

function Cloud({ x, y }: { x: number; y: number }) {
  return <g transform={`translate(${x} ${y})`} aria-hidden>
    <g fill="var(--nb-cloud)"><circle cx={-13} cy={0} r={10} /><circle cy={-6} r={13} /><circle cx={13} r={10} /><rect x={-23} y={0} width={46} height={8} rx={3} /></g>
    {[-12, 0, 12].map((dx, i) => <line key={dx} className="nbhd-rain" x1={dx} x2={dx - 3} y1={13} y2={20}
      stroke="var(--color-bad)" strokeWidth={1.5} strokeLinecap="round" style={{ animationDelay: `${-i * 0.3}s` }} />)}
  </g>;
}

function HouseSvg({ h, x, index, glow, active, selected, onHover, onSelect }: {
  h: House; x: number; index: number; glow: string; active: boolean; selected: boolean;
  onHover: (id: string | null) => void; onSelect: (id: string) => void;
}) {
  const seed = hash(h.id);
  const height = 51 + (seed % 3) * 9;
  const top = GROUND - height;
  const accent = ['sage', 'clay', 'blue'][seed % 3];
  const known = h.state !== 'unknown';
  const title = `${h.label} · ${h.account} — ${known ? `${formatTokens(h.tpm)} tokens/min · ${formatTokens(h.calls)} calls · ${formatTokens(h.errors)} errors` : 'usage unavailable'}` + (h.state === 'off' ? ' · disabled' : '');
  return <><g role="button" tabIndex={0} aria-label={title} aria-pressed={selected}
    data-house={h.id} data-state={h.state} data-level={h.level.toFixed(2)} data-storm={h.storm ? '1' : '0'} data-highlighted={active || selected}
    className="nbhd-house" style={{ '--house-wall': `var(--nb-${accent})`, '--house-side': `var(--nb-${accent}-side)` } as CSSProperties}
    onMouseEnter={() => onHover(h.id)} onMouseLeave={() => onHover(null)} onFocus={() => onHover(h.id)} onBlur={() => onHover(null)}
    onClick={() => onSelect(h.id)} onKeyDown={e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); onSelect(h.id); } }}>
    <title>{title}</title>
    <ellipse cx={x + W / 2 + 13} cy={GROUND + 9} rx={W * 0.62} ry={10} fill="var(--nb-shadow)" opacity={0.4} />
    <rect className="nbhd-house-focus" x={x - 12} y={top - 42} width={W + DEPTH + 23} height={height + 67} rx={8} fill="none" stroke="var(--color-accent)" strokeWidth={1.5} strokeDasharray="3 4" />
    {h.state === 'live' && <ellipse cx={x + W / 2} cy={GROUND + 8} rx={W * 0.72} ry={12} fill={`url(#${glow})`} opacity={h.level * 0.7} />}
    {/* A side wall, overhanging roof, foundation and porch give every key depth. */}
    <path d={`M${x + W},${top}l${DEPTH},-10v${height}l-${DEPTH},10Z`} fill="var(--house-side)" />
    <rect x={x} y={top} width={W} height={height} fill="var(--house-wall)" />
    <path d={`M${x - 7},${top + 1}l${W / 2 + 7},-30 ${W / 2 + 7},30Z`} fill="var(--nb-roof)" />
    <path d={`M${x + W / 2},${top - 29}l${DEPTH},-10 ${W / 2 + 7},30 -${DEPTH},10Z`} fill="var(--nb-roof-top)" />
    <path d={`M${x - 7},${top + 1}h${W + 14}`} stroke="var(--nb-roof-edge)" strokeWidth={3} />
    <path d={`M${x + 14},${top - 13}v-22h9v16`} fill="var(--house-side)" stroke="var(--nb-roof-edge)" strokeWidth={1} />
    <rect x={x - 1} y={GROUND - 3} width={W + 2} height={5} fill="var(--nb-foundation)" />
    <rect x={x + 36} y={GROUND - 26} width={17} height={26} rx={2} fill="var(--nb-door)" />
    <circle cx={x + 49} cy={GROUND - 12} r={1.2} fill="var(--nb-window-on)" />
    <path d={`M${x + 30},${GROUND + 2}h30v4H${x + 27}v4h36`} fill="none" stroke="var(--nb-pavement)" strokeWidth={3} />
    <Window x={x + 18} y={top + 20} h={h} glow={glow} /><Window x={x + 69} y={top + 20} h={h} glow={glow} />
    <path d={`M${x + W + 8},${top + 15}v12l6-3v-12Z`} fill={h.state === 'live' ? 'var(--nb-window-on)' : 'var(--nb-window-off)'} opacity={h.state === 'live' ? 0.2 + h.level * 0.5 : 1} />
    <path d={`M${x + 6},${GROUND}v-9h13v9M${x + 70},${GROUND}v-9h12v9`} fill="var(--nb-tree-dark)" />
    {[x + 9, x + 16, x + 73, x + 79].map((cx, i) => <circle key={cx} cx={cx} cy={GROUND - 10 - (i % 2) * 2} r={2.5} fill="var(--nb-flower)" />)}
    <path d={`M${x + 2},${GROUND + 15}h22m-18-4v9m11-9v9`} stroke="var(--nb-fence)" strokeWidth={2} />
    <text x={x + W / 2 + 3} y={GROUND + 90} textAnchor="middle" fontSize={11} fill={active || selected ? 'var(--color-ink)' : 'var(--nb-label)'}>{short(h.label)}</text>
    <circle cx={x + W / 2 + 3} cy={GROUND + 74} r={2} fill={h.state === 'live' ? 'var(--nb-window-on)' : h.state === 'unknown' ? 'var(--color-warn)' : 'var(--nb-label)'} />
  </g>
    {/* Keep moving smoke/rain out of the button's hit box so its target stays
        stable for pointer, touch and keyboard interaction. */}
    <g className="pointer-events-none" aria-hidden>
    {h.state === 'live' && h.level > 0.12 && <g transform={`translate(${x + 19} ${top - 38})`} aria-hidden>
      {Array.from({ length: h.level > 0.66 ? 3 : 1 }, (_, i) => <circle key={i} className="nbhd-smoke" cy={0} r={3.5} fill="var(--nb-smoke)" style={{ animationDelay: `${-i * 1.1 - index * 0.4}s` }} />)}
    </g>}
    {h.storm && <Cloud x={x + W / 2} y={top - 58} />}
    </g></>;
}

function Street({ account, houses, hot, selected, onHover, onSelect }: {
  account: string; houses: House[]; hot: string | null; selected: string | null;
  onHover: (id: string | null) => void; onSelect: (id: string) => void;
}) {
  const glow = `street-glow-${useId().replace(/:/g, '')}`;
  const occupied = houses.length * W + (houses.length - 1) * GAP + DEPTH;
  const width = Math.max(360, occupied + 60);
  const left = (width - occupied) / 2;
  const live = houses.filter(h => h.state === 'live').length;
  const unknown = houses.some(h => h.state === 'unknown');
  return <figure className="nbhd-block m-0 min-w-0">
    <figcaption className="flex items-center justify-between gap-3 px-3 text-xs">
      <span className="truncate font-medium text-ink" title={account}>{account}</span>
      <span className="shrink-0" style={{ color: 'var(--nb-label)' }}>{houses.length} {houses.length === 1 ? 'house' : 'houses'} · {unknown ? 'usage unavailable' : `${live} lit`}</span>
    </figcaption>
    <div className="nbhd-street-scroll" tabIndex={0} aria-label={`${account} street; scroll to explore`}>
      <svg className="nbhd-street-svg" style={{ minWidth: width }} width="100%" height={HEIGHT} viewBox={`0 0 ${width} ${HEIGHT}`} role="group" aria-label={`${account}: ${houses.length} houses`}>
        <defs><radialGradient id={glow}><stop offset="0%" stopColor="var(--nb-glow)" stopOpacity={0.9} /><stop offset="100%" stopColor="var(--nb-glow)" stopOpacity={0} /></radialGradient></defs>
        <g aria-hidden>
          <path d={`M8 ${GROUND - 3}H${width - 8}l8 13v45H0v-45Z`} fill="var(--nb-ground)" />
          <path d={`M0 ${GROUND + 19}H${width}v36H0Z`} fill="var(--nb-road)" />
          <path d={`M0 ${GROUND + 18}H${width}M0 ${GROUND + 56}H${width}`} stroke="var(--nb-pavement)" strokeWidth={4} />
          <path d={`M0 ${GROUND + 38}H${width}`} stroke="var(--nb-road-line)" strokeWidth={1.5} strokeDasharray="11 13" opacity={0.7} />
          <Tree x={15} y={GROUND + 5} small /><Tree x={width - 17} y={GROUND + 8} />
          <Lamp x={width - 35} glow={glow} /><Lamp x={28} glow={glow} />
          {live > 0 && <g transform={`translate(0 ${GROUND + 30})`}><g className="nbhd-traffic" style={{ '--nb-drive-width': `${width + 28}px`, '--nb-parked-x': `${width * 0.42}px`, animationDuration: `${10 + hash(account) % 5}s`, animationDelay: `-${hash(account) % 7}s` } as CSSProperties}>
            <ellipse cx={9} cy={7} rx={15} ry={3} fill="var(--nb-shadow)" opacity={0.4} />
            <rect x={0} y={-3} width={24} height={10} rx={3} fill="var(--nb-car)" /><path d="M5-3 8-8h9l4 5Z" fill="var(--nb-car)" />
            <path d="M9-7h7l3 4H7Z" fill="var(--nb-car-window)" /><circle cx={5} cy={7} r={3} fill="var(--nb-tire)" /><circle cx={19} cy={7} r={3} fill="var(--nb-tire)" />
            <rect x={22} y={0} width={3} height={2} rx={1} fill="var(--nb-window-on)" />
          </g></g>}
        </g>
        {houses.map((h, i) => <HouseSvg key={h.id} h={h} index={i} x={left + i * (W + GAP)} glow={glow} active={hot === h.id} selected={selected === h.id} onHover={onHover} onSelect={onSelect} />)}
      </svg>
    </div>
  </figure>;
}

const STARS = [[5, 22], [13, 54], [22, 15], [31, 39], [42, 19], [56, 43], [65, 14], [76, 30], [86, 18], [94, 57]];
export default function NeighbourhoodScene({ houses, hot, selected, still, onHover, onSelect }: {
  houses: House[]; hot: string | null; selected: string | null; still: boolean;
  onHover: (id: string | null) => void; onSelect: (id: string) => void;
}) {
  const streets = new Map<string, House[]>();
  for (const h of houses) { const street = streets.get(h.account) ?? []; street.push(h); streets.set(h.account, street); }
  return <div className="nbhd-sky nbhd-scene relative isolate overflow-hidden rounded-2xl border border-line" data-still={still}>
    <div className="nbhd-horizon" aria-hidden />
    <div className="nbhd-moon" aria-hidden />
    {STARS.map(([x, y], i) => <span key={i} className="nbhd-star" aria-hidden style={{ left: `${x}%`, top: y, width: i % 3 === 0 ? 3 : 2, height: i % 3 === 0 ? 3 : 2, animationDelay: `${-i * 0.6}s` }} />)}
    <div className="relative flex items-center justify-between gap-3 px-5 pb-6 pt-5">
      <span className="nbhd-scene-caption">A little neighbourhood</span><span className="nbhd-scene-caption text-right">{streets.size} {streets.size === 1 ? 'street' : 'streets'} / {houses.length} houses</span>
    </div>
    <div className="nbhd-streets relative">
      {[...streets].map(([account, hs]) => <Street key={account} account={account} houses={hs} hot={hot} selected={selected} onHover={onHover} onSelect={onSelect} />)}
    </div>
  </div>;
}
