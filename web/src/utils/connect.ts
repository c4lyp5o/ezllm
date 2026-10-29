// Connect-page logic: what an API key's state is, how a model allowlist matches,
// expiry presets, create-form validation and the paste-ready client recipes.
// Kept out of the component so the list, the editors and the tests agree.
import type { TokenInfo } from './tokens';

// ── allowlist matching ──────────────────────────────────────────────────────

/** Go's path.Match, which is what the server uses for a key's allowlist: `*`
 *  and `?` never cross a `/`, `[a-z]` / `[^a-z]` are classes, `\x` escapes. A
 *  bare `*` is special-cased by the server and allows everything. */
export function globMatch(pattern: string, name: string): boolean {
  if (pattern === '*' || pattern === name) return true;
  let re = '^';
  for (let i = 0; i < pattern.length; i++) {
    const c = pattern[i];
    if (c === '*') re += '[^/]*';
    else if (c === '?') re += '[^/]';
    else if (c === '\\') { i++; if (i >= pattern.length) return false; re += pattern[i].replace(/[.*+?^${}()|[\]\\]/g, '\\$&'); }
    else if (c === '[') {
      const end = pattern.indexOf(']', i + 2);
      if (end < 0) return false;
      let body = pattern.slice(i + 1, end);
      const neg = body.startsWith('^');
      if (neg) body = body.slice(1);
      re += `[${neg ? '^' : ''}${body.replace(/[\\\]]/g, '\\$&')}]`;
      i = end;
    } else re += c.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  }
  try { return new RegExp(`${re}$`).test(name); } catch { return false; }
}

export const splitPatterns = (csv: string) => csv.split(',').map((p) => p.trim()).filter(Boolean);

/** The model ids an allowlist lets through. An empty list allows everything. */
export function allowedModels(csv: string, ids: string[]): string[] {
  const patterns = splitPatterns(csv);
  if (patterns.length === 0) return ids;
  return ids.filter((id) => patterns.some((p) => globMatch(p, id)));
}

// ── a key's state ───────────────────────────────────────────────────────────
export type KeyState = {
  key: 'disabled' | 'expired' | 'spent' | 'expiring' | 'active';
  label: string; tone: 'neutral' | 'ok' | 'warn' | 'bad'; attention: boolean;
};

const DAY = 86_400_000;
/** The expiry as the server reads it: a date alone means the end of that day. */
export function expiryTime(t: Pick<TokenInfo, 'expires_at'>): number | null {
  if (!t.expires_at) return null;
  const ms = Date.parse(t.expires_at);
  return Number.isNaN(ms) ? null : ms;
}
export const daysLeft = (ms: number, now = Date.now()) => Math.ceil((ms - now) / DAY);

/** One status per key, worst first. `used` is the tokens spent inside the key's budget window. */
export function keyState(t: TokenInfo, used: number, now = Date.now()): KeyState {
  if (!t.enabled) return { key: 'disabled', label: 'Disabled', tone: 'neutral', attention: false };
  const exp = expiryTime(t);
  if (exp != null && exp < now) return { key: 'expired', label: 'Expired', tone: 'bad', attention: true };
  if (t.cap_tokens > 0 && used >= t.cap_tokens) return { key: 'spent', label: 'Budget spent', tone: 'bad', attention: true };
  if (exp != null) {
    const d = daysLeft(exp, now);
    if (d <= 7) return { key: 'expiring', label: d <= 1 ? 'Expires today' : `Expires in ${d} days`, tone: 'warn', attention: true };
  }
  return { key: 'active', label: 'Active', tone: 'ok', attention: false };
}

// ── expiry presets ──────────────────────────────────────────────────────────
export const EXPIRY_PRESETS = [
  { key: 'never', label: 'Never', days: 0 },
  { key: '30', label: '30 days', days: 30 },
  { key: '90', label: '90 days', days: 90 },
  { key: '365', label: '1 year', days: 365 },
] as const;

/** A preset as the date-picker value (YYYY-MM-DD, local). Never = empty. */
export function presetDate(days: number, now = new Date()): string {
  if (days <= 0) return '';
  const d = new Date(now.getTime() + days * DAY);
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}

/** "Expires in 30 days" for a date-picker value, counted in calendar days from
 *  today; a past date warns, since the key would be expired at once. */
