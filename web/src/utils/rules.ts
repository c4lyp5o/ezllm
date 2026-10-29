// Rule logic shared by the Rules table and its editor: how a window reads in
// words, what the editor must refuse, which rules have gone stale, and the
// ready-made windows. Kept out of the component so the table, the editor and
// the tests describe a rule the same way.
import { formatDays, type Account, type ModelRule } from './api';
import { formatTokens } from './lib';

export const DAY_NAMES = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'] as const;
export const CAP_WINDOWS = ['5h', 'daily', 'weekly', 'monthly'] as const;
const PERIOD_PHRASE: Record<string, string> = { '5h': '5 hours', daily: 'day', weekly: 'week', monthly: 'month' };

export const parseDays = (csv: string): number[] =>
  csv.split(',').map((x) => x.trim()).filter(Boolean).map(Number).filter((n) => Number.isInteger(n) && n >= 0 && n <= 6);
export const daysCsv = (days: number[]) => [...new Set(days)].sort((a, b) => a - b).join(',');

const sameDays = (a: number[], b: number[]) => daysCsv(a) === daysCsv(b);

/** The allowed days in a word: "Daily", "Weekdays", "Weekends", else "Mon, Wed". */
export function dayPhrase(csv: string): string {
  const d = parseDays(csv);
  if (d.length === 0 || d.length === 7) return 'Daily';
  if (sameDays(d, [1, 2, 3, 4, 5])) return 'Weekdays';
  if (sameDays(d, [0, 6])) return 'Weekends';
  return formatDays(daysCsv(d));
}

export type WindowFields = { win_start?: string; win_end?: string; win_days?: string; win_tz?: string };

/** "Always", or "Weekdays 09:00–18:00". The timezone is shown beside it, not in it. */
export function windowPhrase(w: WindowFields): string {
  if (!w.win_start && !w.win_end) return 'Always';
  return `${dayPhrase(w.win_days ?? '')} ${w.win_start}–${w.win_end}`;
}
export const tzPhrase = (tz: string | undefined) => (!tz || tz === 'instance' ? 'instance timezone' : tz);

export function capLabel(r: { cap_tokens: number; cap_window: string }): string {
  if (!r.cap_tokens) return 'unlimited';
  return `${formatTokens(r.cap_tokens)} / ${r.cap_window}`;
}

/** The cap as a sentence fragment: "5,000,000 tokens per month". */
export const capPhrase = (tokens: number, window: string) =>
  `${tokens.toLocaleString('en-US')} token${tokens === 1 ? '' : 's'} per ${PERIOD_PHRASE[window] ?? window}`;

// ── drafts ──────────────────────────────────────────────────────────────────
export type Draft = {
  account_id: number; model_ids: string[];
  cap_tokens: number; cap_window: string;
  win_start: string; win_end: string; win_days: string; win_tz: string;
  enabled: boolean; note: string;
};
export type DraftErrors = { models?: string; cap?: string; window?: string };

/** Everything the editor must refuse, each with a reason. Mirrors the server's
 *  store.ValidateModelRule: windows are both-or-neither and start != end. */
export function validateDraft(d: Draft): DraftErrors {
  const e: DraftErrors = {};
  if (d.model_ids.length === 0) e.models = 'Select at least one model.';
  if (!Number.isInteger(d.cap_tokens) || d.cap_tokens < 0) e.cap = 'A whole number of tokens, 0 or more.';
  if (!!d.win_start !== !!d.win_end) e.window = 'A window needs both a start and an end, or neither.';
  else if (d.win_start && d.win_start === d.win_end) e.window = 'Start and end cannot be the same time.';
  return e;
}
export const hasErrors = (e: DraftErrors) => !!(e.models || e.cap || e.window);

