// CHAT — a playground: 1-3 side-by-side chat columns, each with its own model
// selector, one shared composer. Sending a message dispatches it to every
// included column whose model is set and streams each answer into its own column.
//
// The workspace is a fixed-height frame: the columns scroll inside themselves
// (following the stream unless the operator scrolls up) and the composer stays
// pinned, so a long answer never pushes the input off screen. Every answer
// carries how it was made (first-token and total time) and, once the ledger has
// written the row, which provider served it and the real token counts — found
// by tagging each request with an X-Ezllm-Session id.
import {
  useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type DragEvent, type ReactNode,
} from 'react';
import { AnimatePresence, motion } from 'motion/react';
import {
  accountModels, chatStream, get, normalizeFeedRow, requests, type Account, type ChatPart, type ModelRow,
} from '../utils/api';
import {
  EMPTY_PARAMS, clearSession, loadPresets, loadSession, newColumn, newId, parseParams, saveSession,
  savePresets, toJSON, toMarkdown, toMessage,
  type Column, type Params, type Preset, type Turn, type TurnMeta,
} from '../utils/chatlog';
import { fmtDuration, fmtMs, nfmt } from '../utils/lib';
import { Markdown } from '../components/markdown';
import { DUR, spring } from '../components/motion';
import {
  btn, ConfirmModal, Combobox, cx, EmptyState, inputCls, SectionLabel, Spinner, Toggle,
} from '../components/ui';

const KEY_MODEL = 'ezllm.chat.model';
const KEY_SYSTEM = 'ezllm.chat.system';
const KEY_PARAMS = 'ezllm.chat.params';

const DEFAULT_SYSTEM_PROMPT =
  'You are a helpful assistant. Answer directly and concisely. If a task is ' +
  'ambiguous, ask one clarifying question instead of guessing.';

const MAX_FILE_BYTES = 6 * 1024 * 1024;
const MAX_ATTACH_BYTES = 16 * 1024 * 1024;
const IMAGE_TYPES = /^image\//;

const readAsDataURL = (f: File) => new Promise<string>((resolve, reject) => {
  const r = new FileReader();
  r.onload = () => resolve(String(r.result));
  r.onerror = () => reject(r.error ?? new Error('read failed'));
  r.readAsDataURL(f);
});

const readAsText = (f: File) => new Promise<string>((resolve, reject) => {
  const r = new FileReader();
  r.onload = () => resolve(String(r.result));
  r.onerror = () => reject(r.error ?? new Error('read failed'));
  r.readAsText(f);
});

const humanBytes = (n: number) =>
  n >= 1024 * 1024 ? `${(n / 1048576).toFixed(1)} MiB` : `${Math.max(1, Math.round(n / 1024))} KiB`;

function readStored(key: string, fallback: string): string {
  try { return localStorage.getItem(key) ?? fallback; } catch { return fallback; }
}
function writeStored(key: string, value: string): void {
  try { localStorage.setItem(key, value); } catch { /* private mode: the session still works */ }
}
function readParams(): Params {
  try { return { ...EMPTY_PARAMS, ...(JSON.parse(readStored(KEY_PARAMS, '{}')) as Partial<Params>) }; } catch { return { ...EMPTY_PARAMS }; }
}

type Attachment = { name: string; size: number; dataUrl?: string; text?: string };

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const turnPlainText = (t: Turn) =>
  (typeof t.content === 'string' ? t.content : t.content.filter((p): p is Extract<ChatPart, { type: 'text' }> => p.type === 'text').map((p) => p.text).join('\n'));

function download(name: string, mime: string, text: string) {
  const url = URL.createObjectURL(new Blob([text], { type: mime }));
  const a = document.createElement('a');
  a.href = url; a.download = name;
  document.body.appendChild(a); a.click(); a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

function AttachmentChip({ name, size, preview, onRemove }: Attachment & { preview?: string; onRemove: () => void }) {
  const ext = (name.split('.').pop() ?? '?').slice(0, 4).toUpperCase();
  return (
    <motion.li
      layout
      initial={{ opacity: 0, scale: 0.96 }}
      animate={{ opacity: 1, scale: 1 }}
      exit={{ opacity: 0, scale: 0.96 }}
      transition={spring}
      className="flex items-center gap-2.5 rounded-lg border border-line bg-raised px-2.5 py-1.5">
      {preview
        ? <img src={preview} alt="" className="h-8 w-8 shrink-0 rounded border border-line object-cover" />
        : <span className="flex h-8 w-8 shrink-0 items-center justify-center rounded border border-line bg-surface font-mono text-[11px] text-mute">
            {ext}
          </span>}
      <span className="min-w-0">
        <span className="block max-w-[180px] truncate text-[13px] text-ink" title={name}>{name}</span>
        <span className="tnum block font-mono text-[11px] text-mute">{humanBytes(size)}</span>
      </span>
      <button type="button" onClick={onRemove} aria-label={`Remove ${name}`}
        className="-mr-1 ml-1 rounded p-1 text-mute transition-colors hover:bg-hover hover:text-bad">✕</button>
    </motion.li>
  );
}

// ── a column's scroll area ────────────────────────────────────────────────
// Follows the stream while the reader is at the bottom; the moment they scroll
// up it lets go, and a chip offers the way back. `follow` changes whenever
// content is added.
function Scroller({ follow, label, children }: { follow: string; label: string; children: ReactNode }) {
  const ref = useRef<HTMLDivElement>(null);
  const stuck = useRef(true);
  const [away, setAway] = useState(false);
  // Our own scrolls to the bottom raise scroll events that land after the
  // content has grown again, so "not near the bottom" alone cannot mean the
  // reader left. Leaving is a scroll UP; content growth never moves the
  // position up. At the bottom always means following.
  const lastTop = useRef(0);
  const measure = () => {
    const el = ref.current;
    if (!el) return;
    const near = el.scrollHeight - el.scrollTop - el.clientHeight < 48;
    if (near) { stuck.current = true; setAway(false); }
    else if (el.scrollTop < lastTop.current - 1) { stuck.current = false; setAway(true); }
    lastTop.current = el.scrollTop;
  };
  // Content can change height without any scroll event (a spinner row leaving
  // shrinks it to meet the reader), so every change re-measures: follow if the
  // reader was at the bottom, and keep the "jump" chip honest either way.
  useLayoutEffect(() => {
    const el = ref.current;
    if (el && stuck.current) el.scrollTop = el.scrollHeight;
    measure();
  }, [follow]);
  // Some growth never re-renders React (a code block laying out, a late
  // paint): watch the content's own size so following does not depend on it.
  const inner = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const node = inner.current;
    if (!node || typeof ResizeObserver === 'undefined') return;
    const ro = new ResizeObserver(() => {
      const el = ref.current;
      if (el && stuck.current) el.scrollTop = el.scrollHeight;
      measure();
    });
    ro.observe(node);
    return () => ro.disconnect();
  }, []);
  return (
    <div className="relative min-h-0 flex-1">
      <div ref={ref} onScroll={measure} role="log" aria-label={label} aria-live="off"
        className="h-full overflow-y-auto px-3 py-3">
        <div ref={inner} className="space-y-3">{children}</div>
      </div>
      {away && (
        <button type="button" onClick={() => { const el = ref.current; if (el) el.scrollTo({ top: el.scrollHeight, behavior: 'smooth' }); }}
          className="absolute bottom-3 right-4 rounded-full border border-line-strong bg-surface px-3 py-1 text-[12px] text-ink shadow-lg shadow-black/40 hover:bg-hover">
          ↓ Jump to latest
        </button>
      )}
    </div>
  );
}

