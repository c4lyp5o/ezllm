// Get started: the first-run flow's state, read from the server so it resumes
// where you left off on any browser.

/** The providers offered as cards, in order. Every id is a PROVIDER_PRESETS key. */
export const START_PRESETS = [
  'openai', 'claude-platform', 'openrouter', 'opencode-go', 'commandcode', 'deepseek',
  'openai-compatible', 'anthropic-compatible',
] as const;
export type StartPreset = typeof START_PRESETS[number];

/** Card titles: shorter than the preset labels, and the two "Other" cards say what they are. */
export const START_TITLES: Record<StartPreset, string> = {
  openai: 'OpenAI', 'claude-platform': 'Anthropic', openrouter: 'OpenRouter', 'opencode-go': 'OpenCode Go',
  commandcode: 'Command Code', deepseek: 'DeepSeek',
  'openai-compatible': 'Other', 'anthropic-compatible': 'Other',
};
export const START_SUBTITLES: Partial<Record<StartPreset, string>> = {
  'openai-compatible': 'OpenAI-compatible', 'anthropic-compatible': 'Anthropic-compatible',
};

type Acct = { id: number; namespace: string; enabled: boolean };
type Tok = { roles: string[]; enabled: boolean; last_used_at?: string | null };

export type StartProgress = { provider: boolean; key: boolean; request: boolean; done: number; next: 1 | 2 | 3 | 4 };

/** An app key is an enabled key that can call models and is not an admin key. */
export const isAppKey = (t: Tok) => t.enabled && t.roles.includes('infer') && !t.roles.includes('admin');

/** Where you are: a provider exists, an app key exists, that key has made a request. */
export function startProgress(accounts: Acct[], tokens: Tok[]): StartProgress {
  const provider = accounts.length > 0;
  const keys = tokens.filter(isAppKey);
  const key = keys.length > 0;
  const request = keys.some((t) => !!t.last_used_at);
  const done = [provider, key, request].filter(Boolean).length;
  const next = !provider ? 1 : !key ? 2 : !request ? 3 : 4;
  return { provider, key, request, done, next };
}

/** The model the test request starts on: a free model of the provider just
 *  added, else its first model, else the first model anywhere. */
export function defaultTestModel(ids: string[], namespace?: string): string {
  const mine = namespace ? ids.filter((id) => id.startsWith(namespace + '/')) : [];
  const pool = mine.length ? mine : ids.filter((id) => id.includes('/'));
  return pool.find((id) => /(^|[-:/_.])free\b/i.test(id)) ?? pool[0] ?? ids[0] ?? '';
}

/** A namespace not already taken: the slug, then slug-2, slug-3… */
export function freeNamespace(slug: string, taken: string[]): string {
  if (!taken.includes(slug)) return slug;
  for (let i = 2; ; i++) {
    const s = `${slug}-${i}`;
    if (!taken.includes(s)) return s;
  }
}

// ── subscriptions ───────────────────────────────────────────────────────────

/** The plans Get started offers to someone with a subscription instead of a key. */
export const SUB_PRESETS = ['claude-sub', 'chatgpt-sub', 'copilot-sub', 'antigravity-sub'] as const;
export type SubPreset = typeof SUB_PRESETS[number];
export const SUB_TITLES: Record<SubPreset, string> = {
  'claude-sub': 'Claude Pro/Max', 'chatgpt-sub': 'ChatGPT Plus/Pro', 'copilot-sub': 'GitHub Copilot', 'antigravity-sub': 'Google Antigravity',
};

/**
 * Where a subscription bridge stands, from what the server reported at
 * startup: ready to connect, on but its CLI isn't signed in, turned off, or
 * unknown (the server didn't list it).
 */
export type BridgeState = 'ready' | 'signed-out' | 'off' | 'unknown';
export function bridgeState(b?: { enabled: boolean; status?: Record<string, unknown> } | null): BridgeState {
  if (!b) return 'unknown';
  if (!b.enabled) return 'off';
  const login = typeof b.status?.login === 'string' ? b.status.login.trim() : '';
  return login && !/^logged[ -]in\b/i.test(login) ? 'signed-out' : 'ready';
}