/** The rule in one sentence, from what the editor currently holds. */
export function ruleSentence(d: Draft, providerName: string): string {
  const n = d.model_ids.length;
  const who = n === 0 ? `Models on ${providerName}` : n === 1 ? `${d.model_ids[0]} on ${providerName}` : `${n} models on ${providerName}`;
  const parts: string[] = [];
  if (d.cap_tokens > 0) parts.push(`are capped at ${capPhrase(d.cap_tokens, d.cap_window)}`);
  if (d.win_start && d.win_end && d.win_start !== d.win_end) {
    const days = dayPhrase(d.win_days);
    parts.push(`may only run ${d.win_start}–${d.win_end}${days === 'Daily' ? ' every day' : ` on ${days.toLowerCase()}`} (${tzPhrase(d.win_tz)})`);
  }
  if (parts.length === 0) return `${who} have no cap and no window, so this rule changes nothing.`;
  return `${who} ${parts.join(' and ')}.${d.enabled ? '' : ' The rule is switched off.'}`;
}

// ── ready-made windows ──────────────────────────────────────────────────────
export type WindowPreset = { key: string; label: string; title: string; win_start: string; win_end: string; win_days: string };
export const WINDOW_PRESETS: WindowPreset[] = [
  { key: 'always', label: 'Always', title: 'No window: allowed at any time', win_start: '', win_end: '', win_days: '' },
  { key: 'business', label: 'Business hours', title: 'Monday to Friday, 09:00–18:00', win_start: '09:00', win_end: '18:00', win_days: '1,2,3,4,5' },
  { key: 'nights', label: 'Nights', title: 'Every day, 22:00–06:00 (wraps midnight)', win_start: '22:00', win_end: '06:00', win_days: '' },
  { key: 'weekends', label: 'Weekends', title: 'Saturday and Sunday, all day', win_start: '00:00', win_end: '23:59', win_days: '0,6' },
];
export const CAP_QUICK = [100_000, 1_000_000, 10_000_000];

/** The preset a draft currently equals, if any (to show it as the active one). */
export const activePreset = (d: Pick<Draft, 'win_start' | 'win_end' | 'win_days'>) =>
  WINDOW_PRESETS.find((p) => p.win_start === d.win_start && p.win_end === d.win_end && daysCsv(parseDays(p.win_days)) === daysCsv(parseDays(d.win_days)))?.key;

// ── groups ──────────────────────────────────────────────────────────────────
// One row per SETTING, not per model: rules on the same provider with identical
// cap, window, state and note are one group with several models. The wire stays
// one rule per (account, model) — a group is a view, and saving fans out.
export type RuleGroup = { key: string; account_id: number; rules: ModelRule[] };

export const groupKey = (r: ModelRule) => [r.account_id, r.cap_tokens, r.cap_tokens ? r.cap_window : '', r.win_start ?? '', r.win_end ?? '',
  r.win_days ?? '', r.win_tz, r.enabled, r.note ?? ''].join('|');

export function groupRules(rules: ModelRule[]): RuleGroup[] {
  const out = new Map<string, RuleGroup>();
  for (const r of rules) {
    const k = groupKey(r);
    const g = out.get(k);
    if (g) g.rules.push(r);
    else out.set(k, { key: k, account_id: r.account_id, rules: [r] });
  }
  return [...out.values()];
}

// ── stale rules ─────────────────────────────────────────────────────────────
export type RuleIssue = { tone: 'bad' | 'warn'; text: string };

/** What has gone wrong around a rule: its provider is gone or off, or its model
 *  left the catalog. A rule switched off on purpose reports nothing. `catalog`
 *  is the provider's model ids: undefined while loading, null if it failed. */
export function groupIssues(g: RuleGroup, acct: Account | undefined, catalog: string[] | null | undefined): RuleIssue[] {
  if (!g.rules[0]?.enabled) return [];
  if (!acct) return [{ tone: 'bad', text: 'Its provider no longer exists, so this rule can never apply.' }];
  const out: RuleIssue[] = [];
  if (!acct.enabled) out.push({ tone: 'warn', text: `${acct.name} is disabled, so this rule has no effect for now.` });
  if (Array.isArray(catalog) && catalog.length > 0) {
    const gone = g.rules.map((r) => r.model_id).filter((m) => !catalog.includes(m));
    if (gone.length > 0) {
      out.push({ tone: 'warn', text: `${acct.name} no longer lists ${gone.length <= 3 ? gone.join(', ') : `${gone.slice(0, 3).join(', ')} and ${gone.length - 3} more`}.` });
    }
  }
  return out;
}
