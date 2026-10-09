// ── ezllm admin API contract (docs/API.md, M3) — types + tiny fetch helper ──

const TOKEN_KEY = 'ezllm.admin.token';

export function getToken(): string | null {
  try { return localStorage.getItem(TOKEN_KEY); } catch { return null; }
}
export function setToken(t: string): void { try { localStorage.setItem(TOKEN_KEY, t); } catch { /* noop */ } }
export function clearToken(): void { try { localStorage.removeItem(TOKEN_KEY); } catch { /* noop */ } }

export async function dashboardLogin(password: string): Promise<string> {
  const response = await fetch('/admin/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ password }),
  });
  if (!response.ok) throw new Error(response.status === 401 ? 'incorrect dashboard password' : `login failed (HTTP ${response.status})`);
  const payload = await response.json() as { token: string };
  return payload.token;
}

export async function changeDashboardPassword(currentPassword: string, newPassword: string): Promise<void> {
  await post('/admin/settings/password', { current_password: currentPassword, new_password: newPassword });
}

export type ApiErrorDetail = {
  status: number;
  type: string;
  message: string;
  code?: number;
  step?: string;
  upstream_status?: number;
  upstream_error?: { type?: string; message?: string } | null;
  hint?: string;
  combos?: { id: number; name: string }[];
};

export class ApiError extends Error {
  status: number;
  detail: ApiErrorDetail;
  constructor(d: ApiErrorDetail) {
    super(d.message);
    this.name = 'ApiError';
    this.status = d.status;
    this.detail = d;
  }
}

export class AuthError extends Error {
  status: 401 | 403;
  constructor(status: 401 | 403, msg: string) { super(msg); this.status = status; }
}

function parseOpenAIError(body: unknown, status: number): ApiErrorDetail {
  const o = (body ?? {}) as Record<string, any>;
  const e = (o.error ?? {}) as Record<string, any>;
  if (e && (e.message || e.type)) {
    return {
      status,
      type: typeof e.type === 'string' ? e.type : 'error',
      message: typeof e.message === 'string' ? e.message : 'request failed',
      code: typeof e.code === 'number' ? e.code : undefined,
      step: typeof e.step === 'string' ? e.step : undefined,
      upstream_status: typeof e.upstream_status === 'number' ? e.upstream_status : undefined,
      upstream_error: (e.upstream_error && typeof e.upstream_error === 'object' ? e.upstream_error : null) as ApiErrorDetail['upstream_error'],
      hint: typeof e.hint === 'string' ? e.hint : undefined,
      combos: (Array.isArray(e.combos) ? e.combos : undefined) as ApiErrorDetail['combos'],
    };
  }
  return { status, type: 'error', message: typeof o === 'object' && 'message' in o ? String(o.message) : `HTTP ${status}` };
}

/** Shared fetch: bearer token from localStorage, OpenAI-shaped error parsing, 401/403 → AuthError. */
export async function apiFetch<T = unknown>(path: string, init: RequestInit = {}, body?: unknown): Promise<T> {
  const token = getToken();
  if (!token) throw new AuthError(401, 'unlocked');
  let payload: string | undefined;
  const headers: Record<string, string> = { Authorization: `Bearer ${token}` };
  if (body !== undefined) { headers['Content-Type'] = 'application/json'; payload = JSON.stringify(body); }
  let res: Response;
  try {
    res = await fetch(path, { ...init, headers, body: payload });
  } catch {
    throw new ApiError({ status: 0, type: 'network', message: 'upstream unreachable — is ezllm running?' });
  }
  const text = await res.text();
  let parsed: unknown;
  try { parsed = JSON.parse(text) as unknown; } catch { parsed = undefined; }
  // Upstream 401s pass through with the PROVIDER's body (string error.code, e.g.
  // anthropic's "authentication_error" — the provider rejecting ITS key). Only
  // our own auth 401 (numeric code from writeErr) may clear the session:
  // treating a provider 401 as a dead session logged the user out mid-chat.
  if (res.status === 401 && parseOpenAIError(parsed, res.status).code === 401) {
    clearToken();
    try { sessionStorage.setItem('ezllm.admin.rejected', '1'); } catch { /* noop */ }
    window.location.reload();
    throw new AuthError(401, 'invalid token');
  }
  if (res.status === 403) throw new AuthError(403, 'this token lacks the admin role');
  if (!res.ok) throw new ApiError(parseOpenAIError(parsed, res.status));
  return parsed as T;
}

