import { subscribePreferences } from './utils/preferences';
// App shell: token unlock gate → sidebar layout → routes.
// Nothing renders or fetches before the unlock screen hands back a token.
import { Suspense, lazy, useCallback, useEffect, useState } from 'react';
import { NavLink, Route, Routes, useLocation } from 'react-router-dom';
import { AnimatePresence, LayoutGroup, motion } from 'motion/react';
import { GateError, completeSetup, dashboardLogin, getSetupStatus, isUnlocked, logout, markUnlocked } from './utils/api';
import { DUR, SelectionLine, spring } from './components/motion';
import RailwayBackground from './components/RailwayBackground';
import PasswordStrength from './components/PasswordStrength';
import NotificationBell from './components/NotificationBell';
import { MIN_PASSWORD } from './utils/passwordStrength';
import { readTheme, setTheme, type Theme } from './utils/theme';
import { Spinner, btn, cx } from './components/ui';
import ThemeToggle from './components/ThemeToggle';
import { PageBoundary, PageLoading } from './components/PageBoundary';

// Pages load on demand: the unlock screen downloads none of them (nor the
// chart library most of them pull in), and each page's code arrives the first
// time it is opened.
const Dashboard = lazy(() => import('./pages/Dashboard'));
const Providers = lazy(() => import('./pages/Providers'));
const Services = lazy(() => import('./pages/Services'));
const Combos = lazy(() => import('./pages/Combos'));
const Compression = lazy(() => import('./pages/Compression'));
const Connect = lazy(() => import('./pages/Connect'));
const Rules = lazy(() => import('./pages/Rules'));
const Stats = lazy(() => import('./pages/Stats'));
const Economics = lazy(() => import('./pages/Economics'));
const Requests = lazy(() => import('./pages/Requests'));
const Settings = lazy(() => import('./pages/Settings'));
const Chat = lazy(() => import('./pages/Chat'));
const Neighbourhood = lazy(() => import('./pages/Neighbourhood'));

// One sidebar entry per destination — Stats / Token Economics / Requests live
// together under Analytics (same data family, tabbed).
// A labelled sidebar, not the dealt icon rail: ten destinations need words.
// The hints are kept as each item's tooltip, not as a second line of text.
type NavItem = { to: string; label: string; hint: string; match?: readonly string[] };

const ANALYTICS_ROUTES = ['/stats', '/economics', '/requests'] as const;

const NAV: NavItem[] = [
  { to: '/', label: 'Dashboard', hint: 'overview · health · ledger' },
  { to: '/stats', label: 'Analytics', hint: 'stats · economics · requests', match: ANALYTICS_ROUTES },
  { to: '/providers', label: 'Providers', hint: 'keys · models · quota' },
  { to: '/services', label: 'Services', hint: 'embeddings · images · audio' },
  { to: '/chat', label: 'Chat', hint: 'test a model' },
  { to: '/combos', label: 'Combos', hint: 'routing chains' },
  { to: '/rules', label: 'Rules', hint: 'quotas · windows' },
  { to: '/compression', label: 'Compression', hint: 'profiles · engines' },
  { to: '/connect', label: 'Connect', hint: 'routes · API keys' },
  { to: '/neighbourhood', label: 'Neighbourhood', hint: 'your keys as a living scene' },
  { to: '/settings', label: 'Settings', hint: 'instance administration' },
];

