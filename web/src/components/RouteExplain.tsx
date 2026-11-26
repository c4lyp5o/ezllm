// Route explain: a dry run of one routing decision. Nothing is sent to a
// provider; the server reports, hop by hop, what a real request would do.
import { useEffect, useState } from 'react';
import { post } from '../utils/api';
import { listTokens, type TokenInfo } from '../utils/tokens';
import { parkedFor, secondsLeft } from '../utils/cooldowns';
import { Chip, Modal, Segmented, Spinner, btn, cx, inputCls, labelCls } from './ui';

export type ExplainHop = {
  account_id: number; namespace: string; model: string;
  status: 'candidate' | 'skipped' | 'refused'; reason?: string;
  wire?: string; translated?: boolean;
  reasoning_allowed?: string[]; reasoning_required?: boolean;
  max_context_tokens?: number; kind?: string;
  parked_until?: string; park_reason?: string;
};
export type Explanation = {
  requested: string; kind: 'combo' | 'direct'; strategy?: string; surface: string;
  hops: ExplainHop[]; usable: number; error?: string; order_note?: string;
  key?: { name: string; error?: string; model_allowed?: boolean; budget_spent?: boolean; spend_cap_reached?: boolean };
};

type Surface = 'chat' | 'anthropic' | 'responses';
const SURFACES: { key: Surface; label: string; title: string }[] = [
  { key: 'chat', label: 'Chat', title: '/v1/chat/completions' },
  { key: 'anthropic', label: 'Anthropic', title: '/v1/messages' },
  { key: 'responses', label: 'Responses', title: '/v1/responses' },
];

const TONE = { candidate: 'ok', skipped: 'warn', refused: 'bad' } as const;
const WORD = { candidate: 'would be tried', skipped: 'skipped', refused: 'fails the request' } as const;

/** What the key itself says before any hop is tried. */
export function keyVerdict(k: NonNullable<Explanation['key']>): string | null {
  if (k.error) return k.error;
  if (k.model_allowed === false) return `Key ${k.name} may not call this model (its allowlist).`;
  if (k.budget_spent) return `Key ${k.name} has used its token budget for this window.`;
  if (k.spend_cap_reached) return `Key ${k.name} has reached its spend cap for this window.`;
  return null;
}

export function RouteExplainModal({ model, onClose }: { model: string; onClose: () => void }) {
  const [surface, setSurface] = useState<Surface>('chat');
  const [client, setClient] = useState('');
  const [est, setEst] = useState('');
  const [tools, setTools] = useState(false);
  const [tokens, setTokens] = useState<TokenInfo[]>([]);
  const [res, setRes] = useState<Explanation | null>(null);
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);

  useEffect(() => { listTokens().then(setTokens).catch(() => setTokens([])); }, []);

  const run = async () => {
    setBusy(true); setErr('');
    try {
      setRes(await post<Explanation>('/admin/route-explain', {
        model, surface, client: client || undefined, est_tokens: Number(est) || 0, has_tools: tools,
      }));
    } catch (e) { setErr(e instanceof Error ? e.message : 'could not explain the route'); }
    finally { setBusy(false); }
  };
  useEffect(() => { void run(); }, [surface]); // eslint-disable-line react-hooks/exhaustive-deps

  const blocked = res?.key ? keyVerdict(res.key) : null;
  return (
    <Modal open onClose={onClose} wide title={`Explain route: ${model}`}
      subtitle="What a request would do right now. Nothing is sent to a provider."
      footer={(close) => <>
        <button type="button" className={cx(btn.base, btn.ghost)} onClick={close}>Close</button>
        <button type="button" className={cx(btn.base, btn.primary)} disabled={busy} onClick={() => void run()}>{busy ? <Spinner /> : null} Explain</button>
      </>}>
      <div className="space-y-4">
        <div className="flex flex-wrap items-end gap-4">
          <div>
            <span className={labelCls}>API</span>
            <div className="mt-1"><Segmented label="API the request uses" options={SURFACES} value={surface} onChange={setSurface} size="sm" /></div>
          </div>
          <div>
            <label className={labelCls} htmlFor="rx-client">Key</label>
            <select id="rx-client" className={cx(inputCls, 'min-w-[10rem]')} value={client} onChange={(e) => setClient(e.target.value)}>
              <option value="">Any key</option>
              {tokens.map((t) => <option key={t.id} value={t.name}>{t.name}</option>)}
            </select>
          </div>
          <div>
            <label className={labelCls} htmlFor="rx-est">Request size (tokens)</label>
            <input id="rx-est" inputMode="numeric" className={cx(inputCls, 'w-32 font-mono')} placeholder="optional" value={est} onChange={(e) => setEst(e.target.value.replace(/[^0-9]/g, ''))} />
          </div>
          <label className="flex items-center gap-2 pb-2 text-[13px] text-dim">
            <input type="checkbox" checked={tools} onChange={(e) => setTools(e.target.checked)} /> Sends tools
          </label>
        </div>
        {err && <div role="alert" className="rounded-lg border border-bad/30 bg-bad-dim px-4 py-3 text-[13px] text-bad">{err}</div>}
        {res && (
          <div className="space-y-3">
            <p className="text-[13px]">
              {blocked
                ? <span className="text-bad">{blocked}</span>
                : res.usable > 0
                  ? <span className="text-ok">{res.usable === 1 ? '1 hop would be tried.' : `${res.usable} hops would be tried.`}</span>
                  : <span className="text-bad">No hop can serve this request{res.error ? `: ${res.error}` : '.'}</span>}
              {res.order_note && <span className="ml-1 text-mute">{res.order_note}.</span>}
            </p>
            <ol className="divide-y divide-line rounded-lg border border-line">
              {res.hops.map((h, i) => (
                <li key={i} className="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2 text-[13px]">
                  <span className="w-5 text-right font-mono text-mute">{i + 1}</span>
                  <span className="font-mono">{h.namespace}<span className="text-mute">/</span>{h.model}</span>
                  <Chip tone={TONE[h.status]}>{WORD[h.status]}</Chip>
                  {h.wire && <span className="text-[12px] text-mute">{h.translated ? `translated to ${h.wire}` : `native ${h.wire}`}</span>}
                  {h.parked_until && <span className="text-[12px] text-warn">parked · {parkedFor(secondsLeft({ until: h.parked_until }))}</span>}
                  {h.reasoning_allowed && h.reasoning_allowed.length > 0 && <span className="text-[12px] text-mute" title="what this model accepts for reasoning_effort">reasoning: {h.reasoning_allowed.join(', ')}</span>}
                  {h.reason && h.status !== 'candidate' && <span className="basis-full pl-8 text-[12px] text-dim">{h.reason}</span>}
                </li>
              ))}
              {res.hops.length === 0 && <li className="px-3 py-2 text-[13px] text-mute">No hops.</li>}
            </ol>
          </div>
        )}
      </div>
    </Modal>
  );
}
