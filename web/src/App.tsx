// App shell: token unlock gate → sidebar layout → routes.
// Nothing renders or fetches before the unlock screen hands back a token.
import { useCallback, useEffect, useState } from 'react';
import { NavLink, Route, Routes, useLocation } from 'react-router-dom';
import { clearToken, dashboardLogin, getToken, setToken } from './api';
import { Spinner, cx } from './ui';
import Dashboard from './pages/Dashboard';
import Providers from './pages/Providers';
import Combos from './pages/Combos';
import Compression from './pages/Compression';
import Connect from './pages/Connect';
import Rules from './pages/Rules';
import Stats from './pages/Stats';
import Economics from './pages/Economics';
import Requests from './pages/Requests';
import Settings from './pages/Settings';
import Chat from './pages/Chat';

const NAV = [
  { to: '/', label: 'Dashboard', hint: 'overview · health · ledger' },
  { to: '/stats', label: 'Stats', hint: 'granularity · range' },
  { to: '/economics', label: 'Token Economics', hint: 'savings · cache · outliers' },
  { to: '/requests', label: 'Requests', hint: 'filter · tokens' },
  { to: '/providers', label: 'Providers', hint: 'accounts · keys · models' },
  { to: '/chat', label: 'Chat', hint: 'test a model' },
  { to: '/combos', label: 'Combos', hint: 'routing chains' },
  { to: '/rules', label: 'Rules', hint: 'caps · windows' },
  { to: '/compression', label: 'Compression', hint: 'profiles · engines' },
  { to: '/connect', label: 'Connect', hint: 'routes · API keys' },
  { to: '/settings', label: 'Settings', hint: 'dashboard password' },
] as const;

const LOGO = (
  <svg viewBox="0 0 24 24" className="h-5 w-5 text-accent" fill="none" stroke="currentColor" strokeWidth="1.8"
    strokeLinecap="round" strokeLinejoin="round" aria-hidden>
    <path d="M4 18V6h4.5a3.5 3.5 0 0 1 0 7H4m4.5 0L13 18" />
  </svg>
);

function Unlock({ onUnlock }: { onUnlock: () => void }) {
  const [value, setValue] = useState('');
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!value) { setErr('enter the dashboard password'); return; }
    setBusy(true);
    dashboardLogin(value)
      .then((token) => { setToken(token); onUnlock(); })
      .catch((error: unknown) => setErr(error instanceof Error ? error.message : 'login failed'))
      .finally(() => setBusy(false));
  };

  return (
    <div className="flex min-h-dvh items-center justify-center bg-bg px-4">
      <div className="w-full max-w-sm rounded-xl border border-line bg-surface p-8 animate-fade-up">
        <div className="flex items-center gap-2.5">
          {LOGO}
          <span className="text-[15px] font-semibold tracking-tight">ezllm</span>
        </div>
        <h1 className="mt-6 text-lg font-semibold">Unlock dashboard</h1>
        <p className="mt-1 text-[13px] text-mute">Sign in with your dashboard password to reach the control plane.</p>
        <p className="mt-3 rounded-lg border border-line bg-raised px-3 py-2 text-xs text-mute">
          First-run password: <code className="font-mono text-ink">STRONGPASSWORD</code>
        </p>
        <form onSubmit={submit} className="mt-6">
          <label className="mb-1.5 block text-[11px] font-medium uppercase tracking-[0.08em] text-mute" htmlFor="dashboard-password">
            Dashboard password
          </label>
          <input
            id="dashboard-password" autoFocus type="password" autoComplete="current-password" spellCheck={false}
            value={value} onChange={(e) => { setValue(e.target.value); setErr(''); }}
            placeholder="Enter dashboard password"
            className={cx('w-full rounded-lg border bg-raised px-3 py-2.5 font-mono text-[13px] transition-colors',
              err ? 'border-bad' : 'border-line hover:border-line-strong focus:border-accent focus:outline-none')}
          />
          {err && <p className="mt-2 text-xs text-bad">{err}</p>}
          <button type="submit" disabled={busy}
            className="mt-4 flex w-full items-center justify-center gap-2 rounded-lg border border-accent/60 bg-accent py-2.5 text-[13px] font-semibold text-[#082a3d] transition-all hover:brightness-110 active:brightness-95 disabled:opacity-50">
            {busy ? <Spinner /> : null} Unlock dashboard
          </button>
        </form>
        <p className="mt-5 border-t border-line pt-4 text-[11px] leading-relaxed text-mute">
          Change the password any time in Settings. Dashboard sessions expire after 12 hours.
        </p>
      </div>
    </div>
  );
}

