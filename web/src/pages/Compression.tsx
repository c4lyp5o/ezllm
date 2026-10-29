// COMPRESSION — profiles (ordered, fail-open pipelines), what they actually
// saved, and a way to try a pipeline on a pasted sample before attaching it.
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { Link, useSearchParams } from 'react-router-dom';
import {
  ApiError, RANGE_HOURS, RANGE_LABEL, compressionProfiles, del, get, patch, post, previewCompression, requests, usage,
  type Combo, type CompressionPreview, type CompressionProfile, type RangeKey, type RequestsSummary, type UsageRow,
} from '../utils/api';
import { BarList, Panel, VIZ } from '../components/charts';
import { ReorderItem, Stagger, StaggerItem } from '../components/motion';
import {
  Chip, ConfirmModal, CopyButton, EmptyState, Modal, Mono, Segmented, SectionLabel, Spinner, Toggle,
  btn, cx, formatTokens, inputCls, labelCls, nfmt,
} from '../components/ui';
import { useRange } from '../utils/preferences';
import { deltaNote, MAX_SPAN_H } from '../utils/lib';
import {
  ENGINE_IDS, ENGINE_INFO, PRESETS, copyName, draftFromPreset, freshDraft, fromProfile, hasErrors, newStage, pipelineAdvice,
  profileBody, settingsWords, stageLabel, stageWords, validateDraft,
  type Preset, type ProfileDraft, type StageDraft,
} from '../utils/compression';

const RANGES: RangeKey[] = ['24h', '7d', '30d', '365d'];
const rangeWords = (r: RangeKey) => (r === '365d' ? 'year' : RANGE_LABEL[r]);

const errText = (e: unknown) => (e instanceof ApiError ? e.detail.message : e instanceof Error ? e.message : String(e));

// ── try it on a sample ──────────────────────────────────────────────────────

const SAMPLE_KEY = 'ezllm.compress.sample';
const readSample = () => { try { return sessionStorage.getItem(SAMPLE_KEY) ?? ''; } catch { return ''; } };
const writeSample = (v: string) => { try { sessionStorage.setItem(SAMPLE_KEY, v); } catch { /* storage blocked: the sample just is not remembered */ } };

