// Theme state. Dark is the default; light is an explicit opt-out.
//
// The class is applied by an inline <script> in index.html BEFORE React
// mounts, so there is never a flash of the wrong palette on load. This module
// is the React-side mirror of that decision — it reads the same key and keeps
// the DOM in sync afterwards.
//
// Why localStorage and not a preference server call: this is a per-browser
// rendering choice on a control plane, not account state. A second device
// legitimately wants a different theme, and a round trip before first paint
// would reintroduce the flash the inline script exists to prevent.

const KEY = 'ezllm.theme';

export type Theme = 'dark' | 'light';

/** Stored preference, or the default when nothing has been chosen. */
export function readTheme(): Theme {
  try {
    return localStorage.getItem(KEY) === 'light' ? 'light' : 'dark';
  } catch {
    return 'dark'; // private mode / storage blocked: the default still applies
  }
}

export function applyTheme(t: Theme): void {
  const root = document.documentElement;
  root.classList.toggle('dark', t === 'dark');
  root.classList.toggle('light', t === 'light');
}

export function setTheme(t: Theme): void {
  applyTheme(t);
  try { localStorage.setItem(KEY, t); } catch { /* noop — the class still applies */ }
}

/**
 * Subscribe to OS-level scheme changes.
 *
 * NOT used by the dashboard: the operator's theme is a stored choice or dark,
 * and nothing follows the OS. This is kept exported because it is the correct
 * helper the moment an "auto" option is added, and deleting it would mean
 * rewriting the matchMedia plumbing from scratch then. It is deliberately
 * unreferenced — see readTheme for the actual policy.
 */
export function onSystemSchemeChange(handler: (t: Theme) => void): () => void {
  if (typeof window.matchMedia !== 'function') return () => {};
  const mq = window.matchMedia('(prefers-color-scheme: light)');
  const fn = (e: MediaQueryListEvent) => handler(e.matches ? 'light' : 'dark');
  mq.addEventListener('change', fn);
  return () => mq.removeEventListener('change', fn);
}
