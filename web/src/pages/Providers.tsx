// PROVIDERS — account cards, add-provider wizard modal, per-key actions, model drawer.
import { useCallback, useEffect, useState } from 'react';
import {
  ApiError, AuthError, del, get, modelRules, patch, post, setModelPin,
  setPrice, deletePrice, startSweep, stopSweep, sweepStatus,
  KINDS, STEPS,
  type Account, type ApiErrorDetail, type KeyRow, type ModelRule, type ModelRow, type Price, type SweepStatus, type TestResult, type TestStep,
} from '../api';
import { Chip, ConfirmModal, Modal, SectionLabel, Skeleton, SkeletonCards, Spinner, Toggle, btn, cx, inputCls, labelCls, relTime, Mono } from '../ui';

// ── helpers ─────────────────────────────────────────────────────────────

const slugify = (s: string) =>
  s.toLowerCase().replace(/[^a-z0-9._-]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 63);

function ErrBox({ e, onDismiss }: { e: ApiErrorDetail | null; onDismiss: () => void }) {
  if (!e) return null;
  return (
    <div className="rounded-lg border border-bad/30 bg-bad-dim px-4 py-3 animate-fade-in">
      <div className="flex items-start justify-between gap-3">
        <p className="text-[13px] text-bad">{e.message}</p>
        <button onClick={onDismiss} className="shrink-0 text-mute hover:text-ink" aria-label="dismiss">✕</button>
      </div>
      {e.upstream_error?.message && (
        <Mono className="mt-1 block text-[11.5px] text-dim">{e.upstream_error.type ? `${e.upstream_error.type}: ` : ''}{e.upstream_error.message}</Mono>
      )}
      {e.hint && <p className="mt-2 text-[11.5px] leading-relaxed text-mute">{e.hint}</p>}
      {e.combos && e.combos.length > 0 && (
        <div className="mt-2 flex flex-wrap gap-1.5">
          {e.combos.map((c) => <Chip key={c.id} tone="warn"><span className="font-mono">{c.name}</span></Chip>)}
        </div>
      )}
    </div>
  );
}

// ── step list (7-step key test UI) ──────────────────────────────────────

const STEP_ORDER: string[] = STEPS as unknown as string[];

// Horizontal step chain (doc #7.5): the six probe steps read left→right with
// arrows between them instead of a numbered vertical list — per-step detail
// rides along as a title tooltip so nothing is lost, only the space.
function StepList({ steps, running }: { steps: TestStep[] | null; running: boolean }) {
  const byStep = new Map((steps ?? []).map((s) => [s.step, s]));
  return (
    <ol className="flex flex-wrap items-center gap-x-1.5 gap-y-1 font-mono text-[11.5px]">
      {STEP_ORDER.map((name, idx) => {
        const s = byStep.get(name);
        const state = s ? (s.ok ? 'ok' : 'bad') : running && idx <= byStep.size ? 'pending' : 'idle';
        const tip = s?.detail ? `${s.detail}${s.ms != null ? ` · ${s.ms}ms` : ''}` : name;
        return (
          <li key={name} className="flex items-center gap-1.5">
            {idx > 0 && <span className="text-mute/40">→</span>}
            <span title={tip} className={cx('flex items-center gap-1.5 rounded-full border px-2 py-0.5',
              state === 'ok' && 'border-ok/40 bg-ok-dim text-ok',
              state === 'bad' && 'border-bad/40 bg-bad-dim text-bad',
              state === 'pending' && 'border-line text-mute',
              state === 'idle' && 'border-line/60 text-mute/50')}>
              <span>{state === 'ok' ? '✓' : state === 'bad' ? '✕' : idx + 1}</span>
              <span>{name}</span>
            </span>
          </li>
        );
      })}
    </ol>
  );
}

// ── add provider modal ──────────────────────────────────────────────────

// RATE_WINDOWS are the window spellings the request-budget field offers. A Go
// duration string, matching cooldown.on_429 and the store's PATCH contract.
const RATE_WINDOWS = ['1s', '10s', '30s', '1m', '5m', '15m', '1h'];

type Draft = {
  name: string; namespace: string; nsTouched: boolean; kind: string; preset: string;
  base_url: string; keyLabel: string; apiKey: string;
  probe_delay_ms: number; notes: string; skipInference: boolean;
  advOpen: boolean; requiresSessionHeader: boolean; customHeaders: string;
  accountId: string;
};

const emptyDraft: Draft = {
  name: '', namespace: '', nsTouched: false, kind: 'opencode-go', preset: 'opencode-go',
  base_url: 'https://opencode.ai/zen/go/v1', keyLabel: 'primary', apiKey: '',
  probe_delay_ms: 400, notes: '', skipInference: false,
  advOpen: false, requiresSessionHeader: true, customHeaders: '',
  accountId: '',
};

const PROVIDER_PRESETS: Record<string, {
  label: string; kind: string; baseURL?: string; sessionHeader?: boolean;
  // accountURL builds the base URL from an account ID the operator supplies.
  // Cloudflare is the only one that needs it: the account ID lives INSIDE the
  // endpoint path.
  accountURL?: (id: string) => string;
}> = {
  'claude-platform': { label: 'Claude Platform', kind: 'anthropic-compatible', baseURL: 'https://api.anthropic.com' },
  'opencode-go': { label: 'OpenCode Go', kind: 'opencode-go', baseURL: 'https://opencode.ai/zen/go/v1', sessionHeader: true },
  'xiaomi-mimo-token-plan': { label: 'Xiaomi MiMo token plan', kind: 'openai-compatible', baseURL: 'https://token-plan-sgp.xiaomimimo.com/v1' },
  'alibaba-model-studio-token-plan': { label: 'Alibaba Cloud Model Studio token plan', kind: 'openai-compatible', baseURL: 'https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1' },
  'commandcode': { label: 'Command Code', kind: 'openai-compatible', baseURL: 'https://api.commandcode.ai/provider/v1' },
  'deepseek': { label: 'DeepSeek', kind: 'openai-compatible', baseURL: 'https://api.deepseek.com' },
  'groq': { label: 'Groq', kind: 'openai-compatible', baseURL: 'https://api.groq.com/openai/v1' },
  'tokenrouter': { label: 'TokenRouter', kind: 'openai-compatible', baseURL: 'https://api.tokenrouter.com/v1' },
  // Vercel AI Gateway: OpenAI-compatible chat + /models discovery over one key,
  // model ids are `creator/model` (slashes inside the id — splitNamespace
  // splits on the FIRST slash only, so `vercel/anthropic/claude-opus-5` routes
  // exactly like tokenrouter's slash-ids already do).
  'vercel-ai-gateway': { label: 'Vercel AI Gateway', kind: 'openai-compatible', baseURL: 'https://ai-gateway.vercel.sh/v1' },
  'ollama-cloud': { label: 'Ollama Cloud', kind: 'openai-compatible', baseURL: 'https://ollama.com/v1' },
  'gemini': { label: 'Google AI Studio', kind: 'gemini-openai', baseURL: 'https://generativelanguage.googleapis.com/v1beta/openai' },
  'openai': { label: 'OpenAI', kind: 'openai-compatible', baseURL: 'https://api.openai.com/v1' },
  // NVIDIA NIM — standard OpenAI-compatible chat, model ids are bare
  // (e.g. meta/llama-3.3-70b-instruct), so splitNamespace routes them like
  // tokenrouter's slash-ids already do.
  'nvidia-nim': { label: 'NVIDIA NIM', kind: 'openai-compatible', baseURL: 'https://integrate.api.nvidia.com/v1' },
  // Cloudflare Workers AI — the account ID composes the base URL. Workers AI
  // serves the whole OpenAI family under /accounts/{id}/ai/v1: /chat/completions,
  // /responses and an Anthropic-compatible /messages, so nothing here needs a
  // new protocol. Model ids carry their own slashes (@cf/moonshotai/kimi-k2.6).
  'cloudflare-workers-ai': {
    label: 'Cloudflare Workers AI', kind: 'openai-compatible',
    accountURL: (id) => `https://api.cloudflare.com/client/v4/accounts/${encodeURIComponent(id.trim())}/ai/v1`,
  },
  'openrouter': { label: 'OpenRouter', kind: 'openai-compatible', baseURL: 'https://openrouter.ai/api/v1' },
  'openai-compatible': { label: 'OpenAI-compatible', kind: 'openai-compatible' },
  'anthropic-compatible': { label: 'Anthropic-compatible', kind: 'anthropic-compatible' },
};
const PRESET_IDS = Object.keys(PROVIDER_PRESETS);
// The add-provider dropdown reads in label order (babe's ask): a copy, so the
// insertion order above stays the documented canonical list.
const sortedPresetIDs = () =>
  [...PRESET_IDS].sort((a, b) => PROVIDER_PRESETS[a].label.localeCompare(PROVIDER_PRESETS[b].label));