// One restrained 16px stroke icon per destination. Inline SVG keeps the
// dashboard dependency-free and inherits currentColor, so the active/hover
// state is handled by the NavLink's text colour alone.
function NavIcon({ label }: { label: string }) {
  const common = {
    viewBox: '0 0 24 24', className: 'h-4 w-4 shrink-0', fill: 'none',
    stroke: 'currentColor', strokeWidth: 1.7, strokeLinecap: 'round' as const,
    strokeLinejoin: 'round' as const, 'aria-hidden': true,
  };
  switch (label) {
    case 'Dashboard':
      return <svg {...common}><rect x="3" y="3" width="7" height="7" rx="1.5" /><rect x="14" y="3" width="7" height="7" rx="1.5" /><rect x="3" y="14" width="7" height="7" rx="1.5" /><rect x="14" y="14" width="7" height="7" rx="1.5" /></svg>;
    case 'Analytics':
      return <svg {...common}><path d="M4 20V10M10 20V4M16 20v-7M22 20H2" /></svg>;
    case 'Providers':
      return <svg {...common}><path d="M4 7h16M4 12h16M4 17h16" /><circle cx="7" cy="7" r="1" fill="currentColor" /><circle cx="7" cy="12" r="1" fill="currentColor" /><circle cx="7" cy="17" r="1" fill="currentColor" /></svg>;
    case 'Services':
      return <svg {...common}><path d="M12 3 3 8l9 5 9-5-9-5Z" /><path d="M3 12l9 5 9-5M3 16l9 5 9-5" /></svg>;
    case 'Chat':
      return <svg {...common}><path d="M21 12a8 8 0 0 1-8 8H5l-2 2V12a8 8 0 0 1 8-8h2a8 8 0 0 1 8 8Z" /></svg>;
    case 'Combos':
      return <svg {...common}><path d="M4 7h5l2 3 2-3h7M4 17h5l2-3 2 3h7" /><circle cx="4" cy="7" r="1.5" /><circle cx="20" cy="7" r="1.5" /><circle cx="4" cy="17" r="1.5" /><circle cx="20" cy="17" r="1.5" /></svg>;
    case 'Rules':
      return <svg {...common}><path d="M12 3 4 6v5c0 5 3.5 8 8 10 4.5-2 8-5 8-10V6l-8-3Z" /><path d="m9 12 2 2 4-4" /></svg>;
    case 'Compression':
      return <svg {...common}><path d="M4 7h16M4 12h10M4 17h7" /><path d="m16 15 3 3 3-3M19 12v6" /></svg>;
    case 'Neighbourhood':
      return <svg {...common}><path d="M3 21V11l5-4 5 4v10M13 21v-7l4-3 4 3v7M2 21h20" /><path d="M7 15h2M17 17h2" /></svg>;
    case 'Connect':
      return <svg {...common}><path d="M9 7V3M15 7V3M7 7h10v4a5 5 0 0 1-10 0V7ZM12 16v5" /></svg>;
    default:
      return <svg {...common}><circle cx="12" cy="12" r="3" /><path d="M12 2v3M12 19v3M2 12h3M19 12h3M5 5l2 2M17 17l2 2M19 5l-2 2M7 17l-2 2" /></svg>;
  }
}

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

// The version beside the wordmark is the RUNNING server's, read from the
// public /healthz (the one source of truth: the Go constant it serves). A
// placeholder constant here once said v0.1.0 while the server was 0.0.19.
function useServerVersion(): string {
  const [version, setVersion] = useState('');
  useEffect(() => {
    let alive = true;
    fetch('/healthz').then((r) => r.json()).then((h: { version?: string }) => {
      if (alive && h.version) setVersion(`v${h.version}`);
    }).catch(() => undefined);
    return () => { alive = false; };
  }, []);
  return version;
}

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

const fieldClass = (bad: boolean) => cx('h-10 w-full rounded-lg border bg-bg px-3 text-[14px] transition-colors',
  bad ? 'border-bad' : 'border-line hover:border-line-strong focus:border-accent focus:outline-none');

function Field({ id, label, ...input }: { id: string; label: string } & React.InputHTMLAttributes<HTMLInputElement>) {
  return (
    <div className="mt-4 first:mt-0">
      <label className="mb-1.5 block text-[12px] font-medium text-mute" htmlFor={id}>{label}</label>
      <input id={id} spellCheck={false} {...input} />
    </div>
  );
}