export const get = <T = unknown>(p: string) => apiFetch<T>(p, { method: 'GET' });
export const post = <T = unknown>(p: string, body?: unknown) => apiFetch<T>(p, { method: 'POST' }, body);
export const patch = <T = unknown>(p: string, body: unknown) => apiFetch<T>(p, { method: 'PATCH' }, body);
export const del = (p: string) => apiFetch<null>(p, { method: 'DELETE' });

// ── Shapes ───────────────────────────────────────────────────────────────

export type UsageRow = { k: string; calls: number; tin: number; tout: number; cread: number; cwrite: number; reasoning: number; saved: number; errors: number; context_pre: number; context_saved: number };

export type Health = {
  status: string; schema_version: number; accounts: number; provider_keys: number;
  models: number; combos: number; ledger_rows: number; dropped_rows: number; uptime_s: number;
};

export type QuotaSnapshot = {
  kind: 'none' | 'percent' | 'money' | 'tokens' | 'unknown' | string;
  percent_rolling?: number | null;
  percent_weekly?: number | null;
  percent_monthly?: number | null;
  balance?: number | string | null;
  unit?: string | null;
  plan?: string | null;
  resets_at?: string | null;
  read_at?: string | null;
};

export type KeyRow = {
  id: number; label: string; hint: string; enabled: boolean;
  last_test_at?: string | null; last_test_ok?: boolean | null; last_test_detail?: string | null;
};

export type ProtocolSupport = { openai?: number | null; anthropic?: number | null; responses?: number | null; untested?: number | null };

export type Account = {
  id: number; name: string; namespace: string;
  kind: 'opencode-go' | 'openai-compatible' | 'anthropic-compatible' | string;
  base_url: string; enabled: boolean; probe_delay_ms?: number; notes?: string | null;
  requires_session_header?: boolean; custom_headers?: Record<string, unknown> | null;
  cap_window?: string | null; cap_tokens?: number;
  keys: KeyRow[];
  quota?: QuotaSnapshot | null;
  models_count?: number;
  protocol_support?: ProtocolSupport | null;
};

export type RecentCall = {
  ts: string; client: string; surface: string; account: string; model: string;
  status: number; ttft_ms: number; total_ms: number; tin: number; tout: number; cread: number;
  saved: number; compression: string; applied: boolean; contextPre?: number; contextSaved?: number;
};

// The feed's snapshot rows come from recentCalls (ledger names: tin, saved);
// live rows come from store.CallEvent (tokens_in, tokens_saved). Read
// whichever the row actually carries so both render with the same fields.
export type FeedRow = {
  ts: string; client: string; surface: string; alias?: string; account: string; model: string;
  status: number; stream?: boolean; ttft_ms?: number; total_ms?: number;
  tin: number; tout: number; cread?: number; reasoning?: number; saved: number;
  contextPre?: number; contextSaved?: number;
  compression: string; applied: boolean; rulesFired?: number; error?: string;
};
const num = (v: unknown): number => (typeof v === 'number' ? v : 0);
export function normalizeFeedRow(raw: any): FeedRow {
  return {
    ts: String(raw.ts ?? ''), client: String(raw.client ?? ''),
    surface: String(raw.surface ?? ''), alias: raw.alias ? String(raw.alias) : undefined,
    account: String(raw.account ?? ''), model: String(raw.model ?? ''),
    status: num(raw.status), stream: raw.stream ? true : undefined,
    ttft_ms: raw.ttft_ms == null ? undefined : num(raw.ttft_ms),
    total_ms: raw.total_ms == null ? undefined : num(raw.total_ms),
    tin: num(raw.tin ?? raw.tokens_in), tout: num(raw.tout ?? raw.tokens_out),
    cread: num(raw.cread ?? raw.tokens_cached_read),
    reasoning: num(raw.reasoning ?? raw.reasoning_tokens),
    rulesFired: num(raw.rules_fired ?? raw.compression_rules_fired),
    saved: num(raw.saved ?? raw.tokens_saved),
    contextPre: num(raw.context_tokens_pre), contextSaved: num(raw.context_tokens_saved),
    compression: String(raw.compression ?? ''), applied: Boolean(raw.applied),
    error: raw.error ? String(raw.error) : undefined,
  };
}

