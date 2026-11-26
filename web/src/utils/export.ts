// Download what a page shows as CSV or JSON. Everything happens in the
// browser: the rows are the ones already loaded, nothing is fetched again.

export type Column<T> = { key: string; get: (row: T) => unknown };

/** One CSV field: quoted when it holds a comma, quote or line break. A field
 *  that starts with = + - @ is prefixed with ' so a spreadsheet does not run it
 *  as a formula (an upstream error text can start with one). */
export function csvField(v: unknown): string {
  if (v == null) return '';
  let s = typeof v === 'string' ? v : typeof v === 'object' ? JSON.stringify(v) : String(v);
  if (/^[=+\-@\t\r]/.test(s) && typeof v === 'string') s = `'${s}`;
  return /[",\r\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
}

export function toCSV<T>(rows: T[], cols: Column<T>[]): string {
  const head = cols.map((c) => csvField(c.key)).join(',');
  const body = rows.map((r) => cols.map((c) => csvField(c.get(r))).join(','));
  return [head, ...body].join('\r\n') + '\r\n';
}

/** Hands the text to the browser as a file to save. */
export function download(name: string, type: string, text: string): void {
  const url = URL.createObjectURL(new Blob([text], { type }));
  const a = document.createElement('a');
  a.href = url;
  a.download = name;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 0);
}

/** "ezllm-requests-2026-11-26" plus the extension. */
export const exportName = (what: string, ext: 'csv' | 'json', now = new Date()) =>
  `ezllm-${what}-${now.toISOString().slice(0, 10)}.${ext}`;