function SidebarContent({ onNavigate }: { onNavigate?: () => void }) {
  const location = useLocation();
  return (
    <div className="flex h-full flex-col">
      <div className="flex items-center gap-2.5 px-5 py-5">
        {LOGO}
        <span className="text-[15px] font-semibold tracking-tight">ezllm</span>
        <span className="ml-auto rounded-full border border-line bg-raised px-2 py-0.5 font-mono text-[10px] text-mute">M3</span>
      </div>
      <nav className="flex-1 space-y-1 px-3">
        {NAV.map((item) => {
          const active = item.to === '/' ? location.pathname === '/' : location.pathname.startsWith(item.to);
          return (
            <NavLink key={item.to} to={item.to} onClick={onNavigate} end={item.to === '/'}
              className={cx('block rounded-lg px-3 py-2 transition-colors', active ? 'bg-accent-dim text-ink' : 'text-dim hover:bg-hover hover:text-ink')}>
              <div className="text-[13px] font-medium">{item.label}</div>
              <div className={cx('text-[10.5px]', active ? 'text-accent/70' : 'text-mute')}>{item.hint}</div>
            </NavLink>
          );
        })}
      </nav>
      <div className="border-t border-line px-5 py-4">
        <button
          type="button"
          className="text-[11.5px] text-mute transition-colors hover:text-bad"
          onClick={() => { clearToken(); onNavigate?.(); window.location.reload(); }}
        >
          Lock dashboard ⇥
        </button>
      </div>
    </div>
  );
}

const PAGE_HEADERS: Record<string, { title: string; sub: string }> = {
  '/': { title: 'Dashboard', sub: 'traffic, health and the last 20 calls' },
  '/stats': { title: 'Stats', sub: 'usage by granularity and range' },
  '/economics': { title: 'Token Economics', sub: 'optimization savings and large requests' },
  '/requests': { title: 'Requests', sub: 'dissect tokens in and out' },
  '/providers': { title: 'Providers', sub: 'accounts, keys, catalogs and quota' },
  '/chat': { title: 'Chat', sub: 'test a routed provider model' },
  '/combos': { title: 'Combos', sub: 'ordered routing chains' },
  '/rules': { title: 'Rules', sub: 'usage caps and allowed-use windows' },
  '/compression': { title: 'Compression', sub: 'profiles, engines and savings' },
  '/connect': { title: 'Connect', sub: 'point clients at this gateway' },
  '/settings': { title: 'Settings', sub: 'dashboard account security' },
};

export default function App() {
  const [token, setTokenState] = useState<string | null>(() => getToken());
  const [drawer, setDrawer] = useState(false);

  const closeDrawer = useCallback(() => setDrawer(false), []);

  useEffect(() => {
    document.body.classList.toggle('overflow-hidden', drawer);
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setDrawer(false); };
    if (drawer) { document.addEventListener('keydown', onKey); return () => document.removeEventListener('keydown', onKey); }
  }, [drawer]);

  if (!token) return <Unlock onUnlock={() => setTokenState(getToken())} />;

  const header = PAGE_HEADERS[location.pathname] ?? PAGE_HEADERS['/'];

  return (
    <div className="flex min-h-dvh">
      {/* Top bar — mobile only */}
      <header className="fixed inset-x-0 top-0 z-30 flex items-center gap-3 border-b border-line bg-surface/95 px-4 py-3 backdrop-blur lg:hidden">
        <button type="button" aria-label="Open menu" onClick={() => setDrawer(true)}
          className="rounded-md border border-line p-1.5 text-dim hover:bg-hover hover:text-ink">
          <svg viewBox="0 0 20 20" className="h-4 w-4" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round">
            <path d="M3 5.5h14M3 10h14M3 14.5h14" />
          </svg>
        </button>
        {LOGO}
        <span className="text-[14px] font-semibold tracking-tight">ezllm</span>
        <span className="ml-auto text-[12px] font-medium text-dim">{header.title}</span>
      </header>

      {/* Sidebar — desktop */}
      <aside className="fixed bottom-0 left-0 top-0 z-30 hidden w-60 flex-col border-r border-line bg-surface lg:flex">
        <SidebarContent />
      </aside>

      {/* Sidebar — mobile drawer */}
      {drawer && (
        <div className="fixed inset-0 z-50 lg:hidden">
          <div className="absolute inset-0 bg-black/60 backdrop-blur-sm animate-fade-in" onClick={closeDrawer} />
          <aside className="absolute bottom-0 left-0 top-0 w-64 border-r border-line bg-surface animate-fade-in">
            <SidebarContent onNavigate={closeDrawer} />
          </aside>
        </div>
      )}

      <main className="min-w-0 flex-1 px-4 pb-16 pt-20 lg:ml-60 lg:pl-8 lg:pr-8 lg:pt-8 xl:px-10">
        <div className="hidden items-baseline gap-3 lg:flex">
          <h1 className="text-[20px] font-semibold tracking-tight">{header.title}</h1>
          <p className="text-[13px] text-mute">{header.sub}</p>
        </div>
        <div className="mt-8">
          <Routes>
            <Route path="/" element={<Dashboard />} />
      <Route path="/stats" element={<Stats />} />
      <Route path="/economics" element={<Economics />} />
      <Route path="/requests" element={<Requests />} />
            <Route path="/providers" element={<Providers />} />
            <Route path="/chat" element={<Chat />} />
            <Route path="/combos" element={<Combos />} />
            <Route path="/rules" element={<Rules />} />
            <Route path="/compression" element={<Compression />} />
            <Route path="/connect" element={<Connect />} />
            <Route path="/settings" element={<Settings />} />
            <Route path="*" element={<Dashboard />} />
          </Routes>
        </div>
      </main>
    </div>
  );
}
