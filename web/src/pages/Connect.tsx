// CONNECT — API keys for clients, and what to paste where.
//
// Things this page gets right on purpose:
//  1. The base URL is derived from window.location, never hardcoded. The same
//     binary answers on localhost, LAN IP or a reverse-proxied hostname; a
//     literal "http://127.0.0.1:20129" copied to a phone is instantly wrong.
//  2. A created key's plaintext is shown exactly once. The server keeps only a
//     hash, so if this UI loses it, it is gone — hence the blocking panel with
//     no way to dismiss until "I saved it".
//  3. A key's name is its identity: usage is counted by name, and creating a
//     key under an existing name re-keys that one. The create form refuses it.
import { useCallback, useEffect, useMemo, useState } from 'react';
import { Link, useSearchParams } from 'react-router-dom';
import { motion } from 'motion/react';
import { ApiError, type UsageRow } from '../utils/api';
import {
  Chip, Combobox, ConfirmModal, CopyButton, EmptyState, Modal, Mono, Segmented, SectionLabel, Spinner, Toggle,
  btn, cx, formatTokens, inputCls, labelCls, nfmt, relTime,
} from '../components/ui';
import { Stagger, StaggerItem } from '../components/motion';
import {
  createToken, listTokens, patchToken, revokeToken, routableModels, testKey, usageByClient,
  type KeyTest, type TokenCreated, type TokenInfo, type V1Model,
} from '../utils/tokens';
import {
  EXPIRY_PRESETS, RECIPES, allowedModels, curlRequest, expiryReadout, expiryTime, hasKeyErrors, keyState, presetDate, recipe,
  splitPatterns, validateKey,
  type KeyDraft, type RecipeKey,
} from '../utils/connect';

const WINDOWS = ['5h', 'daily', 'weekly', 'monthly'] as const;
const WINDOW_LABEL: Record<string, string> = { '5h': '5 hours', daily: 'Day', weekly: 'Week', monthly: 'Month' };
const CAP_QUICK = [100_000, 1_000_000, 10_000_000];

// Browser clients send a preflight. ezllm answers it for /v1/* out of the box
// (unset = any Origin echoed back), so the honest note is how to RESTRICT it —
// telling users to "set the env var to make it work" would be backwards.
const CORS_NOTE = (
  <>
    Browser preflights are allowed on <code className="font-mono text-dim">/v1/*</code> out of
    the box. Set <code className="font-mono text-dim">EZLLM_CORS_ORIGINS</code> to a
    comma-separated allowlist to restrict it.
  </>
);

const errText = (e: unknown, fallback: string) => (e instanceof ApiError ? e.message : e instanceof Error ? e.message : fallback);

/** One-line value + copy, the shape every route/key row needs. */
function FieldRow({ label, value, mono = true }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="flex items-center gap-2">
      <span className="w-20 shrink-0 text-[12px] text-mute">{label}</span>
      <code className={cx('min-w-0 flex-1 truncate rounded-md border border-line bg-raised px-2 py-1 text-[13px]', mono && 'font-mono')}>{value}</code>
      <CopyButton text={value} label="Copy" />
    </div>
  );
}

// The Policy modal's date-picker value for a key's stored RFC3339 expiry.
const expiryDate = (t: TokenInfo) => (t.expires_at ? t.expires_at.slice(0, 10) : '');

// ── allowed-models editor (shared by create and policy) ─────────────────────
// A comma-separated list of ids or globs, with a live readout of what it lets
// through, so a typo'd glob shows up as "matches nothing" before it is saved.
function AllowlistField({ id, value, onChange, ids }: { id: string; value: string; onChange: (v: string) => void; ids: string[] }) {
  const patterns = splitPatterns(value);
  const matched = useMemo(() => allowedModels(value, ids), [value, ids]);
  const add = (m: string) => { if (m && !patterns.includes(m)) onChange([...patterns, m].join(', ')); };
  return (
    <div>
      <input id={id} className={cx(inputCls, 'font-mono text-[13px]')} placeholder="deepseek/*, openai/gpt-5*" spellCheck={false}
        value={value} onChange={(e) => onChange(e.target.value)} />
      {ids.length > 0 && (
        <div className="mt-2 max-w-sm">
          <Combobox label="Add a model to the allowlist" idPrefix={`${id}-pick`} loadingText="No models" placeholder="Add a model…" value=""
            options={ids.map((m) => ({ value: m, label: m }))} onChange={add} />
        </div>
      )}
      <p className="mt-1.5 text-[12px] leading-snug text-mute" aria-live="polite">
        {patterns.length === 0
          ? <>No limit: the key can call every model{ids.length > 0 && <> ({nfmt(ids.length)} routable now)</>}.</>
          : ids.length === 0
            ? 'Comma-separated ids or globs. They are checked before a combo is expanded.'
            : matched.length === 0
              ? <span className="text-warn">Matches none of the {nfmt(ids.length)} models routable right now. Check the spelling: a glob’s * does not cross a “/”.</span>
              : <>Matches {nfmt(matched.length)} of {nfmt(ids.length)} routable models: <span className="font-mono text-dim">{matched.slice(0, 5).join(', ')}{matched.length > 5 ? `, and ${nfmt(matched.length - 5)} more` : ''}</span></>}
      </p>
    </div>
  );
}

