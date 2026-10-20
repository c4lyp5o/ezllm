// CONNECT — the client-facing surface: routes, keys, and what to paste where.
//
// Two things this page gets right on purpose:
//  1. The base URL is derived from window.location, never hardcoded. The same
//     binary answers on localhost, LAN IP or a reverse-proxied hostname; a
//     literal "http://127.0.0.1:20129" copied to a phone is instantly wrong.
//  2. A created key's plaintext is shown exactly once. The server keeps only a
//     hash, so if this UI loses it, it is gone — hence the blocking modal with
//     no way to dismiss until copy, and the "I saved it" confirmation.
import { useCallback, useEffect, useMemo, useState } from 'react';
import { motion } from 'motion/react';
import { ApiError, getToken, type UsageRow } from '../utils/api';
import { Chip, ConfirmModal, Modal, Mono, SectionLabel, Spinner, Toggle, btn, cx, inputCls, labelCls } from '../components/ui';
import { Stagger, StaggerItem } from '../components/motion';
import {
  createToken, listTokens, patchToken, probeModels, revokeToken, usageByClient,
  type TokenCreated, type TokenInfo, type V1Model,
} from '../utils/tokens';

// The gateway speaks two client protocols on one port under /v1: OpenAI and
// Anthropic. The OpenAI Responses route still exists server-side for other
// clients but is not surfaced here.
const SURFACES = [
  // The value is the BASE URL a client of that dialect is configured with —
  // not the endpoint a single request hits. OpenAI clients take the whole
  // base including /v1; Anthropic clients append /v1/messages themselves, so
  // their base is the origin bare. That difference is why the base url row
  // differs between the two boxes, and it is the single most common mistake.
  { key: 'openai', label: 'OpenAI', path: '/v1' },
  { key: 'anthropic', label: 'Anthropic', path: '' },
] as const;

type SurfaceKey = (typeof SURFACES)[number]['key'];

// Browser clients send a preflight. ezllm answers it for /v1/* out of the box
// (unset = any Origin echoed back), so the honest note is
// how to RESTRICT it — telling users to "set the env var to make it work" would
// be backwards and would send someone chasing a fix for a non-problem.
const CORS_NOTE = (
  <>
    {' '}
    · browser preflights are allowed on <code className="font-mono text-dim">/v1/*</code> out of
    the box; set <code className="font-mono text-dim">EZLLM_CORS_ORIGINS</code> to a
    comma-separated allowlist to restrict it
  </>
);

function CopyIcon() {
  return (
    <svg viewBox="0 0 24 24" className="h-3.5 w-3.5" fill="none" stroke="currentColor" strokeWidth="2">
      <rect x="9" y="9" width="11" height="11" rx="2" />
      <path d="M5 15V6a2 2 0 0 1 2-2h9" />
    </svg>
  );
}

function CheckIcon() {
  return (
    <svg viewBox="0 0 24 24" className="h-3.5 w-3.5" fill="none" stroke="currentColor" strokeWidth="2.4">
      <path d="m5 13 4 4L19 7" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

/** Copy to clipboard with a fallback: non-secure contexts (LAN over plain HTTP)
 *  have no navigator.clipboard, and silently failing here would make the page
 *  useless on exactly the phone-on-Wi-Fi setup it exists for. */
async function copyText(s: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(s);
    return true;
  } catch {
    try {
      const ta = document.createElement('textarea');
      ta.value = s;
      ta.setAttribute('readonly', '');
      ta.style.position = 'fixed';
      ta.style.opacity = '0';
      document.body.appendChild(ta);
      ta.select();
      const ok = document.execCommand('copy');
      document.body.removeChild(ta);
      return ok;
    } catch {
      return false;
    }
  }
}