// ── how an answer was made ────────────────────────────────────────────────
// First-token and total time are measured here, in the browser; the served-by
// line and the token counts come from the ledger row once it lands (a moment
// after the stream ends). Speed is an estimate when only characters are known.
function AnswerMeta({ meta }: { meta: TurnMeta }) {
  const s = meta.served;
  const outTokens = s?.tout || Math.round(meta.chars / 4);
  const genMs = meta.totalMs - (meta.ttftMs ?? 0);
  const speed = genMs >= 300 && outTokens > 0 ? Math.round(outTokens / (genMs / 1000)) : null;
  return (
    <div className="mt-2 space-y-0.5 border-t border-line/60 pt-2 text-[11px] text-mute">
      <p className="tnum flex flex-wrap gap-x-3">
        {meta.ttftMs != null && <span title="time to first token, measured in this browser">first token {fmtMs(Math.round(meta.ttftMs))}</span>}
        <span title="from send to the last token">total {fmtDuration(Math.round(meta.totalMs))}</span>
        {speed != null && <span title={s?.tout ? 'output tokens per second after the first token' : 'estimated from characters (about 4 per token)'}>{s?.tout ? '' : '~'}{nfmt(speed)} tok/s</span>}
      </p>
      {s ? (
        <p className="tnum flex flex-wrap gap-x-3">
          <span title="the provider that served this answer">via <span className="text-dim">{s.account}</span>{s.alias && s.alias !== `${s.account}/${s.model}` ? <> · <span className="text-dim">{s.alias}</span></> : null}</span>
          <span className="font-mono text-dim">{s.model}</span>
          <span title="tokens in (not from cache) and out, from the ledger">↑{nfmt(s.tin)} ↓{nfmt(s.tout)} tokens</span>
        </p>
      ) : s === undefined ? <p className="text-mute/70">looking up the provider…</p> : null}
    </div>
  );
}

// ── one turn ──────────────────────────────────────────────────────────────
function TurnView({
  turn, canAct, isLastAnswer, isLastUser, editing, onEditStart, onEditChange, onEditCancel, onEditSend,
  onRegenerate, onDelete,
}: {
  turn: Turn; canAct: boolean; isLastAnswer: boolean; isLastUser: boolean;
  editing: string | null;
  onEditStart: () => void; onEditChange: (v: string) => void; onEditCancel: () => void; onEditSend: () => void;
  onRegenerate: () => void; onDelete: () => void;
}) {
  const text = turnPlainText(turn);
  const images = typeof turn.content === 'string'
    ? []
    : turn.content.filter((p): p is Extract<ChatPart, { type: 'image_url' }> => p.type === 'image_url');
  const editable = turn.role === 'user' && isLastUser && typeof turn.content === 'string';
  const act = 'rounded px-1.5 py-0.5 text-[11px] text-mute transition-colors hover:bg-hover hover:text-ink disabled:opacity-40';
  return (
    <motion.div layout="position" initial={{ opacity: 0, y: 6 }} animate={{ opacity: 1, y: 0 }} transition={spring}
      className={cx('group rounded-lg border px-3 py-2.5 text-[13px] leading-relaxed',
        turn.role === 'user' ? 'border-accent/30 bg-accent-dim' : 'border-line bg-surface')}>
      <div className="mb-1 flex items-center gap-2 text-[11px]">
        <span className="font-semibold text-mute">{turn.role === 'user' ? 'You' : 'Assistant'}</span>
        {turn.incomplete && (
          <span className="text-bad">
            · {turn.incomplete === 'stopped' ? 'stopped' : turn.incomplete === 'interrupted' ? 'interrupted by leaving the page' : 'incomplete'}, not sent as history
          </span>
        )}
        {/* Actions appear on hover or focus; on touch there is no hover, so they stay. */}
        <span className="ml-auto flex items-center gap-0.5 opacity-0 transition-opacity focus-within:opacity-100 group-hover:opacity-100 [@media(hover:none)]:opacity-100">
          {text && <CopyInline text={text} className={act} />}
          {turn.role === 'assistant' && isLastAnswer && <button type="button" className={act} disabled={!canAct} onClick={onRegenerate}>Regenerate</button>}
          {editable && editing == null && <button type="button" className={act} disabled={!canAct} onClick={onEditStart}>Edit</button>}
          <button type="button" className={act} disabled={!canAct} onClick={onDelete}
            aria-label={turn.role === 'user' ? 'Delete this message and its answer' : 'Delete this answer'}>Delete</button>
        </span>
      </div>
      {turn.role === 'assistant'
        ? (turn.content === '' ? null : <Markdown text={typeof turn.content === 'string' ? turn.content : text} />)
        : editing != null ? (
          <div className="space-y-2">
            <textarea value={editing} onChange={(e) => onEditChange(e.target.value)} rows={3} aria-label="Edit message"
              onKeyDown={(e) => { if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); onEditSend(); } if (e.key === 'Escape') { e.stopPropagation(); onEditCancel(); } }}
              className={cx(inputCls, 'w-full resize-y font-sans leading-relaxed')} />
            <div className="flex items-center gap-2">
              <button type="button" className={cx(btn.base, btn.primary, '!h-7 !px-3 !text-[12px]')} disabled={!editing.trim()} onClick={onEditSend}>Resend</button>
              <button type="button" className={cx(btn.base, btn.ghost, '!h-7 !px-3 !text-[12px]')} onClick={onEditCancel}>Cancel</button>
              <span className="text-[11px] text-mute">replaces this message and everything after it in this column</span>
            </div>
          </div>
        ) : (
          <>
            {images.length > 0 && (
              <div className="mb-2 flex flex-wrap gap-2">
                {images.map((p, pi) => <img key={pi} src={p.image_url.url} alt={`attachment ${pi + 1}`} className="max-h-36 rounded border border-line object-contain" />)}
              </div>
            )}
            <p className="whitespace-pre-wrap break-words">{text}</p>
          </>
        )}
      {turn.role === 'assistant' && turn.meta && <AnswerMeta meta={turn.meta} />}
    </motion.div>
  );
}

