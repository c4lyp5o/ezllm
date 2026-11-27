import { subscribePreferences } from './utils/preferences';
import type { UpdateStatus } from './utils/api';
// App shell: token unlock gate → sidebar layout → routes.
// Nothing renders or fetches before the unlock screen hands back a token.
import { Fragment, Suspense, lazy, useCallback, useEffect, useState } from 'react';
import { NavLink, Route, Routes, useLocation } from 'react-router-dom';
import { AnimatePresence, LayoutGroup, motion } from 'motion/react';
import { Brand } from './ez/Brand';
import { UpdateArrow } from './ez/UpdateArrow';
import { GateError, completeSetup, dashboardLogin, getSetupStatus, getUpdate, isUnlocked, logout, markUnlocked } from './utils/api';
import { DUR, SelectionLine, spring } from './components/motion';
import RailwayBackground from './components/RailwayBackground';
import PasswordStrength from './components/PasswordStrength';
import NotificationBell from './components/NotificationBell';
import { MIN_PASSWORD } from './utils/passwordStrength';
import { readTheme, setTheme, type Theme } from './utils/theme';
import { CopyButton, Spinner, btn, cx } from './components/ui';
import ThemeToggle from './components/ThemeToggle';
import { CommandPalette, OPEN_PALETTE } from './components/CommandPalette';
import { PageBoundary, PageLoading } from './components/PageBoundary';
import { useExtras, type ExtraKey } from './utils/extras';

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
const Start = lazy(() => import('./pages/Start'));

// One sidebar entry per destination — Stats / Token Economics / Requests live
// together under Analytics (same data family, tabbed).
// A labelled sidebar, not the dealt icon rail: ten destinations need words.
// The hints are kept as each item's tooltip, not as a second line of text.
type NavItem = { to: string; label: string; hint: string; match?: readonly string[]; extra?: ExtraKey; group: 'core' | 'more' | 'end' };

const ANALYTICS_ROUTES = ['/stats', '/economics', '/requests'] as const;

