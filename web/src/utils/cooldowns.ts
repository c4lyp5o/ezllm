// Parked endpoints: GET /admin/cooldowns lists them, DELETE ends them early.
// A park keeps traffic off a hop for a while after a 429 or a server error; it
// writes no ledger row, so this is the only place an operator can see it.
import { useCallback, useEffect, useState } from 'react';
import { apiFetch, del } from './api';

export type Cooldown = {
  namespace: string; model: string; until: string; strikes: number; probe_active: boolean; reason: string;
  /** Set when the account has several keys: each key parks on its own. */
  key_id?: number; key_hint?: string;
};

/** Seconds left on a park, from now; 0 once it has run out. */
export function secondsLeft(c: Pick<Cooldown, 'until'>, now = Date.now()): number {
  const t = Date.parse(c.until);
  return Number.isNaN(t) ? 0 : Math.max(0, Math.ceil((t - now) / 1000));
}

/** "42s", "3m 10s", "1h 2m": how long a park still holds. */
export function parkedFor(sec: number): string {
  if (sec < 60) return `${sec}s`;
  if (sec < 3600) return `${Math.floor(sec / 60)}m ${sec % 60}s`;
  return `${Math.floor(sec / 3600)}h ${Math.floor((sec % 3600) / 60)}m`;
}

/** Splits parks by whether their model is parked on every key. A provider
 *  with several keys keeps serving a model until all its keys are parked, so
 *  only those count as parked; the rest are a key or two resting. */
export function splitParks(items: Cooldown[], keyCount: number): { full: Cooldown[]; partial: Cooldown[] } {
  const byModel = new Map<string, Cooldown[]>();
  for (const c of items) {
    const k = `${c.namespace}/${c.model}`;
    byModel.set(k, [...(byModel.get(k) ?? []), c]);
  }
  const full: Cooldown[] = [];
  const partial: Cooldown[] = [];
  for (const group of byModel.values()) {
    (group.length >= Math.max(keyCount, 1) ? full : partial).push(...group);
  }
  return { full, partial };
}

const POLL_MS = 10_000;

/** Polls the parks and offers Unpark. `unpark(ns)` clears an account; with a
 *  model, just that model. */
export function useCooldowns() {
  const [list, setList] = useState<Cooldown[]>([]);
  const load = useCallback(async () => {
    try {
      const r = await apiFetch<{ cooldowns: Cooldown[] }>('/admin/cooldowns', { method: 'GET' });
      setList(r.cooldowns ?? []);
    } catch { /* keep the last list: a failed poll is not "nothing parked" */ }
  }, []);
  useEffect(() => {
    void load();
    const iv = setInterval(() => { if (!document.hidden) void load(); }, POLL_MS);
    return () => clearInterval(iv);
  }, [load]);
  const unpark = useCallback(async (namespace: string, model?: string) => {
    const q = new URLSearchParams({ namespace, ...(model ? { model } : {}) });
    await del(`/admin/cooldowns?${q}`);
    await load();
  }, [load]);
  return { list, unpark };
}

/** A clock that ticks once a second while `on`, so countdowns move. */
export function useNow(on: boolean): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!on) return;
    const iv = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(iv);
  }, [on]);
  return now;
}
