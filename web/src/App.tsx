// App shell: token unlock gate → sidebar layout → routes.
// Nothing renders or fetches before the unlock screen hands back a token.
import { useCallback, useEffect, useState } from 'react';
import { NavLink, Route, Routes, useLocation } from 'react-router-dom';
import { AnimatePresence, motion } from 'framer-motion';
import { clearToken, dashboardLogin, getToken, setToken } from './api';
import { DUR, spring } from './motion';
import { readTheme, setTheme, type Theme } from './theme';
import { Spinner, cx } from './ui';
import ThemeToggle from './ThemeToggle';
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

// One sidebar entry per destination — Stats / Token Economics / Requests live
// together under Analytics (same data family, tabbed).
type NavItem = { to: string; label: string; hint: string; match?: readonly string[] };

const ANALYTICS_ROUTES = ['/stats', '/economics', '/requests'] as const;

const NAV: NavItem[] = [
  { to: '/', label: 'Dashboard', hint: 'overview · health · ledger' },
  { to: '/stats', label: 'Analytics', hint: 'stats · economics · requests', match: ANALYTICS_ROUTES },
  { to: '/providers', label: 'Providers', hint: 'accounts · keys · models' },
  { to: '/chat', label: 'Chat', hint: 'test a model' },
  { to: '/combos', label: 'Combos', hint: 'routing chains' },
  { to: '/rules', label: 'Rules', hint: 'quotas · windows' },
  { to: '/compression', label: 'Compression', hint: 'profiles · engines' },
  { to: '/connect', label: 'Connect', hint: 'routes · API keys' },
  { to: '/settings', label: 'Settings', hint: 'dashboard password' },
];

const ANALYTICS_TABS = [
  { to: '/stats', label: 'Stats' },
  { to: '/economics', label: 'Token Economics' },
  { to: '/requests', label: 'Requests' },
] as const;

const isAnalytics = (path: string) => (ANALYTICS_ROUTES as readonly string[]).some((p) => path === p || path.startsWith(`${p}/`));

const LOGO = (
  // The mark is a monogram "R" (same glyph as the favicon) — labelled here so
  // it reads as the ezllm router instead of an unexplained letter.
  <svg viewBox="0 0 24 24" className="h-5 w-5 text-accent" fill="none" stroke="currentColor" strokeWidth="1.8"
    strokeLinecap="round" strokeLinejoin="round" role="img" aria-label="ezllm router mark">
    <title>ezllm router mark</title>
    <path d="M4 18V6h4.5a3.5 3.5 0 0 1 0 7H4m4.5 0L13 18" />
  </svg>
);

// Version badge beside the wordmark. This is a PLACEHOLDER, deliberately a
// plain constant rather than an import of web/package.json's version: the
// dashboard is served from an embedded bundle inside the Go binary, so the JS
// package version and the running build are not the same thing and reading
// one to display the other would be a lie. Bump this at release; the title
// names the milestone so the chip is not the only place that knowledge lives.
const VERSION = 'v0.1.0';

// Where the source lives. Opened in a new tab with an explicit rel: this is a
// cross-origin link out of the control plane, and without rel="noreferrer"
// the destination receives the dashboard's own URL as the referrer.
const REPO_URL = 'https://github.com/c4lyp5o/ezllm';
const PROFILE_URL = 'https://github.com/c4lyp5o';

