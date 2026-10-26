// NEIGHBOURHOOD — a living view of the keys. Each provider key is a house; the
// more tokens it moves, the brighter its windows burn. It is for fun, but every
// element is real data (see web/DESIGN.md → "The scene"):
//   window glow   tokens per minute over the last WINDOW_MIN minutes
//   chimney smoke the key is serving traffic right now
//   rain cloud    a quarter or more of its recent calls failed
//   dark windows  the key (or its account) is disabled
// The numbers behind the scene are always in the table underneath — nothing is
// readable only as a colour.
import { useEffect, useMemo, useState } from 'react';
import { motion, useReducedMotion } from 'motion/react';
import { get, type Account, type UsageResp } from '../utils/api';
import { Chip, EmptyState, SectionLabel, cx, formatTokens } from '../components/ui';

const WINDOW_MIN = 5;
const POLL_MS = 10_000;
// Full glow is the busiest key, but never less than this: a lone key moving a
// handful of tokens a minute must not blaze like a saturated one.
const MIN_REF_TPM = 2_000;

type State = 'live' | 'idle' | 'off';
type House = {
  id: string; hint: string; label: string; account: string;
  state: State; tpm: number; calls: number; errors: number; level: number; storm: boolean;
};

export function glowLevel(tpm: number, ref: number): number {
  if (tpm <= 0) return 0;
  return Math.min(1, Math.log10(1 + tpm) / Math.log10(1 + Math.max(ref, MIN_REF_TPM)));
}

export function buildHouses(accounts: Account[], usage: UsageResp | null): House[] {
  const byHint = new Map((usage?.groups ?? []).map((g) => [g.k, g]));
  const raw = accounts.flatMap((a) => a.keys.map((k) => {
    const g = byHint.get(k.hint);
    const tpm = g ? (g.tin + g.tout) / WINDOW_MIN : 0;
    const enabled = a.enabled && k.enabled;
    return { a, k, tpm: enabled ? tpm : 0, calls: enabled ? g?.calls ?? 0 : 0, errors: enabled ? g?.errors ?? 0 : 0, enabled };
  }));
  const ref = Math.max(MIN_REF_TPM, ...raw.map((r) => r.tpm));
  return raw.map(({ a, k, tpm, calls, errors, enabled }) => ({
    id: `${a.id}:${k.id}`, hint: k.hint, label: k.label || k.hint, account: a.name,
    state: !enabled ? 'off' : tpm > 0 ? 'live' : 'idle',
    tpm, calls, errors, level: glowLevel(tpm, ref),
    storm: calls >= 3 && errors / calls >= 0.25,
  }));
}

function useNeighbourhood() {
  const [accounts, setAccounts] = useState<Account[] | null>(null);
  const [usage, setUsage] = useState<UsageResp | null>(null);
  const [stale, setStale] = useState(false);
  const [at, setAt] = useState<number | null>(null);

  useEffect(() => {
    let alive = true;
    const load = async () => {
      if (document.hidden) return; // a hidden tab has nobody to look at the lights
      try {
        const from = new Date(Date.now() - WINDOW_MIN * 60_000).toISOString();
        const [a, u] = await Promise.all([
          get<Account[]>('/admin/accounts'),
          get<UsageResp>(`/admin/usage?from=${encodeURIComponent(from)}&group_by=key`),
        ]);
        if (!alive) return;
        setAccounts(a); setUsage(u); setStale(false); setAt(Date.now());
      } catch {
        if (alive) setStale(true); // keep the last scene; just say it is old
      }
    };
    void load();
    const iv = setInterval(() => void load(), POLL_MS);
    const onVisible = () => { if (!document.hidden) void load(); };
    document.addEventListener('visibilitychange', onVisible);
    return () => { alive = false; clearInterval(iv); document.removeEventListener('visibilitychange', onVisible); };
  }, []);

  const houses = useMemo(() => (accounts ? buildHouses(accounts, usage) : null), [accounts, usage]);
  return { houses, stale, at };
}

// ── drawing ────────────────────────────────────────────────────────────────
const W = 84;      // house width
const GAP = 26;    // between houses
const PAD = 22;    // street edge
const G = 150;     // ground line y
const H = 196;     // street svg height
const SCALE = 1.3; // logical units -> pixels
// The ground is one strip under every street: the label zone below the ground
// line plus the account caption.
const GROUND_PX = Math.round((H - G) * SCALE) + 28;

