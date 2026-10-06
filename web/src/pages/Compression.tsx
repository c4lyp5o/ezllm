// COMPRESSION — skeleton page. Compression profiles arrive in M5.
import { Chip, SectionLabel } from '../ui';

const ENGINES = ['Caveman', 'RTK', 'session dedup', 'headroom'] as const;

export default function Compression() {
  return (
    <div className="mx-auto max-w-3xl animate-fade-up">
      <div className="rounded-xl border border-line bg-surface px-8 py-10 text-center">
        <SectionLabel className="justify-center">Compression profiles</SectionLabel>
        <h2 className="mt-3 text-lg font-semibold">Arrives in M5</h2>
        <p className="mx-auto mt-2 max-w-md text-[13px] leading-relaxed text-mute">
          Prompt-compression profiles that sit between the router and your hops.
          Combos will reference them from their own settings once shipping.
        </p>
        <div className="mt-6 flex flex-wrap items-center justify-center gap-2">
          {ENGINES.map((e) => (
            <Chip key={e} tone="neutral" className="cursor-not-allowed opacity-50" title="not yet implemented">
              <span className="font-mono">{e}</span> <span className="text-mute">· M5</span>
            </Chip>
          ))}
        </div>
      </div>
      <div className="mt-4 rounded-xl border border-line bg-surface px-6 py-5">
        <SectionLabel>In the plan</SectionLabel>
        <ul className="mt-3 space-y-1.5 text-[13px] text-dim">
          <li className="flex gap-2.5"><span className="text-mute">·</span>Per-profile engine mix, thresholds and minimum-max-tokens guards</li>
          <li className="flex gap-2.5"><span className="text-mute">·</span>Attach a profile to any combo hop — compression applies at proxy time</li>
          <li className="flex gap-2.5"><span className="text-mute">·</span>Savings roll into the ledger (<span className="font-mono text-mute">saved</span> column is already live)</li>
        </ul>
      </div>
    </div>
  );
}