// Core first (what you need to route a request), then More (what ezllm can
// also do for you), then Settings.
const NAV: NavItem[] = [
  { group: 'core', to: '/', label: 'Dashboard', hint: 'overview · health · ledger' },
  { group: 'core', to: '/providers', label: 'Providers', hint: 'keys · models · quota' },
  { group: 'core', to: '/combos', label: 'Combos', hint: 'routing chains' },
  { group: 'core', to: '/connect', label: 'Connect', hint: 'address · app keys' },
  { group: 'core', to: '/stats', label: 'Analytics', hint: 'stats · economics · requests', match: ANALYTICS_ROUTES },
  { group: 'more', extra: 'compression', to: '/compression', label: 'Compression', hint: 'fewer tokens on long chats' },
  { group: 'more', extra: 'rules', to: '/rules', label: 'Rules', hint: 'token limits · allowed hours' },
  { group: 'more', extra: 'services', to: '/services', label: 'Services', hint: 'embeddings · images · audio' },
  { group: 'more', extra: 'chat', to: '/chat', label: 'Chat', hint: 'try your models' },
  { group: 'more', extra: 'neighbourhood', to: '/neighbourhood', label: 'Neighbourhood', hint: 'your providers drawn as a city' },
  { group: 'end', to: '/settings', label: 'Settings', hint: 'instance administration' },
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

// A newer release, when the server's daily check found one. Re-read hourly so a
// tab left open learns about it; a failed read keeps what is shown.
const UPDATE_POLL_MS = 3_600_000;
function useUpdate(): UpdateStatus | null {
  const [update, setUpdate] = useState<UpdateStatus | null>(null);
  useEffect(() => {
    let alive = true;
    const read = () => getUpdate().then((u) => { if (alive) setUpdate(u); }).catch(() => undefined);
    read();
    const t = setInterval(read, UPDATE_POLL_MS);
    return () => { alive = false; clearInterval(t); };
  }, []);
  return update?.available ? update : null;
}

// Where the source lives. Opened in a new tab with an explicit rel: this is a
// cross-origin link out of the control plane, and without rel="noreferrer"
// the destination receives the dashboard's own URL as the referrer.
const REPO_URL = 'https://github.com/c4lyp5o/ezllm';

// The GitHub mark shared by the ez* apps' footers.
function GitHubIcon({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 24 24" className={cx('h-4 w-4', className)} fill="currentColor" aria-hidden focusable="false">
      <path d="M12 .5a11.5 11.5 0 0 0-3.64 22.41c.58.1.79-.25.79-.56v-2c-3.2.7-3.88-1.37-3.88-1.37-.52-1.33-1.28-1.69-1.28-1.69-1.04-.71.08-.7.08-.7 1.16.08 1.77 1.19 1.77 1.19 1.03 1.76 2.7 1.25 3.36.96.1-.75.4-1.25.73-1.54-2.55-.29-5.24-1.28-5.24-5.68 0-1.26.45-2.28 1.18-3.09-.12-.29-.51-1.46.11-3.04 0 0 .97-.31 3.17 1.18a11 11 0 0 1 5.77 0c2.2-1.49 3.17-1.18 3.17-1.18.63 1.58.23 2.75.11 3.04.74.81 1.18 1.83 1.18 3.09 0 4.41-2.69 5.38-5.25 5.67.41.36.78 1.06.78 2.14v3.17c0 .31.21.67.8.56A11.5 11.5 0 0 0 12 .5Z" />
    </svg>
  );
}

function LogOutIcon({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 24 24" className={cx('h-4 w-4', className)} fill="none" stroke="currentColor" strokeWidth="1.8"
      strokeLinecap="round" strokeLinejoin="round" aria-hidden focusable="false">
      <path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4" />
      <path d="m16 17 5-5-5-5" />
      <path d="M21 12H9" />
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
        <Brand name="ezllm" />
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

// Where the setup code is, for the usual Docker install.
const SETUP_CODE_CMD = 'docker logs ezllm | grep setup';

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
      // A new instance goes straight to Get started.
      .then(() => { window.history.replaceState(null, '', '/start'); onDone(); })
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
        This is the first start. Enter the setup code from the ezllm server log, then pick a password.
      </p>
      <div className="mt-3 flex items-center gap-2">
        <code className="min-w-0 flex-1 truncate rounded-md border border-line bg-bg px-2.5 py-1.5 font-mono text-[12px]">{SETUP_CODE_CMD}</code>
        <CopyButton text={SETUP_CODE_CMD} label="Copy" />
      </div>
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
  const update = useUpdate();

  const changeTheme = useCallback((t: Theme) => { setThemeState(t); setTheme(t); }, []);
  // Read again on each page change, so "in use" follows what you just set up.
  const extras = useExtras(location.pathname);

  return (
    <div className="flex h-full flex-col">
      <div className="flex items-center gap-2.5 px-5 pb-6 pt-5">
        <Brand name="ezllm" />
        {version && (
          <a href={update ? update.url : REPO_URL} target="_blank" rel="noreferrer noopener"
            aria-label={update ? `ezllm ${version}, v${update.latest} available, release notes (opens in a new tab)`
              : `ezllm ${version}, source on GitHub (opens in a new tab)`}
            title={update ? `v${update.latest} available` : 'Source on GitHub'}
            className={cx('ml-auto text-[12px] transition-colors', update
              ? 'inline-flex items-center gap-1 rounded bg-warn-dim px-2 py-0.5 text-warn hover:text-ink' : 'text-mute hover:text-ink')}>
            {version}
            {update && <UpdateArrow />}
          </a>
        )}
      </div>
      <LayoutGroup id={onNavigate ? 'nav-drawer' : 'nav-side'}>
      <nav className="flex-1 space-y-0.5 px-3" aria-label="Main">
        {NAV.map((item, i) => {
          const heading = item.group === 'more' && NAV[i - 1]?.group !== 'more';
          const gap = item.group === 'end';
          const inUse = item.extra ? extras?.[item.extra] === true : false;
          const active = item.match
            ? item.match.some((p) => location.pathname === p || location.pathname.startsWith(`${p}/`))
            : item.to === '/' ? location.pathname === '/' : location.pathname.startsWith(item.to);
          return (
            <Fragment key={item.to}>
            {heading && <div className="px-2.5 pb-1 pt-5 text-[11px] font-medium uppercase tracking-wider text-mute" role="presentation">More</div>}
            {gap && <div className="pt-4" role="presentation" />}
            <NavLink to={item.to} onClick={onNavigate} end={item.to === '/'} title={item.hint}
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
              {inUse && <span className="ml-auto text-[11px] text-mute">in use</span>}
            </NavLink>
            </Fragment>
          );
        })}
      </nav>
      </LayoutGroup>
      <div className="px-3 pb-2">
        <button type="button" onClick={() => { onNavigate?.(); window.dispatchEvent(new Event(OPEN_PALETTE)); }}
          className="flex w-full items-center justify-between rounded-md border border-line px-3 py-1.5 text-[12px] text-mute transition-colors hover:border-line-strong hover:text-ink">
          <span>Jump to…</span><kbd className="font-sans text-[11px]">{/Mac|iPhone|iPad/.test(navigator.platform) ? '⌘K' : 'Ctrl K'}</kbd>
        </button>
      </div>
      <div className="grid grid-cols-3 border-t border-line px-3 py-2">
        <a href={REPO_URL} target="_blank" rel="noreferrer noopener"
          aria-label="ezllm source on GitHub (opens in a new tab)" title="Source on GitHub"
          className="flex h-9 items-center justify-center rounded text-mute transition-colors hover:bg-hover hover:text-ink">
          <GitHubIcon />
        </a>
        <ThemeToggle theme={theme} onChange={changeTheme} />
        <button
          type="button"
          aria-label="Log out" title="Log out"
          className="flex h-9 items-center justify-center rounded text-mute transition-colors hover:bg-bad-dim hover:text-bad"
          onClick={() => { void logout().finally(() => { onNavigate?.(); window.location.reload(); }); }}
        >
          <LogOutIcon />
        </button>
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
  '/compression': { title: 'Compression', sub: 'Compression profiles and how much they save.' },
  '/connect': { title: 'Connect', sub: 'App keys, and how to point your apps at ezllm.' },
  '/neighbourhood': { title: 'Neighbourhood', sub: 'Each provider is a building. Taller means busier; red means failing.' },
  '/settings': { title: 'Settings', sub: 'Preferences, security, backups and request limits.' },
  '/start': { title: 'Get started', sub: 'Three steps to your first request.' },
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
      <CommandPalette />
      {/* Top bar — mobile only */}
      <header className="fixed inset-x-0 top-0 z-30 flex h-12 items-center gap-3 border-b border-line bg-bg px-4 lg:hidden">
        <button type="button" aria-label="Open menu" onClick={() => setDrawer(true)}
          className="rounded-md border border-line p-1.5 text-dim hover:bg-hover hover:text-ink">
          <svg viewBox="0 0 20 20" className="h-4 w-4" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round">
            <path d="M3 5.5h14M3 10h14M3 14.5h14" />
          </svg>
        </button>
        <Brand name="ezllm" />
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
              <Route path="/start" element={<Start />} />
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
