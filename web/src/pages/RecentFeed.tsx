// RECENT FEED — live recent-requests list over /admin/stream (SSE).
//
// Render rules:
//  - the snapshot seeds the list (newest first), then live batches PREPEND
//  - rows are keyed (ts|model|tokens|status) so a snapshot/live overlap
//    dedupes instead of double-rendering
//  - capped at 60 rows: this is a pulse monitor, not an archive
//  - status >= 400 renders red; streaming rows show a live dot until done
import { useEffect, useRef, useState } from 'react';
import { openCallStream, type CallEvent } from '../stream';
import { SectionLabel, cx, formatTokens } from '../ui';

const CAP = 60;

const rowKey = (c: CallEvent) =>
  `${c.ts}|${c.model}|${c.tokens_in}|${c.tokens_out}|${c.status}`;

function timeAgo(ts: string): string {
  const ms = Date.now() - new Date(ts).getTime();
  if (!isFinite(ms) || ms < 0) return 'now';
  const s = Math.floor(ms / 1000);
  if (s < 5) return 'now';
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h`;
  return `${Math.floor(h / 24)}d`;
}

export default function RecentFeed() {
  const [rows, setRows] = useState<CallEvent[]>([]);
  const [live, setLive] = useState(false);
  const [dropped, setDropped] = useState(0);
  const seen = useRef(new Set<string>());
  // Re-render while "now" advances without polling the server.
  const [, tick] = useState(0);

  useEffect(() => {
    const stop = openCallStream({
      onSnapshot: (p) => {
        const list = (p.calls ?? []) as CallEvent[];
        const fresh: CallEvent[] = [];
        for (const c of list) {
          const k = rowKey(c);
          if (!seen.current.has(k)) {
            seen.current.add(k);
            fresh.push(c);
          }
        }
        if (fresh.length) {
          setRows((old) => {
            const next = [...fresh, ...old].slice(0, CAP);
            const keep = new Set(next.map(rowKey));
            seen.current = keep; // prune evicted rows so they can reappear
            return next;
          });
        }
        if (typeof p.dropped === 'number') setDropped(p.dropped);
        setLive(true);
      },
      onCalls: (calls) => {
        const fresh: CallEvent[] = [];
        for (const c of calls) {
          const k = rowKey(c);
          if (!seen.current.has(k)) {
            seen.current.add(k);
            fresh.push(c);
          }
        }
        if (!fresh.length) return;
        setRows((old) => {
          const next = [...fresh, ...old].slice(0, CAP);
          const keep = new Set(next.map(rowKey));
          seen.current = keep;
          return next;
        });
      },
      onError: () => setLive(false),
    });
    const iv = setInterval(() => tick((n) => n + 1), 10000); // refresh "ago" labels
    return () => {
      stop();
      clearInterval(iv);
    };
  }, []);

  return (
    <section data-testid="recent-feed">
      <div className="flex items-baseline gap-3 mb-2">
        <SectionLabel>Recent requests</SectionLabel>
        <span className="flex items-center gap-1.5 text-[11px] text-mute">
          <span
            className={cx(
              'size-1.5 rounded-full',
              live ? 'bg-emerald-400 animate-pulse' : 'bg-amber-400/70',
            )}
          />
          {live ? 'live' : 'reconnecting…'}
        </span>
        {dropped > 0 && (
          <span className="text-[11px] text-amber-400/90">
            {dropped} row{dropped === 1 ? '' : 's'} shed under load
          </span>
        )}
      </div>

      <div className="rounded-xl border border-line bg-card/60 overflow-hidden">
        {rows.length === 0 ? (
          <p className="px-4 py-6 text-sm text-mute">
            Waiting for the first request…
          </p>
        ) : (
          <ul className="divide-y divide-line/60">
            {rows.map((c) => {
              const bad = c.status >= 400;
              return (
                <li
                  key={rowKey(c)}
                  className="flex items-center gap-3 px-4 py-2 text-[13px]"
                >
                  <span
                    className={cx(
                      'font-mono tabular-nums w-12 shrink-0',
                      bad ? 'text-red-400' : 'text-mute',
                    )}
                  >
                    {c.status}
                  </span>
                  <span className="font-mono text-ink truncate min-w-0 flex-1">
                    {c.model}
                    {c.alias && c.alias !== c.model && (
                      <span className="text-mute"> · {c.alias}</span>
                    )}
                  </span>
                  <span className="text-mute w-24 shrink-0 hidden sm:block truncate">
                    {c.account}
                  </span>
                  <span className="font-mono tabular-nums text-mute w-28 shrink-0 text-right">
                    {formatTokens(c.tokens_in)}↑ {formatTokens(c.tokens_out)}↓
                  </span>
                  <span className="font-mono tabular-nums text-mute w-14 shrink-0 text-right">
                    {c.total_ms != null ? `${c.total_ms}ms` : '—'}
                  </span>
                  <span className="text-mute w-9 shrink-0 text-right">
                    {timeAgo(c.ts)}
                  </span>
                </li>
              );
            })}
          </ul>
        )}
      </div>
    </section>
  );
}
