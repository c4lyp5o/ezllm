// openCallStream reads /admin/stream (SSE) over fetch so the Authorization
// header rides along — EventSource cannot set headers, and putting the admin
// token in the query string would leak it into access logs.
//
// Returns a close() that stops the reader AND the reconnect loop.

import { getToken, normalizeFeedRow, type FeedRow } from "./api";

export interface CallEvent {
  ts: string;
  client: string;
  surface: string;
  alias: string;
  account: string;
  model: string;
  status: number;
  stream: boolean;
  ttft_ms?: number;
  total_ms?: number;
  tokens_in: number;
  tokens_out: number;
  tokens_cached_read: number;
  tokens_cached_write: number;
  reasoning_tokens: number;
  tokens_saved: number;
  // ledger stamp: "" | "off" | "unknown-profile" | "disabled" | profile name
  compression: string;
  applied: boolean;
  // snapshot rows (GET /admin/stream/snapshot) carry the LEDGER column names
  // instead of the CallEvent names — both shapes land in the feed.
  tin?: number;
  tout?: number;
  saved?: number;
  error?: string;
}

interface StreamPayload {
  calls: CallEvent[] | Record<string, unknown>[];
  ts?: string;
  dropped?: number;
}

type Handlers = {
  onSnapshot: (payload: { calls: FeedRow[]; ts?: string; dropped?: number }) => void;
  onCalls: (calls: FeedRow[]) => void;
  onError?: (why: string) => void;
};

export function openCallStream(handlers: Handlers): () => void {
  let closed = false;
  let attempt = 0;
  let abort: AbortController | null = null;
  let timer: ReturnType<typeof setTimeout> | null = null;

  const parseFrame = (block: string) => {
    // "event: <name>\ndata: <json>\n" — comments (": ping") ignored.
    let event = "";
    let data = "";
    for (const line of block.split("\n")) {
      if (line.startsWith("event:")) event = line.slice(6).trim();
      else if (line.startsWith("data:")) data += line.slice(5).trim();
    }
    if (!event || !data) return null;
    try {
      return { event, payload: JSON.parse(data) as StreamPayload };
    } catch {
      return null;
    }
  };

  const run = async () => {
    while (!closed) {
      abort = new AbortController();
      try {
        const res = await fetch("/admin/stream", {
          headers: { Authorization: `Bearer ${getToken() ?? ""}` },
          cache: "no-store",
          signal: abort.signal,
        });
        if (!res.ok || !res.body) throw new Error(`stream HTTP ${res.status}`);
        attempt = 0; // a good connection resets backoff

        const reader = res.body.getReader();
        const decoder = new TextDecoder();
        let buf = "";
        for (;;) {
          const { done, value } = await reader.read();
          if (done || closed) break;
          buf += decoder.decode(value, { stream: true });
          // frames end with a blank line
          let idx: number;
          while ((idx = buf.indexOf("\n\n")) >= 0) {
            const block = buf.slice(0, idx);
            buf = buf.slice(idx + 2);
            const frame = parseFrame(block);
            if (!frame) continue;
            const raw = (frame.payload.calls ?? []) as any[];
            if (frame.event === "snapshot")
              handlers.onSnapshot({
                calls: raw.map(normalizeFeedRow),
                ts: frame.payload.ts,
                dropped: frame.payload.dropped,
              });
            else if (frame.event === "calls") {
              const calls = raw.map(normalizeFeedRow);
              if (calls.length) handlers.onCalls(calls);
            }
          }
        }
      } catch (err) {
        if (closed) return;
        handlers.onError?.(String(err));
      }
      if (closed) return;
      // reconnect with capped backoff (1s, 2s, 4s ... 15s)
      attempt += 1;
      const wait = Math.min(15000, 1000 * 2 ** Math.min(attempt, 4));
      await new Promise<void>((r) => {
        timer = setTimeout(r, wait);
      });
    }
  };
  void run();

  return () => {
    closed = true;
    abort?.abort();
    if (timer) clearTimeout(timer);
  };
}
