// The bell: operator notifications (new / gone / broken free models, failed
// catalog or price refreshes, a token at its budget). Polls once a minute and
// on focus; nothing is marked read just by opening the panel.
import { useCallback, useEffect, useRef, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { AnimatePresence, motion } from 'motion/react';
import { listNotifications, markNotificationsRead, type Notification } from '../utils/api';
import { DUR, EASE } from './motion';
import { cx } from './ui';

const POLL_MS = 60_000;

// Where a click on each kind takes the operator.
const ROUTE: Record<string, string> = {
  free_models_added: '/combos',
  free_models_removed: '/combos',
  free_model_broken: '/combos',
  catalog_refresh_failed: '/providers',
  client_budget_reached: '/connect',
  price_refresh_failed: '/economics',
 backup_failed: '/settings',
};

// Status colour only where it means something (house rule): warn/error get
// their tone; info stays ink.
const DOT: Record<Notification['severity'], string> = { info: 'bg-ink', warn: 'bg-warn', error: 'bg-bad' };

function ago(iso: string): string {
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 60) return 'just now';
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`;
  return `${Math.floor(s / 86400)} d ago`;
}

function BellIcon() {
  return (
    <svg viewBox="0 0 20 20" className="h-[18px] w-[18px]" fill="none" stroke="currentColor" strokeWidth="1.6"
      strokeLinecap="round" strokeLinejoin="round" aria-hidden>
      <path d="M5 8a5 5 0 0 1 10 0c0 4 1.5 5.5 1.5 5.5h-13S5 12 5 8Z" />
      <path d="M8.5 16.5a1.6 1.6 0 0 0 3 0" />
    </svg>
  );
}

export default function NotificationBell() {
  const [items, setItems] = useState<Notification[]>([]);
  const [unread, setUnread] = useState(0);
  const [open, setOpen] = useState(false);
  const wrap = useRef<HTMLDivElement>(null);
  const loading = useRef(false);
  const navigate = useNavigate();

  const load = useCallback(async () => {
    if (loading.current) return;
    loading.current = true;
    try { const r = await listNotifications(); setItems(r.items); setUnread(r.unread); }
    catch { /* Keep the last successful reading. */ }
    finally { loading.current = false; }
  }, []);

  useEffect(() => {
    const refresh = () => { if (!document.hidden) void load(); };
    refresh();
    const t = window.setInterval(refresh, POLL_MS);
    window.addEventListener('focus', refresh);
    document.addEventListener('visibilitychange', refresh);
    return () => { window.clearInterval(t); window.removeEventListener('focus', refresh); document.removeEventListener('visibilitychange', refresh); };
  }, [load]);

  useEffect(() => {
    if (!open) return;
    load();
    const onDown = (e: MouseEvent) => { if (wrap.current && !wrap.current.contains(e.target as Node)) setOpen(false); };
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setOpen(false); };
    document.addEventListener('mousedown', onDown);
    document.addEventListener('keydown', onKey);
    return () => { document.removeEventListener('mousedown', onDown); document.removeEventListener('keydown', onKey); };
  }, [open, load]);

  const readAll = () => { markNotificationsRead('all').then(load).catch(() => undefined); };
  const pick = (n: Notification) => {
    if (!n.read_at) markNotificationsRead([n.id]).then(load).catch(() => undefined);
    setOpen(false);
    const to = ROUTE[n.kind];
    if (to) navigate(to);
  };

  return (
    <div ref={wrap} className="relative">
      <button type="button" onClick={() => setOpen((v) => !v)}
        aria-label={unread ? `Notifications, ${unread} unread` : 'Notifications'} aria-expanded={open}
        className={cx('relative rounded-md border border-line p-1.5 text-dim transition-colors hover:bg-hover hover:text-ink',
          open && 'bg-hover text-ink')}>
        <BellIcon />
        <AnimatePresence>
          {unread > 0 && (
            <motion.span key="badge"
              initial={{ scale: 0 }} animate={{ scale: 1 }} exit={{ scale: 0 }}
              transition={{ duration: DUR.fast, ease: EASE }}
              className="absolute -right-1.5 -top-1.5 min-w-[18px] rounded-full bg-accent px-1 text-center font-mono text-[11px] font-semibold leading-[18px] text-accent-ink">
              {unread}
            </motion.span>
          )}
        </AnimatePresence>
      </button>
      <AnimatePresence>
        {open && (
          <motion.div role="dialog" aria-label="Notifications"
            initial={{ opacity: 0, y: -6, scale: 0.98 }} animate={{ opacity: 1, y: 0, scale: 1 }}
            exit={{ opacity: 0, y: -6, scale: 0.98 }} transition={{ duration: DUR.fast, ease: EASE }}
            className="absolute right-0 top-full z-50 mt-2 w-[min(360px,calc(100vw-32px))] origin-top-right overflow-hidden rounded-xl border border-line-strong bg-surface shadow-2xl shadow-black/50">
            <div className="flex items-center justify-between border-b border-line px-4 py-3">
              <span className="text-[13px] font-semibold">Notifications</span>
              <button type="button" onClick={readAll} disabled={unread === 0}
                className="text-[12px] text-mute transition-colors hover:text-ink disabled:opacity-40 disabled:hover:text-mute">
                Mark all read
              </button>
            </div>
            <div className="max-h-[min(70vh,480px)] overflow-y-auto">
              {items.length === 0 ? (
                <p className="px-4 py-8 text-center text-[13px] text-mute">Nothing yet. New free models, failed refreshes and spent budgets show up here.</p>
              ) : items.map((n) => (
                <button type="button" key={n.id} onClick={() => pick(n)}
                  className="flex w-full gap-3 border-b border-line px-4 py-3 text-left transition-colors last:border-b-0 hover:bg-hover">
                  <span className={cx('mt-1.5 h-2 w-2 shrink-0 rounded-full', n.read_at ? 'bg-transparent' : DOT[n.severity])} />
                  <span className="min-w-0 flex-1">
                    <span className={cx('block text-[13px] leading-snug', n.read_at ? 'text-dim' : 'font-medium text-ink')}>{n.title}</span>
                    {n.body && <span className="mt-0.5 line-clamp-2 block text-[12px] leading-snug text-mute">{n.body}</span>}
                    <span className="mt-1 block text-[11px] text-mute">
                      {ago(n.updated_at)}{n.count > 1 ? ` · ${n.count.toLocaleString()} times` : ''}
                    </span>
                  </span>
                </button>
              ))}
            </div>
          </motion.div>
        )}
      </AnimatePresence>
    </div>
  );
}