// Endpoints the gateway already knows: every preset plus the two big public
// APIs. Once an account points at one of these, the base URL is fixed — editing
// it only ever introduces a typo, and the adapter owns the path suffixes anyway.
const KNOWN_ENDPOINTS = new Set<string>([
  ...PRESET_IDS.map((p) => PROVIDER_PRESETS[p].baseURL ?? '').filter(Boolean),
  'https://api.openai.com/v1',
  'https://api.anthropic.com',
]);
const isKnownEndpoint = (url: string) => KNOWN_ENDPOINTS.has(url.replace(/\/+$/, ''));

// ── quota presentation (moved from the dashboard's per-account quota panel:
//    quota data belongs under the provider it describes) ──────────────────────

function stale(quota: { read_at?: string | null } | null | undefined): boolean {
  if (!quota?.read_at) return false;
  const t = Date.parse(quota.read_at);
  return !Number.isNaN(t) && Date.now() - t > 5 * 60 * 1000;
}

function QuotaBars({ quota }: { quota: NonNullable<Account['quota']> }) {
  const rows: Array<[string, number]> = [];
  if (quota.percent_rolling != null) rows.push(['rolling', quota.percent_rolling]);
  if (quota.percent_weekly != null) rows.push(['weekly', quota.percent_weekly]);
  if (quota.percent_monthly != null) rows.push(['monthly', quota.percent_monthly]);
  if (rows.length > 0 && rows.every(([, v]) => typeof v === 'number')) {
    return (
      <div className="space-y-1.5">
        {rows.map(([k, v]) => (
          <div key={k} className="flex items-center gap-2.5">
            <span className="w-14 text-[10.5px] uppercase tracking-wide text-mute">{k}</span>
            <div className="h-1.5 min-w-16 flex-1 overflow-hidden rounded-full bg-shade">
              <div className={cx('h-full rounded-full', v >= 90 ? 'bg-bad' : v >= 70 ? 'bg-warn' : 'bg-ok')}
                style={{ width: `${Math.min(100, Math.max(0, v))}%` }} />
            </div>
            <span className="tnum w-10 text-right font-mono text-[11px] text-dim">{v}%</span>
          </div>
        ))}
      </div>
    );
  }
  if (quota.kind === 'money' && quota.balance != null) {
    return (
      <div className="font-mono text-[12px]">
        <span className="text-dim">{quota.balance}</span>
        {quota.unit ? <span className="ml-1 text-mute">{quota.unit}</span> : null}
        {quota.plan ? <div className="mt-0.5 text-[10.5px] text-mute">{quota.plan}</div> : null}
      </div>
    );
  }
  return <span className="text-mute">–</span>;
}

