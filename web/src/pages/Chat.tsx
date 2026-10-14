// CHAT — a live test against the routed catalog, plus the three things that
// make it usable for real work:
//
//   1. File upload. Images become OpenAI vision parts (image_url with a data
//      URI); text and code files are inlined into the prompt inside a fenced
//      block, because the dashboard has no server-side file store and should
//      not grow one for a "paste my log" workflow.
//   2. A custom system prompt, persisted, with a default that is a real
//      default rather than a blank box.
//   3. "Set as default" on the model selector, persisted in this browser.
//
// Why the router passes these through: ezllm forwards request bodies
// UNTRANSLATED (internal/server/server.go records the invariant — "ezllm
// passes bodies through untranslated, so switching the path upstream would
// send the wrong shape"). So a vision part reaches a vision-capable upstream
// as-is, and a model WITHOUT vision returns its own error. That is the honest
// outcome; a silent drop would hide that the call is not doing what was asked.
//
// Compression interaction, stated because it is a real trap. A message whose
// content is an ARRAY is never dropped by session_dedup: compress.hashContent
// returns "" for any array that is not pure {"type":"text"} blocks, and an
// empty key makes the message undroppable by construction. caveman and rtk
// only rewrite `content` STRINGS on plain text messages, so images pass
// through untouched. The one stage that DOES change the picture is `budget`:
// it estimates a message's size from its marshalled form, and a base64 image
// is large by that measure, so a tight budget profile can legitimately drop
// OTHER messages to stay under its cap. That is the stage doing its job — but
// it is why a vision call under a tight budget can lose history, and the
// operator deserves to know it rather than discover it.
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { AnimatePresence, motion } from 'framer-motion';
import {
  accountModels, chatStream, get, messageText, type Account, type ChatMessage, type ChatPart, type ModelRow,
} from '../api';
import { Markdown } from '../markdown';
import { DUR, spring } from '../motion';
import { btn, cx, Chip, Combobox, EmptyState, inputCls, labelCls, SectionLabel, Spinner } from '../ui';

// ── persistence keys ──────────────────────────────────────────────────────
// All three are per-browser. A model default is a rendering/UX preference for
// this operator on this machine, not gateway state — making it a server
// setting would mean the dashboard and every /v1 client inherit a model
// picked while poking at a test chat.
const KEY_MODEL = 'ezllm.chat.model';
const KEY_SYSTEM = 'ezllm.chat.system';

const DEFAULT_SYSTEM_PROMPT =
  'You are a helpful assistant. Answer directly and concisely. If a task is ' +
  'ambiguous, ask one clarifying question instead of guessing.';

// The server's body cap is 32 MiB by default (EZLLM_MAX_BODY_MIB), and base64
// inflates by 4/3 — so a 6 MiB image is already 8 MiB on the wire. These
// limits are enforced BEFORE the fetch, so a too-big file is a readable
// message rather than a 413 from the router.
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

// ── attachment chip ───────────────────────────────────────────────────────
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
        : <span className="flex h-8 w-8 shrink-0 items-center justify-center rounded border border-line bg-surface font-mono text-[10px] text-mute">
            {ext}
          </span>}
      <span className="min-w-0">
        <span className="block max-w-[180px] truncate text-[12px] text-ink" title={name}>{name}</span>
        <span className="tnum block font-mono text-[10.5px] text-mute">{humanBytes(size)}</span>
      </span>
      <button type="button" onClick={onRemove} aria-label={`Remove ${name}`}
        className="-mr-1 ml-1 rounded p-1 text-mute transition-colors hover:bg-hover hover:text-bad">✕</button>
    </motion.li>
  );
}

