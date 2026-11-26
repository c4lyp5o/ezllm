// The paused pill: how long a model (or a provider's models) stays off traffic,
// counting down live, with "Resume" to end it after fixing the cause.
import { useState } from 'react';
import { type Cooldown, parkedFor, secondsLeft, splitParks, useNow } from '../utils/cooldowns';
import { Chip, cx } from './ui';

export function ParkedPill({ items, onUnpark, what, keyCount = 1 }: {
  items: Cooldown[];
  /** The account's enabled keys: a model is parked only when every key is. */
  keyCount?: number;
  onUnpark: () => Promise<void>;
  /** What Unpark releases, for its label: "this model", "acme's 3 models". */
  what: string;
}) {
  const now = useNow(items.length > 0);
  const [busy, setBusy] = useState(false);
  if (items.length === 0) return null;
  const { full, partial } = splitParks(items, keyCount);
  const shown = full.length > 0 ? full : partial;
  const left = Math.min(...shown.map((c) => secondsLeft(c, now)));
  const probing = left === 0 && shown.some((c) => c.probe_active);
  const detail = items.map((c) => `${c.namespace}/${c.model}${c.key_hint ? ` (key ${c.key_hint})` : ''}: ${c.reason || 'paused'}${secondsLeft(c, now) > 0 ? `, ${parkedFor(secondsLeft(c, now))} left` : ''}`).join('\n');
  const models = new Set(shown.map((c) => `${c.namespace}/${c.model}`)).size;
  const keys = new Set(items.filter((c) => c.key_id).map((c) => c.key_id)).size;
  const resting = full.length === 0;
  return (
    <span className="inline-flex items-center gap-1.5">
      <Chip tone={resting ? 'neutral' : 'warn'} title={`${detail}\n\nA paused model gets no traffic until the time runs out; then one request tests it.${keys > 0 ? ' A paused key is skipped; the provider\'s other keys still serve.' : ''}`}>
        {resting
          ? `${keys} of ${keyCount} keys paused · ${parkedFor(left)}`
          : probing ? 'testing' : `paused · ${parkedFor(left)}`}
        {models > 1 && <span className="font-normal">· {models} models</span>}
      </Chip>
      <button type="button" disabled={busy}
        className={cx('rounded-md px-1.5 py-0.5 text-[12px] text-accent hover:bg-hover hover:underline disabled:opacity-60')}
        title={`Send traffic to ${what} again now. A real failure pauses it again.`}
        onClick={async (e) => { e.stopPropagation(); setBusy(true); try { await onUnpark(); } finally { setBusy(false); } }}>
        Resume
      </button>
    </span>
  );
}
