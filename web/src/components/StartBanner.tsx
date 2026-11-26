// The Dashboard's one-line nudge while Get started is unfinished. It goes away
// on its own once all three steps are done, or for good when hidden.
import { useEffect, useState } from 'react';
import { Link } from 'react-router-dom';
import { get, type Account } from '../utils/api';
import { listTokens } from '../utils/tokens';
import { startProgress } from '../utils/start';

const HIDDEN = 'ezllm.start.hidden';

export function StartBanner() {
  const [done, setDone] = useState<number | null>(null);
  const [hidden, setHidden] = useState(() => { try { return localStorage.getItem(HIDDEN) === '1'; } catch { return false; } });
  useEffect(() => {
    if (hidden) return;
    let live = true;
    Promise.all([get<Account[]>('/admin/accounts'), listTokens()])
      .then(([a, t]) => { if (live) setDone(startProgress(a, t).done); })
      .catch(() => { /* a nudge, not a requirement: stay quiet */ });
    return () => { live = false; };
  }, [hidden]);
  if (hidden || done === null || done === 3) return null;
  const hide = () => { try { localStorage.setItem(HIDDEN, '1'); } catch { /* private mode */ } setHidden(true); };
  return (
    <div className="flex items-center justify-between gap-3 rounded-xl border border-line bg-surface px-5 py-3 text-[13px]">
      <span><span className="font-medium">Finish setting up</span> <span className="text-mute">· {done} of 3 done</span></span>
      <span className="flex items-center gap-4">
        <Link to="/start" className="font-medium underline underline-offset-2">Continue</Link>
        <button type="button" onClick={hide} className="text-mute hover:text-ink" aria-label="Hide">✕</button>
      </span>
    </div>
  );
}
