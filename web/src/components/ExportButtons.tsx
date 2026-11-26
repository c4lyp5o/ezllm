// "CSV" / "JSON" buttons for a table's loaded rows.
import { type Column, download, exportName, toCSV } from '../utils/export';
import { btn, cx } from './ui';

export function ExportButtons<T>({ what, rows, cols, note }: {
  what: string; rows: T[]; cols: Column<T>[];
  /** Says which rows are exported, for the buttons' tooltip. */
  note: string;
}) {
  const disabled = rows.length === 0;
  const cls = cx(btn.base, btn.ghost, 'px-2.5 py-1 text-[12px]');
  return (
    <span className="inline-flex items-center gap-1.5">
      <button type="button" className={cls} disabled={disabled} title={`Download ${note} as CSV`}
        onClick={() => download(exportName(what, 'csv'), 'text/csv;charset=utf-8', toCSV(rows, cols))}>CSV</button>
      <button type="button" className={cls} disabled={disabled} title={`Download ${note} as JSON`}
        onClick={() => download(exportName(what, 'json'), 'application/json', JSON.stringify(rows, null, 2) + '\n')}>JSON</button>
    </span>
  );
}
