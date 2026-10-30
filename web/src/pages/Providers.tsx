// PROVIDERS — account cards, add-provider wizard modal, per-key actions, model drawer.
import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { motion } from 'motion/react';
import {
  ApiError, AuthError, del, get, ghDeviceCancel, ghDeviceStart, ghDeviceStatus, listBridges,
  modelRules, patch, post, postKeyTestStream, setModelPin,
  setPrice, deletePrice, startSweep, stopSweep, sweepStatus,
  KINDS, STEPS,
  type Account, type ApiErrorDetail, type Bridge, type GhDeviceStart, type GhDeviceStatus, type KeyRow, type ModelRule, type ModelRow, type Price, type SweepStatus, type TestResult, type TestStep,
} from '../utils/api';
import { useSearchParams } from 'react-router-dom';
import { Chip, ConfirmModal, Modal, SecretInput, SectionLabel, Segmented, Skeleton, SkeletonCards, Spinner, Toggle, btn, cx, inputCls, labelCls, relTime, Mono } from '../components/ui';
import { Collapse, Stagger, StaggerItem } from '../components/motion';
import { bridgeUpdateText, fmtMoney, nfmt, UPDATE_TONE } from '../utils/lib';
import { accountIssues, type Issue } from '../utils/health';

// ── helpers ─────────────────────────────────────────────────────────────

const slugify = (s: string) =>
  s.toLowerCase().replace(/[^a-z0-9._-]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 63);

