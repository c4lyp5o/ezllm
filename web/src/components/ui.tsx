// Tiny shared UI primitives — hand-built with Tailwind, no component library.
import { AnimatePresence, motion } from 'motion/react';
import { createPortal } from 'react-dom';
import { useCallback, useEffect, useId, useMemo, useRef, useState, type ReactNode } from 'react';
import { DUR, SelectionLine, spring } from './motion';
import { cx, fmtDuration, fmtMs, fmtUptime, formatTokens, nfmt, pct } from '../utils/lib';

// formatTokens / fmtMs / fmtDuration / fmtUptime / cx / nfmt / pct are
// re-exported here because existing pages already import them from './ui'.
// Their home is lib.ts (the import-free leaf); keep these aliases until those
// call sites are touched, then drop them.
export { cx, fmtDuration, fmtMs, fmtUptime, formatTokens, nfmt, pct };

// ── labels ──────────────────────────────────────────────────────────────

// A panel's title: sentence case, the text face, readable size. (It was a
// 10.5px tracked-capitals label; at that size the operator read the figure
// before knowing what it counted.)
export function SectionLabel({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <div className={cx('text-[13px] font-semibold text-ink', className)}>
      {children}
    </div>
  );
}

export function Mono({ children, className }: { children: ReactNode; className?: string }) {
  return <span className={cx('font-mono text-[13px]', className)}>{children}</span>;
}

// ── error-cause chip ────────────────────────────────────────────────────────
// The Go classifier (provider.ErrorCause) prefixes the ledger err column with
// `[<cause>]`. Render it as a typed badge; the hover carries the full
// upstream text. Rows written before that deploy carry no marker, so no chip
// renders for them (the raw status still tells the story).
export const CAUSE_TONES: Record<string, 'bad' | 'warn' | 'neutral'> = {
  // The request itself is wrong for this model — not transient.
  bad_request: 'bad', bad_credentials: 'bad', permission_denied: 'bad',
  chat_tools_unsupported: 'bad', protocol_unsupported: 'bad',
  context_too_long: 'bad', too_large: 'bad', not_found: 'bad',
  content_filter: 'bad', quota_exhausted: 'bad', provider_error: 'bad',
  // Transient — retry/failover is the right response.
  rate_limited: 'warn', upstream_capacity: 'warn', overloaded: 'warn',
  timeout: 'warn', automation_gate: 'warn',
};

export function CauseChip({ err, status }: { err?: string; status: number }) {
  if (status < 400 || !err) return null;
  const m = /^\[([a-z_]+)\]:\s?/.exec(err);
  if (!m) return null;
  return (
    <Chip tone={CAUSE_TONES[m[1]] ?? 'neutral'} title={err}>
      {m[1].replace(/_/g, ' ')}
    </Chip>
  );
}

// ── chips & badges ──────────────────────────────────────────────────────

const chipTones: Record<string, string> = {
  // Pale pairs: a tint of the status with text of the same hue; no border.
  neutral: 'border-transparent bg-raised text-dim',
  accent: 'border-transparent bg-accent-dim text-accent',
  ok: 'border-transparent bg-ok-dim text-ok',
  warn: 'border-transparent bg-warn-dim text-warn',
  bad: 'border-transparent bg-bad-dim text-bad',
};