function CopyButton({ text, label = 'Copy' }: { text: string; label?: string }) {
  const [done, setDone] = useState(false);
  return (
    <button
      type="button"
      className={cx(btn.base, btn.ghost, 'px-2.5 py-1.5 text-[12px]')}
      onClick={async () => {
        if (await copyText(text)) {
          setDone(true);
          setTimeout(() => setDone(false), 1400);
        }
      }}
    >
      {done ? <CheckIcon /> : <CopyIcon />}
      {done ? 'Copied' : label}
    </button>
  );
}

/** One-line value + copy, the shape every route/key row needs. */
function FieldRow({ label, value, mono = true }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="flex items-center gap-2">
      <span className="w-24 shrink-0 text-[12px] text-mute">{label}</span>
      <code className={cx('min-w-0 flex-1 truncate rounded-md border border-line bg-raised px-2 py-1 text-[13px]', mono && 'font-mono')}>{value}</code>
      <CopyButton text={value} />
    </div>
  );
}

function snippet(surface: SurfaceKey, base: string, key: string, model: string): string {
  if (surface === 'anthropic') {
    // Anthropic clients append /v1/messages themselves — the base is bare.
    return [
      'ANTHROPIC_BASE_URL=' + base,
      'ANTHROPIC_API_KEY=' + key,
      'ANTHROPIC_MODEL=' + model,
    ].join('\n');
  }
  // OpenAI clients take the base WITH /v1 — without it every request 404s.
  return [
    'OPENAI_BASE_URL=' + base + '/v1',
    'OPENAI_API_KEY=' + key,
    'OPENAI_MODEL=' + model,
  ].join('\n');
}