function CapField({ cap, window, onCap, onWindow, error }: { cap: number; window: string; onCap: (n: number) => void; onWindow: (w: string) => void; error?: string }) {
  return (
    <div>
      <div className="flex flex-wrap items-center gap-3">
        <input aria-label="Token cap" type="number" min={0} step={1000} className={cx(inputCls, '!w-44 font-mono tabular-nums', error && 'border-bad/60')}
          value={cap} onChange={(e) => onCap(Math.max(0, Math.trunc(Number(e.target.value)) || 0))} />
        <span className="flex items-center gap-1.5">
          {CAP_QUICK.map((n) => (
            <button key={n} type="button" onClick={() => onCap(n)}
              className="rounded-md border border-line px-2 py-1 font-mono text-[12px] text-mute hover:border-line-strong hover:text-ink">{nfmt(n)}</button>
          ))}
        </span>
      </div>
      {error && <p className="mt-1 text-[12px] text-bad">{error}</p>}
      {cap > 0 ? (
        <div className="mt-2.5 flex flex-wrap items-center gap-3">
          <span className="text-[12px] text-mute">Counted per</span>
          <Segmented<string> size="sm" label="Budget window" value={window} onChange={onWindow}
            options={WINDOWS.map((w) => ({ key: w, label: WINDOW_LABEL[w] }))} />
          <span className="font-mono text-[12px] text-dim">{nfmt(cap)} tokens per {window === '5h' ? '5 hours' : window === 'daily' ? 'day' : window === 'weekly' ? 'week' : 'month'}</span>
        </div>
      ) : <p className="mt-1.5 text-[12px] text-mute">0 means no budget: the key can spend without limit.</p>}
    </div>
  );
}

function ExpiryField({ value, onChange, error }: { value: string; onChange: (v: string) => void; error?: string }) {
  return (
    <div>
      <div className="flex flex-wrap items-center gap-1.5">
        {EXPIRY_PRESETS.map((p) => {
          const date = presetDate(p.days);
          return (
            <button key={p.key} type="button" aria-pressed={p.days === 0 ? value === '' : value === date} onClick={() => onChange(date)}
              className={cx('rounded-md border px-2.5 py-1 text-[12px] transition-colors',
                (p.days === 0 ? value === '' : value === date) ? 'border-accent bg-accent-dim text-accent' : 'border-line text-mute hover:border-line-strong hover:text-ink')}>
              {p.label}
            </button>
          );
        })}
        <input aria-label="Expiry date" id="tk-expiry" type="date" className={cx(inputCls, '!w-40 py-1', error && 'border-bad/60')} value={value} onChange={(e) => onChange(e.target.value)} />
      </div>
      <p className={cx('mt-1.5 text-[12px]', error || (value && Date.parse(`${value}T23:59:59`) < Date.now()) ? 'text-bad' : 'text-mute')}>{error ?? expiryReadout(value)}</p>
    </div>
  );
}

// ── create ──────────────────────────────────────────────────────────────────
const EMPTY_KEY: KeyDraft = { name: '', role: 'infer', models: '', expiry: '', cap: 0, capWindow: 'monthly' };