const hash = (s: string) => { let h = 0; for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) | 0; return Math.abs(h); };
const clip = (s: string, n: number) => (s.length > n ? `${s.slice(0, n - 1)}…` : s);

function Window({ cx: x, cy: y, level, state, delay, still }: { cx: number; cy: number; level: number; state: State; delay: number; still: boolean }) {
  const on = state === 'live';
  const idle = state === 'idle';
  const lit = on ? 0.3 + 0.7 * level : idle ? 0.08 : 0;
  const flicker = on && !still;
  return (
    <g>
      {/* The glow is a scaled, faded disc — transform and opacity only. */}
      {(on || idle) && (
        <motion.circle cx={x} cy={y} r={26} fill="url(#nb-glow)"
          style={{ transformBox: 'fill-box', transformOrigin: 'center' }}
          initial={{ opacity: 0, scale: 0.3 }}
          animate={{ opacity: on ? 0.3 + 0.7 * level : 0.12, scale: on ? 0.55 + 0.75 * level : 0.45 }}
          transition={{ duration: 0.9, ease: [0.22, 1, 0.36, 1] }} />
      )}
      <rect x={x - 8} y={y - 8} width={16} height={16} rx={2} fill="var(--nb-window-off)" />
      <motion.rect x={x - 8} y={y - 8} width={16} height={16} rx={2} fill="var(--nb-window-on)"
        initial={{ opacity: 0 }}
        animate={flicker ? { opacity: [lit, lit * 0.86, lit] } : { opacity: lit }}
        transition={flicker
          ? { duration: 2.6 + delay, repeat: Infinity, ease: 'easeInOut', delay }
          : { duration: 0.9, ease: [0.22, 1, 0.36, 1] }} />
      {state === 'off' && (
        <g stroke="var(--nb-wall-line)" strokeWidth={2.2} strokeLinecap="round">
          <line x1={x - 9} y1={y - 4} x2={x + 9} y2={y + 2} /><line x1={x - 9} y1={y + 3} x2={x + 9} y2={y - 3} />
        </g>
      )}
    </g>
  );
}

function Smoke({ x, y, puffs, still }: { x: number; y: number; puffs: number; still: boolean }) {
  if (still) return null;
  return (
    <g aria-hidden>
      {Array.from({ length: puffs }, (_, i) => (
        <motion.circle key={i} cx={x} cy={y} r={4} fill="var(--nb-smoke)"
          initial={{ opacity: 0 }}
          animate={{ y: [-2, -34], x: [0, 6 + i * 2], opacity: [0, 0.55, 0], scale: [0.6, 1.5] }}
          transition={{ duration: 3.2, repeat: Infinity, ease: 'easeOut', delay: i * 1.05 }} />
      ))}
    </g>
  );
}

function Storm({ x, y, still }: { x: number; y: number; still: boolean }) {
  return (
    <g aria-hidden>
      <g fill="var(--nb-cloud)">
        <circle cx={x - 14} cy={y} r={11} /><circle cx={x} cy={y - 6} r={14} /><circle cx={x + 15} cy={y} r={10} />
        <rect x={x - 25} y={y} width={50} height={9} rx={4} />
      </g>
      {!still && [-12, 0, 12].map((dx, i) => (
        <motion.line key={dx} x1={x + dx} x2={x + dx - 3} y1={y + 12} y2={y + 20}
          stroke="var(--color-bad)" strokeWidth={1.6} strokeLinecap="round"
          animate={{ y: [0, 16], opacity: [0, 0.9, 0] }}
          transition={{ duration: 0.9, repeat: Infinity, ease: 'linear', delay: i * 0.3 }} />
      ))}
    </g>
  );
}

type Hover = { hot: string | null; setHot: (id: string | null) => void };