export function expiryReadout(date: string, now = Date.now()): string {
  if (!date) return 'Never expires.';
  const end = Date.parse(`${date}T23:59:59`);
  if (Number.isNaN(end)) return '';
  if (end < now) return 'That date has passed: the key would be expired at once.';
  const today = new Date(now); today.setHours(0, 0, 0, 0);
  const d = Math.round((Date.parse(`${date}T00:00:00`) - today.getTime()) / DAY);
  return d <= 0 ? 'Expires at the end of today.' : `Expires in ${d} day${d === 1 ? '' : 's'}.`;
}

// ── create-form validation ──────────────────────────────────────────────────
export type KeyDraft = { name: string; role: 'infer' | 'admin'; models: string; expiry: string; cap: number; capWindow: string };
export type KeyErrors = { name?: string; cap?: string; expiry?: string };

export function validateKey(d: KeyDraft, others: string[]): KeyErrors {
  const e: KeyErrors = {};
  const name = d.name.trim();
  if (!name) e.name = 'Give the key a name. It is how this key appears in the ledger.';
  else if (others.some((n) => n.toLowerCase() === name.toLowerCase())) e.name = `A key called "${others.find((n) => n.toLowerCase() === name.toLowerCase())}" already exists. Creating it again would replace that key's secret and break whatever uses it.`;
  if (!Number.isInteger(d.cap) || d.cap < 0) e.cap = 'A whole number of tokens, 0 or more.';
  if (d.expiry && Date.parse(`${d.expiry}T23:59:59`) < Date.now()) e.expiry = 'That date has passed.';
  return e;
}
export const hasKeyErrors = (e: KeyErrors) => !!(e.name || e.cap || e.expiry);

// ── recipes ─────────────────────────────────────────────────────────────────
export type RecipeKey = 'env-openai' | 'env-anthropic' | 'claude' | 'python' | 'node';
export const RECIPES: { key: RecipeKey; label: string; hint: string }[] = [
  { key: 'env-openai', label: 'OpenAI env', hint: 'Environment variables read by most OpenAI-compatible tools.' },
  { key: 'env-anthropic', label: 'Anthropic env', hint: 'Environment variables read by Anthropic-compatible tools.' },
  { key: 'claude', label: 'Claude Code', hint: 'Run Claude Code against this gateway.' },
  { key: 'python', label: 'Python', hint: 'The official openai package.' },
  { key: 'node', label: 'Node', hint: 'The official openai package.' },
];

const q = (s: string) => JSON.stringify(s);

/** A paste-ready snippet for a client. `base` is the origin; OpenAI clients take
 *  it with /v1, Anthropic clients append /v1/messages themselves so theirs is bare. */
export function recipe(kind: RecipeKey, base: string, key: string, model: string): string {
  switch (kind) {
    case 'env-openai':
      return [`OPENAI_BASE_URL=${base}/v1`, `OPENAI_API_KEY=${key}`, `OPENAI_MODEL=${model}`].join('\n');
    case 'env-anthropic':
      return [`ANTHROPIC_BASE_URL=${base}`, `ANTHROPIC_API_KEY=${key}`, `ANTHROPIC_MODEL=${model}`].join('\n');
    case 'claude':
      return [`export ANTHROPIC_BASE_URL=${base}`, `export ANTHROPIC_AUTH_TOKEN=${key}`, `export ANTHROPIC_MODEL=${model}`, 'claude'].join('\n');
    case 'python':
      return [
        'from openai import OpenAI', '',
        `client = OpenAI(base_url=${q(`${base}/v1`)}, api_key=${q(key)})`,
        `reply = client.chat.completions.create(model=${q(model)}, messages=[{"role": "user", "content": "Hello!"}])`,
        'print(reply.choices[0].message.content)',
      ].join('\n');
    case 'node':
      return [
        'import OpenAI from "openai";', '',
        `const client = new OpenAI({ baseURL: ${q(`${base}/v1`)}, apiKey: ${q(key)} });`,
        `const reply = await client.chat.completions.create({ model: ${q(model)}, messages: [{ role: "user", content: "Hello!" }] });`,
        'console.log(reply.choices[0].message.content);',
      ].join('\n');
  }
}

// Single-quote shell arguments safely, including provider-supplied model IDs.
export const shellQuote = (value: string) => "'" + value.replaceAll("'", "'\\''") + "'";
export function curlRequest(base: string, model: string, key = 'YOUR_EZLLM_KEY'): string {
  const body = JSON.stringify({ model, messages: [{ role: 'user', content: 'Hello!' }] });
  return [
    `curl ${shellQuote(base + '/v1/chat/completions')} \\`,
    `  -H ${shellQuote(`Authorization: Bearer ${key}`)} \\`,
    `  -H 'Content-Type: application/json' \\`,
    `  -d ${shellQuote(body)}`,
  ].join('\n');
}