// The gate before the app. While the server reports first_boot (no password
// set yet) it shows ONLY the set-password page; after that, sign-in.
function Unlock({ onUnlock }: { onUnlock: () => void }) {
  const [mode, setMode] = useState<'loading' | 'setup' | 'login'>('loading');

  useEffect(() => {
    let alive = true;
    // A failed status read falls back to sign-in: the server still refuses
    // login with setup_required while setup is pending.
    getSetupStatus()
      .then((s) => { if (alive) setMode(s.firstBoot ? 'setup' : 'login'); })
      .catch(() => { if (alive) setMode('login'); });
    return () => { alive = false; };
  }, []);

  const done = () => { markUnlocked(); onUnlock(); };

  return (
    // The railway backdrop (item 15) is the login page's own. A flat night
    // ground carries the canvas during its clear/fade so there is no white
    // flash; the card is opaque, not frosted glass.
    <div className="relative flex min-h-dvh items-center justify-center overflow-hidden px-4"
      style={{ background: '#05070d' }}>
      <RailwayBackground />
      <div className="relative w-full max-w-sm rounded-xl border border-line bg-surface p-8">
        <div className="flex items-center gap-2.5">
          {LOGO}
          <span className="text-[15px] font-semibold tracking-tight">ezllm</span>
        </div>
        {mode === 'loading' && <div className="mt-6 flex justify-center"><Spinner /></div>}
        {mode === 'setup' && <SetupForm onDone={done} onAlreadySet={() => setMode('login')} />}
        {mode === 'login' && <SignInForm onDone={done} onSetupRequired={() => setMode('setup')} />}
      </div>
    </div>
  );
}

function SignInForm({ onDone, onSetupRequired }: { onDone: () => void; onSetupRequired: () => void }) {
  const [value, setValue] = useState('');
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!value) { setErr('enter the dashboard password'); return; }
    setBusy(true);
    dashboardLogin(value)
      .then(onDone)
      .catch((error: unknown) => {
        if (error instanceof GateError && error.type === 'setup_required') { onSetupRequired(); return; }
        setErr(error instanceof Error ? error.message : 'login failed');
      })
      .finally(() => setBusy(false));
  };

  return (
    <>
      <h1 className="mt-6 text-[20px] font-semibold">Sign in to ezllm</h1>
      <form onSubmit={submit} className="mt-6">
        <Field id="dashboard-password" label="Dashboard password" autoFocus type="password"
          autoComplete="current-password" placeholder="Enter dashboard password"
          value={value} onChange={(e) => { setValue(e.target.value); setErr(''); }}
          className={fieldClass(!!err)} />
        {err && <p className="mt-2 text-xs text-bad">{err}</p>}
        <button type="submit" disabled={busy} className={cx(btn.base, btn.primary, 'mt-4 w-full')}>
          {busy ? <Spinner /> : null} Sign in
        </button>
      </form>
    </>
  );
}

function SetupForm({ onDone, onAlreadySet }: { onDone: () => void; onAlreadySet: () => void }) {
  const [code, setCode] = useState('');
  const [password, setPassword] = useState('');
  const [confirm, setConfirm] = useState('');
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!code.trim()) { setErr('enter the setup code from the server log'); return; }
    if (password.length < MIN_PASSWORD) { setErr(`the password needs at least ${MIN_PASSWORD} characters`); return; }
    if (password !== confirm) { setErr('the passwords do not match'); return; }
    setBusy(true);
    completeSetup(code, password)
      .then(onDone)
      .catch((error: unknown) => {
        if (error instanceof GateError && error.type === 'setup_complete') { onAlreadySet(); return; }
        setErr(error instanceof Error ? error.message : 'setup failed');
      })
      .finally(() => setBusy(false));
  };

  const edit = (set: (v: string) => void) => (e: React.ChangeEvent<HTMLInputElement>) => { set(e.target.value); setErr(''); };

  return (
    <>
      <h1 className="mt-6 text-[20px] font-semibold">Set the dashboard password</h1>
      <p className="mt-2 text-[13px] text-mute">
        First boot. Enter the setup code printed in the ezllm server log, then choose a password.
      </p>
      <form onSubmit={submit} className="mt-6">
        <Field id="setup-code" label="Setup code" autoFocus autoComplete="one-time-code" placeholder="XXXX-XXXX"
          value={code} onChange={edit(setCode)} className={cx(fieldClass(false), 'font-mono uppercase tracking-widest')} />
        <Field id="setup-password" label={`New password (at least ${MIN_PASSWORD} characters)`} type="password"
          autoComplete="new-password" value={password} onChange={edit(setPassword)} className={fieldClass(false)} />
        <PasswordStrength password={password} />
        <Field id="setup-confirm" label="Confirm password" type="password" autoComplete="new-password"
          value={confirm} onChange={edit(setConfirm)} className={fieldClass(false)} />
        {err && <p className="mt-2 text-xs text-bad">{err}</p>}
        <button type="submit" disabled={busy} className={cx(btn.base, btn.primary, 'mt-4 w-full')}>
          {busy ? <Spinner /> : null} Set password
        </button>
      </form>
    </>
  );
}