function HouseSvg({ h, i, x0, still, hot, setHot }: { h: House; i: number; x0: number; still: boolean } & Hover) {
  const hh = hash(h.id);
  const bodyH = 46 + (hh % 3) * 8;
  const top = G - bodyH;
  const chimneyX = x0 + W * (hh % 2 ? 0.7 : 0.25);
  const title = `${h.label} · ${h.account} — ${formatTokens(h.tpm)} tokens/min · ${formatTokens(h.calls)} calls · ${formatTokens(h.errors)} errors`
    + (h.state === 'off' ? ' · disabled' : '');
  return (
    <motion.g data-house={h.id} data-state={h.state} data-level={h.level.toFixed(2)} data-storm={h.storm ? '1' : '0'}
      tabIndex={0} aria-label={title} className="cursor-default outline-none"
      onMouseEnter={() => setHot(h.id)} onMouseLeave={() => setHot(null)}
      onFocus={() => setHot(h.id)} onBlur={() => setHot(null)}
      initial={still ? false : { opacity: 0, y: 10 }}
      animate={{ opacity: h.state === 'off' ? 0.6 : 1, y: hot === h.id && !still ? -3 : 0 }}
      transition={{ duration: 0.42, ease: [0.22, 1, 0.36, 1], delay: still || hot !== null ? 0 : i * 0.06 }}>
      <title>{title}</title>
      {/* Light from a busy house spills onto the pavement. */}
      {h.state === 'live' && (
        <motion.ellipse cx={x0 + W / 2} cy={G + 2} rx={W * 0.7} ry={7} fill="url(#nb-glow)"
          initial={{ opacity: 0 }} animate={{ opacity: 0.2 + 0.5 * h.level }} transition={{ duration: 0.9 }} />
      )}
      <rect x={chimneyX - 5} y={top - 30} width={10} height={26} fill="var(--nb-wall)" stroke="var(--nb-wall-line)" />
      <rect x={x0} y={top} width={W} height={bodyH} fill="var(--nb-wall)" stroke="var(--nb-wall-line)" />
      <polygon points={`${x0 - 7},${top} ${x0 + W / 2},${top - 28} ${x0 + W + 7},${top}`} fill="var(--nb-roof)" />
      <rect x={x0 + W / 2 - 7} y={G - 22} width={14} height={22} rx={2} fill="var(--nb-door)" />
      <Window cx={x0 + 22} cy={top + 20} level={h.level} state={h.state} delay={0} still={still} />
      <Window cx={x0 + W - 22} cy={top + 20} level={h.level} state={h.state} delay={0.7} still={still} />
      {h.state === 'live' && h.level > 0.12 && (
        <Smoke x={chimneyX} y={top - 34} puffs={h.level > 0.66 ? 3 : h.level > 0.33 ? 2 : 1} still={still} />
      )}
      {h.storm && <Storm x={x0 + W / 2} y={top - 62} still={still} />}
      <text x={x0 + W / 2} y={G + 22} textAnchor="middle" fontSize={12}
        fill={hot === h.id ? 'var(--color-ink)' : 'var(--color-dim)'}>{clip(h.label, 14)}</text>
    </motion.g>
  );
}

function Street({ account, houses, still, hot, setHot }: { account: string; houses: House[]; still: boolean } & Hover) {
  const width = PAD * 2 + houses.length * W + (houses.length - 1) * GAP;
  return (
    <figure className="m-0">
      <svg width={width * SCALE} height={H * SCALE} viewBox={`0 0 ${width} ${H}`} role="img"
        aria-label={`${account}: ${houses.length} ${houses.length === 1 ? 'house' : 'houses'}`}>
        <defs>
          <radialGradient id="nb-glow">
            <stop offset="0%" stopColor="var(--nb-glow)" stopOpacity="0.85" />
            <stop offset="100%" stopColor="var(--nb-glow)" stopOpacity="0" />
          </radialGradient>
        </defs>
        <line x1={0} x2={width} y1={G + 0.5} y2={G + 0.5} stroke="var(--nb-wall-line)" strokeOpacity={0.6} />
        {houses.map((h, i) => <HouseSvg key={h.id} h={h} i={i} x0={PAD + i * (W + GAP)} still={still} hot={hot} setHot={setHot} />)}
      </svg>
      <figcaption className="relative px-1 pb-2 text-[12px] text-ink">
        {account} <span className="text-mute">· {houses.length} {houses.length === 1 ? 'house' : 'houses'}</span>
      </figcaption>
    </figure>
  );
}

