// Analytics tabs QoL: shared range, Stats (deltas, day trend, sortable table,
// links), Economics (unpriced list, run-rate, form submit, richer table) and
// Requests (URL state, quick filters, paging, row detail).
// Runs against the built web/dist with fixtures (see mockdash.mjs).
import { start, json, check, finish } from './mockdash.mjs';

const t = await start();
const row = (k, o = {}) => ({
  k, calls: 0, tin: 0, tout: 0, cread: 0, cwrite: 0, reasoning: 0, saved: 0, errors: 0,
  context_pre: 0, context_saved: 0, p50_ttft_ms: 0, applied_saved: 0, ...o,
});
const seen = { usage: [], requests: [] };
const feed = (n, o = {}) => Array.from({ length: n }, (_, i) => ({
  ts: new Date(Date.now() - i * 60_000).toISOString(), client: 'hermes', surface: 'chat', alias: 'main',
  account: 'Mock provider', model: 'model-a', status: i === 0 ? 500 : 200, tokens_in: 1000 - i, tokens_out: 10,
  error: i === 0 ? 'upstream 500 [server_error]: kaboom' : undefined, ...o,
}));
const summary = { count: 120, tin: 1, tout: 1, cread: 0, cwrite: 0, reasoning: 0, max_tin: 1, max_tout: 1, context_pre: 0, context_saved: 0, compression_saved: 0, applied_calls: 0, applied_saved: 0, declined_calls: 0, held_notional: 0 };

const page = await t.page(async (route, url) => {
  const q = url.searchParams;
  if (url.pathname === '/admin/usage') {
    const hours = Math.round((Date.now() - Date.parse(q.get('from'))) / 3_600_000);
    seen.usage.push({ hours, group: q.get('group_by') });
    const totals = row('all', { calls: hours * 10, tin: hours * 100, tout: hours * 20, errors: 1 });
    let groups = [];
    if (q.get('group_by') === 'account') groups = [row('Alpha', { calls: 5, tin: 50, errors: 1 }), row('Beta', { calls: 50, tin: 500, cread: 200 })];
    if (q.get('group_by') === 'model') groups = [row('model-a', { calls: 3 }), row('model-b', { calls: 2 })];
    if (q.get('group_by') === 'day') groups = [row('2026-10-27', { tin: 10 }), row('2026-10-28', { tin: 30 })];
    await json(route, { groups, totals, group_by: q.get('group_by') ?? '' });
    return true;
  }
  if (url.pathname === '/admin/requests') {
    const limit = Number(q.get('limit') || 50);
    seen.requests.push(url.search);
    await json(route, { rows: feed(Math.min(limit, 120)), summary, filter: {} });
    return true;
  }
  if (url.pathname === '/admin/cost') {
    await json(route, {
      from: '', to: '', currency: 'USD',
      rows: [
        { day: '2026-10-27', account_id: 1, account: 'Alpha', model: 'priced-m', calls: 10, unpriced_calls: 0, tokens_in: 1, tokens_out: 1, tokens_cached_read: 0, tokens_cached_write: 0, usd: 3 },
        { day: '2026-10-28', account_id: 2, account: 'Beta', model: 'free-m', calls: 7, unpriced_calls: 7, tokens_in: 1, tokens_out: 1, tokens_cached_read: 0, tokens_cached_write: 0, usd: 0 },
      ],
      totals: { usd: 3, calls: 17, unpriced_calls: 7, pricing_complete: false },
    });
    return true;
  }
  return false;
});
const tab = (name) => page.getByRole('navigation', { name: 'Analytics views' }).getByRole('link', { name });
const pressed = (name) => page.getByRole('button', { name, exact: true }).first().getAttribute('aria-pressed');

