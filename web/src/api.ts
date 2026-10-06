// ── ezllm admin API contract (docs/API.md, M3) — types + tiny fetch helper ──

const TOKEN_KEY = 'ezllm.admin.token';

export function getToken(): string | null {
  try { return localStorage.getItem(TOKEN_KEY); } catch { return null; }
}
export function setToken(t: string): void { try { localStorage.setItem(TOKEN_KEY, t); } catch { /* noop */ } }
export function clearToken(): void { try { localStorage.removeItem(TOKEN_KEY); } catch { /* noop */ } }

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
  if (res.status === 401) {
    clearToken();
    try { sessionStorage.setItem('ezllm.admin.rejected', '1'); } catch { /* noop */ }
    window.location.reload();
    throw new AuthError(401, 'invalid token');
  }
  if (res.status === 403) throw new AuthError(403, 'this token lacks the admin role');
  const text = await res.text();
  const parsed = JSON.parse(text) as unknown;
  if (!res.ok) throw new ApiError(parseOpenAIError(parsed, res.status));
  return parsed as T;
}

export const get = <T = unknown>(p: string) => apiFetch<T>(p, { method: 'GET' });
export const post = <T = unknown>(p: string, body?: unknown) => apiFetch<T>(p, { method: 'POST' }, body);
export const patch = <T = unknown>(p: string, body: unknown) => apiFetch<T>(p, { method: 'PATCH' }, body);
export const del = (p: string) => apiFetch<null>(p, { method: 'DELETE' });

// ── Shapes ───────────────────────────────────────────────────────────────

export type UsageRow = { k: string; calls: number; tin: number; tout: number; cread: number; cwrite: number; reasoning: number; saved: number; errors: number };

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
};

export type Overview = {
  health: Health;
  usage: UsageRow[];
  usage_by_surface: UsageRow[];
  accounts: Account[];
  recent_calls: RecentCall[];
};

export type TestStep = { step: string; ok?: boolean; ms?: number; detail?: string; error?: string };
export type TestResult = { ok: boolean; steps: TestStep[]; catalog?: { id: string; owned_by?: string }[]; quota?: QuotaSnapshot | null; deferred?: string[] };
export type TestOutcome = { key: KeyRow; test: TestResult };

export type ModelRow = {
  id: string; display?: string | null; owned_by?: string | null;
  openai?: boolean | number | null; anthropic?: boolean | number | null; responses?: boolean | number | null;
  tested_at?: string | null;
};

export type SyncResult = { synced: number; protocol_tested: number; protocol_support: ProtocolSupport; ms: number };

export type Hop = { account_id: number; model_id: string; weight: number; enabled: boolean };

export type Combo = {
  id: number; name: string; strategy: string; sticky_idle_s: number | null; enabled: boolean;
  compression_profile_id?: number | null;
  hops: Hop[];
};

export type CompressionProfile = { id: number; name: string } & Record<string, unknown>;

export const STEPS = ['format', 'catalog', 'auth', 'quota', 'inference', 'protocol'] as const;

export const KINDS = ['opencode-go', 'openai-compatible', 'anthropic-compatible'] as const;

export const STRATEGY_HINTS: Record<string, string> = {
  failover: 'try hops in order until one works',
  true_round_robin: 'rotate through every hop evenly',
  strict_round_robin: 'rotate hops exactly in order, always',
  sticky_last_good: 'stay on the last working hop until it errors or idles 30 min',
  least_used: 'pick whichever hop has the fewest active calls',
};
