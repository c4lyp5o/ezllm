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
  last_used_at?: string | null;
  created_at: string;
}

export interface TokenCreated extends TokenInfo {
  plaintext: string;
}

export type TokenPatch = { name?: string; enabled?: boolean; cap_tokens?: number };

export interface V1Model {
  id: string;
  object?: string;
  owned_by?: string;
}

// GET /admin/tokens writes the ARRAY directly (not {tokens:[...]}), and POST
// answers 201 {token, plaintext} — both verified against admin.go:handleTokens.
export const listTokens = () => get<TokenInfo[]>('/admin/tokens');

export const createToken = async (
  name: string,
  role: 'admin' | 'infer' = 'infer',
): Promise<TokenCreated> => {
  const body = await post<{ token: TokenInfo; plaintext: string }>('/admin/tokens', {
    name,
    roles: [role],
  });
  return { ...body.token, plaintext: body.plaintext };
};

export const patchToken = (id: number, p: TokenPatch) => patch(`/admin/tokens/${id}`, p);

export const revokeToken = (id: number) => del(`/admin/tokens/${id}`);

// Per-key consumption. group_by=client already exists server-side
// (ledger.go:usageGroupColumn), so this is a read, not new backend work.
export const usageByClient = (days = 30) =>
  get<{ groups: UsageRow[] }>(
    `/admin/usage?group_by=client&from=${encodeURIComponent(isoDaysAgo(days))}`,
  ).then((r) => r.groups ?? []);

// Live check for the Connect page: asks the real /v1/models route with the key
// being advertised, so "these routes work" is evidence rather than a claim.
// Same-origin, so the admin token is a valid credential here too.
export async function probeModels(key: string): Promise<V1Model[]> {
  const res = await fetch('/v1/models', { headers: { Authorization: `Bearer ${key}` } });
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  const body = await res.json();
  return (body?.data ?? []) as V1Model[];
}

function isoDaysAgo(days: number): string {
  const d = new Date();
  d.setUTCDate(d.getUTCDate() - days);
  return d.toISOString();
}
