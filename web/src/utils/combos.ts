// Combo logic shared by the Combos page's cards and its editor: which hops can
// actually serve, what the editor must refuse, and what a strategy will do next.
// Kept out of the component so the card and the editor never disagree.
import type { Account, BlockCursor, Combo, FreeInclude, Hop } from './api';

export type HopIssue = { tone: 'bad' | 'warn'; text: string };
export type HopState = { issues: HopIssue[]; usable: boolean };

/** What is wrong with one hop, from the provider it points at. A hop switched
 *  off by the operator is not a fault (it is simply not usable). `catalog` is
 *  that account's model ids: undefined while loading, null when it did not load. */
export function hopState(h: Hop, acct: Account | undefined, catalog: string[] | null | undefined, checkCatalog = true): HopState {
  const issues: HopIssue[] = [];
  if (!acct) {
    issues.push({ tone: 'bad', text: 'its provider no longer exists' });
  } else {
    if (!acct.enabled) issues.push({ tone: 'bad', text: `${acct.name} is disabled` });
    const keys = acct.keys ?? [];
    const live = keys.filter((k) => k.enabled);
    if (acct.enabled && keys.length > 0 && live.length === 0) issues.push({ tone: 'bad', text: `${acct.name} has every key disabled` });
    else if (acct.enabled && live.length > 0 && live.every((k) => k.last_test_ok === false)) {
      issues.push({ tone: 'warn', text: `every enabled key on ${acct.name} failed its last test` });
    }
    const q = acct.quota;
    const worst = Math.max(q?.percent_rolling ?? 0, q?.percent_weekly ?? 0, q?.percent_monthly ?? 0);
    if (acct.enabled && worst >= 100) issues.push({ tone: 'warn', text: `${acct.name} is out of quota` });
    if (checkCatalog && acct.enabled && h.model_id && Array.isArray(catalog) && catalog.length > 0 && !catalog.includes(h.model_id)) {
      issues.push({ tone: 'warn', text: `${acct.name} does not list ${h.model_id}` });
    }
  }
  return { issues, usable: h.enabled !== false && !issues.some((i) => i.tone === 'bad') };
}

export type ComboHealth = { hops: HopState[]; usable: number; total: number; issues: number };

export function comboHealth(c: Combo, accounts: Account[], catalogs: Record<number, string[] | null>): ComboHealth {
  const byId = new Map(accounts.map((a) => [a.id, a]));
  const hops = (c.hops ?? []).map((h) => hopState(h, byId.get(h.account_id), catalogs[h.account_id], !c.auto_free));
  return {
    hops, total: hops.length,
    usable: hops.filter((h) => h.usable).length,
    issues: hops.filter((h) => h.issues.length > 0).length,
  };
}

/** A combo that is on but has nothing that can serve: every request to it fails. */
export const cannotServe = (c: Combo, h: ComboHealth) => c.enabled && h.usable === 0;

/** The live block cursor sitting on this hop, if any. */
export function cursorFor(c: Combo, cursors: BlockCursor[]): BlockCursor | undefined {
  return cursors.find((k) => k.combo === c.name && !!k.hop);
}

// ── draft validation ────────────────────────────────────────────────────────
export type DraftShape = {
  name: string; strategy: string; sticky_idle_s: number; context_size: number; block_size: number;
  auto_free: boolean; hops: (Hop & { custom?: boolean })[];
  auto_free_include?: FreeInclude[];
};
export type DraftErrors = { name?: string; context?: string; block?: string; idle?: string; hops?: string; hop: Record<number, string> };

/** Everything the editor must refuse before it lets Save through, each with a
 *  reason the operator can act on. `others` are the other combos' names. */
export function validateDraft(d: DraftShape, others: string[]): DraftErrors {
  const e: DraftErrors = { hop: {} };
  const name = d.name.trim();
  if (name === '') e.name = 'Give the combo a name — clients pick it by this name.';
  else if (others.some((n) => n.toLowerCase() === name.toLowerCase())) e.name = `A combo called "${others.find((n) => n.toLowerCase() === name.toLowerCase())}" already exists — saving would overwrite it.`;
  else if (name.includes('/')) e.name = 'No "/" in the name: a slash reads as a provider prefix, and combos have no namespace.';
  if (!Number.isInteger(d.context_size) || d.context_size < 1 || d.context_size > 10_000_000) e.context = 'A whole number of tokens, 1 to 10,000,000.';
  if (d.strategy === 'block_round_robin' && (!Number.isInteger(d.block_size) || d.block_size < 1 || d.block_size > 1000)) e.block = 'A whole number, 1 to 1,000.';
  if (d.strategy === 'sticky_last_good' && (!Number.isInteger(d.sticky_idle_s) || d.sticky_idle_s < 0)) e.idle = 'Zero or more whole seconds.';
  if (!d.auto_free) {
    if (d.hops.length === 0) e.hops = 'Add at least one hop.';
    const seen = new Map<string, number>();
    d.hops.forEach((h, i) => {
      if (h.account_id <= 0) e.hop[i] = 'Pick a provider.';
      else if (h.model_id.trim() === '') e.hop[i] = 'Pick or type a model.';
      else {
        const k = `${h.account_id}/${h.model_id.trim()}`;
        const first = seen.get(k);
        if (first !== undefined) e.hop[i] = `Same provider and model as hop ${first + 1}.`;
        else seen.set(k, i);
      }
    });
  }
  return e;
}
export const hasErrors = (e: DraftErrors) => !!(e.name || e.context || e.block || e.idle || e.hops || Object.keys(e.hop).length);

