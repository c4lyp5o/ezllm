// Theme switch. Dark is the default; light is an explicit opt-out.
//
// It lives in its own file rather than in ui.tsx on purpose: ui.tsx is shared
// by every page and is needed by the analytics commit that lands BEFORE the
// theme work. Keeping the toggle separate means ui.tsx never has to import
// theme.ts, so the two commits do not straddle one file.
import { cx } from './ui';
import type { Theme } from '../utils/theme';

// An explicit two-state switch, not a single "dark mode" checkbox: light is
// the default-off state, so a checkbox labelled "Dark" would read wrong on
// first load AND wrong again after an explicit light choice. The label states
// what you currently have; the hint states what the click gives you;
// aria-checked carries the machine-readable state.
export default function ThemeToggle({ theme, onChange }: { theme: Theme; onChange: (t: Theme) => void }) {
  const next: Theme = theme === 'dark' ? 'light' : 'dark';
  return (
    <button
      type="button"
      role="switch"
      aria-checked={theme === 'dark'}
      aria-label={`Theme: ${theme}. Switch to ${next} mode`}
      title={`Switch to ${next} mode`}
      onClick={() => onChange(next)}
      className="group flex w-full items-center gap-2.5 rounded-lg px-3 py-1.5 text-left transition-colors hover:bg-hover"
    >
      <span className="relative flex h-4 w-4 shrink-0 items-center justify-center" aria-hidden>
        {/* Sun and moon cross-fade and swap scale rather than popping, so the
            icon change reads as the same control moving between states. */}
        <svg viewBox="0 0 24 24" className={cx('absolute h-3.5 w-3.5 transition-all duration-200',
          theme === 'dark' ? 'scale-100 opacity-100' : 'scale-75 opacity-0')}
          fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round">
          <path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8Z" />
        </svg>
        <svg viewBox="0 0 24 24" className={cx('absolute h-3.5 w-3.5 transition-all duration-200',
          theme === 'light' ? 'scale-100 opacity-100' : 'scale-75 opacity-0')}
          fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round">
          <circle cx="12" cy="12" r="4" />
          <path d="M12 2v2m0 16v2M2 12h2m16 0h2M5 5l1.5 1.5M17.5 17.5 19 19M19 5l-1.5 1.5M6.5 17.5 5 19" />
        </svg>
      </span>
      <span className="text-[11.5px] text-mute transition-colors group-hover:text-ink">
        {theme === 'dark' ? 'Dark' : 'Light'} mode
      </span>
      <span className="ml-auto text-[10.5px] text-mute/70">switch to {next}</span>
    </button>
  );
}