export type Overview = {
  health: Health;
  usage: UsageRow[];
  usage_by_surface: UsageRow[];
  accounts: Account[];
  recent_calls: RecentCall[];
};

// ── Usage granularity (M5) ─────────────────────────────────────────────────
export type UsageResp = {
  from: string;
  to: string;
  group_by: string;
  groups: UsageRow[];
  totals: UsageRow;
};

// Ranges the dashboard offers. 1y uses 365d (not a calendar year) so the
// label matches the window exactly.
export type RangeKey = '24h' | '7d' | '30d' | '365d';
export const RANGE_HOURS: Record<RangeKey, number> = { '24h': 24, '7d': 168, '30d': 720, '365d': 8760 };
export const RANGE_LABEL: Record<RangeKey, string> = { '24h': '24h', '7d': '7d', '30d': '30d', '365d': '1y' };

// The six granularities the user can select.
export type Granularity = 'global' | 'provider' | 'model' | 'cache' | 'compression' | 'savings';

// group_by the endpoint needs for a given granularity (null = use totals only).
export const GRANULARITY_GROUP: Record<Granularity, string | null> = {
  global: null,
  provider: 'account',
  model: 'model',
  cache: 'account',     // cache cols are per-row; group by account to attribute them
  compression: 'account',
  savings: 'account',
};

// usage fetches the /admin/usage report for a range + granularity.
export function usage(range: RangeKey, gran: Granularity): Promise<UsageResp> {
  const hours = RANGE_HOURS[range];
  const from = new Date(Date.now() - hours * 3600_000).toISOString();
  const g = GRANULARITY_GROUP[gran];
  const params = new URLSearchParams({ from });
  if (g) params.set('group_by', g);
  return get<UsageResp>(`/admin/usage?${params.toString()}`);
}

// ── Requests explorer (token dissection) ──────────────────────────────────
// Filter the calls ledger by tokens-in / tokens-out over a range. The mimo
// token plan kept raising the same question — is the CLIENT sending too much,
// or is the MODEL burning it on output/reasoning? — and aggregates hide the
// outliers that answer it. summary covers the WHOLE matching set, not just
// the returned page.
export type RequestsSummary = {
  count: number; tin: number; tout: number; cread: number;
  cached_read?: number; tokens_in?: number; tokens_out?: number;
  reasoning: number; max_tin: number; max_tout: number;
  context_pre: number; context_saved: number; compression_saved: number;
};
export type RequestsResp = {
  rows: FeedRow[];
  summary: RequestsSummary;
  filter: { from: string; to: string; limit: number; sort: string };
};
export type RequestSort = 'ts_desc' | 'tin_desc' | 'tout_desc';
export type RequestFilter = {
  minTin?: number; maxTin?: number; minTout?: number; maxTout?: number;
  range?: RangeKey; sort?: RequestSort; limit?: number;
};
export function requests(f: RequestFilter = {}): Promise<RequestsResp> {
  const hours = RANGE_HOURS[f.range ?? '24h'];
  const params = new URLSearchParams({ from: new Date(Date.now() - hours * 3600_000).toISOString() });
  if (f.minTin != null) params.set('min_tin', String(f.minTin));
  if (f.maxTin != null) params.set('max_tin', String(f.maxTin));
  if (f.minTout != null) params.set('min_tout', String(f.minTout));
  if (f.maxTout != null) params.set('max_tout', String(f.maxTout));
  if (f.sort) params.set('sort', f.sort);
  if (f.limit) params.set('limit', String(f.limit));
  return get<RequestsResp>(`/admin/requests?${params.toString()}`);
}

export type TestStep = { step: string; ok?: boolean; ms?: number; detail?: string; error?: string };
export type TestResult = { ok: boolean; steps: TestStep[]; catalog?: { id: string; owned_by?: string }[]; quota?: QuotaSnapshot | null; deferred?: string[] };
export type TestOutcome = { key: KeyRow; test: TestResult };

export type ModelRow = {
  id: string; display?: string | null; owned_by?: string | null;
  openai?: boolean | number | null; anthropic?: boolean | number | null; responses?: boolean | number | null;
  tested_at?: string | null;
  /** Operator-pinned surface; empty string = auto (probed). Enforced by the router. */
  pin?: string | null;
};

export type SyncResult = { synced: number; protocol_tested: number; protocol_support: ProtocolSupport; ms: number };

