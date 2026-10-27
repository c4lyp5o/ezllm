// Exercise applied filters, bounded catalog loading, and shared fetch options.
import { start, json, account, models, requestsBody, check, finish } from './mockdash.mjs';
import { build } from 'esbuild';

const compiled = await build({ entryPoints: [new URL('../src/utils/api.ts', import.meta.url).pathname], bundle: true, format: 'esm', platform: 'node', write: false });
const { apiFetch } = await import('data:text/javascript;base64,' + Buffer.from(compiled.outputFiles[0].text).toString('base64'));
const originalFetch = globalThis.fetch;
let request;
try {
  globalThis.fetch = async (path, init) => { request = { path, init }; return new Response('{}', { status: 200 }); };
  await apiFetch('/test', { method: 'POST', headers: { 'X-Test': 'kept' } }, { ok: true });
  check('apiFetch preserves custom headers and JSON content type', request.init.headers.get('X-Test') === 'kept' && request.init.headers.get('Content-Type') === 'application/json');
  await apiFetch('/test', { method: 'POST', headers: new Headers({ 'Content-Type': 'text/plain' }), body: 'raw' });
  check('apiFetch preserves a supplied body and content type', request.init.body === 'raw' && request.init.headers.get('Content-Type') === 'text/plain');
} finally { globalThis.fetch = originalFetch; }

const t = await start();
let queries = [], inFlight = 0, maxInFlight = 0, fail = true;
const accounts = Array.from({ length: 6 }, (_, i) => ({ ...account, id: i + 1, name: `Provider ${i + 1}`, namespace: `p${i + 1}` }));
const page = await t.page(async (route, url) => {
  if (url.pathname === '/admin/requests') { queries.push(url.searchParams.get('min_tin')); await json(route, requestsBody('test')); return true; }
  if (url.pathname === '/admin/accounts') { await json(route, accounts); return true; }
  if (/^\/admin\/accounts\/\d+\/models$/.test(url.pathname)) {
    inFlight++; maxInFlight = Math.max(maxInFlight, inFlight);
    await new Promise(r => setTimeout(r, 120));
    const id = Number(url.pathname.split('/')[3]);
    inFlight--;
    await json(route, fail && id === 2 ? { error: { message: 'unavailable' } } : models, fail && id === 2 ? 503 : 200);
    return true;
  }
  return false;
});
try {
  await page.goto(`${t.base}/economics`);
  await page.getByText('No priced traffic yet').waitFor();
  const initial = queries.length;
  const input = page.getByLabel('Minimum actual input tokens');
  await input.pressSequentially('12345', { delay: 30 });
  await page.waitForTimeout(200);
  check('typing the filter sends no requests', queries.length === initial, queries);
  await page.getByRole('button', { name: 'Analyze' }).click();
  await page.waitForFunction(() => !document.body.textContent.includes('Loading savings…'));
  await page.waitForTimeout(200);
  check('Analyze applies the complete filter once', queries.length === initial + 1 && queries.at(-1) === '12345', queries);
  await input.fill('-1');
  await page.getByRole('button', { name: 'Analyze' }).click();
  check('invalid filter is explained without a request', await page.getByText('Minimum input tokens must be a non-negative whole number.').count() === 1 && queries.length === initial + 1);

  await page.goto(`${t.base}/chat`);
  await page.getByText(/Could not load models for Provider 2/).waitFor();
  check('catalog loads concurrently with a bound', maxInFlight > 1 && maxInFlight <= 4, maxInFlight);
  check('healthy providers remain selectable', await page.getByRole('combobox', { name: 'Model for column 1' }).count() === 1);
  fail = false;
  await page.getByRole('button', { name: 'Retry models' }).click();
  await page.getByText(/Could not load models for Provider 2/).waitFor({ state: 'hidden' });
  const picker = page.getByRole('combobox', { name: 'Model for column 1' });
  await picker.fill('p2');
  await page.getByRole('option').filter({ hasText: 'p2 / Model A' }).waitFor();
  check('retry restores models from recovered provider', await page.getByRole('option').filter({ hasText: 'p2 / Model A' }).count() === 1);
} finally { await t.close(); }
finish();
