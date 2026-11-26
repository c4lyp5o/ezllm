// Settings-form logic: the same bounds the server enforces, checked in the
// browser so a bad value is explained at the field instead of after a save.
// The server stays the authority; this only mirrors its documented ranges.

const UNIT_MS: Record<string, number> = { ns: 1e-6, us: 1e-3, 'µs': 1e-3, ms: 1, s: 1000, m: 60_000, h: 3_600_000 };

/** A Go duration ("90s", "1h30m") in milliseconds, or null when it is not one. */
export function parseDuration(s: string): number | null {
  const t = s.trim();
  if (!/^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/.test(t)) return null;
  let ms = 0;
  for (const m of t.matchAll(/(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/g)) ms += Number(m[1]) * UNIT_MS[m[2]];
  return ms;
}

function validTimezone(tz: string): boolean {
  try { new Intl.DateTimeFormat('en', { timeZone: tz }); return true; } catch { return false; }
}

const whole = (v: unknown, lo: number, hi: number, what: string): string => {
  const n = Number(v);
  return Number.isInteger(n) && n >= lo && n <= hi ? '' : `${what} must be a whole number from ${lo} to ${hi}.`;
};

/** An explanation of what is wrong with a setting's value, or '' when it is fine. */
export function settingError(key: string, value: unknown): string {
  const text = String(value ?? '').trim();
  switch (key) {
    case 'usage_timezone': return text && validTimezone(text) ? '' : 'Use an IANA timezone name, for example Europe/Berlin.';
    case 'catalog_interval': { const ms = parseDuration(text); return ms !== null && ms >= 60_000 ? '' : 'Use a duration of at least 1m, for example 12h.'; }
    case 'dispatch_deadline': { if (text === '') return ''; const ms = parseDuration(text); return ms !== null && ms >= 1000 ? '' : 'Use a duration of at least 1s (for example 30s), or leave it empty for no deadline.'; }
    case 'cooldown_429':
    case 'cooldown_5xx': { const ms = parseDuration(text); return ms !== null && ms > 0 ? '' : 'Use a duration such as 60s or 5m.'; }
    case 'retries': return whole(value, 0, 9, 'Retries');
    case 'attempt_budget': return whole(value, 0, 64, 'The attempt budget');
    case 'cooldown_strikes': return whole(value, 1, 1_000_000, 'The failure count');
    case 'backup_hour': return whole(value, 0, 23, 'The hour');
    case 'backup_keep': return whole(value, 1, 365, 'The number of backups');
    case 'alert_quota_percent': return whole(value, 1, 100, 'The percent');
    case 'ledger_retention_days': { const n = Number(value); return n === 0 || (Number.isInteger(n) && n >= 32 && n <= 3650) ? '' : 'Use 0 to keep every call, or a whole number of days from 32 to 3650.'; }
    case 'alert_cookie_hours': return whole(value, 1, 720, 'The number of hours');
    case 'alert_balance_below': { const n = Number(value); return Number.isFinite(n) && n > 0 && n <= 1_000_000 ? '' : 'Use an amount above 0, for example 2.'; }
    default: return '';
  }
}

// effectiveText says what a value MEANS for its setting: 0 retries is off,
// a 0 attempt budget and an empty deadline are no limit, a switch is On/Off.
// "Unlimited" for every empty value read wrong for most settings.
export function effectiveText(key: string, v: unknown): string {
  if (typeof v === 'boolean') return v ? 'On' : 'Off';
  if (v === 0 && key === 'retries') return '0 (off)';
  if (v === 0 && key === 'attempt_budget') return '0 (no limit)';
  if (v === 0 && key === 'ledger_retention_days') return '0 (keep every call)';
  if (v === '' || v == null) return key === 'dispatch_deadline' ? 'No limit' : '–';
  return String(v);
}
