// COMPRESSION — profile editor for ordered, fail-open compression pipelines.
import { useCallback, useEffect, useState } from 'react';
import {
  ApiError, compressionProfiles, del, patch, post, requests, usage,
  type CompressionProfile, type CompressionStage, type RequestsSummary, type UsageRow,
} from '../utils/api';
import { BarList, Panel, VIZ } from '../components/charts';
import { Chip, ConfirmModal, EmptyState, Mono, SectionLabel, btn, cx, formatTokens, inputCls, labelCls, nfmt, Spinner } from '../components/ui';

type StageDraft = { engine: string; option: string };
type ProfileDraft = {
  name: string;
  enabled: boolean;
  stages: StageDraft[];
  exempt_last_turn: boolean;
  min_compress_ratio: number;
  auto_trigger_tokens: number;
  notes: string;
};

const ENGINE_INFO: Record<string, { label: string; note: string; option?: { key: string; label: string; initial: string; values?: readonly string[] } }> = {
  session_dedup: { label: 'Session dedup', note: 'Drops exact duplicate system, user and assistant turns.' },
  rtk: { label: 'RTK', note: 'Filters noisy CLI/tool output; errors and failures are retained.', option: { key: 'keep_lines', label: 'Keep lines', initial: '6' } },
  headroom: { label: 'Headroom', note: 'Lossless columnar compaction for homogeneous JSON tool payloads.', option: { key: 'min_rows', label: 'Minimum rows', initial: '4' } },
  lite: { label: 'Lite', note: 'Whitespace cleanup only; words and code fences remain unchanged.', option: { key: 'blank_lines', label: 'Blank lines', initial: '1' } },
  caveman: { label: 'Caveman', note: 'Deterministic prose condensation with protected spans and safety gates.', option: { key: 'intensity', label: 'Intensity', initial: 'lite', values: ['lite', 'standard'] } },
};
const ENGINE_IDS = Object.keys(ENGINE_INFO);
const stageLabel = (id: string) => ENGINE_INFO[id]?.label ?? id;
const freshDraft = (): ProfileDraft => ({ name: '', enabled: true, stages: [{ engine: 'session_dedup', option: '' }], exempt_last_turn: true, min_compress_ratio: 0.05, auto_trigger_tokens: 0, notes: '' });
const fromProfile = (p: CompressionProfile): ProfileDraft => ({
  name: p.name,
  enabled: p.enabled,
  stages: (p.stages ?? []).map((s) => ({ engine: s.engine, option: optionValue(s) })),
  exempt_last_turn: p.exempt_last_turn,
  min_compress_ratio: p.min_compress_ratio,
  auto_trigger_tokens: p.auto_trigger_tokens,
  notes: p.notes ?? '',
});
function optionValue(stage: CompressionStage): string {
  const info = ENGINE_INFO[stage.engine]?.option;
  if (!info) return '';
  return String(stage.options?.[info.key] ?? info.initial);
}
function packStages(stages: StageDraft[]): CompressionStage[] {
  return stages.map((s) => {
    const opt = ENGINE_INFO[s.engine]?.option;
    if (!opt || !s.option.trim()) return { engine: s.engine };
    const options: Record<string, unknown> = { [opt.key]: opt.key === 'intensity' ? s.option.trim() : Number(s.option) };
    return { engine: s.engine, options };
  });
}

