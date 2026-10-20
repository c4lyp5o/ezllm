// ── ezllm admin API contract (docs/API.md, M3) — types + tiny fetch helper ──
import { fmtMs, formatTokens } from './lib';

const TOKEN_KEY = 'ezllm.admin.token';
const UNLOCK_KEY = 'ezllm.admin.unlocked';

export function getToken(): string | null {
  try { return localStorage.getItem(TOKEN_KEY); } catch { return null; }
}
export function setToken(t: string): void { try { localStorage.setItem(TOKEN_KEY, t); } catch { /* noop */ } }
export function clearToken(): void {
  try { localStorage.removeItem(TOKEN_KEY); } catch { /* noop */ }
  try { sessionStorage.removeItem(UNLOCK_KEY); } catch { /* noop */ }
}
/** Cookie sessions are httpOnly, so JS cannot inspect them. This marker only
 *  says "this tab logged in"; the cookie is what actually authenticates. */
export function isUnlocked(): boolean {
  if (getToken()) return true;
  try { return sessionStorage.getItem(UNLOCK_KEY) === '1'; } catch { return false; }
}
export function markUnlocked(): void {
  try { sessionStorage.setItem(UNLOCK_KEY, '1'); } catch { /* noop */ }
}

export async function dashboardLogin(password: string): Promise<{ token: string; defaultPassword: boolean }> {
  const response = await fetch('/admin/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    credentials: 'include',
    body: JSON.stringify({ password }),
  });
  const body = await response.json().catch(() => ({})) as { token?: string; default_password?: boolean; error?: { default_password?: boolean } };
  if (!response.ok) {
    const msg = response.status === 401 ? 'incorrect dashboard password' : `login failed (HTTP ${response.status})`;
    throw Object.assign(new Error(msg), { defaultPassword: body.error?.default_password === true });
  }
  // The 200 shape is flat: {token, default_password} (only errors nest under .error).
  return { token: body.token ?? '', defaultPassword: body.default_password === true };
}

