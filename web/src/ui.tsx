// Tiny shared UI primitives — hand-built with Tailwind, no component library.
import { useEffect, useRef, type ReactNode } from 'react';

export const cx = (...xs: Array<string | false | null | undefined>) => xs.filter(Boolean).join(' ');

// ── labels ──────────────────────────────────────────────────────────────

export function SectionLabel({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <div className={cx('text-[10.5px] font-semibold uppercase tracking-[0.14em] text-mute', className)}>
      {children}
    </div>
  );
}

export function Mono({ children, className }: { children: ReactNode; className?: string }) {
  return <span className={cx('font-mono text-[12px]', className)}>{children}</span>;
}

// ── chips & badges ──────────────────────────────────────────────────────

const chipTones: Record<string, string> = {
  neutral: 'border-line bg-raised text-dim',
  accent: 'border-accent-dim bg-accent-dim text-accent',
  ok: 'border-[rgba(52,211,153,0.25)] bg-[rgba(52,211,153,0.1)] text-ok',
  warn: 'border-[rgba(251,191,36,0.25)] bg-[rgba(251,191,36,0.1)] text-warn',
  bad: 'border-[rgba(248,113,113,0.25)] bg-[rgba(248,113,113,0.1)] text-bad',
};

export function Chip({ tone = 'neutral', children, className, title }: {
  tone?: keyof typeof chipTones; children: ReactNode; className?: string; title?: string;
}) {
  return (
    <span title={title} className={cx(
      'inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-[10.5px] font-medium leading-none',
      chipTones[tone], className,
    )}>
      {children}
    </span>
  );
}

export function ProtocolChip({ state, label }: { state: 'yes' | 'no' | 'unknown'; label: string }) {
  const s = state === 'yes'
    ? <span className="text-ok">✓</span>
    : state === 'no'
      ? <span className="text-bad line-through decoration-bad">✕</span>
      : <span className="text-warn">?</span>;
  return (
    <Chip title={`${label}: ${state === 'unknown' ? 'untested' : state === 'yes' ? 'supported' : 'not supported'}`}
      tone={state === 'yes' ? 'ok' : state === 'no' ? 'bad' : 'warn'}>
      {s} <span className="font-mono">{label}</span>
    </Chip>
  );
}

export function StaleBadge({ stale }: { stale: boolean }) {
  if (!stale) return null;
  return <Chip tone="warn">stale</Chip>;
}

// ── skeleton shimmers ───────────────────────────────────────────────────

export function Skeleton({ className }: { className?: string }) {
  return <div aria-hidden className={cx('animate-pulse-soft rounded-md bg-[rgba(255,255,255,0.05)]', className)} />;
}

export function SkeletonRows({ rows = 5, cols = 6, height = 'h-4' }: { rows?: number; cols?: number; height?: string }) {
  return (
    <div className="space-y-3.5">
      {Array.from({ length: rows }).map((_, i) => (
        <div key={i} className="flex items-center gap-4">
          {Array.from({ length: cols }).map((_, j) => (
            <Skeleton key={j} className={cx(height, j === 0 ? 'w-28' : j === cols - 1 ? 'ml-auto block h-4 w-10' : 'block h-4 w-20')} />
          ))}
        </div>
      ))}
    </div>
  );
}

export function SkeletonCards({ n = 3 }: { n?: number }) {
  return (
    <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
      {Array.from({ length: n }).map((_, i) => (
        <div key={i} className="rounded-xl border border-line bg-surface p-4">
          <div className="flex items-center justify-between">
            <Skeleton className="h-5 w-36" />
            <Skeleton className="h-5 w-14" />
          </div>
          <Skeleton className="mt-3 h-3.5 w-48" />
          <div className="mt-4 flex gap-2"><Skeleton className="h-5 w-16" /><Skeleton className="h-5 w-16" /><Skeleton className="h-5 w-16" /></div>
        </div>
      ))}
    </div>
  );
}

// ── modal ───────────────────────────────────────────────────────────────

