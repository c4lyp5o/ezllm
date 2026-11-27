// Neighbourhood page (everything around the scene): the one-line verdict, the
// details card only on selection with its links, the table (sort, search,
// state filter, links by provider ID), phone cards, ?select= deep links, the
// usage window, refresh, a failed load and its retry, the empty city, and full
// screen. The scene itself is covered by verify-neighbourhood.mjs.
import { start, json, check, finish, account } from './mockdash.mjs';

const key = (id, label, hint, o = {}) => ({ id, label, hint, enabled: true, last_test_ok: true, ...o });
const accounts = [
  { ...account, id: 1, name: 'Alpha Gateway', namespace: 'alpha', keys: [key(1, 'busy', 'a')] },
  { ...account, id: 2, name: 'Beta Works', namespace: 'beta', keys: [key(2, 'flaky', 'b', { last_test_ok: false })] },
  { ...account, id: 3, name: 'Gamma Cloud', namespace: 'gamma', keys: [key(3, 'idle', 'c')] },
  { ...account, id: 4, name: 'Delta Offline', namespace: 'delta', enabled: false, keys: [key(4, 'old', 'd')] },
];
const row = (k, calls, tin, errors = 0) => ({ k, calls, tin, tout: 0, errors, cread: 0, cwrite: 0, reasoning: 0, saved: 0, context_pre: 0, context_saved: 0, p50_ttft_ms: 0, applied_saved: 0 });
const usage = { groups: [row('alpha', 40, 120000), row('beta', 8, 5000, 4)], totals: {} };
let accountsStatus = 200, list = accounts;
const froms = [];
const handler = async (route, url) => {
  if (url.pathname === '/admin/accounts') { await json(route, accountsStatus === 200 ? list : { error: { message: 'down' } }, accountsStatus); return true; }
  if (url.pathname === '/admin/usage') {
    if (url.searchParams.get('group_by') === 'account') froms.push(Date.now() - Date.parse(url.searchParams.get('from')));
    await json(route, url.searchParams.get('group_by') === 'key' ? { groups: [], totals: {} } : usage); return true;
  }
  return false;
};
const t = await start();
const errors = [];
const open = async (path = '/neighbourhood', opts = {}) => {
  const page = await t.page(handler, opts);
  page.on('pageerror', (e) => errors.push(e.message));
  await page.addInitScript(() => { try { if (!sessionStorage.getItem('cleared')) { localStorage.clear(); sessionStorage.setItem('cleared', '1'); } } catch {} });
  await page.goto(t.base + path);
  return page;
};
const names = (page) => page.locator('tbody tr[data-row] td:first-child').allInnerTexts();