// A copy action sized for a turn's action row (the shared CopyButton is a pill).
function CopyInline({ text, className }: { text: string; className: string }) {
  const [done, setDone] = useState(false);
  return (
    <button type="button" className={className} onClick={() => {
      void navigator.clipboard.writeText(text).then(() => { setDone(true); setTimeout(() => setDone(false), 1500); }).catch(() => undefined);
    }}>{done ? 'Copied' : 'Copy'}</button>
  );
}

// ── parameter fields ──────────────────────────────────────────────────────
const PARAM_FIELDS: { key: keyof Params; label: string; placeholder: string; hint: string }[] = [
  { key: 'temperature', label: 'Temperature', placeholder: 'default', hint: '0 to 2' },
  { key: 'top_p', label: 'Top P', placeholder: 'default', hint: '0 to 1' },
  { key: 'max_tokens', label: 'Max tokens', placeholder: 'default', hint: 'whole number' },
];

function ParamFields({ value, onChange, inherit, errors, idPrefix }: {
  value: Params; onChange: (p: Params) => void; inherit?: Params; errors: Partial<Record<keyof Params, string>>; idPrefix: string;
}) {
  return (
    <div className="grid gap-3 sm:grid-cols-3">
      {PARAM_FIELDS.map((f) => (
        <div key={f.key}>
          <label className="mb-1 block text-[12px] text-mute" htmlFor={`${idPrefix}-${f.key}`}>{f.label} <span className="text-mute/70">· {f.hint}</span></label>
          <input id={`${idPrefix}-${f.key}`} inputMode="decimal" value={value[f.key]}
            onChange={(e) => onChange({ ...value, [f.key]: e.target.value })}
            placeholder={inherit && inherit[f.key].trim() ? `${inherit[f.key]} (shared)` : f.placeholder}
            className={cx(inputCls, 'font-mono tnum', errors[f.key] && 'border-bad')} />
          {errors[f.key] && <p className="mt-1 text-[12px] text-bad">{f.label} must be {errors[f.key]}.</p>}
        </div>
      ))}
    </div>
  );
}

