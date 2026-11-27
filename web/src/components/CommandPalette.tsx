// Ctrl+K / Cmd+K quick jump: every page, the Settings tabs and the common
// "new …" actions, filtered as you type. Enter goes; Escape closes.
import { useEffect, useMemo, useRef, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { cx } from './ui';

export type PaletteItem = { label: string; to: string; hint?: string; group: 'Pages' | 'More' | 'Actions' | 'Settings' };

export const PALETTE_ITEMS: PaletteItem[] = [
  { group: 'Pages', label: 'Dashboard', to: '/', hint: 'overview, health, recent calls' },
  { group: 'Pages', label: 'Providers', to: '/providers', hint: 'keys, models, quota' },
  { group: 'Pages', label: 'Combos', to: '/combos', hint: 'routing chains' },
  { group: 'Pages', label: 'Connect', to: '/connect', hint: 'address, app keys, client setup' },
  { group: 'Pages', label: 'Analytics: Stats', to: '/stats', hint: 'usage by provider, model, key' },
  { group: 'Pages', label: 'Analytics: Token Economics', to: '/economics', hint: 'cost and savings' },
  { group: 'Pages', label: 'Analytics: Requests', to: '/requests', hint: 'every call, filterable' },
  { group: 'Pages', label: 'Get started', to: '/start', hint: 'first provider, app key, test request' },
  { group: 'More', label: 'Compression', to: '/compression', hint: 'fewer tokens on long chats' },
  { group: 'More', label: 'Rules', to: '/rules', hint: 'token limits and allowed hours' },
  { group: 'More', label: 'Services', to: '/services', hint: 'embeddings, images, audio' },
  { group: 'More', label: 'Chat', to: '/chat', hint: 'try your models' },
  { group: 'More', label: 'Neighbourhood', to: '/neighbourhood', hint: 'providers as a city' },
  { group: 'Actions', label: 'Add provider', to: '/providers?add=1' },
  { group: 'Actions', label: 'New combo', to: '/combos?new=1' },
  { group: 'Actions', label: 'Create app key', to: '/connect?new=1' },
  { group: 'Actions', label: 'New rule', to: '/rules?new=1' },
  { group: 'Actions', label: 'New compression profile', to: '/compression?new=1' },
  { group: 'Settings', label: 'Settings: General', to: '/settings?tab=general' },
  { group: 'Settings', label: 'Settings: Security', to: '/settings?tab=security' },
  { group: 'Settings', label: 'Settings: Backup & Recovery', to: '/settings?tab=backup' },
  { group: 'Settings', label: 'Settings: Gateway', to: '/settings?tab=gateway', hint: 'API conversion, retries, pauses' },
  { group: 'Settings', label: 'Settings: Notifications', to: '/settings?tab=notifications', hint: 'bell and push channels' },
  { group: 'Settings', label: 'Settings: System', to: '/settings?tab=system', hint: 'version, bridges' },
];

/** Items matching every word of the query, in label or hint. */
export function filterPalette(items: PaletteItem[], query: string): PaletteItem[] {
  const words = query.toLowerCase().split(/\s+/).filter(Boolean);
  if (words.length === 0) return items;
  return items.filter((it) => {
    const text = `${it.label} ${it.hint ?? ''}`.toLowerCase();
    return words.every((w) => text.includes(w));
  });
}

/** The event other parts of the app send to open the palette. */
export const OPEN_PALETTE = 'ezllm:open-palette';

export function CommandPalette() {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState('');
  const [active, setActive] = useState(0);
  const navigate = useNavigate();
  const input = useRef<HTMLInputElement>(null);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault();
        setOpen((o) => !o);
      }
    };
    const onOpen = () => setOpen(true);
    document.addEventListener('keydown', onKey);
    window.addEventListener(OPEN_PALETTE, onOpen);
    return () => { document.removeEventListener('keydown', onKey); window.removeEventListener(OPEN_PALETTE, onOpen); };
  }, []);
  useEffect(() => {
    if (open) { setQuery(''); setActive(0); requestAnimationFrame(() => input.current?.focus()); }
  }, [open]);

  const shown = useMemo(() => filterPalette(PALETTE_ITEMS, query), [query]);
  const go = (it: PaletteItem | undefined) => {
    if (!it) return;
    setOpen(false);
    navigate(it.to);
  };
  if (!open) return null;
  return (
    <div className="fixed inset-0 z-50 flex items-start justify-center bg-black/40 px-4 pt-[12vh]" onMouseDown={() => setOpen(false)}>
      <div role="dialog" aria-modal="true" aria-label="Jump to"
        className="w-full max-w-lg overflow-hidden rounded-xl border border-line bg-surface shadow-2xl" onMouseDown={(e) => e.stopPropagation()}>
        <input ref={input} type="text" aria-label="Jump to a page or action" placeholder="Jump to… (pages, settings, new combo)"
          role="combobox" aria-expanded aria-controls="palette-list"
          aria-activedescendant={shown[active] ? `palette-${active}` : undefined}
          className="w-full border-b border-line bg-transparent px-4 py-3 text-[14px] text-ink outline-none placeholder:text-mute"
          value={query}
          onChange={(e) => { setQuery(e.target.value); setActive(0); }}
          onKeyDown={(e) => {
            if (e.key === 'ArrowDown') { e.preventDefault(); setActive((i) => Math.min(i + 1, shown.length - 1)); }
            else if (e.key === 'ArrowUp') { e.preventDefault(); setActive((i) => Math.max(i - 1, 0)); }
            else if (e.key === 'Enter') { e.preventDefault(); go(shown[active]); }
            else if (e.key === 'Escape') { e.preventDefault(); setOpen(false); }
          }} />
        <ul id="palette-list" role="listbox" className="max-h-80 overflow-y-auto py-1">
          {shown.map((it, i) => (
            <li key={it.to} id={`palette-${i}`} role="option" aria-selected={i === active}
              onMouseEnter={() => setActive(i)} onClick={() => go(it)}
              className={cx('flex cursor-pointer items-baseline gap-3 px-4 py-2 text-[13px]', i === active ? 'bg-hover text-ink' : 'text-dim')}>
              <span className="min-w-0 flex-1 truncate">{it.label}</span>
              {it.hint && <span className="truncate text-[12px] text-mute">{it.hint}</span>}
              <span className="text-[11px] text-mute">{it.group}</span>
            </li>
          ))}
          {shown.length === 0 && <li className="px-4 py-3 text-[13px] text-mute">Nothing matches “{query.trim()}”.</li>}
        </ul>
      </div>
    </div>
  );
}