function GitHubIcon({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 16 16" className={cx('h-3.5 w-3.5', className)} fill="currentColor" aria-hidden focusable="false">
      <path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-2.91-.88-2.91-2.79 0-.55.2-1.02.51-1.38-.05-.13-.22-.65.05-1.35 0 0 .61-.19 2 .74a5.4 5.4 0 0 1 1.4-.19c.48 0 .96.06 1.41.19 1.38-.93 1.99-.74 1.99-.74.27.7.1 1.22.05 1.35.32.36.51.83.51 1.38 0 1.92-1.14 2.59-2.92 2.79.3.26.56.76.56 1.53 0 1.11-.01 2-.01 2.27 0 .21.15.46.55.38A7.99 7.99 0 0 0 16 8c0-4.42-3.58-8-8-8Z" />
    </svg>
  );
}

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
  const [theme, setThemeState] = useState<Theme>(() => readTheme());

  const changeTheme = useCallback((t: Theme) => { setThemeState(t); setTheme(t); }, []);

  return (
    <div className="flex h-full flex-col">
      <div className="flex items-center gap-2.5 px-5 py-5">
        {LOGO}
        <span className="text-[15px] font-semibold tracking-tight">ezllm</span>
        <a href={REPO_URL} target="_blank" rel="noreferrer noopener"
          title={`ezllm ${VERSION} — M1–M7 shipped, live on llm.calypso.dedyn.io`}
          className="ml-auto cursor-help rounded-full border border-line bg-raised px-2 py-0.5 font-mono text-[10px] text-mute transition-colors hover:border-line-strong hover:text-ink">
          {VERSION}
        </a>
      </div>
      <nav className="flex-1 space-y-1 px-3">
        {NAV.map((item) => {
          const active = item.match
            ? item.match.some((p) => location.pathname === p || location.pathname.startsWith(`${p}/`))
            : item.to === '/' ? location.pathname === '/' : location.pathname.startsWith(item.to);
          return (
            <NavLink key={item.to} to={item.to} onClick={onNavigate} end={item.to === '/'}
              className={cx('block rounded-lg px-3 py-2 transition-colors', active ? 'bg-accent-dim text-ink' : 'text-dim hover:bg-hover hover:text-ink')}>
              <div className="text-[13px] font-medium">{item.label}</div>
              <div className={cx('text-[10.5px]', active ? 'text-accent/70' : 'text-mute')}>{item.hint}</div>
            </NavLink>
          );
        })}
      </nav>
      <div className="space-y-1 border-t border-line px-3 py-3">
        <ThemeToggle theme={theme} onChange={changeTheme} />
        <div className="flex items-center px-3 py-1">
          <a href={PROFILE_URL} target="_blank" rel="noreferrer noopener"
            aria-label="GitHub profile (c4lyp5o)" title="github.com/c4lyp5o"
            className="flex items-center gap-2 text-[11.5px] text-mute transition-colors hover:text-ink">
            <GitHubIcon />
            <span className="font-mono">c4lyp5o</span>
          </a>
          <button
            type="button"
            className="ml-auto text-[11.5px] text-mute transition-colors hover:text-bad"
            onClick={() => { clearToken(); onNavigate?.(); window.location.reload(); }}
          >
            Log out
          </button>
        </div>
      </div>
    </div>
  );
}

const PAGE_HEADERS: Record<string, { title: string; sub: string }> = {
  '/': { title: 'Dashboard', sub: 'traffic, health and the last 20 calls' },
  '/stats': { title: 'Analytics', sub: 'usage by granularity and range' },
  '/economics': { title: 'Analytics', sub: 'optimization savings and large requests' },
  '/requests': { title: 'Analytics', sub: 'dissect tokens in and out' },
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
  const location = useLocation();

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

      {/* Sidebar — mobile drawer. AnimatePresence plays the exit, so closing
          is a slide-out rather than a snap. */}
      <AnimatePresence>
        {drawer && (
          <div className="fixed inset-0 z-50 lg:hidden">
            <motion.div
              className="absolute inset-0 bg-overlay backdrop-blur-sm"
              initial={{ opacity: 0 }} animate={{ opacity: 1 }} exit={{ opacity: 0 }}
              transition={{ duration: DUR.fast }} onClick={closeDrawer} />
            <motion.aside
              className="absolute bottom-0 left-0 top-0 w-64 border-r border-line bg-surface"
              initial={{ x: '-100%' }} animate={{ x: 0 }} exit={{ x: '-100%' }}
              transition={spring}>
              <SidebarContent onNavigate={closeDrawer} />
            </motion.aside>
          </div>
        )}
      </AnimatePresence>

      <main className="min-w-0 flex-1 px-4 pb-16 pt-20 lg:ml-60 lg:pl-8 lg:pr-8 lg:pt-8 xl:px-10">
        <div className="hidden items-baseline gap-3 lg:flex">
          <h1 className="text-[20px] font-semibold tracking-tight">{header.title}</h1>
          <p className="text-[13px] text-mute">{header.sub}</p>
        </div>
        {isAnalytics(location.pathname) && (
          <nav className="mt-5 flex gap-1 border-b border-line" aria-label="Analytics views">
            {ANALYTICS_TABS.map((tab) => (
              <NavLink key={tab.to} to={tab.to} className="relative -mb-px px-3 py-2 text-[13px] transition-colors">
                {({ isActive }) => (
                  <>
                    <span className={cx('relative z-10', isActive ? 'text-ink' : 'text-mute hover:text-ink')}>
                      {tab.label}
                    </span>
                    {/* The underline is ONE element that slides between tabs
                        (shared layoutId) instead of two borders swapping
                        visibility — the eye follows the indicator rather than
                        re-reading the row. */}
                    {isActive && (
                      <motion.span
                        layoutId="analytics-tab-underline"
                        className="absolute inset-x-0 -bottom-px h-0.5 rounded-full bg-accent"
                        transition={spring} />
                    )}
                  </>
                )}
              </NavLink>
            ))}
          </nav>
        )}
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