function CreateKeyModal({ open, names, ids, onClose, onCreated }: {
  open: boolean; names: string[]; ids: string[]; onClose: () => void; onCreated: (t: TokenCreated, note?: string) => void;
}) {
  const [d, setD] = useState<KeyDraft>(EMPTY_KEY);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const [showAll, setShowAll] = useState(false);
  useEffect(() => { if (open) { setD(EMPTY_KEY); setBusy(false); setErr(''); setShowAll(false); } }, [open]);
  const set = (p: Partial<KeyDraft>) => setD((cur) => ({ ...cur, ...p }));
  const errors = validateKey(d, names);
  const dirty = open && JSON.stringify(d) !== JSON.stringify(EMPTY_KEY);
  const nameShown = errors.name && (showAll || d.name.trim() !== '');

  const submit = async () => {
    if (busy) return;
    if (hasKeyErrors(errors)) { setShowAll(true); return; }
    setBusy(true); setErr('');
    try {
      const t = await createToken(d.name.trim(), d.role, { allowed_models: d.models.trim(), expires_at: d.expiry });
      let note: string | undefined;
      // The create call takes no budget, so it is a second call. The key exists
      // either way: a failure here is reported next to the key, not hidden.
      if (d.cap > 0) {
        try { await patchToken(t.id, { cap_tokens: d.cap, cap_window: d.capWindow }); }
        catch (e) { note = `The key was created, but its budget was not saved (${errText(e, 'unknown error')}). Set it from Policy.`; }
      }
      onCreated(t, note);
    } catch (e) { setErr(errText(e, 'create failed')); } finally { setBusy(false); }
  };

  return (
    <Modal open={open} onClose={onClose} wide busy={busy} dirty={dirty} onSubmit={() => void submit()}
      title="Create an API key" subtitle="The key is shown once, right after you create it"
      footer={(close) => <>
        {showAll && hasKeyErrors(errors) && <span className="mr-auto text-[12px] text-bad">Fix the fields marked in red to create the key.</span>}
        <button type="button" className={cx(btn.base, btn.ghost)} onClick={close}>Cancel</button>
        <button type="submit" disabled={busy} className={cx(btn.base, btn.primary)}>{busy ? <Spinner /> : null}{busy ? 'Creating' : 'Create key'}</button>
      </>}>
      <div className="space-y-6">
        <div>
          <label className={labelCls} htmlFor="tk-name">Name</label>
          <input id="tk-name" className={cx(inputCls, nameShown && 'border-bad/60')} value={d.name} placeholder="hermes-laptop" spellCheck={false}
            aria-invalid={!!nameShown} onChange={(e) => set({ name: e.target.value })} />
          {nameShown ? <p className="mt-1 text-[12px] text-bad">{errors.name}</p> : <p className="mt-1 text-[12px] text-mute">Usage is counted by this name. Use one per app or machine.</p>}
        </div>

        <div>
          <span className={labelCls}>Role</span>
          <Segmented<'infer' | 'admin'> label="Role" value={d.role} onChange={(r) => set({ role: r })}
            options={[{ key: 'infer', label: 'Inference only' }, { key: 'admin', label: 'Full admin' }]} />
          {d.role === 'infer'
            ? <p className="mt-1.5 text-[12px] text-mute">Can call models on /v1 and nothing else. The right choice for any tool or app.</p>
            : <p className="mt-1.5 rounded-lg border border-warn/30 bg-warn-dim p-2.5 text-[13px] leading-relaxed text-warn">
              Full admin can also add providers, read provider keys and mint other keys. Keep it for yourself, never for a tool.
            </p>}
        </div>

        <div>
          <label className={labelCls} htmlFor="tk-models">Allowed models <span className="text-mute">(optional)</span></label>
          <AllowlistField id="tk-models" value={d.models} onChange={(v) => set({ models: v })} ids={ids} />
        </div>

        <div>
          <span className={labelCls}>Expires <span className="text-mute">(optional)</span></span>
          <ExpiryField value={d.expiry} onChange={(v) => set({ expiry: v })} error={errors.expiry} />
        </div>

        <div>
          <span className={labelCls}>Budget <span className="text-mute">(optional)</span></span>
          <CapField cap={d.cap} window={d.capWindow} onCap={(n) => set({ cap: n })} onWindow={(w) => set({ capWindow: w })} error={errors.cap} />
        </div>

        {err && <div role="alert" className="rounded-lg border border-bad/30 bg-bad-dim px-4 py-3 text-[13px] text-bad">{err}</div>}
      </div>
    </Modal>
  );
}

// ── show once ───────────────────────────────────────────────────────────────
function FreshKey({ fresh, base, sample, note, onDone }: { fresh: TokenCreated; base: string; sample: string; note?: string; onDone: () => void }) {
  const [tab, setTab] = useState<'env-openai' | 'env-anthropic' | 'curl'>('env-openai');
  const [test, setTest] = useState<KeyTest | null>(null);
  const [testing, setTesting] = useState(false);
  const model = sample || '<model-id>';
  const text = tab === 'curl' ? curlRequest(base, model, fresh.plaintext) : recipe(tab, base, fresh.plaintext, model);
  const run = async () => { setTesting(true); setTest(await testKey(fresh.plaintext)); setTesting(false); };
  return (
    // Deliberately NOT <Modal>: backdrop clicks and Escape would dismiss a
    // one-time secret. The only way out is the button below.
    <div className="fixed inset-0 z-50 flex items-center justify-center overflow-y-auto bg-overlay px-4 py-6">
      {/* The panel SCALES in slightly rather than fading: a one-time secret is
          the one moment in this app where the operator's attention should be
          pulled to the middle of the screen. */}
      <motion.div role="dialog" aria-modal="true" aria-label="New API key"
        initial={{ opacity: 0, scale: 0.96, y: 8 }} animate={{ opacity: 1, scale: 1, y: 0 }} transition={{ duration: 0.26, ease: [0.22, 1, 0.36, 1] }}
        className="my-auto w-full max-w-lg rounded-2xl border border-accent/40 bg-surface p-6 shadow-2xl">
        <div className="text-[16px] font-semibold">Copy this key now</div>
        <p className="mt-1.5 text-[13px] text-dim">
          ezllm stores only a hash, so this is the <span className="text-ink">only time</span> it can be shown. If you close this without saving it, create a new key.
        </p>
        <div className="mt-4 flex items-center gap-2 rounded-lg border border-line bg-bg px-3 py-2.5">
          <code className="min-w-0 flex-1 break-all font-mono text-[13px] text-accent">{fresh.plaintext}</code>
          <CopyButton text={fresh.plaintext} label="Copy" />
        </div>
        {note && <p role="alert" className="mt-3 rounded-lg border border-warn/30 bg-warn-dim p-2.5 text-[13px] leading-relaxed text-warn">{note}</p>}
        <div className="mt-3 space-y-2">
          <FieldRow label="Base URL" value={base} />
          <FieldRow label="Role" value={fresh.roles.join(' · ')} mono={false} />
          {fresh.roles.includes('admin') && (
            <p className="rounded-lg border border-warn/30 bg-warn-dim p-2.5 text-[13px] leading-relaxed text-warn">
              This key has admin role: it can add providers, read provider keys and mint other keys. For an
              app that only needs models, create an <span className="text-ink">infer</span> key instead.
            </p>
          )}
        </div>

        <div className="mt-4">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <Segmented<'env-openai' | 'env-anthropic' | 'curl'> size="sm" label="Paste-ready" value={tab} onChange={setTab}
              options={[{ key: 'env-openai', label: 'OpenAI env' }, { key: 'env-anthropic', label: 'Anthropic env' }, { key: 'curl', label: 'curl' }]} />
            <CopyButton text={text} label="Copy snippet" />
          </div>
          <pre className="mt-2 max-h-40 overflow-auto rounded-lg border border-line bg-bg px-3 py-2 font-mono text-[12px] leading-relaxed text-dim">{text}</pre>
        </div>

        <div className="mt-3 flex flex-wrap items-center gap-3">
          <button type="button" disabled={testing} className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[13px]')} onClick={() => void run()}>
            {testing ? <Spinner /> : null} Test this key
          </button>
          {test && <span role="status" className={cx('text-[13px]', test.ok ? 'text-ok' : 'text-bad')}>{test.message}</span>}
        </div>

        <div className="mt-5 flex items-center justify-end">
          <button type="button" className={cx(btn.base, btn.primary)} onClick={onDone}>I saved it</button>
        </div>
      </motion.div>
    </div>
  );
}