export default function Connect() {
  const [tokens, setTokens] = useState<TokenInfo[] | null>(null);
  const [usage, setUsage] = useState<UsageRow[] | null>(null);
  const [usageWindows, setUsageWindows] = useState<Record<string, UsageRow[]>>({});
  const [models, setModels] = useState<V1Model[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const [fresh, setFresh] = useState<TokenCreated | null>(null);
  const [name, setName] = useState('');
  const [role, setRole] = useState<'infer' | 'admin'>('infer');
  const [newModels, setNewModels] = useState('');
  const [newExpiry, setNewExpiry] = useState('');
  const [busyId, setBusyId] = useState<number | null>(null);
  const [pendingRevoke, setPendingRevoke] = useState<TokenInfo | null>(null);
  const [budgetTarget, setBudgetTarget] = useState<TokenInfo | null>(null);
  const [budgetCap, setBudgetCap] = useState(0);
  const [budgetWindow, setBudgetWindow] = useState('monthly');
  const [budgetModels, setBudgetModels] = useState('');
  const [budgetExpiry, setBudgetExpiry] = useState('');
  const [budgetBusy, setBudgetBusy] = useState(false);

  // Base URL: same origin as the dashboard, minus the trailing slash.
  const base = useMemo(() => window.location.origin, []);
  const reload = useCallback(async () => {
    setErr(null);
    try {
      const [tk, us, us5h, usDaily, usWeekly] = await Promise.all([
        listTokens(),
        usageByClient(30).catch(() => [] as UsageRow[]),
        usageByClient(5 / 24).catch(() => [] as UsageRow[]),
        usageByClient(1).catch(() => [] as UsageRow[]),
        usageByClient(7).catch(() => [] as UsageRow[]),
      ]);
      setTokens(tk);
      setUsage(us);
      setUsageWindows({ '5h': us5h, daily: usDaily, weekly: usWeekly, monthly: us });
      // Probe the live route with the admin token (a valid bearer for /v1 too),
      // so the "supported" chips below are observations, not labels.
      try {
        const admin = getToken();
        const ms = admin ? await probeModels(admin) : [];
        setModels(ms);
      } catch {
        setModels([]);
      }
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : 'could not load');
    }
  }, []);

  useEffect(() => { void reload(); }, [reload]);

  const doCreate = async () => {
    if (!name.trim()) return;
    setCreating(true);
    try {
      const t = await createToken(name.trim(), role, {
        allowed_models: newModels.trim(),
        expires_at: newExpiry,
      });
      setFresh(t);
      setName('');
      setNewModels('');
      setNewExpiry('');
      await reload();
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : 'create failed');
    } finally {
      setCreating(false);
    }
  };

  const toggle = async (t: TokenInfo, enabled: boolean) => {
    setBusyId(t.id);
    try { await patchToken(t.id, { enabled }); setTokens((p) => (p ?? []).map((x) => (x.id === t.id ? { ...x, enabled } : x))); }
    finally { setBusyId(null); }
  };

  const saveBudget = async () => {
    if (!budgetTarget) return;
    setBudgetBusy(true);
    try {
      const updated = await patchToken(budgetTarget.id, {
        cap_tokens: budgetCap,
        cap_window: budgetWindow,
        allowed_models: budgetModels.trim(),
        expires_at: budgetExpiry,
      });
      setTokens((cur) => cur?.map((t) => (t.id === updated.id ? updated : t)) ?? null);
      setBudgetTarget(null);
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : 'could not save budget');
    } finally { setBudgetBusy(false); }
  };

  const revoke = async (t: TokenInfo) => {
    setBusyId(t.id);
    try { await revokeToken(t.id); await reload(); }
    finally { setBusyId(null); }
  };

  const usageFor = (name: string) => (usage ?? []).find((u) => u.k === name);
  const budgetUsageFor = (name: string, window: string) =>
    (usageWindows[window] ?? []).find((u) => u.k === name);
  const sampleModel = models[0]?.id;

  return (
    <div className="space-y-8">
      <p className="text-[13px] text-dim">
        Point Hermes, Claude Code or any OpenAI/Anthropic-style client at this gateway. One port, two protocols.
      </p>

      {err && (
        <div className="rounded-lg border border-[rgba(248,113,113,0.3)] bg-bad-dim px-4 py-3 text-[13px] text-bad">
          {err}
        </div>
      )}

      {/* ── Routes ─────────────────────────────────────────────────── */}
      <section>
        {/* [&>*]:min-w-0 is load-bearing, not decoration: grid items default to
            min-width:auto, so the unbreakable endpoint URLs inside the cards set
            the track's min-content width and blow the page out sideways on a
            390px phone (measured: 570px scrollWidth before this class). */}
        <Stagger className="mt-3 grid grid-cols-1 gap-3 md:grid-cols-2 [&>*]:min-w-0">
          {SURFACES.map((s) => (
            <StaggerItem key={s.key} className="rounded-xl border border-line bg-surface px-4 py-4">
              <div className="text-[13px] font-semibold">{s.label}</div>
              <div className="mt-3 space-y-2">
                <FieldRow label="Base URL" value={base + s.path} />
              </div>
              {/* The paste-ready snippet lives with the protocol it belongs to,
                  not in a separate section further down: pick your protocol,
                  copy that box, done. */}
              <pre className="mt-3 overflow-x-auto rounded-lg border border-line bg-bg px-3 py-2 font-mono text-[12px] leading-relaxed text-dim">
{snippet(s.key, base, '<your-key>', sampleModel ?? '<model-id>')}
              </pre>
              <div className="mt-2 flex justify-end">
                <CopyButton text={snippet(s.key, base, '<your-key>', sampleModel ?? '<model-id>')} label="Copy env" />
              </div>
            </StaggerItem>
          ))}
        </Stagger>
        <p className="mt-2 text-[12px] text-mute">
          Models list: <code className="font-mono text-dim">{base}/v1/models</code>
          {CORS_NOTE}
          {sampleModel && <> · {models.length} routable now, first is <code className="font-mono text-dim">{sampleModel}</code></>}
        </p>
      </section>

      {/* ── Create a key ───────────────────────────────────────────── */}
      <section>
        <SectionLabel>Create an API key</SectionLabel>
        <div className="mt-3 rounded-xl border border-line bg-surface px-4 py-4">
          <div className="flex flex-wrap items-end gap-3">
            <div className="min-w-[220px] flex-1">
              <label className={labelCls} htmlFor="tk-name">Name</label>
              <input
                id="tk-name" className={inputCls} value={name} placeholder="hermes-laptop"
                onChange={(e) => setName(e.target.value)}
                onKeyDown={(e) => { if (e.key === 'Enter') void doCreate(); }}
              />
            </div>
            <div>
              <span className={labelCls}>Role</span>
              <div className="flex rounded-lg border border-line bg-raised p-0.5">
                {([
                  ['infer', 'Inference only'],
                  ['admin', 'Full admin'],
                ] as const).map(([r, label]) => (
                  <button
                    key={r} type="button"
                    onClick={() => setRole(r)}
                    title={r === 'admin'
                      ? 'Can add providers, read provider keys and mint other keys'
                      : 'Can call models only'}
                    className={cx(
                      'rounded-md px-3 py-1.5 text-[13px] font-medium transition-colors',
                      role === r ? 'bg-accent-dim text-accent' : 'text-mute hover:text-dim',
                    )}
                  >{label}</button>
                ))}
              </div>
            </div>
            <div className="min-w-[180px]">
              <label className={labelCls} htmlFor="tk-models">Allowed models <span className="text-mute">(optional)</span></label>
              <input id="tk-models" className={cx(inputCls, 'font-mono text-[13px]')} placeholder="deepseek/*"
                value={newModels} onChange={(e) => setNewModels(e.target.value)} />
            </div>
            <div>
              <label className={labelCls} htmlFor="tk-expiry">Expires <span className="text-mute">(optional)</span></label>
              <input id="tk-expiry" type="date" className={inputCls} value={newExpiry}
                onChange={(e) => setNewExpiry(e.target.value)} />
            </div>
            <button
              type="button" disabled={!name.trim() || creating}
              className={cx(btn.base, btn.primary)}
              onClick={() => void doCreate()}
            >
              {creating ? <Spinner /> : null}{creating ? 'Creating' : 'Create key'}
            </button>
          </div>
          <p className="mt-3 text-[12px] text-mute">
            <span className="text-dim">infer</span> can call /v1 only. <span className="text-dim">admin</span> can also call /admin — give that role to yourself, not to tools.
          </p>
        </div>
      </section>

      {/* ── Existing keys ─────────────────────────────────────────── */}
      <section>
        <SectionLabel>Your keys</SectionLabel>
        {tokens === null ? (
          <div className="mt-3 space-y-2">{[0, 1].map((i) => <div key={i} className="h-14 animate-pulse rounded-xl border border-line bg-surface" />)}</div>
        ) : tokens.length === 0 ? (
          <p className="mt-3 text-[13px] text-mute">No keys yet — create one above.</p>
        ) : (
          <div className="mt-3 space-y-2">
            <Stagger className="space-y-2">
              {tokens.map((t) => {
                const u = usageFor(t.name);
                const bu = budgetUsageFor(t.name, t.cap_window || 'monthly');
                const used = (bu?.tin ?? 0) + (bu?.tout ?? 0);
                const capPct = t.cap_tokens > 0 ? Math.min(100, (used / t.cap_tokens) * 100) : 0;
                return (
                  <StaggerItem key={t.id}>
                    <div className="flex flex-wrap items-center gap-x-4 gap-y-2 rounded-xl border border-line bg-surface px-4 py-3">
                      <div className="min-w-0">
                        <div className="flex items-center gap-2">
                          <span className="truncate text-[13px] font-semibold">{t.name}</span>
                          <Chip tone={t.roles.includes('admin') ? 'accent' : 'neutral'}>{t.roles.join(' · ')}</Chip>
                        </div>
                        <div className="mt-1 flex items-center gap-3 text-[12px] text-mute">
                          <Mono>{t.hint}</Mono>
                          <span>used {t.last_used_at ? new Date(t.last_used_at).toLocaleString() : 'never'}</span>
                        </div>
                        {(t.expires_at || t.allowed_models) && (
                          <div className="mt-0.5 flex flex-wrap items-center gap-2 text-[11px] text-mute">
                            {t.expires_at && <span>expires {t.expires_at.slice(0, 10)}</span>}
                            {t.allowed_models && <span className="truncate" title={t.allowed_models}>models: {t.allowed_models}</span>}
                          </div>
                        )}
                      </div>
                      <div className="ml-auto flex flex-wrap items-center gap-x-4 gap-y-2 text-[12px] text-dim">
                        <span>{(u?.calls ?? 0).toLocaleString()} calls · {(u?.tin ?? 0).toLocaleString()} in · {(u?.tout ?? 0).toLocaleString()} out</span>
                        <span className="min-w-[120px]">
                          {t.cap_tokens > 0 ? (
                            <>
                              <span className="flex items-center justify-between gap-2">
                                <span className="text-mute">budget · {t.cap_window || 'monthly'}</span>
                                <span className="font-mono text-[11px]">{used.toLocaleString()} / {t.cap_tokens.toLocaleString()}</span>
                              </span>
                              {/* The budget meter FILLS: a key that has burned
                                  a tenth of its cap should read as a tenth
                                  full the moment it loads, not after a
                                  refresh. The colour changes with the fill,
                                  so a key near its cap is obvious at a
                                  glance without reading the numbers. */}
                              <span className="mt-1 block h-1 overflow-hidden rounded-full bg-shade">
                                <motion.span
                                  className={cx('block h-full rounded-full', capPct >= 90 ? 'bg-bad' : capPct >= 70 ? 'bg-warn' : 'bg-accent')}
                                  initial={{ width: 0 }}
                                  animate={{ width: `${capPct}%` }}
                                  transition={{ duration: 0.6, ease: [0.22, 1, 0.36, 1] }}
                                />
                              </span>
                            </>
                          ) : <span className="text-mute">budget unlimited</span>}
                        </span>
                        <span className="flex shrink-0 items-center gap-2">
                          <button type="button" className={cx(btn.base, btn.ghost, 'px-2.5 py-1.5 text-[12px]')}
                            onClick={() => {
                              setBudgetTarget(t);
                              setBudgetCap(t.cap_tokens);
                              setBudgetWindow(t.cap_window || 'monthly');
                              setBudgetModels(t.allowed_models || '');
                              setBudgetExpiry(t.expires_at ? t.expires_at.slice(0, 10) : '');
                            }}>
                            Policy
                          </button>
                          <Toggle checked={t.enabled} onChange={(v) => void toggle(t, v)} disabled={busyId === t.id} />
                          <button type="button" className={cx(btn.base, btn.danger, 'px-2.5 py-1.5 text-[12px]')} onClick={() => setPendingRevoke(t)} disabled={busyId === t.id}>Revoke</button>
                        </span>
                      </div>
                    </div>
                  </StaggerItem>
                );
              })}
            </Stagger>
          </div>
        )}
      </section>

      {/* ── Budget editor ─────────────────────────────────────────── */}
      <Modal open={budgetTarget != null} onClose={() => setBudgetTarget(null)}
        title={`Key policy — ${budgetTarget?.name ?? ''}`}
        subtitle="Budget, model allowlist, and expiry for this key"
        footer={<>
          <button type="button" className={cx(btn.base, btn.ghost)} onClick={() => setBudgetTarget(null)}>Cancel</button>
          <button type="button" disabled={budgetBusy} className={cx(btn.base, btn.primary)} onClick={() => void saveBudget()}>
            {budgetBusy ? <Spinner /> : null} Save
          </button>
        </>}>
        <div className="space-y-4">
          <div className="grid gap-4 sm:grid-cols-2">
            <div>
              <label className={labelCls}>Token cap (0 = unlimited)</label>
              <input type="number" min={0} className={cx(inputCls, 'font-mono tabular-nums')}
                value={budgetCap} onChange={(e) => setBudgetCap(Number(e.target.value) || 0)} />
            </div>
            <div>
              <label className={labelCls}>Window</label>
              <select className={inputCls} value={budgetWindow} onChange={(e) => setBudgetWindow(e.target.value)}>
                {['5h', 'daily', 'weekly', 'monthly'].map((w) => <option key={w} value={w}>{w}</option>)}
              </select>
            </div>
          </div>
          <div>
            <label className={labelCls}>Allowed models <span className="text-mute">(empty = all)</span></label>
            <input className={cx(inputCls, 'font-mono')} placeholder="deepseek/*, openai/gpt-5*"
              value={budgetModels} onChange={(e) => setBudgetModels(e.target.value)} />
            <p className="mt-1 text-[12px] text-mute">Comma-separated ids or globs. Checked before a combo is expanded.</p>
          </div>
          <div>
            <label className={labelCls}>Expires <span className="text-mute">(empty = never)</span></label>
            <input type="date" className={inputCls} value={budgetExpiry}
              onChange={(e) => setBudgetExpiry(e.target.value)} />
          </div>
          <p className="text-[12px] text-mute">
            Usage is the key's actual ledger spend (tokens in + out) inside the window. A spent key answers 429 until the window rolls; max_tokens is reserved before dispatch.
          </p>
        </div>
      </Modal>

      {/* ── Show-once plaintext ────────────────────────────────────── */}
      {fresh && (
        // Deliberately NOT <Modal>: backdrop clicks and Escape would dismiss a
        // one-time secret. The only way out is the button below.
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-overlay px-4">
          {/* The panel SCALES in slightly rather than fading: a one-time
              secret is the one moment in this app where the operator's
              attention should be pulled to the middle of the screen. */}
          <motion.div
            role="dialog"
            aria-modal="true"
            aria-label="New API key"
            initial={{ opacity: 0, scale: 0.96, y: 8 }}
            animate={{ opacity: 1, scale: 1, y: 0 }}
            transition={{ duration: 0.26, ease: [0.22, 1, 0.36, 1] }}
            className="w-full max-w-lg rounded-2xl border border-accent/40 bg-surface p-6 shadow-2xl">
            <div className="text-[16px] font-semibold">Copy this key now</div>
            <p className="mt-1.5 text-[13px] text-dim">
              ezllm stores only a hash, so this is the <span className="text-ink">only time</span> it can be shown. If you close this without saving it, create a new key.
            </p>
            <div className="mt-4 flex items-center gap-2 rounded-lg border border-line bg-bg px-3 py-2.5">
              <code className="min-w-0 flex-1 break-all font-mono text-[13px] text-accent">{fresh.plaintext}</code>
              <CopyButton text={fresh.plaintext} label="Copy" />
            </div>
            <div className="mt-3 space-y-2">
              <FieldRow label="Base URL" value={base} />
              <FieldRow label="Role" value={fresh.roles.join(' · ')} mono={false} />
              {fresh.roles.includes('admin') && (
                <p className="rounded-lg border border-warn/30 bg-warn-dim p-2.5 text-[13px] leading-relaxed text-warn">
                  This key has admin role — it can add providers, read provider keys and mint other keys. For an
                  app that only needs models, create an <span className="text-ink">infer</span> key instead.
                </p>
              )}
            </div>
            <div className="mt-5 flex items-center justify-between gap-3">
              <CopyButton text={`${fresh.name} ${fresh.plaintext}`} label="Copy name + key" />
              <button type="button" className={cx(btn.base, btn.primary)} onClick={() => setFresh(null)}>
                I saved it
              </button>
            </div>
          </motion.div>
        </div>
      )}
      <ConfirmModal
        open={pendingRevoke != null}
        title={pendingRevoke ? `Revoke "${pendingRevoke.name}"?` : 'Revoke token?'}
        body="Clients using this token will fail immediately. It cannot be un-revoked."
        confirmLabel="Revoke"
        busy={pendingRevoke != null && busyId === pendingRevoke.id}
        onClose={() => setPendingRevoke(null)}
        onConfirm={() => { if (pendingRevoke) void revoke(pendingRevoke).then(() => setPendingRevoke(null)); }} />
    </div>
  );
}
