// Chat page state that lives outside the component: the turn/column shapes,
// session persistence, request parameters, saved system prompts and export.
//
// Persistence is sessionStorage on purpose: a conversation survives moving to
// another page and back (the page used to lose everything on navigation), but
// is gone when the tab closes — it can hold prompts the operator would not want
// lying around. Images are never stored (a 6 MiB data URL would blow the
// quota); an attachment-bearing turn keeps its text and notes what was dropped.
import { messageText, type ChatMessage, type ChatPart } from './api';

/** What the ledger knows about a finished request, found via its session id. */
export type Served = {
  account: string; alias?: string; model: string;
  tin: number; tout: number; status: number; ttftMs?: number; totalMs?: number;
};
export type TurnMeta = {
  ttftMs?: number; totalMs: number; chars: number;
  /** undefined = still looking, null = the ledger had no row, else the row. */
  served?: Served | null;
};
export type Turn = {
  id: string; role: 'user' | 'assistant'; content: string | ChatPart[];
  /** An answer that did not finish: never sent back as history. */
  incomplete?: 'error' | 'stopped' | 'interrupted';
  meta?: TurnMeta;
};

export type Params = { temperature: string; max_tokens: string; top_p: string };
export const EMPTY_PARAMS: Params = { temperature: '', max_tokens: '', top_p: '' };

export type Column = {
  id: string; model: string; messages: Turn[]; busy: boolean; error: string;
  /** Receives the shared composer's sends. Off = keep this column's history untouched. */
  include: boolean;
  /** Per-column overrides of the shared parameters ('' = inherit). */
  params: Params;
};

let seq = 0;
export const newId = () => `${Date.now().toString(36)}${(seq++).toString(36)}${Math.random().toString(36).slice(2, 5)}`;
export const newColumn = (model = ''): Column => ({
  id: newId(), model, messages: [], busy: false, error: '', include: true, params: { ...EMPTY_PARAMS },
});

/** A chat message as the API takes it (the turn's bookkeeping dropped). */
export const toMessage = (t: Turn): ChatMessage => ({ role: t.role, content: t.content });

// ── parameters ────────────────────────────────────────────────────────────
export type ParamValues = { temperature?: number; max_tokens?: number; top_p?: number };
export type ParamErrors = Partial<Record<keyof Params, string>>;

/** The effective request parameters for a column: its own value where set,
 *  else the shared one, else nothing (the provider's default). Invalid input
 *  is reported, never silently sent. */
export function parseParams(shared: Params, own: Params): { values: ParamValues; errors: ParamErrors } {
  const values: ParamValues = {};
  const errors: ParamErrors = {};
  const pick = (k: keyof Params) => (own[k].trim() !== '' ? own[k] : shared[k]).trim();
  const num = (k: keyof Params, lo: number, hi: number, msg: string) => {
    const raw = pick(k);
    if (raw === '') return undefined;
    const n = Number(raw);
    if (!Number.isFinite(n) || n < lo || n > hi) { errors[k] = msg; return undefined; }
    return n;
  };
  const t = num('temperature', 0, 2, 'between 0 and 2');
  const p = num('top_p', 0, 1, 'between 0 and 1');
  const raw = pick('max_tokens');
  if (raw !== '') {
    const n = Number(raw);
    if (!Number.isInteger(n) || n < 1) errors.max_tokens = 'a whole number, 1 or more';
    else values.max_tokens = n;
  }
  if (t !== undefined) values.temperature = t;
  if (p !== undefined) values.top_p = p;
  return { values, errors };
}

// ── session persistence ───────────────────────────────────────────────────
const SESSION_KEY = 'ezllm.chat.session.v1';

type StoredTurn = { id: string; role: 'user' | 'assistant'; content: string; incomplete?: Turn['incomplete']; meta?: TurnMeta };
type StoredColumn = { id: string; model: string; include: boolean; params: Params; busy: boolean; messages: StoredTurn[] };