function SidebarContent({ onNavigate }: { onNavigate?: () => void }) {
  const location = useLocation();
  const [theme, setThemeState] = useState<Theme>(() => readTheme());
 useEffect(() => subscribePreferences(() => setThemeState(readTheme())), []);
  const version = useServerVersion();

  const changeTheme = useCallback((t: Theme) => { setThemeState(t); setTheme(t); }, []);

  return (
    <div className="flex h-full flex-col">
      <div className="flex items-center gap-2.5 px-5 pb-6 pt-5">
        {LOGO}
        <span className="text-[15px] font-semibold tracking-tight">ezllm</span>
        {version && (
          <a href={REPO_URL} target="_blank" rel="noreferrer noopener"
            aria-label={`ezllm ${version} — source on GitHub`} title="source on GitHub"
            className="ml-auto text-[12px] text-mute transition-colors hover:text-ink">
            {version}
          </a>
        )}
      </div>
      <LayoutGroup id={onNavigate ? 'nav-drawer' : 'nav-side'}>
      <nav className="flex-1 space-y-0.5 px-3" aria-label="Main">
        {NAV.map((item) => {
          const active = item.match
            ? item.match.some((p) => location.pathname === p || location.pathname.startsWith(`${p}/`))
            : item.to === '/' ? location.pathname === '/' : location.pathname.startsWith(item.to);
          return (
            <NavLink key={item.to} to={item.to} onClick={onNavigate} end={item.to === '/'} title={item.hint}
              aria-current={active ? 'page' : undefined}
              className={cx('flex h-[34px] items-center gap-2.5 rounded px-2.5 transition-colors',
                active ? 'text-ink' : 'text-mute hover:bg-hover hover:text-ink')}>
              <span className={active ? 'text-ink' : 'text-mute'}><NavIcon label={item.label} /></span>
              {/* The selection mark: a 1px accent line exactly as wide as the
                  label, on its baseline — the same mark every tab and toggle
                  uses. No filled pill. */}
              <span className={cx('relative pb-px text-[14px]', active && 'font-medium')}>
                {item.label}
                {active && <SelectionLine layoutId="nav-line" />}
              </span>
            </NavLink>
          );
        })}
      </nav>
      </LayoutGroup>
      <div className="space-y-1 border-t border-line px-3 py-3">
        <ThemeToggle theme={theme} onChange={changeTheme} />
        <div className="flex items-center px-2.5 py-1">
          <a href={PROFILE_URL} target="_blank" rel="noreferrer noopener"
            aria-label="GitHub profile (c4lyp5o)" title="github.com/c4lyp5o"
            className="flex items-center gap-2 rounded p-1.5 text-mute transition-colors hover:bg-hover hover:text-ink">
            <GitHubIcon />
          </a>
          <button
            type="button"
            className="ml-auto rounded px-2.5 py-1.5 text-[13px] text-mute transition-colors hover:bg-bad-dim hover:text-bad"
            onClick={() => { void logout().finally(() => { onNavigate?.(); window.location.reload(); }); }}
          >
            Log out
          </button>
        </div>
      </div>
    </div>
  );
}