// ── policy ──────────────────────────────────────────────────────────────────
function PolicyModal({ target, ids, onClose, onSaved }: {
  target: TokenInfo | null; ids: string[]; onClose: () => void; onSaved: (t: TokenInfo) => void;
}) {
  const [cap, setCap] = useState(0);
  const [window, setWindow] = useState('monthly');
  const [models, setModels] = useState('');
  const [expiry, setExpiry] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  useEffect(() => {
    if (!target) return;
    setCap(target.cap_tokens); setWindow(target.cap_window || 'monthly'); setModels(target.allowed_models || ''); setExpiry(expiryDate(target));
    setBusy(false); setErr('');
  }, [target]);
  const dirty = !!target && (cap !== target.cap_tokens || window !== (target.cap_window || 'monthly') || models.trim() !== (target.allowed_models || '').trim() || expiry !== expiryDate(target));

  const save = async () => {
    if (!target || busy) return;
    setBusy(true); setErr('');
    try {
      const updated = await patchToken(target.id, {
        cap_tokens: cap, cap_window: window, allowed_models: models.trim(),
        // The picker shows only the date; resending an untouched one would be
        // re-read as end of the server's local day and move the deadline.
        ...(expiry !== expiryDate(target) ? { expires_at: expiry } : {}),
      });
      onSaved(updated);
    } catch (e) { setErr(errText(e, 'could not save the policy')); } finally { setBusy(false); }
  };

  return (
    <Modal open={target != null} onClose={onClose} busy={busy} dirty={dirty} onSubmit={() => void save()}
      title={`Key policy — ${target?.name ?? ''}`} subtitle="Budget, model allowlist and expiry for this key"
      footer={(close) => <>
        <button type="button" className={cx(btn.base, btn.ghost)} onClick={close}>Cancel</button>
        <button type="submit" disabled={busy || !dirty} className={cx(btn.base, btn.primary)}>{busy ? <Spinner /> : null} Save</button>
      </>}>
      <div className="space-y-6">
        <div>
          <span className={labelCls}>Token budget</span>
          <CapField cap={cap} window={window} onCap={setCap} onWindow={setWindow} />
          <p className="mt-1.5 text-[12px] leading-snug text-mute">Counted from the key’s real ledger spend (tokens in + out) inside the window. A spent key answers 429 until the window rolls; max_tokens is reserved before dispatch.</p>
        </div>
        <div>
          <label className={labelCls} htmlFor="pol-models">Allowed models <span className="text-mute">(empty = all)</span></label>
          <AllowlistField id="pol-models" value={models} onChange={setModels} ids={ids} />
        </div>
        <div>
          <span className={labelCls}>Expires <span className="text-mute">(empty = never)</span></span>
          <ExpiryField value={expiry} onChange={setExpiry} />
        </div>
        {err && <div role="alert" className="rounded-lg border border-bad/30 bg-bad-dim px-4 py-3 text-[13px] text-bad">{err}</div>}
      </div>
    </Modal>
  );
}

