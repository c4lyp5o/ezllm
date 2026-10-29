// SERVICES — endpoint-level defaults, with surface-specific evidence.
// The default applies only when a client omits `model`; explicit model always wins.
//
// The page answers three questions in this order: how are my services doing
// (summary + filter), which one needs me (cards sort failing first), and what
// do I change (one settings row per card). Test outcomes live at page level so
// a result stays on its card after the list refetches, and so "Test all
// configured" can fill every card in one pass.
import { useCallback, useEffect, useMemo, useState, type FormEvent } from 'react';
import { Link, useSearchParams } from 'react-router-dom';
import {
  accountModels, get, serviceCards, setServiceBinding, testService,
  type Account, type ModelRow, type ServiceCard, type ServiceMode, type ServiceSurface,
} from '../utils/api';
import {
  Chip, Combobox, ConfirmModal, EmptyState, Segmented, Spinner, Toggle, btn, cx, inputCls, labelCls, relTime,
} from '../components/ui';
import { Collapse, Stagger, StaggerItem } from '../components/motion';
import { GENERATABLE, ServiceTryIt } from '../components/ServiceTryIt';

type CatalogModel = ModelRow & { id: string; display: string };

// Per-surface test-prompt defaults: what the operator sees pre-filled in the
// card, matching the backend's serviceProbeBody defaults exactly (empty prompt
// = default on the wire). audio_stt takes a file, not text — no prompt row.
const PROMPT_DEFAULTS: Record<ServiceSurface, string> = {
  embeddings: 'ping',
  images_gen: 'a small red circle',
  audio_tts: 'ok',
  audio_stt: '',
  images_edit: 'make it red',
  moderations: 'ping',
  rerank: 'ping',
  videos_gen: 'a short calm tone',
  music_gen: 'a short calm tone',
};
// Tests that generate media can spend real quota: confirmed before each send.
const PAID_TESTS: Partial<Record<ServiceSurface, string>> = {
  images_gen: 'Image generation tests can use provider quota or incur a charge. Send one small test image request?',
  images_edit: 'Image edit tests can use provider quota or incur a charge. Send one small test edit of a 1×1 image?',
  videos_gen: 'Video generation tests can use provider quota or incur a charge. Send one short test request?',
  music_gen: 'Music generation tests can use provider quota or incur a charge. Send one short test request?',
};
const PROMPT_LABELS: Record<ServiceSurface, string> = {
  embeddings: 'Test text',
  images_gen: 'Prompt to generate',
  audio_tts: 'Text to speak',
  audio_stt: 'Test payload',
  images_edit: 'Edit instruction',
  moderations: 'Text to classify',
  rerank: 'Query (ranks two fixed documents)',
  videos_gen: 'Prompt to generate',
  music_gen: 'Prompt to generate',
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
    case 'images_edit':
      return <svg {...common}><rect x="3" y="4" width="14" height="14" rx="2" /><path d="m14 20 6-6-2-2-6 6v2z" /></svg>;
    case 'moderations':
      return <svg {...common}><path d="M12 3 4 6v6c0 4.5 3.4 8.3 8 9 4.6-.7 8-4.5 8-9V6z" /><path d="m9 12 2 2 4-4" /></svg>;
    case 'rerank':
      return <svg {...common}><path d="M4 6h10M4 12h7M4 18h4M17 4v16M14 17l3 3 3-3" /></svg>;
    case 'videos_gen':
      return <svg {...common}><rect x="3" y="6" width="13" height="12" rx="2" /><path d="m16 10 5-3v10l-5-3z" /></svg>;
    case 'music_gen':
      return <svg {...common}><path d="M9 18V5l11-2v13" /><circle cx="6" cy="18" r="3" /><circle cx="17" cy="16" r="3" /></svg>;
    default:
      return <svg {...common}><rect x="9" y="3" width="6" height="12" rx="3" /><path d="M5 11a7 7 0 0 0 14 0M12 18v3M8 21h8" /></svg>;
  }
}

// ── health ────────────────────────────────────────────────────────────────
// One verdict per card, used for the summary, the filter and the sort order:
//   failing - configured, but the model is gone or its last test failed
//   pending - configured, with nothing proving it yet (untested, or Auto still
//             waiting for a first passing test)
//   working - configured and its last test passed
//   off     - not configured (a request must name its own model)
type Health = 'failing' | 'pending' | 'working' | 'off';
const HEALTH_ORDER: Record<Health, number> = { failing: 0, pending: 1, working: 2, off: 3 };

