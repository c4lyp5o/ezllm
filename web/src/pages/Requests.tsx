import { useRange } from '../utils/preferences';
// REQUESTS — the token dissection explorer.
// min/max filters on tokens in and tokens out over a time range, with summary
// totals over the WHOLE matching set (not just the page). Sort by either axis
// to surface the fattest calls. The question this page exists to answer: is
// the client sending too much history, or is the model burning it on output
// and reasoning? (row shape = the dashboard's recentCalls projection.)
import { Fragment, useCallback, useEffect, useRef, useState, type FormEvent } from 'react';
import { Link, useSearchParams } from 'react-router-dom';
import {
  normalizeFeedRow, requests, usage, RANGE_LABEL,
  type FeedRow, type RangeKey, type RequestSort, type RequestsResp,
} from '../utils/api';
import { CauseChip, CopyButton, SectionLabel, Segmented, btn, cx, inputCls, labelCls } from '../components/ui';
import { Stagger, StaggerItem } from '../components/motion';

const RANGES: RangeKey[] = ['24h', '7d', '30d', '365d'];
const SORTS: { key: RequestSort; label: string }[] = [
  { key: 'ts_desc', label: 'newest' },
  { key: 'tin_desc', label: 'tokens in' },
  { key: 'tout_desc', label: 'tokens out' },
];

// The server allows up to 500 rows a view; these are the steps offered.
const PAGE_SIZES = [50, 100, 250, 500] as const;
const BOUND_PARAM = { minTin: 'min_tin', maxTin: 'max_tin', minTout: 'min_tout', maxTout: 'max_tout' } as const;
const rowId = (r: FeedRow) => `${r.ts}|${r.account}|${r.model}|${r.status}|${r.total_ms}`;

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

// The shared control: the selection is the 1px accent line under its label.
function Seg<T extends string>(props: {
  options: { key: T; label: string }[]; value: T; onChange: (v: T) => void;
}) {
  return <Segmented {...props} />;
}

