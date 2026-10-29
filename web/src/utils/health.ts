// What is wrong with a provider, in one place: the Dashboard's Needs-attention
// strip and the Providers page's "needs attention" filter must never disagree
// about which providers are unhealthy, so both read this.
//
// A provider disabled on purpose is not a fault and reports nothing.
import type { Account } from './api';

/** Where the fix lives: the Edit modal's Settings (cookies) or Keys tab, or the card itself. */
export type IssueFix = 'edit' | 'keys' | 'open';
export type Issue = { id: string; tone: 'bad' | 'warn'; text: string; fix: IssueFix };

export function accountIssues(a: Account, now = Date.now()): Issue[] {
  if (!a.enabled) return [];
  const out: Issue[] = [];
  const keys = a.keys ?? [];
  if (keys.length > 0 && keys.every((k) => !k.enabled)) {
    out.push({ id: `keys-${a.id}`, tone: 'bad', text: `${a.name}: every key is disabled, so it cannot serve`, fix: 'open' });
  } else {
    const failing = keys.filter((k) => k.enabled && k.last_test_ok === false).length;
    if (failing > 0) {
      out.push({ id: `test-${a.id}`, tone: 'warn', text: `${a.name}: ${failing} ${failing === 1 ? 'key' : 'keys'} failed the last test`, fix: 'keys' });
    }
  }
  const q = a.quota;
  const pcts: [string, number][] = [];
  if (q?.percent_rolling != null) pcts.push(['rolling', q.percent_rolling]);
  if (q?.percent_weekly != null) pcts.push(['weekly', q.percent_weekly]);
  if (q?.percent_monthly != null) pcts.push(['monthly', q.percent_monthly]);
  const worst = pcts.sort((x, y) => y[1] - x[1])[0];
  if (worst && worst[1] >= 90) {
    out.push({ id: `quota-${a.id}`, tone: worst[1] >= 100 ? 'bad' : 'warn', text: `${a.name}: ${worst[0]} quota at ${Math.round(worst[1])}%`, fix: 'open' });
  }
  if (a.management_key_expires_at) {
    const days = Math.ceil((Date.parse(a.management_key_expires_at) - now) / 86_400_000);
    if (!Number.isNaN(days) && days <= 3) {
      out.push({ id: `cookie-${a.id}`, tone: days < 0 ? 'bad' : 'warn', text: days < 0 ? `${a.name}: session cookie expired` : `${a.name}: session cookie expires in ${days} ${days === 1 ? 'day' : 'days'}`, fix: 'edit' });
    }
  }
  return out;
}

/** The Providers page's deep-link: opens a provider's card, its Edit modal or its Pricing modal. */
export function providerLink(id: number, intent: 'open' | 'edit' | 'keys' | 'pricing'): string {
  return `/providers?${intent}=${id}`;
}