function healthOf(card: ServiceCard): Health {
  if (card.mode === 'off') return 'off';
  if (card.mode === 'manual' && !card.target_exists) return 'failing';
  if (card.verdict && !card.verdict.ok) return 'failing';
  if (card.verdict?.ok) return 'working';
  return 'pending';
}

function StateChip({ card }: { card: ServiceCard }) {
  if (card.mode === 'off') return <Chip tone="neutral">Not configured</Chip>;
  if (card.mode === 'auto' && !card.resolved_target) return <Chip tone="warn">Waiting for a successful test</Chip>;
  if (!card.target_exists) return <Chip tone="bad">Selected model unavailable</Chip>;
  if (card.verdict?.ok) return <Chip tone="ok">Tested · works on this endpoint</Chip>;
  if (card.verdict && !card.verdict.ok) return <Chip tone="bad">Last test failed</Chip>;
  return <Chip tone="warn">Configured · not tested</Chip>;
}

type TestOutcome = { ok: boolean; dispatched: boolean; detail: string; status: number; ms: number; model: string; at: number };

function OutcomeLine({ o }: { o: TestOutcome }) {
  return (
    <p className={cx('rounded-lg border px-3 py-2 text-[13px]',
      o.dispatched ? (o.ok ? 'border-ok/30 bg-ok-dim text-ok' : 'border-bad/30 bg-bad-dim text-bad') : 'border-warn/30 bg-warn-dim text-warn')}>
      {!o.dispatched
        ? 'Test did not reach the provider — nothing was recorded as tested.'
        : o.ok ? 'Endpoint test succeeded' : `Endpoint test failed (HTTP ${o.status})`}
      {' · '}<span className="font-mono">{o.model}</span>{' · '}{o.ms} ms{o.detail ? ` · ${o.detail}` : ''}
    </p>
  );
}

// ── one card ──────────────────────────────────────────────────────────────

