// Compression-profile logic shared by the Compression page's cards and editor:
// the engine catalogue, the draft shape and its wire form, what the editor must
// refuse, advice on a pipeline's order, and plain-language readouts. Kept out of
// the component so the card and the editor describe a profile the same way.
import type { CompressionProfile, CompressionStage } from './api';
import { formatTokens } from './lib';

// extra carries every stage option this editor has no field for, so saving a
// profile never silently drops options set through the API. toolClasses is
// rtk's tool_classes override, edited as "name=class, name=class". uid is a
// UI-only key so a reordered stage slides instead of re-rendering in place.
export type StageDraft = { uid: number; engine: string; option: string; extra?: Record<string, unknown>; toolClasses?: string };
export type ProfileDraft = {
  name: string;
  enabled: boolean;
  stages: StageDraft[];
  exempt_last_turn: boolean;
  min_compress_ratio: number;
  auto_trigger_tokens: number;
  fresh_tool_output: 'keep' | 'compress';
  notes: string;
};

type EngineOption = { key: string; label: string; initial: string; values?: readonly string[]; words: (v: string) => string };
export const ENGINE_INFO: Record<string, { label: string; note: string; option?: EngineOption }> = {
  session_dedup: { label: 'Session dedup', note: 'Drops exact duplicate system, user and assistant turns.' },
  rtk: { label: 'RTK', note: 'Filters noisy CLI/tool output; errors and failures are retained.', option: { key: 'keep_lines', label: 'Keep lines', initial: '6', words: (v) => `keeps ${v} lines` } },
  headroom: { label: 'Headroom', note: 'Lossless columnar compaction for homogeneous JSON tool payloads.', option: { key: 'min_rows', label: 'Minimum rows', initial: '4', words: (v) => `from ${v} rows` } },
  lite: { label: 'Lite', note: 'Whitespace cleanup only; words and code fences remain unchanged.', option: { key: 'blank_lines', label: 'Blank lines', initial: '1', words: (v) => `${v} blank ${v === '1' ? 'line' : 'lines'} kept` } },
  ansi: { label: 'ANSI cleanup', note: 'Strips terminal escape codes and collapses carriage-return progress lines to what the terminal showed (log/search output only).' },
  fold: { label: 'Fold repeats', note: 'Collapses runs of exactly identical log lines into one line plus a count (log/search output only).', option: { key: 'min_repeat', label: 'Minimum run', initial: '3', words: (v) => `runs of ${v}+` } },
  jsonmin: { label: 'JSON minify', note: 'Removes insignificant whitespace from pretty-printed JSON tool output; every key, number and escape stays byte-identical. Never file reads.' },
  tool_ref: { label: 'Tool reference', note: 'A tool result byte-identical to an earlier one becomes a reference to it; the earlier copy stays. Every tool. Place after the other tool-output stages.', option: { key: 'min_bytes', label: 'Minimum bytes', initial: '200', words: (v) => `from ${v} bytes` } },
  blobs: { label: 'Blobs', note: 'Long base64 runs and data: URIs in tool output become a marker with their length and hash. Never file reads.', option: { key: 'min_bytes', label: 'Minimum bytes', initial: '1024', words: (v) => `from ${v} bytes` } },
  caveman: { label: 'Caveman', note: 'Deterministic prose condensation with protected spans and safety gates.', option: { key: 'intensity', label: 'Intensity', initial: 'lite', values: ['lite', 'standard'], words: (v) => v } },
};
export const ENGINE_IDS = Object.keys(ENGINE_INFO);
export const stageLabel = (id: string) => ENGINE_INFO[id]?.label ?? id;
const NUMERIC_ENGINES = ['rtk', 'headroom', 'lite', 'fold', 'tool_ref', 'blobs'];

let uidSeq = 0;
export const newStage = (engine: string): StageDraft => ({ uid: ++uidSeq, engine, option: ENGINE_INFO[engine]?.option?.initial ?? '' });

export const freshDraft = (): ProfileDraft => ({
  name: '', enabled: true, stages: [newStage('session_dedup')], exempt_last_turn: true,
  min_compress_ratio: 0.05, auto_trigger_tokens: 0, fresh_tool_output: 'keep', notes: '',
});