try {
  const page = await open();
  await page.locator('tbody tr[data-row]').first().waitFor();
  const summary = () => page.getByLabel('Neighbourhood summary').innerText();
  check('the verdict names the failing provider', (await summary()).includes('Beta Works is failing'), await summary());
  check('the summary gives traffic, calls and the failure share', /25,000\s*tokens \/ min/.test(await summary()) && /48\s*calls/.test(await summary()) && (await summary()).includes('8.3%'), await summary());
  check('the summary counts states', (await summary()).includes('1 serving · 1 quiet · 1 switched off'));
  check('no details card until something is selected', (await page.getByLabel('Selected building').count()) === 0);

  // table
  check('default order is busiest first', (await names(page))[0] === 'Alpha Gateway', await names(page));
  check('the failure rate is a column', (await page.locator('tr[data-row="2"]').innerText()).includes('50%'));
  check('state chips use plain words', (await page.locator('tr[data-row="2"]').innerText()).includes('failing') && (await page.locator('tr[data-row="4"]').innerText()).includes('switched off'));
  await page.getByRole('button', { name: 'Failure rate' }).click();
  check('sorting by failure rate lists the failing provider first', (await names(page))[0] === 'Beta Works', await names(page));
  await page.getByRole('button', { name: 'Failure rate' }).click();
  check('a second click reverses it', (await names(page))[0] !== 'Beta Works', await names(page));
  await page.getByRole('button', { name: 'Provider', exact: true }).click();
  check('sorting by name is alphabetical', (await names(page)).join() === 'Alpha Gateway,Beta Works,Delta Offline,Gamma Cloud', await names(page));
  await page.getByLabel('Search buildings').fill('gam');
  check('search filters the table', (await names(page)).join() === 'Gamma Cloud');
  await page.getByLabel('Search buildings').fill('');
  const filters = page.getByRole('group', { name: 'Show buildings' });
  check('the Failing filter counts failing providers', (await filters.getByRole('button', { name: 'Failing (1)' }).count()) === 1);
  await filters.getByRole('button', { name: 'Failing (1)' }).click();
  check('the Failing filter shows only failing buildings', (await names(page)).join() === 'Beta Works');
  await filters.getByRole('button', { name: 'Switched off' }).click();
  check('the Switched off filter works', (await names(page)).join() === 'Delta Offline');
  await filters.getByRole('button', { name: 'All' }).click();
  check('a table row links to its provider', (await page.locator('tr[data-row="1"]').getByRole('link', { name: 'Open Alpha Gateway' }).getAttribute('href')) === '/providers?open=1');
  check('a table row links to its requests by provider ID', (await page.locator('tr[data-row="1"]').getByRole('link', { name: 'Requests for Alpha Gateway' }).getAttribute('href')).includes('account=alpha&'));

  // details card
  await page.locator('tr[data-row="2"]').getByRole('button', { name: 'Beta Works' }).click();
  const card = page.getByLabel('Selected building');
  await card.getByText('Beta Works', { exact: true }).waitFor();
  const ct = await card.innerText();
  check('the card explains a failure', ct.includes('A quarter or more of the calls in the last five minutes failed.'));
  check('the card lists a health problem with a fix link', ct.includes('failed the last test') && (await card.getByRole('link', { name: 'Fix' }).getAttribute('href')) === '/providers?keys=2');
  check('the card gives exact figures', ct.includes('1,000') && ct.includes('4 · 50%'), ct);
  check('the card links to the provider, its requests and its keys',
    (await card.getByRole('link', { name: 'Open provider' }).getAttribute('href')) === '/providers?open=2'
    && (await card.getByRole('link', { name: 'View requests' }).getAttribute('href')).includes('account=beta&')
    && (await card.getByRole('link', { name: 'Edit keys' }).getAttribute('href')) === '/providers?keys=2');
  check('the selection is in the URL', page.url().includes('select=2'));
  await card.getByRole('button', { name: 'Close' }).click();
  check('Close hides the card and clears the URL', (await page.getByLabel('Selected building').count()) === 0 && !page.url().includes('select='));

  // window + refresh
  check('the default window is five minutes', Math.abs(froms.at(-1) - 5 * 60_000) < 5000, froms.at(-1));
  check('tokens per minute are over five minutes', (await page.locator('tr[data-row="1"]').innerText()).includes('24,000'));
  await page.getByRole('group', { name: 'Usage window' }).getByRole('button', { name: '1 hour' }).click();
  await page.waitForTimeout(400);
  check('switching the window refetches that look-back', Math.abs(froms.at(-1) - 3600_000) < 5000, froms.at(-1));
  check('a window change starts no bogus trend', !(await page.locator('tr[data-row="1"]').innerText()).match(/[▲▼]/));
  const reads = froms.length;
  await page.getByRole('button', { name: 'Refresh now' }).click();
  await page.waitForTimeout(400);
  check('Refresh now fetches again', froms.length > reads);
  await page.reload(); await page.locator('tbody tr[data-row]').first().waitFor();
  check('the window is remembered', (await page.getByRole('group', { name: 'Usage window' }).getByRole('button', { name: '1 hour' }).getAttribute('aria-pressed')) === 'true');

  // full screen
  if (await page.getByRole('button', { name: 'Full screen' }).count()) {
    await page.getByRole('button', { name: 'Full screen' }).click();
    await page.waitForTimeout(400);
    const fs = await page.evaluate(() => ({ el: !!document.fullscreenElement, ch: document.querySelector('.nbhd-scene').getBoundingClientRect().height, ih: innerHeight }));
    check('Full screen fills the display with the scene', fs.el && Math.abs(fs.ch - fs.ih) < 4, fs);
    await page.locator('.nbhd-exit-full').click();
    await page.waitForTimeout(300);
    check('Exit full screen returns to the page', await page.evaluate(() => !document.fullscreenElement));
  }
  await page.context().close();

  // deep links
  const deep = await open('/neighbourhood?select=3');
  await deep.locator('[data-building="3"]').waitFor();
  check('?select= preselects that building', (await deep.locator('[data-building="3"]').getAttribute('aria-pressed')) === 'true' && (await deep.getByLabel('Selected building').innerText()).includes('Gamma Cloud'));
  await deep.goto(`${t.base}/neighbourhood?select=999`);
  await deep.locator('[data-building]').first().waitFor();
  await deep.waitForTimeout(300);
  check('a select= naming no provider is dropped', !deep.url().includes('select='), deep.url());
  await deep.context().close();

  // phone: cards, not the table
  const ph = await open('/neighbourhood', { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
  await ph.locator('[data-card]').first().waitFor();
  check('phone: one card per building', (await ph.locator('[data-card]').count()) === 4 && !(await ph.locator('table').isVisible()));
  check('phone: a card carries the numbers and the state', (await ph.locator('[data-card="2"]').innerText()).includes('50%') && (await ph.locator('[data-card="2"]').innerText()).includes('failing'));
  check('phone: no sideways scroll', await ph.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth));
  await ph.context().close();

  // a failed load, then retry
  accountsStatus = 503;
  const bad = await open();
  await bad.getByText('Could not load the neighbourhood.').waitFor();
  check('a failed load says so and offers a retry', (await bad.getByRole('button', { name: 'Try again' }).count()) === 1);
  accountsStatus = 200;
  await bad.getByRole('button', { name: 'Try again' }).click();
  await bad.locator('[data-building]').first().waitFor();
  check('retry restores the city', (await bad.locator('[data-building]').count()) === 4);
  await bad.context().close();

  // no providers
  list = [];
  const empty = await open();
  await empty.getByText('No providers yet.').waitFor();
  check('an empty city points to adding a provider', (await empty.getByRole('link', { name: 'Add a provider' }).getAttribute('href')) === '/providers');
  await empty.context().close();
  list = accounts;

  check('no page errors', errors.length === 0, errors);
} catch (e) {
  check('driver ran to completion', false, String(e));
} finally {
  await t.close();
}
finish();
