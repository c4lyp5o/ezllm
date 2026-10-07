// REQUESTS — the token dissection explorer.
// min/max filters on tokens in and tokens out over a time range, with summary
// totals over the WHOLE matching set (not just the page). Sort by either axis
// to surface the fattest calls. The question this page exists to answer: is
// the client sending too much history, or is the model burning it on output
// and reasoning? (row shape = the dashboard's recentCalls projection.)
import { useCallback, useEffect, useState, type FormEvent } from 'react';
import {
  normalizeFeedRow, requests, RANGE_LABEL,
  type FeedRow, type RangeKey, type RequestSort, type RequestsResp,
} from '../api';
import { SectionLabel, btn, cx, inputCls, labelCls } from '../ui';

const RANGES: RangeKey[] = ['24h', '7d', '30d', '365d'];
const SORTS: { key: RequestSort; label: string }[] = [
  { key: 'ts_desc', label: 'newest' },
  { key: 'tin_desc', label: 'tokens in' },
  { key: 'tout_desc', label: 'tokens out' },
];

type NumFields = { minTin: string; maxTin: string; minTout: string; maxTout: string };
const EMPTY_FIELDS: NumFields = { minTin: '', maxTin: '', minTout: '', maxTout: '' };
type Applied = { minTin?: number; maxTin?: number; minTout?: number; maxTout?: number };

// '' → undefined (unbounded); a valid non-negative integer → the value;
// anything else → 'bad' so the form can refuse it before the API does.
function parseField(raw: string): number | undefined | 'bad' {
  const t = raw.trim();
  if (t === '') return undefined;
  const n = Number(t);
  if (!Number.isInteger(n) || n < 0) return 'bad';
  return n;
}

