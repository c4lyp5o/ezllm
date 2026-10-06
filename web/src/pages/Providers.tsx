// PROVIDERS — account cards, add-provider wizard modal, per-key actions, model drawer.
import { useCallback, useEffect, useState } from 'react';
import {
  ApiError, AuthError, del, get, patch, post,
  KINDS, STEPS,
  type Account, type ApiErrorDetail, type KeyRow, type ModelRow, type ProtocolSupport, type TestResult, type TestStep,
} from '../api';
import { Chip, Modal, SectionLabel, Skeleton, SkeletonCards, Spinner, Toggle, btn, cx, inputCls, labelCls, relTime, Mono } from '../ui';

// ── helpers ─────────────────────────────────────────────────────────────

const slugify = (s: string) =>
  s.toLowerCase().replace(/[^a-z0-9._-]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 63);

const protoState = (v: boolean | number | null | undefined): 'yes' | 'no' | 'unknown' =>
  v === true || v === 1 ? 'yes' : v === false || v === 0 ? 'no' : 'unknown';

const supportCount = (ps: ProtocolSupport | null | undefined, k: string) =>
  (ps && typeof (ps as Record<string, unknown>)[k] === 'number' ? (ps as Record<string, number>)[k] : null);

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

function StepList({ steps, running }: { steps: TestStep[] | null; running: boolean }) {
  const byStep = new Map((steps ?? []).map((s) => [s.step, s]));
  return (
    <ol className="space-y-1.5">
      {STEP_ORDER.map((name, idx) => {
        const s = byStep.get(name);
        const state = s ? (s.ok ? 'ok' : 'bad') : running && idx <= (byStep.size ?? 0) ? 'pending' : 'idle';
        return (
          <li key={name} className="flex items-center gap-3 font-mono text-[12px]">
            <span className={cx('flex h-5 w-5 shrink-0 items-center justify-center rounded-full border text-[10px]',
              state === 'ok' && 'border-ok/40 bg-[rgba(52,211,153,0.1)] text-ok',
              state === 'bad' && 'border-bad/40 bg-[rgba(248,113,113,0.1)] text-bad',
              state === 'pending' && 'border-line text-mute',
              state === 'idle' && 'border-line/60 text-mute/50')}>
              {state === 'ok' ? '✓' : state === 'bad' ? '✕' : idx + 1}
            </span>
            <span className={cx(state === 'ok' ? 'text-dim' : state === 'bad' ? 'text-bad' : 'text-mute')}>{name}</span>
            {s?.detail && <span className="truncate text-mute" title={s.detail}>{s.detail}</span>}
            {s?.ms != null && <span className="ml-auto shrink-0 tabular-nums text-mute/70">{s.ms}ms</span>}
          </li>
        );
      })}
    </ol>
  );
}

// ── add provider modal ──────────────────────────────────────────────────

type Draft = {
  name: string; namespace: string; nsTouched: boolean; kind: string; base_url: string;
  probe_delay_ms: number; notes: string;
  keyLabel: string; apiKey: string; skipInference: boolean;
  advOpen: boolean; requiresSessionHeader: boolean; customHeaders: string;
  quotaMode: string; capWindow: string; capTokens: number;
};

const emptyDraft: Draft = {
  name: '', namespace: '', nsTouched: false, kind: 'openai-compatible', base_url: '',
  probe_delay_ms: 400, notes: '',
  keyLabel: 'primary', apiKey: '', skipInference: false,
  advOpen: false, requiresSessionHeader: false, customHeaders: '',
  quotaMode: 'probe', capWindow: 'monthly', capTokens: 0,
};

