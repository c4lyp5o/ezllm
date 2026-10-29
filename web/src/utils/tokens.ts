// Client tokens — the Connect page's backend. Types + calls over the shared
// apiFetch (bearer from localStorage, OpenAI-shaped error parsing, 401 bounce).
//
// The plaintext appears in the CREATE response only. The server stores an
// irreversible hash, so there is no way back to it afterwards — the UI must show
// it once and never again (admin.go:handleTokens).
import { del, get, patch, post, type UsageRow } from './api';

// Mirrors store.TokenSummary (admin.go). NOTE: roles is an ARRAY — writing a
// single string here renders a 404 blank chip, not an error.
export interface TokenInfo {
  id: number;
  name: string;
  hint: string;
  roles: string[];
  enabled: boolean;
  cap_window: string;
  cap_tokens: number;
  allowed_models: string;
  expires_at: string;
  last_used_at?: string | null;
  created_at: string;
}

export interface TokenCreated extends TokenInfo {
  plaintext: string;
}

export type TokenPatch = {
  name?: string; enabled?: boolean; cap_tokens?: number; cap_window?: string;
  allowed_models?: string; expires_at?: string;
};

export interface V1Model {
  id: string;
  object?: string;
  owned_by?: string;
  protocol?: { chat?: boolean | null };
}

// GET /admin/tokens writes the ARRAY directly (not {tokens:[...]}), and POST
// answers 201 {token, plaintext} — both verified against admin.go:handleTokens.
export const listTokens = () => get<TokenInfo[]>('/admin/tokens');

export const createToken = async (
  name: string,
  role: 'admin' | 'infer' = 'infer',
  extra: { allowed_models?: string; expires_at?: string } = {},
): Promise<TokenCreated> => {
  const body = await post<{ token: TokenInfo; plaintext: string }>('/admin/tokens', {
    name,
    roles: [role],
    ...extra,
  });
  return { ...body.token, plaintext: body.plaintext };
};

export const patchToken = (id: number, p: TokenPatch) => patch<TokenInfo>(`/admin/tokens/${id}`, p);

export const revokeToken = (id: number) => del(`/admin/tokens/${id}`);

// Per-key consumption. group_by=client already exists server-side
// (ledger.go:usageGroupColumn), so this is a read, not new backend work.
export const usageByClient = (days = 30) =>
  get<{ groups: UsageRow[] }>(
    `/admin/usage?group_by=client&from=${encodeURIComponent(isoDaysAgo(days))}`,
  ).then((r) => r.groups ?? []);

// Live check for the Connect page: the routable ids the gateway can serve right
// now, read through the dashboard SESSION. /v1/models is the same list for a
// bearer client, but the dashboard holds a cookie and never a key — this route
// is that same handler behind the admin gate, so "these routes work" is
// evidence again instead of a label.
export async function routableModels(): Promise<V1Model[]> {
  const res = await fetch('/admin/routable-models', { cache: 'no-store' });
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  const body = await res.json();
  return (body?.data ?? []) as V1Model[];
}

function isoDaysAgo(days: number): string {
  const d = new Date(Date.now() - days * 86_400_000);
  return d.toISOString();
}

/** Proves a freshly minted key works: asks /v1/models with ONLY that key (the
 *  dashboard cookie is not sent) and reports what it can see. */
export type KeyTest = { ok: boolean; status: number; models: number; message: string };
export async function testKey(plaintext: string): Promise<KeyTest> {
  try {
    const res = await fetch('/v1/models', { headers: { Authorization: `Bearer ${plaintext}` }, credentials: 'omit', cache: 'no-store' });
    if (!res.ok) return { ok: false, status: res.status, models: 0, message: res.status === 401 || res.status === 403 ? 'The gateway rejected this key.' : `The gateway answered ${res.status}.` };
    const body = await res.json().catch(() => ({}));
    const n = Array.isArray(body?.data) ? body.data.length : 0;
    return { ok: true, status: res.status, models: n, message: `The key works and can see ${n.toLocaleString('en-US')} model${n === 1 ? '' : 's'}.` };
  } catch {
    return { ok: false, status: 0, models: 0, message: 'Could not reach the gateway.' };
  }
}