export default function Compression() {
  const [profiles, setProfiles] = useState<CompressionProfile[] | null>(null);
  // Live savings from the ledger: saved / context_saved per account (the
  // per-account bar list), plus the requests summary's four compression
  // columns — applied_saved is the REAL rewritten total (the per-account
  // `saved` column sweeps in legacy declined rows), declined_calls /
  // held_notional count and measure the rewrites a stage correctly refused.
  const [savings, setSavings] = useState<UsageRow[] | null>(null);
  const [sum, setSum] = useState<RequestsSummary | null>(null);
  const [draft, setDraft] = useState<ProfileDraft | null>(null);
  const [pendingDelete, setPendingDelete] = useState<CompressionProfile | null>(null);
  const [editingID, setEditingID] = useState<number | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const [notice, setNotice] = useState('');

  const reload = useCallback(() => {
    setErr('');
    compressionProfiles().then(setProfiles).catch((e) => setErr(e instanceof Error ? e.message : String(e)));
  }, []);
  useEffect(() => { reload(); }, [reload]);

  // Savings over the last 24h: per-account groups for the bar list, and the
  // requests summary (limit:1 — only its `summary` block is read) for the
  // applied/declined cards. A failure here must not take the editor down, so
  // each leaves its own panel piece empty.
  useEffect(() => {
    usage('account', '24h')
      .then((r) => setSavings(r.groups))
      .catch(() => setSavings([]));
    requests({ range: '24h', limit: 1 })
      .then((r) => setSum(r.summary))
      .catch(() => setSum(null));
  }, []);

  const startNew = () => { setEditingID(null); setDraft(freshDraft()); setErr(''); setNotice(''); };
  const startEdit = (p: CompressionProfile) => { setEditingID(p.id); setDraft(fromProfile(p)); setErr(''); setNotice(''); };
  const update = <K extends keyof ProfileDraft>(key: K, value: ProfileDraft[K]) => setDraft((d) => d ? { ...d, [key]: value } : d);
  const updateStage = (index: number, part: Partial<StageDraft>) => setDraft((d) => d ? { ...d, stages: d.stages.map((s, i) => i === index ? { ...s, ...part } : s) } : d);
  const moveStage = (index: number, delta: number) => setDraft((d) => {
    if (!d) return d;
    const next = index + delta;
    if (next < 0 || next >= d.stages.length) return d;
    const stages = [...d.stages];
    [stages[index], stages[next]] = [stages[next], stages[index]];
    return { ...d, stages };
  });

  const save = async () => {
    if (!draft) return;
    if (draft.stages.some((s) => s.engine === 'caveman' && !['lite', 'standard'].includes(s.option.trim())))
      return setErr('Caveman intensity must be lite or standard.');
    if (draft.stages.some((s) => ['rtk', 'headroom', 'lite'].includes(s.engine) && (!Number.isInteger(Number(s.option)) || Number(s.option) < 0)))
      return setErr('Numeric stage options must be non-negative whole numbers.');
    setBusy(true); setErr(''); setNotice('');
    const body = {
      name: draft.name.trim(), enabled: draft.enabled, stages: packStages(draft.stages),
      exempt_last_turn: draft.exempt_last_turn, min_compress_ratio: draft.min_compress_ratio,
      fail_open: true, auto_trigger_tokens: draft.auto_trigger_tokens, notes: draft.notes.trim(),
    };
    try {
      if (editingID == null) await post<CompressionProfile>('/admin/compression-profiles', body);
      else await patch<CompressionProfile>(`/admin/compression-profiles/${editingID}`, body);
      setDraft(null); setEditingID(null); setNotice('Profile saved.'); reload();
    } catch (e) {
      setErr(e instanceof ApiError ? e.detail.message : e instanceof Error ? e.message : String(e));
    } finally { setBusy(false); }
  };

  const toggleEnabled = async (p: CompressionProfile) => {
    setBusy(true); setErr('');
    try { await patch(`/admin/compression-profiles/${p.id}`, { enabled: !p.enabled }); reload(); }
    catch (e) { setErr(e instanceof ApiError ? e.detail.message : e instanceof Error ? e.message : String(e)); }
    finally { setBusy(false); }
  };
  const remove = async (p: CompressionProfile) => {
    setBusy(true); setErr('');
    try { await del(`/admin/compression-profiles/${p.id}`); reload(); }
    catch (e) { setErr(e instanceof ApiError ? e.detail.message : e instanceof Error ? e.message : String(e)); }
    finally { setBusy(false); }
  };

  const savedTotal = savings?.reduce((a, r) => a + r.saved, 0) ?? 0;
  // The REAL rewritten total: applied_saved only counts rows where a rewrite
  // actually shipped. `savedTotal` (the raw per-account column) additionally
  // sweeps in ~21k legacy tokens_saved sitting on declined rows — direction
  // is right, magnitude is not, so the card reads applied_saved when the
  // summary answered and falls back to the raw column while it loads.
  const rewritten = sum ? sum.applied_saved : savedTotal;
  const declinedCalls = sum?.declined_calls ?? 0;
  const heldNotional = sum?.held_notional ?? 0;
  const appliedCalls = sum?.applied_calls ?? 0;
  const rewritesDecided = appliedCalls + declinedCalls;
  // Hit-rate is meaningful only when a stage actually refused something;
  // 100% of one decision is a rounding error wearing a stat's clothes.
  const showHitRate = declinedCalls > 0;
  const showHeld = heldNotional > 0;
  const hasSavings = rewritten > 0 || showHeld;

  return (
    <div className="mx-auto max-w-4xl animate-fade-up space-y-4">
      <section className="rounded-xl border border-line bg-surface px-5 py-5 sm:px-6">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div><SectionLabel>Compression engines</SectionLabel><p className="mt-1 text-[12px] text-mute">Order matters: each enabled stage receives the previous stage’s output.</p></div>
          <button type="button" className={cx(btn.base, btn.primary)} onClick={startNew}>＋ New profile</button>
        </div>
        <div className="mt-4 grid gap-2 sm:grid-cols-2">
          {ENGINE_IDS.map((id) => <div key={id} className="rounded-lg border border-line bg-raised/50 px-3.5 py-3">
            <div className="flex items-center justify-between"><Mono className="font-semibold">{ENGINE_INFO[id].label}</Mono><Chip tone="ok">live</Chip></div>
            <p className="mt-1.5 text-[11.5px] leading-relaxed text-mute">{ENGINE_INFO[id].note}</p>
          </div>)}
        </div>
      </section>

      {err && <div role="alert" className="rounded-lg border border-bad/30 bg-bad-dim px-4 py-3 text-[13px] text-bad">{err}</div>}
      {notice && <div className="rounded-lg border border-ok/30 bg-ok-dim px-4 py-3 text-[13px] text-ok">{notice}</div>}

      {/* ── live savings ──────────────────────────────────────────────────
          The editor above is the mechanism; this is what it actually did in
          the last 24h. Cards are driven by the requests summary's compression
          columns: rewritten counts ONLY rows where a rewrite shipped
          (applied_saved — the raw tokens_saved column also sweeps in legacy
          declined rows); held is what the correctly-DECLINED rewrites would
          have saved; the rewrite-acceptance rate appears only when a decline
          exists to compare against. No meaningful data → the rewritten card
          alone, never filler. */}
      <Panel
        label={<SectionLabel>Actual savings · last 24h</SectionLabel>}
        aside={<span className="text-[11px] text-mute">
          {hasSavings
            ? `${formatTokens(rewritten)} rewritten${showHeld ? ` · ${formatTokens(heldNotional)} held` : ''}`
            : 'nothing recorded yet'}
        </span>}>
        {savings === null ? (
          <p className="text-[12px] text-mute">Loading savings…</p>
        ) : !hasSavings ? (
          <EmptyState
            title="No savings recorded in the last 24h."
            hint="Either traffic is small enough that every stage declined the minimum-ratio floor, or no profile is attached to a combo. A stage that declines is not a failure — see the note below." />
        ) : (
          <div className="space-y-4">
            <div className={cx('grid gap-3', showHitRate || showHeld ? 'sm:grid-cols-2 lg:grid-cols-3' : 'sm:grid-cols-2')}>
              <div className="rounded-lg border border-line bg-raised/40 px-4 py-3">
                <SectionLabel>rewritten (shipped shorter)</SectionLabel>
                <div className="mt-1 font-mono text-[20px] font-semibold text-ok tnum">{formatTokens(rewritten)}</div>
                <p className="mt-1 text-[11px] leading-relaxed text-mute">
                  {nfmt(appliedCalls)} call{appliedCalls === 1 ? '' : 's'} actually went out shorter than they arrived. A deterministic rewrite, auditable.
                </p>
              </div>
              {showHitRate && (
                <div className="rounded-lg border border-line bg-raised/40 px-4 py-3">
                  <SectionLabel>rewrite acceptance</SectionLabel>
                  <div className="mt-1 font-mono text-[20px] font-semibold text-ink tnum">
                    {Math.round((appliedCalls / Math.max(1, rewritesDecided)) * 100)}%
                  </div>
                  <p className="mt-1 text-[11px] leading-relaxed text-mute">
                    {nfmt(appliedCalls)} of {nfmt(rewritesDecided)} decided rewrites shipped; {nfmt(declinedCalls)} were declined by the ratio floor (tuning data — nothing is claimed as saved).
                  </p>
                </div>
              )}
              {showHeld && (
                <div className="rounded-lg border border-line bg-raised/40 px-4 py-3">
                  <SectionLabel>held (declined rewrites)</SectionLabel>
                  <div className="mt-1 font-mono text-[20px] font-semibold text-warn tnum">{formatTokens(heldNotional)}</div>
                  <p className="mt-1 text-[11px] leading-relaxed text-mute">
                    What the {nfmt(declinedCalls)} declined rewrites WOULD have saved — recorded as notional on the live feed (“held N”), never counted as real savings.
                  </p>
                </div>
              )}
            </div>
            <BarList
              rows={savings.filter((r) => r.saved > 0).map((r) => ({
                key: r.k,
                value: r.saved,
                color: VIZ.saved,
                hint: `${nfmt(r.calls)} calls · ${formatTokens(r.saved)} rewritten`,
              }))}
              emptyLabel="No savings recorded in the last 24h." />
            {declinedCalls > 0 && (
              <p className="border-t border-line pt-3 text-[11.5px] leading-relaxed text-mute">
                A stage can also correctly DECLINE: {nfmt(declinedCalls)} calls in this window carried a real
                rewrite that was rejected — usually the profile's minimum-ratio floor, so the original shipped
                rather than break a cached prefix. Their {formatTokens(heldNotional)} is the held figure above,
                not a loss.
              </p>
            )}
          </div>
        )}
      </Panel>

      {draft && <section className="rounded-xl border border-accent-dim bg-surface px-5 py-5 sm:px-6">
        <div className="flex items-start justify-between gap-4"><div><SectionLabel>{editingID == null ? 'Create profile' : 'Edit profile'}</SectionLabel><p className="mt-1 text-[12px] text-mute">Compose and order deterministic stages. Save validates against the backend engine registry.</p></div>
          <button type="button" className={cx(btn.base, btn.ghost)} onClick={() => setDraft(null)} disabled={busy}>Cancel</button></div>
        <div className="mt-4 grid gap-4 sm:grid-cols-2">
          <div><label className={labelCls} htmlFor="cp-name">Profile name</label><input id="cp-name" className={inputCls} value={draft.name} onChange={(e) => update('name', e.target.value)} placeholder="safe-default" /></div>
          <div><label className={labelCls} htmlFor="cp-trigger">Auto-trigger threshold (tokens; 0 = always)</label><input id="cp-trigger" type="number" min={0} className={inputCls} value={draft.auto_trigger_tokens} onChange={(e) => update('auto_trigger_tokens', Math.max(0, Number(e.target.value) || 0))} /></div>
        </div>
        <div className="mt-4 flex flex-wrap items-center gap-x-6 gap-y-3">
          <label className="flex items-center gap-2 text-[12px] text-dim"><input type="checkbox" checked={draft.enabled} onChange={(e) => update('enabled', e.target.checked)} /> Enabled</label>
          <label className="flex items-center gap-2 text-[12px] text-dim"><input type="checkbox" checked={draft.exempt_last_turn} onChange={(e) => update('exempt_last_turn', e.target.checked)} /> Exempt final user turn</label>
          <label className="flex items-center gap-2 text-[12px] text-dim">Minimum ratio <input type="number" min={0.05} max={0.95} step={0.01} className={cx(inputCls, 'w-24 font-mono')} value={draft.min_compress_ratio} onChange={(e) => update('min_compress_ratio', Number(e.target.value) || 0.05)} /></label>
        </div>
        <div className="mt-5 flex items-center justify-between gap-3"><SectionLabel>Pipeline</SectionLabel><button type="button" className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[12px]')} onClick={() => update('stages', [...draft.stages, { engine: 'lite', option: '' }])}>＋ Add stage</button></div>
        <div className="mt-2 space-y-2">
          {draft.stages.map((stage, i) => {
            const info = ENGINE_INFO[stage.engine];
            return <div key={`${i}-${stage.engine}`} className="grid items-end gap-2 rounded-lg border border-line bg-raised/40 p-3 sm:grid-cols-[auto_minmax(0,1fr)_minmax(0,1fr)_auto]">
              <span className="pb-2 font-mono text-[11px] text-mute">{String(i + 1).padStart(2, '0')}</span>
              <div><label className={labelCls} htmlFor={`cp-engine-${i}`}>Engine</label><select id={`cp-engine-${i}`} className={inputCls} value={stage.engine} onChange={(e) => updateStage(i, { engine: e.target.value, option: ENGINE_INFO[e.target.value]?.option?.initial ?? '' })}>{ENGINE_IDS.map((id) => <option key={id} value={id}>{stageLabel(id)}</option>)}{!ENGINE_IDS.includes(stage.engine) && <option value={stage.engine} disabled>{stageLabel(stage.engine)} (not offered)</option>}</select></div>
              <div>{info?.option ? (
                info.option.values ? (
                  <>
                    <label className={labelCls} htmlFor={`cp-option-${i}`}>{info.option.label}</label>
                    <select id={`cp-option-${i}`} className={inputCls} value={stage.option || info.option.initial}
                      onChange={(e) => updateStage(i, { option: e.target.value })}>
                      {info.option.values.map((v) => <option key={v} value={v}>{v}</option>)}
                    </select>
                  </>
                ) : (
                  <>
                    <label className={labelCls} htmlFor={`cp-option-${i}`}>{info.option.label}</label>
                    <input id={`cp-option-${i}`} className={inputCls} value={stage.option || info.option.initial} onChange={(e) => updateStage(i, { option: e.target.value })} />
                  </>
                )
              ) : <p className="pb-2 text-[11px] text-mute">No stage options</p>}</div>
              <div className="flex gap-1"><button type="button" className={cx(btn.base, btn.ghost, 'px-2 py-1.5')} aria-label="Move stage up" disabled={i === 0} onClick={() => moveStage(i, -1)}>↑</button><button type="button" className={cx(btn.base, btn.ghost, 'px-2 py-1.5')} aria-label="Move stage down" disabled={i === draft.stages.length - 1} onClick={() => moveStage(i, 1)}>↓</button><button type="button" className={cx(btn.base, btn.ghost, 'px-2 py-1.5 text-bad')} aria-label="Remove stage" onClick={() => update('stages', draft.stages.filter((_, j) => j !== i))}>×</button></div>
            </div>;
          })}
          {draft.stages.length === 0 && <p className="rounded-lg border border-dashed border-line px-4 py-5 text-center text-[12px] text-mute">Add at least one stage before saving.</p>}
        </div>
        <div className="mt-4"><label className={labelCls} htmlFor="cp-notes">Notes</label><textarea id="cp-notes" rows={2} className={inputCls} value={draft.notes} onChange={(e) => update('notes', e.target.value)} placeholder="Optional usage notes" /></div>
        <div className="mt-5 flex justify-end"><button type="button" disabled={busy || !draft.name.trim() || draft.stages.length === 0} className={cx(btn.base, btn.primary)} onClick={() => void save()}>{busy ? <Spinner /> : null} Save profile</button></div>
      </section>}

      <section className="rounded-xl border border-line bg-surface px-5 py-5 sm:px-6">
        <div className="flex items-center justify-between"><SectionLabel>Profiles</SectionLabel><button type="button" className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[12px]')} onClick={reload}>Refresh</button></div>
        {profiles === null ? <p className="mt-3 text-[12px] text-mute">Loading profiles…</p> : profiles.length === 0 ? <p className="mt-3 text-[12.5px] text-mute">No profiles yet. Create one above, then attach it to a combo.</p> : (
          <div className="mt-3 space-y-3">{profiles.map((p) => <article key={p.id} className="rounded-lg border border-line bg-raised/30 px-4 py-3">
            <div className="flex flex-wrap items-center gap-2"><Mono className="font-semibold">{p.name}</Mono><Chip tone={p.enabled ? 'ok' : 'warn'}>{p.enabled ? 'enabled' : 'paused'}</Chip><span className="ml-auto text-[11px] text-mute">min {Math.round(p.min_compress_ratio * 100)}% · {p.exempt_last_turn ? 'final turn exempt' : 'final turn editable'} · fail-open</span></div>
            <div className="mt-2 flex flex-wrap items-center gap-1.5">{(p.stages ?? []).map((stage, i) => <span key={i} className="flex items-center gap-1.5">{i > 0 && <span className="text-mute">→</span>}<Chip tone="neutral"><Mono>{stageLabel(stage.engine)}</Mono></Chip>{stage.options && <Mono className="text-[10px] text-mute">{JSON.stringify(stage.options)}</Mono>}</span>)}</div>
            {p.notes && <p className="mt-2 text-[12px] text-dim">{p.notes}</p>}
            <div className="mt-3 flex justify-end gap-2"><button type="button" disabled={busy} className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[12px]')} onClick={() => void toggleEnabled(p)}>{p.enabled ? 'Pause' : 'Enable'}</button><button type="button" disabled={busy} className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[12px]')} onClick={() => startEdit(p)}>Edit</button><button type="button" disabled={busy} className={cx(btn.base, btn.ghost, 'px-3 py-1.5 text-[12px] text-bad')} onClick={() => setPendingDelete(p)}>Delete</button></div>
          </article>)}</div>
        )}
      </section>

      <ConfirmModal
        open={pendingDelete != null}
        title={pendingDelete ? `Delete compression profile “${pendingDelete.name}”?` : 'Delete profile?'}
        body="Combos attached to this profile will stop compressing until you attach another."
        busy={busy}
        onClose={() => setPendingDelete(null)}
        onConfirm={() => { if (pendingDelete) void remove(pendingDelete).then(() => setPendingDelete(null)); }} />

      <section className="rounded-xl border border-line bg-surface px-5 py-5 sm:px-6"><SectionLabel>Wiring</SectionLabel><ul className="mt-3 space-y-1.5 text-[13px] text-dim"><li>· Per-request override uses <Mono>x-ezllm-compression: &lt;profile-name|off&gt;</Mono>.</li></ul></section>
    </div>
  );
}