// ── what happens next ───────────────────────────────────────────────────────
export type PlanHop = { label: string; usable: boolean; why: string };

/** One sentence per strategy on what the next request does, given the hops that
 *  can serve. Reasoning only: it mirrors the router's documented behaviour, it
 *  does not call it. */
export function describePlan(strategy: string, hops: PlanHop[], o: { blockSize: number; idleS: number; hold: boolean[] }): string[] {
  const live = hops.filter((h) => h.usable);
  const out: string[] = [];
  if (hops.length === 0) return ['Nothing to route to yet.'];
  if (live.length === 0) {
    return ['No hop can serve right now, so every request to this combo would fail. Fix the hops marked below or switch one on.'];
  }
  const names = live.map((h) => h.label);
  const list = (xs: string[]) => (xs.length <= 2 ? xs.join(' and ') : `${xs.slice(0, -1).join(', ')} and ${xs[xs.length - 1]}`);
  switch (strategy) {
    case 'failover':
      out.push(`Every request goes to ${names[0]}.`);
      if (names.length > 1) out.push(`If that fails (rate limit, server error, timeout) it moves on to ${list(names.slice(1))}, in that order.`);
      else out.push('There is nothing to fail over to: add a second hop for resilience.');
      break;
    case 'block_round_robin':
      out.push(`${names[0]} serves the next ${o.blockSize} request${o.blockSize === 1 ? '' : 's'}${names.length > 1 ? `, then ${list(names.slice(1))} take the same number in turn, and it loops` : ''}.`);
      if (live.some((_, i) => o.hold[hops.indexOf(live[i])])) out.push('A hop marked "hold until fail" keeps its block for as long as it keeps succeeding.');
      if (names.length === 1) out.push('With one usable hop there is nothing to rotate between.');
      break;
    case 'true_round_robin':
      out.push(`Each request picks one of ${list(names)} at random${names.length > 1 ? `, about 1 in ${names.length} each` : ''}. No hop gets a streak guarantee, and prompt caches go cold.`);
      break;
    case 'sticky_last_good':
      out.push(`The first request goes to ${names[0]}, and traffic stays on whichever hop last worked.`);
      out.push(o.idleS > 0 ? `After ${o.idleS.toLocaleString('en-US')} second${o.idleS === 1 ? '' : 's'} with no requests the choice resets to ${names[0]}.` : 'With a sticky idle of 0 it never expires on idle.');
      break;
    case 'least_used':
      out.push(`Each request goes to whichever of ${list(names)} has had the least recent traffic.`);
      break;
    default:
      out.push('This strategy has no description here.');
  }
  const skipped = hops.filter((h) => !h.usable);
  for (const h of skipped) out.push(`${h.label} is skipped: ${h.why}.`);
  return out;
}

// ── starting points for a new combo ─────────────────────────────────────────
export type Preset = {
  key: string; label: string; hint: string;
  strategy: string; block_size?: number; auto_free?: boolean;
};
export const PRESETS: Preset[] = [
  { key: 'failover', label: 'Failover chain', hint: 'Try one provider, fall back to the next when it fails.', strategy: 'failover' },
  { key: 'cache-warm', label: 'Cache-warm rotation', hint: 'Spread load across providers in runs of five, so each one’s prompt cache stays warm.', strategy: 'block_round_robin', block_size: 5 },
  { key: 'free', label: 'Free pool', hint: 'Filled automatically from every free model your providers list.', strategy: 'least_used', auto_free: true },
];

/** A name for a copy that does not collide: "daily copy", "daily copy 2", … */
export function copyName(name: string, taken: string[]): string {
  const lower = new Set(taken.map((n) => n.toLowerCase()));
  let n = `${name}-copy`;
  for (let i = 2; lower.has(n.toLowerCase()); i++) n = `${name}-copy-${i}`;
  return n;
}