function optionValue(stage: CompressionStage): string {
  const info = ENGINE_INFO[stage.engine]?.option;
  if (!info) return '';
  return String(stage.options?.[info.key] ?? info.initial);
}
function extraOptions(stage: CompressionStage): Record<string, unknown> {
  const key = ENGINE_INFO[stage.engine]?.option?.key;
  const rest: Record<string, unknown> = { ...(stage.options ?? {}) };
  if (key) delete rest[key];
  delete rest.tool_classes;
  return rest;
}
function toolClassesText(stage: CompressionStage): string {
  const tc = stage.options?.tool_classes;
  if (!tc || typeof tc !== 'object') return '';
  return Object.entries(tc as Record<string, unknown>).map(([k, v]) => `${k}=${String(v)}`).join(', ');
}
export const fromProfile = (p: CompressionProfile): ProfileDraft => ({
  name: p.name,
  enabled: p.enabled,
  stages: (p.stages ?? []).map((s) => ({ uid: ++uidSeq, engine: s.engine, option: optionValue(s), extra: extraOptions(s), toolClasses: toolClassesText(s) })),
  exempt_last_turn: p.exempt_last_turn,
  min_compress_ratio: p.min_compress_ratio,
  auto_trigger_tokens: p.auto_trigger_tokens,
  fresh_tool_output: p.fresh_tool_output === 'compress' ? 'compress' : 'keep',
  notes: p.notes ?? '',
});

const TOOL_CLASSES = ['log', 'search', 'preserve'];
/** Reads "name=class, …"; null when any entry is malformed. */
export function parseToolClasses(text: string): Record<string, string> | null {
  const out: Record<string, string> = {};
  for (const part of text.split(',').map((p) => p.trim()).filter(Boolean)) {
    const [name, cls] = part.split('=').map((x) => x?.trim() ?? '');
    if (!name || !TOOL_CLASSES.includes(cls.toLowerCase())) return null;
    out[name] = cls.toLowerCase();
  }
  return out;
}

export function packStages(stages: StageDraft[]): CompressionStage[] {
  return stages.map((s) => {
    const opt = ENGINE_INFO[s.engine]?.option;
    const options: Record<string, unknown> = { ...(s.extra ?? {}) };
    if (opt && s.option.trim()) options[opt.key] = opt.key === 'intensity' ? s.option.trim() : Number(s.option);
    if (s.engine === 'rtk' && s.toolClasses?.trim()) options.tool_classes = parseToolClasses(s.toolClasses) ?? {};
    return Object.keys(options).length ? { engine: s.engine, options } : { engine: s.engine };
  });
}

/** What goes to the server for a draft (save and preview share it). */
export const profileBody = (d: ProfileDraft) => ({
  name: d.name.trim(), enabled: d.enabled, stages: packStages(d.stages),
  exempt_last_turn: d.exempt_last_turn, min_compress_ratio: d.min_compress_ratio,
  fail_open: true, auto_trigger_tokens: d.auto_trigger_tokens, fresh_tool_output: d.fresh_tool_output, notes: d.notes.trim(),
});

// ── validation ──────────────────────────────────────────────────────────────
export type DraftErrors = { name?: string; trigger?: string; ratio?: string; stages?: string; stage: Record<number, string> };

/** Everything the editor must refuse, each with a reason. `others` are the other
 *  profiles' names: saving under a taken name would overwrite or collide. */
export function validateDraft(d: ProfileDraft, others: string[]): DraftErrors {
  const e: DraftErrors = { stage: {} };
  const name = d.name.trim();
  if (!name) e.name = 'Give the profile a name: combos and the x-ezllm-compression header pick it by name.';
  else if (others.some((n) => n.toLowerCase() === name.toLowerCase())) e.name = `A profile called "${others.find((n) => n.toLowerCase() === name.toLowerCase())}" already exists.`;
  else if (name.toLowerCase() === 'off') e.name = '"off" is reserved: it switches compression off in the x-ezllm-compression header.';
  if (!Number.isInteger(d.auto_trigger_tokens) || d.auto_trigger_tokens < 0) e.trigger = 'A whole number of tokens, 0 or more.';
  if (!(d.min_compress_ratio >= 0.05 && d.min_compress_ratio <= 0.95)) e.ratio = 'Between 5% and 95%.';
  if (d.stages.length === 0) e.stages = 'Add at least one stage.';
  for (const s of d.stages) {
    const info = ENGINE_INFO[s.engine];
    if (s.engine === 'caveman' && !['lite', 'standard'].includes(s.option.trim())) e.stage[s.uid] = 'Intensity must be lite or standard.';
    else if (NUMERIC_ENGINES.includes(s.engine) && (!/^\d+$/.test(s.option.trim()))) e.stage[s.uid] = `${info?.option?.label ?? 'Option'} must be a whole number, 0 or more.`;
    else if (s.engine === 'rtk' && s.toolClasses?.trim() && parseToolClasses(s.toolClasses) === null) e.stage[s.uid] = 'Tool classes must read name=log, name=search or name=preserve, comma-separated.';
  }
  return e;
}
export const hasErrors = (e: DraftErrors) => !!(e.name || e.trigger || e.ratio || e.stages || Object.keys(e.stage).length);

