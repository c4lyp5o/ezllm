// LIVE CALLS — one SSE connection for the whole Neighbourhood page.
//
// The gate has exactly one per-request source: /admin/stream. RecentFeed owns
// its own connection because it renders rows; the city needs the same frames
// as *events* (flare this building, send a car to that door) and as a rate
// (how many requests in the last minute). Both consumers here share one
// connection and one rolling window, so the page never opens two streams.
import { useEffect, useRef, useState } from 'react';
import { openCallStream } from './stream';
import type { FeedRow } from './api';

export type LiveCall = {
  ts: string; account: string; status: number; tin: number; tout: number;
};

type Listener = (call: LiveCall) => void;
const listeners = new Set<Listener>();

// Calls that landed before the scene mounted (the page opens the stream while
// accounts and usage are still in flight) are held for a moment and replayed
// to the first subscriber, so the first flare of a visit is not swallowed.
const REPLAY_MS = 4_000;
const replay: { call: LiveCall; at: number }[] = [];

/** Subscribe to every committed call. Returns an unsubscribe function. */
export function subscribeLiveCalls(fn: Listener): () => void {
  const now = Date.now();
  for (const held of replay) if (now - held.at < REPLAY_MS) fn(held.call);
  listeners.add(fn);
  return () => { listeners.delete(fn); };
}

const publish = (call: LiveCall) => {
  replay.push({ call, at: Date.now() });
  if (replay.length > 24) replay.shift();
  for (const fn of listeners) fn(call);
};

const WINDOW_MS = 60_000;

/**
 * useLiveCalls opens the stream once and returns the request rate over the
 * last minute plus whether the stream is currently delivering. A snapshot
 * counts as "connected" — it is the server answering, which is the only
 * liveness signal the browser gets on a quiet gateway.
 */
export function useLiveCalls(): { perMinute: number; connected: boolean } {
  const [perMinute, setPerMinute] = useState(0);
  const [connected, setConnected] = useState(false);
  const seen = useRef<number[]>([]);

  useEffect(() => {
    const trim = () => {
      const cut = Date.now() - WINDOW_MS;
      seen.current = seen.current.filter((t) => t >= cut);
      setPerMinute(seen.current.length);
    };
    const stop = openCallStream({
      onSnapshot: () => setConnected(true),
      onCalls: (calls: FeedRow[]) => {
        const now = Date.now();
        setConnected(true);
        for (const c of calls) {
          publish({ ts: c.ts, account: c.account, status: c.status, tin: c.tin, tout: c.tout });
          seen.current.push(now);
        }
        trim();
      },
      onError: () => setConnected(false),
    });
    const tick = setInterval(trim, 5_000);
    return () => { stop(); clearInterval(tick); };
  }, []);

  return { perMinute, connected };
}
