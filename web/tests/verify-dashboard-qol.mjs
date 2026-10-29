// Dashboard QoL: range switch + prior-period delta, needs-attention strip,
// recent-call filter / pause / expand, breakdown click-through to Requests.
// Runs against the built web/dist with fixtures (see mockdash.mjs).
import { start, json, account, check, finish } from './mockdash.mjs';

const t = await start();
const row = (k, o = {}) => ({
  k, calls: 0, tin: 0, tout: 0, cread: 0, cwrite: 0, reasoning: 0, saved: 0, errors: 0,
  context_pre: 0, context_saved: 0, p50_ttft_ms: 0, applied_saved: 0, ...o,
});
const now = Date.now();
const call = (minsAgo, o = {}) => ({
  ts: new Date(now - minsAgo * 60_000).toISOString(), client: 'hermes', surface: 'chat', account: 'Mock provider',
  model: 'model-a', status: 200, ttft_ms: 120, total_ms: 900, tin: 100, tout: 20, cread: 0, saved: 0,
  compression: '', applied: false, alias: 'main', ...o,
});
const recent = [
  call(1), call(2), call(3, { status: 502, err: 'upstream 502 [bad_gateway]: {"error":{"message":"boom"}}' }),
  call(4, { status: 429, err: 'upstream 429 [rate_limited]: slow down' }),
  call(5, { err: 'failover: mock/a: 503; failover: mock/b: timeout' }),
  call(6, { err: 'failover: mock/a: 503' }), call(7, { err: 'failover: mock/a: 503' }),
];
const overview = {
  health: { status: 'ok', version: 'test', schema_version: 1, accounts: 2, provider_keys: 2, models: 2, combos: 1, ledger_rows: 10, dropped_rows: 0, uptime_s: 100 },
  usage: [row('Mock provider', { calls: 100, tin: 1000, tout: 200, errors: 2 })],
  usage_by_surface: [row('chat', { calls: 100 })],
  accounts: [
    account,
    { ...account, id: 2, name: 'Dead end', keys: [{ id: 9, label: 'k', hint: 'h', enabled: false }] },
  ],
  recent_calls: recent,
  block_cursors: [],
};
const seenFrom = [];
const page = await t.page(async (route, url) => {
  if (url.pathname === '/admin/overview') { await json(route, overview); return true; }
  if (url.pathname === '/admin/usage') {
    const hours = Math.round((Date.now() - Date.parse(url.searchParams.get('from'))) / 3_600_000);
    seenFrom.push(hours);
    const big = hours > 30 && hours < 100 ? 2 : 1; // the 48h window holds twice the 24h one
    const g = url.searchParams.get('group_by');
    const r = row(g ? 'Mock provider' : 'all', { calls: 100 * big, tin: 1000 * big, tout: 200 * big, errors: 2 * big });
    await json(route, { groups: g ? [r] : [], totals: r });
    return true;
  }
  return false;
});

try {
  await page.goto(`${t.base}/`);
  await page.getByText('Needs attention').waitFor();
  const strip = page.getByRole('region', { name: 'Needs attention' });
  const text = await strip.innerText();
  check('attention: all-keys-disabled account is flagged', /Dead end: every key is disabled/.test(text), text);
  check('attention: error rate over the last 15 minutes', /2 of 7 calls in the last 15 minutes failed \(29%\)/.test(text), text);
  check('attention: repeated failover is flagged', /main failed over 3 times/.test(text), text);
  check('attention deep-links to the provider', (await strip.getByRole('link', { name: 'Open provider' }).getAttribute('href')) === '/providers?open=2');

  await page.getByText('calls/min').or(page.getByText('live feed reconnecting')).first().waitFor();
  check('header shows the live pulse chip', true);
  await page.getByText(/^updated /).waitFor();
  check('header shows data freshness', true);

  // Prior-period delta: 24h holds 100 calls, the 2x window 200, so previous = 100.
  await page.getByText(/same as the previous 24 hours/).first().waitFor();
  check('delta vs previous period renders', true);

  // Range switch refetches with a longer look-back and relabels the window.
  seenFrom.length = 0;
  await page.getByRole('button', { name: '7d', exact: true }).first().click();
  await page.getByText('Showing the last 7 days').waitFor();
  await page.waitForTimeout(400);
  check('range switch fetches the 7d window', seenFrom.some((h) => h === 168), seenFrom);
  check('range switch fetches the 2x window for the delta', seenFrom.some((h) => h === 336), seenFrom);
  await page.getByRole('button', { name: '1y', exact: true }).click();
  await page.getByText('Showing the last 365 days').waitFor();
  await page.waitForTimeout(400);
  check('1y skips the over-cap 2x window', !seenFrom.some((h) => h >= 8760 * 2 - 24), seenFrom.slice(-6));
  await page.getByRole('button', { name: '24h', exact: true }).first().click();

  // Recent calls: filter chips and search narrow the shown rows.
  const body = page.locator('tbody').last();
  await page.getByRole('button', { name: /^Errors 2/ }).click();
  check('errors filter keeps only status>=400', (await body.locator('tr').count()) === 2);
  await page.getByRole('button', { name: /^Failed over 3/ }).click();
  check('failover filter keeps the hop-trail rows', (await body.locator('tr').count()) === 3);
  await page.getByRole('button', { name: /^All 7/ }).click();
  await page.getByLabel('Search recent calls').fill('nomatch');
  await page.getByText('No recent call matches.').waitFor();
  check('search with no hit says so', true);
  await page.getByLabel('Search recent calls').fill('');

  // Expand: the full error, the trail and the copy buttons show; rows pause.
  await body.locator('tr').nth(2).click();
  await page.getByText('upstream error').waitFor();
  check('expanded row shows the upstream error', (await page.locator('pre').first().innerText()).includes('boom'));
  check('feed is paused while a row is open', await page.getByText('paused').first().isVisible());
  check('copy buttons present', (await page.getByRole('button', { name: 'Copy error' }).count()) === 1);
  await body.locator('tr').nth(2).click();
  check('collapsing resumes the live feed', (await page.getByText('upstream error').count()) === 0);

  // Click-through: provider name opens Requests filtered server-side.
  let reqUrl = null;
  await page.route('**/admin/requests?*', async (route) => { reqUrl = route.request().url(); await json(route, { rows: [], summary: {}, filter: {} }); });
  await page.getByRole('link', { name: 'Mock provider' }).first().click();
  await page.waitForURL(/\/requests\?/);
  await page.waitForTimeout(400);
  check('provider link carries account + range', /account=Mock\+provider/.test(page.url()) && /range=24h/.test(page.url()), page.url());
  check('requests call is filtered server-side', !!reqUrl && /account=Mock\+provider/.test(reqUrl), reqUrl);
} catch (e) {
  check('driver ran to completion', false, String(e));
} finally {
  await t.close();
}
finish();