export function Chip({ tone = 'neutral', children, className, title }: {
  tone?: keyof typeof chipTones; children: ReactNode; className?: string; title?: string;
}) {
  return (
    <span title={title} className={cx(
      'inline-flex flex-none items-center gap-1 whitespace-nowrap rounded-full border px-2 py-[3px] text-[12px] font-medium leading-none',
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
  // A flat block at the value's real size: the final layout with the figure
  // missing, so nothing shifts when the data lands. No sweep, no gradient.
  return <div aria-hidden className={cx('rounded bg-shade', className)} />;
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
  const onCloseRef = useRef(onClose);

  // Keep Escape working with the LATEST onClose without re-subscribing per
  // render: callers pass an inline `() => setOpen(false)`, a new identity every
  // time their state changes.
  useEffect(() => { onCloseRef.current = onClose; });

  // deps are [open] ONLY. It used to be [open, onClose], so every parent
  // re-render re-ran the autofocus and yanked focus to the FIRST field in the
  // dialog — typing one character in any modal (the Rules note field above all)
  // re-rendered and dropped the caret back onto the account select. Focus
  // belongs to the closed -> open transition; after that the operator owns it.
  useEffect(() => {
    if (!open) return;
    ref.current?.querySelector<HTMLElement>('input,select,textarea,button')?.focus();
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') onCloseRef.current(); };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [open]);

  if (!open) return null;

  const overlay = (e: React.MouseEvent) => { if (e.target === e.currentTarget) onClose(); };

  // No early return on !open: AnimatePresence needs the subtree mounted to
  // PLAY the exit. Returning null instead would snap the dialog away and the
  // `exit` props below would be dead code.
  return createPortal((
    <AnimatePresence>
      {open && (
        <div className="fixed inset-0 z-50 flex items-center justify-center p-4 sm:p-6" onMouseDown={overlay}>
          <motion.div
            className="absolute inset-0 bg-overlay"
            initial={{ opacity: 0 }} animate={{ opacity: 1 }} exit={{ opacity: 0 }}
            transition={{ duration: DUR.fast }} onClick={onClose} />
          <motion.div
            ref={ref} role="dialog" aria-modal="true" aria-label={title}
            className="relative z-10 flex max-h-[90dvh] w-full flex-col overflow-hidden rounded-xl border border-line-strong bg-surface shadow-2xl shadow-black/60"
            style={{ maxWidth: wide ? '56rem' : '44rem' }}
            initial={{ opacity: 0, y: 8, scale: 0.985 }}
            animate={{ opacity: 1, y: 0, scale: 1 }}
            exit={{ opacity: 0, y: 4, scale: 0.99 }}
            transition={spring}>
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
              <div className="sticky bottom-0 flex items-center justify-end gap-3 border-t border-line bg-surface px-6 py-4">
                {footer}
              </div>
            )}
          </motion.div>
        </div>
      )}
    </AnimatePresence>
  ), document.body);
}

// Destructive-action gate. Replaces window.confirm so every delete looks and
// behaves the same. Escape / backdrop / Cancel all dismiss without acting.
export function ConfirmModal({ open, title, body, confirmLabel = 'Delete', busy, onConfirm, onClose }: {
  open: boolean; title: string; body: ReactNode; confirmLabel?: string; busy?: boolean;
  onConfirm: () => void; onClose: () => void;
}) {
  return (
    <Modal open={open} onClose={onClose} title={title}
      footer={<>
        <button type="button" className={cx(btn.base, btn.ghost)} onClick={onClose} disabled={busy}>Cancel</button>
        <button type="button" className={cx(btn.base, btn.danger)} onClick={onConfirm} disabled={busy}>
          {busy ? 'Working…' : confirmLabel}
        </button>
      </>}>
      <div className="text-[13px] text-dim">{body}</div>
    </Modal>
  );
}

// ── form controls ───────────────────────────────────────────────────────

export const inputCls = 'w-full rounded-lg border border-line bg-raised px-3 py-2 text-[13px] transition-colors placeholder:text-mute hover:border-line-strong focus:border-accent focus:outline-none focus:ring-0';
export const labelCls = 'mb-1.5 block text-[12px] font-medium text-mute';

// ── searchable combobox ──────────────────────────────────────────────────
// Shared by Chat (model picker) and Services (per-card binding picker). The
// old <select> forced a scroll through every synced model (dozens once several
// providers are connected). Typing filters by value OR label, Enter picks the
// highlighted match, Escape closes, and clicking outside closes without losing
// the selection. Keyboard: ↑/↓ highlight, Enter confirm.
export type ComboOption = { value: string; label: string };

export function Combobox({ options, value, onChange, disabled, loadingText, placeholder, idPrefix }: {
  options: ComboOption[];
  value: string;
  onChange: (v: string) => void;
  disabled?: boolean;
  loadingText: string;
  placeholder?: string;
  /** Unique DOM id prefix so aria-controls never collides across cards. */
  idPrefix?: string;
}) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState('');
  const [active, setActive] = useState(0);
  const wrapRef = useRef<HTMLDivElement>(null);
  const listRef = useRef<HTMLUListElement>(null);
  const listId = `${idPrefix ?? 'combo'}-listbox`;

  const selected = options.find((o) => o.value === value);
  // Closed → show the current selection; open → show the query being typed.
  const shown = open ? query : (selected?.label ?? '');

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return options;
    return options.filter(
      (o) => o.value.toLowerCase().includes(q) || o.label.toLowerCase().includes(q),
    );
  }, [options, query]);

  const close = useCallback(() => { setOpen(false); setQuery(''); }, []);
  const pick = useCallback((v: string) => { onChange(v); close(); }, [onChange, close]);

  // Click (or tap) outside closes; a click INSIDE the list is handled by the
  // option itself and must not race this listener.
  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) close();
    };
    document.addEventListener('mousedown', onDown);
    return () => document.removeEventListener('mousedown', onDown);
  }, [open, close]);

  // Keep the highlighted row visible while arrowing through a long list.
  useEffect(() => {
    if (!open || !listRef.current) return;
    const el = listRef.current.querySelector('[data-active="true"]') as HTMLElement | null;
    el?.scrollIntoView({ block: 'nearest' });
  }, [open, active, filtered]);

  if (options.length === 0) {
    return <div className={cx(inputCls, 'mt-0 flex w-full items-center font-mono')}>{loadingText}</div>;
  }

  return (
    <div ref={wrapRef} className="relative w-full">
      <input
        type="text"
        role="combobox"
        aria-expanded={open}
        aria-controls={listId}
        aria-autocomplete="list"
        autoComplete="off"
        spellCheck={false}
        disabled={disabled}
        value={shown}
        placeholder={placeholder ?? 'Search models…'}
        onFocus={() => { if (!disabled) { setOpen(true); setQuery(''); setActive(0); } }}
        onChange={(e) => { setQuery(e.target.value); setOpen(true); setActive(0); }}
        onKeyDown={(e) => {
          if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
            if (!open) { setOpen(true); setActive(0); e.preventDefault(); return; }
            if (filtered.length === 0) return;
            e.preventDefault();
            setActive((i) => e.key === 'ArrowDown'
              ? (i + 1) % filtered.length
              : (i - 1 + filtered.length) % filtered.length);
          } else if (e.key === 'Enter') {
            if (!open) return;
            e.preventDefault();
            if (filtered.length > 0) pick(filtered[Math.min(active, filtered.length - 1)].value);
          } else if (e.key === 'Escape' && open) {
            close();
          }
        }}
        className={cx(inputCls, 'mt-0 w-full font-mono')}
      />
      {open && (
        <ul
          id={listId}
          role="listbox"
          ref={listRef}
          className="absolute z-20 mt-1 max-h-64 w-full overflow-y-auto rounded-lg border border-line bg-raised shadow-lg"
        >
          {filtered.length === 0 && (
            <li className="px-3 py-2 text-[13px] text-mute">
              No model matches “{query.trim()}”
            </li>
          )}
          {filtered.map((o, i) => {
            const isActive = i === Math.min(active, Math.max(filtered.length - 1, 0));
            const isSel = o.value === value;
            return (
              <li
                key={o.value}
                role="option"
                aria-selected={isSel}
                data-active={isActive || undefined}
                onClick={() => pick(o.value)}
                className={cx(
                  'cursor-pointer px-3 py-1.5 font-mono text-[13px]',
                  isActive && 'bg-hover',
                  isSel ? 'text-ink' : 'text-dim',
                )}
                title={o.value}
              >
                <span className="block truncate">{o.label}</span>
                {o.label !== o.value && (
                  <span className="block truncate text-[11px] text-mute">{o.value}</span>
                )}
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}

// On is the semantic green, never the accent: a switch states a fact.
export function Toggle({ checked, onChange, label, disabled }: { checked: boolean; onChange: (v: boolean) => void; label?: string; disabled?: boolean }) {
  return (
    <button type="button" role="switch" aria-checked={checked} aria-label={label} disabled={disabled}
      onClick={() => !disabled && onChange(!checked)}
      className={cx('relative h-[18px] w-8 shrink-0 rounded-full transition-colors disabled:cursor-not-allowed disabled:opacity-60',
        checked ? 'bg-ok' : 'bg-line-strong')}>
      <span className={cx('absolute top-[3px] h-3 w-3 rounded-full bg-white transition-all',
        checked ? 'left-[17px]' : 'left-[3px]')} />
    </button>
  );
}

// ── buttons ─────────────────────────────────────────────────────────────

// One filled primary per screen, and it is the INK colour inverted (near-white
// on dark, near-black on light) — never the accent, which belongs to data.
export const btn = {
  base: 'inline-flex h-[34px] items-center justify-center gap-2 rounded-full px-4 text-[13px] font-medium transition-colors duration-150 active:translate-y-px disabled:cursor-not-allowed disabled:opacity-60',
  primary: 'border border-ink bg-ink text-bg hover:border-dim hover:bg-dim',
  ghost: 'border border-line-strong bg-transparent text-ink hover:bg-hover',
  danger: 'border border-bad/50 bg-transparent text-bad hover:bg-bad-dim',
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
  return <kbd className="rounded border border-line bg-raised px-1.5 py-0.5 font-mono text-[11px] text-mute">{children}</kbd>;
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

// ── segmented control ──────────────────────────────────────────────────────
// Stats, Economics and Requests each grew their own copy of this inline
// (three identical 15-line components). One generic version, typed on the
// option key so a typo in the value is a compile error, not a blank toggle.
export function Segmented<T extends string>({
  options, value, onChange, label, size = 'md',
}: {
  options: { key: T; label: string; title?: string }[];
  value: T;
  onChange: (v: T) => void;
  label?: string;
  size?: 'sm' | 'md';
}) {
  const lineId = `seg-${useId()}`;
  return (
    // The selection is the style's one mark: a 1px accent line exactly as
    // wide as the chosen label, on its baseline. No filled pill.
    <div role="group" aria-label={label} className="inline-flex flex-wrap items-center gap-1">
      {options.map((o) => (
        <button
          key={o.key}
          type="button"
          title={o.title}
          aria-pressed={o.key === value}
          onClick={() => onChange(o.key)}
          className={cx(
            'rounded font-medium transition-colors hover:bg-hover',
            size === 'sm' ? 'px-1.5 py-0.5 text-[12px]' : 'px-2 py-1 text-[13px]',
            o.key === value ? 'text-ink' : 'text-mute hover:text-ink',
          )}
        >
          <span className="relative inline-block pb-px">
            {o.label}
            {o.key === value && <SelectionLine layoutId={lineId} />}
          </span>
        </button>
      ))}
    </div>
  );
}

// ── empty state ────────────────────────────────────────────────────────────
// A blank region with no explanation is the most common dashboard bug: the
// operator cannot tell "no data" from "the query failed" from "I broke it".
export function EmptyState({
  title, hint, action,
}: { title: string; hint?: string; action?: ReactNode }) {
  return (
    // The frame stays, one dashed box with the sentence in it: an empty block
    // reads as "nothing yet", never as a broken or unfinished screen.
    <div className="flex flex-col items-start gap-1 rounded border border-dashed border-line-strong px-4 py-3">
      <p className="text-[13px] text-dim">{title}</p>
      {hint && <p className="max-w-prose text-[12px] leading-relaxed text-mute">{hint}</p>}
      {action && <div className="mt-3">{action}</div>}
    </div>
  );
}