function ServiceCardView({
  card, models, loadingModels, outcome, highlighted, busyElsewhere, onSaved, onTest,
}: {
  card: ServiceCard;
  models: CatalogModel[];
  loadingModels: boolean;
  /** The last test run from this page for this service (survives refetches). */
  outcome: TestOutcome | undefined;
  highlighted: boolean;
  /** "Test all" is running: single tests wait their turn. */
  busyElsewhere: boolean;
  onSaved: (card: ServiceCard) => void;
  onTest: (surface: ServiceSurface, model: string, prompt: string) => Promise<void>;
}) {
  const [mode, setMode] = useState<ServiceMode>(card.mode);
  const [target, setTarget] = useState(card.model_target || card.resolved_target || '');
  const [busy, setBusy] = useState(false);
  const [testing, setTesting] = useState(false);
  const [saved, setSaved] = useState(false);
  const [error, setError] = useState('');
  const [prompt, setPrompt] = useState(PROMPT_DEFAULTS[card.surface]);
  const [confirmPaid, setConfirmPaid] = useState(false);
  const [tryOpen, setTryOpen] = useState(false);

  useEffect(() => {
    setMode(card.mode);
    setTarget(card.model_target || card.resolved_target || '');
    setPrompt(PROMPT_DEFAULTS[card.surface]);
  }, [card.mode, card.model_target, card.resolved_target, card.surface]);

  const probeByModel = useMemo(() => new Map(card.probes.map((p) => [p.model, p])), [card.probes]);
  // Models that already passed a real test for THIS endpoint lead the picker and
  // say so; ones whose last test failed are marked rather than hidden.
  const options = useMemo(() => {
    const rank = (m: CatalogModel) => (probeByModel.get(m.id)?.ok ? 0 : probeByModel.has(m.id) ? 2 : 1);
    return [...models].sort((a, b) => rank(a) - rank(b)).map((m) => {
      const p = probeByModel.get(m.id);
      return { value: m.id, label: `${m.display}${p ? (p.ok ? ' — verified' : ' — last test failed') : ''}` };
    });
  }, [models, probeByModel]);
  const verified = useMemo(() => card.probes.filter((p) => p.ok), [card.probes]);

  const hasProviders = card.accounts > 0;
  const selectedIsListed = models.some((m) => m.id === target);
  const dirty = mode !== card.mode || (mode === 'manual' && target !== card.model_target);
  const canSave = dirty && !busy && !testing && !(mode === 'manual' && (!target || !selectedIsListed));
  // Manual tests the model in the box; Auto tests the model it would pick now.
  const testModel = mode === 'manual' ? target : card.mode === 'auto' ? card.resolved_target : '';
  const paid = PAID_TESTS[card.surface];

  const save = async () => {
    if (!canSave) return;
    setBusy(true); setError('');
    try {
      const updated = await setServiceBinding(card.surface, mode, mode === 'manual' ? target : '');
      onSaved(updated);
      setSaved(true);
      setTimeout(() => setSaved(false), 2500);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Could not save this service default.');
    } finally { setBusy(false); }
  };

  const doTest = async () => {
    setConfirmPaid(false);
    if (!testModel) return;
    setTesting(true); setError('');
    try {
      await onTest(card.surface, testModel, mode === 'manual' ? prompt.trim() : '');
    } catch (e) {
      setError(e instanceof Error ? e.message : 'The endpoint test could not be recorded.');
    } finally { setTesting(false); }
  };

  const submit = (e: FormEvent) => { e.preventDefault(); void save(); };
  const discard = () => { setMode(card.mode); setTarget(card.model_target || card.resolved_target || ''); };

  return (
    <section id={`service-${card.surface}`} aria-labelledby={`service-title-${card.surface}`}
      className={cx('scroll-mt-6 rounded-xl border bg-surface transition-colors duration-500', highlighted ? 'border-accent' : 'border-line')}>
      <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2 px-5 pt-4">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <span className="flex h-7 w-7 items-center justify-center rounded-md border border-line bg-raised text-accent">
              <ServiceIcon surface={card.surface} />
            </span>
            <h2 id={`service-title-${card.surface}`} className="text-[15px] font-semibold">{card.label}</h2>
            <code className="rounded border border-line bg-raised px-2 py-0.5 font-mono text-[12px] text-dim">{card.endpoint}</code>
          </div>
          <p className="mt-1.5 max-w-3xl text-[13px] leading-relaxed text-mute">{card.description}</p>
        </div>
        <StateChip card={card} />
      </div>

      {!hasProviders ? (
        <div className="mx-5 mb-4 mt-3 flex flex-wrap items-center justify-between gap-3 rounded-lg border border-line bg-raised px-4 py-3">
          <p className="text-[13px] text-mute">
            <span className="font-medium text-ink">No providers connected.</span> Connect one and sync its model catalog before choosing a default.
          </p>
          <Link to="/providers" className={cx(btn.base, btn.ghost)}>Add provider</Link>
        </div>
      ) : (
        <form onSubmit={submit} className="px-5 pb-4 pt-4">
          {/* One settings row: what it does | which model | what you can do.
              Wide enough that the labels no longer truncate. */}
          <div className="grid items-start gap-4 md:grid-cols-[minmax(210px,0.8fr)_minmax(0,1.7fr)] xl:grid-cols-[minmax(210px,0.7fr)_minmax(0,1.6fr)_auto]">
            <div>
              <label className={labelCls} htmlFor={`mode-${card.surface}`}>Default behavior</label>
              <select id={`mode-${card.surface}`} className={inputCls} value={mode} onChange={(e) => setMode(e.target.value as ServiceMode)}>
                <option value="off">Off</option>
                <option value="manual">Manual default</option>
                <option value="auto">Auto</option>
              </select>
              <p className="mt-1.5 text-[12px] leading-relaxed text-mute">
                {mode === 'off' && <>Requests must include their own <code className="font-mono text-dim">model</code>. The safest first-run state.</>}
                {mode === 'manual' && 'Use the model chosen here when a request omits one.'}
                {mode === 'auto' && 'Use the model that most recently passed a real test for this endpoint.'}
              </p>
            </div>

            <div className="min-w-0">
              {mode === 'manual' && (
                <>
                  <label className={labelCls} htmlFor={`model-${card.surface}`}>Default model for {card.endpoint}</label>
                  {models.length === 0 && !loadingModels ? (
                    <div className="rounded-lg border border-line bg-raised px-3 py-2.5 text-[13px] text-mute">
                      No synced models yet. Open Providers, sync a provider’s catalog, then come back here.
                    </div>
                  ) : (
                    <Combobox id={`model-${card.surface}`} label={`Default model for ${card.endpoint}`} idPrefix={`service-${card.surface}`} options={options} value={target}
                      onChange={setTarget} loadingText={loadingModels ? 'Loading provider models…' : 'No synced models'}
                      disabled={options.length === 0 && loadingModels} placeholder="Search provider models…" />
                  )}
                  {target && !selectedIsListed && !loadingModels && <p className="mt-1 text-[12px] text-warn">The saved target is not in the current enabled-provider catalog.</p>}
                  {verified.length > 0 && <p className="mt-1.5 text-[12px] text-mute">Models already verified here are listed first.</p>}
                </>
              )}
              {mode === 'auto' && (
                <div className="rounded-lg border border-line bg-raised px-3.5 py-3 text-[13px]">
                  <p className="text-mute">
                    Currently picks:{' '}
                    <span className="font-mono text-ink">{card.mode === 'auto' && card.resolved_target ? card.resolved_target : 'nothing yet'}</span>
                    {' · '}<span className="tnum">{verified.length}</span> verified model{verified.length === 1 ? '' : 's'}
                  </p>
                  <p className="mt-1 text-[12px] leading-relaxed text-mute">
                    Auto uses only models that passed a real test for this exact endpoint. Chat compatibility checks do not count.
                  </p>
                  {verified.length > 0 && (
                    <details className="mt-2">
                      <summary className="cursor-pointer text-[12px] text-dim hover:text-ink">Show verified models</summary>
                      <ul className="mt-2 space-y-1 text-[12px]">
                        {verified.slice(0, 12).map((p) => (
                          <li key={p.model} className="flex items-baseline gap-3">
                            <span className="min-w-0 flex-1 truncate font-mono text-dim" title={p.model}>{p.model}</span>
                            <span className="shrink-0 text-mute">{relTime(p.tested_at)}</span>
                          </li>
                        ))}
                        {verified.length > 12 && <li className="text-mute">+{verified.length - 12} more</li>}
                      </ul>
                    </details>
                  )}
                </div>
              )}
            </div>

            <div className="flex flex-wrap items-center gap-2 md:col-span-2 xl:col-span-1 xl:flex-col xl:items-stretch xl:pt-[22px]">
              {(mode === 'manual' || (mode === 'auto' && card.mode === 'auto')) && (
                <button type="button" disabled={!testModel || testing || busy || busyElsewhere || (mode === 'manual' && loadingModels)}
                  title={mode === 'auto' && !card.resolved_target ? 'Auto has no pick yet — test a model under Manual first.' : undefined}
                  onClick={() => (paid ? setConfirmPaid(true) : void doTest())}
                  className={cx(btn.base, btn.ghost)}>
                  {testing ? <><Spinner /> Testing…</> : mode === 'manual' ? 'Test endpoint' : 'Test the pick'}
                </button>
              )}
              <button type="submit" disabled={!canSave} className={cx(btn.base, btn.primary)}>
                {busy ? <><Spinner /> Saving…</> : 'Save default'}
              </button>
              <span className="min-h-[18px] text-[12px]" aria-live="polite">
                {dirty ? (
                  <span className="text-warn">Unsaved · <button type="button" className="underline underline-offset-2 hover:text-ink" onClick={discard}>discard</button></span>
                ) : saved ? <span className="text-ok">Saved ✓</span> : null}
              </span>
            </div>
          </div>

          {mode === 'manual' && (
            <details className="mt-3">
              <summary className="cursor-pointer text-[12px] text-dim hover:text-ink">Test options</summary>
              <div className="mt-2 max-w-xl">
                <label className={labelCls} htmlFor={`prompt-${card.surface}`}>{PROMPT_LABELS[card.surface]}</label>
                {card.surface === 'audio_stt' ? (
                  <p className="text-[12px] leading-relaxed text-mute">
                    This endpoint tests a file upload, not text — the test always sends a fixed 0.2 s silent WAV.
                  </p>
                ) : (
                  <>
                    <input id={`prompt-${card.surface}`} type="text" maxLength={2000} value={prompt}
                      onChange={(e) => setPrompt(e.target.value)} placeholder={PROMPT_DEFAULTS[card.surface]}
                      disabled={testing || busy} className={cx(inputCls, 'font-mono')} />
                    <p className="mt-1 text-[11px] leading-relaxed text-mute">
                      Sent verbatim in the test request; empty = the default. Not saved — only the model default persists.
                    </p>
                  </>
                )}
              </div>
            </details>
          )}
        </form>
      )}

      {hasProviders && (
        <div className="space-y-2 border-t border-line px-5 py-3 text-[12px]">
          <p className="text-mute">
            Effective default:{' '}
            <span className="break-all font-mono text-[13px] text-ink">{card.resolved_target || 'None'}</span>
            {card.verdict && (
              <span title={new Date(card.verdict.tested_at).toLocaleString()}>
                {' · '}{card.verdict.ok ? 'passed' : 'failed'} {relTime(card.verdict.tested_at)}
                {!card.verdict.ok && card.verdict.detail ? ` — ${card.verdict.detail}` : ''}
              </span>
            )}
            {!card.verdict && card.resolved_target && ' · never tested'}
          </p>
          {outcome && <OutcomeLine o={outcome} />}
          {error && <p role="alert" className="rounded-lg border border-bad/30 bg-bad-dim px-3 py-2 text-[13px] text-bad">{error}</p>}
          {GENERATABLE.has(card.surface) && (
            <div>
              <button type="button" aria-expanded={tryOpen} onClick={() => setTryOpen((v) => !v)}
                className="text-[13px] font-medium text-dim hover:text-ink">
                Try it <span aria-hidden>{tryOpen ? '▴' : '▾'}</span>
              </button>
              <Collapse open={tryOpen}>
                <div className="pt-3">
                  {card.resolved_target ? (
                    <ServiceTryIt key={card.resolved_target} surface={card.surface} model={card.resolved_target} />
                  ) : (
                    <p className="text-[12px] text-mute">Save a default model for this service to generate with it here.</p>
                  )}
                </div>
              </Collapse>
            </div>
          )}
        </div>
      )}
      {error && !hasProviders && <p role="alert" className="mx-5 mb-4 rounded-lg border border-bad/30 bg-bad-dim px-3 py-2 text-[13px] text-bad">{error}</p>}

      <ConfirmModal open={confirmPaid} tone="primary" confirmLabel="Send test"
        title={`Test ${testModel || 'this endpoint'}?`} body={paid ?? ''}
        onClose={() => setConfirmPaid(false)} onConfirm={() => void doTest()} />
    </section>
  );
}