// The login page needs the first-run hint state BEFORE any successful login
// exists, so it probes the login endpoint with a wrong password and reads the
// flag off the 401 (docx item 13). Fail-closed: a probe error hides the hint.
export async function probeDefaultPassword(): Promise<boolean> {
  try {
    const response = await fetch('/admin/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      credentials: 'include',
      body: JSON.stringify({ password: '' }),
    });
    const body = await response.json().catch(() => ({})) as { error?: { default_password?: boolean } };
    if (response.status === 401) return body.error?.default_password === true;
    return false;
  } catch {
    return false;
  }
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
  const headers: Record<string, string> = {};
  if (token) headers.Authorization = `Bearer ${token}`;
  if (body !== undefined) { headers['Content-Type'] = 'application/json'; }
  let payload: string | undefined;
  if (body !== undefined) payload = JSON.stringify(body);
  let res: Response;
  try {
    res = await fetch(path, { ...init, headers, body: payload, credentials: 'include' });
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
export const put = <T = unknown>(p: string, body?: unknown) => apiFetch<T>(p, { method: 'PUT' }, body);
export const patch = <T = unknown>(p: string, body: unknown) => apiFetch<T>(p, { method: 'PATCH' }, body);
export const del = (p: string) => apiFetch<null>(p, { method: 'DELETE' });

// ── streamed key test (docx item 8: the step chain must MOVE) ─────────────
// POSTs with Accept: application/x-ndjson and resolves from the terminal
// line the server sends. onStep fires per step AS THEY LAND — the same
// append the final test.steps carries, streamed, so the UI colors the chain
// live instead of a dead spinner followed by a full-color snap.
//
// The server may ignore the header (older build): a plain JSON body is then
// parsed exactly like apiFetch would, so this is safe against both.
export async function postKeyTestStream<T>(
  path: string,
  body: unknown,
  onStep: (s: TestStep) => void,
): Promise<T> {
  const token = getToken();
  let res: Response;
  try {
    res = await fetch(path, {
      method: 'POST',
      headers: {
        ...(token ? { Authorization: `Bearer ${token}` } : {}),
        'Content-Type': 'application/json',
        Accept: 'application/x-ndjson',
      },
      credentials: 'include',
      body: JSON.stringify(body),
    });
  } catch {
    throw new ApiError({ status: 0, type: 'network', message: 'upstream unreachable — is ezllm running?' });
  }
  const contentType = res.headers.get('Content-Type') ?? '';
  const ndjson = contentType.includes('x-ndjson');

  if (res.status === 403) throw new AuthError(403, 'this token lacks the admin role');
  if (!res.ok && !ndjson) {
    // Non-streamed failure (auth, 404, …): same parsing as apiFetch.
    const text = await res.text();
    let parsed: unknown;
    try { parsed = JSON.parse(text) as unknown; } catch { parsed = undefined; }
    if (res.status === 401 && parseOpenAIError(parsed, res.status).code === 401) {
      clearToken();
      try { sessionStorage.setItem('ezllm.admin.rejected', '1'); } catch { /* noop */ }
      window.location.reload();
      throw new AuthError(401, 'invalid token');
    }
    throw new ApiError(parseOpenAIError(parsed, res.status));
  }
  if (!ndjson) {
    // Server answered the old way: one-shot JSON, no step events.
    return await res.json() as T;
  }

  const reader = res.body?.getReader();
  if (!reader) throw new ApiError({ status: 0, type: 'error', message: 'empty stream body' });
  const decoder = new TextDecoder();
  let buf = '';
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });
    let nl: number;
    while ((nl = buf.indexOf('\n')) >= 0) {
      const raw = buf.slice(0, nl).trim();
      buf = buf.slice(nl + 1);
      if (!raw) continue;
      let msg: { type?: string; step?: TestStep; status?: number; error?: ApiErrorDetail; key?: unknown; test?: unknown };
      try { msg = JSON.parse(raw) as typeof msg; } catch { continue; }
      if (msg.type === 'step' && msg.step) onStep(msg.step);
      else if (msg.type === 'error') {
        const detail = { ...(msg.error ?? { status: msg.status ?? 500, type: 'error', message: 'key test failed' }),
          status: msg.error?.status ?? msg.status ?? 500 };
        throw new ApiError(detail);
      } else if (msg.type === 'result') {
        return msg as unknown as T;
      }
    }
  }
  throw new ApiError({ status: 0, type: 'error', message: 'stream ended without a result' });
}

// ── GitHub Copilot device flow (the phone path) ───────────────────────────
// Start a flow, poll its status, cancel it. The OAuth session itself never
// crosses the wire: the key add names the handle (device_handle) and the
// server resolves the credential itself. docs/github-copilot-runbook.md.
export type GhDeviceStart = {
  handle: string;
  verification_uri: string;
  user_code: string;
  expires_in: number;
  interval: number;
};
export type GhDeviceStatus = {
  status: 'pending' | 'authorized' | 'expired' | 'error';
  github_login?: string;
  copilot_expires_at?: string;
  detail?: string;
  // GitHub's RFC 8628 §3.5 "poll less often". GitHub WITHHOLDS the token until
  // the poller lengthens its interval, so this must raise ours (see Providers).
  slow_down?: boolean;
};
export const ghDeviceStart = () => post<GhDeviceStart>('/admin/github/device');
export const ghDeviceStatus = (handle: string) =>
  get<GhDeviceStatus>(`/admin/github/device/${encodeURIComponent(handle)}`);
export const ghDeviceCancel = (handle: string) =>
  del(`/admin/github/device/${encodeURIComponent(handle)}`);

