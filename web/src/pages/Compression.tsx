// COMPRESSION — live profiles (M5): session dedup + RTK shipping today,
// Caveman + headroom land in M6.5. Profiles attach two ways: a combo's
// compression_profile_id, or per-request via the x-ezllm-compression header
// ("<name>", "off" to skip, anything else falls back to the combo).
import { useEffect, useState } from 'react';
import { compressionProfiles, type CompressionProfile } from '../api';
import { Chip, cx, Mono, SectionLabel } from '../ui';

const ENGINES = [
  { id: 'session_dedup', label: 'session dedup', live: true,
    note: 'drops exact duplicate system / user / assistant turns' },
  { id: 'rtk', label: 'RTK', live: true,
    note: 'filters CLI / tool output; errors and failures are never dropped' },
  { id: 'caveman', label: 'Caveman', live: false, note: 'M6.5' },
  { id: 'headroom', label: 'headroom', live: false, note: 'M6.5' },
];

export default function Compression() {
  const [profiles, setProfiles] = useState<CompressionProfile[] | null>(null);
  const [err, setErr] = useState('');

  useEffect(() => {
    compressionProfiles()
      .then(setProfiles)
      .catch((e) => setErr(String((e as Error).message ?? e)));
  }, []);

  return (
    <div className="mx-auto max-w-3xl animate-fade-up space-y-4">
      <div className="rounded-xl border border-line bg-surface px-6 py-5">
        <SectionLabel>Engines</SectionLabel>
        <div className="mt-3 flex flex-wrap gap-2">
          {ENGINES.map((e) => (
            <Chip key={e.id} tone={e.live ? 'ok' : 'neutral'}
              className={e.live ? '' : 'cursor-not-allowed opacity-50'}
              title={e.note}>
              <Mono>{e.label}</Mono> <span className="text-mute">· {e.live ? 'live' : e.note}</span>
            </Chip>
          ))}
        </div>
        <p className="mt-3 text-[12px] leading-relaxed text-mute">
          The final user message is never rewritten, and no stage may drop
          tool calls or their responses. Nothing shrinks the conversation by
          under 5% — a wash keeps the original bytes. If anything ever goes
          wrong the request ships uncompressed (fail-open).
        </p>
      </div>

      <div className="rounded-xl border border-line bg-surface px-6 py-5">
        <SectionLabel>Profiles</SectionLabel>
        {err && <p className="mt-3 text-[12.5px] text-bad">{err}</p>}
        {!err && profiles === null && (
          <p className="mt-3 text-[12.5px] text-mute">loading…</p>)}
        {!err && profiles !== null && profiles.length === 0 && (
          <p className="mt-3 text-[12.5px] text-mute">
            No profiles yet. Create one through the API
            (<Mono>POST /admin/compression-profiles</Mono>) — the editor UI ships with M6.5.
          </p>)}
        {profiles?.map((p) => (
          <div key={p.id} className="mt-3 rounded-lg border border-line px-4 py-3">
            <div className="flex items-center gap-2">
              <Mono className="text-[13px] font-semibold">{p.name}</Mono>
              <Chip tone={p.enabled ? 'ok' : 'warn'}>{p.enabled ? 'enabled' : 'paused'}</Chip>
              <span className="ml-auto text-[11px] text-mute">
                min {Math.round(p.min_compress_ratio * 100)}% ·{' '}
                {p.exempt_last_turn ? 'last turn exempt' : 'full rewrite'} ·{' '}
                {p.fail_open ? 'fail-open' : 'fail-closed'}
              </span>
            </div>
            <div className="mt-2 flex flex-wrap items-center gap-1.5">
              {(p.stages ?? []).map((s, i) => (
                <span key={i} className={cx('flex items-center gap-1.5',
                  !s.engine.match(/^(session_dedup|rtk)$/) && 'opacity-50')}>
                  {i > 0 && <span className="text-mute">→</span>}
                  <Chip tone="neutral"><Mono>{s.engine}</Mono></Chip>
                </span>
              ))}
            </div>
            {p.notes && <p className="mt-2 text-[12px] text-dim">{p.notes}</p>}
          </div>
        ))}
      </div>

      <div className="rounded-xl border border-line bg-surface px-6 py-5">
        <SectionLabel>Wiring</SectionLabel>
        <ul className="mt-3 space-y-1.5 text-[13px] text-dim">
          <li>· A <span className="font-mono text-[12px]">combo</span> carries one profile
            — pick it in the combo editor (<Mono>compression_profile_id</Mono>).</li>
          <li>· Any client can override per request with the
            <Mono> x-ezllm-compression</Mono> header: a profile name, or
            <Mono> off</Mono> to ship the original bytes.</li>
          <li>· Saved tokens land in Stats → Compression / Savings (ledger truth:
            what we sent is what is recorded).</li>
        </ul>
      </div>
    </div>
  );
}