const textOf = (c: string | ChatPart[]): string => {
  if (typeof c === 'string') return c;
  const images = c.filter((p) => p.type === 'image_url').length;
  const text = c.filter((p): p is Extract<ChatPart, { type: 'text' }> => p.type === 'text').map((p) => p.text).join('\n');
  return images > 0 ? `${text}${text ? '\n\n' : ''}[${images} image${images === 1 ? '' : 's'} not kept after leaving the page]` : text;
};

export function saveSession(columns: Column[]): void {
  try {
    const out: StoredColumn[] = columns.map((c) => ({
      id: c.id, model: c.model, include: c.include, params: c.params, busy: c.busy,
      messages: c.messages.map((m) => ({ id: m.id, role: m.role, content: textOf(m.content), incomplete: m.incomplete, meta: m.meta })),
    }));
    sessionStorage.setItem(SESSION_KEY, JSON.stringify(out));
  } catch { /* storage full or blocked: the conversation just will not survive a page change */ }
}

export function loadSession(): Column[] | null {
  try {
    const raw = sessionStorage.getItem(SESSION_KEY);
    if (!raw) return null;
    const stored = JSON.parse(raw) as StoredColumn[];
    if (!Array.isArray(stored) || stored.length === 0) return null;
    return stored.slice(0, 3).map((s) => {
      const messages: Turn[] = s.messages.map((m) => ({ ...m }));
      // A stream that was running when the page was left never finished: keep
      // what arrived, marked, or drop the empty placeholder.
      if (s.busy) {
        const last = messages[messages.length - 1];
        if (last?.role === 'assistant') {
          if (last.content === '') messages.pop(); else last.incomplete = 'interrupted';
        }
      }
      return { id: s.id, model: s.model, include: s.include !== false, params: { ...EMPTY_PARAMS, ...s.params }, messages, busy: false, error: '' };
    });
  } catch { return null; }
}

export function clearSession(): void {
  try { sessionStorage.removeItem(SESSION_KEY); } catch { /* nothing to clear */ }
}

// ── saved system prompts ──────────────────────────────────────────────────
export type Preset = { name: string; text: string };
const PRESETS_KEY = 'ezllm.chat.presets';

export function loadPresets(): Preset[] {
  try {
    const v = JSON.parse(localStorage.getItem(PRESETS_KEY) ?? '[]') as unknown;
    return Array.isArray(v) ? v.filter((p): p is Preset => typeof p?.name === 'string' && typeof p?.text === 'string') : [];
  } catch { return []; }
}
export function savePresets(list: Preset[]): void {
  try { localStorage.setItem(PRESETS_KEY, JSON.stringify(list)); } catch { /* private mode */ }
}

// ── export ────────────────────────────────────────────────────────────────
export type ExportColumn = { label: string; model: string; messages: Turn[] };

const turnText = (t: Turn) => messageText(toMessage(t));

/** Side-by-side conversations as one Markdown document: the system prompt once,
 *  then each column under its own heading. */
export function toMarkdown(system: string, cols: ExportColumn[]): string {
  const out: string[] = ['# Chat export', ''];
  if (system.trim()) out.push('## System prompt', '', system.trim(), '');
  for (const c of cols) {
    out.push(`## ${c.label}`, '');
    for (const t of c.messages) {
      const note = t.incomplete ? ` _(incomplete: ${t.incomplete})_` : '';
      out.push(`**${t.role === 'user' ? 'You' : 'Assistant'}**${note}`, '', turnText(t), '');
      if (t.role === 'assistant' && t.meta?.served) {
        const s = t.meta.served;
        out.push(`> served by ${s.account}${s.alias ? ` · ${s.alias}` : ''} · ${s.model} · ${s.tin} tokens in, ${s.tout} out`, '');
      }
    }
  }
  return out.join('\n');
}

export function toJSON(system: string, cols: ExportColumn[]): string {
  return JSON.stringify({
    system: system.trim() || null,
    columns: cols.map((c) => ({
      label: c.label, model: c.model,
      messages: c.messages.map((t) => ({
        role: t.role, content: turnText(t), ...(t.incomplete ? { incomplete: t.incomplete } : {}),
        ...(t.meta ? { meta: t.meta } : {}),
      })),
    })),
  }, null, 2);
}
