// PROVIDERS — account cards, add-provider wizard modal, per-key actions, model drawer.
import { useCallback, useEffect, useState } from 'react';
import {
  ApiError, AuthError, del, get, modelRules, patch, post, setModelPin,
  setPrice, deletePrice,
  KINDS, STEPS,
  type Account, type ApiErrorDetail, type KeyRow, type ModelRule, type ModelRow, type Price, type TestResult, type TestStep,
} from '../api';
import { Chip, ConfirmModal, Modal, SectionLabel, Skeleton, SkeletonCards, Spinner, Toggle, btn, cx, inputCls, labelCls, relTime, Mono } from '../ui';

// ── helpers ─────────────────────────────────────────────────────────────

const slugify = (s: string) =>
  s.toLowerCase().replace(/[^a-z0-9._-]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 63);

function ErrBox({ e, onDismiss }: { e: ApiErrorDetail | null; onDismiss: () => void }) {
  if (!e) return null;
  return (
    <div className="rounded-lg border border-bad/30 bg-[rgba(248,113,113,0.06)] px-4 py-3 animate-fade-in">
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
              state === 'ok' && 'border-ok/40 bg-[rgba(52,211,153,0.1)] text-ok',
              state === 'bad' && 'border-bad/40 bg-[rgba(248,113,113,0.1)] text-bad',
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

type Draft = {
  name: string; namespace: string; nsTouched: boolean; kind: string; preset: string;
  base_url: string; keyLabel: string; apiKey: string;
  probe_delay_ms: number; notes: string; skipInference: boolean;
  advOpen: boolean; requiresSessionHeader: boolean; customHeaders: string;
};

const emptyDraft: Draft = {
  name: '', namespace: '', nsTouched: false, kind: 'opencode-go', preset: 'opencode-go',
  base_url: 'https://opencode.ai/zen/go/v1', keyLabel: 'primary', apiKey: '',
  probe_delay_ms: 400, notes: '', skipInference: false,
  advOpen: false, requiresSessionHeader: true, customHeaders: '',
};

const PROVIDER_PRESETS: Record<string, { label: string; kind: string; baseURL?: string; sessionHeader?: boolean }> = {
  'claude-platform': { label: 'Claude Platform', kind: 'anthropic-compatible', baseURL: 'https://api.anthropic.com' },
  'opencode-go': { label: 'OpenCode Go', kind: 'opencode-go', baseURL: 'https://opencode.ai/zen/go/v1', sessionHeader: true },
  'xiaomi-mimo-token-plan': { label: 'Xiaomi MiMo token plan', kind: 'openai-compatible', baseURL: 'https://token-plan-sgp.xiaomimimo.com/v1' },
  'alibaba-model-studio-token-plan': { label: 'Alibaba Cloud Model Studio token plan', kind: 'openai-compatible', baseURL: 'https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1' },
  'commandcode': { label: 'Command Code', kind: 'openai-compatible', baseURL: 'https://api.commandcode.ai/provider/v1' },
  'deepseek': { label: 'DeepSeek', kind: 'openai-compatible', baseURL: 'https://api.deepseek.com' },
  'groq': { label: 'Groq', kind: 'openai-compatible', baseURL: 'https://api.groq.com/openai/v1' },
  'tokenrouter': { label: 'TokenRouter', kind: 'openai-compatible', baseURL: 'https://api.tokenrouter.com/v1' },
  'gemini': { label: 'Gemini (API key)', kind: 'gemini-openai', baseURL: 'https://generativelanguage.googleapis.com/v1beta/openai' },
  'openai-compatible': { label: 'OpenAI-compatible', kind: 'openai-compatible' },
  'anthropic-compatible': { label: 'Anthropic-compatible', kind: 'anthropic-compatible' },
};
const PRESET_IDS = Object.keys(PROVIDER_PRESETS);

// Endpoints the gateway already knows: every preset plus the two big public
// APIs. Once an account points at one of these, the base URL is fixed — editing
// it only ever introduces a typo, and the adapter owns the path suffixes anyway.
const KNOWN_ENDPOINTS = new Set<string>([
  ...PRESET_IDS.map((p) => PROVIDER_PRESETS[p].baseURL ?? '').filter(Boolean),
  'https://api.openai.com/v1',
  'https://api.anthropic.com',
]);
const isKnownEndpoint = (url: string) => KNOWN_ENDPOINTS.has(url.replace(/\/+$/, ''));

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
              setD((p) => ({ ...p, preset: presetID, kind: preset.kind, base_url: preset.baseURL ?? '', requiresSessionHeader: preset.sessionHeader ?? false }));
            }}>
              {PRESET_IDS.map((presetID) => <option key={presetID} value={presetID}>{PROVIDER_PRESETS[presetID].label}</option>)}
            </select>
          </div>
          <div>
            <label className={labelCls} htmlFor="ap-delay">Probe delay (ms)</label>
            <input id="ap-delay" type="number" min={0} className={cx(inputCls, 'font-mono tabular-nums')} value={d.probe_delay_ms}
              onChange={(e) => set('probe_delay_ms', Number(e.target.value) || 0)} />
          </div>
        </div>
        {isCompat && !isKnownEndpoint(d.base_url) ? (
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
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<ApiErrorDetail | null>(null);

  useEffect(() => {
    if (account) { setName(account.name); setKind(account.kind); setBaseUrl(account.base_url); setProbeDelay(account.probe_delay_ms ?? 400); setNotes(account.notes ?? ''); setErr(null); }
  }, [account]);

  if (!account) return null;

  const save = async () => {
    setBusy(true); setErr(null);
    try {
      await patch<Account>(`/admin/accounts/${account.id}`, {
        name: name.trim(), kind, base_url: baseUrl.trim(), probe_delay_ms: probeDelay, notes: notes.trim(),
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
        <ErrBox e={err} onDismiss={() => setErr(null)} />
      </div>
    </Modal>
  );
}

// ── models drawer ───────────────────────────────────────────────────────

const PIN_SURFACES = ['openai', 'anthropic', 'responses'] as const;

function ModelsDrawer({ id }: { id: number }) {
  const [models, setModels] = useState<ModelRow[] | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [pinErr, setPinErr] = useState<string | null>(null);
  // A surface the probe never confirmed needs an explicit override. Held here
  // so the prompt is per-row and cannot be bypassed by a stale select value.
  const [pendingForce, setPendingForce] = useState<{ model: string; pin: string } | null>(null);

  useEffect(() => {
    let alive = true;
    get<ModelRow[]>(`/admin/accounts/${id}/models`)
      .then((m) => { if (alive) setModels(m); })
      .catch((e) => { if (alive) setErr(e instanceof Error ? e.message : 'failed to load models'); });
    return () => { alive = false; };
  }, [id]);

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
    const confirmed = pin === 'openai' ? m.openai : pin === 'anthropic' ? m.anthropic : m.responses;
    if (!confirmed) return setPendingForce({ model: m.id, pin });
    applyPin(m.id, pin, false);
  };

  return (
    <div className="border-t border-line px-5 py-4 animate-fade-in">
      <div className="max-h-64 overflow-y-auto pr-1">
        <table className="w-full text-[12px]">
          <thead>
            <tr className="text-left text-[10px] uppercase tracking-[0.1em] text-mute">
              <th className="pb-1.5 font-medium">model</th>
              <th className="pb-1.5 font-medium">response type</th>
            </tr>
          </thead>
          <tbody className="font-mono">
            {models.map((m) => (
              <tr key={m.id} className="border-t border-line/50">
                <td className="max-w-[220px] truncate py-1.5 text-dim" title={m.id}>{m.id}</td>
                <td className="py-1.5">
                  <select
                    aria-label={`response type for ${m.id}`}
                    value={m.pin ?? ''}
                    onChange={(e) => onPinChange(m, e.target.value)}
                    className="rounded border border-line bg-transparent px-1.5 py-0.5 text-[11.5px] text-dim"
                  >
                    <option value="">auto (probed)</option>
                    {PIN_SURFACES.map((s) => {
                      const ok = s === 'openai' ? m.openai : s === 'anthropic' ? m.anthropic : m.responses;
                      return <option key={s} value={s}>{s}{ok ? '' : ' (unconfirmed)'}</option>;
                    })}
                  </select>
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

function AccountCard({ a, onChanged }: { a: Account; onChanged: () => void }) {
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
        {a.quota?.kind && a.quota.kind !== 'none' && (
          <div className="mt-2.5 flex flex-wrap items-center gap-x-4 gap-y-1.5 text-[11.5px] text-mute">
            <span>quota <span className="font-mono text-dim">{a.quota.kind}</span></span>
            {a.quota.read_at && <span>read {relTime(a.quota.read_at)}</span>}
          </div>
        )}
      </button>

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

  const load = useCallback((silent = true) => {
    get<Account[]>('/admin/accounts')
      .then((a) => { setAccounts(a); setErr(null); })
      .catch((e) => { if (!silent || e instanceof AuthError) setErr(e instanceof Error ? e.message : 'failed to load'); });
  }, []);

  useEffect(() => { load(false); }, [load]);

  return (
    <div className="animate-fade-up">
      <div className="mb-4 flex items-center justify-between">
        <SectionLabel>Accounts</SectionLabel>
        <button type="button" className={cx(btn.base, btn.primary)} onClick={() => setAddOpen(true)}>
          <span className="text-[15px] leading-none">＋</span> Add provider
        </button>
      </div>

      {err && <div className="mb-4 rounded-lg border border-bad/30 bg-[rgba(248,113,113,0.06)] px-4 py-3 text-[13px] text-bad">{err}</div>}

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
          {accounts.map((a) => <AccountCard key={a.id} a={a} onChanged={() => load(true)} />)}
        </div>
      )}

      <AddProviderModal open={addOpen} onClose={() => setAddOpen(false)} onDone={() => { setAddOpen(false); load(true); }} />
      {/* edit modal is created per-card when needed */}
    </div>
  );
}
