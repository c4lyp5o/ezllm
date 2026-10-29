// Neighbourhood QoL (everything around the scene): sortable and filterable
// table with error rates, inspector links and health, summary, remembered view
// choices, ?select= deep link, usage window, refresh, the collapsed legend and
// full screen. The scene itself is covered by verify-neighbourhood*.
import { start, json, check, finish, account } from './mockdash.mjs';

const key = (id, label, hint, o = {}) => ({ id, label, hint, enabled: true, last_test_ok: true, ...o });
const accounts = [
  { ...account, id: 1, name: 'Alpha Gateway', namespace: 'alpha', keys: [key(1, 'busy', 'a')] },
  { ...account, id: 2, name: 'Beta Works', namespace: 'beta', keys: [key(2, 'flaky', 'b', { last_test_ok: false })] },
  { ...account, id: 3, name: 'Gamma Cloud', namespace: 'gamma', keys: [key(3, 'idle', 'c')] },
  { ...account, id: 4, name: 'Delta Offline', namespace: 'delta', enabled: false, keys: [key(4, 'old', 'd')] },
];
const row = (k, calls, tin, errors = 0) => ({ k, calls, tin, tout: 0, errors, cread: 0, cwrite: 0, reasoning: 0, saved: 0, context_pre: 0, context_saved: 0, p50_ttft_ms: 0, applied_saved: 0 });
const usage = { groups: [row('a', 40, 120000), row('b', 8, 5000, 4)], totals: {} };
const froms = [];
const t = await start();
const page = await t.page(async (route, url) => {
  if (url.pathname === '/admin/accounts') { await json(route, accounts); return true; }
  if (url.pathname === '/admin/usage') { froms.push(Date.now() - Date.parse(url.searchParams.get('from'))); await json(route, usage); return true; }
  return false;
});
const sortBtn = (label) => page.locator('th button', { hasText: new RegExp(`^${label}`) });
const names = async () => page.locator('tbody tr[data-row] td:first-child button').allTextContents();