// ── protocol sweep ("test all models") ───────────────────────────────────
export type StartSweepOpts = {
  /** Probe every model, not the 6-of-N sample. */
  all?: boolean;
  /** Include models that already carry a verdict on the requested surfaces
   *  (a re-test). Default false: skip what is already proven. */
  includeTested?: boolean;
  /** Surfaces to probe (default: all three). */
  surfaces?: string[];
  /** Scope the sweep to these model ids (the per-row "Test" action). The
   *  catalog refresh still upserts the FULL catalog — only the probe is
   *  scoped, so a one-model test never prunes the drawer. */
  models?: string[];
};
export const startSweep = (id: number, o: StartSweepOpts = {}) =>
  post<{ synced: number; probing: number; probe_all?: boolean; surfaces?: string[]; note?: string; ms: number }>(
    `/admin/accounts/${id}/sync`,
    {
      probe_all: o.all ?? true,
      only_untested: !(o.includeTested ?? false),
      ...(o.surfaces ? { surfaces: o.surfaces } : {}),
      ...(o.models ? { models: o.models } : {}),
    },
  );
export const sweepStatus = (id: number) => get<SweepStatus>(`/admin/accounts/${id}/sync`);
export const stopSweep = (id: number) => del(`/admin/accounts/${id}/sync`);

// ── Shapes ───────────────────────────────────────────────────────────────

// UsageRow is one group of a usage report. Field names are the LEDGER JSON
// tags verbatim and this declaration is pinned by a Go test
// (internal/store/contract_test.go) that parses it with a regex, so it must
// stay a single brace-free line with NO comments inside the braces — prose
// here is read as field names and the contract test fails.
//
// p50_ttft_ms is the median time-to-first-token for the group, computed in
// SQL with window functions (one row per group, no Go-side aggregation).
// Convention: the UPPER of the two middle values on an even count, matching
// the historical Go median exactly. It shipped on every row and was read by
// nothing for its whole life — it sat in that contract test's "sent but
// unread" allowlist, which is precisely why latency was invisible on the
// dashboard. The analytics explorer's Latency focus now renders it.
export type UsageRow = { k: string; calls: number; tin: number; tout: number; cread: number; cwrite: number; reasoning: number; saved: number; errors: number; context_pre: number; context_saved: number; p50_ttft_ms: number; applied_saved: number };

export type Health = {
  status: string; version: string; schema_version: number; accounts: number; provider_keys: number;
  models: number; combos: number; ledger_rows: number; dropped_rows: number;
  lost_rows?: number; unmetered_rows?: number; uptime_s: number;
};

export type QuotaSnapshot = {
  kind: 'none' | 'percent' | 'money' | 'tokens' | 'unknown' | string;
  percent_rolling?: number | null;
  percent_weekly?: number | null;
  percent_monthly?: number | null;
  balance?: number | string | null;
  unit?: string | null;
  plan?: string | null;
  tokens_today?: number | null;
  requests_today?: number | null;
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
  management_key_set?: boolean;
  management_key_expires_at?: string;
  /** Account-WIDE request budget: requests per rate_window (Go duration, e.g.
   *  "60s"). 0 / absent = unlimited. Enforced by the proxy before dispatch, so
   *  a spent account is SKIPPED and the combo fails over. */
  rate_limit?: number; rate_window?: string | null;
  keys: KeyRow[];
  quota?: QuotaSnapshot | null;
  models_count?: number;
  protocol_support?: ProtocolSupport | null;
};

export type RecentCall = {
  ts: string; client: string; surface: string; account: string; model: string;
  /** Caller address captured at request time (X-Real-IP / X-Forwarded-For /
   *  socket peer). Empty when the ledger row predates v14 or no address was
   *  available. Attribution only — a direct connection can claim any value. */
  ip?: string;
  status: number; ttft_ms: number; total_ms: number; tin: number; tout: number; cread: number;
  saved: number; compression: string; applied: boolean; contextPre?: number; contextSaved?: number;
  // alias = the combo name (or "ns/model" for a direct route); err = the
  // verbatim ledger err column, with the leading `[<cause>]` marker when the
  // row was written after the error-cause classifier shipped, plus any
  // `failover:` trail of hops that were skipped or failed before the served one.
  alias?: string; err?: string; saved_notional?: number; rules_fired?: number;
  // block_pos = block_round_robin dispatch position in its block (0 = not
  // counted): the "N requests finished" tag.
  block_pos?: number;
  // translated = the served hop's WIRE differed from the client's surface, so
  // the bodies were translated (anthropic client -> openai-only hop, or any
  // client onto a hop curated for /v1/responses). On such a row `tin` is the
  // vendor's INCLUSIVE total (cache counted inside) and `cread` is 0. Read the
  // marker before comparing `tin` across rows.
  translated?: boolean;
};

