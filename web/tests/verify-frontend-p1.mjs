// Regression checks for report failures, dialog focus, and chat cancellation.
import { start, json, account, check, finish } from './mockdash.mjs';
const t = await start();
let rulesFail = true, costFail = true;
const page = await t.page(async (route, url) => {
  if (url.pathname === '/admin/accounts') { await json(route, [{ ...account, quota: { kind: 'percent', read_at: '', raw: '{}' } }]); return true; }
  if (url.pathname === '/admin/accounts/1/quota') { await json(route, { kind: 'none', raw: '{}' }); return true; }
  if (url.pathname === '/admin/model-rules') { await json(route, rulesFail ? { error: { message: 'unavailable' } } : [], rulesFail ? 503 : 200); return true; }
  if (url.pathname === '/admin/cost') { await json(route, costFail ? { error: { message: 'unavailable' } } : { rows: [], totals: { calls: 0, usd: 0, unpriced_calls: 0, pricing_complete: true } }, costFail ? 503 : 200); return true; }
  return false;
});
await page.addInitScript(() => {
  const original = window.fetch;
  window.__chatStarted = 0; window.__chatAborted = 0;
  window.fetch = (input, init) => {
    if (typeof input === 'string' && input === '/admin/chat') {
      window.__chatStarted++;
      return new Promise((_resolve, reject) => {
        const abort = () => { window.__chatAborted++; reject(new DOMException('Cancelled', 'AbortError')); };
        if (init?.signal?.aborted) abort();
        else init?.signal?.addEventListener('abort', abort, { once: true });
      });
    }
    return original(input, init);
  };
});
try {
  await page.goto(`${t.base}/providers`);
  const open = page.getByRole('button', { name: /Add provider/ });
  await open.click();
  const dialog = page.getByRole('dialog', { name: 'Add provider' });
  await dialog.waitFor();
  await dialog.getByRole('button', { name: 'Close', exact: true }).focus();
  await page.keyboard.press('Shift+Tab');
  check('shift-tab stays in modal', await page.evaluate(() => document.querySelector('[role=dialog]')?.contains(document.activeElement)));
  await page.keyboard.press('Tab');
  check('tab wraps inside modal', await page.evaluate(() => document.querySelector('[role=dialog]')?.contains(document.activeElement)));
  await page.keyboard.press('Escape');
  await dialog.waitFor({ state: 'hidden' });
  check('focus returns to modal opener', await open.evaluate(el => el === document.activeElement));

  await page.getByText('Mock provider', { exact: true }).click();
  await page.getByRole('button', { name: 'Quota', exact: true }).click();
  await page.getByText('Could not load account caps.').waitFor();
  check('failed rules are not shown as no caps', await page.getByText(/No caps set for this account/).count() === 0);
  rulesFail = false;
  await page.getByRole('button', { name: 'Retry caps' }).click();
  await page.getByText(/No caps set for this account/).waitFor();
  check('quota caps retry shows recovered data', await page.getByText('Could not load account caps.').count() === 0);

  await page.goto(`${t.base}/economics`);
  await page.getByText('Could not load the cost report.').waitFor();
  check('failed cost is not shown as empty', await page.getByText('No priced traffic yet').count() === 0);
  costFail = false;
  await page.getByRole('button', { name: 'Retry cost report' }).click();
  await page.getByText('No priced traffic yet').waitFor();
  check('cost retry shows recovered data', await page.getByText('Could not load the cost report.').count() === 0);

  await page.goto(`${t.base}/chat`);
  await page.getByRole('button', { name: '＋ Add column' }).click();
  const message = page.getByRole('textbox', { name: 'Message' });
  await message.fill('hello'); await message.press('Enter');
  await page.waitForFunction(() => window.__chatStarted === 2);
  await page.getByRole('button', { name: 'Remove column' }).click();
  await page.waitForFunction(() => window.__chatAborted === 1);
  check('removing a column aborts only its request', await page.evaluate(() => window.__chatAborted === 1));
  await page.getByRole('navigation', { name: 'Main' }).getByRole('link', { name: 'Dashboard' }).click();
  await page.waitForFunction(() => window.__chatAborted === 2);
  check('leaving chat aborts remaining request', await page.evaluate(() => window.__chatAborted === 2));
} finally { await t.close(); }
finish();