try {
  // ── shared range ──
  await page.goto(`${t.base}/stats`);
  await page.getByRole('button', { name: '30d', exact: true }).first().click();
  await tab('Token Economics').click();
  await page.getByText('Large-request filter').waitFor();
  check('range survives a tab switch (Economics)', (await pressed('30d')) === 'true');
  await tab('Requests').click();
  await page.getByText('dissect').waitFor();
  check('range survives a tab switch (Requests)', (await pressed('30d')) === 'true');

  // ── Stats ──
  await tab('Stats').click();
  await page.getByText('Alpha').first().waitFor();
  await page.getByText(/same as the previous 30 days/).first().waitFor({ timeout: 5000 }).catch(() => {});
  check('stats: prior-period delta on the totals', (await page.getByText(/same as the previous 30 days/).count()) > 0);
  check('stats: 2x window requested for the comparison', seen.usage.some((u) => u.hours === 1440 && !u.group), seen.usage.slice(-4));
  const calls = page.getByRole('button', { name: /^calls/ });
  await calls.click();
  check('stats: header click sorts descending', (await page.getByRole('columnheader', { name: /calls/ }).getAttribute('aria-sort')) === 'descending');
  const first = async () => (await page.locator('tbody tr').first().innerText()).split('\n')[0];
  check('stats: biggest first after sort', (await first()).startsWith('Beta'), await first());
  await calls.click();
  check('stats: second click flips to ascending', (await first()).startsWith('Alpha'), await first());
  const link = page.getByRole('link', { name: 'Beta' }).first();
  check('stats: account name links to Requests', /\/requests\?account=Beta/.test((await link.getAttribute('href')) ?? ''), await link.getAttribute('href'));
  check('stats: derived columns present', (await page.getByRole('columnheader', { name: /cache hit/ }).count()) === 1);
  await page.getByRole('button', { name: 'Everything', exact: true }).click();
  await page.getByText(/per day/).first().waitFor();
  check('stats: Everything view draws a per-day trend', (await page.getByText('2026-10-27').count()) > 0);

  // ── Economics ──
  await tab('Token Economics').click();
  await page.getByText('Counted as $0 until priced').waitFor();
  check('economics: unpriced models are named', (await page.getByText('Beta · free-m').count()) > 0);
  check('economics: run-rate shown', (await page.getByText(/over 30 days at this rate/).count()) > 0);
  seen.requests.length = 0;
  await page.getByText(/top \d+ of 120/).waitFor();
  const minReq = page.waitForRequest((r) => /min_tin=5000/.test(r.url()), { timeout: 5000 }).catch(() => null);
  // Pages remount once shortly after navigation (pre-existing); let it settle before typing.
  await page.waitForTimeout(700);
  await page.getByPlaceholder('all').fill('5000');
  await page.getByPlaceholder('all').press('Enter');
  await minReq;
  check('economics: Enter submits the filter', seen.requests.some((u) => /min_tin=5000/.test(u)), seen.requests);
  check('economics: largest table has time and client columns', (await page.getByRole('columnheader', { name: 'When' }).count()) === 1 && (await page.getByRole('columnheader', { name: 'Client' }).count()) === 1);
  check('economics: says top N of M', (await page.getByText(/top 100 of 120/).count()) > 0);

  // ── Requests ──
  await page.goto(`${t.base}/requests`);
  await page.getByText(/showing 50 of 120/).waitFor();
  seen.requests.length = 0;
  await page.getByRole('button', { name: 'Show 100' }).click();
  await page.getByText(/showing 100 of 120/).waitFor();
  check('requests: show more raises the limit', seen.requests.some((u) => /limit=100/.test(u)), seen.requests);
  check('requests: limit lives in the URL', /limit=100/.test(page.url()), page.url());
  await page.getByLabel('provider').selectOption('Alpha');
  await page.waitForTimeout(300);
  check('requests: provider dropdown filters server-side', seen.requests.some((u) => /account=Alpha/.test(u)) && /account=Alpha/.test(page.url()), page.url());
  await page.reload();
  await page.getByText(/showing/).waitFor();
  check('requests: filters survive a reload', (await page.getByLabel('provider').inputValue()) === 'Alpha');
  await page.locator('tbody tr').first().click();
  await page.getByText('kaboom').waitFor();
  check('requests: row expands to the full error', (await page.getByRole('button', { name: 'Copy error' }).count()) === 1);
} catch (e) {
  check('driver ran to completion', false, String(e));
} finally {
  await t.close();
}
finish();