export type Hop = { account_id: number; model_id: string; weight: number; enabled: boolean };

export type Combo = {
  id: number; name: string; strategy: string; sticky_idle_s: number | null; enabled: boolean;
  compression_profile_id?: number | null; context_size: number;
  hops: Hop[];
};

// ── model rules: eligibility (usage cap + allowed-use window) ──
export type ModelRule = {
  id: number; account_id: number; model_id: string;
  cap_tokens: number; cap_window: string;
  win_start?: string; win_end?: string; win_days?: string;
  win_tz: string; enabled: boolean; note?: string; updated_at: string;
};

export const modelRules = () => get<ModelRule[]>('/admin/model-rules');
export const accountModels = (id: number) => get<ModelRow[]>(`/admin/accounts/${id}/models`);

/** Pin a model to one surface ("" clears). force=true allows a surface the probe never confirmed. */
export const setModelPin = (accountId: number, model: string, pin: string, force = false) =>
  apiFetch<{ model: string; pin: string }>(
    `/admin/accounts/${accountId}/model-pin`,
    { method: 'PUT' },
    { model, pin, force },
  );
export const chat = (model: string, messages: { role: string; content: string }[]) =>
  post<any>('/admin/chat', { model, messages });

/**
 * chatStream sends the same chat request with stream:true and feeds each SSE
 * content delta to onDelta as the upstream emits it. Resolves on [DONE] or
 * stream end; rejects with the usual ApiError/AuthError shape.
 */