export default function Chat() {
  const [catalog, setCatalog] = useState<ModelRow[]>([]);
  const [defaultModel, setDefaultModel] = useState(() => readStored(KEY_MODEL, ''));
  const [columns, setColumns] = useState<Column[]>(() => loadSession() ?? [newColumn()]);
  const [input, setInput] = useState('');
  const [loading, setLoading] = useState(true);
  const [catalogError, setCatalogError] = useState('');
  const [catalogAttempt, setCatalogAttempt] = useState(0);
  const [error, setError] = useState('');
  const [pending, setPending] = useState<Attachment[]>([]);
  const [system, setSystem] = useState(() => readStored(KEY_SYSTEM, DEFAULT_SYSTEM_PROMPT));
  const [systemOpen, setSystemOpen] = useState(false);
  const [params, setParams] = useState<Params>(readParams);
  const [paramsOpen, setParamsOpen] = useState(false);
  const [colParamsOpen, setColParamsOpen] = useState<Set<string>>(new Set());
  const [presets, setPresets] = useState<Preset[]>(loadPresets);
  const [presetName, setPresetName] = useState('');
  const [confirmClear, setConfirmClear] = useState<'all' | string | null>(null);
  const [editing, setEditing] = useState<{ colId: string; turnId: string; text: string } | null>(null);
  const [dragging, setDragging] = useState(false);
  const fileRef = useRef<HTMLInputElement>(null);
  const inputRef = useRef<HTMLTextAreaElement>(null);
  const requests_ = useRef(new Map<string, AbortController>());
  const leaving = useRef(false);
  const sent = useRef<string[]>([]);

  useEffect(() => {
    leaving.current = false;
    return () => {
      leaving.current = true;
      for (const controller of requests_.current.values()) controller.abort();
      requests_.current.clear();
    };
  }, []);

  // The conversation survives a page change: saved (debounced; a stream would
  // otherwise write on every token) and restored on the next visit.
  useEffect(() => {
    const t = setTimeout(() => saveSession(columns), 400);
    return () => { clearTimeout(t); };
  }, [columns]);
  // …and flushed on leave, so the last tokens are kept.
  const latest = useRef(columns);
  latest.current = columns;
  useEffect(() => () => saveSession(latest.current), []);

  useEffect(() => {
    let live = true;
    setLoading(true); setCatalogError('');
    (async () => {
      try {
        const accounts = (await get<Account[]>('/admin/accounts')).filter(a => a.enabled);
        const results: Array<ModelRow[] | null> = new Array(accounts.length).fill(null);
        let next = 0;
        // Bound concurrent catalog reads, retain account order, and keep the
        // models from healthy accounts when another account fails.
        const worker = async () => {
          while (live && next < accounts.length) {
            const index = next++;
            try { results[index] = await accountModels(accounts[index].id); }
            catch { results[index] = null; }
          }
        };
        await Promise.all(Array.from({ length: Math.min(4, accounts.length) }, worker));
        if (!live) return;
        const flat = results.flatMap((models, index) => (models ?? []).map(m => ({
          ...m, id: `${accounts[index].namespace}/${m.id}`,
          display: `${accounts[index].namespace} / ${m.display ?? m.id}`,
        })));
        const failed = accounts.filter((_, index) => results[index] === null);
        if (failed.length) setCatalogError(`Could not load models for ${failed.map(a => a.name).join(', ')}.${failed.length < accounts.length ? ' Other providers remain available.' : ''}`);
        setCatalog(flat);
        const stored = readStored(KEY_MODEL, '');
        const preferred = stored && flat.some(m => m.id === stored) ? stored : flat[0]?.id ?? '';
        setColumns(prev => prev.map(c => (!c.model ? { ...c, model: preferred } : c)));
      } catch (e) {
        if (live) setCatalogError(e instanceof Error ? e.message : 'could not load provider models');
      } finally {
        if (live) setLoading(false);
      }
    })();
    return () => { live = false; };
  }, [catalogAttempt]);

  const modelOptions = useMemo(
    () => catalog.map((m) => ({ value: m.id, label: m.display ?? m.id })),
    [catalog],
  );

  const patchColumn = useCallback((id: string, fn: (c: Column) => Column) => {
    setColumns((prev) => prev.map((c) => (c.id === id ? fn(c) : c)));
  }, []);
  const patchTurn = useCallback((colId: string, turnId: string, fn: (t: Turn) => Turn) => {
    patchColumn(colId, (c) => ({ ...c, messages: c.messages.map((m) => (m.id === turnId ? fn(m) : m)) }));
  }, [patchColumn]);

  const addColumn = useCallback(() => {
    setColumns((prev) => (prev.length >= 3 ? prev : [...prev, newColumn(prev[0]?.model || defaultModel || '')]));
  }, [defaultModel]);

  const removeColumn = useCallback((id: string) => {
    requests_.current.get(id)?.abort();
    requests_.current.delete(id);
    setColumns((prev) => (prev.length <= 1 ? prev : prev.filter((c) => c.id !== id)));
  }, []);

  const setAsDefault = useCallback((id: string) => {
    setDefaultModel(id);
    writeStored(KEY_MODEL, id);
  }, []);

  const addFiles = useCallback(async (files: FileList | File[] | null) => {
    const list = files ? Array.from(files) : [];
    if (list.length === 0) return;
    setError('');
    const next: Attachment[] = [];
    let total = pending.reduce((a, p) => a + p.size, 0);
    for (const f of list) {
      if (f.size > MAX_FILE_BYTES) {
        setError(`${f.name} is ${humanBytes(f.size)} — the per-file cap is ${humanBytes(MAX_FILE_BYTES)}.`);
        continue;
      }
      if (total + f.size > MAX_ATTACH_BYTES) {
        setError(`${f.name} would push the payload past ${humanBytes(MAX_ATTACH_BYTES)}. Send the rest separately.`);
        continue;
      }
      try {
        next.push(IMAGE_TYPES.test(f.type)
          ? { name: f.name, size: f.size, dataUrl: await readAsDataURL(f) }
          : { name: f.name, size: f.size, text: await readAsText(f) });
        total += f.size;
      } catch {
        setError(`could not read ${f.name}`);
      }
    }
    if (next.length) setPending((p) => [...p, ...next]);
  }, [pending]);

  const buildContent = (text: string): string | ChatPart[] => {
    if (pending.length === 0) return text;
    const parts: ChatPart[] = [];
    let textPart = text;
    for (const p of pending) {
      if (p.dataUrl) parts.push({ type: 'image_url', image_url: { url: p.dataUrl } });
      else if (p.text != null) textPart += `\n\n--- ${p.name} ---\n\`\`\`\n${p.text}\n\`\`\`\n`;
    }
    if (textPart.trim()) parts.unshift({ type: 'text', text: textPart.trim() });
    return parts.length === 1 && parts[0].type === 'text' ? parts[0].text : parts;
  };

  // The ledger row for a request lands a moment after its stream ends (rows are
  // written asynchronously), so look for it a few times before giving up.
  const lookupServed = useCallback(async (colId: string, turnId: string, session: string) => {
    for (let i = 0; i < 8; i++) {
      await sleep(i === 0 ? 600 : 800);
      if (leaving.current) return;
      try {
        const r = await requests({ session, range: '24h', limit: 1 });
        if (r.rows[0]) {
          const f = normalizeFeedRow(r.rows[0]);
          patchTurn(colId, turnId, (t) => ({
            ...t, meta: {
              ...(t.meta as TurnMeta),
              served: { account: f.account, alias: f.alias, model: f.model, tin: f.tin, tout: f.tout, status: f.status, ttftMs: f.ttft_ms, totalMs: f.total_ms },
            },
          }));
          return;
        }
      } catch { /* try again */ }
    }
    if (!leaving.current) patchTurn(colId, turnId, (t) => ({ ...t, meta: { ...(t.meta as TurnMeta), served: null } }));
  }, [patchTurn]);

  // Send one user turn to the given columns. `base` replaces a column's history
  // (regenerate and edit-and-resend cut it back first); without it the column's
  // current messages are the history.
  const dispatch = useCallback(async (targets: Column[], content: string | ChatPart[], base?: Map<string, Turn[]>) => {
    const prepared = targets.map((target) => ({
      target,
      history: base?.get(target.id) ?? target.messages,
      user: { id: newId(), role: 'user', content } as Turn,
      assistant: { id: newId(), role: 'assistant', content: '' } as Turn,
    }));
    setColumns((prev) => prev.map((c) => {
      const p = prepared.find((x) => x.target.id === c.id);
      return p ? { ...c, busy: true, error: '', messages: [...p.history, p.user, p.assistant] } : c;
    }));

    await Promise.all(prepared.map(async ({ target, history, user, assistant }) => {
      const controller = new AbortController();
      requests_.current.set(target.id, controller);
      const turns = [
        ...(system.trim() ? [{ role: 'system' as const, content: system.trim() }] : []),
        ...history.filter((m) => !m.incomplete).map(toMessage),
        toMessage(user),
      ];
      const { values } = parseParams(params, target.params);
      const session = `chat-${newId()}`;
      const started = performance.now();
      let firstToken: number | undefined;
      let chars = 0;
      let outcome: 'ok' | 'stopped' | 'error' = 'ok';
      let message = '';
      try {
        await chatStream(target.model, turns, (delta) => {
          if (controller.signal.aborted) return;
          if (firstToken === undefined) firstToken = performance.now() - started;
          chars += delta.length;
          patchTurn(target.id, assistant.id, (t) => (typeof t.content === 'string' ? { ...t, content: t.content + delta } : t));
        }, controller.signal, { params: values, session });
      } catch (e) {
        if (leaving.current) return;
        if (controller.signal.aborted) outcome = 'stopped';
        else { outcome = 'error'; message = e instanceof Error ? e.message : 'chat request failed'; }
      } finally {
        if (requests_.current.get(target.id) === controller) requests_.current.delete(target.id);
      }
      if (leaving.current) return;
      const total = performance.now() - started;
      let kept = false;
      setColumns((prev) => prev.map((c) => {
        if (c.id !== target.id) return c;
        const messages = c.messages.flatMap((m) => {
          if (m.id !== assistant.id) return [m];
          if (m.content === '') return []; // nothing arrived: drop the placeholder
          kept = true;
          return [{ ...m, incomplete: outcome === 'ok' ? undefined : outcome, meta: { ttftMs: firstToken, totalMs: total, chars } } as Turn];
        });
        return { ...c, busy: false, error: message, messages };
      }));
      // The request is in the ledger whether it finished or was cut short.
      if (kept || chars > 0) void lookupServed(target.id, assistant.id, session);
    }));
  }, [system, params, patchTurn, lookupServed]);

  // Everything a send would need to be valid, per included column.
  const paramProblems = useMemo(() => columns.filter((c) => c.include).some((c) => Object.keys(parseParams(params, c.params).errors).length > 0), [columns, params]);
  const sendTargets = columns.filter((c) => c.include && c.model && !c.busy);
  const sendable = sendTargets.length > 0 && (input.trim().length > 0 || pending.length > 0) && !paramProblems;
  const busy = columns.some((c) => c.busy);

  const send = useCallback(async () => {
    const text = input.trim();
    if (!sendable) return;
    const content = buildContent(text);
    if (text) sent.current = [text, ...sent.current.filter((s) => s !== text)].slice(0, 30);
    setInput('');
    setPending([]);
    setError('');
    await dispatch(sendTargets, content);
  }, [input, pending, sendable, sendTargets, dispatch]);

  const stop = useCallback((id: string) => { requests_.current.get(id)?.abort(); }, []);
  const stopAll = useCallback(() => { for (const c of requests_.current.values()) c.abort(); }, []);

  // Escape stops every running answer (unless something else, a dialog or an
  // open edit box, is the one being escaped).
  useEffect(() => {
    if (!busy) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !e.defaultPrevented && !document.querySelector('[role=dialog]')) stopAll();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [busy, stopAll]);

  const regenerate = useCallback((col: Column) => {
    const idx = col.messages.map((m) => m.role).lastIndexOf('user');
    if (idx < 0) return;
    const user = col.messages[idx];
    void dispatch([col], user.content, new Map([[col.id, col.messages.slice(0, idx)]]));
  }, [dispatch]);

  const deleteTurn = useCallback((col: Column, turn: Turn) => {
    patchColumn(col.id, (c) => {
      const i = c.messages.findIndex((m) => m.id === turn.id);
      if (i < 0) return c;
      // A message and its answer go together; an answer can go alone.
      const drop = turn.role === 'user' && c.messages[i + 1]?.role === 'assistant' ? 2 : 1;
      return { ...c, messages: [...c.messages.slice(0, i), ...c.messages.slice(i + drop)] };
    });
  }, [patchColumn]);

  const resendEdit = useCallback(() => {
    if (!editing || !editing.text.trim()) return;
    const col = columns.find((c) => c.id === editing.colId);
    if (!col) return;
    const i = col.messages.findIndex((m) => m.id === editing.turnId);
    if (i < 0) return;
    const text = editing.text.trim();
    setEditing(null);
    void dispatch([col], text, new Map([[col.id, col.messages.slice(0, i)]]));
  }, [editing, columns, dispatch]);

  const clearColumn = (id: string) => patchColumn(id, (c) => ({ ...c, messages: [], error: '' }));
  const doClear = () => {
    const target = confirmClear;
    setConfirmClear(null);
    if (target === 'all') { stopAll(); setColumns((prev) => prev.map((c) => ({ ...c, messages: [], error: '', busy: false }))); clearSession(); }
    else if (target) { stop(target); clearColumn(target); }
  };

  // Composer niceties: it grows with its text, Up recalls the last message, and
  // images can be pasted or dropped in.
  useLayoutEffect(() => {
    const el = inputRef.current;
    if (!el) return;
    el.style.height = 'auto';
    el.style.height = `${Math.min(el.scrollHeight, 200)}px`;
  }, [input]);
  const onDrop = (e: DragEvent) => { e.preventDefault(); setDragging(false); void addFiles(e.dataTransfer.files); };

  const exportCols = () => columns.map((c, i) => ({ label: `Column ${i + 1} — ${c.model || 'no model'}`, model: c.model, messages: c.messages }));
  const stamp = () => new Date().toISOString().slice(0, 16).replace(/[:T]/g, '-');
  const hasMessages = columns.some((c) => c.messages.length > 0);

  const systemDirty = system !== DEFAULT_SYSTEM_PROMPT;
  const paramsSet = PARAM_FIELDS.some((f) => params[f.key].trim() !== '');
  const sharedErrors = parseParams(params, EMPTY_PARAMS).errors;
  const matchingPreset = presets.find((p) => p.text === system)?.name ?? '';
  const savePreset = () => {
    const name = presetName.trim();
    if (!name || !system.trim()) return;
    const next = [...presets.filter((p) => p.name !== name), { name, text: system.trim() }].sort((a, b) => a.name.localeCompare(b.name));
    setPresets(next); savePresets(next); setPresetName('');
  };
  const headBtn = cx(btn.base, btn.ghost, '!h-8 !px-3 !text-[13px] disabled:opacity-45');

  return (
    // A fixed-height workspace from lg up (the composer stays pinned and the
    // columns scroll inside themselves); on a phone the page flows and each
    // column gets its own bounded height, so open panels never squeeze them out.
    <div className="mx-auto flex max-w-7xl flex-col gap-3 lg:h-[calc(100dvh-8.25rem)] lg:min-h-[520px]">
      <div className="flex shrink-0 flex-wrap items-center justify-between gap-2 rounded-xl border border-line bg-surface px-3 py-2">
        <div className="flex flex-wrap items-center gap-2">
          <button type="button" onClick={() => setSystemOpen((v) => !v)} className={headBtn} aria-expanded={systemOpen}>
            System prompt
            {systemDirty && <span className="ml-1 h-1.5 w-1.5 rounded-full bg-accent" aria-label="modified" />}
          </button>
          <button type="button" onClick={() => setParamsOpen((v) => !v)} className={headBtn} aria-expanded={paramsOpen}>
            Parameters
            {paramsSet && <span className="ml-1 h-1.5 w-1.5 rounded-full bg-accent" aria-label="set" />}
          </button>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <details className="relative">
            <summary className={cx(headBtn, 'cursor-pointer list-none', !hasMessages && 'pointer-events-none opacity-45')}>Export ▾</summary>
            <div className="absolute right-0 z-20 mt-1 w-56 rounded-lg border border-line-strong bg-surface p-1 shadow-xl shadow-black/50">
              {[
                { label: 'Copy as Markdown', run: () => void navigator.clipboard.writeText(toMarkdown(system, exportCols())).catch(() => undefined) },
                { label: 'Download Markdown', run: () => download(`chat-${stamp()}.md`, 'text/markdown', toMarkdown(system, exportCols())) },
                { label: 'Download JSON', run: () => download(`chat-${stamp()}.json`, 'application/json', toJSON(system, exportCols())) },
              ].map((o) => (
                <button key={o.label} type="button" className="block w-full rounded px-3 py-1.5 text-left text-[13px] text-dim hover:bg-hover hover:text-ink"
                  onClick={(e) => { o.run(); (e.currentTarget.closest('details') as HTMLDetailsElement | null)?.removeAttribute('open'); }}>
                  {o.label}
                </button>
              ))}
            </div>
          </details>
          <button type="button" onClick={() => setConfirmClear('all')} disabled={!hasMessages} className={headBtn}>Clear all</button>
          <button type="button" onClick={addColumn} disabled={columns.length >= 3} className={headBtn}
            title={columns.length >= 3 ? 'Maximum 3 columns' : 'Add a chat column'}>＋ Add column</button>
        </div>
      </div>

      <AnimatePresence initial={false}>
        {systemOpen && (
          <motion.div key="system"
            initial={{ height: 0, opacity: 0 }} animate={{ height: 'auto', opacity: 1 }} exit={{ height: 0, opacity: 0 }}
            transition={{ duration: DUR.base, ease: [0.22, 1, 0.36, 1] }}
            className="shrink-0 overflow-hidden rounded-xl border border-line bg-surface px-4 py-3">
            <div className="flex flex-wrap items-baseline justify-between gap-3">
              <SectionLabel>System prompt</SectionLabel>
              <span className="text-[12px] text-mute">sent on every request in every column · stored in this browser</span>
            </div>
            <div className="mt-2 flex flex-wrap items-center gap-2">
              <select aria-label="Saved prompts" value={matchingPreset} className={cx(inputCls, '!w-56')}
                onChange={(e) => {
                  const p = presets.find((x) => x.name === e.target.value);
                  if (p) { setSystem(p.text); writeStored(KEY_SYSTEM, p.text); }
                }}>
                <option value="">Saved prompts…</option>
                {presets.map((p) => <option key={p.name} value={p.name}>{p.name}</option>)}
              </select>
              {matchingPreset && (
                <button type="button" className="text-[12px] text-mute underline underline-offset-2 hover:text-bad"
                  onClick={() => { const next = presets.filter((p) => p.name !== matchingPreset); setPresets(next); savePresets(next); }}>
                  Delete “{matchingPreset}”
                </button>
              )}
              <input value={presetName} onChange={(e) => setPresetName(e.target.value)} placeholder="Name to save as…" aria-label="Preset name"
                onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); savePreset(); } }}
                className={cx(inputCls, '!w-48')} />
              <button type="button" className={cx(btn.base, btn.ghost, '!h-8 !px-3 !text-[13px]')} disabled={!presetName.trim() || !system.trim()} onClick={savePreset}>
                Save prompt
              </button>
            </div>
            <textarea value={system} onChange={(e) => setSystem(e.target.value)} onBlur={() => writeStored(KEY_SYSTEM, system)}
              rows={4} placeholder={DEFAULT_SYSTEM_PROMPT} aria-label="System prompt"
              className={cx(inputCls, 'mt-2 w-full resize-y font-sans leading-relaxed')} />
            <div className="mt-2 flex flex-wrap items-center gap-3">
              <button type="button" onClick={() => { setSystem(DEFAULT_SYSTEM_PROMPT); writeStored(KEY_SYSTEM, DEFAULT_SYSTEM_PROMPT); }}
                disabled={!systemDirty} className={cx(btn.base, btn.ghost, '!h-8 !px-3 !text-[13px] disabled:opacity-45')}>
                Reset to default
              </button>
              <span className="text-[12px] text-mute">{system.trim() ? `${nfmt(system.trim().length)} chars` : 'empty — no system message will be sent'}</span>
            </div>
          </motion.div>
        )}
        {paramsOpen && (
          <motion.div key="params"
            initial={{ height: 0, opacity: 0 }} animate={{ height: 'auto', opacity: 1 }} exit={{ height: 0, opacity: 0 }}
            transition={{ duration: DUR.base, ease: [0.22, 1, 0.36, 1] }}
            className="shrink-0 overflow-hidden rounded-xl border border-line bg-surface px-4 py-3">
            <div className="flex flex-wrap items-baseline justify-between gap-3">
              <SectionLabel>Request parameters</SectionLabel>
              <span className="text-[12px] text-mute">shared by every column; a column can override them · empty = the provider&rsquo;s default</span>
            </div>
            <div className="mt-2">
              <ParamFields idPrefix="sp" value={params} errors={sharedErrors}
                onChange={(p) => { setParams(p); writeStored(KEY_PARAMS, JSON.stringify(p)); }} />
            </div>
            <div className="mt-2">
              <button type="button" disabled={!paramsSet} className={cx(btn.base, btn.ghost, '!h-8 !px-3 !text-[13px] disabled:opacity-45')}
                onClick={() => { setParams({ ...EMPTY_PARAMS }); writeStored(KEY_PARAMS, '{}'); }}>Reset parameters</button>
            </div>
          </motion.div>
        )}
      </AnimatePresence>

      {catalogError && <div role="alert" className="shrink-0 rounded-xl border border-warn/30 bg-warn-dim px-4 py-3 text-[13px] text-ink">{catalogError} <button type="button" className="underline" onClick={() => setCatalogAttempt(n => n + 1)}>Retry models</button></div>}
      {error && <div role="alert" className="shrink-0 rounded-xl border border-bad/30 bg-bad-dim px-4 py-3 text-[13px] text-bad">{error}</div>}

      <div className={cx('grid grid-cols-1 gap-3 lg:min-h-0 lg:flex-1', columns.length > 1 && 'lg:grid-cols-2', columns.length > 2 && 'xl:grid-cols-3')}>
        {columns.map((c, idx) => {
          const colErrors = parseParams(params, c.params).errors;
          const ownSet = PARAM_FIELDS.some((f) => c.params[f.key].trim() !== '');
          const lastAnswer = [...c.messages].reverse().find((m) => m.role === 'assistant');
          const lastUser = [...c.messages].reverse().find((m) => m.role === 'user');
          const tail = c.messages[c.messages.length - 1];
          // Anything that makes the conversation taller must change this key, or the
          // scroll-follow would let the reader drift off the bottom (the stats line
          // and the served-by line arrive after the text).
          const follow = `${c.messages.length}:${typeof tail?.content === 'string' ? tail.content.length : 0}:${tail?.meta ? 1 : 0}:${tail?.meta?.served === undefined ? 0 : 1}:${c.error ? 1 : 0}:${c.busy ? 1 : 0}`;
          return (
            <section key={c.id} aria-label={`Column ${idx + 1}`} className={cx('flex h-[70dvh] min-h-[22rem] flex-col overflow-hidden rounded-xl border bg-surface lg:h-auto lg:min-h-0', c.include ? 'border-line' : 'border-line/60')}>
              <div className="flex items-center gap-2 border-b border-line px-3 py-2">
                <span className="flex h-6 w-6 shrink-0 items-center justify-center rounded-md border border-line bg-raised font-mono text-[11px] text-mute">
                  {idx + 1}
                </span>
                <div className="min-w-0 flex-1">
                  <Combobox label={`Model for column ${idx + 1}`} idPrefix={`col-${c.id}`} options={modelOptions} value={c.model}
                    onChange={(v) => patchColumn(c.id, (x) => ({ ...x, model: v }))}
                    disabled={loading || c.busy} loadingText={loading ? 'Loading models…' : 'No synced models'} />
                </div>
                {columns.length > 1 && (
                  <label className="flex shrink-0 items-center gap-1.5 text-[12px] text-mute" title="Off: sends skip this column and leave its conversation untouched">
                    <Toggle checked={c.include} onChange={(v) => patchColumn(c.id, (x) => ({ ...x, include: v }))} label={`Include column ${idx + 1} in sends`} />
                    send
                  </label>
                )}
                {idx > 0 && (
                  <button type="button" onClick={() => removeColumn(c.id)} aria-label="Remove column"
                    className="rounded p-1 text-mute transition-colors hover:bg-hover hover:text-bad">✕</button>
                )}
              </div>
              <div className="flex flex-wrap items-center gap-x-3 gap-y-1 border-b border-line px-3 py-1.5 text-[12px] text-mute">
                <button type="button" onClick={() => setAsDefault(c.model)} disabled={!c.model}
                  className="hover:text-ink disabled:opacity-40" title="Use this model for the first column on every visit">
                  {c.model && c.model === defaultModel ? '★ default' : '☆ set default'}
                </button>
                <button type="button" aria-expanded={colParamsOpen.has(c.id)}
                  onClick={() => setColParamsOpen((cur) => { const n = new Set(cur); if (n.has(c.id)) n.delete(c.id); else n.add(c.id); return n; })}
                  className="hover:text-ink">
                  Parameters{ownSet && <span className="ml-1 text-accent">·</span>}
                </button>
                <span className="tnum ml-auto">{nfmt(c.messages.length)} message{c.messages.length === 1 ? '' : 's'}</span>
                <button type="button" onClick={() => setConfirmClear(c.id)} disabled={c.messages.length === 0} className="hover:text-bad disabled:opacity-40">Clear</button>
              </div>
              {colParamsOpen.has(c.id) && (
                <div className="border-b border-line bg-raised/40 px-3 py-2.5">
                  <ParamFields idPrefix={`cp-${c.id}`} value={c.params} inherit={params} errors={colErrors}
                    onChange={(p) => patchColumn(c.id, (x) => ({ ...x, params: p }))} />
                </div>
              )}
              <Scroller follow={follow} label={`Conversation ${idx + 1}`}>
                {c.messages.length === 0 && !c.busy && (
                  <EmptyState title="No messages yet." hint="Pick a model above, then send from the composer below." />
                )}
                {c.messages.map((m) => (
                  <TurnView key={m.id} turn={m} canAct={!c.busy}
                    isLastAnswer={m.id === lastAnswer?.id} isLastUser={m.id === lastUser?.id}
                    editing={editing?.turnId === m.id ? editing.text : null}
                    onEditStart={() => setEditing({ colId: c.id, turnId: m.id, text: turnPlainText(m) })}
                    onEditChange={(text) => setEditing((e) => (e ? { ...e, text } : e))}
                    onEditCancel={() => setEditing(null)} onEditSend={resendEdit}
                    onRegenerate={() => regenerate(c)} onDelete={() => deleteTurn(c, m)} />
                ))}
                {c.busy && (
                  <div className="flex items-center gap-2 text-[13px] text-mute">
                    <Spinner /> Streaming…
                    <button type="button" onClick={() => stop(c.id)} className="rounded border border-line px-2 py-0.5 text-[12px] text-dim hover:bg-hover hover:text-ink">Stop</button>
                  </div>
                )}
                {c.error && <p role="alert" className="rounded-lg border border-bad/30 bg-bad-dim px-3 py-2 text-[12px] text-bad">{c.error}</p>}
              </Scroller>
            </section>
          );
        })}
      </div>

      <section
        onDragOver={(e) => { e.preventDefault(); setDragging(true); }} onDragLeave={() => setDragging(false)} onDrop={onDrop}
        className={cx('shrink-0 rounded-xl border bg-surface px-4 py-3 shadow-[0_1px_0_rgba(255,255,255,0.03)] transition-colors', dragging ? 'border-accent' : 'border-line')}>
        <AnimatePresence initial={false}>
          {pending.length > 0 && (
            <motion.ul initial={{ height: 0, opacity: 0 }} animate={{ height: 'auto', opacity: 1 }} exit={{ height: 0, opacity: 0 }}
              transition={{ duration: DUR.base, ease: [0.22, 1, 0.36, 1] }}
              className="mb-2 flex list-none flex-wrap gap-2 overflow-hidden p-0">
              {pending.map((p, i) => (
                <AttachmentChip key={`${p.name}-${i}`} {...p} onRemove={() => setPending((cur) => cur.filter((_, j) => j !== i))} />
              ))}
            </motion.ul>
          )}
        </AnimatePresence>
        <textarea ref={inputRef} value={input} onChange={(e) => setInput(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) { e.preventDefault(); void send(); }
            else if (e.key === 'ArrowUp' && input === '' && sent.current[0]) { e.preventDefault(); setInput(sent.current[0]); }
          }}
          onPaste={(e) => {
            const files = Array.from(e.clipboardData.files);
            if (files.length > 0) { e.preventDefault(); void addFiles(files); }
          }}
          placeholder={pending.length > 0 ? 'Add a note about the attachments (optional)…' : 'Message every selected model…  (paste or drop images and files)'}
          rows={2} aria-label="Message"
          className={cx(inputCls, 'w-full resize-none font-sans leading-relaxed')} />
        {paramProblems && <p role="alert" className="mt-1 text-[12px] text-bad">A request parameter is out of range — fix it under Parameters before sending.</p>}
        <div className="mt-2 flex flex-wrap items-center justify-between gap-3">
          <div className="flex flex-wrap items-center gap-2">
            <input ref={fileRef} type="file" multiple className="hidden"
              accept="image/*,text/*,.json,.csv,.md,.log,.ts,.tsx,.js,.jsx,.py,.go,.rs,.java,.rb,.sh,.yml,.yaml,.toml,.sql,.html,.css,.xml,.env,.conf,.ini"
              onChange={(e) => { void addFiles(e.target.files); if (fileRef.current) fileRef.current.value = ''; }} />
            <button type="button" onClick={() => fileRef.current?.click()}
              className={cx(btn.base, btn.ghost, '!h-8 !px-3 !text-[13px]')}>＋ Attach</button>
            <span className="text-[12px] text-mute">images → vision parts · text/code → inlined · ≤ {humanBytes(MAX_FILE_BYTES)} each</span>
          </div>
          <div className="flex items-center gap-3">
            <span className="hidden text-[12px] text-mute md:inline">Enter to send · Shift+Enter for a new line · ↑ recalls · Esc stops</span>
            {busy && <button type="button" onClick={stopAll} className={cx(btn.base, btn.ghost)}>Stop</button>}
            <button type="button" onClick={() => void send()} disabled={!sendable}
              className={cx(btn.base, btn.primary, 'disabled:cursor-not-allowed disabled:opacity-45')}>
              Send to {sendTargets.length} model{sendTargets.length === 1 ? '' : 's'}
            </button>
          </div>
        </div>
      </section>

      <ConfirmModal open={confirmClear != null}
        title={confirmClear === 'all' ? 'Clear every conversation?' : 'Clear this conversation?'}
        confirmLabel="Clear" body="The messages are removed from this page. This cannot be undone."
        onClose={() => setConfirmClear(null)} onConfirm={doClear} />
    </div>
  );
}