const STARS: Array<[number, number, number]> = [
  [6, 12, 2], [14, 30, 1.5], [23, 9, 2.5], [31, 22, 1.5], [42, 14, 2], [53, 31, 1.5], [61, 8, 2.5],
  [72, 20, 1.5], [80, 11, 2], [88, 28, 1.5], [95, 15, 2], [37, 6, 1.5], [67, 34, 2],
];

function Sky({ still }: { still: boolean }) {
  return (
    <div className="pointer-events-none absolute inset-0" aria-hidden>
      <span className="nbhd-moon absolute right-[7%] top-6 rounded-full" />
      {STARS.map(([x, y, d], i) => (
        <span key={i} className="nbhd-star absolute rounded-full"
          style={{ left: `${x}%`, top: `${y}%`, width: d, height: d, animationDelay: `${(i % 5) * 0.8}s`, animationPlayState: still ? 'paused' : 'running' }} />
      ))}
    </div>
  );
}

function Glyph({ kind }: { kind: 'lit' | 'smoke' | 'storm' | 'off' }) {
  return (
    <svg width={18} height={18} viewBox="0 0 18 18" aria-hidden className="shrink-0">
      {kind === 'lit' && <><circle cx={9} cy={9} r={8} fill="var(--nb-glow)" opacity={0.35} /><rect x={4} y={4} width={10} height={10} rx={1.5} fill="var(--nb-window-on)" /></>}
      {kind === 'smoke' && <g fill="var(--nb-smoke)"><circle cx={6} cy={13} r={2.6} /><circle cx={9.5} cy={8.5} r={3.2} opacity={0.75} /><circle cx={13} cy={4} r={2.6} opacity={0.5} /></g>}
      {kind === 'storm' && <><g fill="var(--nb-cloud)"><circle cx={6} cy={7} r={4} /><circle cx={11} cy={5.5} r={4.5} /><rect x={2} y={7} width={14} height={4} rx={2} /></g><g stroke="var(--color-bad)" strokeWidth={1.4} strokeLinecap="round"><line x1={6} y1={13} x2={5} y2={16} /><line x1={11} y1={13} x2={10} y2={16} /></g></>}
      {kind === 'off' && <><rect x={4} y={4} width={10} height={10} rx={1.5} fill="var(--nb-window-off)" /><g stroke="var(--nb-wall-line)" strokeWidth={1.8} strokeLinecap="round"><line x1={3} y1={7} x2={15} y2={10} /><line x1={3} y1={11} x2={15} y2={8} /></g></>}
    </svg>
  );
}

function Ago({ at }: { at: number | null }) {
  const [, tick] = useState(0);
  useEffect(() => { const iv = setInterval(() => tick((n) => n + 1), 1000); return () => clearInterval(iv); }, []);
  if (at === null) return null;
  const s = Math.max(0, Math.round((Date.now() - at) / 1000));
  return <span className="tnum">updated {s < 2 ? 'just now' : `${s}s ago`}</span>;
}