export async function chatStream(
  model: string,
  messages: { role: string; content: string }[],
  onDelta: (text: string) => void,
  signal?: AbortSignal,
): Promise<void> {
  const token = getToken();
  if (!token) throw new AuthError(401, 'unlocked');
  let res: Response;
  try {
    res = await fetch('/admin/chat', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` },
      body: JSON.stringify({ model, messages, stream: true }),
      signal,
    });
  } catch (e) {
    if (signal?.aborted) throw e;
    throw new ApiError({ status: 0, type: 'network', message: 'upstream unreachable — is ezllm running?' });
  }
  if (!res.ok || !res.body) {
    const text = await res.text().catch(() => '');
    let parsed: unknown;
    try { parsed = JSON.parse(text) as unknown; } catch { parsed = undefined; }
    const detail = parseOpenAIError(parsed, res.status);
    // Only OUR auth 401 (numeric code) ends the session — a provider 401 is
    // the upstream rejecting its own key and must not lock the dashboard.
    if (res.status === 401 && detail.code === 401) { clearToken(); throw new AuthError(401, detail.message); }
    throw new ApiError(detail);
  }

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buf = '';
  const emit = (frame: string) => {
    for (const raw of frame.split('\n')) {
      const line = raw.replace(/\r$/, '');
      if (!line.startsWith('data:')) continue;
      const data = line.slice(5).trim();
      if (!data || data === '[DONE]') continue;
      try {
        const o = JSON.parse(data) as { choices?: { delta?: { content?: unknown } }[] };
        const delta = o?.choices?.[0]?.delta?.content;
        if (typeof delta === 'string' && delta.length > 0) onDelta(delta);
      } catch { /* keepalive comment or partial frame — next read finishes it */ }
    }
  };
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true }).replace(/\r\n/g, '\n');
    let sep: number;
    while ((sep = buf.indexOf('\n\n')) !== -1) {
      emit(buf.slice(0, sep));
      buf = buf.slice(sep + 2);
    }
  }
  if (buf.trim()) emit(buf);
}
export const createModelRule = (r: Partial<ModelRule> & { account_id: number; model_id: string }) =>
  post<ModelRule>('/admin/model-rules', r);
// PATCH merges onto the stored row, but window fields are all-or-nothing —
// a payload without them CLEARS the window, so callers always send the full row.
export const updateModelRule = (id: number, r: Partial<ModelRule>) =>
  patch<ModelRule>(`/admin/model-rules/${id}`, r);
export const deleteModelRule = (id: number) => del(`/admin/model-rules/${id}`);

// Mirror of internal/rules Window.InWindow — kept byte-for-byte in spirit:
// no window = always; half window / start==end = fail closed; overnight
// windows attribute the weekday filter to the day the window OPENED.
const DAY_NAMES = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'] as const;

function tzParts(tz: string, d: Date): { h: number; m: number; date: string } {
  try {
    const fmt = new Intl.DateTimeFormat('en-CA', {
      timeZone: tz, year: 'numeric', month: '2-digit', day: '2-digit',
      hour: '2-digit', minute: '2-digit', hourCycle: 'h23',
    });
    const p: Record<string, string> = {};
    for (const part of fmt.formatToParts(d)) if (part.type !== 'literal') p[part.type] = part.value;
    return { h: Number(p.hour), m: Number(p.minute), date: `${p.year}-${p.month}-${p.day}` };
  } catch {
    const p2 = (n: number) => String(n).padStart(2, '0');
    return {
      h: d.getHours(), m: d.getMinutes(),
      date: `${d.getFullYear()}-${p2(d.getMonth() + 1)}-${p2(d.getDate())}`,
    };
  }
}
const dowOfDate = (date: string) => new Date(`${date}T00:00:00Z`).getUTCDay();
const dayBefore = (date: string) =>
  new Date(new Date(`${date}T00:00:00Z`).getTime() - 86400000).toISOString().slice(0, 10);

export function formatDays(csv: string | undefined): string {
  const days = (csv ?? '').split(',').map((x) => x.trim()).filter(Boolean).map(Number);
  if (!days.length) return 'any day';
  const sorted = [...new Set(days)].sort((a, b) => a - b);
  const runs: string[] = [];
  for (let i = 0; i < sorted.length; ) {
    let j = i;
    while (j + 1 < sorted.length && sorted[j + 1] === sorted[j] + 1) j++;
    runs.push(j - i >= 2 ? `${DAY_NAMES[sorted[i]]}–${DAY_NAMES[sorted[j]]}` : sorted.slice(i, j + 1).map((d) => DAY_NAMES[d]).join(','));
    i = j + 1;
  }
  return runs.join(', ');
}

export function windowState(
  r: Pick<ModelRule, 'win_start' | 'win_end' | 'win_days' | 'win_tz'>,
  now: Date = new Date(),
): { open: boolean; label: string } {
  const s = r.win_start ?? '';
  const e = r.win_end ?? '';
  if (!s && !e) return { open: true, label: 'always' };
  if (!s || !e) return { open: false, label: 'invalid (half window)' };
  const [sh, sm] = s.split(':').map(Number);
  const [eh, em] = e.split(':').map(Number);
  const startM = sh * 60 + sm;
  const endM = eh * 60 + em;
  if (!Number.isFinite(startM) || !Number.isFinite(endM)) return { open: false, label: 'invalid time' };
  if (startM === endM) return { open: false, label: 'invalid (start==end)' };

  const { h, m, date } = tzParts(r.win_tz || 'UTC', now);
  const nowM = h * 60 + m;
  let open: boolean;
  let anchor = date;
  if (startM < endM) {
    open = nowM >= startM && nowM < endM;
  } else {
    open = nowM >= startM || nowM < endM;
    if (nowM < endM) anchor = dayBefore(date); // overnight tail: window opened yesterday
  }
  const days = (r.win_days ?? '').split(',').map((x) => x.trim()).filter(Boolean).map(Number);
  if (open && days.length && !days.includes(dowOfDate(anchor))) open = false;
  return { open, label: `${s}–${e}` };
}

// ── compression profiles (M5) ──
export type CompressionStage = { engine: string; options?: Record<string, unknown> };
export type CompressionProfile = {
  id: number;
  name: string;
  enabled: boolean;
  stages: CompressionStage[];
  exempt_last_turn: boolean;
  min_compress_ratio: number;
  fail_open: boolean;
  auto_trigger_tokens: number;
  notes?: string | null;
  created_at?: string | null;
} & Record<string, unknown>;

export function compressionProfiles(): Promise<CompressionProfile[]> {
  return get<CompressionProfile[]>('/admin/compression-profiles');
}

export const STEPS = ['format', 'catalog', 'auth', 'quota', 'inference', 'protocol'] as const;

export const KINDS = ['opencode-go', 'openai-compatible', 'anthropic-compatible', 'gemini-openai'] as const;

export const STRATEGY_HINTS: Record<string, string> = {
  failover: 'try hops in order until one works',
  true_round_robin: 'rotate through every hop evenly',
  strict_round_robin: 'rotate hops exactly in order, always',
  sticky_last_good: 'stay on the last working hop until it errors or idles 30 min',
  least_used: 'pick whichever hop has the fewest active calls',
};
