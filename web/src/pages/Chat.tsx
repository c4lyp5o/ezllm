import { useEffect, useMemo, useState } from 'react';
import { accountModels, chatStream, get, type Account, type ModelRow } from '../api';
import { Markdown } from '../markdown';
import { Spinner, cx, inputCls, btn } from '../ui';

type Message = { role: 'user' | 'assistant'; content: string };

type ChatAccount = Account & { models?: ModelRow[] };

export default function Chat() {
  const [accounts, setAccounts] = useState<ChatAccount[]>([]);
  const [model, setModel] = useState('');
  const [messages, setMessages] = useState<Message[]>([]);
  const [input, setInput] = useState('');
  const [busy, setBusy] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');

  useEffect(() => {
    let live = true;
    (async () => {
      try {
        const rows = await get<Account[]>('/admin/accounts');
        const hydrated = await Promise.all(rows.filter(a => a.enabled).map(async a => ({
          ...a,
          models: await accountModels(a.id),
        })));
        if (live) {
          setAccounts(hydrated);
          const first = hydrated.flatMap(a => (a.models ?? []).map(m => `${a.namespace}/${m.id}`))[0] ?? '';
          setModel(first);
        }
      } catch (e) {
        if (live) setError(e instanceof Error ? e.message : 'could not load provider models');
      } finally {
        if (live) setLoading(false);
      }
    })();
    return () => { live = false; };
  }, []);

  const models = useMemo(() => accounts.flatMap(a => (a.models ?? []).map(m => ({
    value: `${a.namespace}/${m.id}`,
    label: `${a.namespace} / ${m.id}`,
  }))), [accounts]);

  const send = async () => {
    const text = input.trim();
    if (!text || !model || busy) return;
    const next = [...messages, { role: 'user' as const, content: text }];
    setMessages([...next, { role: 'assistant', content: '' }]);
    setInput('');
    setBusy(true);
    setError('');
    try {
      // Stream the reply: each delta lands in the assistant bubble as it
      // arrives, so the answer grows token by token instead of appearing at once.
      await chatStream(model, next, (delta) => {
        setMessages((prev) => {
          const last = prev[prev.length - 1];
          if (!last || last.role !== 'assistant') return prev;
          const updated = [...prev];
          updated[updated.length - 1] = { ...last, content: last.content + delta };
          return updated;
        });
      });
      // Drop a bubble that never received content (upstream said nothing).
      setMessages((prev) => (prev[prev.length - 1]?.content === '' ? prev.slice(0, -1) : prev));
    } catch (e) {
      setError(e instanceof Error ? e.message : 'chat request failed');
      setMessages((prev) => {
        const last = prev[prev.length - 1];
        return last?.role === 'assistant' && last.content === '' ? prev.slice(0, -1) : prev;
      });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="mx-auto max-w-4xl space-y-5">
      <section className="border-b border-line pb-5">
        <div className="flex flex-wrap items-end justify-between gap-4">
          <div>
            <p className="text-[13px] text-mute">A small live test against the configured provider catalog.</p>
          </div>
          <label className="min-w-[260px] text-[11px] font-medium uppercase tracking-wide text-mute">
            Model
            <select value={model} onChange={e => setModel(e.target.value)} disabled={loading || busy} className={cx(inputCls, 'mt-1 w-full')}>
              {models.length === 0 && <option value="">{loading ? 'Loading models…' : 'No synced models'}</option>}
              {models.map(m => <option key={m.value} value={m.value}>{m.label}</option>)}
            </select>
          </label>
        </div>
      </section>

      <section className="min-h-[360px] space-y-4">
        {messages.length === 0 && !loading && (
          <div className="py-20 text-center text-sm text-mute">Send a message to test routing, retries and provider response handling.</div>
        )}
        {messages.map((m, i) => (
          <div key={`${m.role}-${i}`} className={cx('max-w-[88%] rounded-lg border px-4 py-3 text-sm leading-relaxed', m.role === 'user' ? 'ml-auto whitespace-pre-wrap border-accent/30 bg-accent/5' : 'border-line bg-panel')}>
            <div className="mb-1 text-[10px] font-semibold uppercase tracking-wide text-mute">{m.role}</div>
            {m.role === 'assistant' ? <Markdown text={m.content} /> : m.content}
          </div>
        ))}
        {busy && <div className="flex items-center gap-2 text-sm text-mute"><Spinner /> Streaming…</div>}
      </section>

      {error && <div className="border border-bad/30 bg-bad/5 px-4 py-3 text-sm text-bad">{error}</div>}
      <section className="border-t border-line pt-4">
        <textarea value={input} onChange={e => setInput(e.target.value)} onKeyDown={e => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); void send(); } }} placeholder="Message the selected model…" rows={4} disabled={!model || busy} className={cx(inputCls, 'w-full resize-y')} />
        <div className="mt-3 flex items-center justify-between gap-3">
          <span className="text-[11px] text-mute">Enter to send · Shift+Enter for a new line</span>
          <button onClick={() => void send()} disabled={!input.trim() || !model || busy} className={cx(btn.base, btn.primary)}>{busy ? 'Sending…' : 'Send'}</button>
        </div>
      </section>
    </div>
  );
}
