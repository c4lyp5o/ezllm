// What an extra does, shown on its page until it's set up: a few plain
// sentences, when it's worth it, what it costs, and one way to start.
import { useState, type ReactNode } from 'react';
import { btn, cx } from './ui';

export function ExtraIntro({ title, children, worth, action, onAction, onClose }: {
  title: string;
  /** What it does, in a sentence or three. */
  children: ReactNode;
  /** When it helps, and what it costs. */
  worth?: ReactNode;
  action?: string;
  onAction?: () => void;
  /** For tools with nothing to set up: hides the card. */
  onClose?: () => void;
}) {
  return (
    <section aria-label={`About ${title}`} className="relative rounded-xl border border-line bg-surface px-6 py-6">
      {onClose && (
        <button type="button" aria-label={`Hide the note about ${title}`} onClick={onClose}
          className="absolute right-3 top-3 rounded px-1.5 text-mute hover:text-ink">✕</button>
      )}
      <h2 className="text-[15px] font-semibold text-ink">{title}</h2>
      <div className="mt-2 max-w-2xl space-y-2 text-[13px] leading-relaxed text-dim">{children}</div>
      {worth && <p className="mt-2 max-w-2xl text-[13px] leading-relaxed text-mute">{worth}</p>}
      {action && onAction && (
        <button type="button" className={cx(btn.base, btn.primary, 'mt-5')} onClick={onAction}>{action}</button>
      )}
    </section>
  );
}

/** Remembers, in this browser, that someone closed a tool's note. */
export function useIntroHidden(key: string): [boolean, () => void] {
  const storageKey = `ezllm.intro.${key}`;
  const [hidden, setHidden] = useState(() => { try { return localStorage.getItem(storageKey) === '1'; } catch { return false; } });
  return [hidden, () => { setHidden(true); try { localStorage.setItem(storageKey, '1'); } catch { /* private mode */ } }];
}