function ErrBox({ e, onDismiss }: { e: ApiErrorDetail | null; onDismiss: () => void }) {
  if (!e) return null;
  return (
    <div className="rounded-lg border border-bad/30 bg-bad-dim px-4 py-3 animate-fade-in">
      <div className="flex items-start justify-between gap-3">
        <p className="text-[13px] text-bad">{e.message}</p>
        <button type="button" onClick={onDismiss} className="shrink-0 text-mute hover:text-ink" aria-label="dismiss">✕</button>
      </div>
      {e.upstream_error?.message && (
        <Mono className="mt-1 block text-[12px] text-dim">{e.upstream_error.type ? `${e.upstream_error.type}: ` : ''}{e.upstream_error.message}</Mono>
      )}
      {e.hint && <p className="mt-2 text-[12px] leading-relaxed text-mute">{e.hint}</p>}
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
// Transient probe status: parse the real upstream status the tester already
// embeds in a failed step's detail ("…HTTP 503…"). Purely informational,
// never persisted — gone the moment the modal closes or the page refreshes.
// It answers "why did this go red" at a glance: 404 = gone/retired, 401/403
// = credential, 429/5xx = capacity, no answer = the provider never spoke.
function probeStatus(detail?: string): string | null {
  const m = /HTTP (\d{3})/.exec(detail ?? '');
  if (m) return `HTTP ${m[1]}`;
  if (/transport error|did not answer/i.test(detail ?? '')) return 'no answer';
  return null;
}

function StepList({ steps, running }: { steps: TestStep[] | null; running: boolean }) {
  const byStep = new Map((steps ?? []).map((s) => [s.step, s]));
  return (
    <ol className="flex flex-wrap items-center gap-x-1.5 gap-y-1 font-mono text-[12px]">
      {STEP_ORDER.map((name, idx) => {
        const s = byStep.get(name);
        const state = s ? (s.ok ? 'ok' : 'bad') : running && idx <= byStep.size ? 'pending' : 'idle';
        const tip = s?.detail ? `${s.detail}${s.ms != null ? ` · ${s.ms}ms` : ''}` : name;
        const status = state === 'bad' ? probeStatus(s?.detail) : null;
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
              {status && <span className="rounded bg-bad/15 px-1 text-[11px] text-bad">{status}</span>}
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
  base_url: string; keyLabel: string; apiKey: string; managementKey: string;
  probe_delay_ms: number; notes: string; skipInference: boolean;
  advOpen: boolean; requiresSessionHeader: boolean; customHeaders: string;
  accountId: string;
};

const emptyDraft: Draft = {
  name: '', namespace: '', nsTouched: false, kind: 'opencode-go', preset: 'opencode-go',
  base_url: 'https://opencode.ai/zen/go/v1', keyLabel: 'primary', apiKey: '', managementKey: '',
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
  // keyHint is the placeholder/one-liner for credentials that are NOT a
  // pasteable API key: a browser cookie pair, a session JSON, an OAuth
  // sessionKey. Shown under the key field when this preset is selected.
  keyHint?: string;
  // multilineKey swaps the masked single-line input for a textarea. Cookie
  // jars arrive as several lines (or one long semicolon header) and simply do
  // not fit one input row — the field must hold them before the backend can
  // extract what it needs.
  multilineKey?: boolean;
  // bridge names an in-process subscription bridge (GET /admin/bridges). The
  // server supplies its key, so the key field becomes a status panel.
  bridge?: Bridge['id'];
  usageConvention?: string;
}> = {
  // Subscription bridges: the official CLI behind a loopback surface. No key to
  // paste — docs/claude-bridge-runbook.md, docs/codex-bridge-runbook.md,
  // docs/agy-bridge-runbook.md.
  'claude-sub': {
    label: 'Claude Pro/Max', kind: 'anthropic-compatible', baseURL: 'http://127.0.0.1:8790/v1',
    bridge: 'claude-sub', usageConvention: 'anthropic',
  },
  'chatgpt-sub': {
    label: 'ChatGPT Plus/Pro', kind: 'openai-compatible', baseURL: 'http://127.0.0.1:8791/v1',
    bridge: 'chatgpt-sub',
  },
  'antigravity-sub': {
    label: 'Google Antigravity', kind: 'openai-compatible', baseURL: 'http://127.0.0.1:8792/v1',
    bridge: 'antigravity-sub',
  },
  'claude-platform': { label: 'Claude Platform', kind: 'anthropic-compatible', baseURL: 'https://api.anthropic.com' },
  // Web-protocol providers: no API key — credentials are browser sessions.
  // The sentinel base URL is rewritten to the in-process bridge at request
  // time (docs/claude-web-runbook.md, docs/gemini-web-runbook.md).
  'claude-web': {
    label: 'Claude.ai (web session)', kind: 'claude-web', baseURL: 'internal://claudeweb',
    keyHint: 'claude.ai sessionKey cookie, pasted as sessionKey=sk-ant-… (runbook: DevTools → Cookies → claude.ai)',
  },
  'opencode-go': { label: 'OpenCode Go', kind: 'opencode-go', baseURL: 'https://opencode.ai/zen/go/v1', sessionHeader: true },
  'xiaomi-mimo-token-plan': { label: 'Xiaomi MiMo token plan', kind: 'openai-compatible', baseURL: 'https://token-plan-sgp.xiaomimimo.com/v1' },
  'alibaba-model-studio-token-plan': { label: 'Alibaba Cloud Model Studio token plan', kind: 'openai-compatible', baseURL: 'https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1' },
  'commandcode': { label: 'Command Code', kind: 'openai-compatible', baseURL: 'https://api.commandcode.ai/provider/v1' },
  'deepseek': { label: 'DeepSeek', kind: 'openai-compatible', baseURL: 'https://api.deepseek.com' },
  'groq': { label: 'Groq', kind: 'openai-compatible', baseURL: 'https://api.groq.com/openai/v1' },
  'github-copilot': {
    label: 'GitHub Copilot', kind: 'github-copilot', baseURL: 'https://api.githubcopilot.com',
    keyHint: 'Start the GitHub login below and tap Authorize — no terminal needed. Already have a session JSON from `ezllm github-login`? Paste it instead.',
  },
  'tokenrouter': { label: 'TokenRouter', kind: 'openai-compatible', baseURL: 'https://api.tokenrouter.com/v1' },
  // Vercel AI Gateway: OpenAI-compatible chat + /models discovery over one key,
  // model ids are `creator/model` (slashes inside the id — splitNamespace
  // splits on the FIRST slash only, so `vercel/anthropic/claude-opus-5` routes
  // exactly like tokenrouter's slash-ids already do).
  'vercel-ai-gateway': { label: 'Vercel AI Gateway', kind: 'openai-compatible', baseURL: 'https://ai-gateway.vercel.sh/v1' },
  'ollama-cloud': { label: 'Ollama Cloud', kind: 'openai-compatible', baseURL: 'https://ollama.com/v1' },
  // Local Ollama speaks the OpenAI wire (and /v1/messages) natively — no
  // adapter. It ignores credentials, but ezllm stores a key per account.
  'ollama-local': {
    label: 'Ollama (local)', kind: 'openai-compatible', baseURL: 'http://localhost:11434/v1',
    keyHint: 'Any non-empty value (e.g. ollama) — local Ollama ignores the key',
  },
  'gemini': { label: 'Google AI Studio', kind: 'gemini-openai', baseURL: 'https://generativelanguage.googleapis.com/v1beta/openai' },
  'gemini-web': {
    label: 'Google Gemini (web cookies)', kind: 'gemini-web', baseURL: 'internal://geminiweb',
    multilineKey: true,
    keyHint: 'Paste the WHOLE cookie header (all cookies, semicolon-separated) — we keep __Secure-1PSID and __Secure-1PSIDTS. Two lines work too. Use a TEST Google account only.',
  },
  'openai': { label: 'OpenAI Platform', kind: 'openai-compatible', baseURL: 'https://api.openai.com/v1' },
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

// The preset an EXISTING account was created from: the one with the same
// endpoint, else the generic preset of its kind. Edit uses it to know whether
// the key is a one-line secret, a multi-line cookie jar, or a bridge link.
function presetForAccount(a: { kind: string; base_url: string }): (typeof PROVIDER_PRESETS)[string] | null {
  const url = a.base_url.replace(/\/+$/, '');
  const byUrl = url ? PRESET_IDS.find((id) => (PROVIDER_PRESETS[id].baseURL ?? '').replace(/\/+$/, '') === url) : undefined;
  if (byUrl) return PROVIDER_PRESETS[byUrl];
  const byKind = PRESET_IDS.find((id) => PROVIDER_PRESETS[id].kind === a.kind && !PROVIDER_PRESETS[id].baseURL && !PROVIDER_PRESETS[id].accountURL);
  return byKind ? PROVIDER_PRESETS[byKind] : null;
}

// ── optional session credentials ────────────────────────────────────────
// Some providers can also read a balance or usage page with a browser session.
// It is one optional secret, stored encrypted, that rides alongside the API
// key. The Add wizard and the Edit modal used to carry five hand-copied blocks
// each (with the help text only on Add); this is the single definition both
// read, so the guidance cannot drift between them.
type Extra = { label: string; placeholder: string; help: string; expiry?: boolean };
const EXTRAS = {
  tokenrouter: {
    label: 'Management key', placeholder: 'Paste the TokenRouter management key',
    help: 'Used only to read the wallet balance — stored encrypted, never sent upstream.',
  },
  console: {
    label: 'Console session cookie', placeholder: 'Paste the Cookie header from a platform.claude.com request',
    help: 'Used only to read your prepaid credit balance — stored encrypted, sent only to platform.claude.com. Paste the whole Cookie header (DevTools → Network → request headers): we need its sessionKey and lastActiveOrg. It expires with your web session; paste a fresh one when the quota shows an error.',
  },
  openai: {
    label: 'Platform session token', placeholder: 'Paste the sess-… bearer token from platform.openai.com',
    help: 'Used only to read your credit balance — stored encrypted, sent only to api.openai.com. Copy it from the Authorization header of a platform.openai.com request (DevTools → Network). It expires with your web session; paste a fresh one when the quota shows an error.',
  },
  commandcode: {
    label: 'Session cookie', placeholder: 'Paste the __Secure-commandcode_prod_.session_token value',
    help: 'Used only to read your usage page’s limits — stored encrypted, sent only to api.commandcode.ai. It expires with your web session; paste a fresh one when the quota shows an error. Paste the whole Cookie header (DevTools → Network → request headers) to also see how many days are left.',
    expiry: true,
  },
  claudeSub: {
    label: 'Claude.ai session cookie', placeholder: 'Paste the Cookie header from a claude.ai request',
    help: 'Used only to read your plan’s 5-hour and weekly windows live — stored encrypted, sent only to claude.ai. Paste the whole Cookie header (DevTools → Network → request headers): we need its sessionKey and lastActiveOrg, and the cf_clearance/__cf_bm cookies get past Cloudflare. Without it the quota panel shows the bridge’s last-observed reading instead of a live one.',
  },
} satisfies Record<string, Extra>;

function extraForPreset(presetID: string, baseURL: string): Extra | null {
  if (presetID === 'tokenrouter') return EXTRAS.tokenrouter;
  if (presetID === 'anthropic-compatible' && baseURL.includes('api.anthropic.com')) return EXTRAS.console;
  if (presetID === 'openai') return EXTRAS.openai;
  if (presetID === 'commandcode') return EXTRAS.commandcode;
  if (presetID === 'claude-sub') return EXTRAS.claudeSub;
  return null;
}

function extraForAccount(a: Account): Extra | null {
  const url = a.base_url ?? '';
  if (a.kind === 'openai-compatible' && a.namespace === 'tokenrouter') return EXTRAS.tokenrouter;
  if (a.kind === 'anthropic-compatible' && url.includes('api.anthropic.com')) return EXTRAS.console;
  if (a.namespace === 'claude-sub') return EXTRAS.claudeSub;
  if (a.kind === 'openai-compatible' && a.namespace === 'openai') return EXTRAS.openai;
  if (a.kind === 'openai-compatible' && url.includes('api.commandcode.ai')) return EXTRAS.commandcode;
  return null;
}

function ExtraCredentialField({ extra, id, value, onChange, edit, isSet, expiresAt }: {
  extra: Extra; id: string; value: string; onChange: (v: string) => void;
  /** Edit mode: the field rotates a stored value rather than setting a first one. */
  edit?: boolean; isSet?: boolean; expiresAt?: string;
}) {
  return (
    <div>
      <label className={labelCls} htmlFor={id}>
        {extra.label} <span className="text-mute">{edit ? '(optional, rotates the stored value)' : '(optional)'}</span>
      </label>
      <SecretInput id={id} value={value} onChange={onChange}
        placeholder={edit && isSet ? '•••• (set — paste to replace)' : extra.placeholder} />
      <p className="mt-1 text-[12px] text-mute">{extra.help}</p>
      {edit && extra.expiry && <CookieExpiry at={expiresAt} />}
    </div>
  );
}

// ── key manager (Edit modal) ────────────────────────────────────────────
// Keys used to be enterable exactly once, in the Add wizard. This is the way
// back in: add another key, or replace one. Both go through the same
// validate-then-store endpoint as the wizard (POST /admin/accounts/{id}/keys),
// which only persists a key whose live test PASSED — so a bad paste changes
// nothing. Replace is add-then-remove: the new key is proven first, and only
// then is the old one deleted, so a typo can never leave the provider with no
// working key.
type KeyForm = { replace: KeyRow | null };

function KeyManager({ account, onChanged, onBusy, onDirty }: {
  account: Account; onChanged: () => void; onBusy: (b: boolean) => void; onDirty: (d: boolean) => void;
}) {
  const preset = presetForAccount(account);
  const bridgeID = preset?.bridge;
  const multiline = !!preset?.multilineKey;
  const [form, setForm] = useState<KeyForm | null>(null);
  const [label, setLabel] = useState('');
  const [secret, setSecret] = useState('');
  const [skip, setSkip] = useState(false);
  const [busy, setBusy] = useState(false);
  const [steps, setSteps] = useState<TestStep[] | null>(null);
  const [err, setErr] = useState<ApiErrorDetail | null>(null);
  const [note, setNote] = useState<{ tone: 'ok' | 'warn'; text: string } | null>(null);
  const formRef = useRef<HTMLDivElement>(null);
  useEffect(() => { if (form) formRef.current?.scrollIntoView({ block: 'nearest', behavior: 'smooth' }); }, [form]);

  useEffect(() => { onDirty(form != null && secret.trim() !== ''); }, [form, secret, onDirty]);

  const begin = (replace: KeyRow | null) => {
    setForm({ replace }); setLabel(replace?.label ?? ''); setSecret(''); setSkip(false);
    setSteps(null); setErr(null); setNote(null);
  };
  const cancel = () => { setForm(null); setSecret(''); setSteps(null); setErr(null); };

  const submit = async () => {
    if (!form || busy || (!bridgeID && !secret.trim())) return;
    // Reported to the modal in the same batch as our own state, so its guard
    // is up before the next keystroke can arrive.
    setBusy(true); onBusy(true); setErr(null); setNote(null); setSteps([]);
    try {
      await postKeyTestStream<{ key: KeyRow; test: TestResult }>(
        `/admin/accounts/${account.id}/keys`,
        { label: label.trim() || form.replace?.label || 'primary', ...(bridgeID ? { bridge: bridgeID } : { api_key: secret.trim() }), skip_inference: skip },
        (step) => setSteps((prev) => [...(prev ?? []), step]),
      );
      if (form.replace) {
        try {
          await del(`/admin/accounts/${account.id}/keys/${form.replace.id}`);
          setNote({ tone: 'ok', text: `Key replaced — the new key passed its test and “${form.replace.label}” was removed.` });
        } catch (e) {
          setNote({ tone: 'warn', text: `The new key passed its test and is saved, but removing the old key “${form.replace.label}” failed (${e instanceof Error ? e.message : String(e)}). Delete it from the provider card.` });
        }
      } else {
        setNote({ tone: 'ok', text: 'Key added — it passed its test.' });
      }
      setForm(null); setSecret(''); setSteps(null);
      onChanged();
    } catch (e) {
      if (e instanceof ApiError) setErr(e.detail);
      else setErr({ status: 0, type: 'error', message: e instanceof Error ? e.message : String(e) });
    } finally { setBusy(false); onBusy(false); }
  };

  const verb = form?.replace ? 'Replace' : 'Add';
  return (
    <div className="rounded-lg border border-line bg-raised/60 p-4">
      <div className="flex items-center justify-between gap-3">
        <SectionLabel>API keys</SectionLabel>
        {!form && <button type="button" className={cx(btn.base, btn.ghost, '!h-7 !px-3 !text-[12px]')} onClick={() => begin(null)}>Add key</button>}
      </div>
      {account.keys.length === 0 ? (
        <p className="mt-2 text-[13px] text-mute">This provider has no key, so it cannot serve. Add one below.</p>
      ) : (
        <ul className="mt-2 divide-y divide-line/60">
          {account.keys.map((k) => (
            <li key={k.id} className="flex items-center gap-3 py-2 text-[13px]">
              <span className={cx('h-1.5 w-1.5 flex-none rounded-full', !k.enabled ? 'bg-line-strong' : k.last_test_ok === true ? 'bg-ok' : k.last_test_ok === false ? 'bg-bad' : 'bg-line-strong')} />
              <span className="min-w-0 flex-1 truncate">
                <span className="text-ink">{k.label}</span>
                <span className="font-code ml-2 text-[12px] text-mute">{k.hint}</span>
                {!k.enabled && <span className="ml-2 text-[12px] text-warn">disabled</span>}
                {k.enabled && k.last_test_ok === false && <span className="ml-2 text-[12px] text-bad">last test failed</span>}
              </span>
              <button type="button" disabled={busy || form != null} className={cx(btn.base, btn.ghost, '!h-7 !px-3 !text-[12px]')}
                onClick={() => begin(k)} aria-label={`Replace key ${k.label}`}>Replace</button>
            </li>
          ))}
        </ul>
      )}
      <div aria-live="polite">
        {note && <p className={cx('mt-2 text-[12px]', note.tone === 'ok' ? 'text-ok' : 'text-warn')}>{note.text}</p>}
      </div>
      {form && (
        <div ref={formRef} className="mt-3 space-y-3 border-t border-line pt-3">
          <p className="text-[13px] font-medium text-ink">
            {form.replace ? `Replace “${form.replace.label}”` : 'Add a key'}
          </p>
          <div>
            <label className={labelCls} htmlFor="km-label">Label</label>
            <input id="km-label" className={inputCls} value={label} placeholder={form.replace?.label ?? 'primary'}
              onChange={(e) => setLabel(e.target.value)} disabled={busy}
              onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); void submit(); } }} />
          </div>
          {bridgeID ? (
            <p className="text-[13px] text-dim">
              This provider rides a subscription bridge, which supplies its own key. {verb === 'Replace' ? 'Replacing' : 'Adding'} re-links the bridge&rsquo;s current key — there is nothing to paste.
            </p>
          ) : (
            <div>
              <label className={labelCls} htmlFor="km-secret">{multiline ? 'Session cookies' : 'API key'}</label>
              <SecretInput id="km-secret" value={secret} onChange={setSecret} multiline={multiline} disabled={busy}
                placeholder={preset?.keyHint ?? 'Paste the new provider API key'}
                onEnter={() => void submit()} />
              {preset?.keyHint && <p className="mt-1 text-[12px] text-mute">{preset.keyHint}</p>}
            </div>
          )}
          <div className="flex items-center gap-3">
            <Toggle checked={skip} onChange={setSkip} label="skip inference probe" disabled={busy} />
            <span className="text-[13px] text-dim">Skip inference probe <span className="text-mute">(saves a token spend)</span></span>
          </div>
          {(steps != null) && (
            <div className="rounded-lg border border-line bg-surface px-4 py-3">
              <SectionLabel className="mb-2">Validation</SectionLabel>
              <StepList steps={steps} running={busy} />
            </div>
          )}
          <ErrBox e={err} onDismiss={() => setErr(null)} />
          <div className="flex items-center justify-end gap-2">
            <button type="button" className={cx(btn.base, btn.ghost)} onClick={cancel} disabled={busy}>Cancel</button>
            <button type="button" className={cx(btn.base, btn.primary)} disabled={busy || (!bridgeID && !secret.trim())} onClick={() => void submit()}>
              {busy ? <Spinner /> : null} {busy ? 'Validating key…' : form.replace ? 'Validate, save & remove old key' : 'Validate & save key'}
            </button>
          </div>
        </div>
      )}
    </div>
  );
}

// ── quota presentation (moved from the dashboard's per-account quota panel:
//    quota data belongs under the provider it describes) ──────────────────────

function stale(quota: { read_at?: string | null } | null | undefined): boolean {
  if (!quota?.read_at) return false;
  const t = Date.parse(quota.read_at);
  return !Number.isNaN(t) && Date.now() - t > 5 * 60 * 1000;
}

function CookieExpiry({ at }: { at?: string }) {
  if (!at) return null;
  const days = Math.ceil((new Date(at).getTime() - Date.now()) / 86_400_000);
  if (Number.isNaN(days)) return null;
  const text = days < 0 ? 'Cookie expired — paste a fresh one' : days === 0 ? 'Cookie expires today' : `Cookie expires in ${days} ${days === 1 ? 'day' : 'days'}`;
  return <p className={cx('mt-1 text-[12px]', days <= 1 ? 'text-warn' : 'text-mute')}>{text} (as of when it was pasted).</p>;
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
            <span className="w-14 text-[11px] text-mute">{k}</span>
            <div className="h-1.5 min-w-16 flex-1 overflow-hidden rounded-full bg-shade">
              {/* The bar DRAWS IN from zero: a quota reading is a live probe
                  of the provider, and a bar that springs to its width says
                  "this was just measured" in a way a static bar does not.
                  The width is capped at 100 so a provider reporting 140%
                  still shows a full bar instead of overflowing its track. */}
              <motion.div
                className={cx('h-full rounded-full', v >= 90 ? 'bg-bad' : v >= 70 ? 'bg-warn' : 'bg-ok')}
                initial={{ width: 0 }}
                animate={{ width: `${Math.min(100, Math.max(0, v))}%` }}
                transition={{ duration: 0.5, ease: [0.22, 1, 0.36, 1] }}
              />
            </div>
            <span className="tnum w-10 text-right font-mono text-[12px] text-dim">{Math.round(v)}%</span>
          </div>
        ))}
      </div>
    );
  }
  if (quota.kind === 'money' && quota.balance != null) {
    return (
      <div className="font-mono text-[13px]">
        <span className="text-dim">{fmtMoney(Number(quota.balance), '')}</span>
        {quota.unit ? <span className="ml-1 text-mute">{quota.unit}</span> : null}
        {quota.plan ? <div className="mt-0.5 text-[11px] text-mute">{quota.plan}</div> : null}
      </div>
    );
  }
  // Count-shaped usage (Ollama class): what the provider reported, no ceiling.
  // Deliberately NO bar — a bar needs a denominator the provider never stated.
  if (quota.tokens_today != null || quota.requests_today != null) {
    const lines: Array<[string, number]> = [];
    if (quota.requests_today != null) lines.push(['requests·24h', quota.requests_today]);
    if (quota.tokens_today != null) lines.push(['tokens', quota.tokens_today]);
    return (
      <div className="space-y-0.5 font-mono text-[13px]">
        {lines.map(([k, v]) => (
          <div key={k} className="flex items-baseline gap-2">
            <span className="text-dim">{v.toLocaleString()}</span>
            <span className="text-[11px] text-mute">{k}</span>
          </div>
        ))}
      </div>
    );
  }
  return <span className="text-mute">–</span>;
}