// ── advice ──────────────────────────────────────────────────────────────────
const TOOL_OUTPUT_ENGINES = ['rtk', 'ansi', 'fold', 'jsonmin', 'blobs', 'headroom'];

/** Order and repetition worth a second look. Advice, never a refusal: a profile
 *  that triggers it still saves. */
export function pipelineAdvice(stages: { engine: string }[]): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  for (const s of stages) {
    if (seen.has(s.engine) && !out.some((x) => x.includes(stageLabel(s.engine)))) out.push(`${stageLabel(s.engine)} appears more than once. A second pass over the same text rarely finds more.`);
    seen.add(s.engine);
  }
  const ref = stages.findIndex((s) => s.engine === 'tool_ref');
  if (ref >= 0 && stages.some((s, i) => i > ref && TOOL_OUTPUT_ENGINES.includes(s.engine))) {
    out.push('Tool reference works best last: later stages change a result after it was compared with the earlier ones.');
  }
  return out;
}

// ── readouts ────────────────────────────────────────────────────────────────
/** "RTK · keeps 6 lines": a stage as the operator thinks of it, not as JSON. */
export function stageWords(stage: CompressionStage): string {
  const info = ENGINE_INFO[stage.engine];
  const parts: string[] = [];
  if (info?.option) parts.push(info.option.words(optionValue(stage)));
  const tc = stage.options?.tool_classes;
  if (tc && typeof tc === 'object') {
    const n = Object.keys(tc as object).length;
    if (n > 0) parts.push(`${n} tool class${n === 1 ? '' : 'es'}`);
  }
  return parts.length ? `${stageLabel(stage.engine)} · ${parts.join(', ')}` : stageLabel(stage.engine);
}

const pctOf = (r: number) => `${Math.round(r * 100)}%`;
/** A profile's settings as one plain sentence. */
export function settingsWords(p: Pick<CompressionProfile, 'min_compress_ratio' | 'exempt_last_turn' | 'auto_trigger_tokens' | 'fresh_tool_output'>): string {
  const parts = [`Keeps a rewrite only if it saves at least ${pctOf(p.min_compress_ratio)}`];
  parts.push(p.exempt_last_turn ? 'leaves the final turn alone' : 'may rewrite the final turn');
  if (p.fresh_tool_output === 'compress') parts.push('trims a fresh tool result on its first send');
  if (p.auto_trigger_tokens > 0) parts.push(`starts above ${formatTokens(p.auto_trigger_tokens)} tokens`);
  return `${parts.join(' · ')}.`;
}

export const copyName = (name: string, taken: string[]): string => {
  const lower = new Set(taken.map((n) => n.toLowerCase()));
  let n = `${name}-copy`;
  for (let i = 2; lower.has(n.toLowerCase()); i++) n = `${name}-copy-${i}`;
  return n;
};

// ── starting points ─────────────────────────────────────────────────────────
export type Preset = { key: string; label: string; hint: string; name: string; stages: { engine: string; option?: string }[]; fresh_tool_output?: 'keep' | 'compress' };
export const PRESETS: Preset[] = [
  { key: 'safe', label: 'Safe default', hint: 'Drops exact duplicate turns and tidies whitespace. Words never change.', name: 'safe-default', stages: [{ engine: 'session_dedup' }, { engine: 'lite' }] },
  { key: 'agent', label: 'Agent tool output', hint: 'For coding agents: cleans, folds and trims noisy tool results, and references repeats.', name: 'agent-output', stages: [{ engine: 'ansi' }, { engine: 'fold' }, { engine: 'rtk' }, { engine: 'jsonmin' }, { engine: 'blobs' }, { engine: 'tool_ref' }] },
  { key: 'prose', label: 'Prose condensation', hint: 'Also condenses prose. The only preset that rewrites wording.', name: 'prose-condense', stages: [{ engine: 'session_dedup' }, { engine: 'lite' }, { engine: 'caveman', option: 'standard' }] },
];
export function draftFromPreset(p: Preset, current: ProfileDraft): ProfileDraft {
  return {
    ...current,
    name: current.name.trim() ? current.name : p.name,
    stages: p.stages.map((s) => { const st = newStage(s.engine); if (s.option) st.option = s.option; return st; }),
    fresh_tool_output: p.fresh_tool_output ?? 'keep',
  };
}
