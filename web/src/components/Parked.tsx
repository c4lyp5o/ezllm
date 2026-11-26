// The parked pill: how long a hop (or an account's hops) stays off traffic,
// counting down live, with "Unpark" to end it after fixing the cause.
import { useState } from 'react';
import { type Cooldown, parkedFor, secondsLeft, useNow } from '../utils/cooldowns';
import { Chip, cx } from './ui';

export function ParkedPill({ items, onUnpark, what }: {
  items: Cooldown[];
  onUnpark: () => Promise<void>;
  /** What Unpark releases, for its label: "this hop", "acme's 3 models". */
  what: string;
}) {
  const now = useNow(items.length > 0);
  const [busy, setBusy] = useState(false);
  if (items.length === 0) return null;
  const left = Math.min(...items.map((c) => secondsLeft(c, now)));
  const probing = left === 0 && items.some((c) => c.probe_active);
  const detail = items.map((c) => `${c.namespace}/${c.model}: ${c.reason || 'parked'}${secondsLeft(c, now) > 0 ? `, ${parkedFor(secondsLeft(c, now))} left` : ''}`).join('\n');
  return (
    <span className="inline-flex items-center gap-1.5">
      <Chip tone="warn" title={`${detail}\n\nA parked hop gets no traffic until the time runs out; then one request tests it.`}>
        {probing ? 'testing' : `parked · ${parkedFor(left)}`}
        {items.length > 1 && <span className="font-normal">· {items.length} models</span>}
      </Chip>
      <button type="button" disabled={busy}
        className={cx('rounded-md px-1.5 py-0.5 text-[12px] text-accent hover:bg-hover hover:underline disabled:opacity-60')}
        title={`Send traffic to ${what} again now. A real failure parks it again.`}
        onClick={async (e) => { e.stopPropagation(); setBusy(true); try { await onUnpark(); } finally { setBusy(false); } }}>
        Unpark
      </button>
    </span>
  );
}