function AddProviderModal({ open, onClose, onDone }: { open: boolean; onClose: () => void; onDone: () => void }) {
  const [d, setD] = useState<Draft>(emptyDraft);
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<TestResult | null>(null);
  const [err, setErr] = useState<ApiErrorDetail | null>(null);

  useEffect(() => {
    if (open) { setD(emptyDraft); setBusy(false); setResult(null); setErr(null); }
  }, [open]);

  const set = <K extends keyof Draft>(k: K, v: Draft[K]) => setD((p) => ({ ...p, [k]: v }));

  const saveAndValidate = async () => {
    setBusy(true); setErr(null); setResult(null);
    let createdAccountID: number | null = null;
    try {
      const customHeaders = d.customHeaders.trim() ? JSON.parse(d.customHeaders) as Record<string, unknown> : null;
      const preset = PROVIDER_PRESETS[d.preset];
      const payload = {
        name: d.name.trim(), namespace: d.namespace.trim(), kind: preset.kind,
        base_url: preset.baseURL ?? d.base_url.trim(), probe_delay_ms: d.probe_delay_ms,
        requires_session_header: preset.sessionHeader ?? d.requiresSessionHeader,
        custom_headers: customHeaders, notes: d.notes.trim(),
        // No quota_mode / caps here on purpose: quota is always probed, and
        // cap_window + cap_tokens live on Rules (per model, enforced in-flight).
      };
      const account = await post<Account>('/admin/accounts', payload);
      createdAccountID = account.id;
      const response = await post<{ key: KeyRow; test: TestResult }>(`/admin/accounts/${account.id}/keys`, {
        label: d.keyLabel.trim() || 'primary', api_key: d.apiKey.trim(), skip_inference: d.skipInference,
      });
      setResult(response.test);
      onDone();
      onClose();
    } catch (e) {
      if (createdAccountID != null) {
        try { await del(`/admin/accounts/${createdAccountID}?force=true`); }
        catch { /* preserve the original validation failure; user can resolve the account conflict in the UI */ }
      }
      if (e instanceof ApiError) setErr(e.detail);
      else setErr({ status: 0, type: 'error', message: e instanceof Error ? e.message : String(e) });
    } finally { setBusy(false); }
  };

  const isCompat = ['openai-compatible', 'anthropic-compatible', 'gemini-openai'].includes(PROVIDER_PRESETS[d.preset]?.kind ?? '');
  const isAnthropicKind = PROVIDER_PRESETS[d.preset]?.kind === 'anthropic-compatible';
  const presetAccountID = PROVIDER_PRESETS[d.preset]?.accountURL;
  const valid = d.name.trim().length > 0
    && /^[a-z0-9][a-z0-9._-]{0,62}$/.test(d.namespace.trim())
    && (!isCompat || /^https?:\/\//i.test(d.base_url.trim()))
    && d.apiKey.trim().length > 0;

  return (
    <Modal open={open} onClose={onClose} wide title="Add provider"
      subtitle="Add the account and validate its API key before the key is saved."
      footer={(
        <>
          <button type="button" className={cx(btn.base, btn.ghost)} onClick={onClose} disabled={busy}>Cancel</button>
          <button type="button" disabled={busy || !valid} className={cx(btn.base, btn.primary)} onClick={() => void saveAndValidate()}>
            {busy ? <Spinner /> : null} {busy ? 'Validating key…' : 'Save & validate key'}
          </button>
        </>
      )}>
      <div className="space-y-4">
        <div className="grid gap-4 sm:grid-cols-2">
          <div>
            <label className={labelCls} htmlFor="ap-name">Name</label>
            <input id="ap-name" className={inputCls} value={d.name} placeholder="OpenCode Go"
              onChange={(e) => {
                const name = e.target.value;
                setD((p) => ({ ...p, name, namespace: p.nsTouched ? p.namespace : slugify(name) }));
              }} />
          </div>
          <div>
            <label className={labelCls} htmlFor="ap-ns">Namespace</label>
            <input id="ap-ns" className={cx(inputCls, 'font-mono')} value={d.namespace} placeholder="opengo" spellCheck={false}
              onChange={(e) => setD((p) => ({ ...p, namespace: slugify(e.target.value), nsTouched: true }))} />
            <p className="mt-1 text-[11px] text-mute">lowercase slug used in model ids</p>
          </div>
        </div>
        <div className="grid gap-4 sm:grid-cols-2">
          <div>
            <label className={labelCls} htmlFor="ap-kind">Provider type</label>
            <select id="ap-kind" className={inputCls} value={d.preset} onChange={(e) => {
              const presetID = e.target.value;
              const preset = PROVIDER_PRESETS[presetID];
              setD((p) => ({ ...p, preset: presetID, kind: preset.kind,
                accountId: '',
                base_url: preset.baseURL ?? (preset.accountURL ? preset.accountURL('') : ''),
                requiresSessionHeader: preset.sessionHeader ?? false }));
            }}>
              {sortedPresetIDs().map((presetID) => <option key={presetID} value={presetID}>{PROVIDER_PRESETS[presetID].label}</option>)}
            </select>
          </div>
          <div>
            <label className={labelCls} htmlFor="ap-delay">Probe delay (ms)</label>
            <input id="ap-delay" type="number" min={0} className={cx(inputCls, 'font-mono tabular-nums')} value={d.probe_delay_ms}
              onChange={(e) => set('probe_delay_ms', Number(e.target.value) || 0)} />
          </div>
        </div>
        {presetAccountID ? (
          <div>
            <label className={labelCls} htmlFor="ap-account-id">Cloudflare account ID</label>
            <input id="ap-account-id" className={cx(inputCls, 'font-mono')} value={d.accountId}
              placeholder="from wrangler whoami" spellCheck={false}
              onChange={(e) => {
                const accountId = e.target.value.trim();
                setD((p) => ({ ...p, accountId, base_url: presetAccountID(accountId) }));
              }} />
            <p className="mt-1 text-[11px] text-mute">
              The account ID is part of the endpoint. It composes
              <Mono> /accounts/&lt;account_id&gt;/ai/v1</Mono> — Workers AI serves
              chat, responses and anthropic messages under that prefix.
            </p>
            {d.accountId && (
              <div className="mt-2 rounded-lg border border-line bg-raised/60 px-3.5 py-2.5 text-[12px] text-dim">
                Endpoint preset: <Mono>{d.base_url}</Mono>
              </div>
            )}
          </div>
        ) : isCompat && !isKnownEndpoint(d.base_url) ? (
          <div>
            <label className={labelCls} htmlFor="ap-url">Provider API base URL</label>
            <input id="ap-url" className={cx(inputCls, 'font-mono')} value={d.base_url}
              placeholder={isAnthropicKind ? 'https://api.anthropic.com' : 'https://api.provider.com/v1'} spellCheck={false}
              onChange={(e) => set('base_url', e.target.value.trim())} />
            <p className="mt-1 text-[11px] text-mute">{isAnthropicKind
              ? 'Required for compatibility providers. Enter the host only — /v1 paths are added automatically.'
              : 'Required for compatibility providers. Use the base URL ending at the API version, e.g. /v1.'}</p>
          </div>
        ) : (
          <div className="rounded-lg border border-line bg-raised/60 px-3.5 py-2.5 text-[12px] text-dim">
            {isKnownEndpoint(d.base_url) ? 'Known endpoint — fixed: ' : 'Endpoint preset: '}<Mono>{d.base_url}</Mono>
          </div>
        )}
        <div className="grid gap-4 sm:grid-cols-2">
          <div>
            <label className={labelCls} htmlFor="ap-key">API key</label>
            <input id="ap-key" type="password" autoComplete="off" spellCheck={false} className={cx(inputCls, 'font-mono')}
              placeholder="Paste provider API key" value={d.apiKey} onChange={(e) => set('apiKey', e.target.value)} />
          </div>
          <div>
            <label className={labelCls} htmlFor="ap-kl">Key label</label>
            <input id="ap-kl" className={inputCls} value={d.keyLabel} onChange={(e) => set('keyLabel', e.target.value)} placeholder="primary" />
          </div>
        </div>
        <div>
          <label className={labelCls} htmlFor="ap-notes">Notes</label>
          <input id="ap-notes" className={inputCls} value={d.notes} onChange={(e) => set('notes', e.target.value)} placeholder="optional" />
        </div>
        <div className="flex items-center gap-3">
          <Toggle checked={d.skipInference} onChange={(v) => set('skipInference', v)} label="skip inference probe" />
          <span className="text-[12.5px] text-dim">Skip inference probe <span className="text-mute">(saves a token spend)</span></span>
        </div>
        <div className="rounded-lg border border-line bg-raised/60 px-4 py-3">
          <SectionLabel className="mb-2">Validation</SectionLabel>
          <StepList steps={result?.steps ?? null} running={busy} />
          {result?.ok && <p className="mt-2 font-mono text-[11.5px] text-ok">✓ Key validated and stored.</p>}
        </div>
        <details className="rounded-lg border border-line bg-raised/40 px-4 py-3">
          <summary className="cursor-pointer text-[12px] font-medium text-dim">Advanced settings</summary>
          <div className="mt-4 space-y-4">
            <div className="flex items-center gap-3">
              <Toggle checked={d.requiresSessionHeader} onChange={(v) => set('requiresSessionHeader', v)} label="requires_session_header" />
              <span className="text-[12.5px] text-dim">requires session header upstream</span>
            </div>
            <div>
              <label className={labelCls} htmlFor="ap-cH">Custom headers (JSON)</label>
              <textarea id="ap-cH" rows={3} spellCheck={false} className={cx(inputCls, 'font-mono text-[12px]')}
                placeholder={'{"x-team":"ops"}'} value={d.customHeaders} onChange={(e) => set('customHeaders', e.target.value)} />
            </div>
          </div>
        </details>
        <ErrBox e={err} onDismiss={() => setErr(null)} />
      </div>
    </Modal>
  );
}

// ── edit modal ──────────────────────────────────────────────────────────

function EditAccountModal({ account, onClose, onSaved }: { account: Account | null; onClose: () => void; onSaved: () => void }) {
  const [name, setName] = useState('');
  const [kind, setKind] = useState('openai-compatible');
  const [baseUrl, setBaseUrl] = useState('');
  const [probeDelay, setProbeDelay] = useState(400);
  const [notes, setNotes] = useState('');
  const [rateLimit, setRateLimit] = useState(0);
  const [rateWindow, setRateWindow] = useState('60s');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<ApiErrorDetail | null>(null);

  useEffect(() => {
    if (account) {
      setName(account.name); setKind(account.kind); setBaseUrl(account.base_url);
      setProbeDelay(account.probe_delay_ms ?? 400); setNotes(account.notes ?? ''); setErr(null);
      setRateLimit(account.rate_limit ?? 0);
      setRateWindow(account.rate_window || '60s');
    }
  }, [account]);

  if (!account) return null;

  const save = async () => {
    setBusy(true); setErr(null);
    try {
      await patch<Account>(`/admin/accounts/${account.id}`, {
        name: name.trim(), kind, base_url: baseUrl.trim(), probe_delay_ms: probeDelay, notes: notes.trim(),
        // Sent unconditionally: 0 is a meaningful value (unlimited) and the
        // server clears the window alongside it, so "unlimited" is expressible
        // as a real state rather than an omission.
        rate_limit: rateLimit,
        rate_window: rateLimit > 0 ? rateWindow : '',
      });
      onSaved();
      onClose();
    } catch (e) {
      if (e instanceof ApiError) setErr(e.detail);
      else setErr({ status: 0, type: 'error', message: e instanceof Error ? e.message : String(e) });
    } finally { setBusy(false); }
  };

  return (
    <Modal open={account != null} onClose={onClose} title={`Edit ${account.name}`} subtitle="PATCH /admin/accounts/{id}"
      footer={<>
        <button type="button" className={cx(btn.base, btn.ghost)} onClick={onClose}>Cancel</button>
        <button type="button" disabled={busy || !name.trim() || !baseUrl.trim()} className={cx(btn.base, btn.primary)} onClick={() => void save()}>
          {busy ? <Spinner /> : null} Save
        </button>
      </>}>
      <div className="space-y-4">
        <div className="grid gap-4 sm:grid-cols-2">
          <div>
            <label className={labelCls}>Name</label>
            <input className={inputCls} value={name} onChange={(e) => setName(e.target.value)} />
          </div>
          <div>
            <label className={labelCls}>Kind</label>
            <select className={inputCls} value={kind} onChange={(e) => setKind(e.target.value)}>
              {KINDS.map((k) => <option key={k} value={k}>{k}</option>)}
            </select>
          </div>
        </div>
        <div>
          <label className={labelCls}>Base URL</label>
          {isKnownEndpoint(account.base_url) ? (
            <div className="rounded-lg border border-line bg-raised/60 px-3.5 py-2.5 font-mono text-[12px] text-dim">
              {baseUrl} <span className="font-sans text-mute">— known endpoint, fixed</span>
            </div>
          ) : (
            <input className={cx(inputCls, 'font-mono')} value={baseUrl} onChange={(e) => setBaseUrl(e.target.value.trim())} />
          )}
        </div>
        <div className="grid gap-4 sm:grid-cols-2">
          <div>
            <label className={labelCls}>Probe delay (ms)</label>
            <input type="number" min={0} className={cx(inputCls, 'font-mono tabular-nums')} value={probeDelay} onChange={(e) => setProbeDelay(Number(e.target.value) || 0)} />
          </div>
          <div>
            <label className={labelCls}>Notes</label>
            <input className={inputCls} value={notes} onChange={(e) => setNotes(e.target.value)} />
          </div>
        </div>
        <div className="rounded-lg border border-line bg-raised/60 p-4">
          <SectionLabel>Request budget (account-wide)</SectionLabel>
          <p className="mt-1 text-[11px] text-mute">
            The provider's own request ceiling, stated its way. It applies to the credential, so every
            model routed through this account shares it. A spent account is skipped and the combo fails
            over — the provider is never asked a request it has already refused. Leave 0 for unlimited.
          </p>
          <div className="mt-3 grid gap-4 sm:grid-cols-2">
            <div>
              <label className={labelCls}>Requests</label>
              <input type="number" min={0} className={cx(inputCls, 'font-mono tabular-nums')}
                value={rateLimit} onChange={(e) => setRateLimit(Number(e.target.value) || 0)} />
            </div>
            <div>
              <label className={labelCls}>Per window</label>
              <select className={inputCls} value={rateWindow} disabled={rateLimit <= 0}
                onChange={(e) => setRateWindow(e.target.value)}>
                {RATE_WINDOWS.map((w) => <option key={w} value={w}>{w}</option>)}
              </select>
            </div>
          </div>
          {rateLimit > 0 ? (
            <p className="mt-2 text-[11px] text-accent">
              {rateLimit} request{rateLimit === 1 ? '' : 's'} per {rateWindow} — a combo with another hop
              will fail over instead of failing.
            </p>
          ) : null}
        </div>
        <ErrBox e={err} onDismiss={() => setErr(null)} />
      </div>
    </Modal>
  );
}

// ── models drawer ───────────────────────────────────────────────────────

const PIN_SURFACES = ['openai', 'anthropic', 'responses', 'embeddings', 'images_gen', 'audio_tts', 'audio_stt'] as const;

function ModelsDrawer({ id }: { id: number }) {
  const [models, setModels] = useState<ModelRow[] | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [pinErr, setPinErr] = useState<string | null>(null);
  // A surface the probe never confirmed needs an explicit override. Held here
  // so the prompt is per-row and cannot be bypassed by a stale select value.
  const [pendingForce, setPendingForce] = useState<{ model: string; pin: string } | null>(null);
  // The "test all models" job. While it runs the drawer polls the status route
  // and refreshes the model list, because probes write verdicts per model as
  // they go — the list itself is the progress bar.
  const [sweep, setSweep] = useState<SweepStatus | null>(null);
  const [sweepBusy, setSweepBusy] = useState(false);
  const [sweepNote, setSweepNote] = useState<string | null>(null);
  // Default: probe only what is untested — a second click never re-spend.
  const [includeTested, setIncludeTested] = useState(false);
  // The row whose per-model Test was clicked. Its button keeps the spinner
  // while that single-model sweep runs (the header line carries the clock).
  const [testingId, setTestingId] = useState<string | null>(null);

  const loadModels = useCallback(() => {
    get<ModelRow[]>(`/admin/accounts/${id}/models`)
      .then((m) => setModels(m))
      .catch((e) => setErr(e instanceof Error ? e.message : 'failed to load models'));
  }, [id]);

  useEffect(() => {
    let alive = true;
    get<ModelRow[]>(`/admin/accounts/${id}/models`)
      .then((m) => { if (alive) setModels(m); })
      .catch((e) => { if (alive) setErr(e instanceof Error ? e.message : 'failed to load models'); });
    sweepStatus(id).then((s) => { if (alive) setSweep(s); }).catch(() => undefined);
    return () => { alive = false; };
  }, [id]);

  // Poll while a sweep runs: status for the clock, models for the verdicts.
  const running = sweep?.running === true;
  useEffect(() => {
    if (!running) return;
    const iv = setInterval(() => {
      sweepStatus(id)
        .then((s) => {
          setSweep(s);
          if (!s.running) { setTestingId(null); loadModels(); } // the sweep finished: show its verdicts
        })
        .catch(() => undefined);
      if (Math.floor(Date.now() / 3000) % 2 === 0) loadModels();
    }, 3000);
    return () => clearInterval(iv);
  }, [running, id, loadModels]);

  const startTestAll = () => {
    setSweepBusy(true);
    setSweepNote(null);
    setPinErr(null);
    startSweep(id, { all: true, includeTested })
      .then((res) => {
        setSweep({ running: res.probing > 0, probing: res.probing, elapsed_s: 0 });
        setSweepNote(res.probing > 0
          ? `testing ${res.probing} model${res.probing === 1 ? '' : 's'} on ${(res.surfaces ?? []).join(' + ')}`
          : res.note ?? 'nothing to probe');
        loadModels();
      })
      .catch((e: unknown) => setSweepNote(e instanceof Error ? e.message : 'test all failed'))
      .finally(() => setSweepBusy(false));
  };

  const stopTestAll = () => {
    setSweepBusy(true);
    stopSweep(id)
      .then(() => setSweep({ running: false }))
      .catch((e: unknown) => setSweepNote(e instanceof Error ? e.message : 'stop failed'))
      .finally(() => { setSweepBusy(false); setTestingId(null); loadModels(); });
  };

  // Per-row test: the SAME sync endpoint scoped to ONE model (the backend has
  // always accepted a models array). Force re-test — a manual click means "test
  // it now", not "test it unless it is proven". Curated rows never reach this:
  // their button is disabled, because a probe write cannot overwrite an
  // operator-curated verdict (server-side, proven by a test) — clicking would
  // only spend quota and change nothing.
  const testOne = (m: ModelRow) => {
    setSweepBusy(true);
    setTestingId(m.id);
    setSweepNote(null);
    setPinErr(null);
    startSweep(id, { all: true, includeTested: true, models: [m.id] })
      .then((res) => {
        setSweep({ running: res.probing > 0, probing: res.probing, elapsed_s: 0 });
        if (res.probing > 0) {
          setSweepNote(`testing ${m.id} · ${(res.surfaces ?? []).join(' + ')}`);
        } else {
          setTestingId(null);
          // probing=0 with only_untested:false means the id is NOT in the
          // provider's live catalog — the server's generic note would blame
          // verdicts, which cannot be the cause here.
          setSweepNote(`${m.id} is no longer in the provider's live catalog — sync to refresh the list`);
        }
        loadModels();
      })
      .catch((e: unknown) => {
        setTestingId(null);
        setSweepNote(e instanceof Error ? e.message : 'test failed');
      })
      .finally(() => setSweepBusy(false));
  };

  if (err) return <div className="border-t border-line px-5 py-4 text-[12px] text-bad">{err}</div>;
  if (!models) return <div className="border-t border-line px-5 py-4"><Skeleton className="h-3.5 w-40" /><Skeleton className="mt-2 h-3.5 w-56" /></div>;
  if (models.length === 0) return <div className="border-t border-line px-5 py-4 text-[12.5px] text-mute">No models synced — run “Sync models”.</div>;

  const applyPin = (model: string, pin: string, force: boolean) => {
    setPinErr(null);
    setModels((prev) => prev?.map((m) => (m.id === model ? { ...m, pin } : m)) ?? prev);
    setModelPin(id, model, pin, force)
      .then(() => setPendingForce(null))
      .catch((e: unknown) => {
        // Roll the select back to what the server still holds.
        get<ModelRow[]>(`/admin/accounts/${id}/models`).then(setModels).catch(() => undefined);
        if (e instanceof ApiError && e.status === 409) {
          setPendingForce({ model, pin });
          return;
        }
        setPinErr(e instanceof Error ? e.message : 'pin failed');
      });
  };

  const onPinChange = (m: ModelRow, pin: string) => {
    if (pin === '') return applyPin(m.id, '', false);
    // Chat-family surfaces have probe verdicts; a pin to an unconfirmed one
    // needs an explicit force. The new surfaces (embeddings/images/audio)
    // have no probe column yet, so ANY pin to them needs the confirm step.
    const confirmed = pin === 'openai' ? m.openai
      : pin === 'anthropic' ? m.anthropic
      : pin === 'responses' ? m.responses
      : undefined;
    if (!confirmed) return setPendingForce({ model: m.id, pin });
    applyPin(m.id, pin, false);
  };

  const untested = models.filter((m) => m.openai == null && m.anthropic == null && m.responses == null).length;
  const verdict = (v: boolean | number | null | undefined) => (v == null ? '–' : v ? 'ok' : 'no');

  return (
    <div className="border-t border-line px-5 py-4 animate-fade-in">
      {/* Test-all: probe the whole catalog, skip what is already proven, and
          stay stoppable — a 400-model × 3-surface sweep is minutes long. */}
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <span className="text-[11.5px] text-mute">
          {models.length} model{models.length === 1 ? '' : 's'} · {untested} untested
          {running && (
            <span className="ml-2 text-accent">
              testing… {sweep?.probing ?? 0} queued{sweep?.elapsed_s != null ? ` · ${sweep.elapsed_s}s` : ''}
            </span>
          )}
        </span>
        <span className="flex items-center gap-2">
          {!running && (
            <label className="flex items-center gap-1.5 text-[11.5px] text-mute" title="probe models that already carry a verdict too — a re-test after an upstream change">
              <input type="checkbox" checked={includeTested} onChange={(e) => setIncludeTested(e.target.checked)} />
              re-test all
            </label>
          )}
          {running ? (
            <button type="button" className={cx(btn.base, btn.ghost)} disabled={sweepBusy} onClick={stopTestAll}>
              {sweepBusy ? <Spinner /> : null} Stop
            </button>
          ) : (
            <button type="button" className={cx(btn.base, btn.primary)} disabled={sweepBusy || models.length === 0}
              title="one tiny request per model per surface — the probe tests the wire, not the model's tool-dialect"
              onClick={startTestAll}>
              {sweepBusy ? <Spinner /> : null} Test all models
            </button>
          )}
        </span>
      </div>
      {sweepNote && <div className="mb-2 text-[11.5px] text-dim">{sweepNote}</div>}
      <div className="max-h-64 overflow-y-auto pr-1">
        <table className="w-full text-[12px]">
          <thead>
            <tr className="text-left text-[10px] uppercase tracking-[0.1em] text-mute">
              <th className="pb-1.5 font-medium">model</th>
              <th className="pb-1.5 font-medium">status</th>
              <th className="pb-1.5 font-medium">surface</th>
              <th className="pb-1.5 text-right font-medium">test</th>
            </tr>
          </thead>
          <tbody className="font-mono">
            {models.map((m) => (
              <tr key={m.id} className="border-t border-line/50">
                <td className="max-w-[220px] truncate py-1.5 text-dim" title={m.id}>{m.id}</td>
                <td className="whitespace-nowrap py-1.5 text-[11px]">
                  {m.openai == null && m.anthropic == null && m.responses == null ? (
                    <span className="text-mute">untested</span>
                  ) : (
                    <>
                      <div className="text-dim">
                        chat {verdict(m.openai)} · msgs {verdict(m.anthropic)} · resp {verdict(m.responses)}
                      </div>
                      <div className="text-[10px] text-mute"
                        title={m.note ?? undefined}>
                        {m.source === 'curated' ? 'curated' : 'probed'} {relTime(m.tested_at)}
                      </div>
                    </>
                  )}
                </td>
                <td className="py-1.5">
                  <select
                    aria-label={`surface for ${m.id}`}
                    value={m.pin ?? ''}
                    onChange={(e) => onPinChange(m, e.target.value)}
                    className="rounded border border-line bg-transparent px-1.5 py-0.5 text-[11.5px] text-dim"
                  >
                    <option value="">auto (probed)</option>
                    {PIN_SURFACES.map((s) => {
                      const ok = s === 'openai' ? m.openai
                        : s === 'anthropic' ? m.anthropic
                        : s === 'responses' ? m.responses
                        : undefined;
                      // undefined = no probe column for this surface yet.
                      const tag = ok === undefined ? ' (no probe)' : ok ? '' : ' (unconfirmed)';
                      return <option key={s} value={s}>{s}{tag}</option>;
                    })}
                  </select>
                </td>
                <td className="py-1.5 text-right">
                  <button
                    type="button"
                    className={cx(btn.base, btn.ghost, 'px-2 py-0.5')}
                    disabled={running || sweepBusy || m.source === 'curated'}
                    title={m.source === 'curated'
                      ? 'this verdict is operator-curated — probes never overwrite it; release it first to re-test'
                      : 'one tiny request for THIS model on all three surfaces — tests the wire, not the tool-dialect'}
                    onClick={() => testOne(m)}
                  >
                    {testingId === m.id ? <Spinner /> : null}
                    Test
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {pinErr && <div className="mt-2 text-[11.5px] text-bad">{pinErr}</div>}
      </div>
      {pendingForce && (
        <ConfirmModal
          open
          title={`Pin ${pendingForce.model} to ${pendingForce.pin}?`}
          body={`The probe has not confirmed ${pendingForce.model} on ${pendingForce.pin}. Pinning it anyway can make every request 400 if the upstream rejects that surface.`}
          confirmLabel="Pin anyway"
          onConfirm={() => applyPin(pendingForce.model, pendingForce.pin, true)}
          onClose={() => setPendingForce(null)}
        />
      )}
    </div>
  );
}

// ── quota modal ─────────────────────────────────────────────────────────────

// Live /v1/usage read for one account. The button that opens this is only
// enabled once a probe proved the provider exposes /v1/usage; caps are shown
// here read-only and stay owned by the Rules page (doc #7 + #9).
type QuotaLive = { kind: string; raw: string; read_at: string };

const prettyBody = (raw: string) => {
  try { return JSON.stringify(JSON.parse(raw), null, 2); } catch { return raw; }
};

// ── pricing drawer: versioned per-model rates for cost analysis ─────────

// Rates live per (account, model, date). The newest row on or before a call's
// date prices it; re-setting the same version overwrites, a new date versions.
function PricingModal({ account, onClose }: { account: Account; onClose: () => void }) {
  const [rows, setRows] = useState<Price[] | null>(null);
  const [models, setModels] = useState<ModelRow[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [form, setForm] = useState({ model: '', effective_from: new Date().toISOString().slice(0, 10), price_in: '', price_out: '', price_cache_read: '', price_cache_write: '', note: '' });

  const load = useCallback(() => {
    get<Price[]>('/admin/prices')
      .then((ps) => setRows(ps.filter((p) => p.account_id === account.id)))
      .catch((e) => setErr(e instanceof Error ? e.message : 'failed to load prices'));
    get<ModelRow[]>(`/admin/accounts/${account.id}/models`)
      .then(setModels)
      .catch(() => setModels([]));
  }, [account.id]);
  useEffect(() => { load(); }, [load]);

  const setRate = async () => {
    // All four required: the API refuses partial rows so a missing rate can
    // never silently mean "free".
    const nums = [form.price_in, form.price_out, form.price_cache_read, form.price_cache_write];
    if (!form.model || !form.effective_from || nums.some((v) => v.trim() === '' || Number.isNaN(Number(v)))) {
      setErr('model, date and all four rates are required (USD per 1M tokens)');
      return;
    }
    setBusy(true); setErr(null);
    try {
      await setPrice(account.id, {
        model_id: form.model, effective_from: form.effective_from, currency: 'USD',
        price_in: Number(form.price_in), price_out: Number(form.price_out),
        price_cache_read: Number(form.price_cache_read), price_cache_write: Number(form.price_cache_write),
        note: form.note,
      });
      setForm({ ...form, price_in: '', price_out: '', price_cache_read: '', price_cache_write: '', note: '' });
      load();
    } catch (e) { setErr(e instanceof Error ? e.message : 'save failed'); }
    finally { setBusy(false); }
  };

  const remove = async (p: Price) => {
    setBusy(true); setErr(null);
    try { await deletePrice(account.id, p.model_id, p.effective_from); load(); }
    catch (e) { setErr(e instanceof Error ? e.message : 'delete failed'); }
    finally { setBusy(false); }
  };

  const cell = 'rounded border border-line bg-transparent px-2 py-1 text-[12px] text-ink';
  return (
    <Modal open onClose={onClose} title={`Pricing — ${account.name}`}
      subtitle="USD per 1M tokens, versioned by date · cost derives from the rate effective on each call's day"
      footer={<button type="button" className={cx(btn.base, btn.ghost)} onClick={onClose}>Close</button>}>
      <div className="space-y-4">
        {err && <div className="rounded-lg border border-bad/30 bg-bad/5 px-3 py-2 text-[12px] text-bad">{err}</div>}
        {rows === null ? <div className="space-y-2"><Skeleton className="h-3.5 w-40" /><Skeleton className="h-3.5 w-56" /></div>
          : rows.length === 0 ? <p className="text-[12.5px] text-mute">No rates yet — every call from this account is UNPRICED until you add one.</p>
          : <div className="overflow-x-auto"><table className="w-full text-[12px]"><thead><tr className="text-left text-[10px] uppercase tracking-[0.1em] text-mute"><th className="pb-1 font-medium">model</th><th className="pb-1 font-medium">from</th><th className="pb-1 text-right">in</th><th className="pb-1 text-right">out</th><th className="pb-1 text-right">cache r</th><th className="pb-1 text-right">cache w</th><th className="pb-1"></th></tr></thead>
            <tbody className="font-mono">{rows.map((p) => (
              <tr key={`${p.model_id}-${p.effective_from}`} className="border-t border-line/50">
                <td className="max-w-[180px] truncate py-1.5 text-dim" title={p.model_id}>{p.model_id}</td>
                <td className="py-1.5 text-dim">{p.effective_from}</td>
                <td className="py-1.5 text-right text-ink">{p.price_in}</td>
                <td className="py-1.5 text-right text-ink">{p.price_out}</td>
                <td className="py-1.5 text-right text-ink">{p.price_cache_read}</td>
                <td className="py-1.5 text-right text-ink">{p.price_cache_write}</td>
                <td className="py-1.5 text-right"><button type="button" disabled={busy} className="text-mute hover:text-bad disabled:opacity-40" onClick={() => void remove(p)} title="delete this version">✕</button></td>
              </tr>))}</tbody></table></div>}
        <div className="rounded-lg border border-line bg-raised/60 p-3">
          <SectionLabel>set / update a rate</SectionLabel>
          <div className="mt-2 flex flex-wrap gap-2">
            <select aria-label="model" value={form.model} onChange={(e) => setForm({ ...form, model: e.target.value })} className={cell}>
              <option value="">model…</option>
              {models.map((m) => <option key={m.id} value={m.id}>{m.id}</option>)}
            </select>
            <input aria-label="effective from" type="date" value={form.effective_from} onChange={(e) => setForm({ ...form, effective_from: e.target.value })} className={cell} />
            {(['price_in', 'price_out', 'price_cache_read', 'price_cache_write'] as const).map((k) => (
              <input key={k} aria-label={k} inputMode="decimal" placeholder={k.replace('price_', '')} value={form[k]}
                onChange={(e) => setForm({ ...form, [k]: e.target.value })} className={cx(cell, 'w-20')} />
            ))}
            <input aria-label="note" placeholder="note" value={form.note} onChange={(e) => setForm({ ...form, note: e.target.value })} className={cx(cell, 'w-28')} />
            <button type="button" className={cx(btn.base, btn.primary, 'px-3 py-1')} disabled={busy || !models.length} onClick={() => void setRate()}>
              {busy ? <Spinner /> : null} Save
            </button>
          </div>
          {!models.length && <p className="mt-2 text-[11px] text-mute">Sync this account's models first — rates can only name a model the catalog knows.</p>}
        </div>
      </div>
    </Modal>
  );
}

function QuotaModal({ account, onClose }: { account: Account; onClose: () => void }) {
  const [live, setLive] = useState<QuotaLive | null>(null);
  const [busy, setBusy] = useState(true);
  const [err, setErr] = useState<ApiErrorDetail | null>(null);
  const [rules, setRules] = useState<ModelRule[] | null>(null);

  const load = useCallback(() => {
    setBusy(true);
    setErr(null);
    get<QuotaLive>(`/admin/accounts/${account.id}/quota`)
      .then((q) => setLive(q))
      .catch((e: unknown) => setErr({ status: 0, type: 'error', message: e instanceof Error ? e.message : 'quota read failed' }))
      .finally(() => setBusy(false));
  }, [account.id]);

  useEffect(() => { load(); }, [load]);
  useEffect(() => {
    let alive = true;
    modelRules()
      .then((rs) => { if (alive) setRules(rs.filter((r) => r.account_id === account.id)); })
      .catch(() => { if (alive) setRules([]); });
    return () => { alive = false; };
  }, [account.id]);

  const caps = (rules ?? []).filter((r) => r.cap_tokens > 0);

  return (
    <Modal open onClose={onClose} title={`Quota — ${account.name}`}
      subtitle="live GET /v1/usage · caps come from Rules"
      footer={(
        <>
          <button type="button" className={cx(btn.base, btn.ghost)} onClick={onClose}>Close</button>
          <button type="button" className={cx(btn.base, btn.primary)} disabled={busy} onClick={load}>
            {busy ? <Spinner /> : null} Refresh
          </button>
        </>
      )}>
      <div className="space-y-4">
        <ErrBox e={err} onDismiss={() => setErr(null)} />
        <div className="rounded-lg border border-line bg-raised/60 p-4">
          <SectionLabel>Usage</SectionLabel>
          {busy && !live && <Skeleton className="h-3.5 w-56" />}
          {!busy && live?.kind === 'none' && (
            <p className="text-[13px] text-mute">This provider does not expose <code className="font-mono">/v1/usage</code>.</p>
          )}
          {live && live.kind !== 'none' && (
            <div className="mt-2 space-y-2">
              <div className="flex flex-wrap items-center gap-2 text-[12px]">
                <span className="rounded-full border border-line bg-bg px-2 py-0.5 font-mono text-[11px] text-accent">{live.kind}</span>
                <span className="text-mute">read {live.read_at ? relTime(live.read_at) : '—'}</span>
              </div>
              <pre className="max-h-56 overflow-auto whitespace-pre-wrap break-all rounded-md border border-line bg-bg px-3 py-2 font-mono text-[11.5px] leading-relaxed text-dim">
                {prettyBody(live.raw)}
              </pre>
            </div>
          )}
        </div>
        <div className="rounded-lg border border-line bg-raised/60 p-4">
          <SectionLabel>Request budget · account-wide</SectionLabel>
          {(account.rate_limit ?? 0) > 0 ? (
            <p className="mt-2 text-[13px] text-dim">
              <span className="font-mono tabular-nums text-ink">{account.rate_limit}</span> requests per{' '}
              <span className="font-mono text-ink">{account.rate_window}</span>
              <span className="text-mute"> — shared by every model on this account; a spent account is skipped and the combo fails over.</span>
            </p>
          ) : (
            <p className="mt-2 text-[13px] text-mute">
              Unlimited — no request ceiling is set for this account. Set one under Edit provider if the
              provider enforces its own rate limit.
            </p>
          )}
        </div>
        <div className="rounded-lg border border-line bg-raised/60 p-4">
          <SectionLabel>Cap window · cap tokens</SectionLabel>
          {rules === null && <Skeleton className="mt-2 h-3.5 w-48" />}
          {rules !== null && caps.length === 0 && (
            <p className="mt-2 text-[13px] text-mute">No caps set for this account — add one under <span className="text-ink">Rules</span>.</p>
          )}
          {caps.length > 0 && (
            <table className="mt-2 w-full text-[12px]">
              <thead>
                <tr className="text-left text-[10px] uppercase tracking-[0.1em] text-mute">
                  <th className="pb-1.5 font-medium">model</th>
                  <th className="pb-1.5 font-medium">cap tokens</th>
                  <th className="pb-1.5 font-medium">window</th>
                </tr>
              </thead>
              <tbody className="font-mono">
                {caps.map((c) => (
                  <tr key={c.id} className="border-t border-line/50">
                    <td className="max-w-[220px] truncate py-1.5 text-dim" title={c.model_id}>{c.model_id}</td>
                    <td className="py-1.5 tabular-nums text-dim">{c.cap_tokens.toLocaleString()}</td>
                    <td className="py-1.5 text-dim">{c.cap_window}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </div>
    </Modal>
  );
}

// ── account card ────────────────────────────────────────────────────────

function AccountCard({ a, onChanged, onReloadQuota, quotaBusy }: {
  a: Account; onChanged: () => void;
  onReloadQuota: () => void; quotaBusy: boolean;
}) {
  const [open, setOpen] = useState(false);
  const [menuBusy, setMenuBusy] = useState<string | null>(null);
  const [err, setErr] = useState<ApiErrorDetail | null>(null);
  const [flash, setFlash] = useState<string | null>(null);
  const [editing, setEditing] = useState(false);
  const [quotaOpen, setQuotaOpen] = useState(false);
  const [pricingOpen, setPricingOpen] = useState(false);
  const [confirm, setConfirm] = useState<null | { kind: 'account' } | { kind: 'key'; keyID: number; label: string }>(null);

  // The quota button is only live once a probe proved this provider serves
  // /v1/usage: no snapshot or kind "none" both mean the quota read is not available.
  const quotaExposed = a.quota != null && a.quota.kind !== 'none';
  const quotaHint = a.quota == null
    ? 'quota not available — run Test key to check'
    : a.quota.kind === 'none' ? 'quota not available — this provider exposes no usage endpoint' : 'live /v1/usage read';

  const act = async (label: string, fn: () => Promise<unknown>) => {
    setMenuBusy(label); setErr(null); setFlash(null);
    try {
      await fn();
      setFlash(label);
      onChanged();
    } catch (e) {
      if (e instanceof ApiError) setErr(e.detail);
      else setErr({ status: 0, type: 'error', message: e instanceof Error ? e.message : String(e) });
    } finally { setMenuBusy(null); setTimeout(() => setFlash(null), 2500); }
  };

  const removeAccount = async () => {
    setMenuBusy('delete'); setErr(null);
    try {
      await del(`/admin/accounts/${a.id}`);
      onChanged();
    } catch (e) {
      if (e instanceof ApiError) setErr(e.detail);
      else setErr({ status: 0, type: 'error', message: String(e) });
    } finally { setMenuBusy(null); }
  };

  return (
    <div className="rounded-xl border border-line bg-surface transition-colors hover:border-line-strong animate-fade-up">
      <button type="button" className="w-full px-5 py-4 text-left" onClick={() => setOpen((o) => !o)} aria-expanded={open}>
        <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
          <span className="text-[14px] font-semibold">{a.name}</span>
          <Chip><span className="font-mono">{a.namespace}</span></Chip>
          <Chip tone="neutral" className="text-mute"><span className="font-mono">{a.kind}</span></Chip>
          <span className="font-mono text-[11px] text-mute">{a.models_count ?? '–'} models</span>
          {!a.enabled && <Chip tone="warn">disabled</Chip>}
          <span className="ml-auto hidden font-mono text-[11px] text-mute sm:block">{a.base_url}</span>
          <span className={cx('text-mute transition-transform', open && 'rotate-180')}>⌄</span>
        </div>
      </button>

      {/* Quota, slotted under the provider it describes (moved from the
          dashboard's per-account quota panel). The stale pill is GONE by
          request: freshness is now a reload icon that live-probes the
          provider's usage endpoint and refreshes the snapshot — an action,
          not an adjective. The icon glows accent when the snapshot is old. */}
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2 px-5 pb-3">
        <div className="min-w-44 flex-1">
          {a.quota && a.quota.kind !== 'none' && a.quota.kind !== 'unknown'
            ? <QuotaBars quota={a.quota} />
            : <span className="text-[11.5px] text-mute">quota — no probe data yet</span>}
        </div>
        <div className="flex items-center gap-2">
          {a.quota?.kind && a.quota.kind !== 'none' && (
            <Chip tone="neutral"><span className="font-mono">{a.quota.kind}</span></Chip>
          )}
          {a.quota?.read_at && (
            <span className="text-[10.5px] text-mute" title={`snapshot read ${a.quota.read_at}`}>
              read {relTime(a.quota.read_at)}
            </span>
          )}
          <button type="button" disabled={quotaBusy}
            title={stale(a.quota)
              ? 'snapshot is stale — refresh from the provider\u2019s usage endpoint'
              : 'refresh quota from the provider\u2019s usage endpoint'}
            className={cx('rounded-md p-1 text-[13px] leading-none transition-colors disabled:opacity-40',
              stale(a.quota) ? 'text-accent hover:bg-accent-dim' : 'text-mute hover:bg-hover hover:text-ink')}
            onClick={(e) => { e.stopPropagation(); onReloadQuota(); }}>
            {quotaBusy ? <Spinner /> : '↻'}
          </button>
        </div>
      </div>

      {open && (
        <div className="px-5 pb-4 animate-fade-in">
          <div className="rounded-lg border border-line bg-raised/40 px-3.5 py-3">
            <div className="flex flex-wrap items-center gap-2">
              {a.keys.length === 0
                ? <span className="text-[12px] text-mute">no keys — add one when the upstream is confirmed</span>
                : a.keys.map((k) => (
                  <span key={k.id} className="flex items-center gap-2 rounded-full border border-line bg-raised px-2.5 py-1">
                    <Mono className="text-[11.5px]">{k.label} · {k.hint}</Mono>
                    <span className={cx('text-[11px]', k.last_test_ok === true ? 'text-ok' : k.last_test_ok === false ? 'text-bad' : 'text-mute')}
                      title={k.last_test_detail ?? ''}>
                      {k.last_test_ok === true ? '● tested ok' : k.last_test_ok === false ? '● test failed' : 'untested'}
                    </span>
                    {k.last_test_at && <span className="text-[10px] text-mute" title={k.last_test_detail ?? ''}>{relTime(k.last_test_at)}</span>}
                    <button type="button" title="Test key (retest)" disabled={menuBusy != null}
                      className="text-mute transition-colors hover:text-accent disabled:opacity-40"
                      onClick={(e) => { e.stopPropagation(); void act('retest', () => post(`/admin/accounts/${a.id}/keys/${k.id}/retest`, {})); }}>
                      ↻
                    </button>
                    <button type="button" title="Delete key" disabled={menuBusy != null}
                      className="text-mute transition-colors hover:text-bad disabled:opacity-40"
                      onClick={(e) => { e.stopPropagation(); setConfirm({ kind: 'key', keyID: k.id, label: `${k.label} · ${k.hint}` }); }}>
                      ✕
                    </button>
                  </span>
                ))}
            </div>
          </div>

          <div className="mt-3 flex flex-wrap items-center gap-2">
            <button type="button" className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[12px]')} disabled={menuBusy != null}
              onClick={(e) => { e.stopPropagation(); void act('sync', () => post(`/admin/accounts/${a.id}/sync`, {})); }}>
              {menuBusy === 'sync' ? <Spinner /> : '⇄'} Sync models
            </button>
            <button type="button" className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[12px]')} disabled={menuBusy != null || a.keys.length === 0}
              onClick={(e) => { e.stopPropagation(); void act('retest', () => {
                const k = a.keys[0];
                return post(`/admin/accounts/${a.id}/keys/${k.id}/retest`, {});
              }); }}>
              {menuBusy === 'retest' ? <Spinner /> : null} Test key
            </button>
            <span title={quotaHint}>
              <button type="button" className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[12px]')}
                disabled={!quotaExposed}
                onClick={(e) => { e.stopPropagation(); setQuotaOpen(true); }}>
                Quota
              </button>
            </span>
            <button type="button" title="Set per-model USD rates for cost analysis" className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[12px]')}
              onClick={(e) => { e.stopPropagation(); setPricingOpen(true); }}>
              Pricing
            </button>
            <button type="button" className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[12px]')}
              onClick={(e) => { e.stopPropagation(); setEditing(true); }}>
              Edit
            </button>
            <button type="button" className={cx(btn.base, btn.danger, 'px-3 py-1.5 text-[12px]')}
              onClick={(e) => { e.stopPropagation(); setConfirm({ kind: 'account' }); }}>
              Delete
            </button>
            {flash && <span className="text-[11.5px] text-ok">{flash} ✓</span>}
            {menuBusy && <span className="text-[11.5px] text-mute">{menuBusy}…</span>}
          </div>
          {editing && <EditAccountModal account={a} onClose={() => setEditing(false)} onSaved={onChanged} />}
          <ConfirmModal
            open={confirm != null}
            title={confirm?.kind === 'key' ? `Delete key ${confirm.label}?` : `Delete provider ${a.name}?`}
            body={confirm?.kind === 'key'
              ? 'This key is removed immediately. The provider stays, and routes using it will fail until you add a key.'
              : 'This removes the provider and all its keys. Combos that use it will lose that hop.'}
            busy={menuBusy != null}
            onClose={() => setConfirm(null)}
            onConfirm={() => {
              const c = confirm; setConfirm(null);
              if (!c) return;
              if (c.kind === 'key') void act('del key', () => del(`/admin/accounts/${a.id}/keys/${c.keyID}`));
              else void removeAccount();
            }} />
          {quotaOpen && <QuotaModal account={a} onClose={() => setQuotaOpen(false)} />}
          {pricingOpen && <PricingModal account={a} onClose={() => setPricingOpen(false)} />}
          <ErrBox e={err} onDismiss={() => setErr(null)} />
        </div>
      )}

      {open && <ModelsDrawer id={a.id} />}
    </div>
  );
}

// ── page ────────────────────────────────────────────────────────────────

export default function Providers() {
  const [accounts, setAccounts] = useState<Account[] | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [addOpen, setAddOpen] = useState(false);
  const [quotaBusy, setQuotaBusy] = useState<number | null>(null);

  const load = useCallback((silent = true) => {
    get<Account[]>('/admin/accounts')
      .then((a) => { setAccounts(a); setErr(null); })
      .catch((e) => { if (!silent || e instanceof AuthError) setErr(e instanceof Error ? e.message : 'failed to load'); });
  }, []);

  // The reload icon's action: live-probe this provider's usage endpoint
  // (GET /admin/accounts/{id}/quota persists the snapshot server-side), then
  // one silent reload brings the bars up to date. Failures keep the old
  // snapshot — the icon just stops spinning.
  const reloadQuota = useCallback(async (id: number) => {
    setQuotaBusy(id);
    try { await get(`/admin/accounts/${id}/quota`); } catch { /* keep old snapshot */ }
    finally { load(true); setQuotaBusy(null); }
  }, [load]);

  useEffect(() => { load(false); }, [load]);

  return (
    <div className="animate-fade-up">
      <div className="mb-4 flex items-center justify-between">
        <SectionLabel>Accounts</SectionLabel>
        <button type="button" className={cx(btn.base, btn.primary)} onClick={() => setAddOpen(true)}>
          <span className="text-[15px] leading-none">＋</span> Add provider
        </button>
      </div>

      {err && <div className="mb-4 rounded-lg border border-bad/30 bg-bad-dim px-4 py-3 text-[13px] text-bad">{err}</div>}

      {accounts === null ? (
        <SkeletonCards n={3} />
      ) : accounts.length === 0 ? (
        <div className="rounded-xl border border-line bg-surface px-6 py-14 text-center">
          <p className="text-[14px] font-medium">No providers yet</p>
          <p className="mt-1 text-[13px] text-mute">Add your first one — name it, drop in an API key, and the 7-step test runs automatically.</p>
          <button type="button" className={cx(btn.base, btn.primary, 'mt-5')} onClick={() => setAddOpen(true)}>＋ Add your first provider</button>
        </div>
      ) : (
        <div className="space-y-3">
          {/* Alphabetical by namespace — the add-provider preset list sorts the
              same way, so the two lists read in one order. */}
          {[...accounts].sort((a, b) => a.namespace.localeCompare(b.namespace)).map((a) => (
            <AccountCard key={a.id} a={a} onChanged={() => load(true)}
              quotaBusy={quotaBusy === a.id}
              onReloadQuota={() => void reloadQuota(a.id)} />
          ))}
        </div>
      )}

      <AddProviderModal open={addOpen} onClose={() => setAddOpen(false)} onDone={() => { setAddOpen(false); load(true); }} />
      {/* edit modal is created per-card when needed */}
    </div>
  );
}