const PAGE_HEADERS: Record<string, { title: string; sub: string }> = {
  '/': { title: 'Dashboard', sub: 'Traffic, health and the latest calls through the gateway.' },
  '/stats': { title: 'Analytics', sub: 'Usage by provider, model, client and day.' },
  '/economics': { title: 'Analytics', sub: 'What compression and caching saved, and the largest requests.' },
  '/requests': { title: 'Analytics', sub: 'Every call in the ledger, filterable by tokens in and out.' },
  '/providers': { title: 'Providers', sub: 'Keys, model catalogs and quota.' },
  '/services': { title: 'Services', sub: 'Default models for embeddings, images and audio.' },
  '/chat': { title: 'Chat', sub: 'Send a message through any routed model.' },
  '/combos': { title: 'Combos', sub: 'Ordered routing chains with failover.' },
  '/rules': { title: 'Rules', sub: 'Token caps and allowed-use windows per model.' },
  '/compression': { title: 'Compression', sub: 'Profiles, engines and what they save.' },
  '/connect': { title: 'Connect', sub: 'Point clients at this gateway and issue API keys.' },
  '/neighbourhood': { title: 'Neighbourhood', sub: 'Every key is a house; the busier it is, the brighter it shines.' },
  '/settings': { title: 'Settings', sub: 'Preferences, security, backups and gateway policy.' },
};

