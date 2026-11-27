// Get started: three steps from a fresh install to a working request. Add a
// provider, create a key for your app, send a test request with that key.
import { useCallback, useEffect, useState, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { ApiError, get, type Account, type TestStep } from '../utils/api';
import { createToken, listTokens, routableModels, type TokenInfo } from '../utils/tokens';
import { PROVIDER_PRESETS, addProviderWithKey, slugify } from '../utils/providers';
import { START_PRESETS, START_SUBTITLES, START_TITLES, defaultTestModel, freeNamespace, startProgress, type StartPreset } from '../utils/start';
import { curlRequest, recipe } from '../utils/connect';
import { CopyButton, Segmented, SecretInput, Spinner, btn, cx, inputCls, labelCls } from '../components/ui';

const errText = (e: unknown) => (e instanceof ApiError ? e.detail.message : e instanceof Error ? e.message : String(e));

function Step({ n, title, state, summary, open, children }: {
  n: number; title: string; state: 'done' | 'current' | 'todo'; summary?: ReactNode;
  /** Keep the body open after the step is done (the test result and snippets). */
  open?: boolean; children?: ReactNode;
}) {
  return (
    <section className={cx('border-b border-line py-6 last:border-0', state === 'todo' && 'opacity-50')}>
      <div className="flex items-baseline gap-3">
        <span className={cx('grid h-6 w-6 flex-none place-items-center rounded-full text-[12px] font-semibold',
          state === 'done' ? 'bg-ok-dim text-ok' : state === 'current' ? 'bg-ink text-bg' : 'bg-raised text-mute')}>
          {state === 'done' ? '✓' : n}
        </span>
        <h2 className="text-[15px] font-semibold">{title}</h2>
        {state === 'done' && summary && <span className="truncate text-[13px] text-mute">{summary}</span>}
      </div>
      {(state === 'current' || open) && <div className="mt-4 pl-9">{children}</div>}
    </section>
  );
}

// ── 1 · provider ────────────────────────────────────────────────────────────

function AddProvider({ taken, onDone }: { taken: string[]; onDone: (a: Account, models: number) => void }) {
  const [preset, setPreset] = useState<StartPreset | null>(null);
  const [apiKey, setApiKey] = useState('');
  const [baseURL, setBaseURL] = useState('');
  const [busy, setBusy] = useState(false);
  const [steps, setSteps] = useState<TestStep[]>([]);
  const [err, setErr] = useState('');
  const p = preset ? PROVIDER_PRESETS[preset] : null;
  const other = preset === 'openai-compatible' || preset === 'anthropic-compatible';
  const ready = !!preset && apiKey.trim() !== '' && (!other || /^https?:\/\/\S+$/i.test(baseURL.trim()));

  const connect = async () => {
    if (!preset || !p || !ready) return;
    setBusy(true); setErr(''); setSteps([]);
    const url = other ? baseURL.trim() : p.baseURL ?? '';
    const name = other ? (new URL(url).hostname.replace(/^api\./, '') || START_TITLES[preset]) : START_TITLES[preset];
    try {
      const res = await addProviderWithKey(
        { name, namespace: freeNamespace(slugify(name), taken), kind: p.kind, base_url: url,
          requires_session_header: p.sessionHeader ?? false, probe_delay_ms: 400 },
        { label: 'primary', api_key: apiKey.trim() },
        (s) => setSteps((prev) => [...prev, s]));
      onDone(res.account, res.test.catalog?.length ?? 0);
    } catch (e) {
      setErr(errText(e));
    } finally { setBusy(false); }
  };

  return (
    <div>
      <div className="grid grid-cols-2 gap-2 sm:grid-cols-4" role="radiogroup" aria-label="Provider">
        {START_PRESETS.map((id) => (
          <button key={id} type="button" role="radio" aria-checked={preset === id} disabled={busy}
            onClick={() => { setPreset(id); setErr(''); setSteps([]); }}
            className={cx('rounded-lg border px-3 py-3 text-left transition-colors',
              preset === id ? 'border-ink bg-raised' : 'border-line hover:border-line-strong')}>
            <span className="block text-[13px] font-medium">{START_TITLES[id]}</span>
            {START_SUBTITLES[id] && <span className="block text-[11px] text-mute">{START_SUBTITLES[id]}</span>}
          </button>
        ))}
      </div>
      {preset && (
        <div className="mt-4 space-y-3">
          {other && (
            <div>
              <label className={labelCls} htmlFor="start-url">Base URL</label>
              <input id="start-url" className={cx(inputCls, 'font-mono')} spellCheck={false} value={baseURL} disabled={busy}
                placeholder={preset === 'anthropic-compatible' ? 'https://api.example.com' : 'https://api.example.com/v1'}
                onChange={(e) => setBaseURL(e.target.value)} />
            </div>
          )}
          <div>
            <label className={labelCls} htmlFor="start-key">Provider key</label>
            <SecretInput id="start-key" value={apiKey} onChange={(v) => { setApiKey(v); setErr(''); }} disabled={busy} placeholder="Paste the key from your provider" onEnter={() => void connect()} />
          </div>
          <div className="flex items-center gap-3">
            <button type="button" className={cx(btn.base, btn.primary)} disabled={!ready || busy} onClick={() => void connect()}>
              {busy ? <Spinner /> : null} {busy ? 'Checking the key…' : 'Connect'}
            </button>
            {busy && steps.length > 0 && <span className="text-[12px] text-mute">{steps.filter((s) => s.ok).length} checks passed</span>}
          </div>
          {err && <p role="alert" className="text-[13px] text-bad">{err}</p>}
        </div>
      )}
      <p className="mt-4 text-[12px] text-mute">
        Using a Claude, ChatGPT or Copilot subscription instead? <Link className="underline hover:text-ink" to="/providers?add=1">Add it on Providers</Link>.
      </p>
    </div>
  );
}

// ── 2 · app key ─────────────────────────────────────────────────────────────

function CreateKey({ onDone }: { onDone: (plaintext: string) => void }) {
  const [name, setName] = useState('my-app');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const create = async () => {
    setBusy(true); setErr('');
    try { onDone((await createToken(name.trim())).plaintext); }
    catch (e) { setErr(errText(e)); }
    finally { setBusy(false); }
  };
  return (
    <div>
      <p className="mb-3 text-[13px] text-mute">Your app sends this key instead of a provider key. It can call models and nothing else.</p>
      <div className="flex flex-wrap items-end gap-3">
        <div className="w-56">
          <label className={labelCls} htmlFor="start-name">Name</label>
          <input id="start-name" className={inputCls} value={name} disabled={busy} onChange={(e) => setName(e.target.value)} />
        </div>
        <button type="button" className={cx(btn.base, btn.primary)} disabled={busy || !name.trim()} onClick={() => void create()}>
          {busy ? <Spinner /> : null} Create key
        </button>
      </div>
      {err && <p role="alert" className="mt-3 text-[13px] text-bad">{err}</p>}
    </div>
  );
}

// ── 3 · test request ────────────────────────────────────────────────────────

type Reply = { text: string; ms: number; tokens: number };
type Tab = 'env-openai' | 'env-anthropic' | 'curl' | 'python';

function TryIt({ appKey, namespace, onDone }: { appKey: string; namespace?: string; onDone: () => void }) {
  const [models, setModels] = useState<string[] | null>(null);
  const [model, setModel] = useState('');
  const [key, setKey] = useState(appKey);
  const [busy, setBusy] = useState(false);
  const [reply, setReply] = useState<Reply | null>(null);
  const [err, setErr] = useState('');
  const [tab, setTab] = useState<Tab>('env-openai');
  const base = window.location.origin;

  useEffect(() => {
    routableModels().then((list) => {
      const ids = list.map((m) => m.id);
      setModels(ids);
      setModel(defaultTestModel(ids, namespace));
    }).catch(() => setModels([]));
  }, [namespace]);

  const send = async () => {
    setBusy(true); setErr(''); setReply(null);
    const t0 = performance.now();
    try {
      // The app key alone: no dashboard cookie, so this proves exactly what your app will do.
      const res = await fetch('/v1/chat/completions', {
        method: 'POST', credentials: 'omit',
        headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${key.trim()}` },
        body: JSON.stringify({ model, messages: [{ role: 'user', content: 'Say hello in five words.' }], max_tokens: 64 }),
      });
      const body = await res.json().catch(() => null);
      if (!res.ok) throw new Error(body?.error?.message ?? `HTTP ${res.status}`);
      setReply({
        text: String(body?.choices?.[0]?.message?.content ?? '').trim() || '(empty answer)',
        ms: Math.round(performance.now() - t0), tokens: Number(body?.usage?.total_tokens ?? 0),
      });
      onDone();
    } catch (e) { setErr(errText(e)); }
    finally { setBusy(false); }
  };

  const shownKey = key.trim() || 'YOUR_EZLLM_KEY';
  const snippet = tab === 'curl' ? curlRequest(base, model || 'MODEL', shownKey) : recipe(tab, base, shownKey, model || 'MODEL');
  return (
    <div className="space-y-4">
      {!appKey && (
        <div>
          <label className={labelCls} htmlFor="start-appkey">App key</label>
          <SecretInput id="start-appkey" value={key} onChange={setKey} placeholder="Paste the key you created" />
        </div>
      )}
      {appKey && (
        <div>
          <span className={labelCls}>Your app key</span>
          <div className="flex items-center gap-2">
            <code className="min-w-0 flex-1 truncate rounded-lg border border-line bg-bg px-3 py-2 font-mono text-[13px]">{appKey}</code>
            <CopyButton text={appKey} label="Copy" />
          </div>
          <p className="mt-1 text-[12px] text-warn">Copy it now. It is shown only once.</p>
        </div>
      )}
      <div className="flex flex-wrap items-end gap-3">
        <div className="min-w-0 flex-1">
          <label className={labelCls} htmlFor="start-model">Model</label>
          <select id="start-model" className={cx(inputCls, 'font-mono')} value={model} disabled={!models || busy} onChange={(e) => setModel(e.target.value)}>
            {!models && <option>Loading…</option>}
            {models?.map((id) => <option key={id} value={id}>{id}</option>)}
          </select>
        </div>
        <button type="button" className={cx(btn.base, btn.primary)} disabled={busy || !model || !key.trim()} onClick={() => void send()}>
          {busy ? <Spinner /> : null} Send a test request
        </button>
      </div>
      {err && <p role="alert" className="text-[13px] text-bad">{err}</p>}
      {reply && (
        <div className="rounded-lg border border-line bg-bg px-4 py-3" role="status">
          <p className="text-[14px]">{reply.text}</p>
          <p className="mt-1 text-[12px] text-mute">{reply.ms.toLocaleString()} ms · {reply.tokens.toLocaleString()} tokens</p>
        </div>
      )}
      <div>
        <div className="flex items-center justify-between gap-3">
          <span className={labelCls}>Point your app at ezllm</span>
          <Segmented size="sm" value={tab} onChange={setTab} options={[
            { key: 'env-openai', label: 'OpenAI' }, { key: 'env-anthropic', label: 'Anthropic' },
            { key: 'curl', label: 'curl' }, { key: 'python', label: 'Python' },
          ]} />
        </div>
        <pre className="mt-2 overflow-x-auto rounded-lg border border-line bg-bg p-3 font-mono text-[12px] leading-relaxed">{snippet}</pre>
        <div className="mt-2 flex justify-end"><CopyButton text={snippet} label="Copy" /></div>
      </div>
    </div>
  );
}

// ── page ────────────────────────────────────────────────────────────────────

export default function Start() {
  const [accounts, setAccounts] = useState<Account[] | null>(null);
  const [tokens, setTokens] = useState<TokenInfo[] | null>(null);
  const [added, setAdded] = useState<{ account: Account; models: number } | null>(null);
  const [appKey, setAppKey] = useState('');
  const [tested, setTested] = useState(false);
  const [err, setErr] = useState('');

  const load = useCallback(async () => {
    try {
      const [a, t] = await Promise.all([get<Account[]>('/admin/accounts'), listTokens()]);
      setAccounts(a); setTokens(t);
    } catch (e) { setErr(errText(e)); }
  }, []);
  useEffect(() => { void load(); }, [load]);

  if (err) return <p role="alert" className="text-[13px] text-bad">{err}</p>;
  if (!accounts || !tokens) return <div className="flex justify-center py-16"><Spinner /></div>;

  const p = startProgress(accounts, tokens);
  // A key created here stays on step 3 until its request is sent, even though
  // the server already counts it.
  const step = tested ? 4 : appKey ? 3 : p.next;
  const state = (n: number) => (step > n ? 'done' : step === n ? 'current' : 'todo') as 'done' | 'current' | 'todo';
  const provider = added?.account ?? accounts[accounts.length - 1];

  return (
    <div className="mx-auto max-w-2xl">
      <Step n={1} title="Add a provider" state={state(1)}
        summary={provider && `${provider.name}${added ? ` · ${added.models.toLocaleString()} models` : ''}`}>
        <AddProvider taken={accounts.map((a) => a.namespace)}
          onDone={(account, models) => { setAdded({ account, models }); void load(); }} />
      </Step>
      <Step n={2} title="Create a key for your app" state={state(2)} summary={appKey ? 'my-app' : 'done'}>
        <CreateKey onDone={(k) => { setAppKey(k); void load(); }} />
      </Step>
      <Step n={3} title="Send a test request" state={state(3)} summary="it works" open={tested}>
        <TryIt appKey={appKey} namespace={provider?.namespace} onDone={() => setTested(true)} />
      </Step>
      {step === 4 && (
        <section className="py-6">
          <h2 className="text-[15px] font-semibold">You're set.</h2>
          <ul className="mt-3 space-y-2 text-[13px]">
            <li><Link className="underline hover:text-ink" to="/providers?add=1">Add a second provider</Link> <span className="text-mute">so one can stand in for the other.</span></li>
            <li><Link className="underline hover:text-ink" to="/combos">Make a combo</Link> <span className="text-mute">to fall back automatically when a model fails.</span></li>
            <li><Link className="underline hover:text-ink" to="/connect">Set a budget</Link> <span className="text-mute">on your app key.</span></li>
          </ul>
        </section>
      )}
    </div>
  );
}