// The roster's quota column: one compact line per reading, sized to a fixed
// 216px column. The full bars live behind the Quota button.
function QuotaSummary({ quota }: { quota: Account['quota'] }) {
  if (!quota || quota.kind === 'none' || quota.kind === 'unknown') {
    return <span className="text-[13px] text-mute" title="no quota read — this provider has not reported usage">–</span>;
  }
  const pcts: Array<[string, number]> = [];
  if (quota.percent_rolling != null) pcts.push(['Rolling', quota.percent_rolling]);
  if (quota.percent_weekly != null) pcts.push(['Weekly', quota.percent_weekly]);
  if (quota.percent_monthly != null) pcts.push(['Monthly', quota.percent_monthly]);
  if (pcts.length > 0) {
    // The tightest window speaks for the provider: it is the one that refuses first.
    const [k, v] = pcts.reduce((m, r) => (r[1] > m[1] ? r : m));
    return (
      <div className="flex items-center gap-2" title={pcts.map(([n, x]) => `${n} ${Math.round(x)}%`).join(' · ')}>
        <span className="w-14 flex-none text-[12px] text-mute">{k}</span>
        <div className="h-1 min-w-10 flex-1 overflow-hidden rounded-full bg-shade">
          <motion.div
            className={cx('h-full rounded-full', v >= 90 ? 'bg-bad' : v >= 70 ? 'bg-warn' : 'bg-ok')}
            initial={{ width: 0 }}
            animate={{ width: `${Math.min(100, Math.max(0, v))}%` }}
            transition={{ duration: 0.5, ease: [0.22, 1, 0.36, 1] }} />
        </div>
        <span className="tnum w-9 flex-none text-right text-[12px] text-ink">{Math.round(v)}%</span>
      </div>
    );
  }
  if (quota.kind === 'money' && quota.balance != null) {
    return (
      <span className="text-[13px]" title={quota.plan ?? undefined}>
        <span className="tnum text-ink">{fmtMoney(Number(quota.balance), '')}</span>
        {quota.unit ? <span className="ml-1 text-mute">{quota.unit}</span> : null}
        <span className="ml-1.5 text-[12px] text-mute">balance</span>
      </span>
    );
  }
  if (quota.requests_today != null || quota.tokens_today != null) {
    return (
      <span className="text-[13px]">
        {quota.requests_today != null
          ? <><span className="tnum text-ink">{nfmt(quota.requests_today)}</span><span className="ml-1 text-[12px] text-mute">requests, 24h</span></>
          : <><span className="tnum text-ink">{nfmt(quota.tokens_today ?? 0)}</span><span className="ml-1 text-[12px] text-mute">tokens</span></>}
      </span>
    );
  }
  return <span className="text-[12px] text-mute">–</span>;
}

const hostOf = (url: string) => {
  try { return new URL(url).host || url; } catch { return url; }
};

// A button whose width never changes while it works: the label stays in the
// flow (invisible while busy) and the spinner sits over it. Inserting a
// spinner beside the label widened the button and shoved its neighbours.
function BusyButton({ busy, disabled, onClick, children, className }: {
  busy?: boolean; disabled?: boolean; onClick: () => void; children: ReactNode; className?: string;
}) {
  return (
    <button type="button" disabled={disabled || busy} aria-busy={busy || undefined}
      className={cx(btn.base, btn.ghost, 'relative h-[30px] px-3 text-[13px]', className)}
      onClick={(e) => { e.stopPropagation(); onClick(); }}>
      <span className={cx(busy && 'invisible')}>{children}</span>
      {busy && <span className="absolute inset-0 grid place-items-center"><Spinner className="h-3.5 w-3.5" /></span>}
    </button>
  );
}

function IconButton({ label, tone, busy, disabled, onClick, children }: {
  label: string; tone?: 'bad'; busy?: boolean; disabled?: boolean; onClick: () => void; children: ReactNode;
}) {
  return (
    <button type="button" title={label} aria-label={label} disabled={disabled || busy}
      className={cx('grid h-7 w-7 place-items-center rounded-md text-[13px] text-mute transition-colors disabled:opacity-40',
        tone === 'bad' ? 'hover:bg-bad-dim hover:text-bad' : 'hover:bg-hover hover:text-ink')}
      onClick={(e) => { e.stopPropagation(); onClick(); }}>
      {busy ? <Spinner className="h-3 w-3" /> : children}
    </button>
  );
}

const REPO_DOCS = 'https://github.com/c4lyp5o/ezllm/blob/master/';
const statusText = (v: unknown) => (typeof v === 'string' && v.trim() ? v : null);

// BridgeUpdateNote is the host updater's latest report on this bridge's CLI:
// what it did, its reason when it held a release back, and when it checked.
function BridgeUpdateNote({ update }: { update: NonNullable<Bridge['update']> }) {
  const { text, tone } = bridgeUpdateText(update);
  return (
    <div className="text-[12px]">
      <p className={UPDATE_TONE[tone]}>CLI updates: {text}</p>
      <p className="text-mute" title={update.checked_at}>
        {update.detail ? <>{update.detail} · </> : null}checked {relTime(update.checked_at)}
      </p>
    </div>
  );
}

// BridgePanel stands in for the key field on a subscription preset: running →
// what the bridge reported at boot and "no key needed"; off → the exact steps
// that turn it on, with the runbook one click away.
function BridgePanel({ bridge, loading }: { bridge: Bridge | null; loading: boolean }) {
  if (loading) return <div className="flex items-center gap-2 text-[13px] text-mute"><Spinner /> Checking the bridge…</div>;
  if (!bridge) return null;
  const runbook = (
    <a className="text-ink underline underline-offset-2" href={REPO_DOCS + bridge.runbook} target="_blank" rel="noreferrer">
      {bridge.runbook}
    </a>
  );
  if (bridge.enabled) {
    const s = bridge.status ?? {};
    const facts = ([
      ['CLI', statusText(s.cli_version)],
      ['Login', statusText(s.login)],
      ['Plan', statusText(s.subscription)],
      ['Models', statusText(s.models)],
      ['Client tools', statusText(s.client_tools)],
      ['Channel', statusText(s.channel)],
      ['Landlock', statusText(s.landlock)],
    ] as Array<[string, string | null]>).filter((f): f is [string, string] => f[1] !== null);
    const login = statusText(s.login);
    const loggedOut = login !== null && !/^logged[ -]in\b/i.test(login);
    const tools = statusText(s.client_tools);
    const toolsDisabled = tools !== null && /^disabled/i.test(tools);
    return (
      <div className="space-y-2 rounded-lg border border-ok/40 bg-ok-dim/40 px-4 py-3">
        <p className="flex flex-wrap items-center gap-2 text-[13px] text-ok">
          <span className="size-1.5 rounded-full bg-ok" /> Bridge running at <Mono>{bridge.base_url}</Mono> — no key needed
        </p>
        {facts.length > 0 && (
          <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-0.5 text-[12px]">
            {facts.map(([k, v]) => (
              <div key={k} className="contents"><dt className="text-mute">{k}</dt><dd className="font-mono text-dim">{v}</dd></div>
            ))}
          </dl>
        )}
        {bridge.update && <BridgeUpdateNote update={bridge.update} />}
        {toolsDisabled && <p className="text-[12px] text-warn">Client tool calls are unavailable on this bridge. See {runbook} for the reason and options.</p>}
        {loggedOut && <p className="text-[12px] text-warn">The CLI does not report a login — the key test will fail until you log in again ({runbook}).</p>}
        <p className="text-[12px] text-mute">Status was recorded at startup. Setup and troubleshooting: {runbook}</p>
        <p className="text-[12px] text-mute">Save &amp; validate uses the bridge&apos;s own key. Cap the account under Rules before agents use it.</p>
      </div>
    );
  }
  return (
    <div className="space-y-2 rounded-lg border border-warn/40 bg-warn-dim/40 px-4 py-3 text-[13px]">
      <p className="flex items-center gap-2 text-warn"><span className="size-1.5 rounded-full bg-warn" /> This bridge is not running on this server</p>
      <ol className="list-decimal space-y-1 pl-5 text-dim">
        <li>On the host, install the official CLI and log in to a separate folder (the runbook has the exact commands).</li>
        <li>In <Mono>compose.yml</Mono>, uncomment the bridge&apos;s two mounts and set <Mono>{bridge.env}: &quot;on&quot;</Mono>.</li>
        <li>Run <Mono>docker compose up -d</Mono>, then reopen this dialog. If ezllm does not start, check <Mono>docker compose logs ezllm</Mono>.</li>
      </ol>
      <p className="text-[12px] text-mute">Step-by-step: {runbook}</p>
    </div>
  );
}

