// RECENT FEED — live recent-requests list over /admin/stream (SSE).
//
// Render rules:
//  - the snapshot seeds the list (newest first), then live batches PREPEND
//  - rows are keyed (ts|model|tokens|status) so a snapshot/live overlap
//    dedupes instead of double-rendering
//  - capped at 60 rows: this is a pulse monitor, not an archive
//  - status >= 400 renders red; streaming rows show a live dot until done
import { useEffect, useRef, useState } from 'react';
import { openCallStream } from '../utils/stream';
import type { FeedRow } from '../utils/api';
import { SectionLabel, cx, formatTokens } from '../components/ui';

// rows arrive normalized (stream.ts maps every snapshot + live row through
// normalizeFeedRow), so the renderer reads the ledger names: tin/tout/saved.
const CAP = 60;

const rowKey = (c: FeedRow) =>
  `${c.ts}|${c.model}|${c.tin}|${c.tout}|${c.status}`;

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
  const [rows, setRows] = useState<FeedRow[]>([]);
  const [live, setLive] = useState(false);
  const [dropped, setDropped] = useState(0);
  const seen = useRef(new Set<string>());
  // Re-render while "now" advances without polling the server.
  const [, tick] = useState(0);

  useEffect(() => {
    const stop = openCallStream({
      onSnapshot: (p) => {
        const list = p.calls ?? [];
        const fresh: FeedRow[] = [];
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
        const fresh: FeedRow[] = [];
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
        <span className="flex items-center gap-1.5 text-[12px] text-mute">
          <span
            className={cx(
              'size-1.5 rounded-full',
              live ? 'bg-ok animate-pulse' : 'bg-warn',
            )}
          />
          {live ? 'live' : 'reconnecting…'}
        </span>
        {dropped > 0 && (
          <span className="text-[12px] text-warn">
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
                      bad ? 'text-bad' : 'text-mute',
                    )}
                  >
                    {c.status}
                  </span>
                  <span className="font-mono text-ink truncate min-w-0 flex-1">
                    {c.model}
                    {c.alias && c.alias !== c.model && (
                      <span className="text-mute"> · {c.alias}</span>
                    )}
                    {c.compression && (
                      <span
                        title={
                          c.applied
                            ? `compressed by ${c.compression}`
                            : c.savedNotional > 0
                              ? `asked ${c.compression}: ${formatTokens(c.savedNotional)} tokens were compressible but below the profile's minimum ratio — the rewrite was rejected rather than break the cached prefix`
                              : `asked ${c.compression}, no change`
                        }
                        className={cx(
                          'ml-1.5 rounded px-1 py-0.5 font-mono text-[11px] align-middle',
                          c.applied && c.saved > 0
                            ? 'bg-ok-dim text-ok'
                            : c.savedNotional > 0 && !c.applied
                              ? 'bg-warn-dim text-warn'
                              : 'bg-shade text-mute',
                        )}
                      >
                        {c.compression === 'off'
                          ? 'off'
                          : c.applied && c.saved > 0
                            ? `↓${formatTokens(c.saved)}`
                            : c.applied
                              ? 'ok 0'
                              : c.savedNotional > 0
                                ? `held ${formatTokens(c.savedNotional)}`
                                : c.compression.replace('-profile', '')}
                      </span>
                    )}
                    {/* Wire-translated hop → in-tokens are the vendor's
                        INCLUSIVE total. */}
                    {c.translated && (
                      <span
                        title="translated hop: its wire differs from the client's surface; in-tokens below are the vendor's INCLUSIVE total (cache counted inside)"
                        className="ml-1.5 rounded px-1 py-0.5 font-mono text-[11px] align-middle text-accent"
                      >
                        ⇄ translated
                      </span>
                    )}
                  </span>
                  <span className="text-mute w-24 shrink-0 hidden sm:block truncate">
                    {c.account}
                  </span>
                  <span className="font-mono tabular-nums text-mute w-28 shrink-0 text-right"
                    title={`tokens in ${formatTokens(c.tin)} (↑, prompt with cache inside) · tokens out ${formatTokens(c.tout)} (↓, completion)`}>
                    <span title="tokens in — prompt, cache counted inside">↑{formatTokens(c.tin)}</span>
                    {' '}
                    <span title="tokens out — completion">↓{formatTokens(c.tout)}</span>
                    {c.saved > 0 && (
                      <span className="text-ok" title="tokens saved by compression">
                        {' '}
                        −{formatTokens(c.saved)}
                      </span>
                    )}
                  </span>
                  <span className="font-mono tabular-nums text-mute w-14 shrink-0 text-right">
                    {c.total_ms != null ? `${c.total_ms}ms` : '—'}
                  </span>
                  <span className="text-mute w-14 shrink-0 text-right">
                    {timeAgo(c.ts)}
                    {c.ip && (
                      <div className="font-mono text-[9px] text-mute/70 truncate" title={`caller address: ${c.ip}`}>
                        {c.ip}
                      </div>
                    )}
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
