// Import configuration: pick an export file, read what it would change, then
// apply it. Providers it creates arrive turned off, because exports have no keys.
import { useRef, useState } from 'react';
import { type ImportPlan, SECTION_LABEL, importConfig, importWrites, summarizeImport } from '../utils/import';
import { Chip, SectionLabel, btn, cx } from './ui';

const TONE = { create: 'accent', update: 'warn', unchanged: 'neutral', skip: 'neutral' } as const;
const errorText = (e: unknown) => (e instanceof Error ? e.message : 'The import could not be read.');

export default function ImportConfig({ panel }: { panel: string }) {
  const file = useRef<HTMLInputElement>(null);
  const [text, setText] = useState('');
  const [name, setName] = useState('');
  const [plan, setPlan] = useState<ImportPlan | null>(null);
  const [showAll, setShowAll] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [done, setDone] = useState('');

  const reset = () => { setText(''); setName(''); setPlan(null); setShowAll(false); if (file.current) file.current.value = ''; };
  const choose = async (f: File | undefined) => {
    if (!f) return;
    setError(''); setDone(''); setBusy(true);
    try {
      const body = await f.text();
      setPlan(await importConfig(body, true));
      setText(body); setName(f.name);
    } catch (e) { reset(); setError(errorText(e)); }
    finally { setBusy(false); }
  };
  const apply = async () => {
    setBusy(true); setError('');
    try {
      const out = await importConfig(text, false);
      const created = out.items.some((i) => i.section === 'accounts' && i.action === 'create');
      setDone(`Imported ${name}. ${summarizeImport(out.summary)}.${created ? ' New providers are turned off: add a key to each, then turn it on.' : ''}`);
      reset();
    } catch (e) { setError(errorText(e)); }
    finally { setBusy(false); }
  };

  const rows = plan ? plan.items.filter((i) => showAll || i.action !== 'unchanged') : [];
  return (
    <section className={panel}>
      <SectionLabel>Import configuration</SectionLabel>
      <p className="mt-2 text-sm text-mute">Bring in providers, combos, compression profiles, rules and settings from an export. A provider that is already here keeps its keys. A new one is created turned off, because exports have no keys.</p>
      <div className="mt-4 flex flex-wrap items-center gap-3">
        <label className={cx(btn.base, btn.ghost, 'cursor-pointer', busy && 'pointer-events-none opacity-60')}>
          {busy && !plan ? 'Reading…' : 'Choose an export file'}
          <input ref={file} type="file" accept="application/json,.json" className="sr-only" aria-label="Export file to import"
            onChange={(e) => void choose(e.target.files?.[0])} />
        </label>
        {name && <span className="text-sm text-mute">{name}</span>}
      </div>
      {plan && (
        <div className="mt-4">
          <p className="text-sm" role="status">{summarizeImport(plan.summary)}</p>
          {plan.items.some((i) => i.action === 'unchanged') && (
            <label className="mt-2 flex items-center gap-2 text-xs text-mute">
              <input type="checkbox" checked={showAll} onChange={(e) => setShowAll(e.target.checked)} /> Show unchanged
            </label>
          )}
          {rows.length > 0 && (
            <ul className="mt-3 divide-y divide-line rounded-lg border border-line text-sm">
              {rows.map((i) => (
                <li key={`${i.section}:${i.name}`} className="flex flex-wrap items-center gap-2 px-3 py-2">
                  <Chip tone={TONE[i.action]}>{i.action}</Chip>
                  <span className="text-mute">{SECTION_LABEL[i.section]}</span>
                  <span className="font-mono text-[13px]">{i.name}</span>
                  {i.changes?.length ? <span className="text-xs text-mute">· {i.changes.join(', ')}</span> : null}
                  {i.note && <span className="w-full text-xs text-mute">{i.note}</span>}
                </li>
              ))}
            </ul>
          )}
          <div className="mt-4 flex flex-wrap gap-3">
            <button type="button" className={cx(btn.base, btn.primary)} disabled={busy || !importWrites(plan.summary)} onClick={() => void apply()}>
              {busy ? 'Importing…' : 'Apply import'}
            </button>
            <button type="button" className={cx(btn.base, btn.ghost)} disabled={busy} onClick={reset}>Cancel</button>
          </div>
        </div>
      )}
      {error && <p role="alert" className="mt-3 text-sm text-bad">{error}</p>}
      {done && <p role="status" className="mt-3 text-sm text-ok">{done}</p>}
    </section>
  );
}