export function Modal({ open, onClose, title, subtitle, children, footer, wide }: {
  open: boolean; onClose: () => void; title: string; subtitle?: string;
  children: ReactNode; footer?: ReactNode; wide?: boolean;
}) {
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    const node = ref.current;
    if (node) {
      const el = node.querySelector<HTMLElement>('input,select,textarea,button');
      el?.focus();
    }
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose(); };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [open, onClose]);

  if (!open) return null;

  const overlay = (e: React.MouseEvent) => { if (e.target === e.currentTarget) onClose(); };

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center p-4 sm:p-6" onMouseDown={overlay}>
      <div className="absolute inset-0 bg-black/60 backdrop-blur-sm animate-fade-in" onClick={onClose} />
      <div ref={ref} role="dialog" aria-modal="true" aria-label={title}
        className="relative z-10 flex max-h-[90vh] w-full flex-col overflow-hidden rounded-xl border border-line-strong bg-surface shadow-2xl shadow-black/60 animate-fade-up"
        style={{ maxWidth: wide ? '56rem' : '44rem' }}>
        <div className="flex items-start justify-between gap-6 border-b border-line px-6 py-4">
          <div>
            <h2 className="text-[15px] font-semibold">{title}</h2>
            {subtitle && <p className="mt-0.5 text-xs text-mute">{subtitle}</p>}
          </div>
          <button type="button" onClick={onClose} aria-label="Close"
            className="-mr-1 rounded-md p-1 text-mute transition-colors hover:bg-hover hover:text-ink">✕</button>
        </div>
        <div className="min-h-0 flex-1 overflow-y-auto px-6 py-5">{children}</div>
        {footer && (
          <div className="sticky bottom-0 flex items-center justify-end gap-3 border-t border-line bg-surface/95 px-6 py-4 backdrop-blur">
            {footer}
          </div>
        )}
      </div>
    </div>
  );
}

// ── form controls ───────────────────────────────────────────────────────

export const inputCls = 'w-full rounded-lg border border-line bg-raised px-3 py-2 text-[13px] transition-colors placeholder:text-mute hover:border-line-strong focus:border-accent focus:outline-none focus:ring-0';
export const labelCls = 'mb-1.5 block text-[11px] font-medium uppercase tracking-[0.08em] text-mute';

export function Toggle({ checked, onChange, label, disabled }: { checked: boolean; onChange: (v: boolean) => void; label?: string; disabled?: boolean }) {
  return (
    <button type="button" role="switch" aria-checked={checked} aria-label={label} disabled={disabled}
      onClick={() => !disabled && onChange(!checked)}
      className={cx('relative h-5 w-9 shrink-0 rounded-full border transition-colors disabled:cursor-not-allowed disabled:opacity-45',
        checked ? 'border-accent bg-accent-dim' : 'border-line-strong bg-raised')}>
      <span className={cx('absolute top-0.5 h-3.5 w-3.5 rounded-full transition-all',
        checked ? 'left-[18px] bg-accent' : 'left-0.5 bg-mute')} />
    </button>
  );
}

// ── buttons ─────────────────────────────────────────────────────────────

export const btn = {
  base: 'inline-flex items-center justify-center gap-2 rounded-lg px-3.5 py-2 text-[13px] font-medium transition-all duration-150 disabled:cursor-not-allowed disabled:opacity-45',
  primary: 'border border-accent/60 bg-accent text-[#082a3d] hover:brightness-110 active:brightness-95',
  ghost: 'border border-line bg-transparent text-dim hover:border-line-strong hover:bg-hover hover:text-ink',
  danger: 'border border-[rgba(248,113,113,0.3)] bg-transparent text-bad hover:bg-[rgba(248,113,113,0.08)]',
};

export function Spinner({ className }: { className?: string }) {
  return (
    <svg className={cx('animate-spin', className ?? 'h-3.5 w-3.5')} viewBox="0 0 24 24" fill="none">
      <circle className="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="3" />
      <path className="opacity-90" d="M22 12A10 10 0 0 0 12 2" stroke="currentColor" strokeWidth="3" strokeLinecap="round" />
    </svg>
  );
}

// ── misc ────────────────────────────────────────────────────────────────

export function Kbd({ children }: { children: ReactNode }) {
  return <kbd className="rounded border border-line bg-raised px-1.5 py-0.5 font-mono text-[10.5px] text-mute">{children}</kbd>;
}

export function formatTokens(n: number): string {
  if (!Number.isFinite(n)) return '–';
  const a = Math.abs(n);
  if (a >= 1e9) return `${(n / 1e9).toFixed(1).replace(/\.0$/, '')}B`;
  if (a >= 1e6) return `${(n / 1e6).toFixed(1).replace(/\.0$/, '')}M`;
  if (a >= 1e3) return `${(n / 1e3).toFixed(1).replace(/\.0$/, '')}k`;
  return `${n}`;
}

export function relTime(iso: string | null | undefined): string {
  if (!iso) return '–';
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '–';
  const s = Math.round((Date.now() - t) / 1000);
  if (s < 60) return `${s}s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
}

export function clockTime(iso: string): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return iso;
  const d = new Date(t);
  const time = d.toLocaleTimeString([], { hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit' });
  return time;
}