// ── models you can call ─────────────────────────────────────────────────────
function ModelsBrowser({ models }: { models: V1Model[] }) {
  const [query, setQuery] = useState('');
  const q = query.trim().toLowerCase();
  const shown = models.filter((m) => !q || m.id.toLowerCase().includes(q));
  return (
    <details className="rounded-xl border border-line bg-surface px-4 py-3">
      <summary className="cursor-pointer select-none text-[13px] font-semibold">
        Models you can call <span className="font-normal text-mute">· {nfmt(models.length)} routable right now</span>
      </summary>
      {models.length === 0 ? (
        <p className="mt-3 text-[13px] text-mute">Nothing is routable yet. Add and validate a provider, or create a combo.</p>
      ) : (
        <div className="mt-3 space-y-2">
          <input aria-label="Search models" type="search" className={cx(inputCls, 'max-w-sm py-1.5')} placeholder={`Search ${nfmt(models.length)} models…`} value={query} onChange={(e) => setQuery(e.target.value)} />
          <ul className="max-h-72 divide-y divide-line/60 overflow-y-auto rounded-lg border border-line">
            {shown.slice(0, 200).map((m) => (
              <li key={m.id} className="flex items-center gap-3 px-3 py-1.5">
                <span className="min-w-0 flex-1 truncate font-mono text-[12px]">{m.id}</span>
                <span className="text-[11px] text-mute">{m.id.includes('/') ? 'direct' : 'combo'}</span>
                {m.protocol?.chat === false && <span className="text-[11px] text-warn" title="the gateway has probed this model and it does not do chat">no chat</span>}
                <CopyButton text={m.id} label="Copy" />
              </li>
            ))}
            {shown.length === 0 && <li className="px-3 py-2 text-[12px] text-mute">No model matches “{query.trim()}”.</li>}
          </ul>
          {shown.length > 200 && <p className="text-[12px] text-mute">Showing 200 of {nfmt(shown.length)}. Search to narrow it.</p>}
        </div>
      )}
    </details>
  );
}

type Filter = 'all' | 'active' | 'attention' | 'disabled';

