// "What else ezllm does": the extras, one line each, once Get started is done.
// It stays until someone closes it.
import { useEffect, useState } from 'react';
import { Link } from 'react-router-dom';
import { get, type Account } from '../utils/api';
import { listTokens } from '../utils/tokens';
import { startProgress } from '../utils/start';
import { EXTRAS, useExtras } from '../utils/extras';

export const MORE_HIDDEN = 'ezllm.more.hidden';

export function MoreCard() {
  const [ready, setReady] = useState(false);
  const [hidden, setHidden] = useState(() => { try { return localStorage.getItem(MORE_HIDDEN) === '1'; } catch { return false; } });
  const states = useExtras();
  useEffect(() => {
    if (hidden) return;
    let live = true;
    Promise.all([get<Account[]>('/admin/accounts'), listTokens()])
      .then(([a, t]) => { if (live) setReady(startProgress(a, t).done === 3); })
      .catch(() => { /* optional: stay quiet */ });
    return () => { live = false; };
  }, [hidden]);
  if (hidden || !ready) return null;
  const hide = () => { try { localStorage.setItem(MORE_HIDDEN, '1'); } catch { /* private mode */ } setHidden(true); };
  return (
    <section aria-label="What else ezllm does" className="rounded-xl border border-line bg-surface px-5 py-4">
      <div className="flex items-center justify-between gap-3">
        <h2 className="text-[13px] font-semibold text-ink">What else ezllm does</h2>
        <button type="button" onClick={hide} className="text-mute hover:text-ink" aria-label="Hide what else ezllm does">✕</button>
      </div>
      <ul className="mt-2 divide-y divide-line/60">
        {EXTRAS.map((x) => {
          const state = x.tool ? null : states?.[x.key];
          return (
            <li key={x.key}>
              <Link to={x.to} className="group flex items-baseline gap-3 py-2 text-[13px]">
                <span className="w-32 shrink-0 font-medium text-ink group-hover:underline">{x.label}</span>
                <span className="min-w-0 flex-1 text-dim">{x.line}</span>
                {state != null && <span className={state ? 'shrink-0 text-[12px] text-ok' : 'shrink-0 text-[12px] text-mute'}>{state ? 'in use' : 'not set up'}</span>}
              </Link>
            </li>
          );
        })}
      </ul>
    </section>
  );
}
