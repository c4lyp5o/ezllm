// SERVICES — endpoint-level defaults, with surface-specific evidence.
// The default applies only when a client omits `model`; explicit model always wins.
import { useCallback, useEffect, useMemo, useState } from 'react';
import { Link } from 'react-router-dom';
import {
  accountModels, get, serviceCards, setServiceBinding, testService,
  type Account, type ModelRow, type ServiceCard, type ServiceMode, type ServiceSurface,
} from '../utils/api';
import { Chip, Combobox, EmptyState, Spinner, btn, cx, labelCls } from '../components/ui';

type CatalogModel = ModelRow & { id: string; display: string };

// Per-surface test-prompt defaults: what the operator sees pre-filled in the
// card, matching the backend's serviceProbeBody defaults exactly (empty prompt
// = default on the wire). audio_stt takes a file, not text — no prompt row.
const PROMPT_DEFAULTS: Record<ServiceSurface, string> = {
  embeddings: 'ping',
  images_gen: 'a small red circle',
  audio_tts: 'ok',
  audio_stt: '',
};
const PROMPT_LABELS: Record<ServiceSurface, string> = {
  embeddings: 'Test text',
  images_gen: 'Prompt to generate',
  audio_tts: 'Text to speak',
  audio_stt: 'Test payload',
};

function ServiceIcon({ surface }: { surface: ServiceSurface }) {
  const common = {
    viewBox: '0 0 24 24', className: 'h-4 w-4', fill: 'none', stroke: 'currentColor',
    strokeWidth: 1.7, strokeLinecap: 'round' as const, strokeLinejoin: 'round' as const,
    'aria-hidden': true,
  };
  switch (surface) {
    case 'embeddings':
      return <svg {...common}><circle cx="6" cy="6" r="2" /><circle cx="18" cy="6" r="2" /><circle cx="6" cy="18" r="2" /><circle cx="18" cy="18" r="2" /><path d="M8 6h8M6 8v8M18 8v8M8 18h8" /></svg>;
    case 'images_gen':
      return <svg {...common}><rect x="3" y="4" width="18" height="16" rx="2" /><circle cx="9" cy="10" r="2" /><path d="m21 15-4-4-5 5-4-4-5 5" /></svg>;
    case 'audio_tts':
      return <svg {...common}><path d="M4 9v6M8 6v12M12 4v16M16 7v10M20 10v4" /></svg>;
    default:
      return <svg {...common}><rect x="9" y="3" width="6" height="12" rx="3" /><path d="M5 11a7 7 0 0 0 14 0M12 18v3M8 21h8" /></svg>;
  }
}

function StateChip({ card }: { card: ServiceCard }) {
  if (card.mode === 'off') return <Chip tone="neutral">Not configured</Chip>;
  if (card.mode === 'auto' && !card.resolved_target) return <Chip tone="warn">Waiting for a successful test</Chip>;
  if (!card.target_exists) return <Chip tone="bad">Selected model unavailable</Chip>;
  if (card.verdict?.ok) return <Chip tone="ok">Tested · works on this endpoint</Chip>;
  if (card.verdict && !card.verdict.ok) return <Chip tone="bad">Last test failed</Chip>;
  return <Chip tone="warn">Configured · not tested</Chip>;
}