export default function Chat() {
  const [catalog, setCatalog] = useState<ModelRow[]>([]);
  const [model, setModel] = useState('');
  const [defaultModel, setDefaultModel] = useState(() => readStored(KEY_MODEL, ''));
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [input, setInput] = useState('');
  const [busy, setBusy] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [pending, setPending] = useState<Attachment[]>([]);

  // Seeded from storage so the textarea is editable from a cold start and
  // "Reset" restores the real default rather than an empty box.
  const [system, setSystem] = useState(() => readStored(KEY_SYSTEM, DEFAULT_SYSTEM_PROMPT));
  const [systemOpen, setSystemOpen] = useState(false);
  const fileRef = useRef<HTMLInputElement>(null);
  const scrollRef = useRef<HTMLDivElement>(null);

  // ── load the catalog ──
  useEffect(() => {
    let live = true;
    (async () => {
      try {
        const rows = await get<Account[]>('/admin/accounts');
        const flat: ModelRow[] = [];
        for (const a of rows.filter((x) => x.enabled)) {
          const ms = await accountModels(a.id);
          for (const m of ms) {
            flat.push({
              ...m,
              id: `${a.namespace}/${m.id}`,
              display: `${a.namespace} / ${m.display ?? m.id}`,
            });
          }
        }
        if (!live) return;
        setCatalog(flat);
        // Preference order: the stored default (if still present in the
        // catalog), then the first model. A default whose account was since
        // disabled must not strand the selector on an empty value.
        const stored = readStored(KEY_MODEL, '');
        const first = flat[0]?.id ?? '';
        setModel(stored && flat.some((m) => m.id === stored) ? stored : first);
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
  const selected = catalog.find((m) => m.id === model);

  // Keep the transcript pinned to the bottom while it streams.
  useEffect(() => {
    const el = scrollRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [messages]);

  const setAsDefault = useCallback((id: string) => {
    setDefaultModel(id);
    writeStored(KEY_MODEL, id);
  }, []);

  // ── files ──
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
        // An image becomes a vision part; anything else is read as text and
        // inlined. A binary that is not an image would arrive as mojibake, so
        // it is refused rather than sent as garbage a model will confidently
        // interpret.
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

  const send = useCallback(async () => {
    const text = input.trim();
    if ((!text && pending.length === 0) || !model || busy) return;

    // This turn's content. Images stay as vision parts; text files fold into
    // the text part inside a fence so the model sees the filename.
    let content: string | ChatPart[];
    if (pending.length === 0) {
      content = text;
    } else {
      const parts: ChatPart[] = [];
      let textPart = text;
      for (const p of pending) {
        if (p.dataUrl) {
          parts.push({ type: 'image_url', image_url: { url: p.dataUrl } });
        } else if (p.text != null) {
          textPart += `\n\n--- ${p.name} ---\n\`\`\`\n${p.text}\n\`\`\`\n`;
        }
      }
      // No boilerplate when it is just text: a single text part is sent as a
      // plain string, which is the shape every provider expects.
      if (textPart.trim()) parts.unshift({ type: 'text', text: textPart.trim() });
      content = parts.length === 1 && parts[0].type === 'text' ? parts[0].text : parts;
    }

    // The system prompt rides along on EVERY request — that is what makes it a
    // system prompt rather than a one-off instruction. session_dedup drops the
    // later byte-identical copies server-side, so the history does not
    // accumulate duplicates of it.
    const turns: ChatMessage[] = [];
    if (system.trim()) turns.push({ role: 'system', content: system.trim() });
    turns.push(...messages, { role: 'user', content });

    setMessages([...messages, { role: 'user', content }, { role: 'assistant', content: '' }]);
    setInput('');
    setPending([]);
    setBusy(true);
    setError('');

    try {
      // Each delta lands in the assistant bubble as it arrives, so the answer
      // grows token by token instead of appearing all at once.
      await chatStream(model, turns, (delta) => {
        setMessages((prev) => {
          const last = prev[prev.length - 1];
          if (!last || last.role !== 'assistant' || typeof last.content !== 'string') return prev;
          const updated = [...prev];
          updated[updated.length - 1] = { ...last, content: last.content + delta };
          return updated;
        });
      });
    } catch (e) {
      setError(e instanceof Error ? e.message : 'chat request failed');
    } finally {
      // Drop a bubble that never received content (the upstream said nothing).
      setMessages((prev) => {
        const last = prev[prev.length - 1];
        return last?.role === 'assistant' && typeof last.content === 'string' && last.content === ''
          ? prev.slice(0, -1)
          : prev;
      });
      setBusy(false);
    }
  }, [input, pending, messages, model, busy, system]);

  const systemDirty = system !== DEFAULT_SYSTEM_PROMPT;

  return (
    <div className="mx-auto flex max-w-5xl flex-col gap-5">
      {/* ── controls ── */}
      <section className="rounded-xl border border-line bg-surface px-5 py-4">
        <div className="flex flex-wrap items-end gap-4">
          <label className="min-w-[280px] flex-1">
            <span className={labelCls}>Model</span>
            <div className="flex gap-2">
              <Combobox options={modelOptions} value={model} onChange={setModel}
              disabled={loading || busy} loadingText={loading ? 'Loading models…' : 'No synced models'} />
              {/* Set as default. Disabled when it already IS the default, so
                  the button never leaves its state ambiguous about what the
                  click would do. */}
              <button
                type="button"
                onClick={() => setAsDefault(model)}
                disabled={!model || model === defaultModel || busy}
                title={model === defaultModel
                  ? `${model} is already this browser's default`
                  : `Make ${model} the model selected on every visit`}
                className={cx(btn.base, 'shrink-0 whitespace-nowrap', btn.ghost,
                  model === defaultModel && 'opacity-60')}>
                {model === defaultModel ? '★ Default' : '☆ Set default'}
              </button>
            </div>
          </label>

          <button type="button" onClick={() => setSystemOpen((v) => !v)}
            className={cx(btn.base, btn.ghost, 'shrink-0')}
            aria-expanded={systemOpen}>
            {systemOpen ? 'Hide' : 'Edit'} system prompt
            {systemDirty && <span className="ml-1 h-1.5 w-1.5 rounded-full bg-accent" aria-label="modified" />}
          </button>
        </div>

        {/* The protocol chips (✓/✕/? anthropic|openai|responses) were removed
            here on request: they answered a question the caller does not ask
            in the chat surface — the namespace/id selector already resolves
            the hop, and the surface was going to be probed on the first call
            either way. The pin chip survives: it CHANGES behavior (the servable
            surface is fixed, no probe needed). */}
        {selected?.pin && (
          <div className="mt-3 flex flex-wrap items-center gap-1.5 border-t border-line pt-3">
            <Chip tone="accent">pinned · {selected.pin}</Chip>
            <span className="ml-auto text-[11px] text-mute">
              a pin fixes the backend surface — no protocol probe happens
            </span>
          </div>
        )}

        <AnimatePresence initial={false}>
          {systemOpen && (
            <motion.div
              initial={{ height: 0, opacity: 0 }}
              animate={{ height: 'auto', opacity: 1 }}
              exit={{ height: 0, opacity: 0 }}
              transition={{ duration: DUR.base, ease: [0.22, 1, 0.36, 1] }}
              className="overflow-hidden">
              <div className="mt-4 border-t border-line pt-4">
                <div className="flex flex-wrap items-baseline justify-between gap-3">
                  <SectionLabel>System prompt</SectionLabel>
                  <span className="text-[11px] text-mute">
                    sent on every request in this conversation · stored in this browser
                  </span>
                </div>
                <textarea
                  value={system}
                  onChange={(e) => setSystem(e.target.value)}
                  onBlur={() => writeStored(KEY_SYSTEM, system)}
                  rows={5}
                  placeholder={DEFAULT_SYSTEM_PROMPT}
                  aria-label="System prompt"
                  className={cx(inputCls, 'mt-2 w-full resize-y font-sans leading-relaxed')} />
                <div className="mt-2 flex flex-wrap items-center gap-3">
                  <button type="button"
                    onClick={() => { setSystem(DEFAULT_SYSTEM_PROMPT); writeStored(KEY_SYSTEM, DEFAULT_SYSTEM_PROMPT); }}
                    disabled={!systemDirty}
                    className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[12px] disabled:opacity-45')}>
                    Reset to default
                  </button>
                  <span className="text-[11px] text-mute">
                    {system.trim() ? `${system.trim().length} chars` : 'empty — no system message will be sent'}
                  </span>
                </div>
              </div>
            </motion.div>
          )}
        </AnimatePresence>
      </section>

      {/* ── transcript ── */}
      <section ref={scrollRef}
        className="max-h-[52vh] min-h-[280px] space-y-4 overflow-y-auto rounded-xl border border-line bg-surface/40 px-5 py-5">
        {messages.length === 0 && !loading && (
          <EmptyState
            title="Send a message to test routing, retries and provider response handling."
            hint="Attach an image to exercise a vision path, or a log file to paste a large context. Everything here goes through the router, so strategy, failover, retries and compression all apply." />
        )}
        {messages.map((m, i) => {
          const body = messageText(m);
          const images = typeof m.content === 'string'
            ? []
            : m.content.filter((p): p is Extract<ChatPart, { type: 'image_url' }> => p.type === 'image_url');
          return (
            <motion.div
              key={`${m.role}-${i}`}
              layout="position"
              initial={{ opacity: 0, y: 8 }}
              animate={{ opacity: 1, y: 0 }}
              transition={spring}
              className={cx('max-w-[88%] rounded-lg border px-4 py-3 text-sm leading-relaxed',
                m.role === 'user' ? 'ml-auto border-accent/30 bg-accent-dim' : 'border-line bg-surface')}>
              <div className="mb-1.5 flex flex-wrap items-center gap-2 text-[10px] font-semibold uppercase tracking-wide text-mute">
                {m.role}
                {m.role === 'system' && (
                  <span className="font-normal normal-case tracking-normal">sent with every request</span>
                )}
              </div>
              {m.role === 'assistant'
                ? <Markdown text={typeof m.content === 'string' ? m.content : body} />
                : (
                  <>
                    {images.length > 0 && (
                      <div className="mb-2 flex flex-wrap gap-2">
                        {images.map((p, pi) => (
                          <img key={pi} src={p.image_url.url} alt={`attachment ${pi + 1}`}
                            className="max-h-48 rounded border border-line object-contain" />
                        ))}
                      </div>
                    )}
                    <p className="whitespace-pre-wrap break-words">{body}</p>
                  </>
                )}
            </motion.div>
          );
        })}
        {busy && <div className="flex items-center gap-2 text-sm text-mute"><Spinner /> Streaming…</div>}
      </section>

      {error && (
        <div role="alert" className="rounded-xl border border-bad/30 bg-bad-dim px-4 py-3 text-[13px] text-bad">{error}</div>
      )}

      {/* ── composer ── */}
      <section className="rounded-xl border border-line bg-surface px-5 py-4">
        <AnimatePresence initial={false}>
          {pending.length > 0 && (
            <motion.ul
              initial={{ height: 0, opacity: 0 }}
              animate={{ height: 'auto', opacity: 1 }}
              exit={{ height: 0, opacity: 0 }}
              transition={{ duration: DUR.base, ease: [0.22, 1, 0.36, 1] }}
              className="mb-3 flex list-none flex-wrap gap-2 overflow-hidden p-0">
              {pending.map((p, i) => (
                <AttachmentChip key={`${p.name}-${i}`} {...p}
                  onRemove={() => setPending((cur) => cur.filter((_, j) => j !== i))} />
              ))}
            </motion.ul>
          )}
        </AnimatePresence>

        <textarea
          value={input}
          onChange={(e) => setInput(e.target.value)}
          onKeyDown={(e) => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); void send(); } }}
          placeholder={pending.length > 0 ? 'Add a note about the attachments (optional)…' : 'Message the selected model…'}
          rows={3}
          disabled={!model || busy}
          aria-label="Message"
          className={cx(inputCls, 'w-full resize-y font-sans leading-relaxed')} />

        <div className="mt-3 flex flex-wrap items-center justify-between gap-3">
          <div className="flex flex-wrap items-center gap-2">
            <input ref={fileRef} type="file" multiple className="hidden"
              accept="image/*,text/*,.json,.csv,.md,.log,.ts,.tsx,.js,.jsx,.py,.go,.rs,.java,.rb,.sh,.yml,.yaml,.toml,.sql,.html,.css,.xml,.env,.conf,.ini"
              onChange={(e) => { void addFiles(e.target.files); if (fileRef.current) fileRef.current.value = ''; }} />
            <button type="button" onClick={() => fileRef.current?.click()} disabled={busy}
              className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[12px]')}>
              ＋ Attach
            </button>
            <span className="text-[11px] text-mute">
              images → vision parts · text/code → inlined · ≤ {humanBytes(MAX_FILE_BYTES)} each
            </span>
          </div>
          <div className="flex items-center gap-3">
            <span className="hidden text-[11px] text-mute sm:inline">Enter to send · Shift+Enter for a new line</span>
            <button onClick={() => void send()}
              disabled={(!input.trim() && pending.length === 0) || !model || busy}
              className={cx(btn.base, btn.primary)}>
              {busy ? <Spinner /> : null} Send
            </button>
          </div>
        </div>
      </section>
    </div>
  );
}
