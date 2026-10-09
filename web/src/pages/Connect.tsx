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
import { ApiError, type UsageRow } from '../api';
import { Chip, ConfirmModal, Mono, SectionLabel, Spinner, Toggle, btn, cx, inputCls, labelCls } from '../ui';
import {
  createToken, listTokens, patchToken, probeModels, revokeToken, usageByClient,
  type TokenCreated, type TokenInfo, type V1Model,
} from '../tokens';

// The gateway speaks two client protocols on one port under /v1: OpenAI Chat
// and Anthropic Messages. The OpenAI Responses route still exists server-side
// for other clients but is not surfaced here.
const SURFACES = [
  { key: 'openai', label: 'OpenAI Chat', path: '/v1/chat/completions', note: 'the one almost everything speaks' },
  { key: 'anthropic', label: 'Anthropic Messages', path: '/v1/messages', note: 'Claude Code, SDKs, Anthropic-style clients' },
] as const;

type SurfaceKey = (typeof SURFACES)[number]['key'];

// Browser clients send a preflight. ezllm answers it for /v1/* out of the box
// (unset = any Origin echoed back, /admin never granted), so the honest note is
// how to RESTRICT it — telling users to "set the env var to make it work" would
// be backwards and would send someone chasing a fix for a non-problem.
const CORS_NOTE = (
  <>
    {' '}
    · browser preflights are allowed on <code className="font-mono text-dim">/v1/*</code> out of
    the box; set <code className="font-mono text-dim">EZLLM_CORS_ORIGINS</code> to a
    comma-separated allowlist to restrict it (<code className="font-mono text-dim">/admin</code> is never granted)
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
      className={cx(btn.base, btn.ghost, 'px-2.5 py-1.5 text-[11.5px]')}
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
      <span className="w-24 shrink-0 text-[11px] uppercase tracking-[0.08em] text-mute">{label}</span>
      <code className={cx('min-w-0 flex-1 truncate rounded-md border border-line bg-raised px-2 py-1 text-[12px]', mono && 'font-mono')}>{value}</code>
      <CopyButton text={value} />
    </div>
  );
}

function snippet(surface: SurfaceKey, base: string, key: string, model: string): string {
  if (surface === 'anthropic') {
    return [
      'ANTHROPIC_BASE_URL=' + base,
      'ANTHROPIC_API_KEY=' + key,
      'ANTHROPIC_MODEL=' + model,
    ].join('\n');
  }
  return [
    'OPENAI_BASE_URL=' + base,
    'OPENAI_API_KEY=' + key,
    'OPENAI_MODEL=' + model,
  ].join('\n');
}

export default function Connect() {
  const [tokens, setTokens] = useState<TokenInfo[] | null>(null);
  const [usage, setUsage] = useState<UsageRow[] | null>(null);
  const [models, setModels] = useState<V1Model[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const [fresh, setFresh] = useState<TokenCreated | null>(null);
  const [name, setName] = useState('');
  const [role, setRole] = useState<'infer' | 'admin'>('infer');
  const [busyId, setBusyId] = useState<number | null>(null);
  const [pendingRevoke, setPendingRevoke] = useState<TokenInfo | null>(null);

  // Base URL: same origin as the dashboard, minus the trailing slash.
  const base = useMemo(() => window.location.origin, []);
  const reload = useCallback(async () => {
    setErr(null);
    try {
      const [tk, us] = await Promise.all([listTokens(), usageByClient(30).catch(() => [] as UsageRow[])]);
      setTokens(tk);
      setUsage(us);
      // Probe the live route with the admin token (a valid bearer for /v1 too),
      // so the "supported" chips below are observations, not labels.
      try {
        const admin = localStorage.getItem('ezllm.admin.token') || '';
        const ms = await probeModels(admin);
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
      const t = await createToken(name.trim(), role);
      setFresh(t);
      setName('');
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

  const revoke = async (t: TokenInfo) => {
    setBusyId(t.id);
    try { await revokeToken(t.id); await reload(); }
    finally { setBusyId(null); }
  };

  const usageFor = (name: string) => (usage ?? []).find((u) => u.k === name);
  const sampleModel = models[0]?.id;

  return (
    <div className="space-y-8">
      <p className="text-[13px] text-dim">
        Point Hermes, Claude Code or any OpenAI/Anthropic-style client at this gateway. One port, two protocols.
      </p>

      {err && (
        <div className="rounded-lg border border-[rgba(248,113,113,0.3)] bg-[rgba(248,113,113,0.06)] px-4 py-3 text-[13px] text-bad">
          {err}
        </div>
      )}

      {/* ── Routes ─────────────────────────────────────────────────── */}
      <section>
        {/* [&>*]:min-w-0 is load-bearing, not decoration: grid items default to
            min-width:auto, so the unbreakable endpoint URLs inside the cards set
            the track's min-content width and blow the page out sideways on a
            390px phone (measured: 570px scrollWidth before this class). */}
        <div className="mt-3 grid grid-cols-1 gap-3 md:grid-cols-2 [&>*]:min-w-0">
          {SURFACES.map((s) => (
            <div key={s.key} className="rounded-xl border border-line bg-surface px-4 py-4 animate-fade-up">
              <div className="flex items-start justify-between gap-2">
                <div>
                  <div className="text-[13.5px] font-semibold">{s.label}</div>
                  <div className="mt-0.5 text-[11.5px] text-mute">{s.note}</div>
                </div>
              </div>
              <div className="mt-3 space-y-2">
                <FieldRow label="endpoint" value={base + s.path} />
              </div>
            </div>
          ))}
        </div>
        <p className="mt-2 text-[11.5px] text-mute">
          Models list: <code className="font-mono text-dim">{base}/v1/models</code>
          {CORS_NOTE}
          {sampleModel ? <> · {models.length} routable now, first is <code className="font-mono text-dim">{sampleModel}</code></> : <> · none routable yet</>}
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
                      'rounded-md px-3 py-1.5 text-[12px] font-medium transition-colors',
                      role === r ? 'bg-accent-dim text-accent' : 'text-mute hover:text-dim',
                    )}
                  >{label}</button>
                ))}
              </div>
            </div>
            <button
              type="button" disabled={!name.trim() || creating}
              className={cx(btn.base, btn.primary)}
              onClick={() => void doCreate()}
            >
              {creating ? <Spinner /> : null}{creating ? 'Creating' : 'Create key'}
            </button>
          </div>
          <p className="mt-3 text-[11.5px] text-mute">
            <span className="text-dim">infer</span> can call /v1 only. <span className="text-dim">admin</span> can also call /admin — give that role to yourself, not to tools.
          </p>
        </div>
      </section>

      {/* ── Snippet for the tool you're wiring up ─────────────────── */}
      <section>
        <SectionLabel>Paste into your client</SectionLabel>
        <div className="mt-3 space-y-2">
          {SURFACES.map((s) => (
            <div key={s.key} className="rounded-xl border border-line bg-surface px-4 py-3">
              <div className="flex items-center justify-between gap-2">
                <span className="text-[12.5px] font-medium text-dim">{s.label}</span>
                <CopyButton text={snippet(s.key, base, '<your-key>', sampleModel ?? '<model-id>')} label="Copy env" />
              </div>
              <pre className="mt-2 overflow-x-auto rounded-lg border border-line bg-bg px-3 py-2 font-mono text-[11.5px] leading-relaxed text-dim">
{snippet(s.key, base, '<your-key>', sampleModel ?? '<model-id>')}
              </pre>
            </div>
          ))}
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
            {tokens.map((t) => {
              const u = usageFor(t.name);
              return (
                <div key={t.id} className="flex flex-wrap items-center gap-x-4 gap-y-2 rounded-xl border border-line bg-surface px-4 py-3 animate-fade-up">
                  <div className="min-w-0">
                    <div className="flex items-center gap-2">
                      <span className="truncate text-[13.5px] font-semibold">{t.name}</span>
                      <Chip tone={t.roles.includes('admin') ? 'accent' : 'neutral'}>{t.roles.join(' · ')}</Chip>
                    </div>
                    <div className="mt-1 flex items-center gap-3 text-[11.5px] text-mute">
                      <Mono>{t.hint}</Mono>
                      <span>used {t.last_used_at ? new Date(t.last_used_at).toLocaleString() : 'never'}</span>
                    </div>
                  </div>
                  <div className="ml-auto flex items-center gap-4 text-[11.5px] text-dim">
                    <span>{(u?.calls ?? 0).toLocaleString()} calls · {(u?.tin ?? 0).toLocaleString()} in · {(u?.tout ?? 0).toLocaleString()} out</span>
                    <span className="flex items-center gap-2">
                      <Toggle checked={t.enabled} onChange={(v) => void toggle(t, v)} disabled={busyId === t.id} />
                      <button type="button" className={cx(btn.base, btn.danger, 'px-2.5 py-1.5 text-[11.5px]')} onClick={() => setPendingRevoke(t)} disabled={busyId === t.id}>Revoke</button>
                    </span>
                  </div>
                </div>
              );
            })}
          </div>
        )}
      </section>

      {/* ── Show-once plaintext ────────────────────────────────────── */}
      {fresh && (
        // Deliberately NOT <Modal>: backdrop clicks and Escape would dismiss a
        // one-time secret. The only way out is the button below.
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-[rgba(0,0,0,0.72)] px-4">
          <div
            role="dialog"
            aria-modal="true"
            aria-label="New API key"
            className="w-full max-w-lg rounded-2xl border border-accent/40 bg-surface p-6 shadow-2xl animate-fade-up">
            <div className="text-[16px] font-semibold">Copy this key now</div>
            <p className="mt-1.5 text-[12.5px] text-dim">
              ezllm stores only a hash, so this is the <span className="text-ink">only time</span> it can be shown. If you close this without saving it, create a new key.
            </p>
            <div className="mt-4 flex items-center gap-2 rounded-lg border border-line bg-bg px-3 py-2.5">
              <code className="min-w-0 flex-1 break-all font-mono text-[12.5px] text-accent">{fresh.plaintext}</code>
              <CopyButton text={fresh.plaintext} label="Copy" />
            </div>
            <div className="mt-3 space-y-2">
              <FieldRow label="base" value={base} />
              <FieldRow label="role" value={fresh.roles.join(' · ')} mono={false} />
              {fresh.roles.includes('admin') && (
                <p className="rounded-lg border border-amber-500/30 bg-amber-500/10 p-2.5 text-[12px] leading-relaxed text-amber-200">
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
          </div>
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