function ServiceCardView({
  card, models, loadingModels, onSaved, onTested,
}: {
  card: ServiceCard;
  models: CatalogModel[];
  loadingModels: boolean;
  onSaved: (card: ServiceCard) => void;
  onTested: () => void;
}) {
  const [mode, setMode] = useState<ServiceMode>(card.mode);
  const [target, setTarget] = useState(card.model_target || card.resolved_target || '');
  const [busy, setBusy] = useState(false);
  const [testing, setTesting] = useState(false);
  const [message, setMessage] = useState('');
  const [error, setError] = useState('');
  const [prompt, setPrompt] = useState(PROMPT_DEFAULTS[card.surface]);
  const [testResult, setTestResult] = useState<{ ok: boolean; dispatched: boolean; detail: string; status: number; ms: number } | null>(null);

  useEffect(() => {
    setMode(card.mode);
    setTarget(card.model_target || card.resolved_target || '');
    setPrompt(PROMPT_DEFAULTS[card.surface]);
  }, [card.mode, card.model_target, card.resolved_target, card.surface]);

  const options = useMemo(() => models.map((m) => ({ value: m.id, label: m.display })), [models]);
  const hasProviders = card.accounts > 0;
  const selectedIsListed = models.some((m) => m.id === target);
  const dirty = mode !== card.mode || (mode === 'manual' && target !== card.model_target);

  const save = async () => {
    setBusy(true); setError(''); setMessage('');
    try {
      const updated = await setServiceBinding(card.surface, mode, mode === 'manual' ? target : '');
      onSaved(updated);
      setMessage(mode === 'off' ? 'Default cleared.' : mode === 'auto' ? 'Auto mode enabled.' : 'Default model saved.');
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Could not save this service default.');
    } finally { setBusy(false); }
  };

  const runTest = async () => {
    if (!target) return;
    if (card.surface === 'images_gen' && !window.confirm('Image generation tests can use provider quota or incur a charge. Send one small test image request?')) return;
    setTesting(true); setError(''); setMessage(''); setTestResult(null);
    try {
      const result = await testService(card.surface, target, prompt.trim());
      setTestResult({ ok: result.ok, dispatched: result.dispatched, detail: result.detail, status: result.status, ms: result.ms });
      onTested();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'The endpoint test could not be recorded.');
    } finally { setTesting(false); }
  };

  return (
    <section className="rounded-xl border border-line bg-surface p-5" aria-labelledby={`service-${card.surface}`}>
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <span className="flex h-7 w-7 items-center justify-center rounded-md border border-line bg-raised text-accent">
              <ServiceIcon surface={card.surface} />
            </span>
            <h2 id={`service-${card.surface}`} className="text-[15px] font-semibold">{card.label}</h2>
            <code className="rounded border border-line bg-raised px-2 py-0.5 font-mono text-[11px] text-dim">{card.endpoint}</code>
          </div>
          <p className="mt-1.5 max-w-2xl text-[12.5px] leading-relaxed text-mute">{card.description}</p>
        </div>
        <StateChip card={card} />
      </div>

      {!hasProviders ? (
        <div className="mt-4 flex flex-wrap items-center justify-between gap-3 rounded-lg border border-line bg-raised px-4 py-3">
          <div>
            <p className="text-[13px] font-medium text-ink">No providers connected</p>
            <p className="mt-0.5 text-[11.5px] text-mute">Connect a provider and sync its model catalog before choosing a default.</p>
          </div>
          <Link to="/providers" className={cx(btn.base, btn.ghost)}>Add provider</Link>
        </div>
      ) : (
        <div className="mt-5 grid gap-4 lg:grid-cols-[minmax(0,1fr)_minmax(220px,0.7fr)]">
          <div>
            <label className={labelCls} htmlFor={`mode-${card.surface}`}>Default behavior</label>
            <select id={`mode-${card.surface}`} className="w-full rounded-lg border border-line bg-raised px-3 py-2 text-[13px] text-ink focus:border-accent focus:outline-none"
              value={mode} onChange={(e) => setMode(e.target.value as ServiceMode)}>
              <option value="off">Off — require model in each request</option>
              <option value="manual">Manual default — use this model when omitted</option>
              <option value="auto">Auto — choose the most recently tested model</option>
            </select>
            {mode === 'manual' && (
              <div className="mt-3">
                <label className={labelCls} htmlFor={`model-${card.surface}`}>Default model for {card.endpoint}</label>
                {models.length === 0 && !loadingModels ? (
                  <div className="rounded-lg border border-line bg-raised px-3 py-2.5 text-[12px] text-mute">
                    No synced models yet. Open Providers, sync a provider’s catalog, then come back here.
                  </div>
                ) : (
                  <Combobox idPrefix={`service-${card.surface}`} options={options} value={target}
                    onChange={setTarget} loadingText={loadingModels ? 'Loading provider models…' : 'No synced models'}
                    disabled={loadingModels || options.length === 0} placeholder="Search provider models…" />
                )}
                {target && !selectedIsListed && <p className="mt-1 text-[11px] text-warn">The saved target is not in the current enabled-provider catalog.</p>}
              </div>
            )}
            {mode === 'auto' && (
              <p className="mt-3 rounded-lg border border-line bg-raised px-3 py-2.5 text-[11.5px] leading-relaxed text-mute">
                Auto uses only models that passed a real test for this exact endpoint. It does not use chat compatibility checks. Currently verified: <strong className="font-mono text-ink">{card.verified_models}</strong> model{card.verified_models === 1 ? '' : 's'}.
              </p>
            )}
            {mode === 'off' && <p className="mt-3 text-[11.5px] text-mute">Requests must include their own <code className="font-mono">model</code> value. This is the safest first-run state.</p>}
          </div>

          <div className="flex flex-col justify-between gap-4 rounded-lg border border-line bg-raised p-3.5">
            <div>
              <p className="text-[10.5px] font-medium uppercase tracking-[0.08em] text-mute">Effective default</p>
              <p className="mt-1 break-all font-mono text-[12px] text-ink">{card.resolved_target || (mode === 'manual' ? target : '') || 'None'}</p>
              {card.verdict && <p className="mt-1 text-[10.5px] text-mute">Tested {new Date(card.verdict.tested_at).toLocaleString()}</p>}
            </div>
            <div className="flex flex-wrap gap-2">
              {mode === 'manual' && (
                <button type="button" onClick={runTest} disabled={!target || testing || busy || loadingModels}
                  className={cx(btn.base, btn.ghost, 'disabled:cursor-not-allowed disabled:opacity-50')}>
                  {testing ? <><Spinner /> Testing…</> : 'Test endpoint'}
                </button>
              )}
              <button type="button" onClick={save} disabled={busy || testing || (mode === 'manual' && (!target || !selectedIsListed)) || !dirty}
                className={cx(btn.base, btn.primary, 'disabled:cursor-not-allowed disabled:opacity-50')}>
                {busy ? <><Spinner /> Saving…</> : 'Save default'}
              </button>
            </div>
            {mode === 'manual' && (
              <div className="border-t border-line pt-3">
                <label className={labelCls} htmlFor={`prompt-${card.surface}`}>{PROMPT_LABELS[card.surface]}</label>
                {card.surface === 'audio_stt' ? (
                  <p className="text-[11px] leading-relaxed text-mute">
                    This endpoint tests a file upload, not text — the test always sends a fixed 0.2 s silent WAV.
                  </p>
                ) : (
                  <>
                    <input id={`prompt-${card.surface}`} type="text" maxLength={2000} value={prompt}
                      onChange={(e) => setPrompt(e.target.value)} placeholder={PROMPT_DEFAULTS[card.surface]}
                      disabled={testing || busy}
                      className="w-full rounded-lg border border-line bg-surface px-3 py-2 font-mono text-[12px] text-ink placeholder:text-mute focus:border-accent focus:outline-none disabled:opacity-50" />
                    <p className="mt-1 text-[10.5px] leading-relaxed text-mute">
                      Sent verbatim in the test request; empty = the default. Not saved — only the model default persists.
                    </p>
                  </>
                )}
              </div>
            )}
            {card.surface === 'images_gen' && <p className="text-[10.5px] leading-relaxed text-warn">Image tests may spend provider quota. You will be asked before each test.</p>}
          </div>
        </div>
      )}

      {(message || error || testResult) && (
        <div className="mt-4 space-y-2" aria-live="polite">
          {message && <p className="text-[12px] text-ok">{message}</p>}
          {error && <p className="rounded-lg border border-bad/30 bg-bad-dim px-3 py-2 text-[12px] text-bad">{error}</p>}
          {testResult && <p className={cx('rounded-lg border px-3 py-2 text-[12px]',
            testResult.dispatched ? (testResult.ok ? 'border-ok/30 bg-ok-dim text-ok' : 'border-bad/30 bg-bad-dim text-bad') : 'border-warn/30 bg-warn-dim text-warn')}>
            {!testResult.dispatched
              ? 'Test did not reach the provider — nothing was recorded as tested.'
              : testResult.ok ? 'Endpoint test succeeded' : `Endpoint test failed (HTTP ${testResult.status})`}
            {' · '}{testResult.ms} ms{testResult.detail ? ` · ${testResult.detail}` : ''}
          </p>}
        </div>
      )}
    </section>
  );
}