function AddProviderModal({ open, onClose, onDone }: { open: boolean; onClose: () => void; onDone: () => void }) {
  const [d, setD] = useState<Draft>(emptyDraft);
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<TestResult | null>(null);
  // Live steps streamed by the server as the test walks (docx item 8): the
  // chain colors green/red per step instead of sitting dead until the end.
  // On completion result.steps supersedes them — same data, one append path.
  const [liveSteps, setLiveSteps] = useState<TestStep[] | null>(null);
  const [err, setErr] = useState<ApiErrorDetail | null>(null);
  // GitHub device flow (the phone path): start → show the code → poll →
  // the key add names the handle. The OAuth session never leaves the server,
  // so this component only ever holds the handle, the user code and status.
  const [ghStart, setGhStart] = useState<GhDeviceStart | null>(null);
  const [ghStatus, setGhStatus] = useState<GhDeviceStatus | null>(null);
  const [ghBusy, setGhBusy] = useState(false);
  const [ghErr, setGhErr] = useState<ApiErrorDetail | null>(null);
  // Extra seconds GitHub asked for via slow_down (RFC 8628 §3.5). Once GitHub
  // says slow_down it will NOT hand over the token until our interval grows, so
  // this is what stops "Waiting for authorization…" from being permanent while
  // github.com/login/device already says success.
  const [ghBackoff, setGhBackoff] = useState(0);
  // Subscription bridges: fetched on open so the preset shows live state.
  const [bridges, setBridges] = useState<Bridge[] | null>(null);

  useEffect(() => {
    if (!open) return;
    listBridges().then(setBridges).catch(() => setBridges([]));
  }, [open]);

  useEffect(() => {
    if (open) {
      setD(emptyDraft); setBusy(false); setResult(null); setLiveSteps(null); setErr(null);
      setGhStart(null); setGhStatus(null); setGhBusy(false); setGhErr(null); setGhBackoff(0);
    }
  }, [open]);

  // Closing without a save abandons the flow: drop the handle so its
  // session evaporates now instead of sitting in server memory until the
  // TTL. A consumed handle (saved) is already gone — the DELETE is a no-op.
  useEffect(() => {
    if (open) return;
    const handle = ghStart?.handle;
    setGhStart(null);
    setGhStatus(null);
    if (handle) void ghDeviceCancel(handle).catch(() => undefined);
  }, [open, ghStart]);

  // One status tick per interval GitHub asked for, plus any slow_down backoff.
  // GitHub sends slow_down when polls arrive too fast and then WITHHOLDS the
  // token until the interval grows, so the backoff is applied on receipt and
  // the effect re-arms at the new cadence. Stops at a terminal state.
  useEffect(() => {
    const phase = ghStatus?.status;
    if (!ghStart || !open || (phase != null && phase !== 'pending')) return;
    const secs = Math.max(3, ghStart.interval || 5) + ghBackoff;
    const iv = setInterval(() => {
      ghDeviceStatus(ghStart.handle)
        .then((st) => {
          setGhStatus(st);
          // RFC 8628 §3.5: raise the interval by 5s, capped so a hostile or
          // broken upstream cannot push us past the code's own lifetime.
          if (st.slow_down) setGhBackoff((b) => (b >= 30 ? b : b + 5));
        })
        .catch((e) => {
          // EVERY failure must be visible: swallowing a non-ApiError left the
          // panel saying "Waiting for authorization…" forever with no clue.
          if (e instanceof ApiError) setGhErr(e.detail);
          else setGhErr({ status: 0, type: 'error', message: e instanceof Error ? e.message : String(e) });
        });
    }, secs * 1000);
    return () => clearInterval(iv);
  }, [ghStart, ghStatus?.status, open, ghBackoff]);

  const isGitHub = PROVIDER_PRESETS[d.preset]?.kind === 'github-copilot';
  const bridgeID = PROVIDER_PRESETS[d.preset]?.bridge;
  const bridge = bridgeID ? bridges?.find((b) => b.id === bridgeID) ?? null : null;

  const startGitHub = async () => {
    setGhBusy(true); setGhErr(null); setGhStatus(null);
    try {
      setGhStart(await ghDeviceStart());
    } catch (e) {
      if (e instanceof ApiError) setGhErr(e.detail);
      else setGhErr({ status: 0, type: 'error', message: e instanceof Error ? e.message : String(e) });
    } finally { setGhBusy(false); }
  };

  const set = <K extends keyof Draft>(k: K, v: Draft[K]) => setD((p) => ({ ...p, [k]: v }));

  const saveAndValidate = async () => {
    setBusy(true); setErr(null); setResult(null);
    let createdAccountID: number | null = null;
    try {
      const customHeaders = d.customHeaders.trim() ? JSON.parse(d.customHeaders) as Record<string, unknown> : null;
      const preset = PROVIDER_PRESETS[d.preset];
      const payload = {
        name: d.name.trim(), namespace: d.namespace.trim(), kind: preset.kind,
        // Local Ollama is the one preset whose URL field stays editable (a remote
        // box) — send what the operator typed, not the preset's localhost default.
        base_url: d.preset === 'ollama-local' ? d.base_url.trim() : bridge?.base_url ?? preset.baseURL ?? d.base_url.trim(), probe_delay_ms: d.probe_delay_ms,
        ...(preset.usageConvention ? { usage_convention: preset.usageConvention } : {}),
        requires_session_header: preset.sessionHeader ?? d.requiresSessionHeader,
        custom_headers: customHeaders, notes: d.notes.trim(),
        management_key: d.managementKey.trim(),
        // No quota_mode / caps here on purpose: quota is always probed, and
        // cap_window + cap_tokens live on Rules (per model, enforced in-flight).
      };
      const account = await post<Account>('/admin/accounts', payload);
      createdAccountID = account.id;
      // One credential source: a pasted session JSON wins, otherwise the
      // device handle the GitHub flow produced — the server resolves THAT
      // into the session itself, so no credential rides in this body.
      const credential = d.apiKey.trim()
        ? { api_key: d.apiKey.trim() }
        : bridgeID ? { bridge: bridgeID }
        : { device_handle: ghStart?.handle ?? '' };
      // Streamed key test: every step lands in liveSteps as the server
      // records it (NDJSON), so the chain moves. Falls back to the one-shot
      // response shape when the server does not stream.
      const response = await postKeyTestStream<{ key: KeyRow; test: TestResult }>(
        `/admin/accounts/${account.id}/keys`,
        { label: d.keyLabel.trim() || 'primary', ...credential, skip_inference: d.skipInference },
        (step) => setLiveSteps((prev) => [...(prev ?? []), step]),
      );
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

  // Anything typed makes closing ask first; a half-finished GitHub login counts too.
  const dirty = !!(d.name.trim() || d.apiKey || d.managementKey || d.notes.trim() || d.customHeaders.trim() || ghStart);
  // The validation steps and any failure sit together; scroll them into view
  // when a save starts or fails, so a red step is never off-screen below the fold.
  const validationRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (busy || err) validationRef.current?.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
  }, [busy, err]);
  const extra = extraForPreset(d.preset, d.base_url);
  const isCompat = ['openai-compatible', 'anthropic-compatible', 'gemini-openai'].includes(PROVIDER_PRESETS[d.preset]?.kind ?? '');
  const isAnthropicKind = PROVIDER_PRESETS[d.preset]?.kind === 'anthropic-compatible';
  const presetAccountID = PROVIDER_PRESETS[d.preset]?.accountURL;
  const valid = d.name.trim().length > 0
    && /^[a-z0-9][a-z0-9._-]{0,62}$/.test(d.namespace.trim())
    && (!isCompat || /^https?:\/\//i.test(d.base_url.trim()))
    // GitHub has no pasteable key: an authorized device flow substitutes for
    // the API key. Anything else still needs its credential in hand.
    && (d.apiKey.trim().length > 0 || (isGitHub && ghStatus?.status === 'authorized') || !!bridge?.enabled);

  return (
    <Modal open={open} onClose={onClose} wide title="Add provider"
      subtitle="Add the provider and validate its API key before the key is saved."
      busy={busy} dirty={dirty} onSubmit={() => { if (valid) void saveAndValidate(); }}
      footer={(close) => (
        <>
          <button type="button" className={cx(btn.base, btn.ghost)} onClick={close} disabled={busy}>Cancel</button>
          <button type="submit" disabled={busy || !valid} className={cx(btn.base, btn.primary)}>
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
            <p className="mt-1 text-[12px] text-mute">lowercase slug used in model ids</p>
          </div>
        </div>
        <div className="grid gap-4 sm:grid-cols-2">
          <div>
            <label className={labelCls} htmlFor="ap-kind">Provider type</label>
            <select id="ap-kind" className={inputCls} value={d.preset} onChange={(e) => {
              const presetID = e.target.value;
              const preset = PROVIDER_PRESETS[presetID];
              // A bridge preset names itself: one per server, so the
              // operator has nothing to invent.
              const named = preset.bridge && !d.name.trim();
              setD((p) => ({ ...p, preset: presetID, kind: preset.kind,
                ...(named ? { name: preset.label.replace(/ \(bridge\)$/, ''), namespace: presetID, nsTouched: false } : {}),
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
            <p className="mt-1 text-[12px] text-mute">
              The account ID is part of the endpoint. It composes
              <Mono> /accounts/&lt;account_id&gt;/ai/v1</Mono> — Workers AI serves
              chat, responses and anthropic messages under that prefix.
            </p>
            {d.accountId && (
              <div className="mt-2 rounded-lg border border-line bg-raised/60 px-3.5 py-2.5 text-[13px] text-dim">
                Endpoint preset: <Mono>{d.base_url}</Mono>
              </div>
            )}
          </div>
        ) : isCompat && (!isKnownEndpoint(d.base_url) || d.preset === 'ollama-local') ? (
          <div>
            <label className={labelCls} htmlFor="ap-url">Provider API base URL</label>
            <input id="ap-url" className={cx(inputCls, 'font-mono')} value={d.base_url}
              placeholder={isAnthropicKind ? 'https://api.anthropic.com' : d.preset === 'ollama-local' ? 'http://localhost:11434/v1' : 'https://api.provider.com/v1'} spellCheck={false}
              onChange={(e) => set('base_url', e.target.value.trim())} />
            <p className="mt-1 text-[12px] text-mute">{d.preset === 'ollama-local'
              ? 'Where Ollama listens — change the host for a remote machine. Keep the /v1 suffix.'
              : isAnthropicKind
              ? 'Required for compatibility providers. Enter the host only — /v1 paths are added automatically.'
              : 'Required for compatibility providers. Use the base URL ending at the API version, e.g. /v1.'}</p>
          </div>
        ) : isKnownEndpoint(d.base_url) ? null : (
          <div className="rounded-lg border border-line bg-raised/60 px-3.5 py-2.5 text-[13px] text-dim">
            Endpoint preset: <Mono>{d.base_url}</Mono>
          </div>
        )}
        {isGitHub && (
          <div className="space-y-3 rounded-lg border border-line bg-raised/60 px-4 py-3">
            <SectionLabel>GitHub login — device flow</SectionLabel>
            {!ghStart && (
              <div className="flex flex-wrap items-center gap-3">
                <p className="text-[13px] text-dim">
                  No key to paste. Start a login and tap <span className="text-ink">Authorize</span> on your
                  phone — the session stays on the server and never reaches this browser.
                </p>
                <button type="button" className={cx(btn.base, btn.ghost)} disabled={ghBusy}
                  onClick={() => void startGitHub()}>
                  {ghBusy ? <Spinner /> : null} Start GitHub login
                </button>
              </div>
            )}
            {ghStart && (ghStatus == null || ghStatus.status === 'pending') && (
              <div className="space-y-2">
                <ol className="space-y-1 text-[13px] text-dim">
                  <li>
                    1. Open{' '}
                    <a className="text-ink underline underline-offset-2" href={ghStart.verification_uri}
                      target="_blank" rel="noreferrer">{ghStart.verification_uri}</a>
                  </li>
                  <li>
                    2. Enter code{' '}
                    <Mono className="text-[15px] text-ink">{ghStart.user_code}</Mono>
                  </li>
                </ol>
                <div className="flex flex-wrap items-center gap-2 text-[13px] text-mute">
                  <Spinner /> Waiting for authorization…
                  <span className="text-mute/70">
                    code expires in {Math.max(1, Math.round(ghStart.expires_in / 60))} min
                    {ghBackoff > 0 ? ` · polling every ${Math.max(3, ghStart.interval || 5) + ghBackoff}s` : ''}
                  </span>
                </div>
                {ghStatus?.detail && (
                  <p className="text-[13px] text-warn">{ghStatus.detail}</p>
                )}
                <button type="button" className={cx(btn.base, btn.ghost)}
                  onClick={() => {
                    const handle = ghStart.handle;
                    setGhStart(null); setGhStatus(null);
                    void ghDeviceCancel(handle).catch(() => undefined);
                  }}>
                  Cancel login
                </button>
              </div>
            )}
            {ghStatus?.status === 'authorized' && (
              <p className="font-mono text-[13px] text-ok">
                ✓ @{ghStatus.github_login} authorized — Save &amp; validate to store the key
              </p>
            )}
            {(ghStatus?.status === 'expired' || ghStatus?.status === 'error') && (
              <div className="space-y-2">
                <p className="text-[13px] text-bad">
                  {ghStatus.status === 'expired'
                    ? 'The code expired before you authorized it.'
                    : ghStatus.detail || 'GitHub refused the authorization.'}
                </p>
                <button type="button" className={cx(btn.base, btn.ghost)} disabled={ghBusy}
                  onClick={() => void startGitHub()}>
                  {ghBusy ? <Spinner /> : null} Start again
                </button>
              </div>
            )}
            <ErrBox e={ghErr} onDismiss={() => setGhErr(null)} />
          </div>
        )}
        {bridgeID && <BridgePanel bridge={bridge} loading={bridges === null} />}
        <div className={cx('grid gap-4 sm:grid-cols-2', bridgeID && 'hidden')}>
          <div className="sm:col-span-1">
            <label className={labelCls} htmlFor="ap-key">API key</label>
            <SecretInput id="ap-key" value={d.apiKey} onChange={(v) => set('apiKey', v)}
              multiline={!!PROVIDER_PRESETS[d.preset]?.multilineKey}
              placeholder={PROVIDER_PRESETS[d.preset]?.keyHint ?? 'Paste provider API key'} />
            {PROVIDER_PRESETS[d.preset]?.keyHint && (
              <p className="mt-1 text-[12px] text-mute">{PROVIDER_PRESETS[d.preset].keyHint}</p>
            )}
          </div>
          <div>
            <label className={labelCls} htmlFor="ap-kl">Key label</label>
            <input id="ap-kl" className={inputCls} value={d.keyLabel} onChange={(e) => set('keyLabel', e.target.value)} placeholder="primary" />
          </div>
        </div>
        {extra && <ExtraCredentialField extra={extra} id="ap-extra" value={d.managementKey} onChange={(v) => set('managementKey', v)} />}
        <div>
          <label className={labelCls} htmlFor="ap-notes">Notes</label>
          <input id="ap-notes" className={inputCls} value={d.notes} onChange={(e) => set('notes', e.target.value)} placeholder="optional" />
        </div>
        <div className="flex items-center gap-3">
          <Toggle checked={d.skipInference} onChange={(v) => set('skipInference', v)} label="skip inference probe" />
          <span className="text-[13px] text-dim">Skip inference probe <span className="text-mute">(saves a token spend)</span></span>
        </div>
        <div ref={validationRef} className="space-y-3">
          <div className="rounded-lg border border-line bg-raised/60 px-4 py-3">
            <SectionLabel className="mb-2">Validation</SectionLabel>
            <StepList steps={result?.steps ?? liveSteps} running={busy} />
            {result?.ok && <p className="mt-2 font-mono text-[12px] text-ok">✓ Key validated and stored.</p>}
          </div>
          <ErrBox e={err} onDismiss={() => setErr(null)} />
        </div>
        <details className="rounded-lg border border-line bg-raised/40 px-4 py-3">
          <summary className="cursor-pointer text-[13px] font-medium text-dim">Advanced settings</summary>
          <div className="mt-4 space-y-4">
            <div className="flex items-center gap-3">
              <Toggle checked={d.requiresSessionHeader} onChange={(v) => set('requiresSessionHeader', v)} label="requires_session_header" />
              <span className="text-[13px] text-dim">requires session header upstream</span>
            </div>
            <div>
              <label className={labelCls} htmlFor="ap-cH">Custom headers (JSON)</label>
              <textarea id="ap-cH" rows={3} spellCheck={false} className={cx(inputCls, 'font-mono text-[13px]')}
                placeholder={'{"x-team":"ops"}'} value={d.customHeaders} onChange={(e) => set('customHeaders', e.target.value)} />
            </div>
          </div>
        </details>
      </div>
    </Modal>
  );
}

// ── edit modal ──────────────────────────────────────────────────────────

function EditAccountModal({ account, initialTab = 'settings', onClose, onSaved }: {
  account: Account; initialTab?: 'settings' | 'keys'; onClose: () => void; onSaved: () => void;
}) {
  const [tab, setTab] = useState<'settings' | 'keys'>(initialTab);
  // Mounted fresh per edit (the card renders it only while editing), so the
  // fields start from the account once. A key change reloads the account
  // underneath this modal and must NOT reset what the operator has typed.
  const [name, setName] = useState(account.name);
  const [kind, setKind] = useState(account.kind);
  const [baseUrl, setBaseUrl] = useState(account.base_url);
  // Numbers are held as text so the field can be cleared while typing; they
  // are parsed (and validated) only when they matter.
  const [probeDelay, setProbeDelay] = useState(String(account.probe_delay_ms ?? 400));
  const [notes, setNotes] = useState(account.notes ?? '');
  const [managementKey, setManagementKey] = useState('');
  const [rateLimit, setRateLimit] = useState(String(account.rate_limit ?? 0));
  const [rateWindow, setRateWindow] = useState(account.rate_window || '60s');
  const [saving, setSaving] = useState(false);
  const [keyBusy, setKeyBusy] = useState(false);
  const [keyDirty, setKeyDirty] = useState(false);
  const [err, setErr] = useState<ApiErrorDetail | null>(null);

  const extra = extraForAccount(account);
  // Changing the kind of a provider that already has keys or models re-points
  // every request it serves at a different adapter. Fixed once it has either.
  const kindLocked = account.keys.length > 0 || (account.models_count ?? 0) > 0;

  const delayN = probeDelay.trim() === '' ? 0 : Number(probeDelay);
  const rateN = rateLimit.trim() === '' ? 0 : Number(rateLimit);
  const delayOk = Number.isInteger(delayN) && delayN >= 0;
  const rateOk = Number.isInteger(rateN) && rateN >= 0;

  const fieldsDirty = name.trim() !== account.name || kind !== account.kind || baseUrl.trim() !== account.base_url
    || (delayOk && delayN !== (account.probe_delay_ms ?? 400)) || notes.trim() !== (account.notes ?? '')
    || (rateOk && rateN !== (account.rate_limit ?? 0))
    || (rateN > 0 && rateWindow !== (account.rate_window || '60s'))
    || managementKey.trim() !== '';
  const busy = saving || keyBusy;
  const valid = name.trim() !== '' && baseUrl.trim() !== '' && delayOk && rateOk;

  const save = async () => {
    if (!fieldsDirty || !valid) return;
    setSaving(true); setErr(null);
    try {
      await patch<Account>(`/admin/accounts/${account.id}`, {
        name: name.trim(), kind, base_url: baseUrl.trim(), probe_delay_ms: delayN, notes: notes.trim(),
        // Sent unconditionally: 0 is a meaningful value (unlimited) and the
        // server clears the window alongside it, so "unlimited" is expressible
        // as a real state rather than an omission.
        rate_limit: rateN,
        rate_window: rateN > 0 ? rateWindow : '',
        ...(managementKey.trim() ? { management_key: managementKey.trim() } : {}),
      });
      onSaved();
      onClose();
    } catch (e) {
      if (e instanceof ApiError) setErr(e.detail);
      else setErr({ status: 0, type: 'error', message: e instanceof Error ? e.message : String(e) });
    } finally { setSaving(false); }
  };

  return (
    <Modal open onClose={onClose} title={`Edit ${account.name}`}
      subtitle="Settings save with Save. Keys are validated and saved on their own, from the Keys tab."
      busy={busy} dirty={fieldsDirty || keyDirty} onSubmit={() => void save()}
      footer={(close) => <>
        <button type="button" className={cx(btn.base, btn.ghost)} onClick={close} disabled={busy}>
          {fieldsDirty ? 'Cancel' : 'Close'}
        </button>
        {/* Keys save on their own, inside their tab, so Save is for settings only. */}
        {tab === 'settings' && (
          <button type="submit" disabled={busy || !fieldsDirty || !valid} className={cx(btn.base, btn.primary)}>
            {saving ? <Spinner /> : null} Save
          </button>
        )}
      </>}>
      <div className="mb-5 border-b border-line pb-2">
        <Segmented<'settings' | 'keys'> label="Edit section"
          options={[
            { key: 'settings', label: 'Settings' },
            { key: 'keys', label: `Keys · ${account.keys.length}`, title: 'Add, replace or review API keys' },
          ]}
          value={tab} onChange={setTab} />
      </div>
      <div className={cx(tab !== 'keys' && 'hidden')}>
        <KeyManager account={account} onChanged={onSaved} onBusy={setKeyBusy} onDirty={setKeyDirty} />
      </div>
      <div className={cx('space-y-4', tab !== 'settings' && 'hidden')}>
        <div className="grid gap-4 sm:grid-cols-2">
          <div>
            <label className={labelCls} htmlFor="ea-name">Name</label>
            <input id="ea-name" className={inputCls} value={name} onChange={(e) => setName(e.target.value)} />
          </div>
          <div>
            <label className={labelCls} htmlFor="ea-ns">Namespace</label>
            <input id="ea-ns" className={cx(inputCls, 'font-mono')} value={account.namespace} readOnly disabled />
            <p className="mt-1 text-[12px] text-mute">the prefix in model ids; fixed once created</p>
          </div>
        </div>
        <div className="grid gap-4 sm:grid-cols-2">
          <div>
            <label className={labelCls} htmlFor="ea-kind">Kind</label>
            <select id="ea-kind" className={inputCls} value={kind} disabled={kindLocked} onChange={(e) => setKind(e.target.value)}>
              {/* A kind outside the list (gemini-web, github-copilot, …) must still
                  show as itself, not as whichever option sorts first. */}
              {((KINDS as readonly string[]).includes(kind) ? [...KINDS] : [kind, ...KINDS]).map((k) => <option key={k} value={k}>{k}</option>)}
            </select>
            {kindLocked && <p className="mt-1 text-[12px] text-mute">fixed while the provider has keys or models</p>}
          </div>
          <div>
            <label className={labelCls} htmlFor="ea-delay">Probe delay (ms)</label>
            <input id="ea-delay" inputMode="numeric" className={cx(inputCls, 'font-mono tabular-nums', !delayOk && 'border-bad')}
              value={probeDelay} onChange={(e) => setProbeDelay(e.target.value)} />
            {!delayOk && <p className="mt-1 text-[12px] text-bad">a whole number, 0 or more</p>}
          </div>
        </div>
        <div>
          <label className={labelCls} htmlFor="ea-url">Base URL</label>
          {isKnownEndpoint(account.base_url) ? (
            <div id="ea-url" className="rounded-lg border border-line bg-raised/60 px-3.5 py-2.5 font-mono text-[13px] text-dim">
              {baseUrl} <span className="font-sans text-mute">— known endpoint, fixed</span>
            </div>
          ) : (
            <input id="ea-url" className={cx(inputCls, 'font-mono')} value={baseUrl} onChange={(e) => setBaseUrl(e.target.value.trim())} />
          )}
        </div>
        <div>
          <label className={labelCls} htmlFor="ea-notes">Notes</label>
          <input id="ea-notes" className={inputCls} value={notes} onChange={(e) => setNotes(e.target.value)} />
        </div>

        {extra && (
          <ExtraCredentialField edit extra={extra} id="ea-extra" value={managementKey} onChange={setManagementKey}
            isSet={account.management_key_set} expiresAt={account.management_key_expires_at} />
        )}
        <div className="rounded-lg border border-line bg-raised/60 p-4">
          <SectionLabel>Request budget (provider-wide)</SectionLabel>
          <p className="mt-1 text-[12px] text-mute">
            The provider's own request ceiling, stated its way. It applies to the credential, so every
            model routed through this provider shares it. A spent provider is skipped and the combo fails
            over — the provider is never asked a request it has already refused. Leave 0 for unlimited.
          </p>
          <div className="mt-3 grid gap-4 sm:grid-cols-2">
            <div>
              <label className={labelCls} htmlFor="ea-rate">Requests</label>
              <input id="ea-rate" inputMode="numeric" className={cx(inputCls, 'font-mono tabular-nums', !rateOk && 'border-bad')}
                value={rateLimit} onChange={(e) => setRateLimit(e.target.value)} />
              {!rateOk && <p className="mt-1 text-[12px] text-bad">a whole number, 0 or more</p>}
            </div>
            <div>
              <label className={labelCls} htmlFor="ea-window">Per window</label>
              <select id="ea-window" className={inputCls} value={rateWindow} disabled={!(rateN > 0)}
                onChange={(e) => setRateWindow(e.target.value)}>
                {RATE_WINDOWS.map((w) => <option key={w} value={w}>{w}</option>)}
              </select>
            </div>
          </div>
          {rateN > 0 ? (
            <p className="mt-2 text-[12px] text-accent">
              {rateN} request{rateN === 1 ? '' : 's'} per {rateWindow} — a combo with another hop
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

const PIN_SURFACES = ['chat', 'anthropic', 'responses', 'embeddings', 'images_gen', 'audio_tts', 'audio_stt',
  'images_edit', 'moderations', 'rerank', 'videos_gen', 'music_gen'] as const;

// syncedAt is the account's catalog_synced_at: a manual sync bumps it, so the
// drawer refetches instead of keeping the list it loaded when it opened.
function ModelsDrawer({ id, syncedAt }: { id: number; syncedAt?: string }) {
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
  }, [id, syncedAt]);

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
    // No includeTested choice to make anymore: the button IS re-test-all
    // (docx item 14). A manual click means "probe everything now" — the Stop
    // button and the elapsed counter are the brakes, not a checkbox.
    startSweep(id, { all: true, includeTested: true })
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

  if (err) return <div className="border-t border-line px-5 py-4 text-[13px] text-bad">{err}</div>;
  if (!models) return <div className="border-t border-line px-5 py-4"><Skeleton className="h-3.5 w-40" /><Skeleton className="mt-2 h-3.5 w-56" /></div>;
  if (models.length === 0) return <div className="border-t border-line px-5 py-4 text-[13px] text-mute">No models synced — run “Sync models”.</div>;

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
    const confirmed = pin === 'chat' ? m.chat
      : pin === 'anthropic' ? m.anthropic
      : pin === 'responses' ? m.responses
      : undefined;
    if (!confirmed) return setPendingForce({ model: m.id, pin });
    applyPin(m.id, pin, false);
  };

  const untested = models.filter((m) => m.chat == null && m.anthropic == null && m.responses == null).length;

  return (
    // No CSS fade here: the drawer is rendered inside a <Collapse>, which
    // animates the height. A second fade on top of it runs the same opacity
    // ramp twice and lands out of step with the height.
    <div className="border-t border-line/60 px-5 py-4 md:pl-10">
      {/* Test-all: probe the whole catalog, skip what is already proven, and
          stay stoppable — a 400-model × 3-surface sweep is minutes long. */}
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <span className="text-[12px] text-mute">
          {models.length} model{models.length === 1 ? '' : 's'} · {untested} untested
          {running && (
            <span className="ml-2 text-accent">
              Testing… {sweep?.probing ?? 0} queued{sweep?.elapsed_s != null ? ` · ${sweep.elapsed_s}s` : ''}
            </span>
          )}
        </span>
        <span className="flex items-center gap-2">
          {running ? (
            <BusyButton busy={sweepBusy} className="w-[132px]" onClick={stopTestAll}>Stop</BusyButton>
          ) : (
            <span title="re-tests EVERY model (including proven ones) on all three surfaces — one tiny request each; Stop cancels what has not run">
              <BusyButton busy={sweepBusy} disabled={models.length === 0} className="w-[132px]" onClick={startTestAll}>Test all models</BusyButton>
            </span>
          )}
        </span>
      </div>
      {/* The note line is always reserved, so a note arriving never pushes the table down. */}
      <div className="mb-1 h-[18px] truncate text-[12px] text-dim" title={sweepNote ?? undefined}>{sweepNote}</div>
      <div className="max-h-80 overflow-y-auto pr-1">
        {/* Fixed geometry per row: nothing is inserted or removed while a
            model is tested — the verdict marks dim in place and the second
            line says "Testing…", so the row neither grows nor shifts. */}
        <table className="w-full table-fixed text-[13px]">
          <colgroup>
            <col />
            <col className="hidden w-[236px] md:table-column" />
            <col className="w-[128px]" />
            <col className="w-[76px]" />
          </colgroup>
          <thead className="sticky top-0 z-10 bg-surface">
            <tr className="text-left text-[12px] text-mute">
              <th className="py-2 font-normal">Model</th>
              <th className="hidden py-2 font-normal md:table-cell">Protocols</th>
              <th className="py-2 font-normal">Surface</th>
              <th className="py-2"><span className="sr-only">Test</span></th>
            </tr>
          </thead>
          <tbody>
            {models.map((m) => {
              const testing = testingId === m.id;
              const unprobed = m.chat == null && m.anthropic == null && m.responses == null;
              const queued = running && !testing && unprobed;
              const verdicts = (
                <>
                  <div className={cx('flex flex-wrap items-center gap-x-3 gap-y-0.5 text-[12px] transition-opacity', (testing || queued) && 'opacity-40')}>
                      {([
                        ['Chat', m.chat],
                        ['Messages', m.anthropic],
                        ['Responses', m.responses],
                      ] as const).map(([label, v]) => (
                        <span key={label} className="flex items-center gap-1"
                          title={v == null ? `${label}: no probe` : `${label}: ${v === true ? 'ok' : v === false ? 'failed' : `HTTP ${v}`}`}>
                          <span className={cx('h-1.5 w-1.5 rounded-full', v === true ? 'bg-ok' : v == null ? 'bg-line-strong' : 'bg-bad')} />
                          <span className={v === true ? 'text-dim' : v == null ? 'text-mute' : 'text-bad'}>
                            {label}{typeof v === 'number' ? ` ${v}` : ''}
                          </span>
                        </span>
                      ))}
                    </div>
                    <div className="mt-0.5 text-[11px]" title={m.note ?? undefined}>
                      {testing ? <span className="text-accent">Testing…</span>
                        : queued ? <span className="text-accent">Queued</span>
                        : <span className="text-mute">{m.source === 'curated' ? 'Curated' : m.tested_at ? `Probed ${relTime(m.tested_at)}` : 'Not probed'}</span>}
                    </div>
                </>
              );
              return (
              <tr key={m.id} className="h-12 border-t border-line/60 align-middle">
                <td className="pr-3">
                  {/* Wraps on a phone (no hover there, and ids often share a
                      long prefix); one truncated line on desktop, full id on hover. */}
                  <div title={m.id} className="break-all font-code text-[12px] text-ink md:truncate md:break-normal">{m.id}</div>
                  {/* On a phone the protocols column folds under the name. */}
                  <div className="mt-1 md:hidden">{verdicts}</div>
                </td>
                <td className="hidden md:table-cell">
                  {verdicts}
                </td>
                <td>
                  <select
                    aria-label={`surface for ${m.id}`}
                    value={m.pin ?? ''}
                    onChange={(e) => onPinChange(m, e.target.value)}
                    className={cx('h-7 w-[112px] rounded-md border bg-transparent px-1.5 text-[12px] transition-colors hover:border-line-strong focus:border-accent focus:outline-none',
                      m.pin ? 'border-line-strong text-ink' : 'border-transparent text-mute')}
                  >
                    <option value="">Auto</option>
                    {PIN_SURFACES.map((s) => <option key={s} value={s}>{s}</option>)}
                  </select>
                </td>
                <td className="text-right">
                  <BusyButton busy={testing}
                    className="h-7 w-[60px] px-0 text-[12px]"
                    disabled={running || sweepBusy || m.source === 'curated'}
                    onClick={() => testOne(m)}>
                    <span title={m.source === 'curated'
                      ? 'this verdict is operator-curated — probes never overwrite it; release it first to re-test'
                      : 'one tiny request for THIS model on all three surfaces — tests the wire, not the tool-dialect'}>Test</span>
                  </BusyButton>
                </td>
              </tr>
              );
            })}
          </tbody>
        </table>
        {pinErr && <div className="mt-2 text-[12px] text-bad">{pinErr}</div>}
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
// Rates are versioned: the same model can carry several rows, each effective
// from its own date, and a call is priced by the newest row on or before its
// day. So "update a rate" means adding a row with a new date; re-saving the
// SAME date overwrites that row. Clicking a row loads it into the form to make
// either of those one edit away.
const EMPTY_RATE = { price_in: '', price_out: '', price_cache_read: '', price_cache_write: '', note: '' };
// Rates keep their precision (0.075 must not read 0.08) but line up: two
// decimals minimum, four at most.
const fmtRate = (n: number) => n.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 4 });
const RATE_FIELDS = [
  { key: 'price_in', label: 'Input' },
  { key: 'price_out', label: 'Output' },
  { key: 'price_cache_read', label: 'Cache read' },
  { key: 'price_cache_write', label: 'Cache write' },
] as const;

function PricingModal({ account, onClose }: { account: Account; onClose: () => void }) {
  const [rows, setRows] = useState<Price[] | null>(null);
  const [models, setModels] = useState<ModelRow[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [form, setForm] = useState({ model: '', effective_from: new Date().toISOString().slice(0, 10), ...EMPTY_RATE });
  const [removing, setRemoving] = useState<Price | null>(null);

  const load = useCallback(() => {
    get<Price[]>('/admin/prices')
      .then((ps) => setRows(ps.filter((p) => p.account_id === account.id)))
      .catch((e) => setErr(e instanceof Error ? e.message : 'failed to load prices'));
    get<ModelRow[]>(`/admin/accounts/${account.id}/models`)
      .then(setModels)
      .catch(() => setModels([]));
  }, [account.id]);
  useEffect(() => { load(); }, [load]);

  const sorted = useMemo(() => [...(rows ?? [])].sort((a, b) =>
    a.model_id.localeCompare(b.model_id) || b.effective_from.localeCompare(a.effective_from)), [rows]);
  const dirty = Object.entries(EMPTY_RATE).some(([k]) => (form as Record<string, string>)[k].trim() !== '');
  const existing = sorted.find((p) => p.model_id === form.model && p.effective_from === form.effective_from);

  const setRate = async () => {
    // All four required: the API refuses partial rows so a missing rate can
    // never silently mean "free".
    const nums = RATE_FIELDS.map((f) => form[f.key]);
    if (!form.model || !form.effective_from) { setErr('Pick a model and an effective date.'); return; }
    if (nums.some((v) => v.trim() === '' || Number.isNaN(Number(v)) || Number(v) < 0)) {
      setErr('All four rates are required, as non-negative numbers in USD per 1M tokens. Use 0 for a rate that is genuinely free.');
      return;
    }
    setBusy(true); setErr(null);
    try {
      await setPrice(account.id, {
        model: form.model, effective_from: form.effective_from, currency: 'USD',
        price_in: Number(form.price_in), price_out: Number(form.price_out),
        price_cache_read: Number(form.price_cache_read), price_cache_write: Number(form.price_cache_write),
        note: form.note,
      });
      setForm({ ...form, ...EMPTY_RATE });
      load();
    } catch (e) { setErr(e instanceof Error ? e.message : 'save failed'); }
    finally { setBusy(false); }
  };

  const remove = async (p: Price) => {
    setBusy(true); setErr(null);
    try { await deletePrice(account.id, p.model_id, p.effective_from); load(); }
    catch (e) { setErr(e instanceof Error ? e.message : 'delete failed'); }
    finally { setBusy(false); setRemoving(null); }
  };

  const edit = (p: Price) => {
    setErr(null);
    setForm({
      model: p.model_id, effective_from: p.effective_from,
      price_in: String(p.price_in), price_out: String(p.price_out),
      price_cache_read: String(p.price_cache_read), price_cache_write: String(p.price_cache_write), note: p.note ?? '',
    });
  };

  return (
    <>
    <Modal open onClose={onClose} wide title={`Pricing — ${account.name}`}
      subtitle="USD per 1M tokens, versioned by date · a call is priced by the rate effective on its day"
      busy={busy} dirty={dirty} onSubmit={() => void setRate()}
      footer={(close) => (
        <>
          <button type="button" className={cx(btn.base, btn.ghost)} onClick={close} disabled={busy}>Close</button>
          <button type="submit" className={cx(btn.base, btn.primary)} disabled={busy || !models.length}>
            {busy ? <Spinner /> : null} {existing ? 'Overwrite this rate' : 'Save rate'}
          </button>
        </>
      )}>
      <div className="space-y-5">
        {err && <div role="alert" className="rounded-lg border border-bad/30 bg-bad-dim px-3 py-2 text-[13px] text-bad">{err}</div>}
        <div>
          <SectionLabel>Current rates</SectionLabel>
          {rows === null ? (
            <div className="mt-2 space-y-2"><Skeleton className="h-3.5 w-40" /><Skeleton className="h-3.5 w-56" /></div>
          ) : sorted.length === 0 ? (
            <p className="mt-2 text-[13px] text-mute">No rates yet — every call from this provider is UNPRICED, and counts as $0, until you add one.</p>
          ) : (
            <div className="mt-2 max-h-64 overflow-auto rounded-lg border border-line">
              <table className="w-full min-w-[640px] text-[13px]">
                <thead className="sticky top-0 bg-surface">
                  <tr className="border-b border-line text-left text-[11px] text-mute">
                    <th className="px-3 py-2 font-medium">model</th>
                    <th className="px-3 py-2 font-medium">effective from</th>
                    <th className="px-3 py-2 text-right font-medium">input</th>
                    <th className="px-3 py-2 text-right font-medium">output</th>
                    <th className="px-3 py-2 text-right font-medium">cache read</th>
                    <th className="px-3 py-2 text-right font-medium">cache write</th>
                    <th className="px-3 py-2 text-right font-medium"><span className="sr-only">actions</span></th>
                  </tr>
                </thead>
                <tbody className="font-mono tnum">
                  {sorted.map((p) => (
                    <tr key={`${p.model_id}-${p.effective_from}`} className="border-b border-line/50 last:border-0 hover:bg-wash">
                      <td className="max-w-[220px] truncate px-3 py-1.5 font-sans text-ink" title={p.model_id}>{p.model_id}</td>
                      <td className="px-3 py-1.5 text-dim">{p.effective_from}</td>
                      <td className="px-3 py-1.5 text-right text-ink">{fmtRate(p.price_in)}</td>
                      <td className="px-3 py-1.5 text-right text-ink">{fmtRate(p.price_out)}</td>
                      <td className="px-3 py-1.5 text-right text-ink">{fmtRate(p.price_cache_read)}</td>
                      <td className="px-3 py-1.5 text-right text-ink">{fmtRate(p.price_cache_write)}</td>
                      <td className="whitespace-nowrap px-3 py-1.5 text-right font-sans">
                        <button type="button" disabled={busy} className="text-dim underline underline-offset-2 hover:text-ink disabled:opacity-40"
                          onClick={() => edit(p)} aria-label={`Edit rate for ${p.model_id} from ${p.effective_from}`}>Edit</button>
                        <button type="button" disabled={busy} className="ml-3 text-mute hover:text-bad disabled:opacity-40"
                          onClick={() => setRemoving(p)} aria-label={`Delete rate for ${p.model_id} from ${p.effective_from}`}>Delete</button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>

        <div className="rounded-lg border border-line bg-raised/60 p-4">
          <SectionLabel>Set a rate</SectionLabel>
          <div className="mt-3 grid gap-4 sm:grid-cols-2">
            <div>
              <label className={labelCls} htmlFor="pr-model">Model</label>
              <select id="pr-model" className={inputCls} value={form.model} onChange={(e) => setForm({ ...form, model: e.target.value })}>
                <option value="">Choose a model…</option>
                {models.map((m) => <option key={m.id} value={m.id}>{m.id}</option>)}
              </select>
            </div>
            <div>
              <label className={labelCls} htmlFor="pr-date">Effective from</label>
              <input id="pr-date" type="date" className={inputCls} value={form.effective_from}
                onChange={(e) => setForm({ ...form, effective_from: e.target.value })} />
            </div>
          </div>
          <div className="mt-4 grid gap-4 sm:grid-cols-4">
            {RATE_FIELDS.map((f) => (
              <div key={f.key}>
                <label className={labelCls} htmlFor={`pr-${f.key}`}>{f.label} <span className="text-mute">$/1M</span></label>
                <input id={`pr-${f.key}`} inputMode="decimal" className={cx(inputCls, 'font-mono tnum')} placeholder="0.00"
                  value={form[f.key]} onChange={(e) => setForm({ ...form, [f.key]: e.target.value })} />
              </div>
            ))}
          </div>
          <div className="mt-2">
            <button type="button" className="text-[12px] text-dim underline underline-offset-2 hover:text-ink"
              onClick={() => setForm({ ...form, price_cache_read: form.price_in, price_cache_write: form.price_in })}>
              Cache rates = input rate
            </button>
          </div>
          <div className="mt-4">
            <label className={labelCls} htmlFor="pr-note">Note <span className="text-mute">(optional)</span></label>
            <input id="pr-note" className={inputCls} value={form.note} onChange={(e) => setForm({ ...form, note: e.target.value })} />
          </div>
          {existing && <p className="mt-3 text-[12px] text-warn">A rate for this model on {form.effective_from} exists and will be overwritten. Pick a later date to keep the old one for earlier calls.</p>}
          {!models.length && <p className="mt-3 text-[12px] text-mute">Sync this provider&rsquo;s models first — a rate can only name a model the catalog knows.</p>}
        </div>
      </div>
    </Modal>
    <ConfirmModal open={removing != null} title="Delete this rate?" confirmLabel="Delete" busy={busy}
      body={removing ? `${removing.model_id}, effective from ${removing.effective_from}. Calls from that date on fall back to the previous rate, or to unpriced ($0) if there is none.` : ''}
      onClose={() => setRemoving(null)} onConfirm={() => { if (removing) void remove(removing); }} />
    </>
  );
}

// What the claude.ai usage read carries beyond the two headline windows: the
// Opus/Sonnet weekly windows and the per-product split of the week (Claude Code
// vs chat), plus the provenance tags the server stamps on the raw body. Anything
// absent or null is simply not drawn.
function ClaudeUsageExtras({ raw }: { raw: string }) {
  let o: Record<string, unknown> | null = null;
  try { const v: unknown = JSON.parse(raw); o = v && typeof v === 'object' ? (v as Record<string, unknown>) : null; } catch { o = null; }
  if (!o) return null;
  const util = (k: string): number | null => {
    const w = o?.[k] as { utilization?: unknown } | null | undefined;
    return typeof w?.utilization === 'number' ? w.utilization : null;
  };
  const rows: Array<[string, number]> = [];
  const opus = util('seven_day_opus');
  const sonnet = util('seven_day_sonnet');
  if (opus != null) rows.push(['Opus · weekly', opus]);
  if (sonnet != null) rows.push(['Sonnet · weekly', sonnet]);
  const split = (o.seven_day_breakdown as { rows?: Array<{ display_name?: string; percent?: number }> } | null | undefined)?.rows ?? [];
  for (const r of split) {
    if (typeof r?.percent === 'number' && r.display_name) rows.push([`${r.display_name} · share of week`, r.percent]);
  }
  const source = typeof o.source === 'string' ? o.source : '';
  const liveErr = typeof o.live_read_error === 'string' ? o.live_read_error : '';
  if (rows.length === 0 && !source && !liveErr) return null;
  return (
    <div className="space-y-1.5">
      {liveErr && (
        <p className="text-[12px] text-warn">
          Live claude.ai read failed ({liveErr}) — showing the bridge&apos;s last observed reading. Paste a fresh cookie to restore it.
        </p>
      )}
      {source && !liveErr && <p className="text-[12px] text-mute">source: {source}</p>}
      {rows.map(([k, v]) => (
        <div key={k} className="flex items-center gap-2.5">
          <span className="w-44 flex-none truncate text-[11px] text-mute">{k}</span>
          <span className="tnum w-10 text-right font-mono text-[12px] text-dim">{Math.round(v)}%</span>
        </div>
      ))}
    </div>
  );
}

function QuotaModal({ account, onClose, onRead }: { account: Account; onClose: () => void; onRead?: () => void }) {
  const [live, setLive] = useState<QuotaLive | null>(null);
  const [busy, setBusy] = useState(true);
  const [err, setErr] = useState<ApiErrorDetail | null>(null);
  const [rules, setRules] = useState<ModelRule[] | null>(null);
  const [rulesError, setRulesError] = useState(false);
  const [rulesAttempt, setRulesAttempt] = useState(0);

  const load = useCallback(() => {
    setBusy(true);
    setErr(null);
    get<QuotaLive>(`/admin/accounts/${account.id}/quota`)
      .then((q) => { setLive(q); onRead?.(); })
      .catch((e: unknown) => setErr({ status: 0, type: 'error', message: e instanceof Error ? e.message : 'quota read failed' }))
      .finally(() => setBusy(false));
  }, [account.id]);

  useEffect(() => { load(); }, [load]);
  useEffect(() => {
    let alive = true;
    setRules(null); setRulesError(false);
    modelRules()
      .then((rs) => { if (alive) setRules(rs.filter((r) => r.account_id === account.id)); })
      .catch(() => { if (alive) setRulesError(true); });
    return () => { alive = false; };
  }, [account.id, rulesAttempt]);

  const caps = (rules ?? []).filter((r) => r.cap_tokens > 0);

  return (
    <Modal open onClose={onClose} title={`Quota — ${account.name}`}
      subtitle="live provider quota read · caps come from Rules"
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
          {busy && !live && <Skeleton className="mt-2 h-3.5 w-56" />}
          {!busy && live?.kind === 'none' && (
            <p className="mt-2 text-[13px] text-mute">This provider exposes no quota endpoint its key can read.</p>
          )}
          {live && live.kind !== 'none' && (
            <div className="mt-2 space-y-3">
              <div className="flex flex-wrap items-center gap-2 text-[13px]">
                <span className="rounded-full border border-line bg-bg px-2 py-0.5 font-mono text-[12px] text-accent">{live.kind}</span>
                <span className="text-mute">read {live.read_at ? relTime(live.read_at) : '—'}</span>
                {account.quota?.plan && <span className="text-dim">· {account.quota.plan}</span>}
                {account.quota?.resets_at && <span className="text-mute">· resets {relTime(account.quota.resets_at)}</span>}
              </div>
              {/* The reading first, as the card shows it; the provider's own
                  response is evidence, one click away, not the headline. */}
              {account.quota && account.quota.kind !== 'none' && account.quota.kind !== 'unknown' ? (
                <div className="max-w-md"><QuotaBars quota={account.quota} /></div>
              ) : null}
              <ClaudeUsageExtras raw={live.raw} />
              <details className="group">
                <summary className="cursor-pointer text-[13px] text-dim hover:text-ink">Show raw provider response</summary>
                <pre className="mt-2 max-h-56 overflow-auto whitespace-pre-wrap break-all rounded-md border border-line bg-bg px-3 py-2 font-mono text-[12px] leading-relaxed text-dim">
                  {prettyBody(live.raw)}
                </pre>
              </details>
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
              Unlimited — no request ceiling is set for this account. Set one under Edit &amp; keys if the
              provider enforces its own rate limit.
            </p>
          )}
        </div>
        <div className="rounded-lg border border-line bg-raised/60 p-4">
          <SectionLabel>Cap window · cap tokens</SectionLabel>
          {rulesError && <p role="alert" className="mt-2 text-[13px] text-warn">Could not load account caps. <button type="button" className="underline" onClick={() => setRulesAttempt(n => n + 1)}>Retry caps</button></p>}
          {rules === null && !rulesError && <Skeleton className="mt-2 h-3.5 w-48" />}
          {rules !== null && caps.length === 0 && (
            <p className="mt-2 text-[13px] text-mute">No caps set for this account — add one under <span className="text-ink">Rules</span>.</p>
          )}
          {caps.length > 0 && (
            <table className="mt-2 w-full text-[13px]">
              <thead>
                <tr className="text-left text-[11px] text-mute">
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

function AccountCard({ a, open, onToggle, intent, onIntentDone, issues, onChanged, onReloadQuota, quotaBusy }: {
  a: Account; open: boolean; onToggle: () => void;
  /** A deep link asked for this card's Edit or Pricing modal. */
  intent: 'edit' | 'keys' | 'pricing' | null; onIntentDone: () => void;
  issues: Issue[];
  onChanged: () => void;
  onReloadQuota: () => void; quotaBusy: boolean;
}) {
  const [menuBusy, setMenuBusy] = useState<string | null>(null);
  const [err, setErr] = useState<ApiErrorDetail | null>(null);
  const [flash, setFlash] = useState<string | null>(null);
  const [editing, setEditing] = useState(false);
  const [editTab, setEditTab] = useState<'settings' | 'keys'>('settings');
  const [quotaOpen, setQuotaOpen] = useState(false);
  const [pricingOpen, setPricingOpen] = useState(false);
  const [confirm, setConfirm] = useState<null | { kind: 'account' } | { kind: 'key'; keyID: number; label: string }>(null);

  // The quota button is only live once a probe proved this provider exposes a
  // quota read (generic /v1/usage or a host-scoped balance oracle): no
  // snapshot or kind "none" both mean the quota read is not available.
  const quotaExposed = a.quota != null && a.quota.kind !== 'none';
  const quotaHint = a.quota == null
    ? 'quota not available — run Test key to check'
    : a.quota.kind === 'none' ? 'quota not available — this provider exposes no usage endpoint' : 'live provider quota read';

  useEffect(() => {
    if (!intent) return;
    if (intent === 'pricing') { setPricingOpen(true); }
    else { setEditTab(intent === 'keys' ? 'keys' : 'settings'); setEditing(true); }
    onIntentDone();
  }, [intent, onIntentDone]);

  const act = async (label: string, fn: () => Promise<unknown>) => {
    setMenuBusy(label); setErr(null); setFlash(null);
    try {
      await fn();
      setFlash(label);
      onChanged();
    } catch (e) {
      if (e instanceof ApiError) setErr(e.detail);
      else setErr({ status: 0, type: 'error', message: e instanceof Error ? e.message : String(e) });
      // A failed action can still have changed what the card shows (a key
      // test that failed has recorded its verdict), so refresh either way.
      onChanged();
    } finally { setMenuBusy(null); setTimeout(() => setFlash(null), 2500); }
  };

  // Every key, one after the other: a provider with two keys needs both
  // proven, and one bad key must not stop the others being tested.
  const testAll = async () => {
    let failed = 0;
    for (const k of a.keys) {
      try { await post(`/admin/accounts/${a.id}/keys/${k.id}/retest`, {}); } catch { failed++; }
    }
    if (failed > 0) throw new Error(`${failed} of ${a.keys.length} ${a.keys.length === 1 ? 'key' : 'keys'} failed the test — the reason is under each key.`);
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

  // One status mark per row: the worst key verdict (a failed key outranks
  // an untested one, which outranks ok). No keys reads as untested.
  const keyState: 'ok' | 'bad' | 'none' = a.keys.some((k) => k.last_test_ok === false) ? 'bad'
    : a.keys.length > 0 && a.keys.every((k) => k.last_test_ok === true) ? 'ok' : 'none';
  const keyStateText = keyState === 'ok' ? 'keys tested ok' : keyState === 'bad' ? 'a key failed its last test' : 'keys untested';

  return (
    <div className={cx('transition-colors', !open && 'hover:bg-wash')}>
      {/* The row: identity left, then three fixed columns (models, quota,
          chevron) so every figure in the roster sits on one right edge. */}
      <div className="grid grid-cols-[minmax(0,1fr)_auto] items-center gap-x-4 gap-y-2 px-5 py-3.5 md:grid-cols-[minmax(0,1fr)_88px_216px_20px]">
        <button type="button" className="col-span-1 flex min-w-0 items-center gap-3 text-left" onClick={onToggle} aria-expanded={open}>
          <span title={keyStateText} className={cx('h-2 w-2 flex-none rounded-full',
            keyState === 'ok' ? 'bg-ok' : keyState === 'bad' ? 'bg-bad' : 'bg-shade ring-1 ring-line-strong')} />
          <span className="min-w-0">
            <span className="flex items-center gap-2">
              <span className="truncate text-[14px] font-semibold text-ink">{a.name}</span>
              {!a.enabled && <Chip tone="warn">disabled</Chip>}
              {issues.length > 0 && (
                <Chip tone={issues.some((i) => i.tone === 'bad') ? 'bad' : 'warn'} title={issues.map((i) => i.text).join('\n')}>
                  {issues.length === 1 ? '1 issue' : `${issues.length} issues`}
                </Chip>
              )}
            </span>
            <span className="mt-0.5 flex min-w-0 items-center gap-1.5 text-[12px] text-mute">
              <span className="font-code flex-none text-dim">{a.namespace}</span>
              <span aria-hidden="true">·</span>
              <span className="flex-none">{a.kind}</span>
              <span aria-hidden="true" className="hidden sm:inline">·</span>
              <span className="hidden truncate sm:inline" title={a.base_url}>{hostOf(a.base_url)}</span>
            </span>
          </span>
        </button>
        <button type="button" tabIndex={-1} aria-hidden="true" onClick={onToggle}
          title={a.catalog_synced_at ? `catalog synced ${relTime(a.catalog_synced_at)}` : 'catalog not synced yet'}
          className="hidden text-right text-[13px] md:block">
          <span className="tnum text-ink">{a.models_count != null ? nfmt(a.models_count) : '–'}</span>
          <span className="ml-1 text-mute">models</span>
          <span className="block text-[11px] text-mute">
            {a.catalog_synced_at ? `synced ${relTime(a.catalog_synced_at)}` : 'not synced yet'}
          </span>
        </button>
        <div className="col-span-2 flex min-w-0 items-center gap-2 md:col-span-1">
          <div className="min-w-0 flex-1"><QuotaSummary quota={a.quota} /></div>
          <button type="button" disabled={quotaBusy}
            aria-label={`refresh quota for ${a.name}`}
            title={a.quota?.read_at
              ? `read ${relTime(a.quota.read_at)}${stale(a.quota) ? ' — stale, refresh from the provider\u2019s usage endpoint' : ''}`
              : 'refresh quota from the provider\u2019s usage endpoint'}
            className={cx('grid h-6 w-6 flex-none place-items-center rounded-md text-[13px] leading-none transition-colors disabled:opacity-40',
              stale(a.quota) ? 'text-accent hover:bg-accent-dim' : 'text-mute hover:bg-hover hover:text-ink')}
            onClick={onReloadQuota}>
            {quotaBusy ? <Spinner className="h-3 w-3" /> : '↻'}
          </button>
        </div>
        <button type="button" tabIndex={-1} aria-hidden="true" onClick={onToggle}
          className="row-start-1 col-start-2 justify-self-end text-mute md:col-start-4">
          <motion.span className="inline-block" animate={{ rotate: open ? 180 : 0 }} transition={{ duration: 0.22 }}>⌄</motion.span>
        </button>
      </div>

      {/* The expandable body animates its HEIGHT, not just its opacity: the
          row below moves by a real, measurable amount, and a fade alone
          leaves the layout snapping open under the operator's cursor. */}
      <Collapse open={open}>
        <div className="px-5 pb-4 md:pl-10">
          <div className="flex flex-wrap items-center gap-2">
            <BusyButton busy={menuBusy === 'sync'} disabled={menuBusy != null}
              onClick={() => void act('sync', () => post(`/admin/accounts/${a.id}/sync`, {}))}>Sync models</BusyButton>
            <BusyButton busy={menuBusy === 'test keys'} disabled={menuBusy != null || a.keys.length === 0}
              onClick={() => void act('test keys', testAll)}>{a.keys.length > 1 ? 'Test all keys' : 'Test key'}</BusyButton>
            <span title={quotaHint}>
              <BusyButton disabled={!quotaExposed} onClick={() => setQuotaOpen(true)}>Quota</BusyButton>
            </span>
            <span title="Set per-model USD rates for cost analysis">
              <BusyButton onClick={() => setPricingOpen(true)}>Pricing</BusyButton>
            </span>
            <span title="Provider settings, and add or replace its API keys">
              <BusyButton onClick={() => { setEditTab('settings'); setEditing(true); }}>Edit &amp; keys</BusyButton>
            </span>
            <label className="flex items-center gap-2 text-[13px] text-dim" title={a.enabled ? 'Routing may use this provider' : 'Disabled: routing skips this provider'}>
              <Toggle checked={a.enabled} disabled={menuBusy != null} label={`${a.name} enabled`}
                onChange={(v) => void act(v ? 'enable' : 'disable', () => patch(`/admin/accounts/${a.id}`, { enabled: v }))} />
              Enabled
            </label>
            {/* The result slot has a fixed place at the row's end, so a flash
                or a busy word never pushes the buttons around. */}
            <span className="min-w-24 text-[12px]" aria-live="polite">
              {flash ? <span className="text-ok">{flash} ✓</span> : menuBusy ? <span className="text-mute">{menuBusy}…</span> : null}
            </span>
            <button type="button" className={cx(btn.base, 'ml-auto h-[30px] border border-transparent px-3 text-[13px] text-bad hover:bg-bad-dim')}
              onClick={() => setConfirm({ kind: 'account' })}>
              Delete provider
            </button>
          </div>

          {/* Keys: one row each, the verdict in words beside its dot. */}
          <div className="mt-4">
            <div className="mb-1.5 text-[12px] text-mute">Keys</div>
            {a.keys.length === 0
              ? (
                <p className="text-[13px] text-mute">
                  No keys, so this provider cannot serve.{' '}
                  <button type="button" className="text-ink underline underline-offset-2" onClick={() => { setEditTab('keys'); setEditing(true); }}>Add a key</button>
                </p>
              )
              : (
                <ul className="divide-y divide-line/60 rounded-lg border border-line bg-surface">
                  {a.keys.map((k) => (
                    <li key={k.id} className="grid grid-cols-[minmax(0,1fr)_auto] items-center gap-x-4 px-3.5 py-2 sm:grid-cols-[minmax(0,1fr)_180px_auto]">
                      <span className={cx('min-w-0 truncate text-[13px]', !k.enabled && 'opacity-60')}>
                        <span className="text-ink">{k.label}</span>
                        <span className="font-code ml-2 text-[12px] text-mute">{k.hint}</span>
                      </span>
                      <span className="hidden items-center gap-1.5 text-[12px] sm:flex" title={k.last_test_detail ?? ''}>
                        <span className={cx('h-1.5 w-1.5 rounded-full', k.last_test_ok === true ? 'bg-ok' : k.last_test_ok === false ? 'bg-bad' : 'bg-line-strong')} />
                        <span className={k.last_test_ok === true ? 'text-ok' : k.last_test_ok === false ? 'text-bad' : 'text-mute'}>
                          {k.last_test_ok === true ? 'Tested ok' : k.last_test_ok === false ? 'Test failed' : 'Untested'}
                        </span>
                        {k.last_test_at && <span className="text-mute">· {relTime(k.last_test_at)}</span>}
                      </span>
                      <span className="flex items-center gap-1">
                        <Toggle checked={k.enabled} disabled={menuBusy != null} label={`Key ${k.label} enabled`}
                          onChange={(v) => void act(`key ${k.id}`, () => patch(`/admin/accounts/${a.id}/keys/${k.id}`, { enabled: v }))} />
                        <IconButton label="Test this key" disabled={menuBusy != null} busy={menuBusy === `retest ${k.id}`}
                          onClick={() => void act(`retest ${k.id}`, () => post(`/admin/accounts/${a.id}/keys/${k.id}/retest`, {}))}>↻</IconButton>
                        <IconButton label="Delete key" tone="bad" disabled={menuBusy != null}
                          onClick={() => setConfirm({ kind: 'key', keyID: k.id, label: `${k.label} · ${k.hint}` })}>✕</IconButton>
                      </span>
                      {/* Why it failed, in words: it used to live only in a hover tooltip. */}
                      {k.enabled && k.last_test_ok === false && k.last_test_detail && (
                        <p className="col-span-full mt-1 break-words text-[12px] text-bad">{k.last_test_detail}</p>
                      )}
                    </li>
                  ))}
                </ul>
              )}
          </div>
          {a.quota && a.quota.kind !== 'none' && a.quota.kind !== 'unknown' && (
            <div className="mt-4 max-w-md">
              <div className="mb-1.5 text-[12px] text-mute">
                Quota{a.quota.read_at && <span title={`snapshot read ${a.quota.read_at}`}> · read {relTime(a.quota.read_at)}</span>}
              </div>
              <QuotaBars quota={a.quota} />
            </div>
          )}
          {editing && <EditAccountModal account={a} initialTab={editTab} onClose={() => setEditing(false)} onSaved={onChanged} />}
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
          {quotaOpen && <QuotaModal account={a} onClose={() => setQuotaOpen(false)} onRead={onChanged} />}
          {pricingOpen && <PricingModal account={a} onClose={() => setPricingOpen(false)} />}
          <ErrBox e={err} onDismiss={() => setErr(null)} />
        </div>
      </Collapse>

      {/* The models drawer is a SEPARATE collapse: it is its own long table
          and animating it inside the section above would nest two height
          animations and leave the row's frame out of step with its content. */}
      <Collapse open={open}>
        <ModelsDrawer id={a.id} syncedAt={a.catalog_synced_at} />
      </Collapse>
    </div>
  );
}

// ── page ────────────────────────────────────────────────────────────────

type Intent = { id: number; kind: 'edit' | 'keys' | 'pricing' };

export default function Providers() {
  const [accounts, setAccounts] = useState<Account[] | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [addOpen, setAddOpen] = useState(false);
  const [quotaBusy, setQuotaBusy] = useState<number | null>(null);
  const [query, setQuery] = useState('');
  const [onlyIssues, setOnlyIssues] = useState(false);
  const [openIds, setOpenIds] = useState<Set<number>>(new Set());
  const [intent, setIntent] = useState<Intent | null>(null);
  const [params, setParams] = useSearchParams();

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

  // Deep links: ?open=<id> expands that provider, ?edit=<id> and ?pricing=<id>
  // also open the matching modal. The Dashboard's Needs-attention strip and the
  // Economics page link here. Consumed once the providers have loaded, then
  // removed from the URL so a refresh does not re-open it.
  useEffect(() => {
    if (!accounts) return;
    for (const kind of ['open', 'edit', 'keys', 'pricing'] as const) {
      const raw = params.get(kind);
      if (raw == null) continue;
      const id = Number(raw);
      if (accounts.some((x) => x.id === id)) {
        setOpenIds((cur) => new Set(cur).add(id));
        if (kind !== 'open') setIntent({ id, kind });
      }
      setParams((p) => { const n = new URLSearchParams(p); n.delete(kind); return n; }, { replace: true });
    }
  }, [accounts, params, setParams]);
  const clearIntent = useCallback(() => setIntent(null), []);

  const sorted = useMemo(() => [...(accounts ?? [])].sort((a, b) => a.namespace.localeCompare(b.namespace)), [accounts]);
  const issuesOf = useMemo(() => new Map(sorted.map((a) => [a.id, accountIssues(a)])), [sorted]);
  const attention = sorted.filter((a) => (issuesOf.get(a.id) ?? []).length > 0).length;
  const needle = query.trim().toLowerCase();
  const shown = sorted.filter((a) =>
    (!onlyIssues || (issuesOf.get(a.id) ?? []).length > 0)
    && (needle === '' || [a.name, a.namespace, a.kind, hostOf(a.base_url)].some((v) => (v ?? '').toLowerCase().includes(needle))));
  const allOpen = shown.length > 0 && shown.every((a) => openIds.has(a.id));
  const toggleOne = (id: number) => setOpenIds((cur) => { const n = new Set(cur); if (n.has(id)) n.delete(id); else n.add(id); return n; });
  const toggleAll = () => setOpenIds((cur) => {
    const n = new Set(cur);
    if (allOpen) shown.forEach((a) => n.delete(a.id)); else shown.forEach((a) => n.add(a.id));
    return n;
  });

  return (
    // No page-root fade here: the route transition in App.tsx already fades
    // the page in. Two entrances stacked on the same surface reads as a
    // stutter, not as polish.
    <div>
      <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
        <span className="text-[13px] text-mute" aria-live="polite">
          {accounts ? (
            <>
              <span className="tnum text-ink">{nfmt(accounts.length)}</span> provider{accounts.length === 1 ? '' : 's'}
              {attention > 0 && <> · <span className="tnum text-warn">{nfmt(attention)}</span> need{attention === 1 ? 's' : ''} attention</>}
            </>
          ) : '\u00a0'}
        </span>
        <button type="button" className={cx(btn.base, btn.primary)} onClick={() => setAddOpen(true)}>
          <span className="text-[15px] leading-none">＋</span> Add provider
        </button>
      </div>

      {accounts && accounts.length > 0 && (
        <div className="mb-4 flex flex-wrap items-center gap-3">
          <input type="search" value={query} onChange={(e) => setQuery(e.target.value)} aria-label="Search providers"
            placeholder="Search name, namespace, kind or host" className={cx(inputCls, '!w-72 max-w-full !py-1.5')} />
          <button type="button" aria-pressed={onlyIssues} onClick={() => setOnlyIssues((v) => !v)}
            className={cx('rounded-full border px-3 py-1 text-[13px] transition-colors',
              onlyIssues ? 'border-warn/50 bg-warn-dim text-ink' : 'border-line text-dim hover:text-ink')}>
            Needs attention{attention > 0 ? ` ${attention}` : ''}
          </button>
          <button type="button" onClick={toggleAll} disabled={shown.length === 0}
            className="ml-auto text-[13px] text-dim underline underline-offset-2 hover:text-ink disabled:opacity-40">
            {allOpen ? 'Collapse all' : 'Expand all'}
          </button>
        </div>
      )}

      {err && <div className="mb-4 rounded-lg border border-bad/30 bg-bad-dim px-4 py-3 text-[13px] text-bad">{err}</div>}

      {accounts === null ? (
        <SkeletonCards n={3} />
      ) : accounts.length === 0 ? (
        <div className="rounded-xl border border-line bg-surface px-6 py-14 text-center">
          <p className="text-[14px] font-medium">No providers yet</p>
          <p className="mt-1 text-[13px] text-mute">Add your first one — name it, drop in an API key, and the 7-step test runs automatically.</p>
          <button type="button" className={cx(btn.base, btn.primary, 'mt-5')} onClick={() => setAddOpen(true)}>＋ Add your first provider</button>
        </div>
      ) : shown.length === 0 ? (
        <div className="rounded-xl border border-dashed border-line px-6 py-12 text-center">
          <p className="text-[14px] font-medium">No provider matches.</p>
          <p className="mt-1 text-[13px] text-mute">
            {onlyIssues && needle === '' ? 'Nothing needs attention right now.' : 'Try a different search.'}
          </p>
          <button type="button" className={cx(btn.base, btn.ghost, 'mt-4')} onClick={() => { setQuery(''); setOnlyIssues(false); }}>Clear filters</button>
        </div>
      ) : (
        <div className="overflow-hidden rounded-xl border border-line bg-surface">
          {/* Alphabetical by namespace — the add-provider preset list sorts the
              same way, so the two lists read in one order.
              The Stagger lets the cards arrive as a sequence instead of all
              at once; it is a one-shot entrance, so the 60s quota refresh
              (which re-renders this list) never replays it. */}
          <Stagger className="divide-y divide-line">
            {shown.map((a) => (
              <StaggerItem key={a.id}>
                <AccountCard a={a} open={openIds.has(a.id)} onToggle={() => toggleOne(a.id)}
                  intent={intent?.id === a.id ? intent.kind : null} onIntentDone={clearIntent}
                  issues={issuesOf.get(a.id) ?? []}
                  onChanged={() => load(true)}
                  quotaBusy={quotaBusy === a.id}
                  onReloadQuota={() => void reloadQuota(a.id)} />
              </StaggerItem>
            ))}
          </Stagger>
        </div>
      )}

      <AddProviderModal open={addOpen} onClose={() => setAddOpen(false)} onDone={() => { setAddOpen(false); load(true); }} />
      {/* edit modal is created per-card when needed */}
    </div>
  );
}