// ── page ──────────────────────────────────────────────────────────────────

type Filter = 'all' | 'attention' | 'working' | 'off';
type Bulk = { running: boolean; done: number; total: number; summary: string };

export default function Services() {
  const [cards, setCards] = useState<ServiceCard[]>([]);
  const [models, setModels] = useState<CatalogModel[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadingModels, setLoadingModels] = useState(true);
  const [error, setError] = useState('');
  const [catalogGaps, setCatalogGaps] = useState(0);
  const [outcomes, setOutcomes] = useState<Record<string, TestOutcome>>({});
  const [filter, setFilter] = useState<Filter>('all');
  const [includePaid, setIncludePaid] = useState(false);
  const [confirmBulk, setConfirmBulk] = useState(false);
  const [bulk, setBulk] = useState<Bulk | null>(null);
  const [highlight, setHighlight] = useState<string | null>(null);
  const [params, setParams] = useSearchParams();
  const bulkRunning = bulk?.running === true;

  const refresh = useCallback(() =>
    serviceCards().then((r) => setCards(r.services))
      .catch((e: unknown) => setError(e instanceof Error ? e.message : 'Could not refresh service status.')), []);

  useEffect(() => {
    let live = true;
    (async () => {
      try {
        const result = await serviceCards();
        if (live) setCards(result.services);
      } catch (e) {
        if (live) setError(e instanceof Error ? e.message : 'Could not load services.');
      } finally { if (live) setLoading(false); }
      // Every provider's catalog at once, merged as each answers: one slow or
      // broken provider no longer holds the picker (or the whole list) back.
      try {
        const accounts = await get<Account[]>('/admin/accounts');
        let gaps = 0;
        await Promise.all(accounts.filter((a) => a.enabled).map(async (account) => {
          try {
            const rows = await accountModels(account.id);
            if (!live) return;
            setModels((prev) => [...prev, ...rows.map((model) => ({
              ...model,
              id: `${account.namespace}/${model.id}`,
              display: `${account.namespace} / ${model.display ?? model.id}`,
            }))]);
          } catch { gaps++; }
        }));
        if (live) setCatalogGaps(gaps);
      } catch (e) {
        if (live) setError(e instanceof Error ? e.message : 'Could not load provider models.');
      } finally { if (live) setLoadingModels(false); }
    })();
    return () => { live = false; };
  }, []);

  const sortedModels = useMemo(() => [...models].sort((a, b) => a.display.localeCompare(b.display)), [models]);

  const onSaved = useCallback((updated: ServiceCard) => {
    setCards((prev) => prev.map((c) => c.surface === updated.surface ? updated : c));
  }, []);

  // One test, from a card. The POST records the probe; refetch shows the true
  // server verdict and the real Auto target — no guessed local success state.
  const record = (surface: ServiceSurface, model: string, r: { ok: boolean; dispatched: boolean; detail: string; status: number; ms: number }) =>
    setOutcomes((prev) => ({ ...prev, [surface]: { ...r, model, at: Date.now() } }));
  const testOne = useCallback(async (surface: ServiceSurface, model: string, prompt: string) => {
    const r = await testService(surface, model, prompt);
    record(surface, model, r);
    await refresh();
  }, [refresh]);

  // Deep link: ?service=<surface> scrolls to that card and marks it for a moment.
  useEffect(() => {
    const surface = params.get('service');
    if (!surface || cards.length === 0) return;
    setFilter('all');
    setParams((p) => { const n = new URLSearchParams(p); n.delete('service'); return n; }, { replace: true });
    if (!cards.some((c) => c.surface === surface)) return;
    setHighlight(surface);
    requestAnimationFrame(() => document.getElementById(`service-${surface}`)?.scrollIntoView({ block: 'center', behavior: 'smooth' }));
    const t = setTimeout(() => setHighlight(null), 3000);
    return () => clearTimeout(t);
  }, [params, cards, setParams]);

  const counts = useMemo(() => {
    const c: Record<Health, number> = { failing: 0, pending: 0, working: 0, off: 0 };
    for (const card of cards) c[healthOf(card)]++;
    return c;
  }, [cards]);

  const shown = useMemo(() => {
    const keep = (card: ServiceCard) => {
      const h = healthOf(card);
      return filter === 'all' || (filter === 'attention' && (h === 'failing' || h === 'pending')) || (filter === 'working' && h === 'working') || (filter === 'off' && h === 'off');
    };
    return cards.map((c, i) => ({ c, i })).filter(({ c }) => keep(c))
      .sort((a, b) => HEALTH_ORDER[healthOf(a.c)] - HEALTH_ORDER[healthOf(b.c)] || a.i - b.i).map(({ c }) => c);
  }, [cards, filter]);

  // Test everything that is configured, one after another. Paid surfaces
  // (image, video, music) are skipped unless the operator opted in.
  const testable = cards.filter((c) => c.accounts > 0 && c.mode !== 'off' && c.resolved_target);
  const planned = testable.filter((c) => includePaid || !PAID_TESTS[c.surface]);
  const skippedPaid = testable.length - planned.length;
  const runBulk = async () => {
    setConfirmBulk(false);
    setBulk({ running: true, done: 0, total: planned.length, summary: '' });
    let ok = 0; let bad = 0;
    for (const [i, card] of planned.entries()) {
      try {
        const r = await testService(card.surface, card.resolved_target, '');
        record(card.surface, card.resolved_target, r);
        if (r.dispatched && r.ok) ok++; else bad++;
      } catch { bad++; }
      setBulk({ running: true, done: i + 1, total: planned.length, summary: '' });
    }
    await refresh();
    setBulk({
      running: false, done: planned.length, total: planned.length,
      summary: `${ok} passed · ${bad} failed${skippedPaid > 0 ? ` · ${skippedPaid} paid skipped` : ''}`,
    });
  };
  const paidPlanned = planned.filter((c) => PAID_TESTS[c.surface]);

  if (loading) return <div className="flex items-center gap-2 py-12 text-[13px] text-mute"><Spinner /> Loading service settings…</div>;
  if (error && cards.length === 0) return <EmptyState title="Could not load Services" hint={error} />;

  const configured = counts.failing + counts.pending + counts.working;
  return (
    <div className="mx-auto max-w-6xl space-y-5">
      <div className="rounded-xl border border-line bg-surface px-5 py-4">
        <div className="flex flex-wrap items-center justify-between gap-x-6 gap-y-3">
          <div>
            <p className="text-[14px] font-semibold">
              <span className="tnum">{configured}</span> of <span className="tnum">{cards.length}</span> endpoints have a default
              {counts.failing > 0 && <> · <span className="tnum text-bad">{counts.failing}</span> failing</>}
              {counts.pending > 0 && <> · <span className="tnum text-warn">{counts.pending}</span> untested</>}
            </p>
            <p className="mt-1 text-[12px] text-mute">
              A default applies only when a request omits <code className="font-mono text-dim">model</code>; an explicit model always wins.
              {models.length > 0 && <> <span className="tnum">{models.length}</span> models available from enabled providers.</>}
            </p>
          </div>
          <div className="flex flex-wrap items-center gap-3">
            <label className="flex items-center gap-2 text-[12px] text-dim" title="Image, video and music tests can use provider quota">
              <Toggle checked={includePaid} onChange={setIncludePaid} label="include paid tests" disabled={bulkRunning} />
              Include paid tests
            </label>
            <button type="button" disabled={bulkRunning || planned.length === 0}
              onClick={() => (includePaid && paidPlanned.length > 0 ? setConfirmBulk(true) : void runBulk())}
              className={cx(btn.base, btn.ghost)}
              title={planned.length === 0 ? 'Nothing configured to test' : undefined}>
              {bulkRunning ? <><Spinner /> Testing {bulk!.done + 1} of {bulk!.total}…</> : `Test all configured (${planned.length})`}
            </button>
          </div>
        </div>
        <div aria-live="polite" className={cx('text-[12px]', bulk && !bulk.running && 'mt-2')}>
          {bulk && !bulk.running && <span className={bulk.summary.includes('0 failed') ? 'text-ok' : 'text-warn'}>{bulk.summary}</span>}
        </div>
        <details className="mt-2">
          <summary className="cursor-pointer text-[12px] text-dim hover:text-ink">How defaults work</summary>
          <p className="mt-2 max-w-3xl text-[12px] leading-relaxed text-mute">
            Each setting is a default for its endpoint only. If nothing is configured, the endpoint stays available but returns a clear{' '}
            <code className="font-mono text-dim">service_not_configured</code> error instead of sending a blind upstream request.
            Chat protocol checks do not verify image, audio, or embedding support — only a real test of that endpoint does.
          </p>
        </details>
      </div>

      <div className="flex flex-wrap items-center justify-between gap-3">
        <Segmented<Filter> label="Filter services"
          options={[
            { key: 'all', label: `All ${cards.length}` },
            { key: 'attention', label: `Needs attention ${counts.failing + counts.pending}`, title: 'failing, or configured and not yet proven' },
            { key: 'working', label: `Working ${counts.working}` },
            { key: 'off', label: `Not configured ${counts.off}` },
          ]}
          value={filter} onChange={setFilter} />
        {loadingModels && <span className="flex items-center gap-2 text-[12px] text-mute"><Spinner className="h-3 w-3" /> Loading provider models…</span>}
      </div>

      {error && <p className="rounded-lg border border-bad/30 bg-bad-dim px-3 py-2 text-[13px] text-bad" role="alert">{error}</p>}
      {catalogGaps > 0 && !loadingModels && (
        <p className="rounded-lg border border-warn/30 bg-warn-dim px-3 py-2 text-[13px] text-ink" role="status">
          {catalogGaps} provider{catalogGaps === 1 ? '' : 's'} could not list models, so their models are missing from the pickers. Reload to retry.
        </p>
      )}

      {shown.length === 0 ? (
        <EmptyState title="No service matches this filter." hint="Switch the filter to see every endpoint." />
      ) : (
        <Stagger className="grid gap-4">
          {shown.map((card) => (
            <StaggerItem key={card.surface}>
              <ServiceCardView card={card} models={sortedModels} loadingModels={loadingModels}
                outcome={outcomes[card.surface]} highlighted={highlight === card.surface}
                busyElsewhere={bulkRunning} onSaved={onSaved} onTest={testOne} />
            </StaggerItem>
          ))}
        </Stagger>
      )}

      <ConfirmModal open={confirmBulk} tone="primary" confirmLabel={`Run ${planned.length} tests`}
        title="Include paid tests?"
        body={`${paidPlanned.map((c) => c.label).join(', ')} will send real generation requests that can use provider quota or incur a charge.`}
        onClose={() => setConfirmBulk(false)} onConfirm={() => void runBulk()} />
    </div>
  );
}