try {
  await page.goto(`${t.base}/neighbourhood`);
  await page.locator('tbody tr[data-row]').first().waitFor();
  await page.waitForTimeout(600); // lazy pages mount twice ~100ms after navigation (pre-existing); let it settle
  check('summary calls out stormy providers', (await page.getByLabel('Neighbourhood summary').innerText()).includes('1 stormy'));
  check('summary gives the overall failure share', (await page.getByLabel('Neighbourhood summary').innerText()).includes('8.3% of calls failed'), await page.getByLabel('Neighbourhood summary').innerText());
  check('default order is busiest first', (await names())[0] === 'Alpha Gateway', await names());
  check('error rate is a column', (await page.locator('tr[data-row="2"]').innerText()).includes('50%'));

  // sort
  await sortBtn('Calls').click();
  check('sorting by calls puts the most calls first and marks the column', (await names())[0] === 'Alpha Gateway' && (await page.locator('th[aria-sort="descending"]').count()) === 1);
  await sortBtn('Calls').click();
  check('a second click reverses it', (await names())[0] !== 'Alpha Gateway' && (await page.locator('th[aria-sort="ascending"]').count()) === 1);
  await sortBtn('Error rate').click();
  check('sorting by error rate lists the failing provider first', (await names())[0] === 'Beta Works', await names());
  await sortBtn('Provider').click();
  check('sorting by name is alphabetical', (await names()).join() === 'Alpha Gateway,Beta Works,Delta Offline,Gamma Cloud', await names());

  // hovering a row must not move the table (the inspector above it keeps its height)
  const y0 = await page.locator('tr[data-row="1"]').evaluate((e) => Math.round(e.getBoundingClientRect().y));
  await page.locator('tr[data-row="2"]').hover();
  await page.waitForTimeout(250);
  const y1 = await page.locator('tr[data-row="1"]').evaluate((e) => Math.round(e.getBoundingClientRect().y));
  check('hovering a table row does not move the table', y0 === y1, { y0, y1 });

  // search + filters
  await page.getByLabel('Search buildings').fill('gamma');
  check('search filters the table', (await names()).join() === 'Gamma Cloud');
  await page.getByLabel('Search buildings').fill('zzz');
  await page.getByText('No building matches.').waitFor();
  await page.getByRole('button', { name: 'Clear search and filter' }).click();
  await page.getByRole('group', { name: 'Show buildings' }).getByRole('button', { name: /^Stormy/ }).click();
  check('the Stormy filter shows only stormy buildings', (await names()).join() === 'Beta Works');
  await page.getByRole('group', { name: 'Show buildings' }).getByRole('button', { name: 'Disabled' }).click();
  check('the Disabled filter works', (await names()).join() === 'Delta Offline');
  await page.getByRole('group', { name: 'Show buildings' }).getByRole('button', { name: 'All' }).click();

  // inspector
  await page.locator('tr[data-row="2"] button').first().click();
  const insp = page.getByLabel('Selected building');
  await insp.getByText('Beta Works', { exact: true }).waitFor();
  await page.waitForTimeout(100);
  check('the inspector explains a storm', (await insp.innerText()).includes('A quarter or more of the calls in the last five minutes failed.'));
  check('the inspector lists the provider’s health problem with a fix link', (await insp.innerText()).includes('failed the last test') && (await insp.getByRole('link', { name: 'Fix' }).getAttribute('href')) === '/providers?keys=2');
  check('the inspector shows the error rate', (await insp.innerText()).includes('4 errors · 50%'));
  check('the inspector links to the provider, its requests and its keys',
    (await insp.getByRole('link', { name: 'Open provider' }).getAttribute('href')) === '/providers?open=2'
    && (await insp.getByRole('link', { name: 'View requests' }).getAttribute('href')).includes('account=Beta+Works')
    && (await insp.getByRole('link', { name: 'Edit keys' }).getAttribute('href')) === '/providers?keys=2');
  check('the selection is in the URL', page.url().includes('select=2'));
  check('table rows link out too', (await page.locator('tr[data-row="1"]').getByRole('link', { name: 'Open Alpha Gateway' }).getAttribute('href')) === '/providers?open=1');

  // deep link
  await page.goto(`${t.base}/neighbourhood?select=3`);
  await page.locator('tbody tr[data-row]').first().waitFor();
  check('?select= preselects that building', (await page.locator('[data-building="3"]').getAttribute('aria-pressed')) === 'true');
  await page.goto(`${t.base}/neighbourhood?select=999`);
  await page.locator('tbody tr[data-row]').first().waitFor();
  await page.waitForTimeout(300);
  check('a select= naming no provider is dropped', !page.url().includes('select='), page.url());

  // window + refresh + remembered choices
  await page.goto(`${t.base}/neighbourhood`);
  await page.locator('tbody tr[data-row]').first().waitFor();
  check('the default window is five minutes', Math.abs(froms.at(-1) - 5 * 60_000) < 5000, froms.at(-1));
  check('five-minute tokens per minute', (await page.locator('tr[data-row="1"]').innerText()).includes('24,000'));
  await page.getByRole('group', { name: 'Usage window' }).getByRole('button', { name: '1 hour' }).click();
  await page.getByText('· last hour').waitFor();
  check('switching the window refetches that look-back', Math.abs(froms.at(-1) - 3600_000) < 5000, froms.at(-1));
  await page.waitForFunction(() => document.querySelector('tr[data-row="1"]')?.textContent.includes('2,000'));
  check('tokens per minute are averaged over the new window', true);
  check('a window change starts no bogus trend', (await page.locator('tr[data-row="1"]').innerText()).match(/[▲▼]/) === null);
  const reads = froms.length;
  await page.getByRole('button', { name: 'Refresh now' }).click();
  await page.waitForTimeout(400);
  check('Refresh now fetches again', froms.length > reads);
  await page.getByRole('button', { name: 'rain', exact: true }).click();
  await page.getByRole('button', { name: 'Pause traffic' }).click();
  await page.reload();
  await page.locator('tbody tr[data-row]').first().waitFor();
  check('the window is remembered', (await page.getByRole('group', { name: 'Usage window' }).getByRole('button', { name: '1 hour' }).getAttribute('aria-pressed')) === 'true');
  check('the sky is remembered', (await page.getByRole('button', { name: 'rain', exact: true }).getAttribute('aria-pressed')) === 'true');
  check('the pause is remembered', (await page.getByRole('button', { name: 'Resume traffic' }).count()) === 1);
  await page.getByRole('group', { name: 'Usage window' }).getByRole('button', { name: '5 min' }).click();
  await page.getByRole('button', { name: 'Resume traffic' }).click();

  // legend
  check('the legend is collapsed by default', !(await page.getByText('Boarded up = disabled').isVisible()));
  await page.getByText('How to read the city').click();
  check('and opens on request', await page.getByText('Boarded up = disabled').isVisible());

  // full screen
  await page.locator('canvas').scrollIntoViewIfNeeded();
  await page.getByRole('button', { name: 'Full screen' }).click();
  await page.waitForFunction(() => !!document.fullscreenElement, null, { timeout: 4000 }).catch(() => {});
  const fs = await page.evaluate(() => ({ el: !!document.fullscreenElement, cw: document.querySelector('canvas')?.getBoundingClientRect().width, ch: document.querySelector('canvas')?.getBoundingClientRect().height, iw: innerWidth, ih: innerHeight }));
  check('Full screen takes the scene full screen', fs.el, fs);
  check('and the canvas fills the display', fs.el && Math.abs(fs.cw - fs.iw) < 4 && Math.abs(fs.ch - fs.ih) < 4, fs);
  await page.locator('.nbhd-stage').getByRole('button', { name: 'Exit full screen' }).click();
  await page.waitForFunction(() => !document.fullscreenElement);
  check('Exit full screen returns to the page', true);
} catch (e) {
  check('driver ran to completion', false, String(e));
} finally {
  await t.close();
}
finish();