function Stat({ label, value, sub }: { label: string; value: string; sub?: string }) {
  return (
    <div className="rounded-xl border border-line bg-surface px-4 py-3.5">
      <SectionLabel>{label}</SectionLabel>
      <div className="mt-1.5 font-mono text-[20px] font-semibold tabular-nums tracking-tight text-ink">{value}</div>
      {sub && <div className="mt-0.5 text-[12px] text-mute">{sub}</div>}
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

function RowDetail({ r }: { r: FeedRow }) {
  const meta: [string, string][] = [
    ['time', r.ts], ['client', r.client || '–'], ['surface', r.surface || '–'],
    ['provider · combo', [r.account, r.alias].filter(Boolean).join(' · ') || '–'],
    ['caller address', r.ip || '–'],
    ['stream', r.stream ? `yes · first token ${r.ttft_ms ?? '–'} ms` : 'no'],
    ['compression', r.compression ? `${r.compression}${r.applied ? ' (applied)' : ' (no change)'}` : '–'],
  ];
  return (
    <div>
      <dl className="grid gap-x-8 gap-y-1.5 text-[12px] sm:grid-cols-2 lg:grid-cols-3">
        {meta.map(([k, v]) => (
          <div key={k} className="flex gap-2"><dt className="w-28 shrink-0 text-mute">{k}</dt><dd className="min-w-0 break-all text-dim">{v}</dd></div>
        ))}
      </dl>
      {r.error && (
        <div className="mt-3">
          <div className="text-[12px] text-mute">error</div>
          <pre className="font-code mt-1 max-h-40 overflow-auto whitespace-pre-wrap break-words rounded bg-shade px-3 py-2 text-[12px] text-dim">{r.error}</pre>
        </div>
      )}
      <div className="mt-3 flex flex-wrap items-center gap-2">
        {r.error && <CopyButton text={r.error} label="Copy error" />}
        <CopyButton text={JSON.stringify(r, null, 2)} label="Copy row as JSON" />
        {(r.account || r.model) && (
          <Link to={`/requests?${new URLSearchParams({ account: r.account, model: r.model })}`}
            className={cx(btn.base, btn.ghost, '!h-7 !px-3 !text-[12px]')}>Only this provider and model</Link>
        )}
      </div>
    </div>
  );
}

export default function Requests() {
  // Every filter lives in the URL, so a view survives a refresh and can be
  // shared: ?account= / ?model= / ?alias= (a combo) / ?client= (an API key) are exact ledger names (server-side, so the
  // summary strip covers the filtered set), plus range, sort, limit and the
  // four token bounds. The Dashboard and Analytics click-throughs use the
  // same parameters.
  const [params, setParams] = useSearchParams();
  const [prefRange, setPrefRange] = useRange();
  const account = params.get('account') ?? '';
  const model = params.get('model') ?? '';
  const alias = params.get('alias') ?? '';
  const client = params.get('client') ?? '';
  const urlRange = params.get('range');
  const range: RangeKey = (RANGES as string[]).includes(urlRange ?? '') ? urlRange as RangeKey : prefRange;
  const urlSort = params.get('sort');
  const sort: RequestSort = SORTS.some((x) => x.key === urlSort) ? urlSort as RequestSort : 'ts_desc';
  const urlLimit = Number(params.get('limit'));
  const limit = (PAGE_SIZES as readonly number[]).includes(urlLimit) ? urlLimit : PAGE_SIZES[0];
  const applied: Applied = {};
  for (const key of Object.keys(BOUND_PARAM) as (keyof NumFields)[]) {
    const v = params.get(BOUND_PARAM[key]);
    if (v != null && v !== '' && Number.isInteger(Number(v)) && Number(v) >= 0) applied[key] = Number(v);
  }
  const appliedKey = JSON.stringify(applied);

  const patch = (changes: Record<string, string | undefined>) => setParams((p) => {
    const n = new URLSearchParams(p);
    for (const [k, v] of Object.entries(changes)) { if (v == null || v === '') n.delete(k); else n.set(k, v); }
    return n;
  }, { replace: true });
  const setRange = (r: RangeKey) => { patch({ range: r }); setPrefRange(r); };

  const [fields, setFields] = useState<NumFields>(EMPTY_FIELDS);
  useEffect(() => {
    const a2 = JSON.parse(appliedKey) as Applied;
    setFields({
      minTin: String(a2.minTin ?? ''), maxTin: String(a2.maxTin ?? ''),
      minTout: String(a2.minTout ?? ''), maxTout: String(a2.maxTout ?? ''),
    });
  }, [appliedKey]);
  const [data, setData] = useState<RequestsResp | null>(null);
  const [rows, setRows] = useState<FeedRow[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);
  const [open, setOpen] = useState<string | null>(null);
  // Options for the quick-filter dropdowns: the names that actually carry
  // traffic in the window (one aggregate each, not a scan of rows).
  const [names, setNames] = useState<{ accounts: string[]; models: string[]; combos: string[]; clients: string[] }>({ accounts: [], models: [], combos: [], clients: [] });
  useEffect(() => {
    let live = true;
    void Promise.all([usage('account', range).catch(() => null), usage('model', range).catch(() => null), usage('alias', range).catch(() => null), usage('client', range).catch(() => null)]).then(([a, m, c, k]) => {
      if (live) setNames({
        accounts: (a?.groups ?? []).map((g) => g.k).sort(), models: (m?.groups ?? []).map((g) => g.k).sort(),
        combos: (c?.groups ?? []).map((g) => g.k).filter((k) => k !== '(direct)').sort(),
        clients: (k?.groups ?? []).map((g) => g.k).sort(),
      });
    });
    return () => { live = false; };
  }, [range]);

  // Only the newest load may write state: an older selection's response that
  // lands late is dropped instead of overwriting the current one.
  const loadGen = useRef(0);
  const load = useCallback(async () => {
    const gen = ++loadGen.current;
    setLoading(true);
    setErr(null);
    try {
      const r = await requests({ ...JSON.parse(appliedKey) as Applied, range, sort, account, model, alias, client, limit });
      if (gen !== loadGen.current) return;
      setData(r);
      setRows(r.rows.map(normalizeFeedRow));
      setOpen(null);
    } catch (e) {
      if (gen === loadGen.current) setErr(e instanceof Error ? e.message : String(e));
    } finally {
      if (gen === loadGen.current) setLoading(false);
    }
  }, [appliedKey, range, sort, account, model, alias, client, limit]);

  useEffect(() => { load(); }, [load]);

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const next: Record<string, string | undefined> = {};
    for (const key of Object.keys(fields) as (keyof NumFields)[]) {
      const v = parseField(fields[key]);
      if (v === 'bad') {
        setErr(`${key} must be a non-negative whole number`);
        return;
      }
      next[BOUND_PARAM[key]] = v === undefined ? undefined : String(v);
    }
    setErr(null);
    patch(next);
  };

  const clear = () => { setFields(EMPTY_FIELDS); setParams({}, { replace: true }); };
  const matchCount = data?.summary.count ?? 0;
  const canMore = rows.length < matchCount && limit < PAGE_SIZES[PAGE_SIZES.length - 1];
  const withCurrent = (list: string[], cur: string) => (cur && !list.includes(cur) ? [cur, ...list] : list);
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
        {/* Quick filters: names that carried traffic in this window. */}
        <div className="mt-4 flex flex-wrap items-end gap-3">
          <label className="block">
            <span className={labelCls}>provider</span>
            <select className={cx(inputCls, 'w-56')} value={account} onChange={(e) => patch({ account: e.target.value })}>
              <option value="">All providers</option>
              {withCurrent(names.accounts, account).map((n) => <option key={n} value={n}>{n}</option>)}
            </select>
          </label>
          <label className="block">
            <span className={labelCls}>combo</span>
            <select className={cx(inputCls, 'w-48')} value={alias} onChange={(e) => patch({ alias: e.target.value })}>
              <option value="">All combos</option>
              {withCurrent(names.combos, alias).map((n) => <option key={n} value={n}>{n}</option>)}
            </select>
          </label>
          <label className="block">
            <span className={labelCls}>API key</span>
            <select className={cx(inputCls, 'w-48')} value={client} onChange={(e) => patch({ client: e.target.value })}>
              <option value="">All keys</option>
              {withCurrent(names.clients, client).map((n) => <option key={n} value={n}>{n}</option>)}
            </select>
          </label>
          <label className="block">
            <span className={labelCls}>model</span>
            <select className={cx(inputCls, 'w-72 max-w-full')} value={model} onChange={(e) => patch({ model: e.target.value })}>
              <option value="">All models</option>
              {withCurrent(names.models, model).map((n) => <option key={n} value={n}>{n}</option>)}
            </select>
          </label>
        </div>
        <div className="mt-4 flex flex-wrap items-center gap-3">
          <SectionLabel>range</SectionLabel>
          <Seg
            options={RANGES.map((k) => ({ key: k, label: RANGE_LABEL[k] }))}
            value={range} onChange={setRange}
          />
          <SectionLabel>order</SectionLabel>
          <Seg options={SORTS} value={sort} onChange={(v) => patch({ sort: v === 'ts_desc' ? undefined : v })} />
          <SectionLabel>rows</SectionLabel>
          <Seg options={PAGE_SIZES.map((n) => ({ key: String(n), label: String(n) }))} value={String(limit)}
            onChange={(v) => patch({ limit: v === String(PAGE_SIZES[0]) ? undefined : v })} />
        </div>
      </form>

      {/* Summary strip — over the WHOLE matching set, not the returned page. */}
      {s && (
        <Stagger className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
          <StaggerItem>
            <Stat label="requests" value={nfmt(s.count)} sub="matching the filter" />
          </StaggerItem>
          <StaggerItem>
            <Stat label="tokens in" value={nfmt(s.tin)} sub="Σ across matches" />
          </StaggerItem>
          <StaggerItem>
            <Stat label="tokens out" value={nfmt(s.tout)} sub="Σ across matches" />
          </StaggerItem>
          <StaggerItem>
            <Stat label="max in" value={nfmt(s.max_tin)} sub={`max out ${nfmt(s.max_tout)}`} />
          </StaggerItem>
          <StaggerItem>
            <Stat label="reasoning" value={nfmt(s.reasoning)} sub="Σ burned on thinking" />
          </StaggerItem>
        </Stagger>
      )}

      {/* Result rows */}
      <div className="rounded-xl border border-line bg-surface">
        {err && <div className="border-b border-line px-4 py-2.5 text-[13px] text-bad">{err}</div>}
        <div className="overflow-x-auto">
          <table className="w-full text-[13px]">
            <thead>
              <tr className="text-left text-mute">
                <th className="px-4 py-2 font-medium">time</th>
                <th className="px-4 py-2 font-medium">model</th>
                <th className="px-4 py-2 font-medium">client</th>
                <th className="px-4 py-2 text-right font-medium">status</th>
                <th className="px-4 py-2 text-right font-medium" title="tokens in ↑ / tokens out ↓">tokens ↑↓</th>
                <th className="px-4 py-2 text-right font-medium">cached</th>
                <th className="px-4 py-2 text-right font-medium">reasoning</th>
                <th className="px-4 py-2 text-right font-medium">saved</th>
                <th className="px-4 py-2 text-right font-medium">rules</th>
                <th className="px-4 py-2 text-right font-medium">ms</th>
              </tr>
            </thead>
            <tbody>
              {rows.length === 0 && !loading && (
                <tr>
                  <td colSpan={11} className="px-4 py-6 text-center text-mute">
                    No calls match this filter.
                  </td>
                </tr>
              )}
              {rows.map((r, i) => {
                const id = rowId(r);
                const isOpen = open === id;
                return (
                <Fragment key={`${id}#${i}`}>
                <tr tabIndex={0} aria-expanded={isOpen} onClick={() => setOpen(isOpen ? null : id)}
                  onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); setOpen(isOpen ? null : id); } }}
                  className={cx('cursor-pointer border-t border-line transition-colors hover:bg-wash focus-visible:bg-wash focus-visible:outline-none', isOpen && 'bg-wash')}>
                  <td className="whitespace-nowrap px-4 py-2 font-mono text-mute">
                    {fmtTs(r.ts)}
                    {r.ip && <div className="text-[11px] text-mute/70" title="caller address">{r.ip}</div>}
                  </td>
                  <td className="px-4 py-2 font-mono">
                    {r.model}
                    {/* Identity under the model, same shape as the dashboard's
                        recent-calls cell: who served it, and through which
                        combo when it rode one. Direct routes carry no alias,
                        so the line is provider-only there. */}
                    {(r.account || r.alias) && (
                      <div className="mt-0.5 truncate font-sans text-[11px] text-mute"
                        title={[r.account, r.alias].filter(Boolean).join(' · ')}>
                        {r.account}{r.alias ? ` · ${r.alias}` : ''}
                      </div>
                    )}
                    {/* Wire-translated hop: its in-tokens are the vendor's
                        INCLUSIVE total — read before comparing tin. */}
                    {r.translated && (
                      <div className="text-[11px] text-accent"
                        title="translated hop: its wire differs from the client's surface (anthropic client -> chat-only hop, or a hop curated for /v1/responses). The in-tokens on this row are the vendor's INCLUSIVE total (cache counted inside), not ezllm's cold-token convention">
                        ⇄ translated
                      </div>
                    )}
                  </td>
                  <td className="px-4 py-2">{r.client}</td>
                  <td className={cx('px-4 py-2 text-right font-mono', r.status >= 400 ? 'text-bad' : 'text-ink')}>
                    <div>{r.status}</div>
                    <CauseChip err={r.error} status={r.status} />
                  </td>
                  <td className="px-4 py-2 text-right font-mono tabular-nums"
                    title={`tokens in ${nfmt(r.tin)} (↑) · tokens out ${nfmt(r.tout)} (↓)`}>
                    <span title="tokens in — prompt, cache counted inside">↑{nfmt(r.tin)}</span>{' '}
                    <span className="text-mute" title="tokens out — completion">↓{nfmt(r.tout)}</span>
                  </td>
                  <td className="px-4 py-2 text-right font-mono tabular-nums text-mute">{nfmt(r.cread ?? 0)}</td>
                  <td className={cx('px-4 py-2 text-right font-mono tabular-nums',
                    (r.reasoning ?? 0) > 0 ? 'text-warn' : 'text-mute')}>
                    {nfmt(r.reasoning ?? 0)}
                  </td>
                  <td className={cx('px-4 py-2 text-right font-mono tabular-nums',
                    r.saved > 0 ? 'text-ok' : 'text-mute')}>
                    {r.saved > 0 ? nfmt(r.saved) : '—'}
                  </td>
                  {/* caveman attribution: non-zero only when prose rules fired */}
                  <td className={cx('px-4 py-2 text-right font-mono tabular-nums',
                    (r.rulesFired ?? 0) > 0 ? 'text-warn' : 'text-mute')}>
                    {(r.rulesFired ?? 0) > 0 ? nfmt(r.rulesFired ?? 0) : '—'}
                  </td>
                  <td className="px-4 py-2 text-right font-mono tabular-nums text-mute">
                    {r.total_ms == null ? '—' : nfmt(r.total_ms)}
                  </td>
                </tr>
                {isOpen && (
                  <tr className="border-t border-line bg-wash">
                    <td colSpan={10} className="px-4 py-3"><RowDetail r={r} /></td>
                  </tr>
                )}
                </Fragment>
                );
              })}
            </tbody>
          </table>
        </div>
        {rows.length > 0 && (
          <div className="flex flex-wrap items-center justify-between gap-3 border-t border-line px-4 py-3 text-[12px] text-mute">
            <span className="tnum">showing {nfmt(rows.length)} of {nfmt(matchCount)} matching</span>
            {canMore ? (
              <button type="button" onClick={() => patch({ limit: String(PAGE_SIZES.find((n) => n > limit) ?? limit) })}
                className={cx(btn.base, btn.ghost, '!h-7 !px-3 !text-[12px]')}>
                Show {nfmt(Math.min(matchCount, PAGE_SIZES.find((n) => n > limit) ?? limit))}
              </button>
            ) : rows.length < matchCount ? (
              <span>capped at {nfmt(PAGE_SIZES[PAGE_SIZES.length - 1])} rows a view; narrow the range or filters to see the rest</span>
            ) : null}
          </div>
        )}
      </div>
    </div>
  );
}