// The feed's snapshot rows come from recentCalls (ledger names: tin, saved);
// live rows come from store.CallEvent (tokens_in, tokens_saved). Read
// whichever the row actually carries so both render with the same fields.
export type FeedRow = {
  ts: string; client: string; surface: string; alias?: string; account: string; model: string;
  /** Caller address (see RecentCall.ip). */
  ip?: string;
  status: number; stream?: boolean; ttft_ms?: number; total_ms?: number;
  tin: number; tout: number; cread?: number; reasoning?: number; saved: number;
  /** What a REJECTED rewrite would have saved. Non-zero while `applied` is
   *  false is the proof the pipeline ran and declined (usually the minimum
   *  ratio floor), not that compression was skipped. */
  savedNotional: number;
  contextPre?: number; contextSaved?: number;
  compression: string; applied: boolean; rulesFired?: number; error?: string;
  /** Translated hop: its wire differs from the client's surface. When set,
   *  `tin` is the vendor's INCLUSIVE total, not our cold-token convention. */
  translated?: boolean;
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
    // Snapshot rows carry the short key, live SSE rows the ledger column name.
    savedNotional: num(raw.saved_notional ?? raw.compression_saved_notional),
    contextPre: num(raw.context_tokens_pre), contextSaved: num(raw.context_tokens_saved),
    compression: String(raw.compression ?? ''), applied: Boolean(raw.applied),
    error: raw.error ? String(raw.error) : undefined,
    // Snapshot rows and live SSE rows both carry the marker; absent = native.
    translated: raw.translated ? true : undefined,
  };
}

export type Overview = {
  health: Health;
  usage: UsageRow[];
  usage_by_surface: UsageRow[];
  accounts: Account[];
  recent_calls: RecentCall[];
  // Live block_round_robin cursors, keyed by the combo NAME (the ledger
  // alias) — the recent-calls table renders the cursor under the model of
  // every row that alias served.
  block_cursors?: BlockCursor[];
};

