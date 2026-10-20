// CHAT — a playground: 1-3 side-by-side chat columns, each with its own model
// selector, one shared composer. Sending a message dispatches it to every
// column whose model is set and streams each answer into its own column.
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { AnimatePresence, motion } from 'motion/react';
import {
  accountModels, chatStream, get, messageText, type Account, type ChatMessage, type ChatPart, type ModelRow,
} from '../utils/api';
import { Markdown } from '../components/markdown';
import { DUR, spring } from '../components/motion';
import { btn, cx, Chip, Combobox, EmptyState, inputCls, SectionLabel, Spinner } from '../components/ui';

const KEY_MODEL = 'ezllm.chat.model';
const KEY_SYSTEM = 'ezllm.chat.system';

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

type Attachment = { name: string; size: number; dataUrl?: string; text?: string };
type Column = { id: string; model: string; messages: ChatMessage[]; busy: boolean; error: string };

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

let nextColumn = 1;
const newColumn = (model = ''): Column => ({ id: `c${nextColumn++}`, model, messages: [], busy: false, error: '' });

export default function Chat() {
  const [catalog, setCatalog] = useState<ModelRow[]>([]);
  const [defaultModel, setDefaultModel] = useState(() => readStored(KEY_MODEL, ''));
  const [columns, setColumns] = useState<Column[]>(() => [newColumn()]);
  const [input, setInput] = useState('');
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [pending, setPending] = useState<Attachment[]>([]);
  const [system, setSystem] = useState(() => readStored(KEY_SYSTEM, DEFAULT_SYSTEM_PROMPT));
  const [systemOpen, setSystemOpen] = useState(false);
  const fileRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    let live = true;
    (async () => {
      try {
        const rows = await get<Account[]>('/admin/accounts');
        const flat: ModelRow[] = [];
        for (const a of rows.filter((x) => x.enabled)) {
          const ms = await accountModels(a.id);
          for (const m of ms) {
            flat.push({ ...m, id: `${a.namespace}/${m.id}`, display: `${a.namespace} / ${m.display ?? m.id}` });
          }
        }
        if (!live) return;
        setCatalog(flat);
        const stored = readStored(KEY_MODEL, '');
        const preferred = stored && flat.some((m) => m.id === stored) ? stored : flat[0]?.id ?? '';
        setColumns((prev) => prev.map((c) => (!c.model ? { ...c, model: preferred } : c)));
      } catch (e) {
        if (live) setError(e instanceof Error ? e.message : 'could not load provider models');
      } finally {
        if (live) setLoading(false);
      }
    })();
    return () => { live = false; };
  }, []);

  const modelOptions = useMemo(
    () => catalog.map((m) => ({ value: m.id, label: m.display ?? m.id })),
    [catalog],
  );

  const addColumn = useCallback(() => {
    setColumns((prev) => (prev.length >= 3 ? prev : [...prev, newColumn(prev[0]?.model || defaultModel || '')]));
  }, [defaultModel]);

  const removeColumn = useCallback((id: string) => {
    setColumns((prev) => (prev.length <= 1 ? prev : prev.filter((c) => c.id !== id)));
  }, []);

  const setColumnModel = useCallback((id: string, model: string) => {
    setColumns((prev) => prev.map((c) => (c.id === id ? { ...c, model } : c)));
  }, []);

  const setAsDefault = useCallback((id: string) => {
    setDefaultModel(id);
    writeStored(KEY_MODEL, id);
  }, []);

  const addFiles = useCallback(async (files: FileList | null) => {
    if (!files || files.length === 0) return;
    setError('');
    const next: Attachment[] = [];
    let total = pending.reduce((a, p) => a + p.size, 0);
    for (const f of Array.from(files)) {
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

  const send = useCallback(async () => {
    const text = input.trim();
    const targets = columns.filter((c) => c.model && !c.busy);
    if ((!text && pending.length === 0) || targets.length === 0) return;

    const content = buildContent(text);
    setInput('');
    setPending([]);
    setError('');

    // Snapshot each target's history BEFORE the optimistic user turn lands.
    const history = new Map(targets.map((c) => [c.id, c.messages]));

    setColumns((prev) => prev.map((c) => (targets.some((t) => t.id === c.id)
      ? { ...c, busy: true, error: '', messages: [...c.messages, { role: 'user', content } as ChatMessage, { role: 'assistant', content: '' }] }
      : c)));

    await Promise.all(targets.map(async (target) => {
      const turns: ChatMessage[] = [];
      if (system.trim()) turns.push({ role: 'system', content: system.trim() });
      turns.push(...(history.get(target.id) ?? []), { role: 'user', content });
      try {
        await chatStream(target.model, turns, (delta) => {
          setColumns((prev) => prev.map((c) => {
            if (c.id !== target.id) return c;
            const msgs = [...c.messages];
            const last = msgs[msgs.length - 1];
            if (last?.role === 'assistant' && typeof last.content === 'string') {
              msgs[msgs.length - 1] = { ...last, content: last.content + delta };
            }
            return { ...c, messages: msgs };
          }));
        });
      } catch (e) {
        const msg = e instanceof Error ? e.message : 'chat request failed';
        setColumns((prev) => prev.map((c) => (c.id === target.id ? { ...c, error: msg } : c)));
      } finally {
        setColumns((prev) => prev.map((c) => {
          if (c.id !== target.id) return c;
          const msgs = c.messages.filter((m, i) => !(i === c.messages.length - 1 && m.role === 'assistant' && m.content === ''));
          return { ...c, busy: false, messages: msgs };
        }));
      }
    }));
  }, [input, pending, columns, system]);

  const busy = columns.some((c) => c.busy);
  const systemDirty = system !== DEFAULT_SYSTEM_PROMPT;
  const sendable = columns.some((c) => c.model && !c.busy) && (input.trim().length > 0 || pending.length > 0);

  return (
    <div className="mx-auto flex max-w-7xl flex-col gap-4">
      <div className="flex flex-wrap items-center justify-end gap-2 rounded-xl border border-line bg-surface px-4 py-2.5">
        <button type="button" onClick={() => setSystemOpen((v) => !v)}
          className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[13px]')} aria-expanded={systemOpen}>
          {systemOpen ? 'Hide' : 'Edit'} system prompt
          {systemDirty && <span className="ml-1 h-1.5 w-1.5 rounded-full bg-accent" aria-label="modified" />}
        </button>
        <button type="button" onClick={addColumn} disabled={columns.length >= 3}
          className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[13px] disabled:opacity-45')}
          title={columns.length >= 3 ? 'Maximum 3 columns' : 'Add a chat column'}>
          ＋ Add column
        </button>
      </div>

      <AnimatePresence initial={false}>
        {systemOpen && (
          <motion.div
            initial={{ height: 0, opacity: 0 }}
            animate={{ height: 'auto', opacity: 1 }}
            exit={{ height: 0, opacity: 0 }}
            transition={{ duration: DUR.base, ease: [0.22, 1, 0.36, 1] }}
            className="overflow-hidden rounded-xl border border-line bg-surface px-4 py-3">
            <div className="flex flex-wrap items-baseline justify-between gap-3">
              <SectionLabel>System prompt</SectionLabel>
              <span className="text-[12px] text-mute">sent on every request in every column · stored in this browser</span>
            </div>
            <textarea value={system} onChange={(e) => setSystem(e.target.value)} onBlur={() => writeStored(KEY_SYSTEM, system)}
              rows={4} placeholder={DEFAULT_SYSTEM_PROMPT} aria-label="System prompt"
              className={cx(inputCls, 'mt-2 w-full resize-y font-sans leading-relaxed')} />
            <div className="mt-2 flex flex-wrap items-center gap-3">
              <button type="button" onClick={() => { setSystem(DEFAULT_SYSTEM_PROMPT); writeStored(KEY_SYSTEM, DEFAULT_SYSTEM_PROMPT); }}
                disabled={!systemDirty} className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[13px] disabled:opacity-45')}>
                Reset to default
              </button>
              <span className="text-[12px] text-mute">{system.trim() ? `${system.trim().length} chars` : 'empty — no system message will be sent'}</span>
            </div>
          </motion.div>
        )}
      </AnimatePresence>

      {error && <div role="alert" className="rounded-xl border border-bad/30 bg-bad-dim px-4 py-3 text-[13px] text-bad">{error}</div>}

      <div className="grid gap-4" style={{ gridTemplateColumns: `repeat(${columns.length}, minmax(0, 1fr))` }}>
        {columns.map((c, idx) => {
          const selected = catalog.find((m) => m.id === c.model);
          return (
            <section key={c.id} className="flex min-h-[520px] flex-col overflow-hidden rounded-xl border border-line bg-surface">
              <div className="flex items-center gap-2 border-b border-line px-3 py-2.5">
                <span className="flex h-6 w-6 shrink-0 items-center justify-center rounded-md border border-line bg-raised font-mono text-[11px] text-mute">
                  {idx + 1}
                </span>
                <div className="min-w-0 flex-1">
                  <Combobox idPrefix={`col-${c.id}`} options={modelOptions} value={c.model} onChange={(v) => setColumnModel(c.id, v)}
                    disabled={loading || c.busy} loadingText={loading ? 'Loading models…' : 'No synced models'} />
                </div>
                {idx > 0 && (
                  <button type="button" onClick={() => removeColumn(c.id)} aria-label="Remove column"
                    className="rounded p-1 text-mute transition-colors hover:bg-hover hover:text-bad">✕</button>
                )}
              </div>
              {selected && (
                <div className="flex items-center gap-1.5 border-b border-line px-3 py-1.5">
                  <Chip tone="neutral">{selected.id}</Chip>
                  <button type="button" onClick={() => setAsDefault(c.model)}
                    className="ml-auto text-[11px] text-mute hover:text-ink"
                    title="Use this model for the first column on every visit">
                    {c.model === defaultModel ? '★ default' : '☆ set default'}
                  </button>
                </div>
              )}
              <div className="flex-1 space-y-3 overflow-y-auto px-3 py-3">
                {c.messages.length === 0 && !c.busy && (
                  <EmptyState title="No messages yet." hint="Pick a model above, then send from the composer below." />
                )}
                {c.messages.map((m, i) => {
                  const body = messageText(m);
                  const images = typeof m.content === 'string'
                    ? []
                    : m.content.filter((p): p is Extract<ChatPart, { type: 'image_url' }> => p.type === 'image_url');
                  return (
                    <motion.div key={`${c.id}-${i}`} layout="position" initial={{ opacity: 0, y: 6 }} animate={{ opacity: 1, y: 0 }} transition={spring}
                      className={cx('rounded-lg border px-3 py-2.5 text-[13px] leading-relaxed',
                        m.role === 'user' ? 'border-accent/30 bg-accent-dim' : 'border-line bg-surface')}>
                      <div className="mb-1 text-[9.5px] font-semibold text-mute">{m.role}</div>
                      {m.role === 'assistant'
                        ? <Markdown text={typeof m.content === 'string' ? m.content : body} />
                        : (
                          <>
                            {images.length > 0 && (
                              <div className="mb-2 flex flex-wrap gap-2">
                                {images.map((p, pi) => <img key={pi} src={p.image_url.url} alt={`attachment ${pi + 1}`} className="max-h-36 rounded border border-line object-contain" />)}
                              </div>
                            )}
                            <p className="whitespace-pre-wrap break-words">{body}</p>
                          </>
                        )}
                    </motion.div>
                  );
                })}
                {c.busy && <div className="flex items-center gap-2 text-[13px] text-mute"><Spinner /> Streaming…</div>}
                {c.error && <p className="rounded-lg border border-bad/30 bg-bad-dim px-3 py-2 text-[12px] text-bad">{c.error}</p>}
              </div>
            </section>
          );
        })}
      </div>

      <section className="rounded-xl border border-line bg-surface px-4 py-3 shadow-[0_1px_0_rgba(255,255,255,0.03)]">
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
        <textarea value={input} onChange={(e) => setInput(e.target.value)}
          onKeyDown={(e) => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); void send(); } }}
          placeholder={pending.length > 0 ? 'Add a note about the attachments (optional)…' : 'Message every selected model…'}
          rows={2} disabled={busy} aria-label="Message"
          className={cx(inputCls, 'w-full resize-y font-sans leading-relaxed')} />
        <div className="mt-2 flex flex-wrap items-center justify-between gap-3">
          <div className="flex flex-wrap items-center gap-2">
            <input ref={fileRef} type="file" multiple className="hidden"
              accept="image/*,text/*,.json,.csv,.md,.log,.ts,.tsx,.js,.jsx,.py,.go,.rs,.java,.rb,.sh,.yml,.yaml,.toml,.sql,.html,.css,.xml,.env,.conf,.ini"
              onChange={(e) => { void addFiles(e.target.files); if (fileRef.current) fileRef.current.value = ''; }} />
            <button type="button" onClick={() => fileRef.current?.click()} disabled={busy}
              className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[13px]')}>＋ Attach</button>
            <span className="text-[12px] text-mute">images → vision parts · text/code → inlined · ≤ {humanBytes(MAX_FILE_BYTES)} each</span>
          </div>
          <div className="flex items-center gap-3">
            <span className="hidden text-[12px] text-mute sm:inline">Enter to send · Shift+Enter for a new line</span>
            <button onClick={() => void send()} disabled={!sendable}
              className={cx(btn.base, btn.primary, 'disabled:cursor-not-allowed disabled:opacity-45')}>
              {busy ? <Spinner /> : null} Send to {columns.filter((c) => c.model).length || 0}
            </button>
          </div>
        </div>
      </section>
    </div>
  );
}