export default function Services() {
  const [cards, setCards] = useState<ServiceCard[]>([]);
  const [models, setModels] = useState<CatalogModel[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadingModels, setLoadingModels] = useState(true);
  const [error, setError] = useState('');

  useEffect(() => {
    let live = true;
    (async () => {
      try {
        const result = await serviceCards();
        if (live) setCards(result.services);
      } catch (e) {
        if (live) setError(e instanceof Error ? e.message : 'Could not load services.');
      } finally { if (live) setLoading(false); }
      try {
        const accounts = await get<Account[]>('/admin/accounts');
        const flat: CatalogModel[] = [];
        for (const account of accounts.filter((a) => a.enabled)) {
          const rows = await accountModels(account.id);
          for (const model of rows) flat.push({
            ...model,
            id: `${account.namespace}/${model.id}`,
            display: `${account.namespace} / ${model.display ?? model.id}`,
          });
        }
        if (live) setModels(flat);
      } catch (e) {
        if (live) setError(e instanceof Error ? e.message : 'Could not load provider models.');
      } finally { if (live) setLoadingModels(false); }
    })();
    return () => { live = false; };
  }, []);

  const onSaved = useCallback((updated: ServiceCard) => {
    setCards((prev) => prev.map((c) => c.surface === updated.surface ? updated : c));
  }, []);
  const onTested = useCallback(() => {
    // The POST records the probe; refetch to show the true server verdict and
    // the real Auto target. No guessed local success state.
    serviceCards().then((result) => setCards(result.services))
      .catch((e: unknown) => setError(e instanceof Error ? e.message : 'Could not refresh service status.'));
  }, []);

  if (loading) return <div className="flex items-center gap-2 py-12 text-[13px] text-mute"><Spinner /> Loading service settings…</div>;
  if (error && cards.length === 0) return <EmptyState title="Could not load Services" hint={error} />;

  return (
    <div className="mx-auto max-w-6xl space-y-5">
      <div className="rounded-xl border border-line bg-surface px-5 py-4">
        <h2 className="text-[14px] font-semibold">Choose endpoint defaults</h2>
        <p className="mt-1 text-[12px] leading-relaxed text-mute">
          Each setting is a default for its endpoint only. A request that includes <code className="font-mono text-ink">model</code> always uses that explicit choice. If nothing is configured, the endpoint stays available but returns a clear <code className="font-mono text-ink">service_not_configured</code> error instead of sending a blind upstream request.
        </p>
        {models.length > 0 && <p className="mt-2 text-[11px] text-mute">Available models: <span className="font-mono text-dim">{models.length}</span> from enabled providers. Chat protocol checks do not verify image, audio, or embedding support.</p>}
      </div>

      {error && <p className="rounded-lg border border-bad/30 bg-bad-dim px-3 py-2 text-[12px] text-bad" role="alert">{error}</p>}
      <div className="grid gap-4 xl:grid-cols-2">
        {cards.map((card) => <ServiceCardView key={card.surface} card={card} models={models}
          loadingModels={loadingModels} onSaved={onSaved} onTested={onTested} />)}
      </div>
    </div>
  );
}
