// Configuration import: POST /admin/import with an export file. A dry run
// answers the plan; the same call without it writes the plan.
import { apiFetch } from './api';

export type ImportItem = {
  section: 'settings' | 'accounts' | 'compression_profiles' | 'combos' | 'model_rules';
  name: string;
  action: 'create' | 'update' | 'unchanged' | 'skip';
  changes?: string[];
  note?: string;
};
export type ImportPlan = { dry_run: boolean; items: ImportItem[]; summary: Partial<Record<ImportItem['action'], number>> };

export const SECTION_LABEL: Record<ImportItem['section'], string> = {
  settings: 'Setting', accounts: 'Provider', compression_profiles: 'Compression profile', combos: 'Combo', model_rules: 'Rule',
};

/** Sends the file's text as it is: the server reads only what it imports. */
export function importConfig(text: string, dryRun: boolean): Promise<ImportPlan> {
  return apiFetch<ImportPlan>(`/admin/import${dryRun ? '?dry_run=true' : ''}`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: text,
  });
}

/** "2 to create, 1 to update, 5 unchanged": the plan in one line. */
export function summarizeImport(s: ImportPlan['summary']): string {
  const parts: string[] = [];
  if (s.create) parts.push(`${s.create} to create`);
  if (s.update) parts.push(`${s.update} to update`);
  if (s.unchanged) parts.push(`${s.unchanged} unchanged`);
  if (s.skip) parts.push(`${s.skip} skipped`);
  return parts.length ? parts.join(', ') : 'Nothing to import';
}

/** Whether applying the plan would write anything. */
export const importWrites = (s: ImportPlan['summary']) => (s.create ?? 0) + (s.update ?? 0) > 0;