// ── page ───────────────────────────────────────────────────────────────────
export default function Neighbourhood() {
  const { houses, stale, at } = useNeighbourhood();
  const reduce = useReducedMotion() ?? false;
  const [hot, setHot] = useState<string | null>(null);

  const streets = useMemo(() => {
    const m = new Map<string, House[]>();
    for (const h of houses ?? []) m.set(h.account, [...(m.get(h.account) ?? []), h]);
    return [...m.entries()];
  }, [houses]);
  const ref = Math.max(MIN_REF_TPM, ...(houses ?? []).map((h) => h.tpm));
  const ranked = useMemo(() => [...(houses ?? [])].sort((a, b) => b.tpm - a.tpm || a.label.localeCompare(b.label)), [houses]);

  if (houses && houses.length === 0) {
    return <EmptyState title="No keys yet." hint="Add a provider key under Providers and its house appears here." />;
  }

  return (
    <div className="space-y-5">
      <div className="nbhd">
        <div className="nbhd-sky relative overflow-hidden rounded-xl border border-line">
          <Sky still={reduce} />
          <div className="absolute inset-x-0 bottom-0" style={{ height: GROUND_PX, background: 'var(--nb-ground)' }} aria-hidden />
          <div className="relative flex min-h-[300px] flex-wrap items-end gap-x-10 gap-y-2 px-4 pb-0 pt-10">
            {!houses && <p className="px-2 pb-10 text-[13px] text-dim">Lighting the lamps…</p>}
            {streets.map(([account, hs]) => <Street key={account} account={account} houses={hs} still={reduce} hot={hot} setHot={setHot} />)}
          </div>
        </div>
      </div>
      <div className="nbhd space-y-1.5">
        <ul className="flex flex-wrap items-center gap-x-5 gap-y-2 text-[12px] text-dim">
          <li className="flex items-center gap-1.5"><Glyph kind="lit" />Brighter = more tokens per minute</li>
          <li className="flex items-center gap-1.5"><Glyph kind="smoke" />Serving traffic</li>
          <li className="flex items-center gap-1.5"><Glyph kind="storm" />A quarter or more of calls failed</li>
          <li className="flex items-center gap-1.5"><Glyph kind="off" />Disabled</li>
          <li className="ml-auto flex items-center gap-1.5 text-mute">
            <span className={cx('size-1.5 rounded-full', stale ? 'bg-warn' : 'bg-ok')} />
            <Ago at={at} /> · last {WINDOW_MIN} minutes
          </li>
        </ul>
        <p className="text-[12px] text-mute">
          The busiest key is fully lit, but full glow is never less than {formatTokens(MIN_REF_TPM)} tokens a minute.
          {stale && <span className="ml-1 text-warn">Last update failed — showing the previous scene.</span>}
        </p>
      </div>
      {ranked.length > 0 && (
        <div className="nbhd rounded-xl border border-line bg-surface">
          <div className="px-4 pt-3"><SectionLabel>Every house, busiest first</SectionLabel></div>
          <div className="overflow-x-auto">
            <table className="w-full min-w-[640px] text-[13px]">
              <thead>
                <tr className="text-left text-[11px] text-mute">
                  <th className="px-4 py-2 font-medium">Key</th>
                  <th className="px-3 py-2 font-medium">Account</th>
                  <th className="px-3 py-2 font-medium">Glow</th>
                  <th className="px-3 py-2 text-right font-medium">Tokens / min</th>
                  <th className="px-3 py-2 text-right font-medium">Calls</th>
                  <th className="px-3 py-2 text-right font-medium">Errors</th>
                  <th className="px-4 py-2 font-medium">State</th>
                </tr>
              </thead>
              <tbody>
                {ranked.map((h) => (
                  <tr key={h.id} data-row={h.id} className={cx('border-t border-line transition-colors', hot === h.id && 'bg-hover')}
                    onMouseEnter={() => setHot(h.id)} onMouseLeave={() => setHot(null)}>
                    <td className="px-4 py-2 font-mono">{h.label}</td>
                    <td className="px-3 py-2 text-dim">{h.account}</td>
                    <td className="px-3 py-2">
                      <div className="h-1.5 w-20 overflow-hidden rounded-full bg-line" title={`${Math.round(h.level * 100)}% of full glow`}>
                        <div className="h-full rounded-full" style={{ width: `${Math.round(h.level * 100)}%`, background: 'var(--nb-window-on)' }} />
                      </div>
                    </td>
                    <td className="tnum px-3 py-2 text-right font-mono">{formatTokens(h.tpm)}</td>
                    <td className="tnum px-3 py-2 text-right font-mono">{formatTokens(h.calls)}</td>
                    <td className={cx('tnum px-3 py-2 text-right font-mono', h.errors > 0 && 'text-bad')}>{formatTokens(h.errors)}</td>
                    <td className="px-4 py-2">
                      <Chip tone={h.state === 'off' ? 'neutral' : h.storm ? 'bad' : h.state === 'live' ? 'ok' : 'neutral'}>
                        {h.state === 'off' ? 'disabled' : h.storm ? 'stormy' : h.state === 'live' ? 'lit' : 'quiet'}
                      </Chip>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <p className="px-4 pb-3 pt-2 text-[12px] text-mute">Full glow is {formatTokens(ref)} tokens per minute right now.</p>
        </div>
      )}
    </div>
  );
}