export default function Connect() {
  const [tokens, setTokens] = useState<TokenInfo[] | null>(null);
  const [usage, setUsage] = useState<UsageRow[] | null>(null);
  const [usageWindows, setUsageWindows] = useState<Record<string, UsageRow[]>>({});
  const [usageStale, setUsageStale] = useState(false);
  const [models, setModels] = useState<V1Model[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [notice, setNotice] = useState('');
  const [creating, setCreating] = useState(false);
  const [fresh, setFresh] = useState<{ token: TokenCreated; note?: string } | null>(null);
  const [busyId, setBusyId] = useState<number | null>(null);
  const [pendingRevoke, setPendingRevoke] = useState<TokenInfo | null>(null);
  const [policy, setPolicy] = useState<TokenInfo | null>(null);
  const [query, setQuery] = useState('');
  const [filter, setFilter] = useState<Filter>('all');
  const [recipeKey, setRecipeKey] = useState<RecipeKey>('env-openai');
  const [sampleChoice, setSampleChoice] = useState('');
  const [params, setParams] = useSearchParams();

  // Base URL: same origin as the dashboard, minus the trailing slash.
  const base = useMemo(() => window.location.origin, []);
  const reload = useCallback(async () => {
    setErr(null);
    try {
      const [tk, us, us5h, usDaily, usWeekly] = await Promise.all([
        listTokens(),
        usageByClient(30).catch(() => null),
        usageByClient(5 / 24).catch(() => null),
        usageByClient(1).catch(() => null),
        usageByClient(7).catch(() => null),
      ]);
      setTokens(tk);
      setUsageStale(!us || !us5h || !usDaily || !usWeekly);
      if (us) setUsage(us);
      setUsageWindows((previous) => ({
        ...previous,
        ...(us5h ? { '5h': us5h } : {}),
        ...(usDaily ? { daily: usDaily } : {}),
        ...(usWeekly ? { weekly: usWeekly } : {}),
        ...(us ? { monthly: us } : {}),
      }));
      // Live check through the dashboard session: the ids below are what the
      // gateway can route RIGHT NOW, so the lists are observations, not labels.
      try { setModels(await routableModels()); } catch { setModels([]); }
    } catch (e) {
      setErr(errText(e, 'could not load'));
    }
  }, []);
  useEffect(() => { void reload(); }, [reload]);

  // Deep links: /connect?new=1 opens the create form, ?policy=<id> a key's policy.
  useEffect(() => {
    if (!tokens) return;
    const policyId = Number(params.get('policy'));
    if (params.get('new') !== '1' && !policyId) return;
    const next = new URLSearchParams(params);
    next.delete('new'); next.delete('policy');
    setParams(next, { replace: true });
    if (params.get('new') === '1') setCreating(true);
    const t = tokens.find((x) => x.id === policyId);
    if (t) setPolicy(t);
  }, [tokens, params, setParams]);

  const chatModels = useMemo(() => models.filter((m) => m.protocol?.chat !== false).map((m) => m.id), [models]);
  const allIds = useMemo(() => models.map((m) => m.id), [models]);
  const sampleModel = chatModels.includes(sampleChoice) ? sampleChoice : chatModels[0];

  const usageFor = (name: string) => (usage ?? []).find((u) => u.k === name);
  const budgetUsageFor = (name: string, window: string) => (usageWindows[window] ?? []).find((u) => u.k === name);
  const spentOf = (t: TokenInfo) => { const bu = budgetUsageFor(t.name, t.cap_window || 'monthly'); return (bu?.tin ?? 0) + (bu?.tout ?? 0); };
  const stateOf = (t: TokenInfo) => keyState(t, spentOf(t));

  const toggle = async (t: TokenInfo, enabled: boolean) => {
    setBusyId(t.id); setErr(null); setNotice('');
    try {
      await patchToken(t.id, { enabled });
      setTokens((p) => (p ?? []).map((x) => (x.id === t.id ? { ...x, enabled } : x)));
      setNotice(`${enabled ? 'Enabled' : 'Disabled'} ${t.name}.`);
    } catch (e) { setErr(errText(e, 'could not change the key')); }
    finally { setBusyId(null); }
  };
  const revoke = async (t: TokenInfo) => {
    setBusyId(t.id); setErr(null); setNotice('');
    try { await revokeToken(t.id); setNotice(`Revoked ${t.name}.`); await reload(); }
    catch (e) { setErr(errText(e, 'could not revoke the key')); }
    finally { setBusyId(null); setPendingRevoke(null); }
  };

  // ── list ──
  const list = tokens ?? [];
  const q = query.trim().toLowerCase();
  const states = new Map(list.map((t) => [t.id, stateOf(t)]));
  const shown = list
    .filter((t) => !q || t.name.toLowerCase().includes(q) || t.hint.toLowerCase().includes(q) || (t.allowed_models ?? '').toLowerCase().includes(q))
    .filter((t) => {
      const s = states.get(t.id)!;
      return filter === 'all' || (filter === 'active' ? s.key === 'active' || s.key === 'expiring' : filter === 'disabled' ? s.key === 'disabled' : s.attention);
    });
  const activeN = list.filter((t) => ['active', 'expiring'].includes(states.get(t.id)!.key)).length;
  const attentionN = list.filter((t) => states.get(t.id)!.attention).length;
  const expiringN = list.filter((t) => states.get(t.id)!.key === 'expiring').length;
  const spentN = list.filter((t) => states.get(t.id)!.key === 'spent').length;
  const expiredN = list.filter((t) => states.get(t.id)!.key === 'expired').length;

  return (
    <div className="space-y-8">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <p className="max-w-2xl text-[13px] text-dim">
            Point Hermes, Claude Code or any OpenAI/Anthropic-style client at this gateway. One port, two protocols.
          </p>
          <p className="mt-1.5 text-[13px] text-mute" aria-live="polite">
            {tokens === null ? 'Loading keys…' : list.length === 0 ? 'No keys yet.' : (
              <>
                <span className="text-ink">{nfmt(list.length)}</span> key{list.length === 1 ? '' : 's'} · {nfmt(activeN)} active
                {expiringN > 0 && <> · <span className="text-warn">{nfmt(expiringN)} expiring soon</span></>}
                {expiredN > 0 && <> · <span className="text-bad">{nfmt(expiredN)} expired</span></>}
                {spentN > 0 && <> · <span className="text-bad">{nfmt(spentN)} over budget</span></>}
              </>
            )}
          </p>
        </div>
        <button type="button" className={cx(btn.base, btn.primary)} onClick={() => setCreating(true)}>＋ Create key</button>
      </div>

      {usageStale && <p role="alert" className="rounded-lg border border-warn/30 bg-warn-dim p-3 text-sm text-ink">Some key usage could not be loaded. Figures may be unavailable or out of date. <button type="button" className="underline" onClick={() => void reload()}>Retry</button></p>}
      {err && (
        <div role="alert" className="flex items-start justify-between gap-3 rounded-lg border border-[rgba(248,113,113,0.3)] bg-bad-dim px-4 py-3 text-[13px] text-bad">
          <span>{err}</span>
          <button type="button" aria-label="Dismiss" className="text-bad/70 hover:text-bad" onClick={() => setErr(null)}>✕</button>
        </div>
      )}
      {notice && !err && (
        <div className="flex items-center justify-between gap-3 rounded-lg border border-line bg-raised/60 px-4 py-2.5 text-[13px] text-dim" role="status">
          <span>{notice}</span>
          <button type="button" className="text-mute hover:text-ink" aria-label="Dismiss" onClick={() => setNotice('')}>✕</button>
        </div>
      )}

      {/* ── Your keys ──────────────────────────────────────────────── */}
      <section>
        <SectionLabel>Your keys</SectionLabel>
        {tokens === null ? (
          <div className="mt-3 space-y-2">{[0, 1].map((i) => <div key={i} className="h-14 animate-pulse rounded-xl border border-line bg-surface" />)}</div>
        ) : list.length === 0 ? (
          <div className="mt-3 rounded-xl border border-line bg-surface px-6 py-10 text-center">
            <p className="text-[14px] font-medium">No keys yet</p>
            <p className="mx-auto mt-1 max-w-md text-[13px] text-mute">A key lets one app or machine call this gateway. Create one per client so usage and budgets stay separate.</p>
            <button type="button" className={cx(btn.base, btn.primary, 'mt-5')} onClick={() => setCreating(true)}>＋ Create your first key</button>
          </div>
        ) : (
          <div className="mt-3 space-y-3">
            <div className="flex flex-wrap items-center gap-x-5 gap-y-2">
              <input aria-label="Search keys" type="search" placeholder="Search by name or model" value={query}
                onChange={(e) => setQuery(e.target.value)} className={cx(inputCls, 'w-full py-1.5 sm:!w-64')} />
              <Segmented<Filter> size="sm" label="Show" value={filter} onChange={setFilter}
                options={[
                  { key: 'all', label: 'All' }, { key: 'active', label: 'Active' }, { key: 'disabled', label: 'Disabled' },
                  { key: 'attention', label: attentionN > 0 ? `Needs attention (${attentionN})` : 'Needs attention' },
                ]} />
            </div>
            {shown.length === 0 ? (
              <EmptyState title="No key matches" hint="Nothing fits the search and filter together."
                action={<button type="button" className={cx(btn.base, btn.ghost)} onClick={() => { setQuery(''); setFilter('all'); }}>Clear search and filter</button>} />
            ) : (
              <Stagger className="space-y-2">
                {shown.map((t) => {
                  const u = usageFor(t.name);
                  const used = spentOf(t);
                  const capPct = t.cap_tokens > 0 ? Math.min(100, (used / t.cap_tokens) * 100) : 0;
                  const st = states.get(t.id)!;
                  const exp = expiryTime(t);
                  return (
                    <StaggerItem key={t.id}>
                      <div className={cx('rounded-xl border bg-surface px-4 py-3', st.attention ? (st.tone === 'bad' ? 'border-bad/40' : 'border-warn/40') : 'border-line')}>
                        <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
                          <div className="min-w-0 flex-1">
                            <div className="flex flex-wrap items-center gap-2">
                              <span className="truncate text-[13px] font-semibold">{t.name}</span>
                              <Chip tone={t.roles.includes('admin') ? 'accent' : 'neutral'}>{t.roles.join(' · ')}</Chip>
                              <Chip tone={st.tone} title={exp ? `expires ${t.expires_at.slice(0, 10)}` : undefined}>{st.label}</Chip>
                            </div>
                            <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-0.5 text-[12px] text-mute">
                              <Mono>{t.hint}</Mono>
                              <span title={t.last_used_at ? new Date(t.last_used_at).toLocaleString() : undefined}>
                                {t.last_used_at ? `used ${relTime(t.last_used_at)}` : 'never used'}
                              </span>
                              {exp != null && st.key !== 'expired' && st.key !== 'expiring' && <span>expires {t.expires_at.slice(0, 10)}</span>}
                              {t.allowed_models && <span className="truncate" title={t.allowed_models}>models: {t.allowed_models}</span>}
                            </div>
                          </div>
                          <span className="flex shrink-0 items-center gap-2">
                            <Link to={`/requests?${new URLSearchParams({ client: t.name, range: '30d' })}`}
                              className={cx(btn.base, btn.ghost, 'px-2.5 py-1.5 text-[12px]')} title={`The last 30 days of requests made with ${t.name}`}>View requests</Link>
                            <button type="button" className={cx(btn.base, btn.ghost, 'px-2.5 py-1.5 text-[12px]')} onClick={() => setPolicy(t)}>Policy</button>
                            <Toggle checked={t.enabled} onChange={(v) => void toggle(t, v)} disabled={busyId === t.id} label={`${t.enabled ? 'Disable' : 'Enable'} key ${t.name}`} />
                            <button type="button" className={cx(btn.base, btn.danger, 'px-2.5 py-1.5 text-[12px]')} onClick={() => setPendingRevoke(t)} disabled={busyId === t.id}>Revoke</button>
                          </span>
                        </div>
                        <div className="mt-2.5 flex flex-wrap items-end gap-x-6 gap-y-2 text-[12px] text-dim">
                          <span title="in the last 30 days">
                            {nfmt(u?.calls ?? 0)} call{(u?.calls ?? 0) === 1 ? '' : 's'} · {formatTokens(u?.tin ?? 0)} in · {formatTokens(u?.tout ?? 0)} out <span className="text-mute">· 30 days</span>
                          </span>
                          <span className="min-w-[160px] flex-1 sm:max-w-xs">
                            {t.cap_tokens > 0 ? (
                              <>
                                <span className="flex items-center justify-between gap-2">
                                  <span className="text-mute">Budget · {t.cap_window || 'monthly'}</span>
                                  <span className="font-mono text-[11px]">{nfmt(used)} / {nfmt(t.cap_tokens)}</span>
                                </span>
                                {/* The budget meter FILLS: a key that has burned a
                                    tenth of its cap should read as a tenth full the
                                    moment it loads. The colour changes with the fill,
                                    so a key near its cap is obvious at a glance. */}
                                <span className="mt-1 block h-1 overflow-hidden rounded-full bg-shade">
                                  <motion.span className={cx('block h-full rounded-full', capPct >= 90 ? 'bg-bad' : capPct >= 70 ? 'bg-warn' : 'bg-accent')}
                                    initial={{ width: 0 }} animate={{ width: `${capPct}%` }} transition={{ duration: 0.6, ease: [0.22, 1, 0.36, 1] }} />
                                </span>
                              </>
                            ) : <span className="text-mute">Budget unlimited</span>}
                          </span>
                        </div>
                      </div>
                    </StaggerItem>
                  );
                })}
              </Stagger>
            )}
          </div>
        )}
      </section>

      {/* ── Connect a client ───────────────────────────────────────── */}
      <section>
        <SectionLabel>Connect a client</SectionLabel>
        {/* [&>*]:min-w-0 is load-bearing: grid items default to min-width:auto, so
            the unbreakable endpoint URLs set the track's min-content width and
            blow the page out sideways on a 390px phone. */}
        <div className="mt-3 rounded-xl border border-line bg-surface px-4 py-4">
          <div className="grid gap-2 md:grid-cols-2 [&>*]:min-w-0">
            <FieldRow label="OpenAI" value={`${base}/v1`} />
            <FieldRow label="Anthropic" value={base} />
          </div>
          <p className="mt-2 text-[12px] text-mute">
            OpenAI clients take the base with <code className="font-mono text-dim">/v1</code>. Anthropic clients add <code className="font-mono text-dim">/v1/messages</code> themselves, so their base is the bare address. Mixing them up is the most common mistake.
          </p>

          <div className="mt-4 flex flex-wrap items-center gap-x-5 gap-y-2">
            <Segmented<RecipeKey> size="sm" label="Client" value={recipeKey} onChange={setRecipeKey}
              options={RECIPES.map((r) => ({ key: r.key, label: r.label, title: r.hint }))} />
            {chatModels.length > 1 && (
              <span className="flex min-w-0 items-center gap-2 text-[12px] text-mute">
                Model
                <span className="w-64 max-w-full"><Combobox label="Sample model" idPrefix="sample" loadingText="No models" value={sampleModel ?? ''}
                  options={chatModels.map((m) => ({ value: m, label: m }))} onChange={setSampleChoice} /></span>
              </span>
            )}
          </div>
          <pre className="mt-3 overflow-x-auto rounded-lg border border-line bg-bg px-3 py-2 font-mono text-[12px] leading-relaxed text-dim">
{recipe(recipeKey, base, '<your-key>', sampleModel ?? '<model-id>')}
          </pre>
          <div className="mt-2 flex items-center justify-between gap-3">
            <span className="text-[12px] text-mute">{RECIPES.find((r) => r.key === recipeKey)?.hint} Replace <span className="font-mono">&lt;your-key&gt;</span> with a key from above.</span>
            <CopyButton text={recipe(recipeKey, base, '<your-key>', sampleModel ?? '<model-id>')} label="Copy snippet" />
          </div>
        </div>
        <p className="mt-2 text-[12px] text-mute">{CORS_NOTE}</p>
      </section>

      <section>
        <SectionLabel>Try a first request</SectionLabel>
        {sampleModel ? <div className="mt-3 rounded-xl border border-line bg-surface p-4">
          <p className="text-sm text-dim">Create an inference-only key above, then replace YOUR_EZLLM_KEY in this command. It sends a short request to {sampleModel} and may use provider quota. Availability and surface pins can still affect the result.</p>
          <pre className="mt-3 overflow-x-auto rounded-lg border border-line bg-bg p-3 text-xs">{curlRequest(base, sampleModel)}</pre>
          <div className="mt-2 flex justify-end"><CopyButton text={curlRequest(base, sampleModel)} label="Copy request" /></div>
        </div> : <p className="mt-3 text-sm text-dim">Add and validate a provider first; a request example appears when a model is available.</p>}
      </section>

      <ModelsBrowser models={models} />

      <CreateKeyModal open={creating} names={list.map((t) => t.name)} ids={allIds} onClose={() => setCreating(false)}
        onCreated={(t, note) => { setCreating(false); setFresh({ token: t, note }); void reload(); }} />
      <PolicyModal target={policy} ids={allIds} onClose={() => setPolicy(null)}
        onSaved={(updated) => { setTokens((cur) => cur?.map((t) => (t.id === updated.id ? updated : t)) ?? null); setNotice(`Saved the policy for ${updated.name}.`); setPolicy(null); void reload(); }} />
      {fresh && <FreshKey fresh={fresh.token} base={base} sample={sampleModel ?? ''} note={fresh.note} onDone={() => { setNotice(`Created ${fresh.token.name}.`); setFresh(null); }} />}
      <ConfirmModal
        open={pendingRevoke != null}
        title={pendingRevoke ? `Revoke "${pendingRevoke.name}"?` : 'Revoke token?'}
        body={pendingRevoke && (() => {
          const calls = usageFor(pendingRevoke.name)?.calls ?? 0;
          return <>Clients using this key fail immediately, and it cannot be un-revoked.{calls > 0 && <> It made {nfmt(calls)} request{calls === 1 ? '' : 's'} in the last 30 days, so something is using it.</>} To stop it without losing it, disable it instead.</>;
        })()}
        confirmLabel="Revoke"
        busy={pendingRevoke != null && busyId === pendingRevoke.id}
        onClose={() => setPendingRevoke(null)}
        onConfirm={() => { if (pendingRevoke) void revoke(pendingRevoke); }} />
    </div>
  );
}