export default function App() {
  const [token, setTokenState] = useState<string | null>(() => (isUnlocked() ? 'cookie' : null));
  const [drawer, setDrawer] = useState(false);
  const location = useLocation();
  // One bell, placed by width: in the mobile top bar, or beside the page
  // title on desktop (lg). Rendering only one keeps a single poller.
  const [desktop, setDesktop] = useState(() => window.matchMedia?.('(min-width: 1024px)').matches ?? true);
  useEffect(() => {
    const mq = window.matchMedia?.('(min-width: 1024px)');
    if (!mq) return;
    const on = () => setDesktop(mq.matches);
    mq.addEventListener('change', on);
    return () => mq.removeEventListener('change', on);
  }, []);

  const closeDrawer = useCallback(() => setDrawer(false), []);

  useEffect(() => {
    document.body.classList.toggle('overflow-hidden', drawer);
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setDrawer(false); };
    if (drawer) { document.addEventListener('keydown', onKey); return () => document.removeEventListener('keydown', onKey); }
  }, [drawer]);

  if (!token) return <Unlock onUnlock={() => setTokenState('cookie')} />;

  const header = PAGE_HEADERS[location.pathname] ?? PAGE_HEADERS['/'];

  return (
    <div className="flex min-h-dvh">
      {/* Top bar — mobile only */}
      <header className="fixed inset-x-0 top-0 z-30 flex h-12 items-center gap-3 border-b border-line bg-bg px-4 lg:hidden">
        <button type="button" aria-label="Open menu" onClick={() => setDrawer(true)}
          className="rounded-md border border-line p-1.5 text-dim hover:bg-hover hover:text-ink">
          <svg viewBox="0 0 20 20" className="h-4 w-4" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round">
            <path d="M3 5.5h14M3 10h14M3 14.5h14" />
          </svg>
        </button>
        {LOGO}
        <span className="text-[14px] font-semibold tracking-tight">ezllm</span>
        <div className="ml-auto">{!desktop && <NotificationBell />}</div>
      </header>

      {/* Sidebar — desktop */}
      <aside className="fixed bottom-0 left-0 top-0 z-30 hidden w-56 flex-col border-r border-line bg-bg lg:flex">
        <SidebarContent />
      </aside>

      {/* Sidebar — mobile drawer. AnimatePresence plays the exit, so closing
          is a slide-out rather than a snap.

          The DRAWER WRAPPER must itself be the motion element: AnimatePresence
          only holds a removed child alive when that child is a motion
          component. The wrapper used to be a plain <div>, so the overlay's
          and the aside's `exit` props were dead code and the drawer blinked
          out in a single frame instead of sliding away. */}
      <AnimatePresence>
        {drawer && (
          <motion.div
            className="fixed inset-0 z-50 lg:hidden"
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            transition={{ duration: DUR.fast }}
          >
            <motion.div
              className="absolute inset-0 bg-overlay"
              initial={{ opacity: 0 }} animate={{ opacity: 1 }} exit={{ opacity: 0 }}
              transition={{ duration: DUR.fast }} onClick={closeDrawer} />
            <motion.aside
              className="absolute bottom-0 left-0 top-0 w-64 border-r border-line bg-bg"
              initial={{ x: '-100%' }} animate={{ x: 0 }} exit={{ x: '-100%' }}
              transition={spring}>
              <SidebarContent onNavigate={closeDrawer} />
            </motion.aside>
          </motion.div>
        )}
      </AnimatePresence>

      <main className="min-w-0 flex-1 px-4 pb-16 pt-16 lg:ml-56 lg:px-8 lg:pt-7">
        {/* The screen's name at every width: the h1 is never hidden. */}
        <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
          <h1 className="text-[24px] font-semibold tracking-[-0.01em]">{header.title}</h1>
          <p className="text-[14px] text-mute">{header.sub}</p>
          <div className="flex flex-wrap items-center gap-2 self-center sm:ml-auto">
            <div id="page-aside" className="flex flex-wrap items-center gap-2" />
            {desktop && <NotificationBell />}
          </div>
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
                        className="absolute inset-x-3 -bottom-px h-px bg-accent"
                        transition={spring} />
                    )}
                  </>
                )}
              </NavLink>
            ))}
          </nav>
        )}
        {/* Page header → content gap. Was mt-8 and read as a void between the
            title row and the first content row (the health pills on
            Dashboard). */}
        {/* Each screen arrives with a short rise; keyed on the path so a
            tab switch inside Analytics replays it, a poll never does.
            AnimatePresence plays the OUTGOING screen's exit too — without
            it the old screen is unmounted in the same frame the new one
            starts, and the swap reads as a blink rather than a move.
            mode="wait" keeps them from overlapping (both are in normal flow,
            so a crossfade would push the page down); the exit is deliberately
            FAST (DUR.fast) so navigation never feels like it waits.

            Do NOT put initial={false} on this AnimatePresence. That flag is
            inherited by EVERY motion descendant, not just the direct child:
            it overrides each descendant's own `initial`, so the panels'
            staggered entrances were silently skipped and the whole screen
            appeared at its final state on the first paint. */}
        <AnimatePresence mode="wait">
          <motion.div key={location.pathname} className="mt-5"
            initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }}
            exit={{ opacity: 0, y: -6, transition: { duration: DUR.fast, ease: [0.22, 1, 0.36, 1] } }}
            transition={{ duration: 0.32, ease: [0.22, 1, 0.36, 1] }}>
            <PageBoundary key={location.pathname}>
            <Suspense fallback={<PageLoading />}>
            <Routes>
              <Route path="/" element={<Dashboard />} />
      <Route path="/stats" element={<Stats />} />
      <Route path="/economics" element={<Economics />} />
      <Route path="/requests" element={<Requests />} />
              <Route path="/providers" element={<Providers />} />
              <Route path="/services" element={<Services />} />
              <Route path="/chat" element={<Chat />} />
              <Route path="/combos" element={<Combos />} />
              <Route path="/rules" element={<Rules />} />
              <Route path="/compression" element={<Compression />} />
              <Route path="/connect" element={<Connect />} />
              <Route path="/neighbourhood" element={<Neighbourhood />} />
              <Route path="/settings" element={<Settings />} />
              <Route path="*" element={<Dashboard />} />
            </Routes>
            </Suspense>
            </PageBoundary>
          </motion.div>
        </AnimatePresence>
      </main>
    </div>
  );
}