export type BlockCursor = {
  combo: string; hop: string; remaining: number; block_size: number;
  /** Why this block started: 'cold start' | 'block complete' | 'block hop left
   *  candidates'. Empty/absent on a SEEDED cursor — an enabled block combo that
   *  has not dispatched since the last restart, so its size is known but no hop
   *  owns a live block yet. */
  reason?: string;
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

// Which LEDGER DIMENSION the explorer splits by. The server accepts seven
// (store.usageGroupColumn) and the dashboard used three of them for its whole
// life — account, model, day. The rest answer questions the operator
// actually asks and could not see:
//   client  → which application is burning the tokens
//   alias   → which combo is really getting the traffic
//   key     → which API key, per client
//   surface → openai vs anthropic vs responses uptake
// `global` sends no group_by and reads `totals` only.
export type SplitKey = 'global' | 'day' | 'account' | 'model' | 'client' | 'alias' | 'key' | 'surface';

export const SPLIT_LABEL: Record<SplitKey, string> = {
  global: 'Everything',
  day: 'Day',
  account: 'Provider',
  model: 'Model',
  client: 'Client',
  alias: 'Combo',
  key: 'API key',
  surface: 'Surface',
};

/** One-line "what am I looking at" per split, shown under the toggle. */
export const SPLIT_HINT: Record<SplitKey, string> = {
  global: 'one row for the whole range',
  day: 'traffic over time — the only time bucketing the ledger supports',
  account: 'which upstream the calls actually landed on',
  model: 'which model the traffic resolved to',
  client: 'which application sent it (the x-ezllm-client header)',
  alias: 'which combo a request named, vs a direct model call',
  key: 'which API key was used',
  surface: 'openai vs anthropic vs responses endpoint',
};

// group_by the endpoint needs for a given split (null = totals only).
export const SPLIT_GROUP: Record<SplitKey, string | null> = {
  global: null,
  day: 'day',
  account: 'account',
  model: 'model',
  client: 'client',
  alias: 'alias',
  key: 'key',
  surface: 'surface',
};

// Does this split produce a time-ordered series (a line chart) or a set of
// named entities (a ranked bar list)? Only `day` is a time series — the
// ledger's finest bucket is a calendar day, so a 24h range yields one or two
// points and the honest render is a bar, not a smooth line.
export const SPLIT_IS_TIME: Record<SplitKey, boolean> = {
  global: false, day: true, account: false, model: false,
  client: false, alias: false, key: false, surface: false,
};

// Which column the chosen FOCUS is about. The row carries every column, so a
// focus change is a client-side re-read — no second request.
//
// 'compression' was consolidated INTO 'savings' (dashboard polish pass): the
// two focused on the same `saved` column with `savings` additionally adding
// the (currently dead) context-budget column — two buttons for one question.
export type FocusKey = 'tokens' | 'cache' | 'savings' | 'reasoning' | 'latency' | 'errors';

export const FOCUS_LABEL: Record<FocusKey, string> = {
  tokens: 'Tokens', cache: 'Cache',
  savings: 'Savings', reasoning: 'Reasoning', latency: 'Latency', errors: 'Errors',
};

export const FOCUS_HINT: Record<FocusKey, string> = {
  tokens: 'in vs out — raw volume',
  cache: 'prompt-cache reads and writes',
  savings: 'tokens compression removed (plus context budget when a stage ships it)',
  reasoning: 'tokens the model spent thinking (billed, never seen)',
  latency: 'p50 time to first token per group',
  errors: 'calls that came back 4xx/5xx',
};

/** The value a focus reads out of one row. */
export function focusValue(f: FocusKey, r: UsageRow): number {
  switch (f) {
    case 'cache': return r.cread + r.cwrite;
    case 'savings': return r.context_saved + r.saved;
    case 'reasoning': return r.reasoning;
    case 'latency': return r.p50_ttft_ms;
    case 'errors': return r.errors;
    default: return r.tin + r.tout;
  }
}

/** How a focus renders a value — latency is milliseconds, the rest are tokens. */
export function focusFormat(f: FocusKey): (n: number) => string {
  return f === 'latency' ? fmtMs : formatTokens;
}

// Legacy alias: the old granularity axis folded split + focus into one enum.
// Kept as a type so existing call sites keep compiling; the explorer uses
// SplitKey + FocusKey directly.
export type Granularity = SplitKey;
export const GRANULARITY_GROUP = SPLIT_GROUP;

function rangeFrom(range: RangeKey): string {
  return new Date(Date.now() - RANGE_HOURS[range] * 3600_000).toISOString();
}

/** /admin/usage for a split + range. One request, one view — never two. */
export function usage(split: SplitKey, range: RangeKey): Promise<UsageResp> {
  const params = new URLSearchParams({ from: rangeFrom(range) });
  const g = SPLIT_GROUP[split];
  if (g) params.set('group_by', g);
  return get<UsageResp>(`/admin/usage?${params.toString()}`);
}

/** Escape hatch for a group_by the SplitKey map does not name. */
export function usageGroup(groupBy: string, range: RangeKey): Promise<UsageResp> {
  const params = new URLSearchParams({ from: rangeFrom(range), group_by: groupBy });
  return get<UsageResp>(`/admin/usage?${params.toString()}`);
}

// ── Requests explorer (token dissection) ──────────────────────────────────
// Filter the calls ledger by tokens-in / tokens-out over a range. The mimo
// token plan kept raising the same question — is the CLIENT sending too much,
// or is the MODEL burning it on output/reasoning? — and aggregates hide the
// outliers that answer it. summary covers the WHOLE matching set, not just
// the returned page.
export type RequestsSummary = {
  count: number; tin: number; tout: number; cread: number; cwrite: number;
  cached_read?: number; tokens_in?: number; tokens_out?: number;
  reasoning: number; max_tin: number; max_tout: number;
  context_pre: number; context_saved: number; compression_saved: number;
  // Compression-page cards (added with the four summary SQL columns):
  // applied = a rewrite actually shipped; applied_saved = its real tokens
  // (SUM(tokens_saved) sweeps in legacy declined rows, so cards read THIS);
  // declined = a recorded rewrite rejected by the ratio floor; held_notional
  // = what those rejected rewrites would have saved.
  applied_calls: number; applied_saved: number;
  declined_calls: number; held_notional: number;
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

export type ServiceSurface = 'embeddings' | 'images_gen' | 'audio_tts' | 'audio_stt'
  | 'images_edit' | 'moderations' | 'rerank' | 'videos_gen' | 'music_gen';
export type ServiceMode = 'off' | 'manual' | 'auto';
export type ServiceVerdict = { model: string; ok: boolean; detail: string; tested_at: string };
export type ServiceCard = {
  surface: ServiceSurface; endpoint: string; label: string; description: string;
  mode: ServiceMode; model_target: string; resolved_target: string; target_exists: boolean;
  verified_models: number; accounts: number; verdict: ServiceVerdict | null;
};
export type ServiceTestResult = { surface: ServiceSurface; model: string; ok: boolean; dispatched: boolean; status: number; ms: number; detail: string };
export const serviceCards = () => get<{ services: ServiceCard[] }>('/admin/services');
export const setServiceBinding = (surface: ServiceSurface, mode: ServiceMode, model = '') =>
  put<ServiceCard>(`/admin/services/${encodeURIComponent(surface)}`, { mode, model });
export const testService = (surface: ServiceSurface, model: string, prompt = '') =>
  post<ServiceTestResult>(`/admin/services/${encodeURIComponent(surface)}/test`, { model, prompt });

export type ModelRow = {
  id: string; display?: string | null; owned_by?: string | null;
  openai?: boolean | number | null; anthropic?: boolean | number | null; responses?: boolean | number | null;
  tested_at?: string | null;
  /** Operator-pinned surface; empty string = auto (probed). Enforced by the router. */
  pin?: string | null;
  /** Who last wrote the verdicts: 'probe', 'curated', or absent for a legacy row. */
  source?: string | null;
  /** The operator's reason for a curation (curated rows only). */
  note?: string | null;
};

/** The in-flight protocol sweep for one account (the "test all models" job). */
export type SweepStatus = {
  running: boolean;
  started_at?: string | null; surfaces?: string[]; probing?: number; elapsed_s?: number;
};

export type SyncResult = { synced: number; protocol_tested: number; protocol_support: ProtocolSupport; ms: number };

export type Hop = {
  account_id: number; model_id: string; weight: number; enabled: boolean;
  /** block_round_robin only: this hop does not rotate when its block runs
   *  out — it HOLDS the block while it keeps succeeding and releases it on a
   *  hop-attributable failure (429/5xx/transport/failed stream). A 4xx is the
   *  request's fault and keeps the hold. */
  hold_until_failure?: boolean;
};

/** A model that joins an auto_free pool although its id carries no free marker. */
export type FreeInclude = { account_id: number; model_id: string };

export type Combo = {
  id: number; name: string; strategy: string; sticky_idle_s: number | null; enabled: boolean;
  /** block_round_robin: consecutive requests served by one hop before the
   *  rotation advances. 1 = plain per-request rotation. */
  block_size: number;
  compression_profile_id?: number | null; context_size: number;
  hops: Hop[];
  /** auto_free: hops are derived from the catalogs' free-marked models. */
  auto_free?: boolean;
  /** Account ids the id-marker match draws from; empty = every enabled account. */
  auto_free_accounts?: number[];
  auto_free_include?: FreeInclude[];
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

// ── v8: model prices + derived cost ──────────────────────────────────────
// Rates are versioned per (account, model, date); cost is computed server-side
// from the rate EFFECTIVE on each call's date. usd counts PRICED calls only —
// unpriced_calls tells you when the total is a floor, not the full bill.

export type Price = {
  account_id: number; model_id: string; effective_from: string; currency: string;
  price_in: number; price_out: number; price_cache_read: number; price_cache_write: number;
  note?: string;
};

export const prices = () => get<Price[]>('/admin/prices');

/** Upsert one rate version. All four prices are required (absent is NOT zero/free). */
export const setPrice = (accountId: number, p: Omit<Price, 'account_id'>) =>
  apiFetch<Price>(`/admin/accounts/${accountId}/prices`, { method: 'PUT' }, p);

export const deletePrice = (accountId: number, model: string, effectiveFrom: string) =>
  apiFetch<null>(`/admin/accounts/${accountId}/prices`, { method: 'DELETE' }, { model, effective_from: effectiveFrom });

export type CostRow = {
  day: string; account_id: number; account: string; model: string;
  calls: number; unpriced_calls: number;
  tokens_in: number; tokens_out: number; tokens_cached_read: number; tokens_cached_write: number;
  usd: number;
};

export type CostTotals = { usd: number; calls: number; unpriced_calls: number; pricing_complete: boolean; note?: string };

export const cost = (range: RangeKey = '7d') => {
  const hours = RANGE_HOURS[range];
  const params = new URLSearchParams({ from: new Date(Date.now() - hours * 3600_000).toISOString() });
  return get<{ from: string; to: string; currency: string; rows: CostRow[]; totals: CostTotals }>(`/admin/cost?${params.toString()}`);
};
export const chat = (model: string, messages: ChatMessage[]) =>
  post<any>('/admin/chat', { model, messages });

/**
 * A chat turn's content. Plain string for text; an array of parts when a file
 * is attached, following the OpenAI chat-completions shape:
 *   { type: 'text',        text }
 *   { type: 'image_url',   image_url: { url: '<data URI>' } }
 *
 * The router passes request bodies through UNTRANSLATED, so these parts reach
 * the upstream as-is — a vision-capable model sees the image and a model
 * without vision gets its own error, which is the honest outcome rather than
 * a silent drop.
 */
export type ChatPart =
  | { type: 'text'; text: string }
  | { type: 'image_url'; image_url: { url: string } };

export type ChatMessage = { role: 'user' | 'assistant' | 'system'; content: string | ChatPart[] };

/** Flatten a turn's content to plain text, for the textarea echo and copy. */
export function messageText(m: ChatMessage): string {
  if (typeof m.content === 'string') return m.content;
  return m.content
    .map((p) => (p.type === 'text' ? p.text : `[image: ${p.image_url.url.slice(5, 40) || 'attached'}…]`))
    .join('\n');
}

/**
 * chatStream sends the same chat request with stream:true and feeds each SSE
 * content delta to onDelta as the upstream emits it. Resolves on [DONE] or
 * stream end; rejects with the usual ApiError/AuthError shape.
 */
export async function chatStream(
  model: string,
  messages: ChatMessage[],
  onDelta: (text: string) => void,
  signal?: AbortSignal,
): Promise<void> {
  const token = getToken();
  let res: Response;
  try {
    res = await fetch('/admin/chat', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        ...(token ? { Authorization: `Bearer ${token}` } : {}),
      },
      body: JSON.stringify({ model, messages, stream: true }),
      credentials: 'include',
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
  fresh_tool_output?: 'keep' | 'compress';
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
  block_round_robin: 'keep one hop for N requests, then move to the next — keeps that account’s prompt cache warm; needs N <= the hop’s request budget',
  true_round_robin: 'a dice roll per request — perfectly even in expectation and stateless, but no cache reuse and it will streak',
  sticky_last_good: 'stay on the last working hop, and release it after sticky_idle_s of quiet',
  least_used: 'not implemented yet — falls back to hop order',
};