function AddProviderModal({ open, onClose, onDone }: { open: boolean; onClose: () => void; onDone: () => void }) {
  const [step, setStep] = useState<1 | 2>(1);
  const [d, setD] = useState<Draft>(emptyDraft);
  const [busy, setBusy] = useState(false);
  const [createdId, setCreatedId] = useState<number | null>(null);
  const [result, setResult] = useState<TestResult | null>(null);
  const [err, setErr] = useState<ApiErrorDetail | null>(null);

  useEffect(() => {
    if (open) { setStep(1); setD(emptyDraft); setBusy(false); setCreatedId(null); setResult(null); setErr(null); }
  }, [open]);

  const set = <K extends keyof Draft>(k: K, v: Draft[K]) => setD((p) => ({ ...p, [k]: v }));

  const createAccount = async (): Promise<number | null> => {
    if (createdId != null) return createdId;
    const customHeaders = d.customHeaders.trim() ? JSON.parse(d.customHeaders) as Record<string, unknown> : null;
    const body: Record<string, unknown> = {
      name: d.name.trim(), namespace: d.namespace.trim(), kind: d.kind,
      base_url: d.base_url.trim() || null, probe_delay_ms: d.probe_delay_ms,
      requires_session_header: d.requiresSessionHeader, custom_headers: customHeaders,
      notes: d.notes.trim(),
    };
    if (d.quotaMode !== 'probe') {
      body.quota_mode = d.quotaMode;
      if (d.quotaMode === 'tokens') { body.cap_window = d.capWindow; body.cap_tokens = d.capTokens; }
    }
    const acc = await post<Account>('/admin/accounts', body);
    setCreatedId(acc.id);
    return acc.id;
  };

  const testAndAdd = async () => {
    setBusy(true); setErr(null); setResult(null);
    try {
      const id = await createAccount();
      const res = await post<{ key: KeyRow; test: TestResult }>(`/admin/accounts/${id}/keys`, {
        label: d.keyLabel.trim() || 'primary', api_key: d.apiKey, skip_inference: d.skipInference,
      });
      setResult(res.test);
      setTimeout(() => onDone(), 900);
    } catch (e) {
      if (e instanceof ApiError) {
        setErr(e.detail);
        // 409 on create / account exists → surface message, stay on the form
      } else setErr({ status: 0, type: 'error', message: e instanceof Error ? e.message : String(e) });
    } finally { setBusy(false); }
  };

  const step1Valid = d.name.trim().length > 0 && /^[a-z0-9][a-z0-9._-]{0,62}$/.test(d.namespace.trim()) && d.base_url.trim().length > 0;

  return (
    <Modal open={open} onClose={onClose} wide title="Add provider"
      subtitle={step === 1 ? 'account definition — the key is added and tested in the next step' : `testing the key against ${d.name || 'the account'}`}
      footer={step === 1 ? (
        <>
          <button type="button" className={cx(btn.base, btn.ghost)} onClick={onClose}>Cancel</button>
          <button type="button" disabled={!step1Valid} className={cx(btn.base, btn.primary)} onClick={() => setStep(2)}>Next: API key →</button>
        </>
      ) : (
        <>
          <button type="button" className={cx(btn.base, btn.ghost)} disabled={busy} onClick={() => setStep(1)}>← Back</button>
          <button type="button" disabled={busy || d.apiKey.length === 0} className={cx(btn.base, btn.primary)} onClick={() => void testAndAdd()}>
            {busy ? <Spinner /> : null} Test & add
          </button>
        </>
      )}>
      {step === 1 ? (
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
              <p className="mt-1 text-[11px] text-mute">lowercase · <span className="font-mono">^[a-z0-9][a-z0-9._-]{'{0,62}'}$</span></p>
            </div>
          </div>
          <div className="grid gap-4 sm:grid-cols-2">
            <div>
              <label className={labelCls} htmlFor="ap-kind">Kind</label>
              <select id="ap-kind" className={inputCls} value={d.kind} onChange={(e) => set('kind', e.target.value)}>
                {KINDS.map((k) => <option key={k} value={k}>{k}</option>)}
              </select>
            </div>
            <div>
              <label className={labelCls} htmlFor="ap-delay">Probe delay (ms)</label>
              <input id="ap-delay" type="number" min={0} className={cx(inputCls, 'font-mono tabular-nums')} value={d.probe_delay_ms}
                onChange={(e) => set('probe_delay_ms', Number(e.target.value) || 0)} />
            </div>
          </div>
          <div>
            <label className={labelCls} htmlFor="ap-url">Base URL</label>
            <input id="ap-url" className={cx(inputCls, 'font-mono')} value={d.base_url} placeholder="https://provider.example/v1" spellCheck={false}
              onChange={(e) => set('base_url', e.target.value.trim())} />
          </div>
          <div>
            <label className={labelCls} htmlFor="ap-notes">Notes</label>
            <input id="ap-notes" className={inputCls} value={d.notes} onChange={(e) => set('notes', e.target.value)} placeholder="optional" />
          </div>

          <div className="rounded-lg border border-line bg-raised/60">
            <button type="button" className="flex w-full items-center justify-between px-4 py-3 text-[13px] font-medium text-dim hover:text-ink"
              onClick={() => set('advOpen', !d.advOpen)}>
              Advanced
              <span className="text-mute">{d.advOpen ? '−' : '+'}</span>
            </button>
            {d.advOpen && (
              <div className="space-y-4 border-t border-line px-4 py-4">
                <div className="flex items-center gap-3">
                  <Toggle checked={d.requiresSessionHeader} onChange={(v) => set('requiresSessionHeader', v)} label="requires_session_header" />
                  <span className="text-[12.5px] text-dim">requires <span className="font-mono">session</span> header upstream</span>
                </div>
                <div>
                  <label className={labelCls} htmlFor="ap-cH">Custom headers (JSON)</label>
                  <textarea id="ap-cH" rows={3} spellCheck={false} className={cx(inputCls, 'font-mono text-[12px]')} placeholder={'{"x-team":"ops"}'}
                    value={d.customHeaders} onChange={(e) => set('customHeaders', e.target.value)} />
                </div>
                <div className="grid gap-4 sm:grid-cols-3">
                  <div>
                    <label className={labelCls} htmlFor="ap-qm">Quota mode</label>
                    <select id="ap-qm" className={inputCls} value={d.quotaMode} onChange={(e) => set('quotaMode', e.target.value)}>
                      {['probe', 'percent', 'money', 'tokens', 'none'].map((m) => <option key={m} value={m}>{m}</option>)}
                    </select>
                  </div>
                  <div>
                    <label className={labelCls} htmlFor="ap-cw">Cap window</label>
                    <select id="ap-cw" className={inputCls} value={d.capWindow} disabled={d.quotaMode !== 'tokens'} onChange={(e) => set('capWindow', e.target.value)}>
                      {['weekly', 'monthly'].map((w) => <option key={w} value={w}>{w}</option>)}
                    </select>
                  </div>
                  <div>
                    <label className={labelCls} htmlFor="ap-ct">Cap tokens</label>
                    <input id="ap-ct" type="number" min={0} className={cx(inputCls, 'font-mono tabular-nums')} value={d.capTokens}
                      disabled={d.quotaMode !== 'tokens'} onChange={(e) => set('capTokens', Number(e.target.value) || 0)} />
                  </div>
                </div>
              </div>
            )}
          </div>
          <ErrBox e={err} onDismiss={() => setErr(null)} />
        </div>
      ) : (
        <div className="space-y-4">
          <div className="grid gap-4 sm:grid-cols-2">
            <div>
              <label className={labelCls} htmlFor="ap-kl">Key label</label>
              <input id="ap-kl" className={inputCls} value={d.keyLabel} onChange={(e) => set('keyLabel', e.target.value)} placeholder="primary" />
            </div>
            <div>
              <label className={labelCls} htmlFor="ap-key">API key</label>
              <input id="ap-key" type="password" autoComplete="off" spellCheck={false} className={cx(inputCls, 'font-mono')}
                placeholder="sk-…" value={d.apiKey} onChange={(e) => set('apiKey', e.target.value)} />
            </div>
          </div>
          <div className="flex items-center gap-3">
            <Toggle checked={d.skipInference} onChange={(v) => set('skipInference', v)} label="skip inference probe" />
            <span className="text-[12.5px] text-dim">Skip inference probe <span className="text-mute">(saves a token spend)</span></span>
          </div>

          <div className="rounded-lg border border-line bg-raised/60 px-4 py-3">
            <SectionLabel className="mb-2">Key test — format → catalog → auth → quota → inference → protocol</SectionLabel>
            <StepList steps={result?.steps ?? null} running={busy} />
            {result?.ok && <p className="mt-2 font-mono text-[11.5px] text-ok">✓ key stored</p>}
          </div>
          <ErrBox e={err} onDismiss={() => setErr(null)} />
          {err?.step && (
            <p className="text-[12px] text-bad">
              failed at <Chip tone="bad"><span className="font-mono">{err.step}</span></Chip>
              {err.upstream_status ? <span className="ml-2 text-mute">upstream HTTP {err.upstream_status}</span> : null}
            </p>
          )}
        </div>
      )}
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
          <input className={cx(inputCls, 'font-mono')} value={baseUrl} onChange={(e) => setBaseUrl(e.target.value.trim())} />
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

function ModelsDrawer({ id }: { id: number }) {
  const [models, setModels] = useState<ModelRow[] | null>(null);
  const [err, setErr] = useState<string | null>(null);

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

  return (
    <div className="border-t border-line px-5 py-4 animate-fade-in">
      <div className="max-h-64 overflow-y-auto pr-1">
        <table className="w-full text-[12px]">
          <thead>
            <tr className="text-left text-[10px] uppercase tracking-[0.1em] text-mute">
              <th className="pb-1.5 font-medium">model</th>
              <th className="pb-1.5 text-center font-medium">openai</th>
              <th className="pb-1.5 text-center font-medium">anthropic</th>
              <th className="pb-1.5 text-center font-medium">responses</th>
            </tr>
          </thead>
          <tbody className="font-mono">
            {models.map((m) => (
              <tr key={m.id} className="border-t border-line/50">
                <td className="max-w-[260px] truncate py-1.5 text-dim" title={m.id}>{m.id}</td>
                {(['openai', 'anthropic', 'responses'] as const).map((k) => (
                  <td key={k} className="py-1.5 text-center" title={protoState(m[k])}>
                    <span className={protoState(m[k]) === 'yes' ? 'text-ok' : protoState(m[k]) === 'no' ? 'text-bad' : 'text-warn'}>
                      {protoState(m[k]) === 'yes' ? '✓' : protoState(m[k]) === 'no' ? '✕' : '?'}
                    </span>
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

// ── account card ────────────────────────────────────────────────────────

function AccountCard({ a, onChanged }: { a: Account; onChanged: () => void }) {
  const [open, setOpen] = useState(false);
  const [menuBusy, setMenuBusy] = useState<string | null>(null);
  const [err, setErr] = useState<ApiErrorDetail | null>(null);
  const [flash, setFlash] = useState<string | null>(null);
  const [editing, setEditing] = useState(false);

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
          {!a.enabled && <Chip tone="warn">disabled</Chip>}
          <span className="ml-auto hidden font-mono text-[11px] text-mute sm:block">{a.base_url}</span>
          <span className={cx('text-mute transition-transform', open && 'rotate-180')}>⌄</span>
        </div>
        <div className="mt-2.5 flex flex-wrap items-center gap-x-4 gap-y-1.5 text-[11.5px] text-mute">
          <span>{a.keys.length} key{a.keys.length === 1 ? '' : 's'}</span>
          <span>{a.models_count ?? '–'} models</span>
          {(['openai', 'anthropic', 'responses'] as const).map((k) => {
            const n = supportCount(a.protocol_support, k);
            return n == null ? null : (
              <span key={k} className="flex items-center gap-1">
                <span className="text-ok">✓</span><span className="font-mono">{k} {n}</span>
              </span>
            );
          })}
          {a.quota?.kind && a.quota.kind !== 'none' && (
            <span>quota <span className="font-mono text-dim">{a.quota.kind}</span></span>
          )}
        </div>
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
                    {k.last_test_ok === true && <span className="text-[10px] text-ok" title="last test ok">●</span>}
                    {k.last_test_ok === false && <span className="text-[10px] text-bad" title="last test failed">●</span>}
                    {k.last_test_at && <span className="text-[10px] text-mute" title={k.last_test_detail ?? ''}>{relTime(k.last_test_at)}</span>}
                    <button type="button" title="Test key (retest)" disabled={menuBusy != null}
                      className="text-mute transition-colors hover:text-accent disabled:opacity-40"
                      onClick={(e) => { e.stopPropagation(); void act('retest', () => post(`/admin/accounts/${a.id}/keys/${k.id}/retest`, {})); }}>
                      ↻
                    </button>
                    <button type="button" title="Delete key" disabled={menuBusy != null}
                      className="text-mute transition-colors hover:text-bad disabled:opacity-40"
                      onClick={(e) => { e.stopPropagation(); void act('del key', () => del(`/admin/accounts/${a.id}/keys/${k.id}`)); }}>
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
            <button type="button" className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[12px]')}
              onClick={(e) => { e.stopPropagation(); setEditing(true); }}>
              Edit
            </button>
            <button type="button" className={cx(btn.base, btn.danger, 'px-3 py-1.5 text-[12px]')}
              onClick={(e) => { e.stopPropagation(); void removeAccount(); }}>
              Delete
            </button>
            {flash && <span className="text-[11.5px] text-ok">{flash} ✓</span>}
            {menuBusy && <span className="text-[11.5px] text-mute">{menuBusy}…</span>}
          </div>
          {editing && <EditAccountModal account={a} onClose={() => setEditing(false)} onSaved={onChanged} />}
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