function PreviewPanel({ profile, profileId, blocked }: {
  profile?: ProfileDraft; profileId?: number; blocked?: string;
}) {
  const [text, setText] = useState(readSample);
  const [kind, setKind] = useState<'tool_output' | 'prompt'>('tool_output');
  const [tool, setTool] = useState('terminal');
  const [ignoreFloors, setIgnoreFloors] = useState(false);
  const [running, setRunning] = useState(false);
  const [result, setResult] = useState<CompressionPreview | null>(null);
  const [err, setErr] = useState('');
  const gen = useRef(0);

  const run = async () => {
    const mine = ++gen.current;
    setRunning(true); setErr('');
    try {
      const body = profileId !== undefined && !profile
        ? { profile_id: profileId }
        : { profile: { stages: profileBody(profile!).stages, exempt_last_turn: profile!.exempt_last_turn, min_compress_ratio: profile!.min_compress_ratio, auto_trigger_tokens: profile!.auto_trigger_tokens, fresh_tool_output: profile!.fresh_tool_output } };
      const r = await previewCompression({ ...body, text, kind, tool: kind === 'tool_output' ? tool.trim() || 'terminal' : undefined, ignore_floors: ignoreFloors });
      if (mine === gen.current) setResult(r);
    } catch (e) { if (mine === gen.current) { setErr(errText(e)); setResult(null); } }
    finally { if (mine === gen.current) setRunning(false); }
  };

  const saved = result ? result.tokens_before - result.tokens_after : 0;
  return (
    <div className="space-y-3">
      <div>
        <label className={labelCls} htmlFor="cp-sample">Sample</label>
        <textarea id="cp-sample" rows={6} className={cx(inputCls, 'font-mono text-[12px]')} spellCheck={false}
          placeholder={kind === 'tool_output' ? 'Paste a tool result: build output, test run, JSON, a log…' : 'Paste a long prompt or message…'}
          value={text} onChange={(e) => { setText(e.target.value); writeSample(e.target.value); }} />
      </div>
      <div className="flex flex-wrap items-center gap-x-5 gap-y-2">
        <Segmented<'tool_output' | 'prompt'> size="sm" label="Sample kind" value={kind} onChange={setKind}
          options={[{ key: 'tool_output', label: 'Tool output' }, { key: 'prompt', label: 'Prompt' }]} />
        {kind === 'tool_output' && (
          <label className="flex items-center gap-2 text-[12px] text-mute"
            title="The tool's name decides how RTK treats the text: terminal and bash output is filtered, read_file is kept whole.">
            Tool
            <input aria-label="Tool name" className={cx(inputCls, '!w-36 py-1 font-mono text-[12px]')} value={tool} spellCheck={false}
              onChange={(e) => setTool(e.target.value)} />
          </label>
        )}
        <label className="flex items-center gap-2 text-[12px] text-mute" title="Skip the minimum-saving and size floors to see what the engines alone can do.">
          <input type="checkbox" checked={ignoreFloors} onChange={(e) => setIgnoreFloors(e.target.checked)} />
          Ignore the minimum saving
        </label>
        <button type="button" className={cx(btn.base, btn.primary, 'ml-auto px-4 py-1.5 text-[13px]')}
          disabled={running || !text.trim() || !!blocked} title={blocked} onClick={() => void run()}>
          {running ? <Spinner /> : null} Run on sample
        </button>
      </div>
      {blocked && <p className="text-[12px] text-warn">{blocked}</p>}
      {err && <div role="alert" className="rounded-lg border border-bad/30 bg-bad-dim px-4 py-2.5 text-[13px] text-bad">{err}</div>}
      {result && (
        <div className="space-y-3 rounded-lg border border-line bg-raised/40 p-4" aria-live="polite">
          <div className="flex flex-wrap items-center gap-x-4 gap-y-1.5">
            {result.applied
              ? <Chip tone="ok">Would ship shorter</Chip>
              : <Chip tone="warn" title={result.note}>Would ship unchanged</Chip>}
            <span className="font-mono text-[13px] tabular-nums">
              {nfmt(result.tokens_before)} <span className="text-mute">→</span> {nfmt(result.tokens_after)} tokens
              {result.applied && saved > 0 && <span className="text-ok"> · saves {nfmt(saved)} ({Math.round((saved / Math.max(1, result.tokens_before)) * 100)}%)</span>}
            </span>
          </div>
          {!result.applied && (
            <p className="text-[12px] leading-snug text-mute">
              {result.note || 'The stages found nothing to shorten.'}
              {result.saved_notional > 0 && <> The stages would have saved {nfmt(result.saved_notional)} tokens, but the saving is too small to ship.</>}
            </p>
          )}
          {result.stages.length > 0 && (
            <table className="w-full text-[12px]">
              <thead><tr className="text-left text-mute"><th className="py-1 font-medium">Stage</th><th className="py-1 text-right font-medium">Before</th><th className="py-1 text-right font-medium">After</th><th className="py-1 text-right font-medium">Saved</th></tr></thead>
              <tbody>
                {result.stages.map((s, i) => (
                  <tr key={i} className="border-t border-line/60">
                    <td className="py-1.5">{stageLabel(s.engine)}</td>
                    <td className="py-1.5 text-right font-mono tabular-nums text-mute">{nfmt(s.pre)}</td>
                    <td className="py-1.5 text-right font-mono tabular-nums text-mute">{nfmt(s.post)}</td>
                    <td className={cx('py-1.5 text-right font-mono tabular-nums', s.saved > 0 ? 'text-ok' : 'text-mute')}>{nfmt(s.saved)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          {result.applied && (
            <div>
              <div className="mb-1 flex items-center justify-between gap-3">
                <span className="text-[12px] font-medium text-mute">After</span>
                <CopyButton text={result.after} label="Copy result" />
              </div>
              <pre className="max-h-64 overflow-auto whitespace-pre-wrap break-words rounded-lg border border-line bg-raised p-3 font-mono text-[12px] leading-relaxed text-dim">{result.after}</pre>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

// ── the editor ──────────────────────────────────────────────────────────────

type EditorTarget = { open: boolean; editing: CompressionProfile | null; template: CompressionProfile | null; fresh?: boolean };
const CLOSED: EditorTarget = { open: false, editing: null, template: null };
const snapshot = (d: ProfileDraft) => JSON.stringify({ ...d, stages: d.stages.map(({ uid: _u, ...s }) => s) });

function ProfileModal({ target, profiles, onClose, onSaved }: {
  target: EditorTarget; profiles: CompressionProfile[]; onClose: () => void; onSaved: (name: string, created: boolean) => void;
}) {
  const { open, editing, template } = target;
  const [d, setD] = useState<ProfileDraft>(freshDraft());
  const initial = useRef('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const [showAll, setShowAll] = useState(false);
  const [preset, setPreset] = useState<string | null>(null);

  useEffect(() => {
    if (!open) return;
    setErr(''); setBusy(false); setShowAll(false); setPreset(null);
    const next = editing ? fromProfile(editing)
      : template ? { ...fromProfile(template), name: copyName(template.name, profiles.map((p) => p.name)) }
        : freshDraft();
    setD(next);
    initial.current = snapshot(next);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, editing, template]);

  const update = <K extends keyof ProfileDraft>(key: K, value: ProfileDraft[K]) => setD((cur) => ({ ...cur, [key]: value }));
  const updateStage = (uid: number, part: Partial<StageDraft>) => setD((cur) => ({ ...cur, stages: cur.stages.map((s) => (s.uid === uid ? { ...s, ...part } : s)) }));
  const moveStage = (i: number, to: number) => setD((cur) => {
    if (to < 0 || to >= cur.stages.length || to === i) return cur;
    const stages = [...cur.stages];
    const [s] = stages.splice(i, 1);
    stages.splice(to, 0, s);
    return { ...cur, stages };
  });

  const others = profiles.filter((p) => p.id !== editing?.id).map((p) => p.name);
  const errors = validateDraft(d, others);
  const dirty = open && snapshot(d) !== initial.current;
  const advice = pipelineAdvice(d.stages);
  const nameShown = errors.name && (showAll || d.name.trim() !== '');

  const submit = async () => {
    if (busy) return;
    if (hasErrors(errors)) { setShowAll(true); return; }
    setBusy(true); setErr('');
    try {
      if (editing) await patch<CompressionProfile>(`/admin/compression-profiles/${editing.id}`, profileBody(d));
      else await post<CompressionProfile>('/admin/compression-profiles', profileBody(d));
      onSaved(d.name.trim(), !editing);
    } catch (e) { setErr(errText(e)); } finally { setBusy(false); }
  };

  return (
    <Modal open={open} onClose={onClose} wide busy={busy} dirty={dirty} onSubmit={() => void submit()}
      title={editing ? `Edit ${editing.name}` : template ? `Duplicate ${template.name}` : 'New profile'}
      subtitle="An ordered pipeline: each stage receives the previous stage’s output"
      footer={(close) => <>
        {showAll && hasErrors(errors) && <span className="mr-auto text-[12px] text-bad">Fix the fields marked in red to save.</span>}
        <button type="button" className={cx(btn.base, btn.ghost)} onClick={close}>Cancel</button>
        <button type="submit" disabled={busy || (!!editing && !dirty)} className={cx(btn.base, btn.primary)}>
          {busy ? <Spinner /> : null} Save profile
        </button>
      </>}>
      <div className="space-y-6">
        {!editing && !template && (
          <div>
            <SectionLabel className="mb-2">Start from</SectionLabel>
            <div className="grid gap-2 sm:grid-cols-3">
              {PRESETS.map((p: Preset) => (
                <button key={p.key} type="button" aria-pressed={preset === p.key}
                  onClick={() => { setPreset(p.key); setD((cur) => draftFromPreset(p, cur)); }}
                  className={cx('rounded-lg border px-3 py-2 text-left transition-colors',
                    preset === p.key ? 'border-accent/60 bg-accent-dim' : 'border-line bg-raised/60 hover:border-line-strong')}>
                  <span className="block text-[13px] font-medium">{p.label}</span>
                  <span className="mt-0.5 block text-[12px] leading-snug text-mute">{p.hint}</span>
                </button>
              ))}
            </div>
            <p className="mt-1.5 text-[12px] text-mute">Starting points to adjust, not recommendations for your traffic. Try the result on a sample below.</p>
          </div>
        )}

        <section className="grid gap-4 sm:grid-cols-[minmax(0,1.4fr)_minmax(0,1fr)_auto] sm:items-start">
          <div>
            <label className={labelCls} htmlFor="cp-name">Profile name</label>
            <input id="cp-name" className={cx(inputCls, nameShown && 'border-bad/60')} value={d.name} spellCheck={false}
              aria-invalid={!!nameShown} onChange={(e) => update('name', e.target.value)} placeholder="safe-default" />
            {nameShown && <p className="mt-1 text-[12px] text-bad">{errors.name}</p>}
          </div>
          <div>
            <label className={labelCls} htmlFor="cp-trigger">Only compress above (tokens)</label>
            <input id="cp-trigger" type="number" min={0} className={cx(inputCls, 'font-mono tabular-nums', errors.trigger && 'border-bad/60')} value={d.auto_trigger_tokens}
              onChange={(e) => update('auto_trigger_tokens', Math.max(0, Math.trunc(Number(e.target.value)) || 0))} />
            {errors.trigger ? <p className="mt-1 text-[12px] text-bad">{errors.trigger}</p> : <p className="mt-1 text-[12px] text-mute">0 means every request.</p>}
          </div>
          <label className="flex items-center gap-2.5 text-[13px] text-dim sm:pt-7">
            <Toggle checked={d.enabled} onChange={(v) => update('enabled', v)} label="Profile enabled" />
            {d.enabled ? 'Enabled' : 'Paused'}
          </label>
        </section>

        <section>
          <SectionLabel className="mb-2.5">Safety</SectionLabel>
          <div className="grid gap-4 sm:grid-cols-2">
            <div>
              <label className={labelCls} htmlFor="cp-ratio">Keep a rewrite only if it saves at least (%)</label>
              <input id="cp-ratio" type="number" min={5} max={95} step={1} className={cx(inputCls, '!w-28 font-mono tabular-nums', errors.ratio && 'border-bad/60')}
                value={Math.round(d.min_compress_ratio * 100)} onChange={(e) => update('min_compress_ratio', (Number(e.target.value) || 0) / 100)} />
              {errors.ratio ? <p className="mt-1 text-[12px] text-bad">{errors.ratio}</p>
                : <p className="mt-1 text-[12px] text-mute">A smaller saving is not worth changing the request (and its cache) for, so the original ships.</p>}
            </div>
            <div className="space-y-3">
              <label className="flex items-start gap-2.5 text-[13px] text-dim">
                <input type="checkbox" className="mt-1" checked={d.exempt_last_turn} onChange={(e) => update('exempt_last_turn', e.target.checked)} />
                <span>Leave the final turn alone<span className="block text-[12px] text-mute">What you just sent is never rewritten.</span></span>
              </label>
              <div>
                <span className={labelCls} title="keep: a tool result arriving as the last message ships whole. compress: tool-output engines trim it on its first send; it then stays frozen. A last message with any prose is always exempt.">Newest tool result</span>
                <Segmented<'keep' | 'compress'> size="sm" label="Newest tool result" value={d.fresh_tool_output} onChange={(v) => update('fresh_tool_output', v)}
                  options={[{ key: 'keep', label: 'Send whole' }, { key: 'compress', label: 'Trim on first send' }]} />
              </div>
            </div>
          </div>
        </section>

        <section>
          <div className="mb-2 flex items-center justify-between gap-3">
            <SectionLabel>Pipeline <span className="font-normal text-mute">· runs top to bottom</span></SectionLabel>
            <button type="button" className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[13px]')}
              onClick={() => setD((cur) => ({ ...cur, stages: [...cur.stages, newStage('lite')] }))}>＋ Add stage</button>
          </div>
          <div className="space-y-2">
            {d.stages.map((stage, i) => {
              const info = ENGINE_INFO[stage.engine];
              const problem = errors.stage[stage.uid];
              // `layout`: reordering the pipeline SLIDES each stage to its new
              // position. Order is the execution order, so seeing the row move
              // is the difference between "I moved a row" and "I changed the
              // pipeline".
              return (
                <ReorderItem key={stage.uid} className={cx('rounded-lg border bg-raised/40 p-3', problem ? 'border-bad/50' : 'border-line')}>
                  <div className="grid items-start gap-2 sm:grid-cols-[auto_minmax(0,1fr)_minmax(0,1fr)_auto]">
                    <span className="pt-2 font-mono text-[12px] text-mute">{String(i + 1).padStart(2, '0')}</span>
                    <div>
                      <label className={labelCls} htmlFor={`cp-engine-${stage.uid}`}>Engine</label>
                      <select id={`cp-engine-${stage.uid}`} className={inputCls} value={stage.engine}
                        onChange={(e) => updateStage(stage.uid, { engine: e.target.value, option: ENGINE_INFO[e.target.value]?.option?.initial ?? '', extra: {}, toolClasses: '' })}>
                        {ENGINE_IDS.map((id) => <option key={id} value={id}>{stageLabel(id)}</option>)}
                        {!ENGINE_IDS.includes(stage.engine) && <option value={stage.engine} disabled>{stageLabel(stage.engine)} (not offered)</option>}
                      </select>
                    </div>
                    <div>
                      {info?.option ? (
                        <>
                          <label className={labelCls} htmlFor={`cp-option-${stage.uid}`}>{info.option.label}</label>
                          {info.option.values ? (
                            <select id={`cp-option-${stage.uid}`} className={inputCls} value={stage.option || info.option.initial}
                              onChange={(e) => updateStage(stage.uid, { option: e.target.value })}>
                              {info.option.values.map((v) => <option key={v} value={v}>{v}</option>)}
                            </select>
                          ) : (
                            <input id={`cp-option-${stage.uid}`} className={cx(inputCls, 'font-mono tabular-nums')} inputMode="numeric" value={stage.option}
                              onChange={(e) => updateStage(stage.uid, { option: e.target.value })} />
                          )}
                          {stage.engine === 'rtk' && <>
                            <label className={cx(labelCls, 'mt-2')} htmlFor={`cp-classes-${stage.uid}`}
                              title="Name your app's tools: log and search output is head/tail filtered, preserve is never touched. Unlisted tools use the built-in table; unknown tools are preserved.">Tool classes (name=log|search|preserve)</label>
                            <input id={`cp-classes-${stage.uid}`} className={cx(inputCls, 'font-mono')} placeholder="terminal=log, web_extract=preserve"
                              value={stage.toolClasses ?? ''} onChange={(e) => updateStage(stage.uid, { toolClasses: e.target.value })} />
                          </>}
                        </>
                      ) : <p className="pt-7 text-[12px] text-mute">No options for this stage.</p>}
                    </div>
                    <div className="flex items-center gap-0.5 sm:pt-6">
                      <button type="button" aria-label={`Move stage ${i + 1} up`} title="Move up" disabled={i === 0}
                        className="rounded p-1 text-mute hover:bg-hover hover:text-ink disabled:opacity-30" onClick={() => moveStage(i, i - 1)}>↑</button>
                      <button type="button" aria-label={`Move stage ${i + 1} down`} title="Move down" disabled={i === d.stages.length - 1}
                        className="rounded p-1 text-mute hover:bg-hover hover:text-ink disabled:opacity-30" onClick={() => moveStage(i, i + 1)}>↓</button>
                      <button type="button" aria-label={`Remove stage ${i + 1}`} title="Remove"
                        className="rounded p-1 text-mute hover:bg-hover hover:text-bad" onClick={() => setD((cur) => ({ ...cur, stages: cur.stages.filter((s) => s.uid !== stage.uid) }))}>✕</button>
                    </div>
                  </div>
                  <p className="mt-1.5 pl-7 text-[12px] leading-snug text-mute">{info?.note ?? 'An engine this editor has no description for.'}</p>
                  <div className="mt-1.5 flex flex-wrap items-center gap-x-4 pl-7 text-[12px] text-mute">
                    {i > 1 && <button type="button" className="hover:text-ink" onClick={() => moveStage(i, 0)}>Move to top</button>}
                    <button type="button" className="hover:text-ink"
                      onClick={() => setD((cur) => { const at = cur.stages.findIndex((s) => s.uid === stage.uid); const copy = { ...stage, ...newStage(stage.engine), option: stage.option, extra: stage.extra, toolClasses: stage.toolClasses }; const stages = [...cur.stages]; stages.splice(at + 1, 0, copy); return { ...cur, stages }; })}>Duplicate</button>
                  </div>
                  {problem && <p className="mt-1.5 pl-7 text-[12px] text-bad">{problem}</p>}
                </ReorderItem>
              );
            })}
            {d.stages.length === 0 && <p className="rounded-lg border border-dashed border-line px-4 py-5 text-center text-[13px] text-mute">No stages yet. Add at least one before saving.</p>}
          </div>
          {showAll && errors.stages && <p className="mt-1.5 text-[12px] text-bad">{errors.stages}</p>}
          {advice.length > 0 && (
            <ul className="mt-2 space-y-0.5 text-[12px] text-warn">{advice.map((a) => <li key={a}>{a}</li>)}</ul>
          )}
        </section>

        <section>
          <details className="group rounded-lg border border-line">
            <summary className="cursor-pointer select-none px-4 py-2.5 text-[13px] font-medium text-dim hover:text-ink">
              Try it on a sample <span className="font-normal text-mute">· runs this draft, nothing is saved or sent</span>
            </summary>
            <div className="border-t border-line p-4">
              <PreviewPanel profile={d} blocked={errors.stages || Object.keys(errors.stage).length ? 'Fix the pipeline errors first.' : undefined} />
            </div>
          </details>
        </section>

        <section>
          <label className={labelCls} htmlFor="cp-notes">Notes</label>
          <textarea id="cp-notes" rows={2} className={inputCls} value={d.notes} onChange={(e) => update('notes', e.target.value)} placeholder="Optional: what this profile is for" />
        </section>

        {err && <div role="alert" className="rounded-lg border border-bad/30 bg-bad-dim px-4 py-3 text-[13px] text-bad">{err}</div>}
      </div>
    </Modal>
  );
}

// ── the page ────────────────────────────────────────────────────────────────

type By = 'provider' | 'combo';

export default function Compression() {
  const [profiles, setProfiles] = useState<CompressionProfile[] | null>(null);
  const [combos, setCombos] = useState<Combo[]>([]);
  const [editor, setEditor] = useState<EditorTarget>(CLOSED);
  const [trying, setTrying] = useState<CompressionProfile | null>(null);
  const [pendingDelete, setPendingDelete] = useState<CompressionProfile | null>(null);
  const [busyID, setBusyID] = useState<number | null>(null);
  const [deleting, setDeleting] = useState(false);
  const [err, setErr] = useState<ReactErr | null>(null);
  const [notice, setNotice] = useState('');
  const [range, setRange] = useRange();
  const [by, setBy] = useState<By>('provider');
  const [params, setParams] = useSearchParams();

  // Live savings from the ledger: saved per account and per combo, plus the
  // requests summary's four compression columns — applied_saved is the REAL
  // rewritten total (the per-account `saved` column sweeps in legacy declined
  // rows), declined_calls / held_notional count and measure the rewrites a
  // stage correctly refused.
  const [byAccount, setByAccount] = useState<UsageRow[] | null>(null);
  const [byCombo, setByCombo] = useState<UsageRow[] | null>(null);
  const [sum, setSum] = useState<RequestsSummary | null>(null);
  const [priorSaved, setPriorSaved] = useState<number | null>(null);
  const [savingsError, setSavingsError] = useState(false);
  const [savingsAttempt, setSavingsAttempt] = useState(0);

  const reload = useCallback(() => {
    compressionProfiles().then((p) => { setProfiles(p); }).catch((e) => setErr({ text: errText(e) }));
    get<Combo[]>('/admin/combos').then(setCombos).catch(() => setCombos([]));
  }, []);
  useEffect(() => { reload(); }, [reload]);

  useEffect(() => {
    let live = true;
    setByAccount(null); setByCombo(null); setSum(null); setSavingsError(false); setPriorSaved(null);
    Promise.all([usage('account', range), usage('alias', range), requests({ range, limit: 1 })])
      .then(([a, c, report]) => {
        if (!live) return;
        setByAccount(a.groups); setByCombo(c.groups); setSum(report.summary);
        // The previous equal window: a 2x report minus this one. Comparison only,
        // so a failure or an over-long span just leaves the figure without one.
        const hours = RANGE_HOURS[range];
        if (hours * 2 <= MAX_SPAN_H) {
          requests({ hours: hours * 2, limit: 1 })
            .then((r2) => { if (live) setPriorSaved(Math.max(0, (r2.summary.applied_saved ?? 0) - (report.summary.applied_saved ?? 0))); })
            .catch(() => {});
        }
      })
      .catch(() => { if (live) setSavingsError(true); });
    return () => { live = false; };
  }, [range, savingsAttempt]);

  // Deep links: /compression?edit=<id> and ?new=1.
  useEffect(() => {
    if (!profiles) return;
    const edit = Number(params.get('edit'));
    if (!edit && params.get('new') !== '1') return;
    const next = new URLSearchParams(params);
    next.delete('edit'); next.delete('new');
    setParams(next, { replace: true });
    const p = profiles.find((x) => x.id === edit);
    if (p) setEditor({ open: true, editing: p, template: null });
    else if (params.get('new') === '1') setEditor({ open: true, editing: null, template: null });
  }, [profiles, params, setParams]);

  const attached = useMemo(() => {
    const m = new Map<number, Combo[]>();
    for (const c of combos) if (c.compression_profile_id) m.set(c.compression_profile_id, [...(m.get(c.compression_profile_id) ?? []), c]);
    return m;
  }, [combos]);

  const toggleEnabled = async (p: CompressionProfile, enabled: boolean) => {
    setBusyID(p.id); setErr(null); setNotice('');
    setProfiles((prev) => prev?.map((x) => (x.id === p.id ? { ...x, enabled } : x)) ?? prev);
    try { await patch(`/admin/compression-profiles/${p.id}`, { enabled }); setNotice(`${enabled ? 'Enabled' : 'Paused'} ${p.name}.`); }
    catch (e) { setErr({ text: errText(e) }); }
    finally { setBusyID(null); reload(); }
  };

  const askDelete = (p: CompressionProfile) => {
    const users = attached.get(p.id) ?? [];
    // The server refuses a profile a combo still uses, so say so up front.
    if (users.length > 0) { setErr({ text: `${p.name} is still used by`, combos: users }); return; }
    setErr(null); setPendingDelete(p);
  };
  const remove = async (p: CompressionProfile) => {
    setDeleting(true); setErr(null);
    try { await del(`/admin/compression-profiles/${p.id}`); setNotice(`Deleted ${p.name}.`); reload(); }
    catch (e) { setErr({ text: errText(e), combos: e instanceof ApiError ? e.detail.combos as Combo[] | undefined : undefined }); }
    finally { setDeleting(false); setPendingDelete(null); }
  };

  // Only the summary's applied total describes rewrites that shipped.
  const rewritten = sum?.applied_saved ?? 0;
  const declinedCalls = sum?.declined_calls ?? 0;
  const heldNotional = sum?.held_notional ?? 0;
  const appliedCalls = sum?.applied_calls ?? 0;
  const rewritesDecided = appliedCalls + declinedCalls;
  // Hit-rate is meaningful only when a stage actually refused something;
  // 100% of one decision is a rounding error wearing a stat's clothes.
  const showHitRate = declinedCalls > 0;
  const showHeld = heldNotional > 0;
  const hasSavings = rewritten > 0 || showHeld;
  const rows = ((by === 'provider' ? byAccount : byCombo) ?? []).filter((r) => r.saved > 0 && r.k !== '(direct)');
  const delta = priorSaved != null ? deltaNote(rewritten, priorSaved, rangeWords(range)) : undefined;

  const total = profiles?.length ?? 0;
  const enabledN = (profiles ?? []).filter((p) => p.enabled).length;
  const unattachedN = (profiles ?? []).filter((p) => !(attached.get(p.id)?.length)).length;
  const openNew = () => setEditor({ open: true, editing: null, template: null });

  return (
    // No page-root fade: the route transition in App.tsx already owns the
    // page entrance. A second entrance stacked on it reads as a stutter.
    <div className="mx-auto max-w-4xl space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <SectionLabel>Compression profiles</SectionLabel>
          <p className="mt-1 text-[13px] text-mute" aria-live="polite">
            {profiles === null ? 'Loading profiles…' : total === 0 ? 'Shorten what is sent upstream, without changing what it means.' : (
              <>
                <span className="text-ink">{nfmt(total)}</span> profile{total === 1 ? '' : 's'} · {nfmt(enabledN)} enabled
                {unattachedN > 0 && <> · <span className="text-warn">{nfmt(unattachedN)} not used by any combo</span></>}
              </>
            )}
          </p>
        </div>
        <button type="button" className={cx(btn.base, btn.primary)} onClick={openNew}>＋ New profile</button>
      </div>

      {err && (
        <div role="alert" className="flex items-start justify-between gap-3 rounded-lg border border-bad/30 bg-bad-dim px-4 py-3 text-[13px] text-bad">
          <span>
            {err.text}
            {err.combos && err.combos.length > 0 && <>
              {' '}{err.combos.map((c, i) => (
                <span key={c.id}>{i > 0 && ', '}<Link className="underline underline-offset-2" to={`/combos?edit=${c.id}`}>{c.name}</Link></span>
              ))}.
              {' '}Switch those combos to another profile first.
            </>}
          </span>
          <button type="button" aria-label="Dismiss" className="text-bad/70 hover:text-bad" onClick={() => setErr(null)}>✕</button>
        </div>
      )}
      {notice && !err && (
        <div className="flex items-center justify-between gap-3 rounded-lg border border-line bg-raised/60 px-4 py-2.5 text-[13px] text-dim" role="status">
          <span>{notice}</span>
          <button type="button" className="text-mute hover:text-ink" aria-label="Dismiss" onClick={() => setNotice('')}>✕</button>
        </div>
      )}

      {profiles === null ? null : profiles.length === 0 ? (
        <div className="rounded-xl border border-line bg-surface px-6 py-12 text-center">
          <p className="text-[14px] font-medium">No profiles yet</p>
          <p className="mx-auto mt-1 max-w-md text-[13px] text-mute">A profile is an ordered list of stages that shorten requests before they go upstream. Create one, then attach it to a combo.</p>
          <button type="button" className={cx(btn.base, btn.primary, 'mt-5')} onClick={openNew}>＋ Create your first profile</button>
        </div>
      ) : (
        <Stagger className="space-y-3">
          {profiles.map((p) => {
            const users = attached.get(p.id) ?? [];
            return (
              <StaggerItem key={p.id}>
                <article className="rounded-xl border border-line bg-surface px-5 py-4">
                  <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
                    <Mono className="text-[14px] font-semibold">{p.name}</Mono>
                    <span className="ml-auto flex flex-wrap items-center gap-3">
                      <label className="flex items-center gap-2 text-[13px] text-dim">
                        <Toggle checked={p.enabled} disabled={busyID === p.id} label={`${p.enabled ? 'Pause' : 'Enable'} profile ${p.name}`}
                          onChange={(v) => void toggleEnabled(p, v)} />
                        {p.enabled ? 'Enabled' : 'Paused'}
                      </label>
                      <span className="flex items-center gap-2">
                        <button type="button" className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[13px]')} title="Run this profile on a sample you paste"
                          onClick={() => setTrying(p)}>Try it</button>
                        <button type="button" className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[13px]')} onClick={() => setEditor({ open: true, editing: p, template: null })}>Edit</button>
                        <button type="button" className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[13px]')} title="Start a new profile from this one"
                          onClick={() => setEditor({ open: true, editing: null, template: p })}>Duplicate</button>
                        <button type="button" className={cx(btn.base, btn.danger, 'px-3 py-1.5 text-[13px]')} onClick={() => askDelete(p)}>Delete</button>
                      </span>
                    </span>
                  </div>
                  <div className="mt-2.5 flex flex-wrap items-center gap-1.5">
                    {(p.stages ?? []).map((stage, i) => (
                      <span key={i} className="flex items-center gap-1.5">
                        {i > 0 && <span className="text-mute" aria-hidden>→</span>}
                        <Chip tone="neutral" title={ENGINE_INFO[stage.engine]?.note}>{stageWords(stage)}</Chip>
                      </span>
                    ))}
                  </div>
                  <p className="mt-2 text-[12px] leading-snug text-mute">{settingsWords(p)}</p>
                  <div className="mt-2 flex flex-wrap items-center gap-x-2 gap-y-1 text-[12px]">
                    {users.length === 0
                      ? <span className="text-warn">Not used by any combo, so it does nothing until one is set to use it. A request can still pick it with the header.</span>
                      : <>
                        <span className="text-mute">Used by</span>
                        {users.map((c) => <Link key={c.id} to={`/combos?edit=${c.id}`} className="rounded-md border border-line px-2 py-0.5 font-mono text-dim hover:border-line-strong hover:text-ink">{c.name}</Link>)}
                      </>}
                  </div>
                  {p.notes && <p className="mt-2 text-[13px] text-dim">{p.notes}</p>}
                </article>
              </StaggerItem>
            );
          })}
        </Stagger>
      )}

      {/* ── live savings ──────────────────────────────────────────────────
          The editor is the mechanism; this is what it actually did. Cards are
          driven by the requests summary's compression columns: rewritten counts
          ONLY rows where a rewrite shipped (applied_saved — the raw
          tokens_saved column also sweeps in legacy declined rows); held is what
          the correctly-DECLINED rewrites would have saved; the acceptance rate
          appears only when a decline exists to compare against. No meaningful
          data → the empty state alone, never filler. */}
      <Panel
        label={<SectionLabel>Actual savings · last {rangeWords(range)}</SectionLabel>}
        aside={<span className="flex flex-wrap items-center justify-end gap-x-4 gap-y-1">
          <span className="text-[12px] text-mute">
            {savingsError ? 'report unavailable' : !sum || byAccount === null ? 'loading…' : hasSavings
              ? `${formatTokens(rewritten)} rewritten${showHeld ? ` · ${formatTokens(heldNotional)} held` : ''}`
              : 'nothing recorded yet'}
          </span>
          <Segmented<RangeKey> size="sm" label="Savings range" value={range} onChange={setRange}
            options={RANGES.map((k) => ({ key: k, label: RANGE_LABEL[k] }))} />
        </span>}>
        {savingsError ? (
          <p role="alert" className="text-[13px] text-warn">Could not load compression savings. <button type="button" className="underline" onClick={() => setSavingsAttempt((n) => n + 1)}>Retry savings</button></p>
        ) : byAccount === null || sum === null ? (
          <p className="text-[13px] text-mute">Loading savings…</p>
        ) : !hasSavings ? (
          <EmptyState
            title={`No savings recorded in the last ${rangeWords(range)}.`}
            hint="Either no combo uses a profile yet, or every stage declined because the saving was too small. A stage that declines is not a failure." />
        ) : (
          <div className="space-y-4">
            <div className={cx('grid gap-3', showHitRate || showHeld ? 'sm:grid-cols-2 lg:grid-cols-3' : 'sm:grid-cols-2')}>
              <Stagger className="contents">
                <StaggerItem className="rounded-lg border border-line bg-raised/40 px-4 py-3">
                  <SectionLabel>Rewritten</SectionLabel>
                  <div className="mt-1 font-mono text-[20px] font-semibold text-ok tnum">{formatTokens(rewritten)}</div>
                  {delta && <div className={cx('mt-0.5 text-[12px]', delta.tone ?? 'text-mute')}>{delta.text}</div>}
                  <p className="mt-1 text-[12px] leading-relaxed text-mute">
                    Tokens removed from {nfmt(appliedCalls)} call{appliedCalls === 1 ? '' : 's'} that went out shorter than they arrived.
                  </p>
                </StaggerItem>
                {showHitRate && (
                  <StaggerItem className="rounded-lg border border-line bg-raised/40 px-4 py-3">
                    <SectionLabel>Rewrites that shipped</SectionLabel>
                    <div className="mt-1 font-mono text-[20px] font-semibold text-ink tnum">
                      {Math.round((appliedCalls / Math.max(1, rewritesDecided)) * 100)}%
                    </div>
                    <p className="mt-1 text-[12px] leading-relaxed text-mute">
                      {nfmt(appliedCalls)} of {nfmt(rewritesDecided)} shipped; {nfmt(declinedCalls)} were declined for saving too little. Nothing is claimed as saved for those.
                    </p>
                  </StaggerItem>
                )}
                {showHeld && (
                  <StaggerItem className="rounded-lg border border-line bg-raised/40 px-4 py-3">
                    <SectionLabel>Held back</SectionLabel>
                    <div className="mt-1 font-mono text-[20px] font-semibold text-warn tnum">{formatTokens(heldNotional)}</div>
                    <p className="mt-1 text-[12px] leading-relaxed text-mute">
                      What the {nfmt(declinedCalls)} declined rewrites would have saved. Shown on the live feed as “held”, never counted as savings.
                    </p>
                  </StaggerItem>
                )}
              </Stagger>
            </div>
            <div className="flex items-center gap-3">
              <span className="text-[12px] text-mute">Rewritten tokens by</span>
              <Segmented<By> size="sm" label="Savings breakdown" value={by} onChange={setBy}
                options={[{ key: 'provider', label: 'Provider' }, { key: 'combo', label: 'Combo' }]} />
            </div>
            <BarList
              rows={rows.map((r) => ({
                key: r.k, value: r.saved, color: VIZ.saved,
                hint: `${nfmt(r.calls)} calls · ${formatTokens(r.saved)} rewritten`,
                to: by === 'combo' ? `/requests?${new URLSearchParams({ alias: r.k, range })}` : undefined,
              }))}
              emptyLabel={`No savings recorded in the last ${rangeWords(range)}.`} />
            {declinedCalls > 0 && (
              <p className="border-t border-line pt-3 text-[12px] leading-relaxed text-mute">
                {nfmt(declinedCalls)} calls carried a rewrite that was declined, usually because it saved less than the
                profile’s minimum. The original shipped instead of breaking a cached prefix. The {formatTokens(heldNotional)}
                {' '}above is that unclaimed saving, not a loss.
              </p>
            )}
          </div>
        )}
      </Panel>

      <details className="rounded-xl border border-line bg-surface px-5 py-4 sm:px-6">
        <summary className="cursor-pointer select-none text-[13px] font-semibold">Engines <span className="font-normal text-mute">· the {ENGINE_IDS.length} stages a profile can use</span></summary>
        <dl className="mt-3 grid gap-x-6 gap-y-3 sm:grid-cols-2">
          {ENGINE_IDS.map((id) => (
            <div key={id}>
              <dt className="text-[13px] font-medium">{ENGINE_INFO[id].label}</dt>
              <dd className="mt-0.5 text-[12px] leading-relaxed text-mute">{ENGINE_INFO[id].note}</dd>
            </div>
          ))}
        </dl>
      </details>

      <section className="rounded-xl border border-line bg-surface px-5 py-4 sm:px-6">
        <SectionLabel>Using a profile</SectionLabel>
        <ul className="mt-2 space-y-1.5 text-[13px] text-dim">
          <li>A combo applies its profile to every request routed through it. Set that when you edit the combo.</li>
          <li className="flex flex-wrap items-center gap-2">
            One request can pick a profile, or switch compression off, with the header
            <Mono>x-ezllm-compression: &lt;profile-name|off&gt;</Mono>
            <CopyButton text="x-ezllm-compression: off" label="Copy off" />
          </li>
          {(profiles ?? []).length > 0 && (
            <li className="flex flex-wrap items-center gap-2">
              Copy a profile name:
              {(profiles ?? []).map((p) => <CopyButton key={p.id} text={p.name} label={p.name} />)}
            </li>
          )}
        </ul>
      </section>

      <ProfileModal target={editor} profiles={profiles ?? []} onClose={() => setEditor(CLOSED)}
        onSaved={(name, created) => { setEditor(CLOSED); setNotice(created ? `Created ${name}.` : `Saved ${name}.`); reload(); }} />
      <Modal open={trying != null} onClose={() => setTrying(null)} wide title={trying ? `Try ${trying.name}` : 'Try a profile'}
        subtitle="Runs the real engines on what you paste. Nothing is saved, sent upstream or recorded"
        footer={(close) => <button type="button" className={cx(btn.base, btn.ghost)} onClick={close}>Close</button>}>
        {trying && <PreviewPanel profileId={trying.id} />}
      </Modal>
      <ConfirmModal
        open={pendingDelete != null}
        title={pendingDelete ? `Delete compression profile “${pendingDelete.name}”?` : 'Delete profile?'}
        body="No combo uses it. Requests that name it in the x-ezllm-compression header will stop compressing."
        busy={deleting}
        onClose={() => setPendingDelete(null)}
        onConfirm={() => { if (pendingDelete) void remove(pendingDelete); }} />
    </div>
  );
}

type ReactErr = { text: string; combos?: { id: number; name: string }[] };