function Seg<T extends string>({ options, value, onChange }: {
  options: { key: T; label: string }[]; value: T; onChange: (v: T) => void;
}) {
  return (
    <div className="inline-flex flex-wrap gap-1 rounded-lg border border-line bg-surface p-1">
      {options.map((o) => (
        <button
          key={o.key}
          onClick={() => onChange(o.key)}
          className={cx(
            'rounded-md px-2.5 py-1 text-[12px] font-medium transition-colors',
            o.key === value ? 'bg-accent text-white' : 'text-mute hover:text-ink',
          )}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}

function Stat({ label, value, sub }: { label: string; value: string; sub?: string }) {
  return (
    <div className="rounded-xl border border-line bg-surface px-4 py-3.5">
      <SectionLabel>{label}</SectionLabel>
      <div className="mt-1.5 font-mono text-[20px] font-semibold tabular-nums tracking-tight text-ink">{value}</div>
      {sub && <div className="mt-0.5 text-[11px] text-mute">{sub}</div>}
    </div>
  );
}

const nfmt = (v: number) => v.toLocaleString('en-US');
const fmtTs = (ts: string) =>
  new Date(ts).toLocaleString('en-GB', {
    day: '2-digit', month: 'short', hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false,
  });

const FIELDS: { key: keyof NumFields; label: string; ph: string }[] = [
  { key: 'minTin', label: 'tokens in ≥', ph: 'e.g. 50000' },
  { key: 'maxTin', label: 'tokens in ≤', ph: 'e.g. 200000' },
  { key: 'minTout', label: 'tokens out ≥', ph: 'e.g. 1000' },
  { key: 'maxTout', label: 'tokens out ≤', ph: 'e.g. 8000' },
];

export default function Requests() {
  const [fields, setFields] = useState<NumFields>(EMPTY_FIELDS);
  const [applied, setApplied] = useState<Applied>({});
  const [range, setRange] = useState<RangeKey>('24h');
  const [sort, setSort] = useState<RequestSort>('ts_desc');
  const [data, setData] = useState<RequestsResp | null>(null);
  const [rows, setRows] = useState<FeedRow[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    setErr(null);
    try {
      const r = await requests({ ...applied, range, sort });
      setData(r);
      setRows(r.rows.map(normalizeFeedRow));
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, [applied, range, sort]);

  useEffect(() => { load(); }, [load]);

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const parsed: Applied = {};
    for (const key of Object.keys(fields) as (keyof NumFields)[]) {
      const v = parseField(fields[key]);
      if (v === 'bad') {
        setErr(`${key} must be a non-negative whole number`);
        return;
      }
      if (v !== undefined) parsed[key] = v;
    }
    setApplied(parsed);
  };

  const clear = () => { setFields(EMPTY_FIELDS); setApplied({}); };
  const s = data?.summary;

  return (
    <div className="space-y-5">
      {/* Filter bar — token bounds commit on Apply; range/sort apply instantly. */}
      <form onSubmit={submit} className="rounded-xl border border-line bg-surface px-6 py-5">
        <SectionLabel>dissect · filter the calls ledger</SectionLabel>
        <div className="mt-3 flex flex-wrap items-end gap-3">
          {FIELDS.map((f) => (
            <label key={f.key} className="block">
              <span className={labelCls}>{f.label}</span>
              <input
                className={cx(inputCls, 'w-36 font-mono')}
                inputMode="numeric"
                placeholder={f.ph}
                value={fields[f.key]}
                onChange={(e) => setFields({ ...fields, [f.key]: e.target.value })}
              />
            </label>
          ))}
          <div className="flex items-center gap-2 pb-0.5">
            <button type="submit" className={cx(btn.base, btn.primary)}>Apply</button>
            <button type="button" onClick={clear} className={cx(btn.base, btn.ghost)}>Clear</button>
          </div>
        </div>
        <div className="mt-4 flex flex-wrap items-center gap-3">
          <SectionLabel>range</SectionLabel>
          <Seg
            options={RANGES.map((k) => ({ key: k, label: RANGE_LABEL[k] }))}
            value={range} onChange={setRange}
          />
          <SectionLabel>order</SectionLabel>
          <Seg options={SORTS} value={sort} onChange={setSort} />
        </div>
      </form>

      {/* Summary strip — over the WHOLE matching set, not the returned page. */}
      {s && (
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
          <Stat label="requests" value={nfmt(s.count)} sub="matching the filter" />
          <Stat label="tokens in" value={nfmt(s.tin)} sub="Σ across matches" />
          <Stat label="tokens out" value={nfmt(s.tout)} sub="Σ across matches" />
          <Stat label="max in" value={nfmt(s.max_tin)} sub={`max out ${nfmt(s.max_tout)}`} />
          <Stat label="reasoning" value={nfmt(s.reasoning)} sub="Σ burned on thinking" />
        </div>
      )}

      {/* Result rows */}
      <div className="rounded-xl border border-line bg-surface">
        {err && <div className="border-b border-line px-4 py-2.5 text-[12px] text-bad">{err}</div>}
        <div className="overflow-x-auto">
          <table className="w-full text-[12px]">
            <thead>
              <tr className="text-left text-mute">
                <th className="px-4 py-2 font-medium">time</th>
                <th className="px-4 py-2 font-medium">model</th>
                <th className="px-4 py-2 font-medium">client</th>
                <th className="px-4 py-2 text-right font-medium">status</th>
                <th className="px-4 py-2 text-right font-medium">in</th>
                <th className="px-4 py-2 text-right font-medium">out</th>
                <th className="px-4 py-2 text-right font-medium">cached</th>
                <th className="px-4 py-2 text-right font-medium">reasoning</th>
                <th className="px-4 py-2 text-right font-medium">ms</th>
              </tr>
            </thead>
            <tbody>
              {rows.length === 0 && !loading && (
                <tr>
                  <td colSpan={9} className="px-4 py-6 text-center text-mute">
                    No calls match this filter.
                  </td>
                </tr>
              )}
              {rows.map((r, i) => (
                <tr key={`${r.ts}-${i}`} className="border-t border-line">
                  <td className="whitespace-nowrap px-4 py-2 font-mono text-mute">{fmtTs(r.ts)}</td>
                  <td className="px-4 py-2 font-mono">{r.model}</td>
                  <td className="px-4 py-2">{r.client}</td>
                  <td className={cx('px-4 py-2 text-right font-mono', r.status >= 400 ? 'text-bad' : 'text-ink')}>
                    {r.status}
                  </td>
                  <td className="px-4 py-2 text-right font-mono tabular-nums">{nfmt(r.tin)}</td>
                  <td className="px-4 py-2 text-right font-mono tabular-nums">{nfmt(r.tout)}</td>
                  <td className="px-4 py-2 text-right font-mono tabular-nums text-mute">{nfmt(r.cread ?? 0)}</td>
                  <td className={cx('px-4 py-2 text-right font-mono tabular-nums',
                    (r.reasoning ?? 0) > 0 ? 'text-warn' : 'text-mute')}>
                    {nfmt(r.reasoning ?? 0)}
                  </td>
                  <td className="px-4 py-2 text-right font-mono tabular-nums text-mute">
                    {r.total_ms == null ? '—' : nfmt(r.total_ms)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>
    </div>
  );
}
